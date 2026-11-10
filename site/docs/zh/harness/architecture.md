# Harness 架构

本页描述每个 Harness 能力住在哪里、各部件如何在单一进程内组装，以及哪些边界是承载性的。
阅读前假定你已了解 Core 概念（账本、内核动词、服务图、loadout）——见 Core 主线。

## 1. 分层

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ 客户端                                                                        │
│  web UI（NSH web 构建）· TUI（NSH tui target）· 任意 HTTP 客户端              │
│  共享投影：@keel/chat-surface（零 DOM 状态机）                                │
└───────────────────────────────┬──────────────────────────────────────────────┘
                                │ HTTP + SSE（同源；结构上无需 CORS）
┌───────────────────────────────▼──────────────────────────────────────────────┐
│ HTTP 边缘 + AgentAPI（internal/httpapi、internal/agentapi）                   │
│  边缘：tenants/sessions/input/retry/events/stream/files/runtime drain        │
│  产品：capabilities、会话列表、cancel、decisions、plugins/triggers/          │
│        subagents、settings 面                                                │
└───────────────────────────────┬──────────────────────────────────────────────┘
                                │ 接缝：kernel.Committer/Reader/LeaseManager、
                                │ agentext 原语、服务绑定
┌───────────────────────────────▼──────────────────────────────────────────────┐
│ Harness 产品原语（internal/agentext）                                         │
│  插件运行时 · hook 总线 · decision broker · 触发器 · 子代理 · run 控制 ·      │
│  partial hub                                                                  │
├──────────────────────────────────────────────────────────────────────────────┤
│ 宿主装配（internal/host）+ agent loop 集成（internal/agent）                  │
│  system prompt 分节 · 技能 · 工具 registry · MCP 工具 · 遥测                 │
├──────────────────────────────────────────────────────────────────────────────┤
│ 服务图（internal/servicegraph + internal/loadout）                            │
│  描述符、依赖解析、生命周期、generation、per-run 租约                         │
├──────────────────────────────────────────────────────────────────────────────┤
│ 契约内核 + 账本（internal/kernel）· loop（internal/loop）·                    │
│ 内容模型（internal/agentcore）· 工具执行（internal/agenttool）                │
│  事件 kind、幂等键、租约/fencing、run 行、store 投影                          │
└───────────────────────────────┬──────────────────────────────────────────────┘
                                │
                       PostgreSQL（事件、run 行、tasks、blobs）+ 本地 CAS
