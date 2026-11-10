# 一次输入的解剖

本页跟随一条输入走完整个 Harness：从提交它的 HTTP 请求，到 claim 与 loop，再到客户端最终
渲染的已提交事件。请与[恢复与压缩](/zh/harness/recovery-and-compaction)一起阅读，后者覆盖
进程中途死亡时会改变什么。

## 1. 路径

```
① 提交     POST /v1/sessions/{id}/input  ──▶ user.message（edge 类），单事务
② claim    worker 轮询 ──▶ Kernel.Claim ──▶ Lease{owner, epoch, readWatermark}
③ 装载     runner.loadEvents ──▶ read watermark 落盘（durable 事实）
④ 决策     owedTurn? ──▶ 解析模型 ──▶ 补齐缺失工具结果
⑤ loop     loop.StartRun ──▶ 模型流 ⇄ 工具批次（每条 assistant 消息一批）
⑥ 提交     assistant.message(inputSeqs) · tool.result(toolCallId) · compaction
⑦ 结算     run.finished（摘要）──▶ Kernel.Finish ──▶ 未读输入重排队？
⑧ 投影     SSE `id: <seq>` 帧 ──▶ chat-surface ──▶ ConversationView
```

## 2. ① 提交：先提交，再谈其他

客户端发送 `{text, requestId?, delivery?, selection?, attachments?}`
（`internal/httpapi/input_commit.go:26`）。输入端点与重试端点汇入同一条提交路径
`commitUserInput`（`internal/httpapi/input_commit.go:58`），因此载荷形状、外部会话预检与
内核提交不会在两者之间漂移。

- 提交是 **edge 类**：不携带租约围栏。内核校验 tenant/session/kind/幂等键、分配 `seq`，并
  **在同一事务中把 run 入队**——输入绝不会可见而没有 run 等待它
  （`internal/httpapi/input_commit.go:58`、`docs/KERNEL.md` §2）。
- `requestId` 是 durable 幂等键（`KeyRequest`）：同一请求重投返回同一 submission 与 seq，
  绝不会产生第二条消息（`internal/kernel/validate.go:37`）。
- `delivery` 取 `queue`（默认）或 `guide`。`guide` 意为「注入运行中的 turn」；若没有活跃
  attempt，输入端点把它降级为 `queue`，**且降级在已提交事件里可见**——日志从不说谎
  （`docs/KERNEL.md` §2）。

## 3. ② claim：run 有了主人

worker 轮询可领取的 run，每个 claim 在自己的 goroutine 中执行，由并发信号量设界
（`internal/agent/worker.go:170`）。`Kernel.Claim` 返回 `Lease`：作用域、fencing 令牌
`owner@epoch`、到期时间、是否有**未读输入**、上一 attempt 的 **read watermark**，以及本次
claim 是否接管了过期租约（`internal/kernel/kernel.go:123`、`internal/kernel/lease.go:16`）。

claim 是「恰好一个写者」成立的根源：attempt 的每次提交都携带令牌，内核以
`stale-epoch`/`lease-lost` 拒绝过期 owner（`internal/kernel/kernel.go:355`）。worker 每
`ttl/3` 续租，一旦续租证明 claim 已丢立即取消 attempt，被取代的 run 无法继续写入
（`internal/agent/worker.go:327`）。

## 4. ③–④ 装载与决策

`runner.RunAttempt` 以严格顺序完成飞行前检查
（`internal/agent/runner.go:242`、`:280`）：

1. 逐页**装载**已提交日志（`internal/agent/runner.go:911`）；
2. **记录 read watermark**——`Reads.RecordRead` 在其他一切之前先持久化装载水位，因此无论
   接下来发生什么（模型失败、被杀、排水），继任者都能确切知道本次 attempt 没读到哪些输入
   （`internal/agent/runner.go:295`）；
3. **解码**已提交事件为转录；不可读的载荷跳过并留诊断，而不是让 run 失败
   （`internal/agent/messages.go:30`、`internal/agent/runner.go:301`）；
4. **判断是否欠一次模型调用**（`owedTurn`，`internal/agent/runner.go:683`）：只有当没有
   *完成态* assistant 消息引用该输入时，它才未被回答（见 §6）；最新 assistant 带工具调用
   意味着必须补齐其结果；终止失败（`stopReason: error|aborted`）不消费任何输入；
5. **解析执行身份**（provider/model/thinking/请求补丁）——来自 run 的 selection 事实，且在
   任何提交之前：无法执行的 selection 在此干净地失败本次 attempt
   （`internal/agent/runner.go:321`）；
6. **补齐缺失的工具结果**：上一个进程可能已提交工具调用但没有结果；执行是 at-least-once，
   内核保证每个 `toolCallId` 恰好一条结果（`internal/agent/runner.go:829`）；
7. **把未读输入挪到最后**：上一 attempt 运行期间到达的输入在日志里位于该 attempt 的答案
   *之前*；runner 把未读输入移到转录末尾，让模型把它当作自己的 prompt 而不是「已答过的
   内容」（`internal/agent/runner.go:360`、`:888`）。

## 5. ⑤ loop 运行

`loop.StartRun` 驱动 PI 形状的 loop：每回合一次模型请求，每条 assistant 消息一个工具批次，
stop reason 决定回合收束（`internal/loop/loop.go`）。Harness 在其外包裹三件事：

