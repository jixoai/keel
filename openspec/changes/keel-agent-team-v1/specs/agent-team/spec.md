# agent-team Specification

## ADDED Requirements

### Requirement: AgentTeam 必须有队长

AgentTeam MUST 是一个命名的团队配置，MUST 恰好包含一个 captain（队长），captain MUST 引用
一个存在的 Agent。成员 MAY 为 0..N；只有队长的团队是合法团队。

- 团队 MUST 保存名称、描述、captain agentId、成员列表、团队级默认（工具/模型覆盖）、
  团队能力开关与 feature 绑定（port → 命令）。
- captain 与成员 MUST 引用 Agent 配置（Agent 库或 inline 定义），MUST NOT 用 subagent
  参数代替。
- 成员 MUST 有可编辑的 label/description；默认值只是建议，不得作为不可变文案。
- 缺少 captain、captain 引用的 Agent 不存在、或团队被归档时，团队状态 MUST 为 `invalid`，
  MUST NOT 被会话启动。

#### Scenario: 只有队长的团队
- **WHEN** 用户创建一个只包含 captain 的团队
- **THEN** 团队有效并可启动；启动后 captain 可以按能力开关自行添加成员

#### Scenario: 缺队长
- **WHEN** 用户删除 captain 或创建一个没有 captain 的团队
- **THEN** 保存被拒绝（或状态为 invalid），错误为 `missing_captain`；会话启动被阻止

### Requirement: 成员是独立 Agent，不是 subagent

团队成员 MUST 是完整 Agent 配置，MUST 拥有自己的默认模型、工具能力、系统提示词与头像。
成员不是 captain 同一次 run 内的多角色提示词，也不是一次工具调用的子过程。

运行机制：成员按既有 Agent/run 机制启动；AgentTeam 只提供配置空间、成员关系与编排，
不定义第二套 session/run 拓扑。

#### Scenario: 成员独立运行
- **WHEN** 团队启动并给成员分配任务
- **THEN** 成员以自己的 Agent 配置启动独立 session/run；captain 能看到成员状态与结果，
  但成员不共享 captain 的模型上下文

### Requirement: 团队 CLI 与自组装

AgentTeam 插件 MUST 提供 CLI：`keel team list|show|create|update|delete|member add|remove|list|save|start`。
每个子命令 MUST 在插件能力树中声明 summary/description/args_schema/output_schema/permissions/
danger/callable_by。

- captain 是否可调用团队 CLI MUST 由团队/Agent 的 `teamCli.enabled` 与逐命令白名单决定。
  Agent 级与团队级 `teamCli` 同时存在时 MUST 有单一裁决：两侧都启用才启用，白名单取交集，
  任一侧关闭即关闭（`EffectiveTeamCLI`）。
- 关闭时 MUST 从 captain 的工具/CLI 面移除，而不是只在提示词里劝阻；调用返回 typed
  permission error（`team_cli_disabled` / `team_cli_not_allowed` / `team_cli_not_declared`）。
- team 配置是一份 DB-backed 文件；成员增删 MUST 修改这份 team 配置，MUST NOT 改写
  modelprovider/agentprovider 的共享配置。session 启动时冻结团队快照，运行中修改不影响当前 run。
- 运行中的成员变更 MUST NOT 改写已启动 run 的历史快照；下一次成员启动使用新配置。

#### Scenario: captain 自建成员
- **WHEN** `teamCli.enabled` 为 true 且允许 member.add
- **THEN** captain 可运行 `keel team member add`，成员进入当前团队实例；默认不持久化，
  除非显式 save

#### Scenario: 禁用自组装
- **WHEN** `teamCli.enabled` 为 false
- **THEN** captain 看不到 `keel team member ...` 命令，团队只能由人工在设置页调整

### Requirement: 会话启动模式

会话创建 MUST 支持两种模式：

- **standard**：单会话；用户选择模型（或默认 Agent）后启动。main-agent MAY 通过 bash /
  agent-plugin CLI 启动其它 Agent（subagent 能力）。
- **team**：用户选择 AgentTeam 预设后启动；团队 MUST 有 captain；captain 成为主 Agent，
  成员按团队策略启动。

会话 MUST 保存模式与相应引用（model 或 teamId）。缺少 captain/模型时 MUST 阻止启动并给出
修复入口。

#### Scenario: 标准模式
- **WHEN** 用户选择 standard 并选一个模型
- **THEN** 不要求团队；会话使用该模型的 main-agent

#### Scenario: 团队模式
- **WHEN** 用户选择 team 并选一个有效团队
- **THEN** captain 启动为主 Agent，成员按策略成为独立 Agent；缺 captain 的团队不出现在可选项

### Requirement: 团队快照与引用保护

启动 run MUST 冻结 team/agent/tool/prompt 快照，并把 captain、成员 id、团队版本/hash 写入
run manifest。运行中编辑 Agent/Team MUST NOT 影响当前 run。未封存（无 revision/hash）的团队
MUST NOT 启动：启动门 MUST 要求已 Seal（`not_sealed`），否则 manifest 会冻结空 hash。

- Agent 被团队引用时 MUST NOT 被硬删除；只能归档/禁用，并列出引用团队。
- 团队被会话引用时 MUST NOT 被硬删除；只能归档，历史会话保留快照可读。
- 团队切换只影响下一次 run，不改写历史。

#### Scenario: 运行中修改团队
- **WHEN** 团队正在运行时用户修改成员或工具
- **THEN** 当前 run 继续使用旧快照；下一次 run 使用新快照

#### Scenario: 删除被引用的 Agent
- **WHEN** 用户删除团队中的成员 Agent
- **THEN** 删除被拒绝并列出团队；提供从团队移除或归档的修复动作
