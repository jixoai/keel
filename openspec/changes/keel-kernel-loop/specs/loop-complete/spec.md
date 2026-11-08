# loop-complete

## ADDED Requirements

### Requirement: 真实模型端到端

系统 SHALL 提供可复跑的真实模型 E2E：至少两路 provider（OpenAI 兼容的 A6API 与 Anthropic），
一次完整回合包含 input → 模型 → 工具往返 → 终答；脚本 MUST 从环境读取凭据（不入库），
失败（401/超时/限流）MUST 可见且可诊断。

#### Scenario: 工具往返
- **WHEN** 用户输入触发一个工具调用
- **THEN** 日志含 assistant（含 toolCall）→ tool.result → 终答，`inputSeqs` 完整

### Requirement: 压缩接线

系统 SHALL 按阈值自动压缩（token 账驱动），并支持 `POST /v1/sessions/{id}/compact` 手动压缩：
切点保留最近 N token、摘要提交 `compaction` 事件 + checkpoint；压缩后历史事件仍可读；
恢复 = checkpoint + tail；压缩 MUST NOT 在工具调用中间发生，MUST NOT 压缩最后一条 user。

#### Scenario: 自动压缩
- **GIVEN** 上下文超过阈值
- **WHEN** 下一次请求准备
- **THEN** 生成 compaction 事件与 checkpoint，后续请求使用摘要 + 保留段

#### Scenario: 手动压缩
- **WHEN** 客户端调用手动压缩
- **THEN** 立即产生 compaction 事件；失败时上下文保持原样

### Requirement: 默认工具集

默认 loadout SHALL 提供：`clock`、`echo`、文件读（受租户/路径约束）、HTTP fetch（受安全
约束）、以及 MCP 工具（`mcp__<server>__<tool>`，已有客户端）；工具结果 MUST 结构化
（名称/参数/结果/耗时/错误），供 UI 工具卡与 `run.finished.toolTimings` 消费。

#### Scenario: MCP 工具零改动可用
- **WHEN** 配置一个真实 MCP server
- **THEN** 其工具以 `mcp__` 前缀出现在工具列表并可被模型调用

### Requirement: run.finished 由 worker 提交

worker SHALL 在 run 收口时提交 `run.finished`（含 cancelled/failed），字段与 protocol-v1
一致；MUST NOT 因摘要失败而改变 run 终态。

#### Scenario: 取消也摘要
- **WHEN** run 被取消
- **THEN** 仍有一条 `run.finished`（stopReason=cancelled），会话可继续
