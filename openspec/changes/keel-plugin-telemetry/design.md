# Design: keel-plugin-telemetry

## 1. 三层与现状

| 层 | 职责 | 现状 | 本 change |
|---|---|---|---|
| `internal/telemetry` | PI 契约：`Context`/`Span`/`Metrics`/`Store`、Memory/Noop/OTLP adapter | 已实现，OTLP 仅在 `Shutdown` 全量 POST | 补增量批量/边界/重试；契约形状不变 |
| `internal/metrics` | 固定基数 Prometheus 运维面（`/metrics`） | 已实现；标签白名单拒绝 tenant/session/run | **不改**；遥测插件不得写它 |
| `keel.telemetry` 插件 | 可选 exporter 生命周期、配置、审批、状态 | 不存在 | 新增，默认 disabled |
| 事件日志 | 业务事实（`run.finished`、`compaction` 等） | 已实现 | 不改；不是遥测替代 |

当前缺口（实现基线）：

- `servicehost.newTelemetryFactory` 按 tenant 文档/`KEEL_TELEMETRY` 直接选 adapter；启用
  OTLP 没有插件审批，也没有“插件未启用”的可见状态。
- `internal/telemetry/otlp.go` 的 `Shutdown` 每次导出全部内存 span/measurement：重复调用
  会重复导出，长跑进程没有周期导出。
- `agent.RunTelemetry` 的 span 带 tenant/session/model/provider；指标只带
  model/provider/outcome。没有统一 allowlist、采样、基数上限或 log 通道。
- `TurnTelemetry` 只发 hook，没有 telemetry log。

## 2. 信号与标识

| 信号 | 形状 | 允许的标识 | 禁止 |
|---|---|---|---|
| Span | `keel.agent.run`（attempt 根）、turn/tool/compaction 子 span；事件只带名称与有界属性 | `tenant`、`session`、`run`、`turn`、`tool`、`model`、`provider`、`outcome`、`status`、`kind`、`error.kind`、`http.status_code`、`mcp.server`、`plugin`、`command` | prompt、消息正文、工具参数/结果、文件/附件名、路径、URL query、secret、Authorization、原始异常文本 |
| Metric | 计数器/直方图/up-down；`keel.agent.runs`、`keel.agent.run.duration`、token 计量 | 标签只允许 `model`、`provider`、`outcome`、`status`、`tool`、`kind`、`result`、`api`、`stop` | tenant/session/run/request/trace/span 标签；这些只可作为 resource 或 span/log 属性 |
| Log | 结构化记录（level、event code、有界属性） | 同 span 的标识 + `level`、`event`、`logger` | 自由文本 message、正文、参数、堆栈、密钥 |

PI 的 span/metric 形状不因本 change 改变；`docs/PI-PARITY.md` §3 仍是唯一契约来源。

## 3. TelemetryHub（新 seam）

```text
servicehost telemetry service
  └─ TelemetryHub（进程级，绑定 tenant）
       ├─ base: Memory/Noop（现有行为）
       └─ exporters: 0..N（插件注册；每个 exporter 有独立配置/队列）
            └─ keel.telemetry → OTLP/HTTP JSON
```

- `agent.RunTelemetry.Context` 指向 hub 包装后的 `telemetry.Context`；runner/provider 代码
  不需要知道 exporter。
- 插件 `Start` 通过 `PluginEnv.Telemetry` 注册 exporter；`Stop` 注销并做有界 flush。注册
  发生在服务图创建之后也没问题：hub 是进程级对象，`Context()` 动态读取当前 exporter 集。
- 每个 span/metric/log 在产生时捕获当时的 exporter 集；停用不会把在途记录丢进新 exporter。
- hub 的扇出 MUST 不阻塞调用方：入队失败/队列满只计数并丢弃，绝不返回错误给 run。
- 单 tenant 进程只有一个 hub；多 tenant 进程 MUST 每 tenant 一个 hub。做不到时插件 MUST
  拒绝导出（fail closed），不得共享队列或 endpoint。

