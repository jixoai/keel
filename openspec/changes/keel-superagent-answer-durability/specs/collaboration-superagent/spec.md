# collaboration-superagent

## ADDED Requirements

### Requirement: 答复投递必须可恢复且不得静默成功

Attention 的答复 MUST 按「持久意图 → Attention answered → 投递 → 投递证据(receipt)」
的顺序写入。意图与待投递输入 MUST 位于同一 Ticket 文档并由同一次
`UpdateWorkItem` 持久化；receipt MUST 与移除 pending 在同一次写中落库。任何
「Attention answered 但没有 pending、也没有 receipt」的状态 MUST 被重试或启动扫描
自愈（从 Attention 自身的答复重新武装 intent 并投递），或在工单已关闭/撤销时以
明确错误（`ErrAnswerNotDelivered`）失败；MUST NOT 返回成功。重复同一答复 MUST
幂等；不同答复 MUST 冲突。投递尝试 MAY 重复（at-least-once），但输入请求 MUST
带以 Attention id 定作用域的幂等键。

#### Scenario: 崩溃在 intent 之后、答复记录之前
- **WHEN** 进程写完答案 intent 后在 `Host.AnswerAttention` 之前退出
- **THEN** 重试先补写 Attention answered，再投递，最终留下 receipt

#### Scenario: Attention 已 answered 但无投递证据
- **WHEN** 旧版本或中断留下 `status=answered` 且无 pending/receipt 的 Attention，工单仍开放
- **THEN** 重试从 Attention.Answer 重建 intent 并实际投递（或启动扫描完成同样的修复）
- **AND** 工单已关闭时返回明确失败，不返回成功

#### Scenario: 投递后崩溃
- **WHEN** Worker 已收到答复但进程在移除 pending 之前退出
- **THEN** 重试以相同幂等键再投递一次并写 receipt，MUST NOT 丢失答复

### Requirement: 投递重试与人工出口

系统 MUST 区分临时错误与永久错误：TurnStarter/DB/launcher/网络等临时错误 MUST
无限重试（退避 1s 起、30s 封顶），MUST NOT 因尝试次数达到固定上限而标记 Failed；
只有明确的永久错误（部署无法投递、权限拒绝、无效请求、工单关闭、租户不匹配）
MAY 标记 Failed。Failed 的 pending MUST 在 Board/API 可见（含完整行与
`failedPendingCount`），且 MUST 提供人工重试与清除；清除一条已记录但未投递的答复
MUST 留下 durable 放弃标记，避免启动扫描复活。

#### Scenario: 临时失败超过十次
- **WHEN** launcher 连续失败 12 次 housekeeping 后恢复
- **THEN** 第 11 次及之后的 tick 仍会投递，条目在恢复前不是 Failed
- **AND** 成功投递后 pending 清空、快照推进

#### Scenario: 永久失败与人工处理
- **WHEN** 投递被权限拒绝或部署无 TurnStarter
- **THEN** 条目标记 Failed 并在 Board 上列出错误
- **AND** `retry` 重新尝试；`dismiss` 移除条目并使答复不再被自动重投

### Requirement: 不可信来源上下文不得派工或解锁工单

pending 输入 MUST 在捕获时持久化 Policy 判定的 `Trusted` 与派工授权
`AllowDispatch`。没有 live assignment 时，只有结构化的 Attention 答复或
policy-accepted 的人类 follow-up MAY 创建新 Worker；不可信上下文 MUST 等待 live
Worker 或保持待投递，MUST NOT 自行派工。blocked 工单 MUST 只由答复或 trusted
follow-up 解锁；不可信上下文 MUST NOT 把工单改成 active，也 MUST NOT 在
Attention 仍 open 时投递给阻塞中的 Worker。不可信条目 MAY 延后，但 MUST NOT
阻塞其后的人类答复/可信 follow-up。

#### Scenario: Worker 结束后不可信上下文
- **WHEN** 不可信评论在 Worker 存活时被持久化，随后该 Worker 结束
- **THEN** housekeeping 不派新 Worker；条目保留为待投递
- **AND** 之后的可信 follow-up 可以派工，不可信上下文随后投递给新 Worker

#### Scenario: blocked 工单收到不可信评论
- **WHEN** 工单在等待人工答复（Attention open），线程出现未授权评论
- **THEN** 工单保持 blocked、Attention 保持 open、不可信内容不投递

### Requirement: Answer 的 API/SSE/UI 语义

答复被持久记录后，API MUST 以明确状态表达投递状态：已投递 200；
已记录但等待投递 202 且携带 `attention` 与 `deliveryPending`；已记录但永久投递
失败 409 且携带 `attention`、`deliveryFailed` 与稳定 code；部署无法投递 503；
非法选项/空答复等未记录输入 MAY 400。MUST NOT 把已记录事实落成 400。记录答复
（包括 pending/failed）后 MUST 触发 Board/Inbox 的变更通知（SSE）。Web MUST
解析 202/409 并显示「已记录、等待投递」/「已记录、投递失败」，并在工单详情提供
可见的重试与清除入口。

#### Scenario: 202 等待投递
- **WHEN** AnswerAttention 返回 `ErrAnswerDeliveryPending`
- **THEN** 响应为 202，body 同时带 attention 与 deliveryPending=true
- **AND** 页面提示已记录、等待投递，收件箱在 SSE 后刷新

#### Scenario: 409 投递失败
- **WHEN** AnswerAttention 返回 `ErrAnswerDeliveryFailed`
- **THEN** 响应为 409，body 带 attention、deliveryFailed=true 与
  `code=answer_delivery_failed`，页面指向工单的重试入口

### Requirement: 同内容 pending 去重

在第一个编辑尚未投递时，issue_updated 的重复 bump（title/body 与最后一个未失败
pending snapshot 相同）MUST NOT 捕获第二条 pending，也 MUST NOT 在之后重复投递；
真正不同的编辑仍 MUST 逐条捕获。

#### Scenario: 评论造成的 updated_at bump
- **WHEN** 一次真实编辑已捕获为 pending 但未投递，随后同内容事件带新 event id 到达
- **THEN** 不新增 pending 条目；只有内容真正变化时才新增

### Requirement: 队列单写者

`Orchestrator.Run` MUST 在轮询/派工之前取得租约。生产 PostgreSQL MUST 使用会话级
advisory lock；第二进程 MUST fail closed（`ErrWriterLeaseHeld`），且产品启动 MUST
在中断旧 worker 会话之前完成租约获取，避免第二副本破坏第一副本的 Worker。Store
不支持租约时 Run MUST fail closed。MemoryStore 的进程内租约仅用于测试/演示。

#### Scenario: 第二副本启动
- **WHEN** 同一 tenant 的第二个 keel serve 尝试启动 super-agent
- **THEN** 它在中断任何 worker 之前被租约拒绝，不轮询、不投递
- **AND** 第一副本退出后新进程可以取得租约

### Requirement: Answer 评论补偿与审计真实性

已投递答复的 Channel 评论 MUST 先以幂等键持久化到 outbox，崩溃只会造成重试；
receipt MUST 记录评论行是否已入队，启动补偿扫描 MUST 为
`commentQueued=false` 的 answered Attention 补齐该行。super-agent 审计只在投递
成功后记录 `accepted`；冲突、无效答复、未投递 MUST NOT 写 accepted 行。

#### Scenario: 评论入队失败后重启
- **WHEN** Worker 已收到答复但 outbox 入队失败，进程随后重启
- **THEN** 启动扫描补写同一幂等键的评论行，且只有一行
- **AND** 对话线程看到答复评论，时间不早于 Worker 实际收到答复
