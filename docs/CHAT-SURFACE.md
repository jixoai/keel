# CHAT-SURFACE — headless 会话投影核心

`@keel/chat-surface`（`packages/chat-surface/`）是 keel 的**框架无关会话投影核心**：
输入是 keel 已提交事件（SSE/快照）＋本地乐观输入，输出是 `ConversationView`。
零 DOM、零 React、零 NSH、零运行时依赖；不直接 `fetch`（`EventSource` 由宿主注入），
不写 UI。NSH web/TUI 与将来的任意渲染器（assistant-ui 等）共用这一份状态机。

契约单源：`packages/chat-surface/src/types.ts`（冻结）＋本文件（事件/映射/对齐表）。
需求与场景：`openspec/changes/keel-agent/specs/chat-surface/spec.md`。

## 1. 包结构

| 文件 | 职责 |
| --- | --- |
| `src/types.ts` | 冻结契约：`WireEvent` / `ConversationView` / `TurnView` / `SurfaceEngine` / `EventSource` / `SurfaceCommands` |
| `src/events.ts` | wire payload 解析（宽容：字段别名、pi content block、缺省值） |
| `src/projection.ts` | 事件→视图投影 + queue/guide/stop 状态机（纯逻辑，无定时器） |
| `src/engine.ts` | `SurfaceEngine` 门面：乐观输入、订阅、partial、`attach` 断线重连 |
| `src/agui.ts` | `toAgUi(events)`：keel 事件 → AG-UI 事件集 |
| `test/` | 投影/状态机/续传/审批/AG-UI/10k 性能基准（`bun test`） |

```ts
import { ChatSurfaceEngine, toAgUi } from "@keel/chat-surface";
```

## 2. API

### SurfaceEngine（冻结）

| 方法 | 语义 |
| --- | --- |
| `view()` | 当前 `ConversationView` |
| `submit(text, delivery, requestId)` | 乐观输入立即上屏；权威事件按 `requestId` 收敛 |
| `apply(event)` | 应用一条权威事件；按 `(sessionId, seq)` 幂等；返回 `applied`/`duplicate`/`buffered` |
| `applyPartial({text, turnId?})` | token partial：不落视图，仅 `engine.partial()` |
| `hydrate(events)` | 从快照重建（保留乐观输入与游标水位） |
| `requestStop()` | 进入 `stopping`，等待权威确认 |
| `subscribe(fn)` | 视图变化通知；返回取消函数 |

### 扩展面（实现类额外提供，不破坏冻结接口）

| 方法 | 说明 |
| --- | --- |
| `submitNow(text, requestId)` | 忙碌时「立即发送」：stop barrier + reservation（见 §5） |
| `canAutoDrain()` | session ready 且队头可被运行时自动消费时为 true（提示值） |
| `promotionReady()` | 停止确认后，等待宿主用同一 `requestId` 发送的 reservation |
| `resumeAutoDrain()` | 用户显式恢复：解除 stop 后的自动消费暂停 |
| `answerDecision(id, answer)` | 审批乐观作答（首个答案胜出）；POST 仍由宿主执行 |
| `applyMany(events)` | 批量/补洞应用（按 seq 排序） |
| `attach(source, {partials, limit})` | 快照分页 → 订阅流；断线回到 `open` 时先补拉再信任流 |
| `disconnect()` | 停流并把连接状态置 `closed` |
| `needsResync()` / `lastSeq()` | gap 检测与游标 |
| `partial()` / `setConnection(status)` | partial 帧 / 连接状态投影 |

## 3. wire 事件契约（payload）

`WireEvent = { seq, kind, payload }`；`payload` 为 JSON。解析器接受以下字段（缺省值在括号内）：

