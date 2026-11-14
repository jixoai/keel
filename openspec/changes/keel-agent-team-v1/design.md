# Design: keel-agent-team-v1

## 0. 本 change 的裁决来源

Owner 原话见 `user-requirements.md`（逐字记录，不得改写）。本设计把「模型预设」整段替换为
**Agent / AgentTeam 预设**；模型 Provider 仍保留，但不再作为会话配置的顶层单元。

### Owner 已澄清的本质

- **AgentTeam 不是新的运行拓扑**：它不引入第二套 session/run 机制；成员就是按 Agent 配置
  启动的 Agent，复用普通模式同样的运行机制。不要把它复杂化成“多会话拓扑”。
- **AgentTeam 是一个配置空间**：在 team 配置里自由组合 model provider 与 agent provider，
  并对组合做微调；这些微调只落在 team 配置里，**不污染** modelprovider/agentprovider 的
  共享配置。普通模式也能做同样的事，但需要把调整落到共享配置上，久而久之产生配置污染。
- **价值是开箱即用的协作**：team 配置把提示词/角色/模型/工具等磨合结果固化下来，用户直接
  选一个磨合好的团队，而不是从零拼装。
- **一切皆文件，文件可落地数据库**：team 配置是一份文件（DB-backed），队长 CLI 的成员增删
  修改这份 team 配置文件；session 启动时冻结快照，运行中修改不影响当前 run。model/agent
  provider 的共享配置不被改写。


## 1. 分层

```
ModelPlugin ──► ProviderInstance / ModelCatalog
                     │
AgentPlugin ──► Agent（名字/头像/默认模型/工具/AGENTS.md）
                     │
AgentTeamPlugin ──► AgentTeam（队长必选 + 成员 + 团队能力/CLI）
                     │
Session ──► 启动模式：standard（选模型） | team（选团队预设）
                     │
Harness ──► 解析为单 Agent 的 LLM 绑定 + 工具/提示词/CLI 能力 → AgentLoop
```

- **内核**只接收一个解析后的 Agent 运行配置（LLM 绑定、工具清单、system sections、AGENTS.md）。
  内核不知道 AgentTeam、Provider 厂商或插件 CLI；这些都在 Harness/插件侧解析。
- **ModelPlugin** 管模型供应商；**AgentPlugin** 管 Agent 智能体；**AgentTeamPlugin** 管团队。
  三者都用 CLI + UI 管理，且都通过统一的插件能力树声明自己。
- **subagent 能力**由 agent-plugin 天然提供；AgentTeam 不是 subagent 的集合，成员是独立 Agent。

## 2. 插件能力树

每个插件 MUST 声明 `provides.cli`（能力 + 子命令），作为设置页、`--help`、创造模式和
Agent 自身可调用能力的**同源真值**。

```yaml
provides:
  cli:
    - path: ["model", "provider", "list"]
      summary: "列出模型 Provider"
      description: "按类型/状态过滤；不返回凭据明文。"
      args_schema: { type: object, properties: { type: { enum: [llm,image,video,decision,tts,stt] } } }
      output_schema: { type: array, items: { $ref: "#/definitions/model_provider_summary" } }
      permissions: ["model.read"]
      danger: read          # read | write | destructive
      idempotent: true
      callable_by: [ui, agent, team_captain]
    - path: ["model", "provider", "add"]
      summary: "新增模型 Provider"
      args_schema: { type: object, properties: { type: { type: string }, name: { type: string } } }
      output_schema: { type: object }
      permissions: ["model.write"]
      danger: write
      callable_by: [ui, agent]
```

规则：

- `path` MUST 是稳定命令树；子命令 MUST 能拼出 `keel <group> <subcommand>`。
- 每个能力 MUST 有 `summary` 与 `description`，MUST 声明 `args_schema`、`output_schema`、
  `permissions`、`danger`、`idempotent`、`callable_by`；写/破坏性能力 MUST 有显式确认语义。
  缺任一 schema 或 permissions 为空即插件装配失败（`pluginpkg.Validate` 是唯一校验器）。
- 能力树 MUST 可机器读取（`keel plugin capabilities --json`）且与 `--help`/UI 一致。
- 插件卸载/禁用后，其能力 MUST 从 UI、CLI 和 Agent 工具面同时消失；不得留下死命令。
- Agent/Team 的 CLI 能力可以按实例禁用（例如队长 `agent-team` CLI 关闭后不能自建成员）。

