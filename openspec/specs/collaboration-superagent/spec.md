# collaboration-superagent Specification

## Purpose
The super-agent collaboration mode: an orchestrator Bot turns external Source
events into Tickets with a recorded Mandate, dispatches them to Worker Bot
sessions through the guarded collaboration Host, routes structured Attention back
to the source thread, and writes results back through a durable Channel outbox.
Tickets, Assignments and Attentions are tenant-scoped durable records independent
of any Worker session's lifetime; answers and follow-ups are durable inputs with
at-least-once delivery and an operator recovery surface.

## Requirements

### Requirement: Worker 模型工具不得越权 Ticket

Worker 的模型工具 MUST 只作用于调用会话绑定的 Ticket/Assignment。工具 MUST 从 ambient
run scope 的 tenant/session 经 durable session index 解析 `mode_id` 与 `work_item_id`，并
MUST 要求 assignment 的 `session_id` 恰为该会话；MUST NOT 接受模型传入的任意 ticket/assignment
id 作为作用域。非 superagent worker 会话（含 orchestrator root、普通会话与 worker 的 bare
sub-agent）MUST fail closed 并返回明确错误，MUST NOT 静默成功或改投其他 Ticket。工具写操作
MUST 走 `task.write`，审计 MUST 记录 actor/tenant/ticket/action 且 MUST NOT 包含报告正文或
问题正文；blocked/closed Ticket MUST 拒绝新的报告/问题。`report` 与 `ask` MUST 幂等：重复
requestId MUST 返回同一事实（报告行或 Attention），MUST NOT 产生第二条终态通知或第二个问题。
requestId MUST 以 assignment+session 定作用域：被替换 worker 的旧 id MUST NOT 命中替换
assignment 的报告或前任 Attention。`waiting` 报告 MUST 引用一个仍为 open、且
assignment/session 都属于调用者的 Attention；已答/超时/他人的 Attention MUST 拒绝，MUST NOT
把 Ticket 置为 blocked 却没有 askable 的问题。模型输入的 summary/prompt/options/result MUST
有硬上限（字节/数量）。

#### Scenario: 非 worker 会话调用
- **WHEN** 普通会话或 worker 的 sub-agent 调用 `report`/`ask`
- **THEN** 返回类型化拒绝（scope/not_worker），不创建报告或 Attention

#### Scenario: 模型传入其他 Ticket
- **WHEN** 模型在参数中附带其他 ticketId/assignmentId
- **THEN** 工具忽略该字段，只作用于调用会话绑定的 Ticket

### Requirement: super-agent 是工单/调度模式

系统 SHALL 提供 `superagent` 协作模式：orchestrator Bot 从 Source 接收事件，创建/更新
Ticket，并把 Ticket 派给 Worker Bot Session；MUST NOT 把它实现为 Bot kind、单个“更大的
sub-agent”或纯 prompt 约定。orchestrator 与 worker MUST 是 Bot 角色，可在不同 WorkItem
中复用。

#### Scenario: 角色复用
- **WHEN** 同一个 Bot 在一个 Ticket 中作为 worker，在另一个 Ticket 中作为 orchestrator
- **THEN** 两次都使用自己的 Bot/权限边界，只改变本次 mode 位置

### Requirement: Source 与 Trigger 幂等

系统 SHALL 从 Source 读取事件并转为 Trigger；每个 Trigger MUST 有来源、actor、thread
和幂等键。重复读取同一事件 MUST NOT 创建第二个 Ticket。Source MUST 持久化读位置；重启后
MUST 从读位置继续，不重放已消费事件。

#### Scenario: 重复事件
- **WHEN** Source 因重试再次读到同一 Issue/comment 事件
- **THEN** 只产生一个 Trigger/一个 Ticket，重复事件被记录为已见

### Requirement: Ticket 生命周期与 Mandate

accept MUST 创建 Ticket 并记录 Mandate（来源、范围、强度、Policy 版本、发起人身份）；
Ticket MUST 独立于 Session 生命周期，支持 open → ended 以及人工 follow-up 后 resume；
只有人的 stop 可以撤销 Mandate。Ticket MUST 带 tenant_id，跨租户不可见。

#### Scenario: Worker 结束但 Ticket 未结束
- **WHEN** 一个 Worker Assignment 结束，但 Ticket 仍有未完成/待审状态
- **THEN** Ticket 保持 open，可创建新 Assignment 继续