| kind | 字段 | 说明 |
| --- | --- | --- |
| `user.message` | `text`, `requestId`(`req_<seq>`), `delivery`(`queue`), `origin?`, `turnId?`, `timestamp?` | `delivery=guide` 只在有在途 turn 时有效 |
| `assistant.message` | pi `AssistantMessage`：`content[{type:text|thinking|toolCall,...}]`；keel 扩展 `inputSeqs[]`, `turnId?`, `origin?`, `stopReason?`, `usage?`, `timestamp?` | 也接受扁平 `text`/`thinking`/`toolCalls` |
| `tool.result` | `toolCallId`, `toolName?`, `content`（text blocks）, `details?`, `result?`, `isError?`, `error?`, `durationMs?`, `timestamp?` | 结果优先取 `result` → `details` → 文本 |
| `compaction` | `summary`, `status?`(`completed`), `phase?`, `operationId?`, `manual?`, `tokensBefore?`, `timestamp?` | `started/retrying` 视为维护中 |
| `run.started` | `turnId?`, `runId?` | 只更新已存在 turn，不凭空造 turn |
| `run.finished` | `stopReason?`(`done`), `status?`, `turns?`, `tokensIn/tokensOut?` 或 `tokens{in,out}/usage`, `durationMs?`, `turnId?`, `runId?`, `errorMessage?` | `cancelled/aborted → interrupted`；`error/failed → failed` |
| `decision` (及 `decision.*`) | `decisionId`, `prompt`/`question`, `options[{id|optionId,label,description?}]`, `recommendation?`, `status?`(`open`), `answer?`（字符串或 `{optionId,text,at}`） | 同 id 更新同一行；`timed_out → timeout` |
| `goal.*` | `status?`/`phase?`, `target?`, ... | 验证/继续活动期间 `busy=true`，阻止 auto-drain |
| `external.event` | 任意 | 兼容日志；落到最近 turn 的 `postTurnEvents` |
| 其它 | 任意 | 前进兼容：落到 `postTurnEvents`（找不到 turn 时丢弃） |

> 服务端 SHOULD 在 `user.message`/`assistant.message` 带 `turnId` 与 `origin`；缺失时见 §4。
> 时间字段缺失时 `InputView.createdAt` 回退到事件 `seq`（确定性优先；服务端应带
> `timestamp`，工具耗时则用 `timestamp + durationMs` 反推）。
> 审批在 `agentext` 是产品层原语（独立 decision journal，不是内核事件 kind）：宿主可把它
> 作为 `decision` 事件喂给核心，或从 `GET /v1/sessions/{id}/decisions` 合成同形状事件；
> 核心只消费 `decision` 事件，不直接访问 decisions API。

## 4. Turn 投影

- **权威身份**：`turnId`。缺失时派生 `t_<起手事件 seq>`（确定性）。显式 `turnId` 晚到且输入
  属于已在途 turn 时作为**别名**并入（first identity wins），避免同一 turn 裂成两条。
- **`ordinal` 仅展示**，投影与恢复从不拿它做锚点。
- **origin 推断**：payload `origin` 优先；否则 guide 消费→`turn_steer`；提交时忙碌→`queue_drain`；
  否则 `user_prompt`；无输入的新 turn→`goal_continuation`（有历史时）。
- **visibleInputs**：放在 turn 里的输入。首个输入在 turn 创建时进入；其余输入在
  `assistant.inputSeqs` 首次引用时进入并标记 `consumed`。`inputSeqs` 是**整段 transcript**
  （多轮 tool 循环会重复旧 seq）：新 turn 只认领尚未消费的 fresh 输入，避免把
  queue-drain 的回答挂回已完成的旧 turn。
- **workSegments**：turn 创建即有 `s0`（trigger=首个输入）；**每条被消费的 guide 输入新开一段**；
  同一 turn 的 `turnId` 不变。工具调用/结果/assistant 文本/thinking 落到当前段。
- **assistantOutput**：各段 assistant 文本按顺序拼接（段间/消息间 `\n\n`）。
- **runSummary**：`run.finished` 落到收口 turn。
- **postTurnEvents**：compaction / goal.* / external / 未知 kind 的追加日志
  `{kind, seq, note?}`（note 截断 500 字符）。
- **状态**：`running` → `settling`（assistant 已产出但 goal verifier 等 post work 未收口）
  → `completed|interrupted|failed`（`run.finished` 权威）。`assistant.stopReason=error/aborted`
  可提前把 turn 置 `failed/interrupted`。
- **压缩**：active compaction 使 `busy=true`，但**不改变 turn 边界**；`completed` 不是
  session-ready 边界（仍等 `run.finished`）。手动压缩且无在途 turn 时创建
  `origin=manual_compact` 的 turn。
- **有界**：只保留最近 `windowTurns`（默认 200，`0`=不限）；裁剪时同步清 toolCall 索引与
  已消费输入的 seq 索引；`ordinal` 继续递增。

### 与冻结类型的差异（types.ts 不可改）

| spec 字段 | 本包落点 | 原因 |
| --- | --- | --- |
| `workLog` | `workSegments[].toolCalls` + `postTurnEvents` | 冻结的 `TurnView` 无 `workLog` 字段；工具天然属于工作段 |
| `actionTargets` | 渲染器从 `visibleInputs`/`assistantOutput` 派生 | 冻结的 `TurnView` 无该字段 |