```

顺序是硬约束：每一层只能使用其下方的层。面向内核的规则由机器把守——产品包绝不允许成为
内核包的依赖（`internal/architecture/boundary_test.go`）。

## 2. 宿主进程与服务图

`keel serve` 是唯一了解所有层的代码。它按固定顺序启动，而这个顺序**就是**安全契约
（`cmd/keel/serve.go:75`）：

1. 打开 store，应用内核迁移；
2. 构造接缝集合（`Committer`/`Reader`/`Leases`/`External`、只读 run 镜像、租户配额、审计
   sink）；
3. 从 flag/环境构造默认租户文档（`cmd/keel/serve.go:181`），含存储的模型设置覆盖；
4. `servicehost.Boot` → `loadout.FromTenantConfig` → `Engine.Apply` → 断言运行路径上的服务
   全部 `ACTIVE`（`cmd/keel/serve.go:213`）；
5. 运行时就绪后才启动 SSE hub——启动失败不能占住连接池（`cmd/keel/serve.go:275`）；
6. 构建产品层（decisions、triggers、subagents、plugins），并把 AgentAPI 挂在 HTTP 边缘旁
   （`cmd/keel/serve.go:282`、`:294`）。

默认 loadout 是权威装配（`internal/loadout/loadout.go:63`；见 `docs/LOADOUT.md` §2）：

| 服务 | provides | requires | optional | 默认 |
| --- | --- | --- | --- | --- |
| `store` | `ctx.store` | — | — | 恒开 |
| `llm` | `ctx.llm` | — | — | 恒开（空模型 = fake provider） |
| `prompt` | `ctx.prompt` | — | `ctx.mcp`、`ctx.sandbox`、`ctx.external-tools` | 恒开 |
| `mcp` | `ctx.mcp` | — | — | 配了 mcp.json 才开 |
| `sandbox` | `ctx.sandbox` | — | — | 从不开（无工厂；以降级诊断呈现） |
| `tools` | `ctx.tools` | `ctx.prompt` | — | 恒开（同一 host 的 registry 视图） |
| `pi-loop` | `ctx.loop` | `ctx.llm`、`ctx.tools` | `ctx.compaction` | 恒开 |
| `compaction` | `ctx.compaction` | `ctx.llm` | `ctx.store` | `compaction.enabled=true` |
| `session-projection` | `ctx.session-projection` | `ctx.store` | — | 恒开 |
| `telemetry` | `ctx.telemetry` | — | — | `telemetry.adapter` 非空 |
| `worker` | `ctx.worker` | store/loop/prompt/tools | `ctx.telemetry` | 进程级 `-no-worker` |
| `external-tools` | `ctx.external-tools` | — | — | `external.tools` 非空 |
| `external-agent` | `ctx.external-agent` | `ctx.store` | — | `external.backends` 非空 |

两个刻意的形态：`prompt` 与 `tools` 共享同一个 `host.Host` 对象（不存在第二份 registry 供
MCP/工具刷新分叉）；`sandbox` 不造空工厂——未实现的服务就是一个不存在的 optional 提供者，
依赖方按降级诊断处理，而不是半启动（`docs/LOADOUT.md` §2）。

### 生命周期与 generation

服务实例沿 `PENDING → LOADING → ACTIVE → RETIRING → UNLOADING → PENDING` 迁移；激活失败
落在 `FAILED`，可被后续 `Apply` 重试（`internal/servicegraph/lifecycle.go:21`）。每次
`Apply` 令 generation 恰好 +1；装配身份
`(name, version, provides, requires, optional, role)` 未变的服务**跨代共享**，不重建
（`docs/LOADOUT.md` §4）。

每个被 claim 的 run 会向引擎*获取*一份租约（`internal/servicegraph/engine.go:270`）并钉住
generation：即使宿主之后重装配服务，该 run 仍使用它开始时的 loop 实例。进入退休的
generation 不接新 run，待引用归零后按反序卸载；超过退休截止则强制卸载——其租约被标记为
broken，持有者必须以 `drain-expired` 诊断让 run 失败（`internal/agent/worker.go:242`、
`internal/servicegraph/engine.go:303`、`:650`）。宿主 ticker 周期调用 `CollectExpired`，
空闲进程也不会累积旧代（`docs/LOADOUT.md` §4）。

## 3. 插件宿主

插件宿主是 Harness 的扩展运行时。今天它是 `agentext.Runtime`：一个**进程内、声明式**契约。

```
agentext.Runtime
├─ assemble()：先卸载上一代装配 → generation+1 → 按依赖顺序激活
│    ├─ 跨插件工具重名 → fail-closed 诊断
│    ├─ requires 未满足 → PENDING，附 "waiting for: …"
│    └─ Start(ctx, env) → hooks Subscribe → triggers Register → 工具适配
├─ publishTools()：每个 active 插件的工具经 ToolSink 推入宿主 registry
│    └─ keel serve 中：sink = rt.RefreshProductTools     cmd/keel/serve.go:464
├─ Snapshot()：generation、插件状态、hooks 统计、工具名    plugin.go:505
└─ Close()：反序卸载全部，随后发布空工具集
```

出处：插件契约 `internal/agentext/plugin.go:67`；装配 `:209`；激活 `:317`；反序清理 `:387`；
卸载 `:417`；快照 `:505`。`keel serve` 当前装配两个内置插件：`keel.example`
（`internal/agentext/builtin.go:38`）与 `keel.subagents`（`internal/agentext/builtin.go:256`）。

今天这个宿主给你的能力：按依赖顺序激活、对每个声明的副作用（hook 订阅、触发器注册、
effects）做精确反序撤销、generation 计数、可观测状态与诊断，以及工具重名的 fail-closed
规则。它**没有**的：清单、安装、沙箱、权限与 UI 托管——那些是[插件](/zh/harness/plugins/)
部分描述的规划 `plugin.yaml` 层。在此之前，「插件」指的是编译进二进制的 Go 值。

## 4. 工具与审批

工具是模型触碰世界的唯一途径，因此这条路径很窄、可审计。

```
模型流（provider）
   │ assistant.message 携带 toolCalls
   ▼
