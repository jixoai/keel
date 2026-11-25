# collaboration-core

## ADDED Requirements

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
