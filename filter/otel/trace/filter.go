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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)

import (
	"dubbo.apache.org/dubbo-go/v3/common/constant"
	"dubbo.apache.org/dubbo-go/v3/common/extension"
	"dubbo.apache.org/dubbo-go/v3/filter"
	"dubbo.apache.org/dubbo-go/v3/protocol/base"
	"dubbo.apache.org/dubbo-go/v3/protocol/result"
)

/*
*

┌─────────────────────────────────────────────────────────────────────────────┐
│                     OTel Trace 分布式追踪架构                                 │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│   ┌─────────────────┐        RPC调用          ┌─────────────────┐            │
│   │   Consumer      │ ─────────────────────► │   Provider      │            │
│   │ (Client Filter) │                        │ (Server Filter) │            │
│   └────────┬────────┘                        └────────┬────────┘            │
│            │                                          │                     │
│            │ ① Start Client Span                      │                     │
│            │ ② Inject Context → Attachments           │                     │
│            │                                          │                     │
│            │─────────────────────────────────────────►│                     │
│            │                                          │ ③ Extract Context   │
│            │                                          │ ④ Start Server Span │
│            │                                          │                     │
│            │◄─────────────────────────────────────────│                     │
│            │                                          │ ⑤ End Server Span   │
│            │ ⑥ End Client Span                        │                     │
│            │                                          │                     │
│   ┌────────▼────────┐                        ┌────────▼────────┐            │
│   │  TracerProvider │                        │  TracerProvider │            │
│   │  (创建Span)      │                        │  (创建Span)     │            │
│   └────────┬────────┘                        └────────┬────────┘            │
│            │                                          │                     │
│            └──────────────────┬───────────────────────┘                     │
│                               ▼                                             │
│                    ┌─────────────────┐                                      │
│                    │  OTel Collector  │                                     │
│                    │  (聚合Span数据)   │                                     │
│                    └────────┬────────┘                                      │
│                             ▼                                               │
│              ┌─────────────────────────┐                                    │
│              │  Zipkin/Jaeger/OTLP      │  (可视化展示)                       │
│              └─────────────────────────┘                                    │
├─────────────────────────────────────────────────────────────────────────────┤
│  Span 关系图：                                                               │
│                                                                             │
│     trace-id: abc123                                                        │
│     ┌──────────────────────────────────────────────────────────────────┐    │
│     │  Client Span                                                     │    │
│     │  span-id: 001                                                    │    │
│     │  parent-span-id: (root)                                          │    │
│     │  ┌────────────────────────────────────────────────────────────┐  │    │
│     │  │  Server Span                                               │  │    │
│     │  │  span-id: 002                                              │  │    │
│     │  │  parent-span-id: 001                                       │  │    │
│     │  └────────────────────────────────────────────────────────────┘  │    │
│     └──────────────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────────────┘
*/
func init() {
	// TODO: use single filter to simplify filter field in configuration
	// 注册服务端追踪Filter
	extension.SetFilter(constant.OTELServerTraceKey, func() filter.Filter {
		return &otelServerFilter{
			// 在上下文中注入/提取Trace Context
			Propagators: otel.GetTextMapPropagator(),
			/**
			创建Tracer实例，管理Span生命周期
			TracerProvider 是创建 Tracer 的工厂，负责：
			- 管理 Tracer 的创建和配置
			- 控制 Span 的采样策略
			- 管理 Span 处理器（SpanProcessor）
			*/
			TracerProvider: otel.GetTracerProvider(), // 获取全局默认的 TracerProvider
		}
	})
	// 注册客户端追踪Filter
	extension.SetFilter(constant.OTELClientTraceKey, func() filter.Filter {
		return &otelClientFilter{
			// 在上下文中注入/提取Trace Context
			Propagators: otel.GetTextMapPropagator(),
			// 创建Tracer实例，管理Span生命周期
			TracerProvider: otel.GetTracerProvider(),
		}
	})
}

var _ filter.Filter = (*otelServerFilter)(nil)