#### Scenario: 人工停止
- **WHEN** 有权限的人要求停止 Ticket
- **THEN** Ticket 结束且不再接受自动输入，既有记录保留可审计

### Requirement: Assignment 独立于 Session

Ticket MUST 能绑定多个 Assignment；Assignment MUST 记录 Worker Bot、Session、状态和
报告。Assignment 结束 MUST NOT 自动结束 Ticket；重启后 MUST 能恢复或重派未完成 Assignment。
派工 MUST 幂等，重复 requestId MUST 返回同一 Assignment。

#### Scenario: 重启恢复
- **WHEN** host 在 Assignment running 时重启
- **THEN** 该 Assignment 被标记中断并以 recovery note 重启，不重复执行已完成外部操作

#### Scenario: 重派
- **WHEN** 原 Worker Session 不可用，Ticket 创建新 Assignment
- **THEN** 新 Assignment 继承 Ticket/Mandate，旧 Assignment 结果保留

### Requirement: Subscription 与输入扇出

Ticket MUST 能订阅一个或多个 Thread；匹配的 Trigger MUST fan-out 给所有活跃订阅的 Ticket，
且 MUST NOT 因发送者身份跳过订阅。上下文订阅 MUST NOT 授予权限或自动 resume 已结束 Ticket；
人员消息按 Ticket 归属规则处理。

#### Scenario: 后续讨论到达 Worker
- **WHEN** 订阅 Thread 出现新的人类评论
- **THEN** 该输入被投递给对应 Ticket/Worker，不创建无关 Ticket

### Requirement: 结构化 Attention

Worker MUST 通过结构化 Attention 请求人决策，包含问题、来源、选项/自由文本、路由和
过期时间；系统 MUST NOT 从自由文本摘要推断“等待人工”。人答复 MUST 唤醒同一 Ticket；
问题 MUST 路由回原始对话/来源，不能发到无关 Channel。

#### Scenario: 问题回原对话
- **WHEN** Worker 对一个来自 GitHub Issue 的 Ticket 提出问题
- **THEN** Attention 出现在该 Issue/PR 线程；答复后同一 Ticket 继续

### Requirement: Channel 投递与重试

系统 MUST 通过 Channel 把确认、结果、Attention 写回 Source；消息 MUST 先持久化再发送，
按 Ticket/会话保序，失败按退避重试；消息 id MUST 幂等，已发送/已丢弃不得重复入队。
Channel MUST NOT 泄露其他租户或私有 Source 内容。

#### Scenario: 发送失败重试
- **WHEN** GitHub 评论写入暂时 5xx
- **THEN** 消息保留在 Outbox 并按退避重试，成功后标记 sent

#### Scenario: 不重复终态通知
- **WHEN** 同一 Ticket 的完成通知已发送，后续 CI 事件到达
- **THEN** 不再发送第二条完成通知

### Requirement: 权限、策略与租户边界

谁能通过哪个 Source 发起工作 MUST 由 Policy/权限判定；所有跨 Bot 调用 MUST 走
`bot.invoke`；Source 文本 MUST NOT 修改 Policy 或授权新工作；所有记录 MUST 带 tenant_id，
跨租户读写返回 404/403。MUST NOT 新增 super-agent 专属权限。

#### Scenario: 未授权发起
- **WHEN** 不在允许列表的人通过 Source 请求工作
- **THEN** Eval 记录 ignore/unavailable，不创建 Ticket，不执行工具

### Requirement: Worker 可以使用 subagent

Worker Session MUST 能使用 `subagent` mode 继续拆分自己的任务；sub-agent 的深度/预算/
权限由 subagent Host 执行，MUST NOT 把 Ticket/Assignment 层级计入 sub-agent depth。

#### Scenario: Worker 派子任务
- **WHEN** 一个 Worker 在 Ticket 内派两个 sub-agent
- **THEN** sub-agent depth 从 Worker 的委派树 0 开始计算，Ticket 层级不消耗 depth

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
- **WHEN** AnswerAttention 返回 `ErrAnswerDeliveryPending`（含 intent 已持久化但
  Attention 写临时失败）
- **THEN** 响应为 202，body 同时带 attention 与 deliveryPending=true
- **AND** 页面提示已记录、等待投递，收件箱在 SSE 后刷新
- **AND** housekeeping 补写缺失的 decision 后完成投递