## 3. 模型插件（model-plugin）

沿用 Provider 设计，但明确 CLI 管理面：

- Provider 类型：`llm` / `image` / `video` / `decision` / `tts` / `stt`，由插件声明。
- 实例：类型、插件来源、凭据引用、endpoint、模型目录、参数 schema、健康状态。
- CLI：
  - `keel model provider list|show|add|update|remove|test`
  - `keel model catalog <provider-id> [--refresh]`
  - `keel model capability`（可选：列出类型/参数 schema）
- 凭据只存 `credentialRef`；CLI/UI/事件/日志不得出现明文。
- 删除被 Agent/Team 引用的 Provider 必须拒绝并列出引用方。

详细的类型/实例/模型 schema 与决策 Provider 仍见
`specs/provider-registry/spec.md` 与 `specs/decision-provider/spec.md`。

## 4. Agent 插件（agent-plugin）

Agent 是完整智能体配置，不是 subagent：

```json
{
  "id": "agent_design_lead",
  "name": "设计主理人",
  "description": "负责视觉方向、组件规范与评审",
  "avatar": { "kind": "identicon", "seed": "agent_design_lead" },
  "defaultModel": {
    "providerId": "prov_ds", "modelId": "deepseek-v4.1",
    "tuning": { "reasoning": "high", "context": { "maxInput": 96000 } }
  },
  "tools": [
    { "toolId": "bash", "enabled": true },
    { "toolId": "read", "enabled": true },
    { "toolId": "write", "enabled": true },
    { "toolId": "keel.tasks", "enabled": true }
  ],
  "systemPrompt": { "kind": "file", "ref": "AGENTS.md" },
  "skills": ["design-review"],
  "teamCli": { "enabled": true, "commands": ["member.add", "member.remove"] }
}
```

规则：

- **默认模型**只是默认；会话/任务/团队可以显式覆盖，不得把 Agent 写死到某一模型。
- **工具能力** MUST 从插件工具注册源选择（`/v1/plugins/tools` 或 `keel tools list`）；
  Agent 配置只保存 toolId + 开关/配置，不得内联一份硬编码工具清单。
- **系统提示词**支持 inline 或文件引用（AGENTS.md）；解析后进入 run 的 system sections。
- **头像**默认确定性 identicon（GitHub 风格 5×5 网格），由 `seed`（默认 agent id）本地生成，
  不依赖外部头像服务；支持上传/URL 覆盖。相同 seed 必须得到相同头像。
- **description/标签**用于团队选择、Agent 库和会话显示；可由用户自由编辑。
- **CLI**：`keel agent list|show|create|update|delete|start|stop`；
  `start` 是启动，`create/update/delete` 是管理，不得只提供 start。
- Agent 删除前检查团队/会话引用；被引用时只能归档。

## 5. AgentTeam 插件（agent-team-plugin）

```json
{
  "id": "team_product",
  "name": "产品小队",
  "description": "产品、设计、工程三角协作",
  "captain": { "agentId": "agent_product_lead" },
  "members": [
    { "agentId": "agent_design_lead", "label": "设计", "description": "视觉与组件" },
    { "agentId": "agent_engineer", "label": "工程", "description": "实现与测试" }
  ],
  "defaults": { "tools": ["bash", "read", "write"], "modelOverride": null },
  "capabilities": {
    "teamCli": { "enabled": true, "commands": ["member.add", "member.remove", "member.list", "member.save"] }
  },
  "featureBindings": [
    { "port": "team.assign", "cli": "keel team assign" }
  ]
}
```

规则：

- **captain 必选且恰好一个**；成员 0..N，允许只有队长的团队。
- 成员 MUST 是 Agent 配置（引用 Agent 库或 inline 定义），MUST NOT 是“subagent 参数”。
- 团队预设保存队长、成员、团队级默认（工具/模型覆盖）、能力开关、feature 绑定。
  `featureBindings` 是团队预设的真实字段（port + cli），进入内容 hash 与 run 快照。
