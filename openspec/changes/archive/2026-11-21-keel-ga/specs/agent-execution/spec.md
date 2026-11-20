# agent-execution

## ADDED Requirements

### Requirement: 事件是上下文唯一来源

worker SHALL 只从已提交事件重建模型上下文；MUST NOT 依赖进程内消息缓存。

事件映射：
- `user.message` → `UserMessage`
- `assistant.message` → `AssistantMessage`（含 `stopReason`）
- `tool.result` → `ToolResultMessage`
- `compaction` → 折叠点（其前的事件不再进入上下文）

#### Scenario: 冷启动重建
- **WHEN** 进程重启后领取一个已有会话
- **THEN** 模型收到的消息与崩溃前已提交事件一一对应

### Requirement: 持久 run 与租约

系统 SHALL 用 `runs(tenant_id, session_id)` 表示会话的执行状态，状态为
`pending|running|done|failed`，并带 `lease_until`。worker 用
`FOR UPDATE SKIP LOCKED` 领取 `pending` 或租约过期的 `running` run。

#### Scenario: 崩溃后回收
- **GIVEN** 一个 run 处于 `running` 且 `lease_until` 已过期
- **WHEN** 任一 worker 轮询
- **THEN** 它领取该 run 并从已提交事件继续，不产生重复 user 事件

### Requirement: 同一会话串行

同一会话同时最多一个活跃 run；新的 user 事件在 run 进行中 MUST 排队
（run 完成后再次置 `pending`）。

#### Scenario: 连发两条消息
- **WHEN** 客户端在模型回复前又提交一条消息
- **THEN** 第二条在第一条 run 结束后被处理，顺序与事件 seq 一致

### Requirement: 工具执行 at-least-once

工具结果 SHALL 以 `tool.result` 事件提交。进程在工具执行中死亡时，恢复后该工具
MUST 重新执行；框架 MUST NOT 声称 exactly-once。

#### Scenario: 工具执行中 SIGKILL
- **WHEN** 工具已开始但结果未提交时进程被 kill
- **THEN** 恢复后同一 `toolCallId` 重新执行，最终只提交一条 `tool.result`

### Requirement: 提交即可见

`assistant.message` MUST 在完整消息产生后一次性提交；客户端订阅到的消息
MUST 是已提交的完整消息。token 级 partial MAY 通过非持久旁路广播，但 MUST NOT
进入事件表。

#### Scenario: 断线期间产生回复
- **WHEN** 客户端断开时模型完成回复
- **THEN** 重连后按 `Last-Event-ID` 收到该完整消息

### Requirement: 压缩不覆盖历史

上下文超阈值时 SHALL 追加 `compaction` 事件并写 checkpoint；原始事件 MUST 仍可按
seq 读回。

#### Scenario: 压缩后回放
- **WHEN** 触发一次压缩
- **THEN** 新 run 从 checkpoint 摘要 + 后续事件重建，历史事件仍在

### Requirement: 确定性 fake provider

无 provider 凭据时系统 SHALL 使用 fake provider：回复是已提交消息的纯函数，
用于本地开发与 E2E。

#### Scenario: 无密钥启动
- **WHEN** 未配置模型密钥
- **THEN** 服务启动成功，输入得到确定性回复，且重复提交不重复回复
