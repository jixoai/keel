# collaboration-core Specification

## Purpose
TBD - created by archiving change keel-collab-core. Update Purpose after archive.

## Requirements

### Requirement: Bot 是角色，不是协作模式

系统 MUST 把 Bot 定义为角色主体（Profile + ModelKind + Harness + 权限）；MUST NOT 为
`main`、`sub`、`super`、`worker`、`orchestrator` 等协作位置新增 Bot kind 或第二套账号主体。
同一个 Bot MUST 能在不同 WorkItem/Session 中以不同协作位置运行。

#### Scenario: 同一 Bot 切换位置
- **WHEN** 同一个 Bot 在一个 WorkItem 中作为 main，在另一个 WorkItem 中作为 sub
- **THEN** 两次运行使用同一个 Bot/Account/权限边界，只改变本次 mode 的位置

#### Scenario: 拒绝 BotKind
- **WHEN** 配置或 API 尝试创建 `kind=super` 的 Bot
- **THEN** 装配/请求失败，并返回类型化错误

### Requirement: CollaborationMode 描述与注册

系统 SHALL 提供 ModeDescriptor 与 ModeRegistry。每个模式 MUST 声明 id、所需 Bot 角色、
拓扑、工作单元、输入/输出、Attention 能力、预算/并发/深度上限和 UI 入口。注册 MUST 在
装配期完成；未知或重复 mode id MUST 在启动时失败。

#### Scenario: 注册内置模式
- **WHEN** `keel serve` 装配 `subagent` 与 `superagent` 模式
- **THEN** 只读模式列表包含两者及其角色/限制，重复 id 不覆盖

#### Scenario: 停用模式
- **WHEN** 管理员停用一个模式
- **THEN** 新 WorkItem 被拒绝，既有 WorkItem/Assignment 仍可读、可审计、可人工接管

### Requirement: 受控 Host 是唯一 Session 创建门

所有 mode 创建的 Session MUST 通过 `collab.Host`；Host MUST 校验 tenant、`bot.invoke`、
permission、预算、深度/宽度、并发和取消传播，并写审计。Mode 插件 MUST NOT 直接调用
kernel/store 创建 Session 或绕过 Host。

#### Scenario: 插件绕过被拒
- **WHEN** 一个 mode 插件尝试直接创建不属于自己的 Session
- **THEN** 创建被拒绝并写 `permission.denied`/`collab.invalid_creation` 审计

#### Scenario: 租户隔离
- **WHEN** 租户 A 的 mode 请求创建租户 B 的 Session
- **THEN** Host 返回 404/403，且不创建任何行

### Requirement: 深度策略显式表达

系统 MUST 用显式 DepthPolicy 表达委派深度：`forbidden`（禁止派生）、`unlimited`（不限）
和 `limited(max)` 三种状态 MUST 可区分；MUST NOT 让同一个数值 0 同时表示禁止、继承和不限。
ModeDescriptor MUST 声明 depth policy；tenant 上限和 WorkItem 预算只能收窄它。

#### Scenario: 禁止派生
- **WHEN** 模式声明 depth=forbidden
- **THEN** 任何子 Session 创建返回 `depth_exceeded`，且不创建子 Session

#### Scenario: 不限深度
- **WHEN** 模式声明 depth=unlimited 且 tenant 上限也不限
- **THEN** 不因深度拒绝，但仍执行预算/宽度/并发/权限检查

#### Scenario: 有限上限
- **WHEN** 模式声明 depth=limited(3)，main 为 depth 0
- **THEN** depth 3 的子 Session 可运行，depth 3 再派生 depth 4 被拒绝

### Requirement: 创建幂等与容量原子预留

同一个 tenant/mode/parent/WorkItem/idempotency-key MUST 返回同一个 Session；成功后重试
MUST NOT 重新创建、MUST NOT 因宽度/并发已满而失败，部分失败的重试 MUST 从已创建的
Session 继续完成绑定。宽度/并发检查与预留 MUST 是同一个原子步骤，MUST NOT 出现
check-then-act 超发。

#### Scenario: 成功后重试
- **WHEN** 同一 idempotency key 在创建成功后再次提交
- **THEN** 返回原 sessionId，launcher 只被调用一次

#### Scenario: 部分失败重试
- **WHEN** Session 已创建但索引/绑定写入失败后使用同一 key 重试
- **THEN** 不重新创建 Session，补完索引与 Assignment 绑定后返回原 sessionId

#### Scenario: 并发上限
- **WHEN** 多个 goroutine 同时创建且 maxConcurrent=2
- **THEN** 最多创建 2 个 Session，其余返回 `concurrency_exceeded`

### Requirement: WorkItem 成员边界与 Scope

