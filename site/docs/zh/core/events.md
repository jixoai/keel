# 事件与协议

Core 的 durable 契约是**事件账本**。其余一切——转录、checkpoint、SSE 帧、Harness UI——都是它的投影。
本页列出事件种类、提交如何分类与幂等、输入的投递模式含义，以及 SSE 流的保证。

## 1. 事件种类（全部词汇）

注册表在 `internal/kernel/validate.go:46`：**25 个已注册 kind**（下表中同族且共享形状的 kind 合并成
一行）。未注册的 kind 不能进入账本；字符串值与 `internal/store` 逐字对齐并由测试钉住；edge 白名单恰好是
edge-scoped 条目。

| kind | 类别 | 幂等键 | 当前是否发射 | 语义 |
|---|---|---|---|---|
| `user.message` | edge-scoped | `requestId` | 是（客户端输入） | 客户端唯一可写的 kind；`payload.delivery: queue\|guide` |
| `assistant.message` | owner-scoped | `generationId` | 是（loop） | 模型回答；`payload.inputSeqs` 记录本次请求呈现给模型的输入 |
| `tool.result` | owner-scoped | `toolCallId` | 是（loop） | 工具结果，按 `toolCallId` 与调用配对 |
| `compaction` | owner-scoped | —（无） | 是（compaction 服务） | 上下文压缩事实 |
| `run.started` | owner-scoped | —（无） | **否——保留词汇** | fence 已实现，但当前无运行时发射（run 行本身已记录开始/尝试次数） |
| `run.failed` | owner-scoped | —（无） | **否——保留词汇** | 同上 |
| `run.finished` | owner-scoped | `run`（`KeyRun`） | 是（worker，每个 settle 的 attempt 一条） | attempt 摘要：status/turns/tokens/toolTimings/context/stopReason/error/durationMs |
| `external.event` | external-scoped | `requestId` | 是（外部网关） | Keel 对外部 backend 事实的兼容日志；只在 `state_owner=external` 的会话里合法 |
| `task.created` · `task.updated` · `task.attached` · `task.detached` · `task.closed` · `task.note` | owner-scoped（双车道） | `task` | 是（task-log 服务） | 任务卡的只追加历史；卡片与事件视图都是这些行的投影 |
| `fs.write` | owner-scoped | `fs` | 是（文件插件） | 一次成功的工作区文件写入 |
| `fs.capture` | owner-scoped（双车道） | `capture` | 是（文件插件 / harness 后台任务） | 一次捕获结算；后台任务活过 run，改走 system 车道并携带 `payload.jobId` |
| `fs.capture_failed` | system-scoped | `capture-failed` | 是（文件插件） | 捕获失败作为独立事实 |
| `config.rotated` | system-scoped | `config` | **否——仅有词汇** | `config.toml` 版本的账本锚点；轮转目前是库 API（`docs/ASSEMBLY.md:205`） |
| `placement.joined` | system-scoped | `placement` | **否——仅有词汇** | 会话的组归属事实；placement 包把归属放在行里而非事件里 |
| `placement.left` | system-scoped | `placement` | **否——仅有词汇** | 离开组 |
| `supervision.escalated` | system-scoped | `supervision` | 是（supervisor） | supervisor 对停滞/被弃 run 的升级 |
| `guardrail.tripped` | system-scoped | `guardrail` | 是（agent guardrails） | 护栏拒绝或中止了一次 attempt |
| `resume.attempt` | system-scoped | `resume` | 是（supervisor） | 对被恢复 attempt 的续跑 |
| `notification.ack` | system-scoped | `notif-ack` | 是（notify） | 某个消费者消费了通知 |
| `notification.push_sent` | system-scoped | `notif-push` | 是（notify） | 通知的唯一原生/页内推送兜底 |

三点源码明确强调（`internal/kernel/validate.go:34`）：

- `run.started` / `run.failed` 是**保留**的：词汇与 fence 都在，但没有运行时发射它们。不要把它们的
  存在读成「loop 会发生命周期事件」。
- `run.finished` 不同：worker 每个 settle 的 attempt 发一次，因为客户端需要 turns/tokens/stopReason，
  而不该从转录里反推。它是记账，不是消息——转录解码会跳过它（`internal/kernel/kernel.go:87`）。
