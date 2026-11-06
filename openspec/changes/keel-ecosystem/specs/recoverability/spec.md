# recoverability

## ADDED Requirements

### Requirement: 模型请求幂等

每次模型请求 SHALL 带 `generation_id`，并写入对应的 `assistant.message` 事件。
恢复时若该 id 的事件已存在，MUST 复用而不再请求模型。

#### Scenario: 模型流中 kill 后重试
- **WHEN** 进程在流式输出中途被 kill，重启后再次执行同一 run
- **THEN** 不产生第二条最终回复；已有 `generation_id` 的消息被复用

### Requirement: 工具结果唯一

`tool.result` SHALL 以 `(tenant_id, session_id, tool_call_id)` 唯一；
重复执行同一调用 MUST 只保留一条结果事件。

#### Scenario: 工具执行中 kill
- **WHEN** 工具在结果提交前被 kill
- **THEN** 恢复后重新执行，最终恰好一条 `tool.result`

### Requirement: checkpoint 回放

恢复 SHALL 使用「最近 checkpoint + 其后的 tail 事件」；checkpoint MUST NOT 覆盖事件。

#### Scenario: 压缩后崩溃
- **WHEN** 压缩提交后进程崩溃
- **THEN** 新进程从 checkpoint 摘要 + tail 重建，历史事件仍可读

### Requirement: 崩溃矩阵进 CI

系统 SHALL 对五个 await 点各有一个 kill 测试：模型流、工具执行、事件追加、压缩、SSE 发送。
nightly SHALL 跑 10 分钟随机 kill 混沌。

#### Scenario: 追加中 kill
- **WHEN** 事件事务提交前 kill
- **THEN** 重启后不存在半条事件

### Requirement: run 状态收敛

任意崩溃/重试后，run SHALL 最终收敛到 `done` 或 `failed`；
MUST NOT 永久停留在 `running`（租约过期可被回收）。

#### Scenario: 多次失败
- **WHEN** run 连续失败达到 `maxAttempts`
- **THEN** 状态变为 `failed` 并记录 `last_error`
