# Proposal: keel-agent-team-v1

## Why

上一个设计把「模型预设」当作会话的配置单元，但真正的复用单元不是模型组合，而是
**Agent** 与 **AgentTeam**。一个会话需要的不只是模型，还需要：

- 一个可命名的 Agent（名字、头像、默认模型、工具能力、系统提示词/AGENTS.md）；
- 一个可复用的团队（必须有队长，其余成员自由组合）；
- 模型、Agent、团队都通过插件扩展，并且都用 CLI 管理，而不是写死在 UI/内核里。

因此本 change 把「模型预设」整段替换为 **AgentTeam 预设**，并补齐 plugin 的
**能力声明树（CLI + 子命令 + 特性描述）**，让模型、Agent、团队都能被插件化地注册、
发现、配置和 CLI 管理。

AgentTeam 的直接价值不是“另一种运行拓扑”，而是**配置空间**：普通模式也能完成同样的事，
但要把模型/Agent 的调整落到共享 provider 配置上，造成配置污染；AgentTeam 把
model + agent + 提示词 + 工具 + 微调的磨合结果固化在 team 配置里，开箱即用。
team 配置是一份文件（可落地数据库），队长 CLI 修改这份配置，不改写共享 provider/agent 配置。

## What Changes

### 1. 模型插件（model-plugin）

- 继续提供多种模型 Provider：LLM / 图像 / 视频 / 决策 / TTS / STT；类型由插件声明。
- Provider 实例、模型目录、参数 schema、凭据引用、健康检查保持原设计。
- **新增 CLI 管理能力**：`keel model provider list|add|update|remove|test`、
  `keel model catalog`；CLI 与 UI 共用同一注册源。

### 2. Agent 插件（agent-plugin）

Agent 是**完整的智能体配置**，不是 subagent 的别名。一个 Agent 至少可配置：

1. **默认模型**：指向 model provider 的具体模型绑定；只是默认，允许按任务/会话覆盖。
2. **工具能力**：从插件提供的**工具注册源**中选择；不得在 UI 写死工具列表。
3. **系统提示词**：AGENTS.md 正文/引用，支持按 Agent 覆盖。
4. **名字与头像**：名字可编辑；头像默认用确定性 identicon（GitHub/npm 风格头像那套），
   支持上传/URL 覆盖。
5. 描述、标签、技能、权限、团队 CLI 能力开关等扩展字段。

Agent 插件 MUST 提供 CLI：`keel agent list|show|create|update|delete|start|stop`，
不仅能启动，也能管理 Agent 配置。

### 3. AgentTeam 插件（agent-team-plugin）

- **AgentTeam 必须有队长（captain）**；队长是 Agent 的一个实例。
- 成员自由：可以 0 个、1 个或 N 个；成员本身就是 Agent 配置，可以引用已有 Agent 或现场创建。
- Team 预设保存：队长、成员、团队级默认模型/工具、团队能力开关、CLI 组装权限。
- AgentTeam 插件提供 CLI + UI：`keel team list|show|create|update|delete|member add|remove|start`。
- **队长可以通过 CLI 自己创建成员/组装团队**；只有显式禁用队长的
  `agent-team` CLI 能力时，团队才被锁死。
- 团队不是 subagent：成员是独立 Agent（有自己的配置、模型、工具、提示词），由团队插件
  管理；subagent 能力仍由 agent-plugin 天然提供。

### 4. 会话启动模式

会话开始时选择：

- **标准模式**：单会话；选择一个模型（或默认 Agent），直接开始。main-agent 仍可通过
  bash 参数启动其它 Agent（subagent 能力）。
- **AgentTeam 模式**：选择一个团队预设；必须包含队长；团队成员可固定或由队长运行时组装。

### 5. Plugin 能力声明树

每个插件 MUST 声明自己的能力与子能力（CLI/sub-command），包含：

- 命令路径、摘要、描述、参数/输出 schema、权限、危险等级、示例；
- 能力特性（是否只读、是否幂等、是否可被 Agent 调用、是否可被团队 CLI 调用）；
- 机器可读（`extensibility.json` / manifest）与人类可读（`--help` / EXTENSIBILITY.md）同源。

这样模型、Agent、团队、工具的能力都能被设置页、创造模式和 Agent 自身发现与管理，而不是
靠硬编码菜单。

### 6. 设置与 UI

- 设置仍是完整页面：模型 Provider / Agents / AgentTeam / 工具 / 凭据 / 能力 / 客户端 / 插件 / 高级。
- 新增 Agent 卡片、Agent 编辑器、头像生成、工具注册源选择器、团队画布、队长槽、成员库、
  Team CLI 权限开关、会话启动模式选择器。
- 旧的「模型预设」页面改名为「AgentTeam」；标准模式保留模型快速选择。

## Non-Goals

- 不把 AgentTeam 做成 subagent 的集合；成员是完整 Agent，不是一次工具调用的子过程。
- 不写死工具列表；工具必须来自插件注册源。
- 不写死头像服务；默认 identicon 必须可离线生成，外部 URL 只是覆盖。
- 不做计费/价格；usage 只计量。
- 不要求团队必须有成员；只要求有队长。
- 不新增第二套 transport；Agent/Team CLI 复用既有 bash/plugin 通道。

## Impact

- **DB**：`model_providers`、`model_provider_models`、`agents`、`agent_teams`、
  `agent_team_members`、`session_agent` / `session_team`、run manifest 增加 agent/team 快照。
- **API**：`/v1/model/providers`、`/v1/agents`、`/v1/agent-teams`、`/v1/plugins/capabilities`、
  `/v1/sessions/{id}/agent` 与 `/v1/sessions/{id}/team`。
- **插件清单**：`provides.provider_types`、`provides.provider`（layer + feature ports）、
  `provides.feature_ports`、`provides.agents`、`provides.agent_teams`、
  `provides.tools`（注册源）、`provides.cli`（能力树）。
- **CLI**：`keel model ...`、`keel agent ...`、`keel team ...`、`keel plugin capabilities`。
- **客户端**：设置页 Agent/Team 视图、会话启动模式、团队画布、头像 identicon、工具选择器。
- **内核**：仍只接收解析后的单 Agent LLM 绑定；Agent/Team 解析在 Harness/插件侧，内核不 import。
- **测试**：capability tree schema、团队队长约束、CLI 权限开关、工具注册源、头像确定性、
  会话快照、凭据零泄漏、旧模型预设破坏性删除（不做迁移）。
