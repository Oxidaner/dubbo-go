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
	"dubbo.apache.org/dubbo-go/v3/metrics"
)

// metricSet is the metric set for rpc
type metricSet struct {
	provider *providerMetrics
	consumer *consumerMetrics
}

type providerMetrics struct {
	rpcCommonMetrics
}

type consumerMetrics struct {
	rpcCommonMetrics
}

// rpcCommonMetrics is the common metrics for both provider and consumer
type rpcCommonMetrics struct {
	qpsTotal                      metrics.QpsMetricVec        // QPS 每秒请求数
	requestsTotal                 metrics.CounterVec          // requests provider收到的请求总数
	requestsTotalAggregate        metrics.AggregateCounterVec // requests 滑动窗口下provider收到的请求总数
	requestsProcessingTotal       metrics.GaugeVec            // requests provider正在处理的请求数量
	requestsSucceedTotal          metrics.CounterVec          // requests 累计成功请求数
	requestsSucceedTotalAggregate metrics.AggregateCounterVec // requests 滑动窗口下provider中成功请求数
	requestsFailedTotal           metrics.CounterVec          // requests 累计失败请求数
	requestsFailedTotalAggregate  metrics.AggregateCounterVec // requests 滑动窗口下provider中失败请求数
	rtMilliseconds                metrics.RtVec               // requests 响应时间 毫秒
	rtMillisecondsQuantiles       metrics.QuantileMetricVec   // requests 响应时间 毫秒分位数
	rtMillisecondsAggregate       metrics.RtVec               // requests 滑动窗口下provider中响应时间 毫秒

	// Granular error metrics
	requestsTimeoutTotal                     metrics.CounterVec          // requests 超时请求数 累计超时请求数
	requestsTimeoutTotalAggregate            metrics.AggregateCounterVec // requests 滑动窗口下provider中超时请求数
	requestsLimitTotal                       metrics.CounterVec          // requests 限流请求数 累计限流请求数
	requestsLimitTotalAggregate              metrics.AggregateCounterVec // requests 滑动窗口下provider中限流请求数
	requestsServiceUnavailableTotal          metrics.CounterVec          // requests 服务不可用请求数 累计服务不可用请求数
	requestsServiceUnavailableTotalAggregate metrics.AggregateCounterVec // requests 滑动窗口下provider中服务不可用请求数
	requestsBusinessFailedTotal              metrics.CounterVec          // requests 业务失败请求数 累计业务失败请求数
	requestsBusinessFailedTotalAggregate     metrics.AggregateCounterVec // requests 滑动窗口下provider中业务失败请求数
	requestsUnknownFailedTotal               metrics.CounterVec          // requests 未知失败请求数 累计未知失败请求数
	requestsUnknownFailedTotalAggregate      metrics.AggregateCounterVec // requests 滑动窗口下provider中未知失败请求数
}

// buildMetricSet will call init functions to initialize the metricSet
func buildMetricSet(registry metrics.MetricRegistry) *metricSet {
	ms := &metricSet{
		provider: &providerMetrics{},
		consumer: &consumerMetrics{},
	}
	ms.provider.init(registry)
	ms.consumer.init(registry)
	return ms
}

func (pm *providerMetrics) init(registry metrics.MetricRegistry) {
	pm.qpsTotal = metrics.NewQpsMetricVec(registry, metrics.NewMetricKey("dubbo_provider_qps_total", "The number of requests received by the provider per second"))
	pm.requestsTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_total", "The total number of received requests by the provider"))
	pm.requestsTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_total_aggregate", "The total number of received requests by the provider under the sliding window"))
	pm.requestsProcessingTotal = metrics.NewGaugeVec(registry, metrics.NewMetricKey("dubbo_provider_requests_processing_total", "The number of received requests being processed by the provider"))
	pm.requestsSucceedTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_succeed_total", "The number of requests successfully received by the provider"))
	pm.requestsSucceedTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_succeed_total_aggregate", "The number of successful requests received by the provider under the sliding window"))
	pm.requestsFailedTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_failed_total", "Total Failed Requests"))
	pm.requestsFailedTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_failed_total_aggregate", "Total Failed Aggregate Requests"))
	pm.rtMilliseconds = metrics.NewRtVec(registry,
		metrics.NewMetricKey("dubbo_provider_rt_milliseconds", "response time among all requests processed by the provider"),
		&metrics.RtOpts{Aggregate: false},
	)
	pm.rtMillisecondsAggregate = metrics.NewRtVec(registry,
		metrics.NewMetricKey("dubbo_provider_rt", "response time of the provider under the sliding window"),
		&metrics.RtOpts{Aggregate: true, BucketNum: metrics.DefaultBucketNum, TimeWindowSeconds: metrics.DefaultTimeWindowSeconds},
	)
	pm.rtMillisecondsQuantiles = metrics.NewQuantileMetricVec(registry, []*metrics.MetricKey{
		metrics.NewMetricKey("dubbo_provider_rt_milliseconds_p50", "The total response time spent by providers processing 50% of requests"),
		metrics.NewMetricKey("dubbo_provider_rt_milliseconds_p90", "The total response time spent by providers processing 90% of requests"),
		metrics.NewMetricKey("dubbo_provider_rt_milliseconds_p95", "The total response time spent by providers processing 95% of requests"),
		metrics.NewMetricKey("dubbo_provider_rt_milliseconds_p99", "The total response time spent by providers processing 99% of requests"),
	}, []float64{0.5, 0.9, 0.95, 0.99})

	// Granular error metrics
	pm.requestsTimeoutTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_timeout_total", "Total Timeout Failed Requests"))
	pm.requestsTimeoutTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_timeout_total_aggregate", "Total Timeout Failed Requests under the sliding window"))
	pm.requestsLimitTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_limit_total", "Total Limit Failed Requests"))
	pm.requestsLimitTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_limit_total_aggregate", "Total Limit Failed Requests under the sliding window"))
	pm.requestsServiceUnavailableTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_failed_service_unavailable_total", "Total Service Unavailable Failed Requests"))
	pm.requestsServiceUnavailableTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_failed_service_unavailable_total_aggregate", "Total Service Unavailable Failed Requests under the sliding window"))
	pm.requestsBusinessFailedTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_business_failed_total", "Total Failed Business Requests"))
	pm.requestsBusinessFailedTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_business_failed_total_aggregate", "Total Failed Business Requests under the sliding window"))
	pm.requestsUnknownFailedTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_unknown_failed_total", "Total Unknown Failed Requests"))
	pm.requestsUnknownFailedTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_provider_requests_unknown_failed_total_aggregate", "Total Unknown Failed Requests under the sliding window"))
}