- `allowMemberCreate` / `allowMemberRemove` **不是独立字段**：它们只是 `commands` 白名单里
  `member.add` / `member.remove` 的语义别名，单独存在会造出第二套真值。单一真值＝
  `teamCli.commands`。
- **队长 CLI**：
  - `keel team show`, `keel team member add|remove|list|save`,
    `keel team save`, `keel team start`。
  - 队长能否调用由 `capabilities.teamCli.enabled` 与逐命令白名单决定；Agent 级
    `teamCli` 与团队级 `teamCli` 同时存在时：两侧都开才开，白名单取交集（`EffectiveTeamCLI`），
    关闭后这些命令必须从队长的工具/CLI 面消失，直接调用返回 typed permission error
    （`team_cli_disabled` / `team_cli_not_allowed` / `team_cli_not_declared`）。
- **变更落点（一切皆文件）**：team 配置是一份 DB-backed 文件；CLI 的成员增删修改这份
  team 配置，MUST NOT 改写 modelprovider/agentprovider 的共享配置。session 启动时冻结
  team 配置快照；运行中修改不影响当前 run，下一次 run 使用新快照。
- **运行机制**：成员复用既有 Agent/run 机制启动；AgentTeam 只提供配置空间、成员关系和
  编排能力，不定义第二套 session/run 拓扑。
- Team CLI 本身也是插件能力树的一部分：`keel team ...` 的每个子命令都要有
  summary/description/permissions/danger/callable_by。

## 6. 会话启动模式

会话创建 MUST 先选择启动模式：

### Standard（标准模式）

- 单会话；用户选择一个模型（`providerId + modelId`），或选择一个默认 Agent。
- 启动后是一个 main-agent；它可以通过 bash 参数/agent-plugin CLI 启动其它 Agent，
  这就是天然的 subagent 能力。
- 不要求创建团队，不显示队长概念。

### AgentTeam（团队模式）

- 用户选择一个**团队预设**（不是模型预设）。
- 团队必须有 captain；成员可以固定、为空，或由队长运行时组装。
- 启动后 captain 是主 Agent；成员按既有 Agent 机制启动（或按团队策略懒启动）。
  团队配置只负责组合与微调，不引入额外运行拓扑。
- 会话 header 显示团队名 + 队长；成员列表/状态可见；缺 captain 的团队不能启动。

#### 兼容

- 旧的模型选择保留为 Standard 的快速选择；旧模型预设**不迁移**，按 Owner 1A 直接删除。

## 7. 工具注册源

- `keel.tools`（或等价 harness 插件）提供工具注册源：toolId、来源插件、summary、
  input/output schema、权限、危险等级、可被 Agent/Team 调用性。
- Agent 编辑器从该注册源搜索/分组/选择工具；团队可以给成员默认工具集，但不得覆盖成员的
  安全权限上限。
- 工具清单哈希进入 run manifest；安装/卸载插件导致的工具面变化必须可审计。
- 模型可见工具名保持稳定；MCP/技能/任务等动态能力通过 action 进入，不新增工具名。

## 8. 设置页与会话 UI

### 8.1 信息架构（完整页面）

```
设置
├─ 模型
│  ├─ Model Providers
│  └─ 凭据
├─ Agents
│  ├─ Agent 库
│  └─ 新建 Agent
├─ AgentTeam
│  ├─ 团队预设
│  └─ 新建团队
├─ 工具
│  └─ 工具注册源 / 能力
├─ 能力
│  ├─ 图像 / 视频 / 决策 / 语音（TTS、STT）
├─ 客户端
│  ├─ 外观 / 通知
├─ 插件
│  ├─ 已安装 / 市场 / 创造模式（能力树）
└─ 高级
   ├─ 日志与诊断 / 更新
```

### 8.2 组件（先部件后组合）