- **owner-scoped 不等于「一定携带租约写入」**。注册表里标了 `system: true` 的 kind（全部 `task.*` 与
  `fs.capture`）在 run 拥有该事实时是 owner-scoped，也可以在无 run 拥有时走 system 车道——初始任务卡
  先于任何 run 存在，而捕获任务会活过启动它的 attempt（`internal/kernel/validate.go:56`）。

## 2. 提交分类：谁携带 fence

`internal/kernel/kernel.go:292` 定义四个类别；提交的类别由 kind 决定，不由调用方决定。

| 类别 | kind | fence（lease token） | 校验对象 |
|---|---|---|---|
| `owner-scoped` | `assistant.message`、`tool.result`、`compaction`、`run.*`、`fs.write`，以及 run 拥有事实时的双车道 kind | **必须携带** | 会话当前 owner + epoch |
| `edge-scoped` | `user.message` | **必须不带** | 租户/会话存在性、edge 白名单、幂等 |
| `external-scoped` | `external.event` | 必须不带 | 会话必须标记 `state_owner=external` 且未关闭 |
| `system-scoped` | `fs.capture_failed`、`config.rotated`、`placement.*`、`supervision.escalated`、`guardrail.tripped`、`resume.attempt`、`notification.*`，以及无 run 拥有事实时的双车道 kind | 必须不带——提交必须置 `CommitRequest.System` | kind 必须是 `ClassSystem` 或声明了 `system: true`；客户端永不可写 |

最后这一类的作用，是让「harness 事实」不会变成「伪造租约」：system-scoped 提交只接受显式的 system
车道，因此 harness 驱动的事实（失败的捕获、placement 变更、通知 ack）从不需要伪造 owner token
（`internal/kernel/kernel.go:306`）。客户端根本触达不到该车道：edge 端点恰好只收 `ClassEdge` 条目。

edge 白名单恰好是 `ClassEdge` 条目（`internal/kernel/validate.go:102` 的 `EdgeKinds()`），因此
`POST /v1/sessions/{id}/events` 只收 `user.message`，其他一律 400；跨租户或不存在的会话 404。
handoff drain 期间 edge 输入照常落账（这就是「换进程时用户还能打字」的机制）——由下一个持有租约的
进程消费（`docs/SERVICE-GRAPH.md:204`）。

外部会话是对称的另一面：对它的原生 edge 写入被 409 拒绝（`ErrExternalSession`），
`/v1/sessions/{id}/run` 也拒绝为它入队——内核的入队谓词按 `state_owner <> 'external'` fail-closed
过滤，因此即使存在脏 run 行也永不被领取。

## 3. 幂等：一个键，一个作用域

一次提交恰好携带一个幂等键，由 kind 决定它属于哪个空间（`internal/kernel/validate.go:188` 的 `keyFor`）：

- `requestId` — `user.message`、`external.event`
- `generationId` — `assistant.message`
- `toolCallId` — `tool.result`
- `run` — `run.finished`：用 attempt id，因此被重试的 attempt 是新事实，而同一次 attempt 的提交重试
  仍去重（`internal/kernel/validate.go:53`）
- `task` — 六个 `task.*`；`fs` — `fs.write`；`capture` / `capture-failed` — `fs.capture` /
  `fs.capture_failed`；`config` / `placement` / `supervision` / `guardrail` / `resume` / `notif-ack` /
  `notif-push` — 对应的 system-scoped kind（`internal/kernel/kernel.go:234`）
- `compaction`、`run.started`、`run.failed` 不声明键，**必须不带**；传了就是错误，而不是被静默忽略。

唯一性作用域是 `(tenant_id, session_id, dedupe_key)`——一个部分唯一索引
（`internal/store/schema.sql:117`）。推论：

- **重试同一输入是安全的**。带同一 `requestId` 重投 `POST /v1/sessions/{id}/input` 返回同一
  submission（实测响应：`{"created":true,"requestId":"…","seq":1,…}`）；作用域是会话，因此两个会话里
  的同一 `requestId` 是两次独立提交，绝不会返回别家会话的 submission。
- **工具副作用是 at-least-once，账本是 exactly-once**。执行层可能跑两次工具（崩溃恢复、handoff），
  但同一 dedupe key 每个会话只能插入一次，所以最终日志里每个 `toolCallId` 恰好一条结果。
  replay-safe ≠ exactly-once，框架不假装保证前者等于后者（`docs/ARCHITECTURE.md:46`）。
- 载荷比较忽略格式：键顺序不同、空白不同仍是同一个请求（`samePayload`/`canonicalJSON`，
  `internal/kernel/validate.go:637`）。

