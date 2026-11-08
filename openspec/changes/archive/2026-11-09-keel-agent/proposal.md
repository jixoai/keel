# Proposal: keel-agent

> 立项：2026-11-08。**分层纪律（Owner 裁决）**：内核（`keel-kernel-loop`）只做 loop/事件/工具/
> 账本；**本 change 是 keel 的 agent 产品层**——`keel serve` 启动 Agent 服务，供 AI UI 对接。
> keel-agent 是内核的「第一验证者」：内核价值由它证明。UI 与 TUI 是平级消费方，不盖过后端。

## Why

内核已具备 loop/账本/服务图/handoff，但「产品」还缺：

- 没有统一入口：现在只有 `cmd/runtime`，没有 `keel` CLI（`keel serve` / `keel tui`）；
- 没有产品级 AgentAPI：会话列表、停止、审批（decision）、token 级 partial 都缺；
- 没有扩展面：插件、sub-agent、触发器（时间/WebHook）、hooks 都没有承载点；
- 没有客户端：web 与 TUI 都没有。

## What Changes

### 1. `keel` CLI 与 `keel serve`

- 新二进制 `cmd/keel`：`keel serve`（Agent 服务，HTTP/SSE + 静态 UI + 认证中间件）、
  `keel tui`（NSH TUI 客户端）、`keel version|doctor`。
- `keel serve` 复用现有装配（servicehost/loadout），把产品层能力挂上；**内核包不改**。

### 2. AgentAPI（产品面）

- `GET /v1/sessions`（列表/分页）、`POST /v1/sessions/{id}/cancel`（停止，幂等）、
  `POST /v1/sessions/{id}/decisions/{decisionId}`（审批答案，幂等）、
  `GET /v1/sessions/{id}/events/stream?partials=1`（token 级 partial 旁路）。
- AgentAPI 版本化：`/v1` 前缀 + 能力探测（`GET /v1/capabilities`）。
- 认证在服务中间件层（cookie/网络中间件），内核仍只认 `X-Tenant-ID`。

### 3. 扩展面（第一期就做）

- **插件（Agent plugin）**：`agentext` 契约——插件声明 `provides/requires/hooks/triggers/tools`，
  随 loadout 装配，effect 反序卸载；内置示例插件。
- **Sub-agent**：工具/原语 `spawn_subagent(task, agent, budget)`；子 run 是独立会话，
  父会话拿到结构化结果；生命周期事件与 hooks（见下）。
- **触发器（Trigger）**：时间（cron/interval）与 WebHook（HTTP 回调）两类；触发 → 创建/唤醒
  会话 → 开始一个 turn；触发器有幂等键与审计。
- **Hooks（第一期的最小闭集，后续只增不改）**：
  `subagent.started` / `subagent.finished`、`trigger.before` / `trigger.after`、
  `turn.started` / `turn.finished`、`decision.opened` / `decision.answered`。
  hook 是声明式订阅（插件/配置），执行 MUST 不阻塞账本；失败可见、可重试。

### 4. 客户端（平级消费方）

- **NSH web**（`web/`）：会话列表/视图/composer（queue/guide）/审批卡/附件/断线重连；
  由 `keel serve` 同源托管。
- **NSH TUI**（`keel tui`）：同一份 NSH 代码经 TUI target 渲染（NSH 原生支持终端），
  覆盖会话列表/对话/composer/审批。
- 两者都只消费 `packages/chat-surface`（headless 核心）+ AgentAPI；不直连 SSE 帧。

## Non-Goals

- 不在产品层实现新的 loop 语义（queue/guide、inputSeqs 属内核 `keel-kernel-loop`）；
- 不做 OpenWebUI/LibreChat/Anthropic 兼容面（Owner 裁决）；
- 不做多租户 API key/RLS（`keel-ga` tenancy-quotas）。

## Impact

- 新二进制 `cmd/keel`；`internal/agentapi`（AgentAPI）、`internal/agentext`（插件/subagent/
  trigger/hooks）为新增产品包；内核包零 import 变化（纯度测试守护）。
- `web/`（NSH）+ `packages/chat-surface`（TS）；`keel serve` 同源托管。
- 验收：AgentAPI E2E、扩展面 E2E（subagent/trigger/hook）、web 与 TUI 截图、既有 11 门回归。
