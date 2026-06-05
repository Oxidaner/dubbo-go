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

package rpc

import (
	"github.com/dubbogo/gost/log/logger"
)

import (
	"dubbo.apache.org/dubbo-go/v3/common"
	"dubbo.apache.org/dubbo-go/v3/common/constant"
	"dubbo.apache.org/dubbo-go/v3/metrics"
)

var (
	rpcMetricsChan = make(chan metrics.MetricsEvent, 1024) // 带缓冲channel
)

/**
┌─────────────────────────────────────────────────────────────────────────┐
│                         连接架构全景图                                     │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ┌──────────────────┐           ┌─────────────────┐           ┌─────────┐
│  │   metricsFilter  │           │  metrics.Bus    │           │rpcCollector│
│  │   (发布者)        │           │  (事件路由中心)  │            │ (订阅者)   │
│  └────────┬─────────┘           └────────┬────────┘           └────┬────┘
│           │                              │                         │
│           │ ① metrics.Publish(event)     │                         │
│           │─────────────────────────────►│                         │
│           │                              │                         │
│           │                              │ ② ch <- event          │
│           │                              │────────────────────────►│
│           │                              │                         │
│           │                              │ ③ range ch 异步消费      │
│           │                              │                         │
│  Filter层(同步)                         Bus层(解耦)              Collector层(异步)
│                                                                       │
├───────────────────────────────────────────────────────────────────────┤
│  关键组件关系：                                                          │
│  ┌─────────────────────────────────────────────────────────────  ────┐ │
│  │  Filter 层                                                         │ │
│  │  • Invoke() 发布 BeforeInvokeEvent                                 │ │
│  │  • 执行 invoker.Invoke()                                           │ │
│  │  • 发布 AfterInvokeEvent (带耗时和结果)                               │ │
│  ├───────────────────────────────────────────────────────────────────┤ │
│  │  Bus 层 (metrics/bus.go)                                           │ │
│  │  • listener: map[string]chan MetricsEvent  // 事件类型→channel映射   │ │
│  │  • Publish(): 向对应channel发送事件 (非阻塞，满则丢弃)                  │ │
│  │  • Subscribe(): 注册channel到指定事件类型                             │ │
│  ├────────────────────────────────────────────────────────────────────┤ │
│  │  Collector 层                                                      │ │
│  │  • rpcMetricsChan = make(chan MetricsEvent, 1024)  // 带缓冲channel │ │
│  │  • Subscribe(constant.MetricsRpc, rpcMetricsChan)  // 订阅RPC事件   │ │
│  │  • for event := range rpcMetricsChan { ... }       // 异步消费循环   │ │
│  └────────────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────────┘


*/
// init will add the rpc collectorFunc to metrics.collectors slice, and lazy start the rpc collector goroutine
func init() {
	collectorFunc := func(registry metrics.MetricRegistry, url *common.URL) {
		if url.GetParamBool(constant.RpcEnabledKey, true) {
			rc := &rpcCollector{
				registry:  registry,
				metricSet: buildMetricSet(registry),
			}
			go rc.start() // 独立goroutine异步消费
		}
	}
	// 注册 "rpc" 类型的采集器函数
	metrics.AddCollector("rpc", collectorFunc)
}

// rpcCollector is a collector which will collect the rpc metrics
type rpcCollector struct {
	registry  metrics.MetricRegistry
	metricSet *metricSet // metricSet is a struct which contains all metrics about rpc
}

// start will subscribe the rpc.metricsEvent from channel rpcMetricsChan, and handle the event from the channel
func (c *rpcCollector) start() {
	// ① 订阅 "rpc" 类型事件，指定消费channel
	metrics.Subscribe(constant.MetricsRpc, rpcMetricsChan)

	// ② 启动异步消费循环
	for event := range rpcMetricsChan {
		if rpcEvent, ok := event.(*metricsEvent); ok {
			switch rpcEvent.name {
			case BeforeInvoke:
				c.beforeInvokeHandler(rpcEvent)
			case AfterInvoke:
				c.afterInvokeHandler(rpcEvent)
			default:
			}
		} else {
			logger.Error("Bad metrics event found in RPC collector")
		}
	}
}

func (c *rpcCollector) beforeInvokeHandler(event *metricsEvent) {
	url := event.invoker.GetURL()
	role := getRole(url) // 判断是Provider还是Consumer

	if role == "" {
		return
	}
	labels := buildLabels(url, event.invocation) // 构建维度标签
	c.recordQps(role, labels)                    // 记录QPS
	c.incRequestsProcessingTotal(role, labels)   // 处理中请求数+1
}

