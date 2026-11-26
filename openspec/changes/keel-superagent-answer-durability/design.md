# Design: keel-superagent-answer-durability

## 1. 状态机与写序

一条答复的生命周期只有四个持久事实：

```
(1) Ticket.answers/pending: answer intent        <- UpdateWorkItem #1
(2) collab_attentions.status = answered          <- Host.AnswerAttention
(3) InputSink.Deliver 或 dispatch assignment     <- observed success (not durable)
(4) Ticket: pending removed + AnswerReceipt      <- UpdateWorkItem #2
```

- (1) 与「pending 输入」是同一个 WorkItem JSON 文档，一次 `UpdateWorkItem` 写入；
  包含 `optionId/answerText/answerActor`，所以 (2) 之前的任何崩溃都可用同一意图
  重放。
- (2) 幂等：同 option/text 重试返回同一事实；不同答复冲突。
- (3) 是外部效果。成功后最多重复一次（sink 以 `RequestID=attention:<id>` 去重），
  这是 at-least-once，不是恰好一次。
- (4) 与 receipt 同一次写；receipt 是「Worker 已收到」的持久证据。
- Channel 答复评论在 (4) 之前用幂等键 `attention:<id>:answer` 入 outbox；崩溃只会
  导致重试，不会丢评论。receipt.commentQueued 记录该行是否已存在，启动扫描补齐。

由此得到恢复规则：

| 持久状态 | 恢复动作 |
|---|---|
| intent + Attention open | 重试 completion：写 (2) 再投递 |
| intent + Attention answered + pending | 继续投递 |
| Attention answered + receipt | 成功（幂等） |
| Attention answered + 无 intent/receipt + 工单未关闭 | 从 Attention.Answer 重建 intent 并投递 |
| Attention answered + 无 intent/receipt + 工单已关闭/撤销 | `ErrAnswerNotDelivered`（显式失败） |
| 工单被停止 | `ErrWorkItemClosed`，不写任何东西 |

「answered」本身永远不是成功判据，只有 receipt 或刚刚成功的投递才是。

## 2. retry 与人工出口

`deliverPendingLocked` 每轮只处理 due 条目：

- 不可信输入 + blocked 工单：状态延后（不消耗 attempt，30s 再看），不 halt 后续条目，
  保证后面的 answer 能通过。
- 不 due：同权限类别内保序；若后面有人类授权条目（answer/trusted follow-up），
  不阻塞它。
- 投递错误：`pendingPermanentError` 只认 `ErrPendingNotDeliverable`、权限、无效请求、
  关闭/租户错误；其余（TurnStarter/DB/launcher/网络）无限重试，退避 1s→30s。
- 永久失败：`Failed=true` 保留整条 pending 行；Board 增加 `pendingCount`/
  `failedPendingCount` 并原样投影 pending；`RetryPending` 清 Failed/退避并立即尝试，
  `DismissPending` 删除并（答复类）写一条 `Dismissed` receipt，防止启动扫描复活。

## 3. 信任与状态

- capture 时持久化 `Trusted`（Policy verdict）与 `AllowDispatch`（trusted）。
- 无 live assignment 时只有 `PendingAnswer` 或 `AllowDispatch` 才能派工；不可信
  context 返回 `errPendingAwaitingLiveWorker`（临时、不 Failed）。
- blocked → active 只允许 answer / trusted follow-up；不可信 context 保持 blocked、
  Attention 仍 open。
- replied answer 走 `AnswerAttention`：always `AllowDispatch=true`（人类决策）。

## 4. 多副本

`Orchestrator.Run` 先取租约再轮询：

- PG：`pg_try_advisory_lock(hashtext("keel.superagent.writer|<tenant>"))`，锁挂在
  专用 pool 连接上，进程退出/连接断开自动释放；第二进程得到
  `ErrWriterLeaseHeld`，`cmd/keel` 在中断旧 worker 会话之前就失败退场。
- MemoryStore：进程内 lease（测试/演示）。
- Store 没有 lease 能力时 `Run` fail closed（记录错误并保持空闲）。
- 写入口仍由 `o.mu` 串行化；lease 解决的是跨进程。

## 5. 审计

`AuditAttentionAnswer="accepted"` 只在 receipt 落库后写；冲突（不同答复）、
无效选项、未投递都不写 accepted。`RetryPending`/`DismissPending` 各有独立
audit kind，失败写 `failed`。

## 6. API 语义

| 情况 | HTTP |
|---|---|
| 已记录且已投递 | 200 `{attention}` |
| 已记录、等待投递 | 202 `{attention, deliveryPending:true}` |
| 已记录、永久投递失败 | 409 `{attention, deliveryFailed:true, code:"answer_delivery_failed"}` |
| 已记录、工单关闭无投递 | 409 `{attention, deliveryFailed:true, code:"answer_not_delivered"}` |
| 部署无 TurnStarter 且有 live worker | 503 |
| option 不属于问题 / 缺答复 | 400（未记录，可修正） |

记录答复（含 pending/failed）后调用 `o.changed(tenantID)`，SSE 刷新 Board/Inbox。
Web 解析 202 显示「已记录、等待投递」，409 显示「已记录、投递失败」；工单详情的
「待投递输入」区提供重试/清除。

## 7. 停止/终态硬门（第五轮）

`ticketDeliveryClosed` 是唯一判据：`Mandate.Revoked()` 或状态为
`cancelling/cancelled/done/failed`。`deliverPendingLocked` 在循环前返回、`attemptPendingDeliveryLocked`
在入口再查一次（defense in depth），`answerAttentionLocked`/`retryAnsweredAttentionLocked`/
`RetryPending` 在动作前查。失败返回 `collab.ErrWorkItemClosed`（API 409），不写 pending、
不投递、不派工。`DismissPending` 不做该检查：它是终态 pending 的结算动作。UI 的 retry
按钮在停止/终态 disabled，dismiss 保留。

## 8. 补偿扫描的退避

`answerRepair` 由失败的 enqueue/receipt 或失败的 Recover 扫描置位；
`answerRepairAttempts/Next` 按 `pendingBackoff`（1s→30s）调度。`Recover` 的扫描错误只
记录并保持标记，不再中止恢复。housekeeping 到期重跑；一次完整通过清除标记。

## 9. 租约健康

`WriterLease.Healthy` 在持有锁的连接上 `SELECT 1`。`Run` 在 Recover 前与每轮 poll 前
调用；失败即记录并退出（defer Release 会丢弃死连接）。PG 后端被 restart/failover
终止时 advisory lock 随之释放，第二进程可接管；第一进程不会在失联后继续写。真实 PG
用例用 `pg_terminate_backend` 验证健康变红与接管。

## 10. 临时 decision 写失败

intent 已持久化后 `Host.AnswerAttention` 返回非永久错误：返回
`ErrAnswerDeliveryPending`（API 202 `deliveryPending`），`AnswerAttention` 触发
`o.changed`；housekeeping 的 `completeAnswerIntentLocked` 补写 decision 后投递。只有
`answerRejectionPermanent` 才删除 intent 并返回 4xx。

## 11. Observe / Bot 身份

`dispatchTicketLocked` 拒绝 `MandateObserve`（覆盖 accept、恢复、queued dispatch 与
resume）；`ResumeTicket` 对 observe 返回 `ErrInvalidRequest`。`superagentReady` 拒绝
`principal.BotID != ""`，产品 API 只服务非 Bot 身份；worker Bot 只能通过模型工具写。