#### Scenario: 409 投递失败
- **WHEN** AnswerAttention 返回 `ErrAnswerDeliveryFailed`
- **THEN** 响应为 409，body 带 attention、deliveryFailed=true 与
  `code=answer_delivery_failed`，页面指向工单的重试入口

### Requirement: 同内容 pending 去重

在第一个编辑尚未投递时，issue_updated 的重复 bump（title/body 与最后一个 pending
snapshot 相同，含 failed 行；dismiss 移除后不再比较）MUST NOT 捕获第二条 pending，也 MUST NOT 在之后重复投递；
真正不同的编辑仍 MUST 逐条捕获。

#### Scenario: 评论造成的 updated_at bump
- **WHEN** 一次真实编辑已捕获为 pending 但未投递，随后同内容事件带新 event id 到达
- **THEN** 不新增 pending 条目；只有内容真正变化时才新增

### Requirement: 队列单写者与租约健康

`Orchestrator.Run` MUST 在轮询/派工之前取得租约。生产 PostgreSQL MUST 使用会话级
advisory lock；第二进程 MUST fail closed（`ErrWriterLeaseHeld`），且产品启动 MUST
在中断旧 worker 会话之前完成租约获取，避免第二副本破坏第一副本的 Worker。Store
不支持租约时 Run MUST fail closed。租约 MUST 暴露 `Healthy` 并在 Recover 之前及每轮
poll 之前复核（PostgreSQL `SELECT 1` 走持有锁的会话）；连接被 PG 重启/failover 杀掉、
锁已释放时 Run MUST 立即停止写并退出。MemoryStore 的进程内租约仅用于测试/演示。

#### Scenario: 第二副本启动
- **WHEN** 同一 tenant 的第二个 keel serve 尝试启动 super-agent
- **THEN** 它在中断任何 worker 之前被租约拒绝，不轮询、不投递
- **AND** 第一副本退出后新进程可以取得租约

#### Scenario: 租约后端被杀死
- **WHEN** 持有租约的 PostgreSQL 会话被 restart/failover 终止
- **THEN** 下一次 `Healthy` 失败，Run 停止写并退出
- **AND** 第二个进程可以在锁释放后接管

### Requirement: Answer 评论补偿与审计真实性

已投递答复的 Channel 评论 MUST 先以幂等键持久化到 outbox，崩溃只会造成重试；
receipt MUST 记录评论行是否已入队，启动补偿扫描 MUST 为
`commentQueued=false` 的 answered Attention 补齐该行。super-agent 审计只在投递
成功后记录 `accepted`；冲突、无效答复、未投递 MUST NOT 写 accepted 行。

#### Scenario: 评论入队失败后重启
- **WHEN** Worker 已收到答复但 outbox 入队失败，进程随后重启
- **THEN** 启动扫描补写同一幂等键的评论行，且只有一行
- **AND** 对话线程看到答复评论，时间不早于 Worker 实际收到答复

### Requirement: 停止与终态后的投递边界

`Mandate` 已撤销、Ticket 为 cancelling/cancelled/done/failed 时，系统 MUST 拒绝任何
输入投递与重试：`deliverPendingLocked`、`attemptPendingDeliveryLocked`、
`answerAttentionLocked`、`retryAnsweredAttentionLocked`、`retryPending` MUST 在投递前
返回 closed/not-delivered（HTTP 409），MUST NOT 投递、MUST NOT 清除或改写 pending 行
（不增加 attempt、不标 Failed）。`dismissPending` MUST 保留为终态 pending 的结算入口
（HTTP 200）；Web MUST 对停止/终态禁用 retry 并保留 dismiss。该边界 MUST 覆盖
`Host.Cancel` 失败后仍存在的 live assignment。

#### Scenario: cancelling 且仍有 live assignment
- **WHEN** 停止请求已撤销 Mandate，Host.Cancel 失败留下 cancelling 与 live worker，
  且存在 answer/follow-up pending
- **THEN** housekeeping/retry 不投递任何内容，pending 原样保留
- **AND** retry API 返回 409，dismiss 仍可结算

#### Scenario: 终态工单的 pending
- **WHEN** 工单为 done/failed/cancelled 且仍有 pending
- **THEN** answer/retry 返回 409，零投递、零派工；retry 审计为 failed，dismiss 审计为
  accepted

### Requirement: 补偿扫描失败必须退避重试