## 4. 插件与配置

官方 `keel.telemetry`（bundled、in-process、默认 **disabled**）：

```yaml
id: keel.telemetry
version: 1.0.0
kind: in-process
layer: harness
description: Optional OTLP exporter for PI telemetry signals.
provides:
  telemetry:
    exporters: [otlp-http]
    signals: [traces, metrics, logs]
```

配置仍来自 tenant 文档/环境（不新增第二份）：

```yaml
telemetry:
  adapter: otlp            # otlp | none | memory
  endpoint: https://collector.example:4318
  headers: { Authorization: "secret:OTLP_TOKEN" }   # 只引用 secret，不落明文
  service_name: keel
  signals: [traces, metrics, logs]                 # 可选；默认 traces+metrics
  sample_ratio: 1.0                                # 0..1
```

启用语义：

- 插件未启用：配置惰性；base 仍是 Memory/Noop；`/v1/telemetry/status` 显示
  `plugin_disabled`；MUST NOT 发出任何 collector 请求。
- 启用：`plugin.lifecycle.enable` + durable 审批，展示 exporter、signals、endpoint host、
  采样与是否需要 headers（值不可见）。
- `adapter` 缺失/`none`：插件可启用但 `signals=[]` 且状态 `inactive`；这是合法状态。
- `adapter=otlp` 但 endpoint 缺失/非法：插件 `FAILED`，不创建 exporter，不影响 run。
- 停用：注销 exporter、停止队列、flush 有界、释放 goroutine/连接/状态。`keel.telemetry`
  是 bundled 插件，MUST 只可停用、不可卸载（`ErrPluginBundled`）；停用即零残留。若未来以
  可安装包发布，卸载 MUST 与停用同样释放全部状态。

## 5. 导出管线

```text
record → allowlist/redaction → bounded queue → batch → POST /v1/{traces,metrics,logs}
                                                        ├ 2xx: 删除已确认批次
                                                        ├ 429: 按 Retry-After 退避
                                                        ├ 5xx/网络: 指数退避 + 熔断
                                                        └ 其它 4xx: 丢弃批次并计数
```

| 参数 | 默认 | 上限/语义 |
|---|---|---|
| 队列 | 8192 spans / 4096 metrics / 2048 logs | 字节上限 8 MiB；满时丢最旧并计数 |
| 批次 | 512 spans 或 1 MiB 或 5s | 单 POST ≤4 MiB，超出拆分 |
| 重试 | 5xx/429/网络 | 1s 起指数退避至 60s，最多 5 次；429 优先 `Retry-After` |
| 熔断 | 连续 5 次失败 | 打开 30s，半开只放一个探针 |
| 关机 | 5s deadline | flush 剩余；未发出计数，不阻塞进程退出 |
| 去重 | 每 exporter 一个确认游标 | 同一 exporter 生命周期内已 2xx 的批次不再导出；无持久队列，崩溃重启丢失未发出批次且不重放（at-most-once across restart）；存活 exporter 的“已发送但响应丢失”重试可能重复（collector 按 trace/span id 幂等） |

不使用 OpenTelemetry SDK（`scripts/binary-size.sh` 门）；OTLP/HTTP JSON 由现有零依赖
adapter 承担。

## 6. 采样

- span 头采样：`sample_ratio ∈ [0,1]`，对 trace id 做确定性哈希；根 span 决定，子 span
  继承；未采样时回调仍执行，但拿到 nop span。
- `sample_ratio=0`：只停 traces；metrics/logs 不受影响。
- metric 不采样（聚合计数必须完整）。
- log 按 level 过滤，并可用独立比例（默认 1.0）；错误级别不受比例影响，避免丢故障。
- 首版不做尾采样；错误 span 不保证被采样，文档必须写明。

## 7. 脱敏与基数

