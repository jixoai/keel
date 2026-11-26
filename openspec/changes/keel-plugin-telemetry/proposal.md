# Proposal: keel-plugin-telemetry

> 立项：2026-11-26。基线：`docs/PI-PARITY.md` §3（`TelemetryContext`/`TelemetrySpan`/
> `TelemetryStore` + adapters）、`internal/telemetry`（PI 契约、Memory/Noop/OTLP）、
> `internal/metrics`（固定基数 Prometheus）、`internal/servicehost/services.go` 的
> `newTelemetryFactory`、`internal/agent/telemetry.go`、`internal/harnessext/turnwatch.go`。
> 参考：DSH `deepseek-ai/dsh-superagent-prototype@8c32a9dc` 没有 OTLP/遥测契约，只有面板
> 统计；本 change 不套用其面板口径，以 PI telemetry 为唯一标准。
> 定位：遥测是**可选的可观测导出插件**，不是第二套指标系统，也不是业务事件日志。

## Why

PI telemetry 契约已经在 `internal/telemetry` 落地，但产品化还差三件事：

- **可选且可审计**：当前由 tenant 文档/`KEEL_TELEMETRY` 配置直接选择 adapter；启用 OTLP
  没有插件审批、没有可见状态，也没有“默认关”的产品语义。
- **导出不完整**：`OTLP` 只在 `Shutdown` 时把内存里的 span/metric 一次性 POST；没有周期
  批量、队列上限、重试/退避、熔断或去重，长跑进程要么攒爆内存，要么退出时才导出。
- **边界未冻结**：没有属性 allowlist、采样、指标基数上限、正文/密钥脱敏与租户隔离的
  统一判据；`internal/metrics` 的固定基数 `/metrics` 与 PI telemetry 指标的关系也没有
  写清，容易被做成第三套指标。

下一波插件若各自接 OTLP，会重复 SDK、泄漏 prompt/工具参数、把 session/run 做成指标标签，
或让导出故障拖垮 run。本 change 把遥测收敛成一个默认关闭的官方插件。

## What Changes

1. **信号契约**：span（run/turn/tool/compaction）、metric（run/token/duration 等）、
   log（结构化字段）统一标识：span/log 可带 tenant/session/run/turn/tool；metric 标签
   只允许有界枚举，MUST NOT 带 tenant/session/run/request/trace/span。
2. **可选插件**：新增官方 `keel.telemetry`（in-process，bundled，**默认 disabled**）；
   `provides.telemetry` 声明支持的 exporter/signals；启用走 `plugin.lifecycle.enable` +
   durable 审批；停用即停止导出并释放资源（bundled 插件只可停用、不可卸载，停用即零残留）。
   未启用时现有配置惰性、可见但不出网。
3. **Hub seam**：宿主提供进程级 `TelemetryHub`，包装 PI `TelemetryContext`，向已注册
   exporter 有界扇出；插件在 `Start/Stop` 注册/注销；不依赖插件与服务图的启动顺序。
4. **OTLP/HTTP JSON**：复用零依赖 `internal/telemetry.NewOTLP`，补齐周期批量、批次/队列
   上限、重试/退避/`Retry-After`、熔断、关机 flush、已确认批次不重复导出；MUST NOT
   引入 OpenTelemetry SDK（二进制预算门）。
5. **采样**：span 头采样比例（0..1，默认 1.0，按 trace id 确定性）；子 span 继承父决策；
   metric 不采样；log 按级别/独立比例过滤。尾采样不在首版。
6. **脱敏与基数**：属性 allowlist；禁止 prompt/message/tool args/results/file name/path/
   URL query/secret/Authorization；错误只导出类别/状态；metric 标签白名单 + 每族 series
   上限，溢出丢弃并计数。
7. **租户与故障隔离**：每 tenant 独立 exporter 配置与队列；跨租户不共享信号；导出失败
   MUST NOT 阻塞 run/tool，队列满丢最旧并计数；插件诊断（`DEGRADED`/`FAILED`）可见，
   harness 不受影响。
8. **与 internal/metrics 的关系**：`/metrics` 仍是固定基数 Prometheus 运维面；遥测插件
   MUST NOT 写/复制它，每个指标族只有一个 owner；运行时指标（`RuntimeMetrics`）不变。
9. **审计与状态**：启用/停用/配置变更写审计；`GET /v1/telemetry/status`（或插件视图）
   给出 signals、endpoint host（脱敏）、采样、队列、溢出、导出行数、最近错误类别；
   不逐 span 写审计。

## Capabilities

### New Capabilities

- `plugin-telemetry`: PI 遥测契约的可选插件化、OTLP 导出、采样/脱敏/基数、租户与故障隔离，
  以及与 Prometheus `/metrics` 的边界。

### Modified Capabilities

<!-- 复用 docs/PI-PARITY.md 的 telemetry 契约与 ops-runtime 的指标 requirement；本 change
     只新增遥测插件 capability。若实现必须改既有 requirement 的正文，实现阶段补精确
     delta，本提案不虚构修改。 -->

## Impact

- `internal/telemetry`（OTLP 增量批量/重试/边界；不改变 PI 契约形状）、
  `internal/servicehost`（TelemetryHub 接线，替代直接 adapter 选择）、
  `internal/harnessext`（`PluginEnv.Telemetry`、插件状态）、`internal/pluginpkg`
  （`provides.telemetry` 与校验）、`cmd/keel`/`cmd/runtime`（配置映射）、
  `docs/HARNESS.md`/`docs/PI-PARITY.md`/`docs/RUNBOOK.md`。
- 不新增第二套指标 registry；不改变 `internal/metrics` 的固定标签白名单；
  事件日志与 `run.finished` 仍是业务汇总，不是遥测替代。
- 测试：假 OTLP collector（traces/metrics/logs）、队列/重试/熔断、采样确定性、
  脱敏/基数、租户隔离、插件生命周期、关机 flush、kill/重启。

## Out of scope

- OpenTelemetry SDK、OTLP/gRPC、自动插桩、尾采样、Prometheus remote-write。
- 日志聚合后端、仪表盘/告警产品、计费或按租户计量。
- 多租户部署下的共享 collector 拓扑；首版仍按进程/租户隔离，多租户进程 fail closed。
- 把 prompt、消息正文、工具参数/结果、附件内容或密钥纳入遥测。