启动补偿扫描（answered 无证据/评论未入队）失败时，系统 MUST 保持补偿标记并按
1s→30s 退避在 housekeeping 重跑；MUST NOT 因一次错误留下永久孤儿。`Recover` 的扫描
错误 MUST NOT 中止其余恢复工作。

#### Scenario: 扫描第一次失败
- **WHEN** Recover 的 `ListAttentionsByStatus` 第一次返回错误
- **THEN** 恢复继续，补偿标记保留
- **AND** 退避到期后的 housekeeping 完成 answer 的 re-arm 与投递

### Requirement: Observe 与 Bot 身份的派工边界

`MandateObserve` MUST NOT 派工：accept、queued dispatch、恢复与人工 resume MUST 都不
创建 Worker。`/v1/superagent/*` 产品 API MUST 拒绝 Bot principal（worker Bot key 或任何
委托），即使其 Harness 收窄包含 task.read/task.write；模型工具是 Bot 唯一的写入口。

#### Scenario: observe 工单被人工恢复
- **WHEN** 操作者对 observe-only Mandate 的工单执行 resume
- **THEN** 请求被拒绝，零派工

#### Scenario: worker Bot key 调用产品 API
- **WHEN** 请求身份带有 BotID
- **THEN** 所有 `/v1/superagent/*` 路由返回 403

### Requirement: 租约失联后所有写入口 fail closed

`WriterLease` 失联（Healthy 失败/已释放）后，产品 API 与 Source 处理的所有写入口
（answer/report/ask/resume/stop/retry/dismiss、Source 事件、poll/housekeeping/recover）
MUST 在任何状态变更前拒绝（`ErrWriterLeaseLost`，HTTP 503）；读取（Board/Inbox）
MUST 保持可用。`Run` 失联退出与写入口检查 MUST 同时存在，避免 API 进程在租约
死亡后继续写而第二副本已接管。

#### Scenario: 租约连接死亡后的 API 写
- **WHEN** 租约连接被 PostgreSQL 终止，第二进程取得租约
- **THEN** 第一进程的 answer/report/ask/stop/retry 均返回 503，不写、不投递
- **AND** 读接口仍返回既有投影

### Requirement: 补偿扫描在本轮内不得清掉新置位的标记

补偿扫描 MUST 在结束前重新读取本轮触及的 Ticket 的当前 receipt；任何
`CommentQueued=false && !Dismissed` 的 receipt MUST 令本轮不完整（保留 repair 标记并
退避重跑）。`loadTicket` 失败 MUST 视为不完整而不是静默跳过。`deliverPendingLocked`
MUST 返回本轮未入队的 answer 评论数，供扫描判定。

#### Scenario: Recover 扫描期间 outbox 仍失败
- **WHEN** 答复已投递但评论入队失败，Recover 的扫描再次入队失败
- **THEN** repair 标记保留，退避到期后的 housekeeping 再次扫描
- **AND** outbox 恢复后恰好写入一条 answer 评论并标记 CommentQueued

### Requirement: 终态 answer 的 409 载荷

停止/终态工单上的 answer MUST 返回 409，载荷 MUST 含 `attention`、
`deliveryFailed=true` 与 `code=answer_not_delivered`；Web MUST 按该 code 显示
“答复未送达/工单已停止或终态”，不得显示“已记录、投递失败”。retry 在终态返回 409
（本 change 既有要求）；dismiss 可在终态结算（200）。

#### Scenario: 停止后回复答案
- **WHEN** 工单已 stopped/done/failed，操作者调用 answer
- **THEN** 响应 409，code=answer_not_delivered，带 attention 与 deliveryFailed
- **AND** Web 的提示不再声称答复已记录

### Requirement: retry 后条目仍 Failed 的 API 语义

`retryPending` 的 HTTP 结果 MUST 区分“动作被接受”和“投递成功”：条目在本次重试后
仍为 Failed/Pending 时，接口返回 200 与工单当前状态（Board 显示失败行），审计行
MUST 记为 `failed`；只有条目离开队列（或答案已投递）才记 `accepted`。停止/终态
工单的 retry MUST 返回 409 且不改变条目。

#### Scenario: 投递 seam 仍不可用
- **WHEN** 操作者对 Failed 条目 retry，seam 仍拒绝
- **THEN** 200 + 工单（条目仍 Failed），审计 pending_retried=failed
- **AND** 下一次 retry 或 dismiss 仍是操作者的选择
