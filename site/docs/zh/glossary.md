---
# 术语表只有一页：单条目的侧栏轨道只会留下大片空白，因此本页不显示侧栏。
sidebar: false
---

# 术语表

> 本项目的命名规则：产品层称 **Harness**；产品实体是 **Bot**、**TeamBot**、**Account**、
> **Role**、**Profile**。旧的「Agent」与 `TeamAgent` 已删除（alpha 零兼容），不是别名。
> 协议名 **AgentAPI** 与 **AgentBackend** 是契约，予以保留——改名会破坏客户端与适配器。
> PI 技术包（`agentcore`、`agentloop`、`agenttool`、`internal/agent`）同样保留原名。本站
> 全部页面遵循此规则，并由扫描脚本（`scripts/naming-scan.sh`）检查。

## 产品词汇

| 术语 | 定义 | 一手出处 |
| --- | --- | --- |
| **Keel** | 本项目：无状态、多租户的智能体运行时底座。 | `README.md` |
| **Core** | 契约内核 + 事件账本 + 服务图 + loadout + 存储 + handoff。不可替换的部分。 | `docs/SERVICE-GRAPH.md`、`docs/KERNEL.md` |
| **Harness** | Core 之上的产品层：会话、回合、工具、审批、插件、触发器、子代理与客户端界面。进程即 `keel serve`。 | `internal/harnessext/doc.go`、`docs/HARNESS.md` |
| **Account** | 登录与权限主体；human / bot / service 共用一套模型。 | `accounts` 表、`/v1/accounts`、`internal/botteam` |
| **Role** | 身份与灵魂：名字、Instruction、语气、默认行为；**不授予**权限。 | `roles` 表、`/v1/roles` |
| **Profile** | `Account + Role` 的绑定，Bot 通过它获得人格与权限上限。 | `profiles` 表、`/v1/profiles` |
| **Bot** | 可运行的 AI 员工：`Profile + ModelKind + Harness`；是权限主体，拥有 Session。 | `bots` 表、`/v1/bots`、`keel bot` |
| **TeamBot** | Mission + Bot 名单（Roster）；是资源，不是权限主体，不拥有 Session/Account/Profile。 | `team_bots` 表、`/v1/team-bots`、`keel team-bot` |
| **AgentAPI** | `/v1` 产品 API 契约（别名：Harness API）。名称保留；web UI 与 TUI 消费的 wire 契约。 | `internal/harnessapi/api.go` |
| **AgentBackend** | 外部智能体运行时（例如 DSH）作为 external backend 被驱动的协议。名称保留。 | `internal/gateway/protocol.go`、`docs/DSH-REFERENCE.md` |
| **DSH** | DeepSeek Harness——插件模型的外部参照，也是 external-agent 目标。是参照，不是本项目的事实源。 | `docs/DSH-REFERENCE.md` |

## 会话与运行词汇

| 术语 | 定义 | 出处 |
| --- | --- | --- |
| **账本（Ledger）** | 只追加的事件日志；唯一真值。投影与 checkpoint 皆由它派生。 | `docs/KERNEL.md` |
| **事件 kind** | durable 事实的闭集词汇——**共 25 个已注册 kind**：`user.message`、`assistant.message`、`tool.result`、`compaction`、`run.started`/`run.failed`（保留）、`run.finished`（携带 `run` 键）、`external.event`、六个 `task.*`、`fs.write`/`fs.capture`/`fs.capture_failed`，以及 system 类 `config.rotated`、`placement.joined`/`placement.left`、`supervision.escalated`、`guardrail.tripped`、`resume.attempt`、`notification.ack`/`notification.push_sent`。 | `internal/kernel/validate.go:46` |
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
| **Decision（审批/问句）** | 审批/问句原语：durable 行、幂等作答、有界超时。不是内核事件 kind。 | `internal/harnessext/decisions.go:233` |
| **触发器（Trigger）** | 定时或 webhook 启动的回合（`interval`/`cron`/`webhook`），带幂等键与审计。 | `internal/harnessext/triggers.go:30` |
| **子代理（Sub-agent）** | 由工具或 API 派生的子会话，带预算与结构化结果。 | `internal/harnessext/subagent.go:22` |

