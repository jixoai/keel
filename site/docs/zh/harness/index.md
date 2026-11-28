# Harness

> **Harness** 是 keel 的产品层。它把 Core 的事件账本与 loop 变成可用的智能体产品：会话、回合、
> 工具、审批、插件、触发器，以及与之对话的客户端界面。产品实体是 **Account**、**Role**、
> **Profile**、**Bot**、**TeamBot**：Bot 是可运行的 AI 员工（`Profile + ModelKind + Harness`），
> TeamBot 是 Mission + Bot 名单。旧的「Agent」/`TeamAgent` 已删除（alpha 零兼容）；
> 协议名 **AgentAPI** 与 **AgentBackend** 是契约，予以保留（见[术语表](/zh/glossary)）。

Harness 不是另一个运行时。它是建立在 Core 之上的**宿主进程**（`keel serve`），加一组从不
渗入内核的产品原语。本节描述的一切，就是你从 `cmd/keel` 构建得到的同一个二进制。

## 1. Harness 拥有什么——以及不拥有什么

Core 各包（`internal/kernel`、`internal/loop`、`internal/agentcore`、`internal/agenttool`、
`internal/agent`）只认识事件、租约、工具与转录。一切**策略**都住在 Harness 里并叠加其上：
插件装配、hooks、审批、触发器、子代理与 token 级 partials（`internal/harnessext/doc.go:1`）。

| 问题 | 由谁回答 | 出处 |
| --- | --- | --- |
| 这条事件可以落账吗？ | Core（契约内核） | `internal/kernel/validate.go:46` |
| 此刻谁拥有这个会话？ | Core（租约 + fencing epoch） | `internal/kernel/lease.go:16` |
| 模型看到什么？ | Harness 宿主装配 + loop | `internal/agent/runner.go:280` |
| 这次 run 有哪些工具？ | Harness（宿主 registry + 插件工具） | `internal/agenttool/registry.go` |
| 这个工具可以执行吗？它看到了什么？ | Harness（hooks、审批、策略） | `internal/harnessext/hooks.go:18` |
| 哪些插件处于激活状态？ | Harness（插件运行时） | `internal/harnessext/plugin.go:163` |
| 客户端展示什么？ | Harness 投影（`@keel/chat-surface`） | `packages/chat-surface/src/types.ts` |

依赖方向只有一条：**Harness → Core**。内核包永远不允许 import 产品包，且有测试机器把守
（`internal/architecture/boundary_test.go`）。

## 2. 与 Core 的关系

本站 Core 主线覆盖账本、内核动词、服务图与 loadout、存储与 handoff。对 Harness 而言，
最要紧的 Core 事实是：

- **事件日志是唯一真值。** 每次状态变化都是一条只追加事件；checkpoint 与各类投影都是派生
  （`docs/KERNEL.md`）。
- **一会话一写者。** run 只能在有效租约与当前 fencing epoch 下提交
  （`internal/kernel/lease.go:71`、`:89`）。
- **服务图负责装配，账本负责裁决。** 服务声明 `provides`/`requires` 与清理副作用；它们从不
  直接写事件表（`docs/SERVICE-GRAPH.md` §2）。
- **run 钉住一个 loadout。** 每个 run 记录 `loadout_id`/`revision`/`generation`，配置变更
  绝不打扰在途 run（`internal/agent/worker.go:70`、`docs/LOADOUT.md` §4）。

Harness 通过窄接缝消费 Core——`kernel.Committer`、`kernel.Reader`、`kernel.LeaseManager`、
`kernel.ExternalSessions`（`internal/kernel/seams.go`），以及服务图提供的 per-run 绑定
（`internal/agent/worker.go:73`）。任何 Harness 组件都不持有事件表的原始写句柄
（`docs/LOADOUT.md` §7）。

## 3. 进程视图

`keel serve` 是承载 Harness 的单一进程。它按固定顺序启动 store、契约内核、服务图（默认
loadout）与产品层，并拥有全部关闭顺序（`cmd/keel/serve.go:75`、`:213`、`:430`）。

