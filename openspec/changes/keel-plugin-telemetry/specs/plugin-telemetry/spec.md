# plugin-telemetry

## ADDED Requirements

### Requirement: 遥测信号契约与标识

系统 SHALL 保持 `docs/PI-PARITY.md` §3 的 PI telemetry 契约形状不变：run = 一个 span，
turn/tool/compaction = 子 span；metric 使用 PI instrument；log 使用结构化字段。Span 与 log
MAY 携带 `tenant/session/run/turn/tool` 标识；metric 标签 MUST NOT 携带
`tenant/session/run/request/trace/span` 等高基数标识，租户身份只可出现在 resource 或
span/log 属性。任何信号 MUST NOT 携带 prompt、消息正文、工具参数/结果、附件/文件名、
路径、URL query、secret 或 Authorization。

#### Scenario: run span 带身份

- **WHEN** 一次 attempt 产生 `keel.agent.run` span
- **THEN** span 属性含 tenant、session 与 model/provider，不含消息正文或工具参数

#### Scenario: 指标不带身份

- **WHEN** 记录 `keel.agent.runs` 与 `keel.agent.run.duration`
- **THEN** 标签只有 model/provider/outcome 等有界值，没有 tenant/session/run

#### Scenario: 日志无自由正文

- **WHEN** 一条含用户提示词的日志进入遥测
- **THEN** 自由文本 message 被拒绝，只导出有界 `event` 代码、级别与身份字段，正文不导出

### Requirement: 遥测插件可选且默认关闭

系统 SHALL 提供官方 `keel.telemetry`（in-process、bundled、默认 disabled），由
`provides.telemetry` 声明 exporter/signals。启用 MUST 走 `plugin.lifecycle.enable` 与
durable 审批，审批展示 exporter、signals、endpoint host、采样与是否需要 secret headers
（不显示值）。插件未启用时现有 telemetry 配置 MUST 惰性：base 保持 Memory/Noop，
MUST NOT 发出任何 collector 请求，状态 MUST 显示 `plugin_disabled`。停用 MUST 停止导出、
在有界时间内 flush，并释放队列、连接与定时器；`keel.telemetry` 是 bundled 插件，MUST
只可停用、不可卸载，停用即零残留；若以可安装包发布，卸载 MUST 同样零残留。

#### Scenario: 默认不出网

- **WHEN** 部署配置了 OTLP endpoint 但未启用 `keel.telemetry`
- **THEN** 进程不向 collector 发请求，状态为 `plugin_disabled`

#### Scenario: 启用需审批

- **WHEN** 管理员启用 `keel.telemetry`
- **THEN** 必须先通过 plugin lifecycle 审批；拒绝/超时则插件不启用、不出网

#### Scenario: 停用即停

- **WHEN** 停用插件
- **THEN** 不再产生新导出，队列/连接在限定时间内关闭，run 不受影响

### Requirement: Hub 注册/注销与零残留

宿主 MUST 提供进程级 `TelemetryHub` 包装 PI `TelemetryContext`；插件 MUST 在 `Start`
注册 exporter、在 `Stop` 注销。注册 MUST 不依赖插件与服务图的启动顺序，且 MUST NOT
阻塞 span/metric 产生。停用（及可安装包的卸载）MUST 移除 exporter、队列、游标与状态，
MUST NOT 留下 goroutine、连接或定时器。同一进程多 tenant 时 MUST 每 tenant 一个 hub；
无法隔离时插件 MUST fail closed（不导出），MUST NOT 共享队列或 endpoint。

#### Scenario: 晚于服务图注册

- **WHEN** 插件在 telemetry service 创建之后才启用
- **THEN** hub 接受注册，后续 run span 能到达该 exporter，无需重启进程

#### Scenario: 停用无泄漏

- **WHEN** 启用再停用插件多轮
- **THEN** goroutine/连接数回到基线，后续不再有导出请求

#### Scenario: 多租户 fail closed

- **WHEN** 同一进程承载两个 tenant 且没有 per-tenant hub
- **THEN** 插件拒绝导出并报告原因，不把任一 tenant 的信号发给另一 endpoint

### Requirement: OTLP 导出有界、可重试、可恢复

