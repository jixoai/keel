# compaction

## ADDED Requirements

### Requirement: 阈值触发

当估算上下文超过模型窗口的阈值时，系统 SHALL 触发压缩；阈值与保留量可配置。

#### Scenario: 超阈值
- **WHEN** 估算 token 超过窗口的 80%
- **THEN** 触发一次压缩，并记录 `tokensBefore`

### Requirement: 切点安全

压缩 MUST NOT 在工具调用与工具结果之间切入；MUST NOT 压缩最后一条 user 消息。

#### Scenario: 工具轮中间
- **WHEN** 上下文结尾是未完成的 toolCall
- **THEN** 本次不压缩，等工具结果提交后再评估

### Requirement: 只追加不覆盖

压缩 SHALL 追加 `compaction` 事件并写 checkpoint；原始事件 MUST 保持可读。

#### Scenario: 回放
- **WHEN** 从 checkpoint 恢复
- **THEN** 摘要在上下文中，历史事件仍可按 seq 查询

### Requirement: 失败可观测

摘要请求失败时 MUST 保留原上下文、记录错误，并让 run 继续。

#### Scenario: provider 失败
- **WHEN** 摘要请求超时
- **THEN** 原上下文不变，`compaction` 事件带 `errorMessage`，run 继续

### Requirement: 手动压缩

系统 SHALL 提供 `POST /v1/sessions/{id}/compact`，语义与自动压缩一致。

#### Scenario: 手动触发
- **WHEN** 操作者调用 compact
- **THEN** 追加一条 `compaction` 事件，返回压缩前后 token
