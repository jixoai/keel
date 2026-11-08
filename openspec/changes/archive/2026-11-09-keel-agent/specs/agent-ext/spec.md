# agent-ext

> 产品层扩展面（第一期就落地，后续只增不改）：插件、sub-agent、触发器、hooks。

## ADDED Requirements

### Requirement: 插件契约

`internal/agentext` SHALL 定义 Agent 插件：声明 `provides` / `requires` / `tools` /
`hooks` / `triggers` / `effects`，随 loadout 装配；卸载 MUST 反序清理 effects；
必需依赖缺失 MUST 使插件停在 PENDING（可见诊断），而不是启动失败。

#### Scenario: 插件装配与卸载
- **WHEN** 装配一个提供 hook 与工具的示例插件，然后卸载
- **THEN** 工具/hook/触发器按反序消失，无残留监听器；重装配 generation+1

### Requirement: Sub-agent

系统 SHALL 提供 `spawn_subagent` 原语：以独立会话运行子 agent（同一租户、独立 run），
父会话收到结构化结果（`{status, summary, artifacts?, usage}`）；子会话 MUST 可在会话列表
中按 `parentSessionId` 关联；父会话 MUST NOT 被子 agent 的失败拖垮（失败可见、可重试）。

#### Scenario: 子 agent 完成
- **WHEN** 父 turn 调用 `spawn_subagent`
- **THEN** 产生子会话并运行；完成后父会话收到结果并继续同一 turn

### Requirement: 触发器

系统 SHALL 支持时间触发器（cron/interval）与 WebHook 触发器：触发 MUST 幂等（幂等键）、
有审计（何时、何因、目标会话）；触发动作 = 创建或唤醒会话并开始一个 turn；
失败 MUST 可见并按策略重试；触发器 MUST 可手动 fire（`POST /v1/triggers/{id}/fire`）。

#### Scenario: 时间触发
- **GIVEN** 一个每 5 分钟的 interval 触发器
- **WHEN** 到期
- **THEN** 幂等地创建一个 turn（同一触发窗口不重复），审计可见

#### Scenario: WebHook 触发
- **WHEN** 外部 POST 到触发器 webhook 并携带幂等键
- **THEN** 触发一次；重复键不重复触发

### Requirement: Hooks（第一期闭集）

系统 SHALL 提供声明式 hooks：`subagent.started|finished`、`trigger.before|after`、
`turn.started|finished`、`decision.opened|answered`。hook 执行 MUST 不阻塞账本
（异步/有界队列），失败 MUST 可见、可重试；hook 的 payload MUST 含 tenant/session/run 标识
与相关 id；MUST NOT 含密钥。

#### Scenario: turn 生命周期
- **WHEN** 一个 turn 开始并结束
- **THEN** `turn.started`/`turn.finished` 各触发一次；hook 失败不回滚账本

### Requirement: 扩展面可观测

插件/触发器/hook 的状态 MUST 可查询（`GET /v1/plugins`、`/v1/triggers`）；指标标签 MUST
有界（不含 session/run id）；日志 MUST 脱敏。

#### Scenario: 查询装配
- **WHEN** 调用 `GET /v1/plugins`
- **THEN** 返回已装配插件及其 hooks/triggers/tools 声明与状态
