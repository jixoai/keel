# Proposal: keel-superagent-answer-durability

> 立项：2026-11-27。承接已归档的 `keel-collab-superagent`（live GitHub 交付），
> 修复第五轮复审仍然存在的 1 个 P0 与 3 个 P1，并把 3 个 P2 一并收口。
> 定位：不是新能力，而是把「Attention 答复 → Worker 收到」这条跨文档写路径
> 改成可恢复的持久化协议。

## Why

复审确认的失败模式：

- **P0-1 answer 双写窗口**：旧实现先 `Host.AnswerAttention`，再单独写 pending。
  进程在两次写之间崩溃后，Attention 已经是 `answered`，但没有任何待投递输入、
  也没有投递证据；重试 sameAnswer 直接返回成功，Worker 永远收不到答复，且没有
  任何重投路径。
- **P1-1 10 次后永久 Failed**：临时错误（TurnStarter/DB/launcher/网络）也在第 10
  次被标成 Failed；Board/API 没有人工重试/清除入口。
- **P1-2/P1-3 不可信评论获得派工/解锁能力**：无 live assignment 时，pending 里的
  不可信上下文仍会派新 Worker；blocked 工单被不可信评论投递后会变 active，而
  Attention 仍 open。
- **P1-4 API/UI 语义**：`ErrAnswerDeliveryPending` 落 400；记录答复后不发 SSE；
  Web 不能显示「已记录、等待投递」，失败也没有可见重试入口。
- **P2**：同内容 pending 重复捕获；多副本没有单写者约束；answer 评论 outbox 与
  pending 移除之间没有补偿；审计在投递前就写 `accepted`。

## What Changes

1. **答案写序**：`durable intent → Attention answered → delivery → receipt`。
   intent 与 receipt 都存在 Ticket 的 WorkItem JSON 文档里，于是「answer 意图」
   与「待投递输入」是同一次 `UpdateWorkItem`；任何中断都能从 Attention 自身或
   intent 重放。
2. **恢复语义**：重试/重启时，`answered` 但无 intent/receipt 的 Attention 会被
   重新武装并投递；工单已关闭时明确返回 `ErrAnswerNotDelivered`，绝不静默成功。
3. **重试策略**：临时错误无限重试（退避 30s 封顶）；只有权限/不可投递/4xx 等
   永久错误标 `Failed`。Board 暴露 `pendingCount/failedPendingCount` 与整条
   pending 行，API 提供 retry/dismiss。
4. **信任边界**：capture 时持久化 `Trusted/AllowDispatch`；无 live assignment 时
   只有 answer 或 policy-accepted follow-up 可派工；blocked 工单只允许这两类
   输入解锁。
5. **API/UI**：pending → 202 `{attention, deliveryPending}`；failed → 409
   `{attention, deliveryFailed, code}`；不可投递 → 503；记录答复后触发
   `o.changed`/SSE；Web 显示「已记录、等待投递」并在工单详情提供重试/清除。
6. **P2**：同内容 pending snapshot 去重；PG advisory lock 单写者 lease（启动即
   fail closed，且在中断旧 worker 会话之前获取）；answer 评论 outbox 前置入队 +
   启动补偿扫描；`accepted` 审计只在投递成功后写。

## Impact

- 代码：`internal/superagent/{domain,store,orchestrator}.go`、
  `internal/store/superagent.go`、`internal/harnessapi/superagent.go`、
  `cmd/keel/superagent.go`、`scripts/superagent-acceptance/main.go`、
  `web/src/{data/{contract,http,superagent,fixture},screens/books/superagent*}`。
- 数据：Ticket 文档新增 `answers`、pending 新增信任/答案字段；无新表。
  PG 侧只新增一个会话级 advisory lock 与一个按状态查询。
- 兼容：Alpha 零兼容，旧字段不保留垫片；旧的「answered 无证据」行由启动扫描
  或显式重试自愈。