- **属性 allowlist**：只有 §2 的键可导出；未知键丢弃并计数（debug 日志一次）。字符串
  值 ≤128 字符，数组 ≤16 项，单记录属性 ≤32 个。
- **禁止**：prompt、message、tool args/result、附件/文件名、路径、URL query、secret、
  Authorization、cookie、原始异常文本。错误只导出 `error.kind`/`status`/HTTP 类。
- **metric 标签**：固定白名单；每族最多 1,000 series，全局最多 5,000；溢出丢弃并递增
  `telemetry_metric_series_dropped_total`，warn 一次。指标名必须在插件启动时注册，
  未注册名称丢弃。
- **headers**：从 secret store 解析；状态页只显示 header 名/数量，不显示值；日志不打印。
- 事件日志/审计不含遥测 payload。

## 8. 租户与故障隔离

- 每 tenant 独立 exporter 配置、队列、游标与状态；tenant id 作为 resource 属性或
  span/log 属性，MUST NOT 作为 metric 标签。
- 跨租户：A 的 endpoint 只收到 A 的信号；B 停用/失败不影响 A；跨租户读取 status 404。
- 导出失败（连接拒绝、超时、5xx、429、熔断）MUST NOT 传播到 `StartSpan`、metric 记录、
  run/tool 结果或 HTTP 响应；只增加计数与 `DEGRADED` 诊断。
- 插件初始化失败（endpoint 非法、secret 缺失）→ `FAILED`，不影响 harness；修复配置后
  重新启用即可恢复，无需重启进程。
- 队列/连接/goroutine 在停用（或可安装包的卸载）时释放；泄漏由 `go test -race` 与
  goroutine 计数钉住。

## 9. 与 `internal/metrics` / 事件日志的关系

| 面 | 用途 | 数据 | 谁写 |
|---|---|---|---|
| `/metrics`（`internal/metrics`） | 运维抓取/Prometheus | 固定基数计数器/直方图；无 tenant/session/run 标签 | 运行时、MCP、worker 等既有调用点 |
| `internal/telemetry` + 插件 | 追踪/租户内诊断/可选 OTLP | span/metric/log；span/log 可带身份 | runner/provider/turn/hub |
| 事件日志 | 会话事实/回放 | `run.finished`、`compaction` 等业务事件 | 内核 |

- 遥测插件 MUST NOT 写 `metrics.Registry`，MUST NOT 生成 second Prometheus endpoint，
  MUST NOT 做 remote-write。
- 每个指标族只有一个 owner；Prometheus 的 `keel_agent_*` 与 telemetry 的
  `keel.agent.*` 是两套命名空间，文档给出对照，不互相复制。
- `run.finished` 仍是业务汇总，不是 telemetry span 的替代；也不进模型上下文。

## 10. 兼容与迁移

- `internal/telemetry` 的 PI 契约与 Memory/Noop 行为不变；`OTLP.Shutdown` 保留为
  “flush 剩余”语义，并改为增量游标，重复调用不重复导出已确认批次。
- 现有 `KEEL_TELEMETRY`/tenant `telemetry.adapter` 仍是配置输入；差别是 **OTLP 需要插件
  启用**。未启用时配置惰性，状态页明确说明；不保留“配置即出网”的旧路径（alpha 零兼容）。
- 不新增 `/v1/traces` 之类的入站路由；只作为 OTLP 客户端出站。
- 状态接口：`GET /v1/telemetry/status`（`plugin.read` 或等效），返回
  `enabled/signals/endpointHost/sampleRatio/queueDepth/dropped/exported/lastErrorClass`；
  不返回 headers、endpoint query 或租户数据。

## 11. 非目标

- OTel SDK、OTLP/gRPC、自动插桩、尾采样、Prometheus remote-write；
- 日志聚合/告警/仪表盘产品、计费；
- 多租户共享 collector 拓扑；
- 把正文、工具参数/结果、附件或密钥纳入遥测。
