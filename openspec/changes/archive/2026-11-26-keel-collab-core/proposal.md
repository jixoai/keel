# Proposal: keel-collab-core

> 立项：2026-11-25。来源：Bot 角色模型、sub-agent 讨论、`dsh-superagent-prototype` 研究。
> 目标：把“Bot 是角色”和“协作模式是可变拓扑”固化成内核/宿主契约，供 subagent、superagent
> 及未来模式复用；不把任何一种拓扑写成 Bot 类型。

## Why

当前 Bot 已经是稳定的角色主体（Profile + ModelKind + Harness + 权限），但协作能力仍以
`subagent` 这一种父子树为中心。研究 `dsh-superagent-prototype` 后确认：super-agent 不是
“更大的 sub-agent”，而是 Source → Ticket → Assignment → Worker 的工单/调度工作系统。
两者拓扑、生命周期、人机边界和外部世界接口都不同，未来还会出现 pipeline、peer review、
swarm 等模式。

如果把 main/sub 或 super-agent 写成 `BotKind`，会把“模式”误固化进“角色”，并让权限、
Session 归属、审计出现第二套语义。需要先建立一个 mode-agnostic 的协作核心：
Bot 只定义角色；协作模式声明拓扑；所有模式共用受控的创建、预算、权限和审计原语。

## What Changes

1. **CollaborationMode 契约**：每个模式声明 id、所需 Bot 角色、拓扑、工作单元、
   Assignment 语义、输入/输出、Attention、预算/并发/深度上限、生命周期和 UI 入口。
   mode 是插件；Bot 不新增 kind。
2. **通用协作原语（端口，不先绑定存储）**：
   - `WorkItem`：可持久的工作单元（工单/任务），带来源、授权范围、状态和输入；
   - `Assignment`：WorkItem → Bot/Session 的绑定，独立于 Session 生命周期；
   - `Attention`：结构化的人机决策请求，复用 decision 语义；
   - `Source` / `Channel`：外部事件的读写边界；
   - `Link`：`delegation | assignment | subscription | review` 等关系，不写死父子。
3. **受控 Host**：模式插件只能通过 `collab.Host` 创建/唤醒 Session；Host 统一执行租户、
   `bot.invoke` 权限、预算、深度、并发、取消和审计。模式不得直接访问 kernel/store 创建
   Session，也不得自己实现深度计数。
4. **模式注册表**：harness 装配期注册内置模式；插件可注册新模式；UI 只读发现当前部署
   启用的模式。模式被禁用时，已有 WorkItem/Assignment 仍可读、可审计、可人工接管。
5. **Bot 模型不变**：Bot 仍是角色；同一个 Bot 可以在不同 WorkItem/Session 中以不同 mode
   运行。单租户部署 = 一个 tenant，不使用第二套代码路径。

## Capabilities

### New Capabilities

- `collaboration-core`: Bot 角色与协作模式的分离、模式契约、受控 Host、通用协作原语和
  租户/权限/预算/审计不变量。

### Modified Capabilities

<!-- 本 change 只引入新契约；既有 tool-surface / task-log 的行为由后续模式 change 修改。 -->

## Impact

- 新增 `internal/collab`（或同义包）：Mode 描述、Registry、Host 端口、Link/WorkItem/
  Assignment/Attention 接口、类型化错误。
- `cmd/keel`/`internal/servicehost` 装配模式注册表；现有 `SubagentRunner` 先适配为
  `subagent` mode 的 Host 使用者，不改变现有 API。
- 不新增 BotKind、不改变 `bots` 表语义；新模式元数据按 tenant 存储。
- 为 `keel-collab-subagent`、`keel-collab-superagent` 提供契约；两者可并行实现，
  core 的 Host/Mode 接口先冻结。

## Out of scope

- super-agent 的 Ticket/Assignment 持久化与 Source 适配（在 `keel-collab-superagent`）。
- sub-agent 的深度/预算落地（在 `keel-collab-subagent`）。
- macOS 原生化、公证、特权 helper；keel 只通过外置程序对接特殊权限。
- 把 `keel-ecosystem` 旧板整体解冻。
