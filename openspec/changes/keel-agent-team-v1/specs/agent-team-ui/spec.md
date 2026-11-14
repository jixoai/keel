# agent-team-ui Specification

## ADDED Requirements

### Requirement: Agent 与 AgentTeam 必须可从产品到达

Agent 与 AgentTeam MUST 有可用的产品入口，而不只是数据结构：HTTP API（`/v1/agents`、
`/v1/teams` 及成员操作）、CLI（`keel agent *`、`keel team *`）、设置页 section（Agents、
Teams）。仅有校验函数或 DTO 转换 MUST NOT 被视为「已实现」。

#### Scenario: 从设置页创建一个 Agent
- **WHEN** 用户打开设置 › Agents 并点「新建 Agent」，填写名字与默认模型后保存
- **THEN** `GET /v1/agents` 返回该 Agent
- **AND** 列表卡片显示它的头像、名字与默认模型

#### Scenario: 建立团队必须先选队长
- **WHEN** 用户打开设置 › Teams 新建团队但未选队长
- **THEN** 保存按钮不可用，并说明「团队必须有队长」
- **AND** 选中队长后保存成功，`GET /v1/teams` 返回该团队且 captain 字段非空

### Requirement: 队长可通过 CLI 组装团队

Team 的 captain MUST 能够通过 CLI 增删成员；当 captain 的 `teamCli` 禁用该子命令时，
操作 MUST 被拒绝并返回可读原因。

#### Scenario: 队长 CLI 添加成员
- **WHEN** 执行 `keel team member add <teamId> <agentId>`，且 captain 允许该子命令
- **THEN** 命令成功，`GET /v1/teams/{id}` 的 members 含该 Agent

#### Scenario: 权限关闭时拒绝
- **WHEN** captain 的 `teamCli` 禁止成员增删
- **THEN** 命令以非零状态退出，stderr 说明是权限限制而非服务错误

### Requirement: 插件能力必须可发现

声明了 CLI 能力树的插件（model / agent / team）MUST 在设置页的插件面可见，并展示其命令
路径与权限要求。

#### Scenario: 查看插件能力
- **WHEN** 用户打开设置 › 插件并选中一个声明了 CLI 的插件
- **THEN** 界面列出它的命令路径（如 `keel team member add`）与 `danger` / `callable_by`