系统 SHALL 按 `signals` 配置通过 OTLP/HTTP JSON 导出 `traces`/`metrics`/`logs` 到配置的
绝对 http(s) endpoint；MUST NOT 引入 OpenTelemetry SDK。导出 MUST 周期批量（默认 512
spans 或 1 MiB 或 5s），单 POST ≤4 MiB；队列 MUST 有字节与条数上限，满时丢最旧并计数。5xx/429/网络
失败 MUST 指数退避（429 优先 `Retry-After`），连续失败 MUST 熔断；其它 4xx MUST 丢弃
该批并计数，不无限重试。同一 exporter 生命周期内已收到 2xx 的批次 MUST NOT 再导出；无持久
队列时崩溃重启 MUST 丢失未发出批次且 MUST NOT 重放已确认批次（at-most-once across restart）；
存活 exporter 的“已发送但响应丢失”重试 MAY 重复，collector 必须按 trace/span id 幂等。关机
MUST 在期限内
有界 flush，剩余丢弃并计数，MUST NOT 阻塞进程退出。

#### Scenario: collector 不可用

- **WHEN** collector 连接被拒绝
- **THEN** 导出进入退避/熔断，队列满时丢最旧并计数；run、工具与 HTTP 响应不受影响

#### Scenario: 已确认不重复

- **WHEN** 一批 span 在同一进程内已收到 2xx，随后进程继续运行
- **THEN** 该批不会被再次 POST；后续批次从确认游标之后继续

#### Scenario: 关机 flush

- **WHEN** 进程在队列仍有记录时收到关闭信号
- **THEN** 在 5s 期限内尽力导出，超时则记录丢弃数并退出

### Requirement: 采样与日志过滤可控

Span MUST 支持头采样：比例 0..1（默认 1.0），按 trace id 确定性决定，子 span MUST 继承
父决策；未采样 span MUST NOT 导出，但回调 MUST 正常执行。`sample_ratio=0` MUST 只停
traces，MUST NOT 停 metrics 或 logs。Metric MUST NOT 被 trace 采样。Log MUST 按级别过滤，
错误级别 MUST NOT 被普通采样丢弃。首版 MUST NOT 声称尾采样或“错误必采样”。

#### Scenario: 确定性子集

- **WHEN** 采样比例 0.1 处理大量请求
- **THEN** 同一 trace 的所有 span 决策一致，导出量约为 10%，metric 计数完整

#### Scenario: 零采样只停 trace

- **WHEN** `sample_ratio=0`
- **THEN** 没有 trace 导出，metric 与错误日志仍按配置导出

### Requirement: 脱敏 allowlist 与正文禁止

导出前 MUST 经过属性 allowlist：只允许有界标识/结果键（`tenant/session/run/turn/tool/
model/provider/outcome/status/kind/error.kind/http.status_code/mcp.server/plugin/command`
与 `level/event/logger`）；未知键 MUST 丢弃并计数。
字符串值 MUST 限制长度、数组/属性数量 MUST 有上限。错误 MUST 只导出
`error.kind/status/http.status_code` 等有界类别，MUST NOT 导出原始异常文本。Exporter
headers MUST 从 secret store 解析，MUST NOT 出现在日志、状态或导出的属性中。审计与事件
日志 MUST NOT 包含遥测 payload。

#### Scenario: 提示词中的密钥

- **WHEN** 工具参数或提示词包含 `sk-...` 或 `Authorization`
- **THEN** 该内容不出现在任何 span/metric/log 属性中

#### Scenario: 原始错误

- **WHEN** provider 错误文本回显了请求体
- **THEN** 只导出错误类别/状态码，不导出原始文本

#### Scenario: header 脱敏

- **WHEN** 查看 telemetry 状态或日志
- **THEN** 只看到 header 名/数量，不看到值

### Requirement: 指标基数上限

Telemetry metric MUST 使用固定标签白名单（model/provider/outcome/status/tool/kind/result/api/stop）；
MUST NOT 使用 tenant/session/run/request/trace/span 标签。每个指标族 MUST
有 series 上限（默认 1,000），全局 MUST 有上限（默认 5,000）；溢出 MUST 丢弃新 series
并递增丢弃计数，warn 一次。metric 名称 MUST 在插件启动时注册；未注册名称 MUST 丢弃。