## 4. 投递模式：`queue` 与 `guide`

`user.message.payload.delivery` 是 edge（写意图）与 loop（消费意图）之间的 durable 契约
（`internal/kernel/validate.go:378`）：

- **`queue`**（缺省；字段缺失即 `queue`，因此旧客户端与旧 dump 继续可用）：run 结束后、下一次
  claim 时才进入模型上下文。
- **`guide`**：当前 turn 内注入，**在所有 sibling tool results 提交之后**，一次一条，不打断模型流。

没有活跃 attempt 时，HTTP 边界把 `guide` 降级为 `queue`——并且写入的事件就写着
`delivery: queue`，客户端能看见实际发生了什么并调整 UI。

注入点刻意不是提交时刻，而是**请求边界**：注入的 seq 由 loop 的 `PrepareRequest` 快照归属，并并入
随后 `assistant.message.inputSeqs`。loop 会通过事件缓冲跑到 runner 前面，若在提交时读实时状态，会把
「下一次请求才注入的 guide」记到上一次回答上（`docs/KERNEL.md:54`）。

## 5. 回答消费了什么：`inputSeqs`

`assistant.message.payload.inputSeqs` = 这次请求呈现给模型的 `user.message` seq 集合。它是日志自证的
「这条回答看见了什么」，内核用它回答一个更难的问题：**还有输入没被回答吗？**（`docs/KERNEL.md:58`）

- 输入「未被回答」＝没有任何**完成态** assistant（`end_turn`/`tool_use`/…）的 `inputSeqs` 引用它。
  终止消息若 `stopReason: error|aborted` 则不算回答：失败会重试，取消不重跑。
- 终止失败的 assistant 消息**不进模型上下文**——重放空/错误 assistant 会污染下一次请求。当最新
  assistant 是终止失败时 `owedTurn` 为真，因此「工具轮完成 + 下一轮 provider 失败」不会被误判为
  「无事可做」而落成 `done`。
- `read_watermark` 保留为诊断与**老日志/升级前缀**的判定：第一条带 `inputSeqs` 的 assistant 之前按
  水位判定，从此之后由 `inputSeqs` 决定。`runs.pending_input` 与 `runs.read_watermark` 能跨接管存活，
  正是为了让 drain/kill/退出不吞掉未读输入
  （`internal/kernel/schema/0004-pending-input.sql:25`、`docs/HANDOFF.md:150`）。

## 6. 结算：状态策略

worker 在 settle 之前用同一 lease 提交 `run.finished`；摘要写入失败不改变 run 终态。状态策略是
`finishStatus`（`internal/kernel/validate.go:314`）：

| 情形 | 结果 run 状态 |
|---|---|
| 取消（`FinishRequest.Cancelled`） | `failed`——终态。输入在日志里保持未答；后续输入启动的新 run 会回答它。重新入队会让取消变成空操作。 |
| attempt 失败，尝试预算未耗尽 | `pending`——重试 |
| attempt 失败，尝试预算耗尽 | `failed` |
| 成功，但期间有新输入到达 | `pending`（`moreInput`） |
| 成功，无新输入 | `done` |

provider 失败按 PI 语义是 `stopReason: error` 的终止消息；runner 把它归类为 attempt failed，worker
以失败 settle，重试策略照常（`docs/KERNEL.md:74`）。

## 7. 读日志：快照、SSE、partial

**快照与续传**。`GET /v1/sessions/{id}/events?after=<seq>&limit=<n>` 返回已提交事件与游标。
`GET /v1/sessions/{id}/events/stream?after=<seq>` 是 SSE 流：帧的 `id:` 字段就是事件 `seq`，因此
SSE 客户端用 `Last-Event-ID`（或 `after`）续传，绝不丢已提交事件。订阅者由 `LISTEN/NOTIFY` 唤醒而非
轮询（`internal/store/schema.sql:313`），心跳是兜底。

**partial 是旁路，不属于账本**。开启 `partials=1` 时，token 级帧以 `event: partial` 推送且**不带**
`id:`；它们从不写入事件表、从不移动 `Last-Event-ID`、断线后不重放。partial 期间断线的客户端重连后
只收到已提交事件；完整消息会带着自己的 `id` 到达。

**drain 帧**。runtime 排水（SIGTERM 或 handoff）时，每条打开中的 SSE 连接先收完未读事件，然后收到：