// otelServerFilter implements server-side tracing for Dubbo requests
// by creating and managing trace spans using the configured propagator
// and tracer provider.
type otelServerFilter struct {
	Propagators    propagation.TextMapPropagator
	TracerProvider trace.TracerProvider
}

func (f *otelServerFilter) OnResponse(ctx context.Context, result result.Result, invoker base.Invoker, protocol base.Invocation) result.Result {
	return result
}

func (f *otelServerFilter) Invoke(ctx context.Context, invoker base.Invoker, invocation base.Invocation) result.Result {
	// ① 从Attachments中提取上游传递的Trace Context
	attachments := invocation.Attachments()
	bags, spanCtx := Extract(ctx, attachments, f.Propagators)
	ctx = baggage.ContextWithBaggage(ctx, bags)

	//	创建Span，设置Span属性和状态
	// ② 获取Tracer（命名空间 + 版本号）
	tracer := f.TracerProvider.Tracer(
		constant.TraceScopeName,
		trace.WithInstrumentationVersion(constant.Version),
	)

	// 代表一次RPC调用的追踪单元
	// ③ 创建Server Span（关联上游Span作为父Span）
	ctx, span := tracer.Start(
		trace.ContextWithRemoteSpanContext(ctx, spanCtx), // 关键：关联父Span
		invocation.ActualMethodName(),
		trace.WithSpanKind(trace.SpanKindServer), // 标识为服务端Span
		trace.WithAttributes(
			semconv.RPCSystemApacheDubbo,                      // RPC框架标识
			semconv.RPCService(invoker.GetURL().ServiceKey()), // 服务名
			semconv.RPCMethod(invocation.MethodName()),        // 方法名
		),
	)
	defer span.End() // ④ 调用结束时自动关闭Span

	// ⑤ 执行实际RPC调用
	res := invoker.Invoke(ctx, invocation)

	// ⑥ 根据结果设置Span状态
	if res.Error() != nil {
		span.SetStatus(codes.Error, res.Error().Error())
	} else {
		span.SetStatus(codes.Ok, codes.Ok.String())
	}
	return res
}

var _ filter.Filter = (*otelClientFilter)(nil)

// otelClientFilter implements client-side tracing for Dubbo requests
// by creating and managing trace spans using the configured propagator
// and tracer provider.
type otelClientFilter struct {
	Propagators    propagation.TextMapPropagator
	TracerProvider trace.TracerProvider
}

func (f *otelClientFilter) OnResponse(ctx context.Context, result result.Result, invoker base.Invoker, protocol base.Invocation) result.Result {
	return result
}

func (f *otelClientFilter) Invoke(ctx context.Context, invoker base.Invoker, invocation base.Invocation) result.Result {
	//	创建Span，设置Span属性和状态
	// ① 获取Tracer
	tracer := f.TracerProvider.Tracer(
		constant.TraceScopeName,
		trace.WithInstrumentationVersion(constant.Version),
	)

	// 代表一次RPC调用的追踪单元
	// ② 创建Client Span
	var span trace.Span
	ctx, span = tracer.Start(
		ctx,
		invocation.ActualMethodName(),
		trace.WithSpanKind(trace.SpanKindClient), // 标识为客户端Span
		trace.WithAttributes(
			semconv.RPCSystemApacheDubbo,
			semconv.RPCService(invoker.GetURL().ServiceKey()),
			semconv.RPCMethod(invocation.MethodName()),
		),
	)
	defer span.End() // ③ 调用结束时自动关闭Span

	// ④ 将Trace Context注入到Attachments
	attachments := invocation.Attachments()
	if attachments == nil {
		attachments = map[string]any{}
	}
	Inject(ctx, attachments, f.Propagators) // 关键：注入Context
	for k, v := range attachments {
		invocation.SetAttachment(k, v)
	}

	// ⑤ 执行实际RPC调用
	res := invoker.Invoke(ctx, invocation)

	// ⑥ 根据结果设置Span状态
	if res.Error() != nil {
		span.SetStatus(codes.Error, res.Error().Error())
	} else {
		span.SetStatus(codes.Ok, codes.Ok.String())
	}
	return res
}