## 5. queue / guide / stop 状态机

### 状态变量

- `busy = stopping || compactionActive || goalActive || 存在 running/settling turn`
  （`ConversationView.busy`）。
- `stopping`：用户已请求停止，等权威确认。
- `pausedAfterStop`：用户 stop 确认后，队列**不自动发送**，直到用户显式发送/恢复。
- `pending`（投影为 `ConversationView.queue`）：未进入 turn 的输入（乐观 + 权威未消费）。
  - `delivery="guide"` 的 pending 项 = 渲染器在对话流末尾渲染的「等待引导当前任务…」临时行。
  - `delivery="queue"` = 队列面板项。
  - `immediate=true` = 「立即发送」reservation。
- `canAutoDrain()`：`!stopping && !pausedAfterStop && !maintenance && 无在途 turn &&
  无 immediate reservation && 队头是 queue`。这是**提示值**：真正 drain 属运行时，核心不维护
  第二份权威队列。

### 主流程

```text
submit(guide) & busy & !stopping
  -> pending guide（对话流末尾 + 等待提示）
  -> 权威 user.message：
       delivery=guide  -> 保持 pending guide
       delivery=queue  -> 降级：同一 requestId 项回到队列面板（顺序不变）
  -> assistant.inputSeqs 引用该 seq
       -> 标记 consumed、移出 queue、进入 turn.visibleInputs、新开 workSegment
  -> run.finished 仍未消费
       -> 同一项改投 queue（原文/requestId/顺序不变）

submit(queue) & busy
  -> pending queue；assistant.inputSeqs 在 ready 后的新 turn 消费 -> origin=queue_drain

submit(queue) & idle
  -> 权威 user.message 直接开 turn（origin=user_prompt）

submitNow & busy
  -> immediate 项置队头 + stopping + guide 全部降级
  -> run.finished 确认 -> stopping=false
  -> promotionReady() 返回该项；宿主用同一 requestId 发送 -> user.message 收敛

requestStop & busy
  -> stopping=true；pending guide 全部降级 queue
  -> run.finished（cancelled/done）-> stopping=false
  -> pausedAfterStop=true（队列停摆）；用户显式发送或 resumeAutoDrain() 解除
```

### 与 zcode `interaction-behavior.md` 对齐表

| zcode 行为 | 本包实现 | 备注 |
| --- | --- | --- |
| `queue` 为默认 | `parseUserMessage` 缺省 `queue`；`submit` 缺省 queue | 一致 |
| 运行中输入进入队列面板 | pending `delivery=queue`；`assistant.inputSeqs` 消费后进 turn | 一致 |
| 队列消费边界 = session ready，而非任意 timeline 完成 | `busy` 只由 `run.finished`（及维护活动）清除；assistant 文本完成不清除 | 一致 |
| `context_compaction.started/retrying` 不自动 drain | `compactionActive → busy`；`canAutoDrain()=false` | 一致 |
| auto compact `completed` 不是 ready 边界 | `compactionActive=false` 后 turn 仍 running，busy 仍 true | 一致 |
| goal verification 进行中不 drain；target 未 complete 不消费 | verifier started → `settling + goalActive`；target 状态由服务端 guard | **客户端只投影 busy；drain 纪律由服务端执行**（客户端看不到 target 全量） |
| guide 只在 streaming、无附件、无 stop 时尝试 | `submit` 本地：idle/stopping/维护时降级 queue；权威事件若 `guide` 但无在途 turn 也按 queue 投影 | 一致（附件资格由服务端判定） |
| guide 等 tool result batch，一次一条 | 服务端不变量；客户端按 `inputSeqs` 事实投影（多条 guide 被引用则各开一段） | 与 zcode renderer 一样「只做 projection 分流」 |
| guide 临时行 + 「等待引导当前任务…」 | `queue[]` 中 `delivery=guide` 的项 | 渲染约定见 §9 |
| guide 降级回队列，同一 identity/顺序 | 同一 `requestId`（zcode `sourceCommandId`），更新 `delivery`，`InputView.downgraded=true` | 一致 |
| run 结束未消费 guide 原地改投 queue | `run.finished` → `downgradePendingGuides()` | 一致 |
| stop barrier + reservation（`startNow` 原子抢占） | `submitNow`：immediate 队头 + stopping；确认后 `promotionReady()` | **差异**：冻结的 `SurfaceCommands.send` 无 `startNow`；核心只表达 reservation，宿主用同一 requestId 发送（不产生第二条 QueueItem 投影） |
| 停止确认前禁止 drain | `stopping → canAutoDrain()=false` | 一致 |
| stop 后队列不自动发送 | `pausedAfterStop`（直到显式发送/`resumeAutoDrain()`） | 一致（比 zcode 显式化） |
| renderer 不维护第二份权威队列 | 队列完全由事件 + 本地乐观输入派生；核心无发送/调度副作用 | 一致 |
| `Cmd/Ctrl+Enter` 覆盖单条投递语义 | UI 层决策；核心暴露 `submit(..., delivery)` 与 `submitNow` | 本包不含快捷键 |