| # | 组件 | 关键状态 |
|---|---|---|
| A1 | Agent 卡片 | ready/archived/缺模型/缺工具；头像、名字、描述、默认模型、工具数 |
| A2 | Agent 编辑器 | 身份（名字/头像/描述）、默认模型、工具选择器、AGENTS.md、团队 CLI 开关 |
| A3 | 头像生成器 | identicon 预览/换 seed/上传/URL；确定性提示 |
| A4 | 工具注册源选择器 | 搜索/分组/权限/危险等级/全选；空/加载/错误 |
| A5 | 团队卡片 | captain 必选、成员数、能力开关、valid/invalid |
| A6 | 团队画布 | captain 槽（必填）、成员列表、添加成员/新建 Agent、默认工具/模型 |
| A7 | 成员卡 | Agent 头像/名字/标签/描述/默认模型/状态；可移除/替换 |
| A8 | Team CLI 能力面板 | enabled、允许的子命令、关闭影响说明 |
| A9 | 会话启动模式选择 | Standard / AgentTeam；Standard 选模型，Team 选团队 |
| A10 | 团队预设选择器 | 当前团队/captain/成员/无效原因 |
| A11 | 插件能力树 | CLI 命令/子命令/描述/权限/危险等级/可调用者 |
| A12 | 状态组件 | 空/加载/错误/权限不足/成功 |

### 8.3 页面组合

- **Agents**：A1 网格 + A2 右侧编辑；A3 在身份区；A4 在工具区。
- **AgentTeam**：A5 列表 + A6 画布；A7 成员卡；A8 能力面板。
- **会话启动**：A9 模式选择 → Standard 选模型 / Team 选 A10 团队。
- **会话 header**：团队名/队长；成员抽屉；切换团队只影响下一次 run。
- **插件**：A11 能力树，展示 `keel model`/`keel agent`/`keel team` 的完整命令树。

### 8.4 视觉/交互

- 沿用 ZCode 的 `text-ui-*`、solid-glass、shadcn 组件形态；设置页是完整页面。
- Agent 名字、头像、描述、绑定标签均可编辑；默认值只是建议，不是不可变文案。
- 头像默认 identicon 必须本地/确定性；上传/URL 是覆盖。
- 五态（空/加载/错误/权限不足/成功）+ 390px 单列 + 键盘可达。
- 不写死工具列表/团队结构/模型名；全部从注册源和 schema 渲染。

## 9. 数据、API 与迁移

```sql
model_providers(id, tenant_id, type_id, plugin_id, name, credential_ref, endpoint, config_json, status, ...)
model_provider_models(provider_id, model_id, display, capabilities_json, context, max_output, parameter_schema_ref, ...)
agents(id, tenant_id, name, description, avatar_kind, avatar_seed, avatar_url, default_provider_id, default_model_id, tuning_json, system_prompt_ref, team_cli_json, ...)
agent_tools(agent_id, tool_id, enabled, config_json)
agent_teams(id, tenant_id, name, description, captain_agent_id, defaults_json, capabilities_json, ...)
agent_team_members(team_id, agent_id, label, description, position)
session_agent(session_id, mode, model_provider_id, model_id, agent_id, team_id)
run_manifest(... agent_snapshot_json, team_snapshot_json, tool_manifest_hash ...)
```

- API：`/v1/model/providers`、`/v1/model/providers/{id}/models`、`/v1/agents`、
  `/v1/agent-teams`、`/v1/plugins/capabilities`、`/v1/sessions/{id}/agent|team`。
- 所有表租户隔离；凭据只存 ref。
- **破坏性移除（Owner 1A）**：旧模型预设表/API/UI 直接删除，不做迁移、不做兼容层；
  旧单模型下拉配置不映射为 Agent/Team。新版本从 Provider / Agent / Team 重新配置。
  旧会话历史仍可读，但不能用旧 preset 启动新的 standard/team run。
- 运行快照：启动时冻结 agent/team/tool/prompt hash；运行中编辑 Agent/Team 不影响当前 run。

## 10. 验收要点

- 创建 Agent：名字/头像/默认模型/工具/AGENTS.md；头像 identicon 对相同 seed 稳定。
- 创建团队：captain 必选；只配 captain 也可启动；成员可引用已有 Agent 或现场创建。
- Standard 启动选模型；Team 启动选团队；团队缺 captain 不能启动。
- 队长 CLI：开启时可 `keel team member add`；关闭后命令从工具/CLI 面消失。
- team 配置是 DB-backed 文件；队长 CLI 修改 team 配置，不污染 model/agent provider。
- session 启动冻结团队快照；运行中修改不影响当前 run。
- 工具列表来自注册源；新增/卸载插件后 UI/CLI/Agent 工具面同步。
- 设置页 390px 单列；所有状态有文字+图标；凭据零泄漏。
