# observability

## ADDED Requirements

### Requirement: 每次 run 一条终态遥测

Runner SHALL 在 run 结束时追加 `run.finished` 事件，包含：`turns`、`tokensIn`、
`tokensOut`、`toolTimings`（按工具名聚合）、`contextTokens`、`model`、`provider`、
`durationMs`、`stopReason`。该事件 MUST NOT 进入模型上下文。

#### Scenario: 一次工具调用
- **WHEN** 一次 run 调用 echo 一次并结束
- **THEN** `run.finished` 中 turns≥2、toolTimings 含 echo 的耗时、tokens 已归一

### Requirement: 指标固定基数

系统 SHALL 提供 Prometheus 文本指标；MUST NOT 使用 session/run id 作为标签。
series 数在 1 与 1000 会话下 MUST 相同。

#### Scenario: 规模不增 series
- **WHEN** 从 1 个会话扩到 1000 个会话
- **THEN** `/metrics` 的 series 数不变（仅数值变化）

### Requirement: 日志可追溯且脱敏

每条请求日志 SHALL 含 `request_id`/`tenant_id`/`session_id`/`run_id`；
MUST NOT 记录消息正文、工具参数原文、密钥。

#### Scenario: 敏感输入
- **WHEN** 用户消息包含疑似密钥
- **THEN** 日志中不出现该字符串

### Requirement: 可选 trace

系统 SHALL 支持 OTLP trace（默认关闭）：run 一个 span，turn/tool 为子 span。
关闭时 MUST 零额外网络与内存开销。

#### Scenario: 默认关闭
- **WHEN** 未配置 OTLP endpoint
- **THEN** 不产生 trace 导出连接