### 刻意差异清单

1. **startNow 表示**：冻结接口没有 `startNow` 命令；`submitNow` 只做状态表达与 reservation，
   真正抢占由宿主/BFF 用同一 `requestId` 发起。核心绝不自行调用网络。
2. **无墙钟定时器**：decision 超时、compaction 超时都以**事件**为准；核心不设 `setTimeout`，
   保证同一 fixture 逐字节确定。渲染层若需倒计时，用 decision 的 `deadline`/`timeoutAt` 自行展示。
3. **turnId 派生**：服务端未带 `turnId` 时派生 `t_<seq>`；显式 id 晚到以「first identity wins」
   并入，避免重挂载。
4. **workLog/actionTargets 落点**：冻结类型没有这两个字段（见 §4 差异表）。
5. **渲染窗口**：默认只保留最近 200 个 turn（zcode 全量历史）；可用 `windowTurns: 0` 关闭。
6. **target=complete guard**：客户端无法看到 goal target 全量；本包只把 verifier 活动折算成
   `busy`，**服务端 MUST 继续执行 target guard**，客户端不以 UI 状态代替调度。

## 6. 续传与幂等

- **幂等键**：引擎实例绑定一个 `sessionId`；`(sessionId, seq)` 等价于水位 `seq <= lastSeq → duplicate`。
  重复事件不改变视图（逐字节一致）。
- **gap**：`seq > lastSeq + 1` 时事件进入 4096 条缓冲（`buffered`），`needsResync()=true`；
  缺失 seq 到达后连续 flush。宿主看到 `buffered` 或连接 `open`（曾断开）时调用
  `attach` 的补拉路径：`list(after=lastSeq)` 分页 → `applyMany` → 再信任流。
- **attach 顺序**：`connecting → list(after) 分页 → stream(after) → open`；流中断 →
  `reconnecting/closed`；恢复 `open` 时**先 list 补洞再信任重连流**。
- **hydrate**：完整快照重建（排序、去重、重置投影），保留乐观输入（快照内已有同一
  `requestId` 则不重复）、保留 `stopping/paused` 客户端意图、水位不回退。
- **乐观收敛**：`submit` 以 `requestId` 建 pending；权威 `user.message` 合并同一条
  `InputView`（补 `seq`/文本/投递模式），消费后进 turn，不产生重复行。已进入日志的
  `requestId` 对 `submit` 永久幂等（迟到重试不会造出幽灵队列项；已知请求表上限 10,000）。
- **partial**：`applyPartial` 不进入 `ConversationView`；`engine.partial()` 提供最新帧；
  已提交 `assistant.message` 到达即清空。partial 不参与 `Last-Event-ID`。
- **断开期间**：pending 输入不丢；重连后事件按 seq 收敛。若用户已点 stop，`stopping`
  在 hydrate 中保留（客户端意图优先）。

## 7. AG-UI 映射（`toAgUi`）

`toAgUi(events, {threadId?, view?})` 纯函数、确定性；重复 seq 忽略；按 seq 排序。
`view` 传入时先发 `STATE_SNAPSHOT`（当前 `ConversationView`）。

