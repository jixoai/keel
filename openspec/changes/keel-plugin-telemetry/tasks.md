# Tasks: keel-plugin-telemetry

## 1. 契约与 Hub seam

- [x] 1.1 在 `internal/telemetry` 固化信号标识与 allowlist 常量：span/log 身份键、
  metric 标签白名单、禁止键前缀（message/prompt/tool.args/tool.result/file/path/url/
  secret/authorization）；补表驱动单测。
- [x] 1.2 新增 `TelemetryHub`（`internal/servicehost` 或 `internal/telemetry/hub`）：
  包装 PI `TelemetryContext`，支持注册/注销 exporter、按 span 捕获 exporter 集、有界
  扇出、队列满丢最旧并计数；`StartSpan`/metric 记录 MUST 不返回错误给调用方。
- [x] 1.3 `servicehost.newTelemetryFactory` 改为构造 hub + base（Memory/Noop），
  `agent.RunTelemetry.Context` 指向 hub；现有 `KEEL_TELEMETRY`/tenant 配置映射保持输入，
  但 OTLP 选择由插件启用决定；补配置矩阵测试（未启用/启用/缺 endpoint）。
- [x] 1.4 `harnessext.PluginEnv` 增加 `Telemetry`，插件 `Start/Stop` 可注册/注销；
  确保启停顺序不依赖服务图；补晚注册与多轮启停测试。

## 2. 插件与配置

- [x] 2.1 新增官方 `internal/pluginpkg/builtin/keel.telemetry.yaml`（in-process、
  bundled）与 `provides.telemetry` schema/校验：exporters 闭集 `[otlp-http]`、signals
  闭集 `[traces,metrics,logs]`、未知字段拒绝；在 `cmd/keel/serve.go` 的 builtin 注册中
  提供 factory 并设 `DefaultEnabled:false`（现逻辑只对 testkit 例外），补 boot 测试。
- [x] 2.2 插件实现：读取 tenant `telemetry` 配置（adapter/endpoint/headers/serviceName/
  signals/sampleRatio）；`headers` 只接受 `secret:` 引用，禁止明文；配置非法 → `FAILED`
  且不出网；`adapter=none/memory` → 合法 inactive。
- [x] 2.3 生命周期：启用走 `plugin.lifecycle.enable` + durable 审批（展示 exporter、
  signals、endpoint host、采样、是否需要 headers，不显示值）；停用注销并 flush、释放状态；
  bundled 插件只可停用不可卸载（`ErrPluginBundled` 负控）；补审批/停用测试。
- [x] 2.4 安装/启停/配置变更写审计（pluginId、tenant、actor、动作、signals、endpoint
  host、采样、结果）；不含 header 值/secret/payload；补审计断言。

## 3. OTLP 导出管线

- [x] 3.1 扩展 `internal/telemetry/otlp.go`：增量 drain（确认游标）替代“Shutdown 全量
  POST”；`Shutdown` 只 flush 未确认批次；重复 `Shutdown` 不重复导出；补 idempotency 单测。
- [x] 3.2 周期批量：512 spans 或 1 MiB 或 5s；单 POST ≤4 MiB 自动拆分；traces/metrics/
  logs 分别有队列与字节上限；满时丢最旧并计数。
- [x] 3.3 错误处理：5xx/429/网络指数退避（429 优先 `Retry-After`），最多 5 次；连续
  5 次失败熔断 30s，半开单探针；其它 4xx 丢弃批次并计数；补假 collector 测试
  （500/429/timeout/400）。
- [x] 3.4 关机：5s deadline flush；未发出计数；不阻塞进程退出；补 SIGTERM/超时测试。
- [x] 3.5 依赖门：不引入 OpenTelemetry SDK；`scripts/binary-size.sh` 增量在预算内；
  > 实测：`go.mod` 未新增依赖；`cmd/keel` linux/amd64 +212 KiB（arm64 +192 KiB），
  > 在复审指定的 256 KiB 预算内通过。证据：`evidence/binary-size.txt`。
  `go.mod` 不新增 OTel 依赖。

## 4. 采样、脱敏、基数

- [x] 4.1 头采样：`sample_ratio` 0..1，按 trace id 确定性；子 span 继承；未采样回调仍
  执行（nop span）；`0` 只停 traces；补确定性/继承测试。
