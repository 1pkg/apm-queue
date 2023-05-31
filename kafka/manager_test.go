// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package kafka

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kmsg"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestNewManager(t *testing.T) {
	_, err := NewManager(ManagerConfig{})
	assert.Error(t, err)
	assert.EqualError(t, err, "kafka: invalid manager config: "+strings.Join([]string{
		"kafka: at least one broker must be set",
		"kafka: logger must be set",
	}, "\n"))
}

func TestManagerDeleteTopics(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
	)
	defer tp.Shutdown(context.Background())

	cluster, commonConfig := newFakeCluster(t)
	core, observedLogs := observer.New(zapcore.DebugLevel)
	commonConfig.Logger = zap.New(core)
	commonConfig.TracerProvider = tp
	m, err := NewManager(ManagerConfig{CommonConfig: commonConfig})
	require.NoError(t, err)
	t.Cleanup(func() { m.Close() })

	var deleteTopicsRequest *kmsg.DeleteTopicsRequest
	cluster.ControlKey(kmsg.DeleteTopics.Int16(), func(req kmsg.Request) (kmsg.Response, error, bool) {
		deleteTopicsRequest = req.(*kmsg.DeleteTopicsRequest)
		return &kmsg.DeleteTopicsResponse{
			Version: 7,
			Topics: []kmsg.DeleteTopicsResponseTopic{{
				Topic:        kmsg.StringPtr("topic1"),
				ErrorCode:    kerr.UnknownTopicOrPartition.Code,
				ErrorMessage: &kerr.UnknownTopicOrPartition.Message,
			}, {
				Topic:        kmsg.StringPtr("topic2"),
				ErrorCode:    kerr.InvalidTopicException.Code,
				ErrorMessage: &kerr.InvalidTopicException.Message,
			}, {
				Topic:   kmsg.StringPtr("topic3"),
				TopicID: [16]byte{123},
			}},
		}, nil, true
	})
	err = m.DeleteTopics(context.Background(), "topic1", "topic2", "topic3")
	require.Error(t, err)
	assert.EqualError(t, err,
		`failed to delete topic "topic2": `+
			`INVALID_TOPIC_EXCEPTION: The request attempted to perform an operation on an invalid topic.`,
	)

	require.Len(t, deleteTopicsRequest.Topics, 3)
	assert.Equal(t, []kmsg.DeleteTopicsRequestTopic{{
		Topic: kmsg.StringPtr("topic1"),
	}, {
		Topic: kmsg.StringPtr("topic2"),
	}, {
		Topic: kmsg.StringPtr("topic3"),
	}}, deleteTopicsRequest.Topics)

	matchingLogs := observedLogs.FilterFieldKey("topic")
	assert.Equal(t, []observer.LoggedEntry{{
		Entry: zapcore.Entry{
			Level:   zapcore.DebugLevel,
			Message: "kafka topic does not exist",
		},
		Context: []zapcore.Field{
			zap.String("topic", "topic1"),
		},
	}, {
		Entry: zapcore.Entry{
			Level:   zapcore.InfoLevel,
			Message: "deleted kafka topic",
		},
		Context: []zapcore.Field{
			zap.String("topic", "topic3"),
		},
	}}, matchingLogs.AllUntimed())

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "DeleteTopics", spans[0].Name)
	assert.Equal(t, codes.Error, spans[0].Status.Code)
	require.Len(t, spans[0].Events, 1)
	assert.Equal(t, "exception", spans[0].Events[0].Name)
	assert.Equal(t, []attribute.KeyValue{
		semconv.ExceptionTypeKey.String("*kerr.Error"),
		semconv.ExceptionMessageKey.String(
			"INVALID_TOPIC_EXCEPTION: The request attempted to perform an operation on an invalid topic.",
		),
	}, spans[0].Events[0].Attributes)
}

func newFakeCluster(t testing.TB) (*kfake.Cluster, CommonConfig) {
	cluster, err := kfake.NewCluster()
	require.NoError(t, err)
	t.Cleanup(cluster.Close)
	return cluster, CommonConfig{
		Brokers: cluster.ListenAddrs(),
		Logger:  zap.NewNop(),
	}
}