internal/loop            回合结构，每条 assistant 消息一个工具批次
   ▼
internal/agenttool      批次执行（默认并行；声明则串行）、按 toolCallId 配对结果、结果裁剪
   ▼
agenttool.ToolRegistry   按名查找（宿主 registry：内置 + 技能 + MCP + 插件工具）
   ▼
tool.result 提交          内核与调用配对；重复结果按 toolCallId 去重
```

- registry 归宿主装配（`internal/host`）所有，`tools` 服务只是它的视图，不是副本
  （`docs/LOADOUT.md` §2）。
- 跨崩溃是 **at-least-once**：已提交调用但缺结果时，下一次 attempt 会重新执行；内核保证
  每个 `toolCallId` 恰好一条结果（`internal/agent/runner.go:829`、`:937`）。有副作用的工具
  必须自带幂等键（`docs/ARCHITECTURE.md` §3）。
- 工具结果可能很大；执行器会裁剪，转录保持有界（`internal/agenttool/utf8trim.go`）。

**审批**不是工具执行器的属性，而是产品层的 decision 原语
（`internal/agentext/decisions.go:233`）。需要人工回答的 run 打开一个 decision，以有界超时
等待（默认 5 分钟，`internal/agentext/decisions.go:162`），按答案继续或失败。Decision 是
tasks 记账表中的 durable 行，**绝不是新的内核事件 kind**——内核词汇表不变
（`internal/kernel/validate.go:36`、`internal/agentext/doc.go`）。见
[审批与问句](/zh/harness/approvals-and-questions)。

## 5. 投影

客户端从不直接读账本，也从不自行拼装状态：

```
已提交事件（GET /events、SSE /events/stream）
        │
        ▼
@keel/chat-surface   wire payload 解析      packages/chat-surface/src/events.ts
  │                  纯投影                 packages/chat-surface/src/projection.ts
  │                  门面 + 重连            packages/chat-surface/src/engine.ts
  ▼
ConversationView     turns、work segments、queue、decisions、run 摘要、
                     连接状态——冻结契约在
                     packages/chat-surface/src/types.ts
        │
        ▼
NSH 渲染器           web 屏 / TUI target（同一份 TSX）
```

投影刻意**不是**权威。它镜像日志，绝不杜撰事实：未读输入由
`assistant.message.inputSeqs` 推导（见[一次输入的解剖](/zh/harness/anatomy-of-a-turn)），
queue/guide 状态是事件驱动的，停止只有 `run.finished` 才算确认
（`packages/chat-surface/src/projection.ts`）。Partials 是证明这条规则的例外：token 增量
沿日志旁路传输，永不落账，已提交消息一到即被丢弃
（`internal/agentext/streamtee.go:24`、`packages/chat-surface/src/engine.ts`）。

同一份投影驱动两个界面，这也是 web UI 与 TUI 不会漂移的原因：`keel tui` 用同一份 TSX
对接同一个 AgentAPI（`docs/WEBUI.md`、`web/README.md`）。

## 6. 什么可替换，什么不可以

| 可替换（装配） | 不可替换（契约） |
| --- | --- |
| agent loop（须过 PI conformance） | 事件顺序、seq、一会话一写者 |
| prompt 组装 | 工具调用 ↔ 结果配对 |
| 工具注册与执行 | `requestId` / `generationId` / `toolCallId` 幂等 |
| LLM provider | 租约所有权与 fencing epoch |
| 会话读投影 | 租户隔离与配额执行 |
| 压缩、MCP、遥测、sandbox | 崩溃恢复真值 |
| 外部 AgentBackend / ToolProvider | 默认 loop 的 PI 行为契约 |

这张表是 Core 服务图规则的 Harness 版（`docs/SERVICE-GRAPH.md` §3）：*服务可以换，服务不能
改账本*。插件若想影响账本，就必须像所有人一样走内核的 `Commit`
（`internal/kernel/kernel.go:599`）。