- **压缩**：当 loadout 的 compaction 服务提供设置时启用
  （`internal/agent/runner.go:489`、`:467`）；
- **guide 注入**：loop 在首个请求前、每个完成的回合之后（所有 sibling 工具结果已提交）以及
  `PrepareNextTurn` 之后轮询 steering 供给（`internal/agent/runner.go:447`）；
- **请求边界快照**：`PrepareRequest` 钉住每个请求实际呈现的内容，消费事实按请求顺序推导，
  绝不通过读取实时状态——loop 会借助事件缓冲跑到前面
  （`internal/agent/runner.go:455`）。

## 6. ⑥ 提交：账本里落了什么

loop 产出的每条事件都通过 `Committer.Commit` 携带 run 的 fencing 令牌提交
（`internal/agent/runner.go:953`）：

| 事件 | 幂等键 | 载荷要点 | 出处 |
| --- | --- | --- | --- |
| `assistant.message` | `generationId`（`u<promptSeq>-a<ordinal>`） | pi `AssistantMessage` + **`inputSeqs`** + usage/stopReason | `internal/agent/runner.go:562`、`:651` |
| `tool.result` | `toolCallId` | 结果 content/details、`isError` | `internal/agent/runner.go:943` |
| `compaction` | — | `summary`、`firstKept`、`summarized`、`timestamp` | `internal/agent/runner.go:478` |
| `run.finished` | — | attempt 摘要（见下） | `internal/agent/worker.go:391` |

`inputSeqs` 是整个设计的承重事实：`assistant.message.payload.inputSeqs` 列出该请求呈现给
模型的已提交 `user.message` 序列号。「未答输入」因此从水位启发式变成日志中的事实——继任者
能区分 queue 输入与被消费的 guide 输入，assistant 提交与结算之间崩溃也不会重复回答
（`internal/agent/runner.go:707`、`docs/KERNEL.md` §2）。

`generationId` 从已提交日志推导，因此重试的 attempt 推导出相同 id，内核去重返回已提交的
答案而非追加第二条（`internal/agent/runner.go:634`、`internal/kernel/validate.go:36`）。

### 一次带工具回合的事件序列

```
seq  kind                payload（节选）
1    user.message        {text: "看看工作区", requestId: "req_1", delivery: "queue"}
2    assistant.message   {content: [{type:"toolCall", id:"tc_1", name:"workspace.list"}],
                          inputSeqs: [1], stopReason: "tool_use"}          ← 提交 seq 1 的消费事实
3    tool.result         {toolCallId: "tc_1", content: [{type:"text", text:"src/ web/ docs/"}]}
4    assistant.message   {content: [{type:"text", text:"…"}], inputSeqs: [1], stopReason: "end_turn"}
5    run.finished        {status:"done", turns: 2, tokens:{…}, toolTimings: [{tool:"workspace.list", durationMs: 90}],
                          context:{messages: 4, tokens: 812}, stopReason:"end_turn", durationMs: 2140}
```

若在 seq 2–4 运行期间提交了一条 `guide` 输入，它会以独立的 `user.message` 出现（比如 seq 3），
并由*下一个*在 `inputSeqs` 中引用它的 assistant 消费；转录为每条被消费的 guide 输入新开
work segment，UI 因此可以展示它（`packages/chat-surface/src/projection.ts`）。

## 7. ⑦ 结算

结算前，worker 先提交 `run.finished`——attempt 自己的账目：status、turns、tokens、工具耗时、
最终上下文大小、stop reason、错误、provider/model 与耗时
（`internal/agent/runner.go:146`、`internal/agent/worker.go:388`）。它是记账而非消息：
转录解码器会跳过它（`internal/kernel/validate.go:31`）。

随后 `Kernel.Finish` 在同一租约下结算 run（`internal/agent/worker.go:399`）：

- **cursor** 与 **read watermark** 随结算提交；水位之上的输入会让会话重新入队——这正是
  运行中途到达的消息会被回答而不是被搁置的原因（`internal/agent/runner.go:207`）；
- **取消**以终态结算而非重排队（`Cancelled: true`）；
- 钉住的 **loadout** 写入 run 行，供审计（`internal/agent/worker.go:414`、
  `docs/LOADOUT.md` §5）。

失败策略：失败的 attempt 最多重试 `MaxAttempts`（默认 3，`internal/agent/worker.go:163`）；
provider 失败由 loop 以终止消息 `stopReason: error` 交付，worker 将其转为失败结算，使输入
被重试、run 行记录原因（`internal/agent/worker.go:361`）。

## 8. ⑧ 投影

客户端视图只由已提交事件构建。SSE 端点以 `id: <seq>` 发出它们；partials 以无 id 的帧旁路
传输（`internal/httpapi/server.go:628`，见[流式与 partial](/zh/harness/streaming-and-partial)）。
`@keel/chat-surface` 解析载荷，投影为 turns/work segments/queue/decisions，并以
`run.started`/`run.finished` 作为回合边界（`packages/chat-surface/src/projection.ts`）。

两个客户端可见的事实直接来自日志而非服务端运行时状态：

- **就绪性**——队列只能在 `run.finished` 边界排空，assistant 消息到达不算
  （`packages/chat-surface/src/projection.ts`）；
- **降级**——若 `guide` 已提交但没有活跃 turn 消费它，事件会写 `delivery: queue`，UI 把该项
  原位放回队列（`docs/CHAT-SURFACE.md` §5）。
