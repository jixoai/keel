# Proposal: keel-collab-subagent

> 立项：2026-11-25。依赖 `keel-collab-core` 的 Mode/Host 契约。
> 定位：sub-agent 是一种“树形委派”协作模式，不是 Bot 类型，也不是 super-agent 的子集。

## Why

现有 `agent` 工具和 `/v1/sessions/{id}/subagents` 已能派生子会话，但没有统一的深度语义：
入口不同、父链接只存在于首条消息 payload、没有 durable depth、没有统一的最大深度/子节点数
限制。讨论确认：

- Bot 是角色；
- main/sub 是工作模式；
- 默认最大深度 3，`-1` 表示不限，`0` 表示不允许派子代理；
- 同一 Bot 可以在不同任务中当 main 或 sub；
- 权限和审计跟着 Bot 角色走，不跟着 main/sub 位置走。

本 change 把 sub-agent 做成 `keel-collab-core` 的第一个正式模式，并补上安全不变量。

## What Changes

1. **subagent mode 描述**：roles = main/sub；topology = tree；workUnit = delegation；
   支持 `maxDepth`、`maxChildren`、turns/tokens/timeout 预算。
2. **深度语义**：main = 0；每次 spawn 子 = 父 + 1；`maxDepth=3` 允许 0..3；`-1` 不限；
   `0` 禁止派生；超限在创建子 Session 前拒绝（`subagent_depth_exceeded`）。
3. **唯一 spawn 门**：`agent` 工具、HTTP 子代理路由、触发器和未来插件统一走 Host；
   子 Session 的首条 durable 记录包含 `mode=subagent`、`parentSessionId`、`depth`。
4. **预算/取消/审计**：深度、子节点数、turns/tokens/timeout 与取消传播统一由 Host 执行；
   每次 spawn/拒绝写审计，不记录正文/密钥。
5. **结果与 UI**：父 Session 收结构化结果（status/summary/artifacts/usage）；web/TUI 展示
   子代理树、取消/重试；不新增 BotKind。
6. **fork 与 spawn 区分**：fork 是账本分叉，不消耗 sub-agent depth；只有 spawn 委派树计数。

## Capabilities

### New Capabilities

- `collaboration-subagent`: sub-agent 模式的树形委派、深度/预算/权限/取消/审计与 UI。

### Modified Capabilities

<!-- 现有 tool-surface 的行为由 core Host 适配；spec 级变化在实现时按需补 delta。 -->

## Impact

- `internal/harnessext/subagent*`、`internal/toolsurface/agent*`、HTTP 子代理路由；
  `cmd/keel/serve.go` 装配 subagent mode。
- durable 子链接在首条输入/事件 payload；不新增 kernel Bot 语义。
- 现有 `agent` 工具参数/返回保持兼容；新增可选 `maxDepth`/`depth` 投影。
- 测试：深度 0/3/-1、递归、重启恢复、取消传播、权限、租户隔离、无孤儿。

## Out of scope

- super-agent 的 Ticket/Assignment/Source/Attention（`keel-collab-superagent`）。
- fork 的账本语义修改；fork 仍按既有 eventSeq 规则。
- 跨租户子代理、跨部署子代理、远程 runtime 的 sub-agent（后续 change）。