func (cm *consumerMetrics) init(registry metrics.MetricRegistry) {
	cm.qpsTotal = metrics.NewQpsMetricVec(registry, metrics.NewMetricKey("dubbo_consumer_qps_total", "The number of requests sent by consumers per second"))
	cm.requestsTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_total", "The total number of requests sent by consumers"))
	cm.requestsTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_total_aggregate", "The total number of requests sent by consumers under the sliding window"))
	cm.requestsProcessingTotal = metrics.NewGaugeVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_processing_total", "The number of received requests being processed by the consumer"))
	cm.requestsSucceedTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_succeed_total", "The number of successful requests sent by consumers"))
	cm.requestsSucceedTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_succeed_total_aggregate", "The number of successful requests sent by consumers under the sliding window"))
	cm.requestsFailedTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_failed_total", "Total Failed Requests"))
	cm.requestsFailedTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_failed_total_aggregate", "Total Failed Aggregate Requests"))
	cm.rtMilliseconds = metrics.NewRtVec(registry,
		metrics.NewMetricKey("dubbo_consumer_rt_milliseconds", "response time among all requests from consumers"),
		&metrics.RtOpts{Aggregate: false},
	)
	cm.rtMillisecondsAggregate = metrics.NewRtVec(registry,
		metrics.NewMetricKey("dubbo_consumer_rt", "response time of the consumer under the sliding window"),
		&metrics.RtOpts{Aggregate: true, BucketNum: metrics.DefaultBucketNum, TimeWindowSeconds: metrics.DefaultTimeWindowSeconds},
	)
	cm.rtMillisecondsQuantiles = metrics.NewQuantileMetricVec(registry, []*metrics.MetricKey{
		metrics.NewMetricKey("dubbo_consumer_rt_milliseconds_p50", "The total response time spent by consumers processing 50% of requests"),
		metrics.NewMetricKey("dubbo_consumer_rt_milliseconds_p90", "The total response time spent by consumers processing 90% of requests"),
		metrics.NewMetricKey("dubbo_consumer_rt_milliseconds_p95", "The total response time spent by consumers processing 95% of requests"),
		metrics.NewMetricKey("dubbo_consumer_rt_milliseconds_p99", "The total response time spent by consumers processing 99% of requests"),
	}, []float64{0.5, 0.9, 0.95, 0.99})

	// Granular error metrics
	cm.requestsTimeoutTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_timeout_total", "Total Timeout Failed Requests"))
	cm.requestsTimeoutTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_timeout_total_aggregate", "Total Timeout Failed Requests under the sliding window"))
	cm.requestsLimitTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_limit_total", "Total Limit Failed Requests"))
	cm.requestsLimitTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_limit_total_aggregate", "Total Limit Failed Requests under the sliding window"))
	cm.requestsServiceUnavailableTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_failed_service_unavailable_total", "Total Service Unavailable Failed Requests"))
	cm.requestsServiceUnavailableTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_failed_service_unavailable_total_aggregate", "Total Service Unavailable Failed Requests under the sliding window"))
	cm.requestsBusinessFailedTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_business_failed_total", "Total Failed Business Requests"))
	cm.requestsBusinessFailedTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_business_failed_total_aggregate", "Total Failed Business Requests under the sliding window"))
	cm.requestsUnknownFailedTotal = metrics.NewCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_unknown_failed_total", "Total Unknown Failed Requests"))
	cm.requestsUnknownFailedTotalAggregate = metrics.NewAggregateCounterVec(registry, metrics.NewMetricKey("dubbo_consumer_requests_unknown_failed_total_aggregate", "Total Unknown Failed Requests under the sliding window"))
}
