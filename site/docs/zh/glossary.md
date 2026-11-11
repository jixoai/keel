---
# 术语表只有一页：单条目的侧栏轨道只会留下大片空白，因此本页不显示侧栏。
sidebar: false
---

# 术语表

> 本项目的命名规则：产品层称 **Harness**（旧称「Agent」）。协议名 **AgentAPI** 与
> **AgentBackend** 是契约，予以保留——改名会破坏客户端与适配器。本站全部页面遵循此规则，
> 并由扫描脚本（`scripts/naming-scan.sh`）检查。

## 产品词汇

| 术语 | 定义 | 一手出处 |
| --- | --- | --- |
| **Keel** | 本项目：无状态、多租户的智能体运行时底座。 | `README.md` |
| **Core** | 契约内核 + 事件账本 + 服务图 + loadout + 存储 + handoff。不可替换的部分。 | `docs/SERVICE-GRAPH.md`、`docs/KERNEL.md` |
| **Harness**（原 Agent） | Core 之上的产品层：会话、回合、工具、审批、插件、触发器、子代理与客户端界面。进程即 `keel serve`。 | `internal/agentext/doc.go`、`docs/HARNESS.md` |
| **AgentAPI** | `/v1` 产品 API 契约（别名：Harness API）。名称保留；web UI 与 TUI 消费的 wire 契约。 | `internal/agentapi/api.go:49` |
| **AgentBackend** | 外部智能体运行时（例如 DSH）作为 external backend 被驱动的协议。名称保留。 | `internal/gateway/protocol.go`、`docs/DSH-REFERENCE.md` |
| **DSH** | DeepSeek Harness——插件模型的外部参照，也是 external-agent 目标。是参照，不是本项目的事实源。 | `docs/DSH-REFERENCE.md` |

## 会话与运行词汇

| 术语 | 定义 | 出处 |
| --- | --- | --- |
| **账本（Ledger）** | 只追加的事件日志；唯一真值。投影与 checkpoint 皆由它派生。 | `docs/KERNEL.md` |
| **事件 kind** | durable 事实的闭集词汇：`user.message`、`assistant.message`、`tool.result`、`compaction`、`run.started`/`run.failed`（保留）、`run.finished`、`external.event`。 | `internal/kernel/validate.go:36` |
| **seq** | 会话内事件序号；客户端续传的游标（`id: <seq>`）。 | `internal/httpapi/server.go:628` |
| **会话（Session）** | 一段对话：一份事件日志，任一时刻至多一个写者。 | `docs/KERNEL.md` |
| **run / attempt** | 会话待办工作的一次执行。一个 run 可被多次 attempt（重试）；每次 attempt 被 claim、围栏与结算。 | `internal/kernel/lease.go`、`internal/agent/worker.go:223` |
| **claim / lease / epoch** | 内核的所有权机制：owner 获得带 fencing 令牌 `owner@epoch` 的租约；过期 owner 被拒。 | `internal/kernel/lease.go:16` |
| **Turn（回合）** | run 内一次模型请求/响应循环；一个回合可包含一个工具批次。 | `internal/loop/loop.go` |
| **`inputSeqs`** | `assistant.message.payload.inputSeqs`——某次请求呈现给模型的 user 消息序号集合。消费事实：输入被回答当且仅当有完成态 assistant 消息引用它。 | `internal/agent/runner.go:707` |
| **read watermark** | attempt 装载到的最高已提交 seq；水位之上的输入在结算时使会话重排队。 | `internal/agent/worker.go:399` |
| **`delivery: queue` / `guide`** | 已提交输入的消费方式：`queue` 等下一次 claim；`guide` 在 drain 点注入运行中的回合（无活跃 attempt 时降级为 `queue`，且在事件里可见）。 | `docs/KERNEL.md` §2 |
| **Partial** | 沿日志旁路传输的 token 级增量；永不入库、永不重放、不携带游标。 | `internal/httpapi/partials.go:5` |
| **压缩（Compaction）** | 用一条已提交的摘要事件替换转录最旧的一段；历史绝不被编辑。 | `internal/compaction/compact.go` |
| **Handoff** | 跨进程接替：新 runtime、排空旧的、新 epoch、从账本继续。 | `docs/HANDOFF.md` |
| **Decision（审批/问句）** | 审批/问句原语：durable 行、幂等作答、有界超时。不是内核事件 kind。 | `internal/agentext/decisions.go:233` |
| **触发器（Trigger）** | 定时或 webhook 启动的回合（`interval`/`cron`/`webhook`），带幂等键与审计。 | `internal/agentext/triggers.go:30` |
| **子代理（Sub-agent）** | 由工具或 API 派生的子会话，带预算与结构化结果。 | `internal/agentext/subagent.go:44` |

## 装配词汇

| 术语 | 定义 | 出处 |
| --- | --- | --- |
| **服务图（Service graph）** | 一方服务的装配：按 capability 解析依赖、有序激活、反序拆卸。 | `internal/servicegraph/lifecycle.go:21` |
| **Loadout** | 服务集合的不可变快照 `{id, revision, version, summary, services}`，按 run 钉住。 | `docs/LOADOUT.md` §1 |
| **Generation** | 每次重装配 +1 的计数器；旧代实例继续服务在途 run，之后退休。 | `internal/servicegraph/engine.go:270`、`docs/LOADOUT.md` §4 |
| **Selection** | 随 run 记录的模型/provider 选择；按当前代的 provider 配置校验。 | `internal/modelcfg`、`internal/agent/runner.go:321` |
| **插件（Plugin）** | Harness 的扩展。今天：进程内 Go 值（`agentext.Plugin`）。规划：三种 kind 之一的 `plugin.yaml` 包。 | `internal/agentext/plugin.go:67` |
| **`plugin.yaml`** | 规划中的插件清单（id/version/kind/engines/provides/requires/permissions/entry/source）。 | 规划：`keel-plugin-core` spec |
| **in-process / sidecar / webview** | 三种规划中的插件 kind：编译进宿主 / 经 stdio JSON-RPC 的外部进程 / 宿主托管的 UI 包。 | 规划：`keel-plugin-core` spec |
| **`keel.plugin.*`** | 规划中的桥：webview 插件借它读自身状态、调自身工具、请求审批、订阅事件——仅此而已。 | 规划：`keel-plugin-core` spec |
| **创造模式（Creation mode）** | 规划中的内置插件 `keel.creation`：自然语言 → 生成 webview 插件 → 安装 → 启用 → 深链，带审批与回滚。 | 规划：`keel-plugin-core` spec |

## DSH 词汇对照（仅作参照）

| DSH 术语 | 在那里指什么 | 在这里如何映射 |
| --- | --- | --- |
| `apply(ctx)` | 插件入口，接收注入的服务 | `Plugin.Start(ctx, *PluginEnv)` |
| inject / DI | 由运行时解析并注入的服务 | 服务图绑定 + `PluginEnv` |
| effect | 插件拥有的可逆副作用 | `Plugin.Effects`，按反序释放 |
| bundle | 一层补丁（配置 + 插件） | 规划中的启用清单；今天对应默认 loadout |
| profile | 有序的 bundle 栈 | loadout（不可变快照，按 run 钉住） |
| Cordis | TS 插件运行时 | **不移植**——`agentext` + `servicegraph` 以 Go 表达同一语义 |