| keel 事件 | AG-UI 事件 | 说明 |
| --- | --- | --- |
| （任意首事件） | `RUN_STARTED {threadId, runId}` | `runId` = payload.runId 或 `run_<seq>` |
| `user.message` | `CUSTOM keel.user.message` + Step 边界 | Step 名 `turn:<turnId|t_seq>` |
| `assistant.message` 文本 | `TEXT_MESSAGE_START/CONTENT/END` | 每个 text block 一条消息，id `msg_<seq>[_i]` |
| `assistant.message` thinking | `REASONING_START`, `REASONING_MESSAGE_START/CONTENT/END`, `REASONING_END` | id `reason_<seq>[_i]` |
| `assistant.message` toolCalls | `TOOL_CALL_START/ARGS/END` | args 一次 JSON 字符串；`parentMessageId` 指向文本消息 |
| `tool.result` | `TOOL_CALL_RESULT` | `role:"tool"`；`isError` 另发 `CUSTOM keel.tool.error` |
| `run.started` | `RUN_STARTED` | 已在 run 内则忽略 |
| `run.finished` 正常 | `STEP_FINISHED` + `RUN_FINISHED {outcome:success, result}` | `result.stopReason/turns/durationMs` |
| `run.finished` + 未答 decision | `RUN_FINISHED {outcome:{type:"interrupt",interrupts}}` | AG-UI 的 HITL 暂停形态；`interrupt.id=decisionId` |
| `run.finished` error/failed | `RUN_ERROR {message, code}` | 不再发 `RUN_FINISHED` |
| `decision` | `CUSTOM keel.decision` | 完整问题/选项/推荐/状态；开/答/超时/取消都发 |
| `compaction` | `CUSTOM keel.compaction` | 状态/摘要/tokensBefore |
| `goal.*` | `CUSTOM keel.goal` | 生命周期事实 |
| `external.event` | `CUSTOM keel.external` | |
| 未知 kind | `CUSTOM keel.event` | 前进兼容 |
| payload `state` / `patch` | `STATE_SNAPSHOT` / `STATE_DELTA` | 任意 kind 的附加状态帧 |

- 恢复（resume）：AG-UI 客户端以 `RUN_FINISHED.outcome.interrupts[].id` 作为 `decisionId`
  提交答案后开启新 run；本包不实现 run 输入协议，只负责映射。
- partial 不映射（AG-UI 消费方可直接读 `engine.partial()`）。

映射由 `test/agui.test.ts` 的精确类型序列钉住。

## 8. 性能与有界

基准：10,000 事件（2,000 个完整 turn：user → assistant(tool) → tool.result → assistant →
run.finished），`windowTurns=200`。环境：Bun 1.4.4 / Apple Silicon arm64 / 2026-11-06。

| 操作 | best | median（5 次） | 门限 |
| --- | --- | --- | --- |
| `hydrate(10k)` + `view()` | 5.6–14.6 ms | 8.8–24.9 ms | best < 50 ms |
| 逐条 `apply` 10k + `view()` | 7.5–15.1 ms | 10.3–21.1 ms | best < 100 ms |

> 门限取多次采样的 **best**（“单次投影”本身），median 一并记录以观察 GC/调度噪声；
> 共享 CI 机器上偶发的一次 >1s 暂停属于环境，不是投影复杂度。

有界策略：
- `windowTurns`（默认 200）裁剪 turn；同步清 toolCall 索引与已消费输入索引；
- `knownRequests` 上限 10,000（hydrate 乐观输入去重）；
- gap 缓冲上限 4,096；
- pending/queue 只含未消费输入；decisions 只随审批数量增长（非事件量），未做丢弃以免破坏审计。

## 9. NSH 渲染器使用示例

```ts
import { ChatSurfaceEngine } from "@keel/chat-surface";
import type { EventSource, SurfaceCommands } from "@keel/chat-surface";

const engine = new ChatSurfaceEngine({ sessionId, windowTurns: 200 });
const source: EventSource = { list, stream };          // 宿主实现（HTTP/SSE 或 BFF）
const commands: SurfaceCommands = { send, stop, answerDecision };

const render = () => {
  const view = engine.view();
  // view.turns[].workSegments[].toolCalls -> 工具卡
  // view.queue.filter(q => q.delivery === "guide") -> 对话流末尾的「等待引导当前任务…」行
  // view.queue.filter(q => q.delivery === "queue")  -> 队列面板
  // view.queue[0]?.immediate                       -> 「立即发送」reservation
  // view.decisions                                 -> 审批卡
  // view.busy / view.stopping / view.connection    -> 停止按钮/连接状态
};
const off = engine.subscribe(render);
const detach = await engine.attach(source, { partials: true });

// 发送（queue 默认；运行中切 guide）
await commands.send(text, { requestId: crypto.randomUUID(), delivery: mode });

// 「立即发送」：引擎表达 reservation，宿主用同一 requestId 发送
const input = engine.submitNow(text, requestId);
engine.requestStop();
void commands.stop();
// run.finished 确认后：
const promote = engine.promotionReady();
if (promote) await commands.send(promote.text, { requestId: promote.requestId, delivery: "queue" });

// 审批
const ok = engine.answerDecision(decisionId, optionId);
if (ok) await commands.answerDecision(decisionId, optionId);

// 卸载
off();
detach();
engine.disconnect();
```