func (c *rpcCollector) afterInvokeHandler(event *metricsEvent) {
	url := event.invoker.GetURL()
	role := getRole(url)

	if role == "" {
		return
	}
	labels := buildLabels(url, event.invocation)
	c.incRequestsTotal(role, labels)           // 请求总数+1
	c.decRequestsProcessingTotal(role, labels) // 处理中请求数-1
	if event.result != nil {
		if event.result.Error() == nil {
			c.incRequestsSucceedTotal(role, labels) // 成功计数
		} else {
			// Increment total failed count
			c.incRequestsFailedTotal(role, labels) // 失败计数
			// Classify and increment granular error metrics
			errType := classifyError(event.result.Error())
			c.incRequestsFailedByType(role, labels, errType) // 错误分类
		}
	}
	c.reportRTMilliseconds(role, labels, event.costTime.Milliseconds())
}

func (c *rpcCollector) recordQps(role string, labels map[string]string) {
	switch role {
	case constant.SideProvider:
		c.metricSet.provider.qpsTotal.Record(labels)
	case constant.SideConsumer:
		c.metricSet.consumer.qpsTotal.Record(labels)
	}
}

func (c *rpcCollector) incRequestsTotal(role string, labels map[string]string) {
	switch role {
	case constant.SideProvider:
		c.metricSet.provider.requestsTotal.Inc(labels)
		c.metricSet.provider.requestsTotalAggregate.Inc(labels)
	case constant.SideConsumer:
		c.metricSet.consumer.requestsTotal.Inc(labels)
		c.metricSet.consumer.requestsTotalAggregate.Inc(labels)
	}
}

func (c *rpcCollector) incRequestsProcessingTotal(role string, labels map[string]string) {
	switch role {
	case constant.SideProvider:
		c.metricSet.provider.requestsProcessingTotal.Inc(labels)
	case constant.SideConsumer:
		c.metricSet.consumer.requestsProcessingTotal.Inc(labels)
	}
}

func (c *rpcCollector) decRequestsProcessingTotal(role string, labels map[string]string) {
	switch role {
	case constant.SideProvider:
		c.metricSet.provider.requestsProcessingTotal.Dec(labels)
	case constant.SideConsumer:
		c.metricSet.consumer.requestsProcessingTotal.Dec(labels)
	}
}

func (c *rpcCollector) incRequestsSucceedTotal(role string, labels map[string]string) {
	switch role {
	case constant.SideProvider:
		c.metricSet.provider.requestsSucceedTotal.Inc(labels)
		c.metricSet.provider.requestsSucceedTotalAggregate.Inc(labels)
	case constant.SideConsumer:
		c.metricSet.consumer.requestsSucceedTotal.Inc(labels)
		c.metricSet.consumer.requestsSucceedTotalAggregate.Inc(labels)
	}
}

func (c *rpcCollector) incRequestsFailedTotal(role string, labels map[string]string) {
	switch role {
	case constant.SideProvider:
		c.metricSet.provider.requestsFailedTotal.Inc(labels)
		c.metricSet.provider.requestsFailedTotalAggregate.Inc(labels)
	case constant.SideConsumer:
		c.metricSet.consumer.requestsFailedTotal.Inc(labels)
		c.metricSet.consumer.requestsFailedTotalAggregate.Inc(labels)
	}
}

func (c *rpcCollector) incRequestsFailedByType(role string, labels map[string]string, errType ErrorType) {
	var ms *rpcCommonMetrics

	switch role {
	case constant.SideProvider:
		ms = &c.metricSet.provider.rpcCommonMetrics
	case constant.SideConsumer:
		ms = &c.metricSet.consumer.rpcCommonMetrics
	default:
		return
	}

	switch errType {
	case ErrorTypeTimeout:
		ms.requestsTimeoutTotal.Inc(labels)
		ms.requestsTimeoutTotalAggregate.Inc(labels)
	case ErrorTypeLimit:
		ms.requestsLimitTotal.Inc(labels)
		ms.requestsLimitTotalAggregate.Inc(labels)
	case ErrorTypeServiceUnavailable:
		ms.requestsServiceUnavailableTotal.Inc(labels)
		ms.requestsServiceUnavailableTotalAggregate.Inc(labels)
	case ErrorTypeBusinessFailed:
		ms.requestsBusinessFailedTotal.Inc(labels)
		ms.requestsBusinessFailedTotalAggregate.Inc(labels)
	default:
		ms.requestsUnknownFailedTotal.Inc(labels)
		ms.requestsUnknownFailedTotalAggregate.Inc(labels)
	}
}

func (c *rpcCollector) reportRTMilliseconds(role string, labels map[string]string, cost int64) {
	switch role {
	case constant.SideProvider:
		c.metricSet.provider.rtMilliseconds.Record(labels, float64(cost))
		c.metricSet.provider.rtMillisecondsAggregate.Record(labels, float64(cost))
		c.metricSet.provider.rtMillisecondsQuantiles.Record(labels, float64(cost))
	case constant.SideConsumer:
		c.metricSet.consumer.rtMilliseconds.Record(labels, float64(cost))
		c.metricSet.consumer.rtMillisecondsAggregate.Record(labels, float64(cost))
		c.metricSet.consumer.rtMillisecondsQuantiles.Record(labels, float64(cost))
	}
}