Assign/Ask/CreateSession 作用于 WorkItem 时 MUST 验证调用方是该 WorkItem 的 owner、
Host-created 成员或 Assignment 参与者；bare child MUST NOT 作为访问其他 WorkItem 的跳板。
WorkItem `Scope.BotIDs` MUST 限制可调用的目标 Bot；它只是收窄，不授予权限。WorkItem
scoped Session MUST 绑定 Assignment，取消时 MUST 覆盖其 bare delegation 子树。

#### Scenario: 跨 WorkItem 拒绝
- **WHEN** 属于 WorkItem A 的 session 对 WorkItem B 执行 Assign/Ask
- **THEN** 返回 403，写 `permission.denied`，不创建绑定或 Attention

#### Scenario: Scope 拒绝
- **WHEN** WorkItem scope 只允许 bot-1，调用方 Assign/CreateSession 目标 bot-2
- **THEN** 返回 403，不创建绑定或 Session

#### Scenario: bare 子树取消
- **WHEN** 已绑定 Session 派生了未绑定 Assignment 的 bare 子 Session
- **THEN** 取消 WorkItem 时子树一并取消，不留孤儿 Session

### Requirement: 人工接管使用独立 operator 权限

停用模式的既有 WorkItem 可以人工接管，但 Host MUST NOT 信任调用方自报的
`humanTakeover` 布尔值；接管 MUST 通过独立的 operator/人工权限 seam 判定，缺权时拒绝。

#### Scenario: 无 operator 权限
- **WHEN** 模式已停用且调用方没有 operator 权限
- **THEN** Assign 返回 403，既有 WorkItem/Assignment 仍可读、可审计

### Requirement: 审计 fail-closed

OpenWorkItem、Assign、Ask 和 CreateSession 的 accepted 审计 MUST 在状态写入前完成；审计
写入失败 MUST 拒绝该写入，MUST NOT 产生未审计的 WorkItem/Assignment/Attention/Session。

#### Scenario: 审计不可用
- **WHEN** AuditSink 返回错误
- **THEN** 写入被拒绝（`audit_unavailable`），store/publisher/launcher 均未被调用

### Requirement: Attention 生命周期与会话归档

Attention MUST 可被回答并复用 decision 语义（同答案幂等、冲突答案拒绝、超时标记
`timed_out`）；Host MUST 能把 Session 的终态（done/failed/cancelled）投影到索引和
Assignment，释放宽度/并发配额。

#### Scenario: Attention 过期
- **WHEN** 超过 deadline 仍未回答
- **THEN** Answer 返回 `attention_expired`，行状态为 `timed_out`

#### Scenario: 会话完成释放配额
- **WHEN** 一个 active Session 被标记 done/failed/cancelled
- **THEN** 其宽度/并发槽位释放，Assignment 记录对应终态

### Requirement: 通用协作原语以端口提供

系统 SHALL 提供 WorkItem、Assignment、Attention、Source、Channel、Link 的端口/记录类型。
WorkItem MUST 独立于 Session 生命周期；Assignment MUST 独立于 Session 结束；Attention
MUST 是结构化的人机决策请求；Link MUST 表达 delegation/assignment/subscription/review
等关系，而不是只表达父子。

#### Scenario: Assignment 跨 Session
- **WHEN** 一个 WorkItem 的 Worker Session 结束或被替换
- **THEN** Assignment 仍记录原绑定与结果，新的 Assignment 可继续同一 WorkItem

#### Scenario: 人机 Attention
- **WHEN** Worker 需要人做决定
- **THEN** Attention 包含问题、来源、选项/自由文本和路由，不依赖自由文本摘要推断

### Requirement: 模式不能自带权限或审计旁路

Mode MUST 使用现有 Bot 权限和 Host 审计；MUST NOT 新增“super 权限”或“mode 专属权限”。
Mode 的配置、WorkItem、Assignment 和 Attention MUST 带 tenant_id；单租户部署 MUST 只使用
一个 tenant。

#### Scenario: 模式不扩权
- **WHEN** 一个 mode 需要调用另一个 Bot
- **THEN** 仍按目标 Bot 的 `bot.invoke` 授权，缺权返回 403 并审计，不因 mode 身份自动放行

### Requirement: 兼容与可观测

现有 `agent` 工具和 subagent HTTP API MUST 继续可用，并由 `subagent` mode 适配；新增
mode/WorkItem/Assignment 的状态 MUST 可从结构化日志和只读 API 查询，MUST NOT 进入
模型上下文或泄露其他租户数据。

#### Scenario: 旧入口不绕过
- **WHEN** 调用现有 `agent` 工具或 `/v1/sessions/{id}/subagents`
- **THEN** 请求经过同一 Host 检查，权限/深度/预算与新模式入口一致