```
event: drain
data: {"retry_after_ms":3000}
```

客户端应按提示重连到新 owner，并用 `Last-Event-ID` 续传；新流首帧 = 游标+1。

**一条实测流**。对 fake provider 提交一次输入，恰好产生如下序列（实际抓取，`…` 为省略）：

```json
{"events":[
  {"seq":1,"kind":"user.message","payload":{"text":"hello keel","requestId":"9e5088a0-…"}},
  {"seq":2,"kind":"assistant.message","payload":{"role":"assistant","content":[{"text":"…","type":"text"}],"inputSeqs":[1],"stopReason":"end_turn"}},
  {"seq":3,"kind":"run.finished","payload":{"status":"done","turns":1,"provider":"fake","model":"fake","stopReason":"end_turn","durationMs":24}}
]}
```

注意账本里**没有**什么：没有 `run.started`，没有 partial 帧，没有租约/epoch 记账。那些是 run 行与进程
的事务；账本记录后继进程必须能重放的事实。

## 8. 校验：内核拒绝什么

内核在提交时校验「重放所依赖」的载荷形状，因为坏载荷一旦进日志就是永久的
（`internal/kernel/validate.go:260`）：

- `user.message`：`text`/`requestId` 若存在必须是字符串；`delivery` 必须是 `queue`/`guide`；
  `selection` 必须是 JSON 对象；`retryOf` 必须是非负整数。未知字段允许存在，以便 edge 携带元数据而
  无需改内核。
- `assistant.message`：每个 `toolCall` 块必须有 `id` 与 `name`，且同一消息内 `id` 不得重复——否则
  结果无法附着到唯一的调用（`internal/kernel/validate.go:260`）。
- `tool.result`：`payload.toolCallId` 必须与请求的 `toolCallId` 相等，且必须指向已提交的对应
  assistant 调用（`internal/kernel/validate.go:284`）。
- `run.finished`：`status` 必须是 `done`/`failed`/`cancelled` 之一；计数器若存在必须是非负整数；
  `toolTimings` 必须是对象数组（不得为 null）（`internal/kernel/validate.go:417`）。
- `external.event`：载荷必须带 `backend` 与 `externalRef`；`stateOwner` 若存在必须为 `external`
  （`internal/kernel/validate.go:569`）。

## 9. 问服务端，不要猜

客户端的线契约是版本化的：Harness API 以 `/v1` 前缀且只增不改（`internal/harnessapi/api.go:53`）。
客户端不得从二进制版本推断特性；它问 `GET /v1/capabilities` 并对 `false` 降级。该文档由
`internal/harnessapi/api.go:556` 的 feature 映射构建；下面是有省略的形态——依赖部署接线的块
（`plugins`、`pluginRevision`、`mcpApps`、`configFile`）只在其 seam 接线时出现，且每个 feature flag
都跟随它真正需要的 seam：

```json
{"apiVersion":"v1","serviceVersion":"keel-agent/0.1.0",
 "features":{"decisions":true,"hooks":true,"partials":true,"plugins":true,
             "plugins.manage":true,"plugins.audit":true,"plugins.approvals":true,"plugins.bridge":true,
             "sessions.cancel":true,"sessions.list":true,
             "session.search":true,"session.export":true,"session.deliverables":true,
             "session.filePreview":true,"task.panel":true,"team.tasks":true,
             "settings.models":true,"settings.secrets":false,"settings.skills":true,
             "skills.completion":true,"subagents":true,"triggers":true,
             "mcp.apps":true,"mcp.apps.sandboxOrigin":false,"mcp.apps.context":true,
             "mcp.apps.tools":true,"mcp.apps.resources":true,"mcp.apps.message":true,
             "mcp.apps.links":true,"mcp.events":false},
 "limits":{"sessionsPageDefault":50,"sessionsPageMax":200,"subagentsPageMax":200,
           "teamTasksPageDefault":50,"teamTasksPageMax":200,
           "searchPageDefault":20,"searchPageMax":100,
           "exportPageDefault":200,"exportPageMax":500,"exportPageBytes":524288,
           "deliverablesDefault":100,"deliverablesMax":500},
 "hooks":["subagent.started","subagent.finished","trigger.before","trigger.after",
          "turn.started","turn.finished","decision.opened","decision.answered","mcp.surface.changed"],
 "plugins":["keel.example"]}
```

端点参考在下一页：[Harness API 参考](/zh/core/agentapi)。