## 装配词汇

| 术语 | 定义 | 出处 |
| --- | --- | --- |
| **服务图（Service graph）** | 一方服务的装配：按 capability 解析依赖、有序激活、反序拆卸。 | `internal/servicegraph/lifecycle.go:21` |
| **Loadout** | 服务集合的不可变快照 `{id, revision, version, summary, services}`，按 run 钉住。 | `docs/LOADOUT.md` §1 |
| **Generation** | 每次重装配 +1 的计数器；旧代实例继续服务在途 run，之后退休。 | `internal/servicegraph/engine.go:270`、`docs/LOADOUT.md` §4 |
| **Selection** | 随 run 记录的模型/provider 选择；按当前代的 provider 配置校验。 | `internal/modelcfg`、`internal/agent/runner.go:321` |
| **装配行（Assembly / row）** | 服务图之上的配置栈：base → mode → preset → `config.toml`，按 row id（`<plugin-id>#<row>`）覆盖，last write wins；只有 base 可以新增 row。 | `docs/ASSEMBLY.md:41`、`docs/ASSEMBLY.md:53` |
| **插件（Plugin）** | Harness 的扩展：三种 kind（`in-process`/`sidecar`/`webview`）之一的 `plugin.yaml` 包，安装进受管根目录并由插件管理器启用；生命周期状态 `installed/enabled/disabled/failed/invalid`（`uninstalled` 只留在审计轨迹）。 | `internal/pluginpkg/manifest.go:142`、`internal/harnessext/plugin_manager.go:31` |
| **`plugin.yaml`** | 插件包清单：id/version/kind/layer/description/engines/provides/contributes/extensibility/requires/permissions/entry/source，加载时严格校验（未知字段是错误，不是 no-op）。 | `internal/pluginpkg/manifest.go:142` |
| **in-process / sidecar / webview** | 三种插件 kind，均已可加载：编译进宿主 / 以网关 stdio 行级 JSON-RPC 说话的 `tool_provider` 外部进程 / 在产品同源 `/plugins/<id>/` 下、仅对 enabled 插件提供的 UI 包。 | `internal/harnessext/plugin_sidecar.go:22`、`cmd/keel/plugins_ui.go:17` |
| **`keel.plugin.*`** | webview 插件借用它的桥：读自身状态、调自身工具、请求审批、订阅事件——仅此而已（`state`/`call`/`approve`/`subscribe`，另加主题令牌）。 | `internal/harnessapi/plugin_bridge.go:75` |
| **创造模式（Creation mode）** | 内置插件 `keel.creation`，默认启用：自然语言 → 生成 webview 插件 → 安装 → 启用 → 深链，带审批与回滚。六个动作——`catalog`/`scaffold`/`validate`/`install`/`list`/`remove`；G8 端到端验收仍 Deferred，不得写成已完成。 | `internal/harnessext/creation.go:91`、`internal/pluginpkg/builtin/keel.creation.yaml:1` |

## DSH 词汇对照（仅作参照）

| DSH 术语 | 在那里指什么 | 在这里如何映射 |
| --- | --- | --- |
| `apply(ctx)` | 插件入口，接收注入的服务 | `Plugin.Start(ctx, *PluginEnv)` |
| inject / DI | 由运行时解析并注入的服务 | 服务图绑定 + `PluginEnv` |
| effect | 插件拥有的可逆副作用 | `Plugin.Effects`，按反序释放 |
| bundle | 一层补丁（配置 + 插件） | 装配行：`<plugin-id>#<row>` 的 impl+params 覆盖，last write wins（`internal/assembly`、`docs/ASSEMBLY.md:53`） |
| profile | 有序的 bundle 栈 | loadout（不可变快照，按 run 钉住） |
| Cordis | TS 插件运行时 | **不移植**——`harnessext` + `servicegraph` 以 Go 表达同一语义 |
