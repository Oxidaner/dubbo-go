# 设计文档：增强应用级 Metadata 链路的可观测性与错误语义

> 对应 Issue：[#3356](https://github.com/apache/dubbo-go/issues/3356)
> 背景讨论：[#3188 (comment)](https://github.com/apache/dubbo-go/issues/3188#issuecomment-4582464870)
> 目标版本：3.3.2
> 状态：Draft

---

## 0. TL;DR（一句话目标）

> 当 consumer 订阅不到服务时，运维/开发能**在 1 分钟内**通过指标 + 日志判断根因到底是
> **mapping / metadata report / RPC metadata service / revision / cache** 中的哪一个阶段，而不需要加日志重新编译。

为达到这个目标，本次改造围绕三根支柱：

1. **错误语义（Error Semantics）**：给应用级 metadata 链路上的错误打上**统一的分类**（5 类），并携带 `app / revision / serviceKey / registryId / storageType` 等上下文。
2. **指标（Metrics）**：为 mapping 的 `register / get / listen / remove` 四个操作补齐成功/失败计数与耗时；暴露 cache hit/miss、metadata source、storage type。
3. **结构化日志（Structured Logs）**：在链路每个阶段输出**格式统一、可被日志系统解析**的 `key=value` 日志。

本文覆盖：**前置知识 → 现状分析 → 设计方案 → 开发步骤 → 测试方案 → 验收标准**。

---

## 1. 背景与目标

### 1.1 Issue 要解决什么

`#2534` 之后，dubbo-go 的服务发现主路径转向**应用级服务发现（Application-Level Service Discovery）**。应用级 metadata 链路包括：

- 本地 `MetadataInfo`（应用维度的元数据容器）
- revision 计算
- metadata report 的发布/获取
- service-app mapping（接口名 → 应用名 映射）
- RPC `MetadataService`（通过 RPC 直接向 provider 拉取 MetadataInfo）

**问题**：当服务发现失败（consumer 订阅不到 provider）时，目前很难判断是哪个阶段出的问题。原因是：

- mapping 的 register/get/listen/remove **没有统一的指标或结构化日志**。
- revision 计算、metadata cache 的 hit/miss **难以观测**。
- metadata report 获取、RPC metadata 获取、URL 构造、mapping 的错误**没有清晰分类**，错误信息散乱（有的是裸 `errors.New`，有的带 `[MetadataRPC]` 前缀，有的什么都没有）。

### 1.2 本次范围（In Scope）

只关注**应用级 metadata 链路的可观测性与错误语义**，核心改动文件：

| 文件 | 角色 |
|---|---|
| `metadata/report_instance.go` | `DelegateMetadataReport`：metadata report 调用的统一委托层（指标埋点入口） |
| `metadata/mapping/metadata/service_name_mapping.go` | service-app mapping 的实现（register/get/listen/remove） |
| `metadata/client.go` | 通过 metadata report / RPC 拉取远端 `MetadataInfo` |
| `registry/servicediscovery/service_instances_changed_listener_impl.go` | 实例变更监听 + cache + revision 处理 |

辅助/新增：

| 文件 | 角色 |
|---|---|
| `metadata/errors.go`（新增） | 统一错误类型与分类 |
| `metadata/observability.go`（新增，可选） | 统一日志格式辅助函数 |
| `metrics/metadata/metric_set.go` / `collector.go`（扩展） | mapping & cache 指标 |

### 1.3 非目标（Out of Scope）

为避免范围蔓延，本次**不做**：

- 不引入新的日志框架（dubbo-go 用的是 gost 的 printf 风格 logger，不是 zap 的 `Infow` 结构化 API；"结构化"在这里指**统一的 `key=value` 文本格式**）。
- 不重构 metadata 模块的包结构 / Identifier 系统 / ServiceDefinition / 重试调度器（那是 #3188 讨论的更大重构，本 issue 只做观测）。
- 不改变服务发现的功能行为（除了少量"返回错误而非静默 nil"的健壮性修复）。

---

## 2. 前置知识（开发前你需要了解的）

> 这一节是给第一次接触这块代码的同学的"地图"。如果你已经熟悉应用级服务发现，可直接跳到 §3。

### 2.1 接口级 vs 应用级服务发现

- **接口级（老模型）**：注册中心里以"接口"为粒度注册 URL，consumer 直接按接口订阅 URL 列表。注册数据量大。
- **应用级（现模型）**：注册中心里只注册"应用实例"（host:port + 一份元数据摘要）。一个应用的所有接口共用一条实例记录。consumer 要拿到接口对应的 URL，需要两步：
  1. 通过 **service-app mapping** 知道"我要的接口 `X` 是由哪些应用 `[A, B]` 提供的"。
  2. 订阅这些应用的实例列表，再从每个实例的 **MetadataInfo** 里还原出接口的真实 URL。

> 这就是为什么 metadata 链路一旦出问题，consumer 就会"订阅不到服务"——它根本拼不出 URL。

### 2.2 应用级 metadata 链路的五个阶段（核心心智模型）

```
┌─────────────────────────── Provider 侧（启动/导出）───────────────────────────┐
│ ① 组装本地 MetadataInfo        metadata/metadata.go  AddService()              │
│ ② 计算 revision                customizer/service_revision_customizer.go       │
│ ③ 发布元数据：                                                                 │
│    ├─ (3a) 写 metadata center  report_instance.go PublishAppMetadata()         │
│    └─ (3b) 导出 RPC MetadataService（provider 把自己的 MetadataInfo 暴露成 RPC）│
│ ④ 注册 service-app mapping     mapping Map() → RegisterServiceAppMapping()      │
└────────────────────────────────────────────────────────────────────────────┘
                                   │
                       注册中心（实例 + revision 摘要）
                                   │
┌─────────────────────────── Consumer 侧（订阅）────────────────────────────────┐
│ Ⓐ 查 mapping + 监听变化   mapping Get() → GetServiceAppMapping(.., listener)    │
│ Ⓑ 收到实例变更事件        service_instances_changed_listener_impl.go OnEvent()  │
│ Ⓒ 按 revision 查本地缓存   GetMetadataInfo(): metaCache 命中? ── hit ──► 直接用   │
│                                                 └─ miss ─┐                      │
│ Ⓓ 缓存未命中，拉取 MetadataInfo：                        ▼                      │
│    ├─ storageType=remote  GetMetadataFromMetadataReport() → GetAppMetadata()    │
│    └─ storageType=local   GetMetadataFromRpc()  → buildURL + RPC 调 provider    │
│ Ⓔ 用 MetadataInfo 还原 URL，notify directory（toInstanceServiceURLs）           │
└────────────────────────────────────────────────────────────────────────────┘
```

**每个阶段可能的失败，正好对应 Issue 要求的 5 类错误**：

| # | 失败点 | 错误分类（本次定义） |
|---|---|---|
| 3a/Ⓓ-remote | 写/读 metadata center 失败 | `metadata_report` |
| 3b/Ⓓ-local | RPC 调 provider 的 MetadataService 失败（连不上 / invoke 报错 / 返回 nil / 类型不符 / JSON 解析失败） | `rpc_metadata` |
| Ⓓ-local | 构造 metadata service URL 失败（实例元数据里没有 protocol，`buildStandardMetadataServiceURL` 返回 nil） | `url_construction` |
| Ⓓ/Ⓒ | 拉回来的 MetadataInfo.revision 与请求的 revision 不一致 | `revision_mismatch` |
| ④/Ⓐ | mapping 注册/查询/移除失败（无 report 实例、report 报错） | `mapping` |

> **把这张表记牢**——它是整个设计的骨架。指标的 label、错误的 category、日志的字段，全部围绕它展开。

### 2.3 涉及的核心数据结构

```go
// metadata/info/metadata_info.go
type MetadataInfo struct {
    App      string                  // 应用名
    Revision string                  // 元数据摘要（crc32 聚合）
    Services map[string]*ServiceInfo // matchKey -> 服务信息
    // ...
}

// metadata/report/report.go —— 远端 metadata 报告统一接口
type MetadataReport interface {
    GetAppMetadata(application, revision string) (*info.MetadataInfo, error)
    PublishAppMetadata(application, revision string, info *info.MetadataInfo) error
    RegisterServiceAppMapping(interfaceName, group, application string) error
    GetServiceAppMapping(interfaceName, group string, l mapping.MappingListener) (*gxset.HashSet, error)
    RemoveServiceAppMappingListener(interfaceName, group string) error
}
```

`DelegateMetadataReport`（`report_instance.go`）是上面接口的**包装层**，所有真实实现（nacos/zk/etcd）调用都经过它——所以它是做指标埋点的**最佳统一入口**。目前它只对 `PublishAppMetadata`(push) 和 `GetAppMetadata`(sub) 埋了点，**mapping 三个方法是裸调用**。

### 2.4 revision 是怎么算出来的

`registry/servicediscovery/customizer/service_revision_customizer.go::resolveRevision`：

```go
// 对每个导出 URL 生成 "app+path+version+port[+method]" 描述串，排序后逐个 crc32 求和
// 思路对齐 dubbo-java org.apache.dubbo.metadata.Metadata#calAndGetRevision
```

- provider 把算出的 revision 放进**实例元数据**（`ExportedServicesRevisionPropertyName`）。
- consumer 拿到实例后，用这个 revision 作为 **缓存 key** 和 **拉取参数**。
- revision = `"0"` 表示该实例没有有效服务元数据（当前代码会跳过）。

> **`revision_mismatch` 的含义**：consumer 用 revision `R` 去拉，但 provider 回了一份 `MetadataInfo.Revision != R` 的数据。这通常意味着 provider 重启换了 revision、或老版本 Java 行为不一致、或缓存串号。当前代码**完全没有校验这一点**，是个观测盲区。

### 2.5 dubbo-go 的 metrics 子系统（重点）

理解这套机制是写指标的前提。它是**事件驱动**的：

```
业务代码                         metrics 总线              collector(订阅者)            registry(Prometheus)
─────────                       ───────────              ──────────────              ───────────────────
NewMetadataMetricTimeEvent() ─┐
   ...do work...               │  metrics.Publish(e) ──► chan ──► 按 event.Name 分发 ──► StateCount()/Rt()
event.Succ = ...               │                                  handleXxx(event)       └─► 暴露给 /metrics
event.End = time.Now()        ─┘
```

关键类型（`metrics/api.go`, `metrics/common.go`, `metrics/metadata/*`）：

| 概念 | 说明 |
|---|---|
| `MetricKey{Name, Desc}` | 一个指标的定义（名字 + 帮助文本） |
| `MetricLevel` | 一组 label。`ApplicationMetricLevel`（app/ip/host/version）、`ServiceMetricLevel`（多一个 `interface`） |
| `BaseCollector.StateCount(total, succ, fail, level, ok)` | 一次性给 `total+1` 且 `succ/fail` 二选一 +1 |
| `c.R.Rt(NewMetricId(key, level), &RtOpts{}).Observe(ms)` | 记录一次耗时 |
| `MetadataMetricEvent{Name, Succ, Start, End, Attachment}` | metadata 域的事件载体；`Attachment` 是 `map[string]string`，可放 interface/group/storageType 等 |
| `MetadataMetricCollector` | 订阅 `constant.MetricsMetadata` 事件，按 `event.Name` 分发处理 |

**现有 metadata 指标**（`metrics/metadata/metric_set.go`）：

```
dubbo_metadata_push_num_{total,succeed,failed}      + dubbo_push_rt_milliseconds        (app 级)
dubbo_metadata_subscribe_num_{total,succeed,failed} + dubbo_subscribe_rt_milliseconds   (app 级)
dubbo_metadata_store_provider_{...}                 + dubbo_store_provider_interface_rt (service 级)
dubbo_subscribe_service_rt_milliseconds
```

> 我们要**新增** mapping 与 cache 指标，复用同一套 event/collector 机制——**不要造新轮子**。

### 2.6 dubbo-go 的 logger

- 来自 `github.com/dubbogo/gost/log/logger`，是 **printf 风格**：`logger.Infof / Warnf / Errorf`，**没有** `Infow(msg, "k", v)` 这种结构化 API。
- 仓库里另有 `logger.CtxLogger`（#3195）支持 `CtxInfof(ctx, ...)`，用于把 OpenTelemetry trace 信息带进日志。
- 因此本设计里的"结构化日志" = **约定一个统一的 `key=value` 文本格式**，让 Loki/ELK 能正则解析。我们会提供一个小helper 来保证格式一致。

### 2.7 Go 错误处理基础（实现错误分类要用）

```go
import "errors"

// 自定义错误实现 Unwrap，就能配合 errors.Is / errors.As 使用
type MetadataError struct { Category Category; ... ; Err error }
func (e *MetadataError) Unwrap() error { return e.Err }

var me *MetadataError
if errors.As(err, &me) {        // 取出分类
    switch me.Category { ... }
}
```

- `errors.As(err, &target)`：沿 `Unwrap` 链找到第一个能赋值给 `target` 的错误。
- 仓库现在用的是 `github.com/pkg/errors`（`perrors.New/Errorf/Wrap`）。我们的 `MetadataError` 要兼容标准库 `errors.As`/`Is`，所以**自己实现 `Unwrap()`**即可，不依赖 pkg/errors 的链。

---

## 3. 现状分析（Gap Analysis）

下面这张表是"现在能看到什么 / 看不到什么"的逐项盘点，也是验收时的对照基线。

| 链路阶段 | 操作 / 函数 | 指标 | 日志 | 错误分类 | 现状问题 |
|---|---|:--:|:--:|:--:|---|
| 发布元数据 | `PublishAppMetadata` | ✅ push | ❌ | ❌ | 已有指标，但失败时无分类日志 |
| 拉取元数据(report) | `GetAppMetadata` | ✅ sub | ❌ | ❌ | 同上 |
| mapping 注册 | `RegisterServiceAppMapping` / `Map` | ❌ | ❌ | ❌ | 硬编码 `retryTimes=10`、无退避、无间隔、重试全程无日志 |
| mapping 查询 | `GetServiceAppMapping` / `Get` | ❌ | ❌ | ❌ | 失败仅返回裸 error |
| mapping 监听 | `GetServiceAppMapping` 的 listener | ❌ | ❌ | ❌ | 完全不可观测 |
| mapping 移除 | `RemoveServiceAppMappingListener` / `Remove` | ❌ | ❌ | ❌ | 同上 |
| RPC 拉取 | `GetMetadataFromRpc` | ❌ | ⚠️ 散乱 `[MetadataRPC]` | ❌ | 日志前缀不统一、无指标 |
| URL 构造 | `buildStandardMetadataServiceURL` | ❌ | ❌ | ❌ | **返回 nil 后调用方 `url.SetParam` 会 panic**（隐藏 bug） |
| revision 校验 | （无） | ❌ | ❌ | ❌ | 拉回的 metadata 不校验 revision 是否匹配 |
| 缓存 | `GetMetadataInfo` 的 `metaCache` | ❌ | ❌ | — | hit/miss 完全不可见 |
| 实例变更 | `OnEvent` | ❌ | ⚠️ 部分 Infof | ❌ | 有零散日志但无指标、字段不统一 |

**一句话结论**：除了 push/sub 两个老指标，整条链路基本"裸奔"，且有一个 URL 构造返回 nil 导致的潜在 panic。

---

## 4. 设计方案

### 4.1 总览：三支柱 + 统一上下文

所有改动共享一组**统一上下文字段**（贯穿 error / metric label / log field）：

| 字段 | 含义 | 来源 |
|---|---|---|
| `app` | 应用名 | URL/instance |
| `revision` | 元数据摘要 | instance 元数据 |
| `serviceKey`（或 `interface`+`group`） | 接口标识 | URL |
| `registryId` | 注册中心 id | report 实例 key |
| `storageType` | `local` / `remote` | instance 元数据 `MetadataStorageTypePropertyName` |
| `source` | `report` / `rpc` | 拉取路径 |
| `errorCategory` | 5 类之一 | 错误分类 |

> 统一字段名是关键：这样一条 trace 里 metric 的 label、日志的 key、错误信息里的 kv 才能**对得上**，排查时一搜到底。

### 4.2 支柱一：错误语义（typed errors）

新增 `metadata/errors.go`：

```go
package metadata

import (
    "fmt"
    "sort"
    "strings"
)

// Category 是应用级 metadata 链路的错误分类。
type Category string

const (
    CategoryMetadataReport   Category = "metadata_report"   // 读写 metadata center 失败
    CategoryRPCMetadata      Category = "rpc_metadata"      // RPC MetadataService 失败
    CategoryURLConstruction  Category = "url_construction"  // 构造 metadata service URL 失败
    CategoryRevisionMismatch Category = "revision_mismatch" // 拉回的 revision 与请求不一致
    CategoryMapping          Category = "mapping"           // service-app mapping 失败
    CategoryUnknown          Category = "unknown"
)

// Error 携带分类 + 操作名 + 上下文 + 原始错误。
type Error struct {
    Category Category
    Op       string            // 例如 "GetAppMetadata" / "RegisterServiceAppMapping"
    Context  map[string]string // app/revision/serviceKey/registryId/storageType/...
    Err      error             // 被包装的底层错误，可为 nil
}

func (e *Error) Error() string {
    var b strings.Builder
    fmt.Fprintf(&b, "[metadata][%s] op=%s", e.Category, e.Op)
    // 稳定排序，保证日志/测试可复现
    keys := make([]string, 0, len(e.Context))
    for k := range e.Context {
        keys = append(keys, k)
    }
    sort.Strings(keys)
    for _, k := range keys {
        fmt.Fprintf(&b, " %s=%s", k, e.Context[k])
    }
    if e.Err != nil {
        fmt.Fprintf(&b, " err=%v", e.Err)
    }
    return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// ---- 构造器（每类一个，调用点更简洁）----

func newError(cat Category, op string, err error, ctx map[string]string) *Error {
    return &Error{Category: cat, Op: op, Err: err, Context: ctx}
}

func NewMappingError(op string, err error, ctx map[string]string) *Error {
    return newError(CategoryMapping, op, err, ctx)
}
func NewReportError(op string, err error, ctx map[string]string) *Error { /* ... */ }
func NewRPCMetadataError(op string, err error, ctx map[string]string) *Error { /* ... */ }
func NewURLConstructionError(op string, ctx map[string]string) *Error { /* ... */ }
func NewRevisionMismatchError(op string, want, got string, ctx map[string]string) *Error { /* ... */ }

// CategoryOf 从任意 error 中提取分类（供测试和指标 label 使用）。
func CategoryOf(err error) Category {
    var me *Error
    if errors.As(err, &me) {
        return me.Category
    }
    return CategoryUnknown
}
```

**设计要点**：

- 实现 `Unwrap()` → 兼容标准库 `errors.Is/As`，且不破坏现有 `perrors` 链。
- `Error()` 字段**稳定排序** → 日志可解析、单测可断言。
- `CategoryOf(err)` 同时服务于"指标打 `errorCategory` label"和"测试断言分类"。

> **决策点（建议默认采用）**：用"单一 `Error` 结构体 + `Category` 字段"，而不是"每类一个 sentinel error"。理由：链路里需要携带大量上下文（revision、host 等），sentinel error 表达不了；`Category` 枚举又足够测试断言。若 reviewer 更偏好 sentinel，可额外导出 `var ErrRevisionMismatch = &Error{Category: CategoryRevisionMismatch}` 配合 `errors.Is` 兜底。

### 4.3 支柱二：指标

#### 4.3.1 扩展 `metrics/metadata/metric_set.go`

新增 `MetricName`：

```go
const (
    MetadataPush MetricName = iota
    MetadataSub
    StoreProvider
    SubscribeServiceRt
    // ↓ 本次新增
    MappingRegister
    MappingGet
    MappingListen
    MappingRemove
    MetadataCacheHit
    MetadataCacheMiss
)
```

新增 `MetricKey`（沿用既有命名规范 `dubbo_metadata_*_{total,succeed,failed}` 与 `dubbo_*_rt_milliseconds`）：

```go
const (
    dubboMappingRegister = "dubbo_metadata_mapping_register"
    dubboMappingGet      = "dubbo_metadata_mapping_get"
    dubboMappingListen   = "dubbo_metadata_mapping_listen"
    dubboMappingRemove   = "dubbo_metadata_mapping_remove"
    dubboMetadataCache   = "dubbo_metadata_cache"
)

var (
    // mapping register（app/service 级，建议 service 级带 interface label）
    mappingRegisterNum     = metrics.NewMetricKey(dubboMappingRegister+totalSuffix, "Total Mapping Register Num")
    mappingRegisterSucceed = metrics.NewMetricKey(dubboMappingRegister+succSuffix, "Succeed Mapping Register Num")
    mappingRegisterFailed  = metrics.NewMetricKey(dubboMappingRegister+failedSuffix, "Failed Mapping Register Num")
    mappingRegisterRt      = metrics.NewMetricKey(dubboMappingRegister+"_rt_milliseconds", "Mapping Register RT")
    // get / listen / remove 同构 ……

    // cache hit/miss（app 级 counter）
    metadataCacheHit  = metrics.NewMetricKey(dubboMetadataCache+"_hit_total", "Metadata Cache Hit Num")
    metadataCacheMiss = metrics.NewMetricKey(dubboMetadataCache+"_miss_total", "Metadata Cache Miss Num")
)
```

> label 维度：mapping 类用 `ServiceMetricLevel`（带 `interface`），便于按接口排查；cache 类用 `ApplicationMetricLevel`。失败计数额外带一个 `error_category` label（通过 `NewMetricIdByLabels` 注入），这样 Prometheus 里能直接 `sum by (error_category)`。

#### 4.3.2 扩展 `metrics/metadata/collector.go`

```go
switch event.Name {
case MetadataPush:       c.handleMetadataPush(event)
case MetadataSub:        c.handleMetadataSub(event)
case StoreProvider:      c.handleStoreProvider(event)
case SubscribeServiceRt: c.handleSubscribeService(event)
// ↓ 新增
case MappingRegister:    c.handleMapping(event, mappingRegisterNum, mappingRegisterSucceed, mappingRegisterFailed, mappingRegisterRt)
case MappingGet:         c.handleMapping(event, mappingGetNum, ...)
case MappingListen:      c.handleMapping(event, ...)
case MappingRemove:      c.handleMapping(event, ...)
case MetadataCacheHit:   c.R.Counter(metrics.NewMetricId(metadataCacheHit, metrics.GetApplicationLevel())).Inc()
case MetadataCacheMiss:  c.R.Counter(metrics.NewMetricId(metadataCacheMiss, metrics.GetApplicationLevel())).Inc()
}

func (c *MetadataMetricCollector) handleMapping(e *MetadataMetricEvent, num, succ, fail, rt *metrics.MetricKey) {
    level := metrics.NewServiceMetric(e.Attachment[constant.InterfaceKey])
    c.StateCount(num, succ, fail, level, e.Succ)
    c.R.Rt(metrics.NewMetricId(rt, level), &metrics.RtOpts{}).Observe(e.CostMs())
}
```

#### 4.3.3 埋点入口：`report_instance.go`

把 mapping 三个委托方法改成和 push/sub 一样的"包事件"写法：

```go
func (d *DelegateMetadataReport) RegisterServiceAppMapping(interfaceName, group, application string) error {
    event := metadataMetrics.NewMetadataMetricTimeEvent(metadataMetrics.MappingRegister)
    event.Attachment[constant.InterfaceKey] = interfaceName
    event.Attachment[constant.GroupKey] = group
    err := d.instance.RegisterServiceAppMapping(interfaceName, group, application)
    event.Succ = err == nil
    event.End = time.Now()
    metrics.Publish(event)
    return err
}
// GetServiceAppMapping → MappingGet（若传入 listener 非 nil，再额外 Publish 一个 MappingListen 事件）
// RemoveServiceAppMappingListener → MappingRemove
```

> **为什么放这里**：`DelegateMetadataReport` 是所有 report 实现的唯一入口，埋一次覆盖 nacos/zk/etcd 全部实现，且与现有 push/sub 风格完全一致。

#### 4.3.4 cache 埋点：`GetMetadataInfo`（listener 文件）

```go
if metadataInfo, ok := metaCache.Get(revision); ok {
    metrics.Publish(metadataMetrics.NewMetadataMetricTimeEvent(metadataMetrics.MetadataCacheHit))
    return metadataInfo.(*info.MetadataInfo), nil
}
metrics.Publish(metadataMetrics.NewMetadataMetricTimeEvent(metadataMetrics.MetadataCacheMiss))
// ... 继续拉取 ...
```

### 4.4 支柱三：结构化日志

新增 `metadata/observability.go`，提供统一格式 helper：

```go
// fields 渲染成稳定排序的 "k=v k=v" 串，配合统一前缀使用。
func fields(kv map[string]string) string { /* sort keys, join "k=v" */ }

// 统一前缀，便于按阶段 grep：
//   [Metadata][Mapping]  [Metadata][Report]  [Metadata][RPC]  [Metadata][Cache]  [Metadata][Listener]
```

日志规范（示例）：

```go
// 成功（debug/info 级，避免刷屏，热路径用 Debug）
logger.Infof("[Metadata][Mapping] %s", fields(map[string]string{
    "op": "register", "result": "ok", "app": appName,
    "interface": serviceInterface, "registryId": id, "cost_ms": ...,
}))

// 失败（warn/error 级，必带 error_category）
logger.Errorf("[Metadata][RPC] %s", fields(map[string]string{
    "op": "getMetadataInfo", "result": "fail", "error_category": string(CategoryRPCMetadata),
    "host": url.Location, "revision": revision, "err": err.Error(),
}))
```

**落点**：

- `mapping/.../service_name_mapping.go`：`Map/Get/Remove` 成功/失败、**每次重试**都打日志（带第几次重试、剩余次数）。
- `client.go`：把现有散乱的 `[MetadataRPC]` 统一成 `[Metadata][RPC]` 前缀 + `fields()`；URL 构造失败、nil 返回、类型不符、JSON 解析失败各打一条带 `error_category` 的日志。
- `service_instances_changed_listener_impl.go`：cache hit/miss（debug）、revision 处理、拉取失败（warn，带 category）统一格式。

> 注意：mapping 包当前在 `package metadata`（子目录），但 import 路径是 `dubbo.apache.org/dubbo-go/v3/metadata`——helper 放在主 `metadata` 包即可被复用；mapping 实现文件已 `import "dubbo.apache.org/dubbo-go/v3/metadata"`。

### 4.5 逐文件改造清单（含健壮性修复）

#### (A) `metadata/mapping/metadata/service_name_mapping.go`

- `Map()`：保留重试，但
  - 把硬编码 `retryTimes=10` 提为可读常量并加注释（间隔/退避可作为后续 issue，本次至少**每次重试打日志**）。
  - 无 report 实例 / 重试耗尽 → 返回 `NewMappingError("Map", err, {interface, app, group})`。
- `Get()` / `Remove()`：无 report 实例 / report 报错 → 包成 `NewMappingError`。

#### (B) `metadata/client.go`

- `buildStandardMetadataServiceURL`：当前 protocol 为空时返回 `nil`。**修复**：让调用方 `GetMetadataFromRpc` 在拿到 nil 时返回 `NewURLConstructionError(...)`，**避免对 nil URL 调 `SetParam` 触发 panic**（潜在 bug）。
- `GetMetadataFromRpc`：refer 失败 / invoke 失败 / nil / 类型不符 / JSON 解析失败 → 统一包 `NewRPCMetadataError(...)`，日志统一前缀。
- `GetMetadataFromMetadataReport`：无 report 实例 / `GetAppMetadata` 失败 → 包 `NewReportError(...)`。
- 两条路径拿到 `MetadataInfo` 后，**新增 revision 校验**：`got.Revision != "" && got.Revision != revision` → 记 `revision_mismatch`（默认仅告警+指标，不阻断，保持行为兼容；是否阻断作为决策点）。

#### (C) `registry/servicediscovery/service_instances_changed_listener_impl.go`

- `GetMetadataInfo`：补 cache hit/miss 指标 + debug 日志；按 `storageType` 在日志里标 `source=report|rpc`；拉取失败时日志带 `CategoryOf(err)`。
- `OnEvent`：把零散 `Infof/Warnf` 收敛到统一格式（带 `app/service/revision/instanceCount`）。

#### (D) `metadata/report_instance.go`

- 见 §4.3.3：mapping 三方法补指标。push/sub 失败时也补一条带 category 的 warn 日志（可选）。

### 4.6 端到端示例：consumer 订阅不到服务，怎么定位

改造后，排查动作：

1. **看指标**（Prometheus / Grafana）：
   - `dubbo_metadata_mapping_get_failed_total > 0` 且 `error_category="mapping"` → 第一步 mapping 查询就挂了（多半 metadata center 连不上 / 没数据）。
   - mapping 正常，但 `dubbo_metadata_subscribe_num_failed_total`（report 路径）或 RPC 路径失败上升 → 是拉 MetadataInfo 挂了；再看 `error_category` 区分 `metadata_report` / `rpc_metadata` / `url_construction`。
   - `dubbo_metadata_cache_miss_total` 一直涨且伴随拉取失败 → 缓存永远填不进去。
   - 出现 `revision_mismatch` → provider 在重启/灰度，consumer 缓存与实例 revision 串号。
2. **按前缀 grep 日志**：`[Metadata][Mapping]` / `[Metadata][RPC]` / `[Metadata][Report]`，每条都带 `app/revision/interface/registryId`，直接锁定是哪个 app、哪个接口、哪个注册中心。

> 这正是 Issue 里"难以判断根因是 mapping、metadata report、RPC metadata service、revision 还是 cache"的直接答案。

---

## 5. 开发步骤（建议分 PR / commit）

> 每一步都能独立编译、独立测试、独立 review。建议拆成 4 个小 commit 甚至 2~3 个 PR。

### Phase 0：准备（0.5 天）
- [ ] 通读 §2 列出的 4 个核心文件 + metrics 子系统。
- [ ] 本设计文档评审通过（贴到 issue #3356 下让 maintainer 确认范围与 §4.2 / §4.5 的决策点）。

### Phase 1：错误语义（1 天）
- [ ] 新增 `metadata/errors.go`（`Category` / `Error` / 构造器 / `CategoryOf`）。
- [ ] 改造 `mapping/.../service_name_mapping.go`、`client.go` 的错误返回为分类错误。
- [ ] 修复 `buildStandardMetadataServiceURL` 返回 nil 的 panic 隐患（URL 构造失败走 `url_construction`）。
- [ ] 新增 revision 校验逻辑（`revision_mismatch`）。
- [ ] 单测：`errors.As` 能取出每一类 category；URL 构造失败不再 panic。

### Phase 2：指标（1 天）
- [ ] 扩展 `metrics/metadata/metric_set.go`（新增 MetricName + MetricKey）。
- [ ] 扩展 `metrics/metadata/collector.go`（handle mapping + cache）。
- [ ] `report_instance.go` 三个 mapping 方法补埋点。
- [ ] `GetMetadataInfo` 补 cache hit/miss 埋点。
- [ ] 单测：构造事件 → 经 collector → 断言对应 counter/rt 被记录（参考 `collector_test.go`）。

### Phase 3：结构化日志（0.5 天）
- [ ] 新增 `metadata/observability.go`（`fields()` + 前缀常量）。
- [ ] 4 个文件按统一格式补/改日志（成功 info/debug，失败 warn/error 带 `error_category`）。

### Phase 4：失败路径测试 + 文档（1 天）
- [ ] 失败路径单测（见 §6）。
- [ ] 在 issue / 本文件补一份"排查手册"（§4.6 那张决策流程）。
- [ ] `go test ./metadata/... ./metrics/... ./registry/servicediscovery/...` 全绿；`make license`/`make verify`（如有）通过。

---

## 6. 测试方案

> Issue 明确要求"增加失败路径测试，验证错误信息或错误分类"。这是验收硬指标。

### 6.1 错误分类单测（`metadata/errors_test.go`）

```go
func TestCategoryOf(t *testing.T) {
    err := NewRevisionMismatchError("GetMetadataFromRpc", "r1", "r2", map[string]string{"host": "1.2.3.4"})
    assert.Equal(t, CategoryRevisionMismatch, CategoryOf(err))

    // 经过 fmt.Errorf("%w") 包裹后仍可识别
    wrapped := fmt.Errorf("outer: %w", err)
    assert.Equal(t, CategoryRevisionMismatch, CategoryOf(wrapped))

    // 非本类错误
    assert.Equal(t, CategoryUnknown, CategoryOf(errors.New("x")))
}
```

### 6.2 失败路径单测（每类至少一个）

| 用例 | 断言 |
|---|---|
| `ServiceNameMapping.Map` 无 report 实例 | 返回 `CategoryMapping`，错误串含 `op=Map` |
| `GetMetadataFromMetadataReport` 无 report 实例 | 返回 `CategoryMetadataReport` |
| `GetMetadataFromRpc` 的 invoker invoke 报错（mock invoker） | 返回 `CategoryRPCMetadata` |
| 实例元数据无 protocol → `buildStandardMetadataServiceURL` 返回 nil | 返回 `CategoryURLConstruction`，**且不 panic** |
| mock provider 返回 `Revision != 请求 revision` | 记到 `revision_mismatch`（或返回该分类） |

> mock 手段：`extension.SetMetadataReportFactory` / mock `report.MetadataReport`；RPC 路径 mock `base.Invoker`（仓库已有 mock invoker 模式，可参考 `protocol/` 下测试）。

### 6.3 指标单测（`metrics/metadata/collector_test.go` 扩展）

```go
func TestMappingRegisterMetric(t *testing.T) {
    // 起 collector（参考现有 test 的 setup），Publish 一个 MappingRegister 成功事件，
    // 断言 mappingRegisterNum/_succeed_total 计数 +1，rt 被 observe。
}
func TestCacheHitMissMetric(t *testing.T) { /* Publish hit/miss，断言 counter */ }
```

### 6.4 回归
- `go test ./metadata/... ./metrics/... ./registry/servicediscovery/...`
- 确认未改变服务发现正常路径行为（已有 listener 测试 `..._impl_test.go` 仍绿）。

---

## 7. 验收标准（Definition of Done）

**功能性**

- [ ] mapping 的 `register / get / listen / remove` 四个操作均有：成功/失败计数指标 + 耗时指标 + 统一格式日志。
- [ ] cache `hit / miss` 有计数指标；日志能看出某次 `GetMetadataInfo` 命中缓存还是回源。
- [ ] 拉取路径在日志/指标中可区分 `source=report|rpc`、`storageType=local|remote`、`revision`。
- [ ] 5 类错误（`metadata_report` / `rpc_metadata` / `url_construction` / `revision_mismatch` / `mapping`）均可通过 `CategoryOf(err)` 程序化识别。
- [ ] 失败错误信息统一携带 `app / revision / serviceKey(or interface) / registryId / storageType` 等上下文。

**质量性**

- [ ] `buildStandardMetadataServiceURL` 返回 nil 的 panic 隐患被修复（有测试覆盖）。
- [ ] 每类错误至少 1 个失败路径单测；`CategoryOf` 单测覆盖含 `%w` 包裹场景。
- [ ] 新增指标在 collector 单测中验证可被正确记录。
- [ ] `go test ./metadata/... ./metrics/... ./registry/servicediscovery/...` 全部通过，无 data race（`-race`）。
- [ ] 不引入新依赖；不改变正常服务发现行为；所有新文件含 Apache License 头。

**可观测性目标（人工验收）**

- [ ] 拿一个"consumer 订阅不到服务"的故障场景（如停掉 metadata center / 让 provider 返回 nil metadata），仅凭新增的指标 + 日志，能在不改代码的情况下指出失败发生在 5 个阶段中的哪一个。
- [ ] Grafana / `/metrics` 中能看到新增的 `dubbo_metadata_mapping_*` 与 `dubbo_metadata_cache_*` 指标。

---

## 8. 风险与兼容性

| 风险 | 说明 | 缓解 |
|---|---|---|
| 日志刷屏 | 热路径（OnEvent/cache）频繁打日志 | 成功路径用 `Debugf`；失败/异常才 `Warn/Error` |
| 指标基数爆炸 | mapping 指标带 `interface` label，接口多时 series 多 | 与现有 `store_provider` 同级别，可接受；`error_category` 取值固定 5 类，不爆 |
| 行为变更 | revision_mismatch 若直接 return 会改变发现行为 | **默认只告警+指标，不阻断**（决策点，见 §4.5(B)） |
| 包结构 | mapping 子目录包名为 `metadata` 易混淆 | 本次不动包结构（属于 #3188 范畴），仅复用主包 helper |

---

## 9. 决策点汇总（需 maintainer 确认）

1. **错误模型**：单一 `Error{Category}` 结构体（推荐） vs sentinel errors。→ §4.2
2. **revision_mismatch 处理**：仅告警+指标（推荐，行为兼容） vs 直接判错返回。→ §4.5(B)
3. **mapping 指标维度**：service 级（带 `interface`，推荐） vs app 级。→ §4.3.1
4. **重试增强**：本次是否顺带把硬编码 `retryTimes=10` 改为可配置退避（建议**仅加日志**，可配置退避另开 issue 对齐 #3188）。→ §4.5(A)

---

## 10. 参考资料

- Issue #3356（本任务）、#3188（metadata 模块历史与差异讨论）、#2534（metadata 瘦身 PR）、#2432（重构 draft）
- 代码：`metrics/metadata/{collector,metric_set}.go`、`metrics/registry/*`（更完整的 collector 范例）、`metrics/api.go`、`metrics/common.go`
- dubbo-java：`org.apache.dubbo.metadata.MetadataInfo` / `Metadata#calAndGetRevision` / `AbstractMetadataReport`（错误与重试语义参考）

---

## 附录 A：开发自查清单

```
[ ] metadata/errors.go        新增并通过单测（5 类 + CategoryOf + %w 包裹）
[ ] metadata/observability.go fields() helper + 前缀常量
[ ] metrics/metadata/metric_set.go  +MappingRegister/Get/Listen/Remove +CacheHit/Miss + keys
[ ] metrics/metadata/collector.go   +handleMapping +cache 分支
[ ] report_instance.go        mapping 三方法埋点
[ ] mapping/.../service_name_mapping.go  分类错误 + 重试日志
[ ] client.go                 URL 构造修复 + RPC/report 错误分类 + revision 校验 + 日志统一
[ ] listener_impl.go          cache hit/miss 埋点 + OnEvent 日志统一
[ ] *_test.go                 失败路径 + 指标 + race
[ ] License 头 / gofmt / go vet
```
