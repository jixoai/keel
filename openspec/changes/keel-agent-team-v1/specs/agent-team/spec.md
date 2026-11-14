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

AgentTeam 插件 MUST 提供 CLI：
`keel team list|show|create|update|delete|member add|remove|list|save|start`；
每个子命令 MUST 在能力树声明 summary/description/schema/permissions/danger/callable_by。

- captain 能否调用 CLI MUST 由 Agent/团队两级 `teamCli.enabled` 与逐命令白名单单一裁决
  （`EffectiveTeamCLI`：两侧均启用、白名单取交集）；关闭 MUST 从工具/CLI 面移除并返回
  typed error（`team_cli_disabled`/`team_cli_not_allowed`/`team_cli_not_declared`）。
- 成员增删 MUST 只改 team 配置（DB-backed），MUST NOT 改写共享 provider 配置；
  运行中修改 MUST NOT 影响已启动 run。

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

### Requirement: 会话绑定随输入携带（selection.teamId / agentId）

会话的执行绑定 MUST 随输入事实携带：`user.message.payload.selection` 增加 `agentId` 与
`teamId` 键，同现时 `teamId` 优先。租户文档默认 `selection` MUST 仍只接受模型键——
文档级绑定会让租户内所有会话静默变成某个 Agent。

- 提交时 MUST 解析绑定（存在性/归档/团队已 Seal）：未知/归档/未封存 MUST 在提交处 400、
  字段路径点名，MUST NOT 先存历史后失败。
- 团队模式 run 的工具面 MUST 自动包含委派原语（`spawn_subagent`）；唯一例外是 captain 的
  `tools` 显式列出它且 `enabled=false`。
- 团队模式队长的系统提示词 MUST 按「输入 selection > Agent 默认模型 > 租户默认」解析。

#### Scenario: 团队会话的输入
- **WHEN** 输入的 selection 为 `{providerId, modelId, teamId}`
- **THEN** 提交通过；该 run 使用 captain 的 systemPrompt 与工具授权，队长的默认模型仅在没有
  显式 modelId 时兜底

#### Scenario: 未知团队
- **WHEN** 输入的 selection 命名一个不存在或已归档的 teamId
- **THEN** 提交返回 400 并点名该 teamId；事件日志不新增任何事件

#### Scenario: 文档不得绑定执行身份
- **WHEN** 租户文档的 selection 携带 teamId 或 agentId
- **THEN** 文档被拒绝（未知键），文档仍只描述模型身份

### Requirement: run 冻结执行绑定快照

每次 attempt 启动时 MUST 冻结执行绑定并把身份写进 `run.finished` 的 attempt 摘要：
`agentId`、`teamId`（团队模式）、`teamRevision`、`teamHash`、`promptHash`（attempt 实际
system prompt 的摘要）。运行中编辑 Agent/Team MUST NOT 影响已启动的 attempt；下一次 attempt
解析新配置。

- teamHash/teamRevision MUST 来自存储文档（`agentteam.Seal` 的 revision 与内容 hash），
  MUST NOT 由客户端提供。
- 无法解析绑定（文档缺失/损坏）时 attempt MUST 在任何 commit 之前失败，MUST NOT 降级为
  「无绑定」静默运行。

#### Scenario: 运行中修改团队
- **WHEN** 团队 run 执行期间用户改了成员或提示词
- **THEN** 该 attempt 的 promptHash/teamHash 仍是旧值；下一次 attempt 使用新快照

#### Scenario: 摘要可审计
- **WHEN** 读取该会话的事件流
- **THEN** `run.finished` 带 agentId/teamId/teamRevision/teamHash/promptHash，可据此证明
  「哪个 Agent、哪一版团队、哪份提示词」回答了这次 attempt
