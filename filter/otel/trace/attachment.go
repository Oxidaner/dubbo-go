/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package trace

import (
	"context"
)

import (
	"go.opentelemetry.io/otel/baggage"

	"go.opentelemetry.io/otel/propagation"

	"go.opentelemetry.io/otel/trace"
)

// metadataSupplier 实现了 OTel 的 TextMapCarrier 接口
// 用于适配 Dubbo Attachments 与 OTel 上下文传递协议
type metadataSupplier struct {
	metadata map[string]any
}

var _ propagation.TextMapCarrier = (*metadataSupplier)(nil) // 接口断言

// Get 从Attachments中获取指定key的值
func (s *metadataSupplier) Get(key string) string {
	if s.metadata == nil {
		return ""
	}
	item, ok := s.metadata[key].([]string)
	if !ok {
		return ""
	}
	if len(item) == 0 {
		return ""
	}
	return item[0]
}

// Set 将Trace Context写入Attachments
func (s *metadataSupplier) Set(key string, value string) {
	if s.metadata == nil {
		s.metadata = map[string]any{}
	}
	s.metadata[key] = value
}

// Keys 返回所有key
func (s *metadataSupplier) Keys() []string {
	out := make([]string, 0, len(s.metadata))
	for key := range s.metadata {
		out = append(out, key)
	}
	return out
}

// Inject injects correlation context and span context into the dubbo
// metadata object. This function is meant to be used on outgoing
// requests.

// Inject: 将SpanContext注入到Dubbo Attachments
func Inject(ctx context.Context, metadata map[string]any, propagators propagation.TextMapPropagator) {
	propagators.Inject(ctx, &metadataSupplier{
		metadata: metadata,
	})
}

// Extract returns the baggage and span context that
// another service encoded in the dubbo metadata object with Inject.
// This function is meant to be used on incoming requests.
// Extract: 从Dubbo Attachments中提取SpanContext
func Extract(ctx context.Context, metadata map[string]any, propagators propagation.TextMapPropagator) (baggage.Baggage, trace.SpanContext) {
	ctx = propagators.Extract(ctx, &metadataSupplier{
		metadata: metadata,
	})
	return baggage.FromContext(ctx), trace.SpanContextFromContext(ctx)
}