```
keel serve（单进程）
├─ HTTP 边缘            /v1/tenants, /v1/sessions/{id}/input, /v1/sessions/{id}/events[/stream],
│                       /v1/files, /v1/runtime/{drain,status}      internal/httpapi/server.go:147
├─ 产品 API（Harness API）
│   ├─ 能力发现         GET /v1/capabilities                        internal/harnessapi/api.go:257
│   ├─ 会话             GET /v1/sessions, POST …/{id}/cancel        internal/harnessapi/api.go:171
│   ├─ 审批             GET/POST /v1/sessions/{id}/decisions[/…]    internal/harnessapi/api.go:173
│   ├─ 插件             GET /v1/plugins, POST …/install|{id}/enable|disable|uninstall|update,
│   │                   GET …/audit, POST /v1/plugin-approvals/{id} internal/harnessapi/plugins.go:56
│   ├─ 插件 UI          GET /plugins/{id}/*, /v1/plugins/{id}/bridge/* cmd/keel/plugins_ui.go:17
│   ├─ 触发器           GET /v1/triggers, POST …/{id}/fire          internal/harnessapi/ext.go:39
│   └─ 子代理           GET /v1/subagents, POST …/{id}/subagents    internal/harnessapi/ext.go:128
├─ 扩展面（harnessext）
│   ├─ 插件运行时       声明式插件、generation、反序卸载            internal/harnessext/plugin.go:163
│   ├─ 插件管理器       清单包、生命周期、权限、审计                internal/harnessext/plugin_manager_ops.go:504
│   ├─ hook 总线        闭集事件、异步、有界队列                    internal/harnessext/hooks.go:298
│   ├─ 审批             开/答/等/超时/取消 + durable 行             internal/harnessext/decisions.go:233
│   ├─ 触发器           interval/cron/webhook + 幂等 + 审计         internal/harnessext/triggers.go:305
│   ├─ 子代理           带预算与结构化结果的子会话                  internal/harnessext/subagent.go:175
│   ├─ run 控制         为正在执行的 attempt 注册取消               internal/harnessext/turns.go:223
│   └─ partials         进程本地 token 增量（永不进日志）           internal/harnessext/turns.go:360
├─ 宿主装配             system prompt、技能、工具 registry、MCP     internal/host/host.go
├─ 服务图               store/llm/prompt/mcp/tools/pi-loop/…        internal/loadout/loadout.go:63
├─ Worker               claim → execute → settle                    internal/agent/worker.go:170
└─ 静态 UI 宿主         web/dist（NSH web 构建）· /plugins/<id>/      cmd/keel/static.go:16
```

## 4. 一次会话的生命周期

下图是一个会话从客户端第一次输入到 run 结算的完整真值路径——包括进程死亡时发生什么。

```
客户端                       Harness 宿主（keel serve）              Core（内核 + 账本）
  │                              │                                        │
  │ POST /v1/sessions            │                                        │
  ├─────────────────────────────▶│ 建会话行 + session                  ───▶│
  │                              │                                        │
  │ POST /v1/sessions/{id}/input │                                        │
  ├─────────────────────────────▶│ commitUserInput（edge 类）          ───▶│ 校验 → 分配 seq → 同事务入队 run
  │   201 {seq, requestId}       │◀───────────────────────────────────────│  （200 = requestId 重放；
  │   202 = 已落账但排队中        │                                        │    202 + queued{position,etaSeconds}）
  │                              │                                        │
  │                              │ worker 轮询                            │
  │                              ├─ claim() ─────────────────────────────▶│ 租约 + fencing epoch
  │                              │  runner.RunAttempt()：                 │
  │                              │   1 读事件 ───────────────────────────▶│
  │                              │   2 记录 read watermark ──────────────▶│
  │                              │   3 解析模型 / 会话 selection          │
  │                              │   4 补齐缺失工具结果                   │
  │                              │   5 loop（模型流、工具批次）           │
  │                              │       提交 assistant.message ────────▶│ 按 generationId 去重
  │                              │       提交 tool.result ──────────────▶│ 按 toolCallId 配对
  │                              │   6 提交 run.finished（摘要）─────────▶│
  │                              │   7 finish() ────────────────────────▶│ 结算 / 重排未读输入
  │ GET /…/events/stream         │                                        │
  ├─────────────────────────────▶│ SSE：已提交事件（id: = seq）          ◀─│ LISTEN/NOTIFY 唤醒
  │   ?partials=1                │ SSE：partial 帧（无 id）              │  （进程本地 hub）
  │                              │                                        │
  │ （崩溃：kill -9）             │                     租约过期 ────────▶│ 下一次 claim 接管（epoch+1）
  │                              │                     继任者从账本重建
```

两条承载性属性，按顺序阅读：

