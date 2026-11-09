// @keel/chat-surface — 框架无关的会话投影核心（零 DOM）。
//
// 输入：keel 事件（已提交事件 + 可选 partial 帧 + 本地乐观输入）。
// 输出：ConversationView（Turn[]），供任意渲染器（NSH / 将来的 web 渲染器）消费。
// 契约单源：openspec/changes/keel-chat-surface/specs/chat-surface/spec.md
// 与 docs/CHAT-SURFACE.md（事件映射表）。

/** 服务端事件（wire 形状）：GET /v1/sessions/{id}/events 的 events[] 元素。 */
export interface WireEvent {
  seq: number;
  kind: string; // user.message | assistant.message | tool.result | compaction | run.finished | decision | external.event
  payload: unknown;
}

/** 投递模式（与 protocol-v1 一致）。 */
export type Delivery = "queue" | "guide";

/** 用户输入（乐观或权威）。 */
export interface InputView {
  /** 权威事件 seq；乐观输入为 undefined。 */
  seq?: number;
  requestId: string;
  text: string;
  delivery: Delivery;
  /** 服务端把 guide 降级为 queue 时为 true。 */
  downgraded?: boolean;
  /** 已被某个 assistant 的 inputSeqs 消费。 */
  consumed?: boolean;
  createdAt: number;
}

/** 一个 product turn 内的视觉工作段（原始输入 / 每条被消费的 guide）。 */
export interface WorkSegment {
  segmentId: string;
  triggerInput?: InputView;
  /** 该段首/末事件自带的时间戳（epoch ms）。没有事件时间戳的段为空——
   * 渲染器的计时只能来自账本事实或本地时钟，绝不写回。 */
  startedAt?: number;
  endedAt?: number;
  /** 该段内的工具调用与结果（按事件顺序）。 */
  toolCalls: ToolCallView[];
  /** 该段的 assistant 文本（可能为空：纯工具段）。 */
  assistantText?: string;
  assistantThinking?: string;
}

export interface ToolCallView {
  toolCallId: string;
  name: string;
  args?: unknown;
  result?: unknown;
  error?: string;
  startedAt?: number;
  finishedAt?: number;
}

export type TurnOrigin =
  | "user_prompt"
  | "queue_drain"
  | "turn_steer"
  | "goal_continuation"
  | "manual_compact";

export type TurnStatus = "running" | "settling" | "completed" | "interrupted" | "failed";

export interface RunSummary {
  turns: number;
  tokensIn: number;
  tokensOut: number;
  stopReason: string;
  durationMs: number;
}

export interface TurnView {
  turnId: string;
  /** 仅展示用。 */
  ordinal: number;
  origin: TurnOrigin;
  status: TurnStatus;
  visibleInputs: InputView[];
  workSegments: WorkSegment[];
  assistantOutput?: string;
  /** 该回合失败时的原因（账本里 assistant.message / run.finished 的
   * errorMessage）。渲染器据此出错误横幅；重试是对同一输入的再次提交。 */
  error?: string;
  postTurnEvents: Array<{ kind: string; seq: number; note?: string }>;
  runSummary?: RunSummary;
}

export interface DecisionView {
  decisionId: string;
  seq: number;
  question: string;
  options: Array<{ optionId: string; label: string; description?: string }>;
  recommendation?: string;
  status: "open" | "answered" | "timeout" | "cancelled";
  answer?: string;
}

export interface QueueItemView {
  requestId: string;
  text: string;
  delivery: Delivery;
  /** 用户显式「立即发送」抢占。 */
  immediate?: boolean;
}

export interface ConnectionState {
  status: "connecting" | "open" | "reconnecting" | "closed";
  lastSeq: number;
}

export interface ConversationView {
  sessionId: string;
  turns: TurnView[];
  decisions: DecisionView[];
  queue: QueueItemView[];
  /** 运行中（session 未 ready）时 true。 */
  busy: boolean;
  /** 用户已请求停止、等待权威确认。 */
  stopping: boolean;
  connection: ConnectionState;
}

/** 渲染器可调用的命令面（由宿主注入实现：BFF/直连均可）。 */
export interface SurfaceCommands {
  send(text: string, opts: { requestId: string; delivery: Delivery }): Promise<void>;
  stop(): Promise<void>;
  answerDecision(decisionId: string, answer: string): Promise<void>;
  upload?(file: Blob): Promise<{ fileId: string }>;
}

/** 事件源抽象：HTTP/SSE、测试 fixture、或 BFF 代理。 */
export interface EventSource {
  /** 拉取快照（after 之后的事件）。 */
  list(after: number, limit?: number): Promise<WireEvent[]>;
  /** 订阅增量；返回取消函数。partial 帧可选。 */
  stream(opts: {
    after: number;
    partials?: boolean;
    onEvent: (e: WireEvent) => void;
    onPartial?: (p: { text: string; turnId?: string }) => void;
    onState: (s: ConnectionState["status"]) => void;
  }): () => void;
}

/** 核心入口（实现见 engine.ts）。 */
export interface SurfaceEngine {
  view(): ConversationView;
  /** 乐观输入（发送后立即上屏；权威事件到达后收敛）。 */
  submit(text: string, delivery: Delivery, requestId: string): void;
  /** 应用一条权威事件（幂等）。 */
  apply(e: WireEvent): void;
  /** 应用 partial（不落状态，只给渲染器）。 */
  applyPartial(p: { text: string; turnId?: string }): void;
  /** 从快照重建。 */
  hydrate(events: WireEvent[]): void;
  /** 标记停止请求（等待权威确认）。 */
  requestStop(): void;
  subscribe(fn: () => void): () => void;
}