- [x] 4.2 日志过滤：level + 可选比例；错误级别不被普通采样丢弃；log 只导出有界
  `event` 代码与身份字段，自由文本 message 不导出；补桥接测试。
- [x] 4.3 脱敏：属性 allowlist 与长度/数量上限；未知键丢弃并计数；错误只导出
  `error.kind/status/http.status_code`；headers 不进日志/状态；补含 `sk-`/Authorization/
  路径/文件名的负控。
- [x] 4.4 基数：metric 标签白名单、每族 1,000/全局 5,000 series 上限、未注册指标名丢弃、
  溢出计数；补 10k 会话模拟测试证明 series 不增长。

## 5. 租户、故障隔离、状态

- [x] 5.1 per-tenant hub/exporter/queue/state；多 tenant 单进程且无法隔离时 fail closed；
  补双租户测试（A endpoint 只收 A，B 停用不影响 A）。
- [x] 5.2 故障隔离：collector 拒绝/超时/熔断时 run/tool/SSE/HTTP 结果不变；只增计数与
  `DEGRADED` 诊断；插件初始化失败 → `FAILED`，harness 可用；补集成测试。
- [x] 5.3 状态：`GET /v1/telemetry/status`（`plugin.read`）返回 enabled/signals/endpoint
  host/sampleRatio/queueDepth/dropped/exported/lastErrorClass；跨租户 404；不含 header 值；
  补 handler 测试。
- [x] 5.4 资源回收：停用/卸载后队列、连接、goroutine、定时器归零；多轮启停无泄漏；
  补 goroutine 计数与 `-race`。

## 6. 与 internal/metrics 的边界与文档

- [x] 6.1 确认遥测插件不写 `internal/metrics` registry、不新增 Prometheus endpoint/
  remote-write；`/metrics` series 在插件启用前后不变；补断言。
- [x] 6.2 文档：`docs/HARNESS.md` 增补 telemetry 插件与状态；`docs/PI-PARITY.md` 明确
  “契约不变、导出插件化”；`docs/RUNBOOK.md` 增补 collector 故障排查与默认关闭说明。
- [x] 6.3 指标族 owner 表（Prometheus vs telemetry）写入 design/运维文档，避免双写。

## 7. 测试与 E2E

- [x] 7.1 单测：hub 扇出/队列溢出、OTLP payload、重试/熔断、采样、allowlist、基数、
  配置校验。
- [x] 7.2 集成：假 OTLP collector 收 traces/metrics/logs；默认关闭零请求；启用后收到
  带 tenant/session 的 span 与无身份标签的 metric；停用后停止。
- [x] 7.3 故障：collector 500/429/超时/宕机；run 全部完成；状态计数正确；恢复后半开
  成功。
- [ ] 7.4 真实服务 E2E（6 步起）：默认关闭 → 启用审批 → 发一轮对话 → collector
  收到 span/metric → 改采样 → 停用 → 零请求；日志/报告证据归档。
  > 按本次任务约定，以假 OTLP collector 的集成/E2E 测试代替（见 `evidence/fake-collector-e2e.txt`），
  > 未跑真实外部 collector 的浏览器 6 步。
- [x] 7.5 进程 kill -9 后重启：未 flush 有界丢失；正常关机不重复已确认批次；无持久队列，
  重启不重放（at-most-once across restart）；存活 exporter 的模糊重试可能重复；无孤儿 goroutine。

## 8. 验收与归档

- [ ] 8.1 `go test -race` 相关包（telemetry/servicehost/harnessext/pluginpkg/agent/
  host/cmd）全绿；`scripts/binary-size.sh` 通过；`nsh check` 相关面全绿。
- [x] 8.2 `openspec validate keel-plugin-telemetry --strict` 通过；证据目录含输出。
- [ ] 8.3 独立 reviewer（子代理）复核 P0/P1 清零；重点审正文/密钥、基数、租户隔离与
  故障不拖垮 run。
- [ ] 8.4 归档：`openspec archive keel-plugin-telemetry`，更新 `MAINLINE.md` 与
  `openspec/specs/plugin-telemetry/spec.md` 的 Purpose。