1. **先提交，再展示。** 客户端显示的内容没有一处是本地杜撰的；投影消费的是已提交事件
   （`packages/chat-surface/src/events.ts`），乐观输入按 `requestId` 收敛
   （`packages/chat-surface/src/engine.ts`）。见[一次输入的解剖](/zh/harness/anatomy-of-a-turn)。
2. **恢复不等于续命。** 被杀死的进程由新的 claim 取代：从账本重建转录，只重做缺的那部分
   （工具是 at-least-once）。见[恢复与压缩](/zh/harness/recovery-and-compaction)。

## 5. 已实现 / 部分实现 / Deferred

本站使用三档，下表每一行都标明它属于哪一档：

- **已实现**——本仓库已交付；断言附 `path:line` 出处。
- **部分实现**——机制已交付，但某一部分缺失或比契约更窄（缺口写明）。
- **Deferred（推迟）**——已定规格并登记、不在本构建中；由 `plugins/COVERAGE.md` 跟踪，某个门被
  推迟时由其 change 的 `DISPOSITION.md` 记录。

| 领域 | 状态 | 位置 |
| --- | --- | --- |
| 账本、内核动词、恢复、handoff | 已实现 | [一次输入的解剖](/zh/harness/anatomy-of-a-turn)、[恢复与压缩](/zh/harness/recovery-and-compaction) |
| 流式、partials、drain 帧；排队准入（`202` + `queued{position,etaSeconds}`，`429` 只给租户配额/内存上限） | 已实现 | [流式与 partial](/zh/harness/streaming-and-partial) |
| 审批/问句（decision 原语） | 已实现 | [审批与问句](/zh/harness/approvals-and-questions) |
| 进程内声明式插件（`harnessext.Plugin`） | 已实现 | [插件宿主](/zh/harness/plugins/) |
| MCP Apps 宿主（目录/沙箱/桥/events/era） | 已实现（P1–P5；P6 验收进行中） | [MCP Apps](/zh/harness/mcp-apps) |
| `plugin.yaml` 包、安装/生命周期 API、sidecar 与 webview kind | 已实现；`agent_backend` sidecar 为 Deferred | [清单](/zh/harness/plugins/manifest)、[三种 kind](/zh/harness/plugins/kinds)、[生命周期](/zh/harness/plugins/lifecycle) |
| 插件权限与审计端点 | 部分实现：工具与 sidecar 环境已强制；`net`/`secrets` gate 未路由；`fs` 仅声明 | [权限](/zh/harness/plugins/permissions) |
| 插件 UI 桥（`keel.plugin.*`）、`/plugins/<id>/` 托管 | 已实现（后端 + 客户端打开路径）；客户端安装/更新卡未接线 | [插件 UI](/zh/harness/plugins/ui) |
| 创造模式（`keel.creation`） | 动作/模板已实现；端到端门 G8 为 Deferred | [创造模式](/zh/harness/creation-mode) |
| 官方插件套件覆盖矩阵 | 已实现（catalog + CI 门）；逐插件状态以矩阵自己的词表为准 | [插件总览](/zh/harness/plugins/) |

输入的状态码真值在 `internal/httpapi/server.go:497`：新建为 `201`、同一 `requestId` 重放为
`200`、输入已落账但 run 在等组槽位为 `202`，`429` 只用于租户配额或内存上限
（`internal/httpapi/server.go:924`）。

## 6. 接下来读什么

- [架构](/zh/harness/architecture)——分层、服务图、每个原语住在哪里。
- [一次输入的解剖](/zh/harness/anatomy-of-a-turn)——一次输入，逐步展开，附事件序列。
- [流式与 partial](/zh/harness/streaming-and-partial)——SSE、续传游标、反压与 token 级旁路。
- [审批与问句](/zh/harness/approvals-and-questions)——decision 原语。
- [恢复与压缩](/zh/harness/recovery-and-compaction)——崩溃、租约、handoff、上下文压缩。
- [插件](/zh/harness/plugins/)——插件宿主、包标准，以及与 DSH 的对照。
- [MCP Apps（交互界面）](/zh/harness/mcp-apps)——交互式工具结果：声明、沙箱、桥动作与降级。
- [创造模式](/zh/harness/creation-mode)——从自然语言到运行中的插件（动作已交付；端到端门为 Deferred）。
- [运维](/zh/harness/operations)——serve、TUI、诊断、升级。
