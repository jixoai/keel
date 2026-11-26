# collaboration-superagent

## ADDED Requirements

> **Worker tool boundary (task 4.5).** The model-facing `report`/`ask` tools
> are product extra tools, not part of the fixed eleven builtin tools: they are
> registered only when the mode is enabled. Both resolve the Ticket/Assignment
> from the ambient run session plus the durable session index; the schema has no
> ticket or assignment argument, so a model cannot name another Ticket. A
> non-worker session (including a bare sub-agent of a worker) fails closed with
> a typed refusal. The HTTP routes remain the operator surface:
> `POST /v1/superagent/tickets/{id}/report`, `.../ask`, `.../resume`,
> `.../stop`, and `POST /v1/superagent/attention/{id}/answer`. Operator actions
> require `task.write` plus the independent `OperatorAuthorizer` seam;
> policy-allowed Source comments may answer one unambiguous open Attention.
> The web/TUI operator board (read, answer, resume, stop over this HTTP surface) is implemented in
> `web/src/screens/books/superagent.tsx`; GitHub CI status, Feishu and auto-merge remain out of scope.

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