#### Scenario: 会话增长不增 series

- **WHEN** 会话数/run 数从 10 增长到 10,000
- **THEN** telemetry metric 的 series 数保持有界，不随身份数增长

#### Scenario: series 溢出

- **WHEN** 某指标族的标签组合超过上限
- **THEN** 新组合被丢弃并计数，已有 series 继续工作，进程不崩溃

### Requirement: 租户隔离与状态可查

每个 tenant MUST 有独立的 telemetry 配置、exporter、队列与状态；tenant id MUST 作为
resource 或 span/log 属性，MUST NOT 作为 metric 标签。跨租户 MUST NOT 看到对方的信号、
配置或状态；跨租户读取 telemetry status MUST 返回 404。系统 SHALL 提供状态查询
（`GET /v1/telemetry/status`，需 `plugin.read`，或等价插件视图），至少返回 enabled、
signals、endpoint host（脱敏）、sampleRatio、queueDepth、dropped、exported、
lastErrorClass。

#### Scenario: 两租户互不串流

- **WHEN** 租户 A 与 B 各自配置不同 endpoint
- **THEN** A 的 collector 只收到 A 的信号，B 停用/失败不影响 A

#### Scenario: 状态可诊断

- **WHEN** 管理员查看 telemetry 状态
- **THEN** 能看到启停、信号、采样、队列、导出行数、丢弃数与最近错误类别，且无 header 值

### Requirement: 导出故障不拖垮运行

任何导出失败、队列溢出、熔断、插件诊断 `DEGRADED`/`FAILED` MUST NOT 改变 run/tool 的
成功、失败或耗时语义，MUST NOT 返回错误给 `StartSpan`/metric 记录，MUST NOT 阻塞 SSE 或
HTTP。失败 MUST 以计数、限频日志与插件诊断可见；修复配置/重启插件后 MUST 可恢复，无需
重启进程。逐 span 导出 MUST NOT 写审计。

#### Scenario: collector 宕机

- **WHEN** collector 在 run 期间不可达
- **THEN** run 正常完成，telemetry 状态显示丢弃/错误计数，审计没有逐 span 记录

#### Scenario: 插件初始化失败

- **WHEN** endpoint 非法或 secret 缺失
- **THEN** 插件状态 `FAILED` 且不出网，harness/run 继续可用

### Requirement: 与 internal/metrics 的边界

`internal/metrics` 的 Prometheus `/metrics` MUST 继续是固定基数运维面；遥测插件 MUST NOT
写入、复制或替换它，MUST NOT 新增 second Prometheus endpoint 或 remote-write。每个指标族
MUST 只有一个 owner；`agent.RunTelemetry.Runtime` 的运行时指标 MUST 保持不变。
`docs/PI-PARITY.md` 的 telemetry 契约仍是唯一形状来源，`run.finished` 仍是业务汇总事件，
MUST NOT 被当作 telemetry 的替代。

#### Scenario: /metrics 不受影响

- **WHEN** 启用遥测插件并导出 OTLP
- **THEN** `/metrics` 的 series 与标签不因插件变化，仍无 tenant/session/run 标签

#### Scenario: 单 owner

- **WHEN** 审查某个指标族
- **THEN** 它只由 `internal/metrics` 或 telemetry 之一拥有，不存在双写

### Requirement: 审计与配置变更

遥测插件的安装/启用/停用/卸载与 telemetry 配置变更 MUST 写审计：包含 pluginId、
tenant、actor、动作、signals、endpoint host（脱敏）、采样与结果；MUST NOT 包含 header
值、secret、payload 或租户数据。配置非法 MUST fail closed 并给出字段级原因，MUST NOT
半应用。审计不得逐 span/metric 增长。

#### Scenario: 配置变更留痕

- **WHEN** 管理员把采样从 1.0 改为 0.1
- **THEN** 审计记录动作、旧/新采样与 endpoint host，不记录 header 值

#### Scenario: 非法配置

- **WHEN** endpoint 不是绝对 http(s) URL
- **THEN** 启用/更新被拒绝并给字段级错误，不创建 exporter，既有配置保持不变
