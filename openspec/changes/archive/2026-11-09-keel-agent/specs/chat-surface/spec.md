# chat-surface

## ADDED Requirements

### Requirement: 纯逻辑核心（零 DOM）

`packages/chat-surface` SHALL 是一个框架无关的纯 TS 包：输入为 keel 事件流（SSE 已提交事件 +
可选 partial 帧 + 本地乐观输入），输出为 `ConversationView`；MUST NOT 依赖 DOM、React、NSH 或
任何渲染框架；单测可在 Node 下运行。

#### Scenario: Node 下消费同一事件序列
- **WHEN** 用一段固定的事件 fixture（用户/助手/工具/压缩/审批）驱动核心
- **THEN** 输出稳定的 `ConversationView`，同一输入两次运行逐字节一致

### Requirement: Turn 投影

`ConversationView` SHALL 由 `Turn[]` 组成，`turnId` 为权威身份；每个 `Turn` MUST 含
`origin`（`user_prompt|queue_drain|turn_steer|goal_continuation|manual_compact`）、
`status`（`running|settling|completed|interrupted|failed`）、`visibleInputs`、
`workSegments`（每条 guide 输入开一个视觉工作段）、`workLog`（工具调用/结果/耗时）、
`assistantOutput`、`postTurnEvents`、`actionTargets`。
`turnOrdinal` 仅用于展示，MUST NOT 作为 mutation/恢复锚点。

#### Scenario: 引导开新工作段
- **GIVEN** 一个 product turn 内先有一次 guide 注入
- **THEN** 该 turn 有 2 个 workSegments（原始输入 + guide），turn 边界不变

#### Scenario: 取消/失败的状态
- **WHEN** 收到 `run.finished`（stopReason=cancelled/error）或取消确认
- **THEN** turn 状态为 `interrupted`/`failed`，已展示内容保留

### Requirement: queue / guide 状态机

核心 SHALL 实现与 zcode `interaction-behavior.md` 对齐的投递状态机：

- 默认 `queue`；运行中输入进入队列面板（含「立即发送」抢占路径，reservation + stop barrier）；
- `guide`：立即显示为当前对话流末尾的用户消息 + 「等待引导当前任务…」；收到
  `delivery=guide` 的消费事实后转为正式输入行；若服务端降级为 `queue`，临时行消失、
  同一项回到队列面板（同一 sourceCommandId）；
- 队列消费边界是 **session ready**（不是任意一条 timeline 完成）：compaction/goal
  verification 等维护活动进行中不自动 drain；`run.finished` 且无未答输入才是 ready。
- 停止：进入 `stopping`，等待权威确认（run.finished=cancelled 或 done），期间禁止自动 drain。

#### Scenario: 引导降级
- **WHEN** 客户端发 guide，服务端返回 `delivery=queue`
- **THEN** UI 不显示「等待引导」，该项出现在队列面板且顺序保持

#### Scenario: 停止期间的队列
- **GIVEN** 队列里有一条消息，用户点停止
- **WHEN** 停止确认到达
- **THEN** 队列不自动发送，需用户显式恢复/发送

### Requirement: 续传与幂等

核心 SHALL 以 `after`/`Last-Event-ID` 游标续传；重复事件 MUST 幂等（按 `(sessionId, seq)` 去重）；
断线后 MUST 先拉快照（`GET /events?after=`）再订阅，避免 gap；重连期间乐观输入不丢，
以 `requestId` 与服务端事实收敛。

#### Scenario: 断线重连
- **WHEN** 流中断后以最后 seq 重连
- **THEN** 视图不重复、不跳号；乐观输入要么被确认，要么按 `requestId` 去重后保留一条

### Requirement: 审批（decision）投影

核心 SHALL 把 `decision` 事件投影为可作答的行（问题/选项/推荐/状态）；提交答案走
`POST /decisions/{id}`；超时/取消 MUST 更新行状态；同一 decisionId 只允许一个答案。

#### Scenario: 审批超时
- **WHEN** decision 状态变为 timeout
- **THEN** 行显示「已超时」，选项禁用，run 状态随事件更新

### Requirement: AG-UI 映射

核心 SHALL 提供 `toAgUi(events)` 映射（keel 事件 → AG-UI 事件集），作为与其它渲染器
（含将来 assistant-ui）的公共接口；映射表 MUST 落 `docs/CHAT-SURFACE.md` 并被单测钉住。

#### Scenario: 文本与工具
- **WHEN** 输入一条 assistant（text+toolCall）与一条 tool.result
- **THEN** 输出 `TextMessage*` 与 `ToolCall*`（含 result）序列，顺序与事件一致

### Requirement: 性能与有界

核心 MUST 对长会话有界：只保留渲染所需窗口（分页拉取），工具卡/消息列表不得无界增长；
10k 事件的 fixture 下单次投影 MUST 在 50ms 量级完成（Node 基准，记录在文档）。

#### Scenario: 10k 事件的有界投影
- **GIVEN** 一个 10k 已提交事件的会话
- **WHEN** 核心从快照重建视图
- **THEN** 单次投影在 50ms 量级完成（实测 hydrate 约 7–9ms、逐条 apply 约 11–21ms），
  且渲染窗口只保留最新 N 轮，内存不随事件总数无界增长
