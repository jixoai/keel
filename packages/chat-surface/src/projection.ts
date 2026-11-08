// Conversation projection: keel committed events -> ConversationView.
//
// This file is the queue/guide state machine described in
// docs/CHAT-SURFACE.md. It is pure: no timers, no fetch, no DOM, no clock
// except the injectable `now` used for optimistic inputs. The same event
// fixture always produces the same view (byte-identical JSON).

import type {
  ConnectionState,
  ConversationView,
  DecisionView,
  Delivery,
  InputView,
  QueueItemView,
  ToolCallView,
  TurnOrigin,
  TurnStatus,
  TurnView,
  WireEvent,
  WorkSegment,
} from "./types";
import {
  asRecord,
  outcomeForStopReason,
  parseAssistantMessage,
  parseCompaction,
  parseDecision,
  parseRunFinished,
  parseToolResult,
  parseUserMessage,
  str,
} from "./events";

export const DEFAULT_WINDOW_TURNS = 200;
const KNOWN_REQUESTS_LIMIT = 10_000;
const GAP_BUFFER_LIMIT = 4_096;
const NOTE_LIMIT = 500;

export type ApplyResult = "applied" | "duplicate" | "buffered";

export interface ProjectionOptions {
  sessionId: string;
  /** Render window in turns (0 = unbounded). Default 200. */
  windowTurns?: number;
  /** Clock for optimistic createdAt. Default Date.now. */
  now?: () => number;
  /** Diagnostics hook for malformed payloads (never throws out of apply). */
  onError?: (event: WireEvent, error: unknown) => void;
}

interface PendingInput {
  view: InputView;
  /** What the client asked for (for downgrade detection). */
  requestedDelivery: Delivery;
  /** Explicit user "send now" reservation (queue head promotion). */
  immediate: boolean;
  /** Submitted while a run was active (origin hint: queue_drain). */
  wasBusyAtSubmit: boolean;
}

interface ToolCallEntry {
  turn: TurnView;
  segment: WorkSegment;
  call: ToolCallView;
}

function isOpen(status: TurnStatus): boolean {
  return status === "running" || status === "settling";
}

function terminalStatus(stopReason: string): TurnStatus {
  const outcome = outcomeForStopReason(stopReason);
  return outcome === "completed" ? "completed" : outcome === "interrupted" ? "interrupted" : "failed";
}

function payloadNote(payload: unknown): string | undefined {
  const p = asRecord(payload);
  const text =
    str(p.note) ??
    str(p.summary) ??
    str(p.status) ??
    str(p.phase) ??
    str(p.type) ??
    str(p.message) ??
    str(p.reason);
  if (text !== undefined) return text.slice(0, NOTE_LIMIT);
  try {
    const json = JSON.stringify(payload);
    return json === undefined ? undefined : json.slice(0, NOTE_LIMIT);
  } catch {
    return undefined;
  }
}

export class ConversationProjection {
  readonly sessionId: string;
  private readonly windowTurns: number;
  private readonly now: () => number;
  private readonly onError?: (event: WireEvent, error: unknown) => void;

  private turns: TurnView[] = [];
  private readonly turnById = new Map<string, TurnView>();
  private readonly toolCallIndex = new Map<string, ToolCallEntry>();
  private readonly inputsBySeq = new Map<number, InputView>();
  private readonly inputByRequest = new Map<string, InputView>();
  private readonly pending = new Map<string, PendingInput>();
  private pendingOrder: string[] = [];
  private decisions: DecisionView[] = [];
  private readonly decisionById = new Map<string, DecisionView>();

  private lastSeqValue = 0;
  private ordinal = 0;

  private stoppingValue = false;
  private stopRequestedByUser = false;
  private pausedAfterStopValue = false;
  private compactionActive = false;
  private goalActive = false;

  private connectionStatus: ConnectionState["status"] = "closed";

  private gapBuffer = new Map<number, WireEvent>();
  private gapDetected = false;
  private replaying = false;

  private readonly knownRequests = new Set<string>();
  private knownRequestOrder: string[] = [];

  constructor(opts: ProjectionOptions) {
    if (!opts.sessionId) throw new Error("chat-surface: sessionId is required");
    this.sessionId = opts.sessionId;
    this.windowTurns = opts.windowTurns ?? DEFAULT_WINDOW_TURNS;
    this.now = opts.now ?? (() => Date.now());
    this.onError = opts.onError;
  }

  get lastSeq(): number {
    return this.lastSeqValue;
  }

  get stopping(): boolean {
    return this.stoppingValue;
  }

  get pausedAfterStop(): boolean {
    return this.pausedAfterStopValue;
  }

  // ---------------------------------------------------------------------
  // Apply
  // ---------------------------------------------------------------------

  /**
   * Applies one committed event. Idempotent by (sessionId, seq): the engine is
   * scoped to one session, the watermark is the seq. Events that arrive after a
   * gap are buffered until the gap is filled (see needsResync()).
   */
  apply(event: WireEvent): ApplyResult {
    const seq = event?.seq;
    if (typeof seq !== "number" || !Number.isFinite(seq) || seq <= 0) return "duplicate";
    if (seq <= this.lastSeqValue) return "duplicate";
    if (this.lastSeqValue > 0 && seq > this.lastSeqValue + 1) {
      if (this.gapBuffer.size < GAP_BUFFER_LIMIT) this.gapBuffer.set(seq, event);
      this.gapDetected = true;
      return "buffered";
    }
    this.applyOrdered(event);

    // Flush whatever became contiguous.
    for (;;) {
      const next = this.gapBuffer.get(this.lastSeqValue + 1);
      if (!next) break;
      this.gapBuffer.delete(this.lastSeqValue + 1);
      this.applyOrdered(next);
    }
    if (this.gapBuffer.size === 0) this.gapDetected = false;
    return "applied";
  }

  /** Applies a batch in seq order. Returns the number of newly applied events. */
  applyMany(events: readonly WireEvent[]): number {
    if (events.length === 0) return 0;
    const ordered = this.isAscending(events) ? events : [...events].sort((a, b) => a.seq - b.seq);
    let applied = 0;
    for (const event of ordered) {
      if (this.apply(event) === "applied") applied += 1;
    }
    return applied;
  }

  /** True when a gap was observed and the host should list events after lastSeq. */
  needsResync(): boolean {
    return this.gapDetected;
  }

  // ---------------------------------------------------------------------
  // Optimistic local input
  // ---------------------------------------------------------------------

  submit(text: string, delivery: Delivery, requestId: string, opts?: { immediate?: boolean }): InputView {
    if (!requestId) throw new Error("chat-surface: requestId is required");
    // A requestId that already reached the log is authoritative even after it
    // was consumed: a late retry must not create a phantom queue item.
    const known = this.inputByRequest.get(requestId);
    if (known) return known;
    const existing = this.pending.get(requestId);
    if (existing) return existing.view;

    const busy = this.isBusy();
    const immediate = opts?.immediate === true && busy;
    // A guide only exists while a stream is in flight. If the session is idle
    // or stopping, the server would degrade it; do the same locally so the UI
    // never shows a phantom "waiting for guide current task".
    const effective: Delivery = delivery === "guide" && (busy && !this.stoppingValue) ? "guide" : "queue";
    this.pausedAfterStopValue = false;

    const view: InputView = {
      requestId,
      text,
      delivery: effective,
      downgraded: delivery === "guide" && effective === "queue" ? true : undefined,
      consumed: false,
      createdAt: this.now(),
    };
    this.addPending({
      view,
      requestedDelivery: delivery,
      immediate,
      wasBusyAtSubmit: busy,
    });
    if (immediate) {
      // A "send now" reservation preempts the queue: it becomes the head so
      // display order and promotion order agree, and no other item can drain
      // while it waits for the stop barrier.
      this.movePendingToFront(requestId);
      this.stoppingValue = true;
      this.stopRequestedByUser = true;
      this.downgradePendingGuides();
    }
    return view;
  }

  // ---------------------------------------------------------------------
  // Stop
  // ---------------------------------------------------------------------

  /** Marks a user stop request; the barrier lifts only on run.finished. */
  requestStop(): boolean {
    if (!this.isBusy()) return false;
    this.stoppingValue = true;
    this.stopRequestedByUser = true;
    // Stop invalidates guide eligibility: pending guides fall back to queue
    // with their identity and order unchanged.
    this.downgradePendingGuides();
    return true;
  }

  /** Clears the post-stop auto-drain pause (explicit user resume). */
  resumeAutoDrain(): void {
    this.pausedAfterStopValue = false;
  }

  // ---------------------------------------------------------------------
  // Decisions
  // ---------------------------------------------------------------------

  /** Optimistically marks a decision answered; first answer wins. Host POSTs separately. */
  answerDecision(decisionId: string, answer: string): boolean {
    const row = this.decisionById.get(decisionId);
    if (!row || row.status !== "open") return false;
    row.status = "answered";
    row.answer = answer;
    return true;
  }

  // ---------------------------------------------------------------------
  // Connection
  // ---------------------------------------------------------------------

  setConnection(status: ConnectionState["status"]): void {
    this.connectionStatus = status;
  }

  // ---------------------------------------------------------------------
  // Hydrate
  // ---------------------------------------------------------------------

  /**
   * Rebuilds the projection from a complete snapshot (events from seq 1, or at
   * least a self-consistent window). Optimistic inputs survive when the
   * snapshot does not already contain their requestId. The seq watermark never
   * moves backwards.
   */
  hydrate(events: readonly WireEvent[]): void {
    const optimistic: PendingInput[] = [];
    const previousLast = this.lastSeqValue;
    for (const requestId of this.pendingOrder) {
      const pending = this.pending.get(requestId);
      if (pending && pending.view.seq === undefined) {
        optimistic.push({
          view: { ...pending.view },
          requestedDelivery: pending.requestedDelivery,
          immediate: pending.immediate,
          wasBusyAtSubmit: pending.wasBusyAtSubmit,
        });
      }
    }

    this.resetProjection();
    const ordered = [...events].sort((a, b) => a.seq - b.seq);
    this.replaying = true;
    try {
      for (const event of ordered) {
        // Duplicate seqs in a snapshot are ignored, exactly like apply().
        if (event.seq <= this.lastSeqValue) continue;
        this.applyOrdered(event);
      }
    } finally {
      this.replaying = false;
    }
    this.lastSeqValue = Math.max(previousLast, this.lastSeqValue);

    for (const entry of optimistic) {
      if (!this.knownRequests.has(entry.view.requestId) && !this.pending.has(entry.view.requestId)) {
        this.pending.set(entry.view.requestId, entry);
        this.pendingOrder.push(entry.view.requestId);
      }
    }
  }

  private resetProjection(): void {
    this.turns = [];
    this.turnById.clear();
    this.toolCallIndex.clear();
    this.inputsBySeq.clear();
    this.inputByRequest.clear();
    this.pending.clear();
    this.pendingOrder = [];
    this.decisions = [];
    this.decisionById.clear();
    this.lastSeqValue = 0;
    this.ordinal = 0;
    this.compactionActive = false;
    this.goalActive = false;
    this.gapBuffer.clear();
    this.gapDetected = false;
    // stopping / pausedAfterStop / connection are client intents: preserved.
  }

  // ---------------------------------------------------------------------
  // View
  // ---------------------------------------------------------------------

  view(): ConversationView {
    const queue: QueueItemView[] = [];
    for (const requestId of this.pendingOrder) {
      const pending = this.pending.get(requestId);
      if (!pending) continue;
      const item: QueueItemView = {
        requestId,
        text: pending.view.text,
        delivery: pending.view.delivery,
      };
      if (pending.immediate) item.immediate = true;
      queue.push(item);
    }
    return {
      sessionId: this.sessionId,
      turns: this.turns,
      decisions: this.decisions,
      queue,
      busy: this.isBusy(),
      stopping: this.stoppingValue,
      connection: { status: this.connectionStatus, lastSeq: this.lastSeqValue },
    };
  }

  isBusy(): boolean {
    if (this.stoppingValue || this.compactionActive || this.goalActive) return true;
    for (let i = this.turns.length - 1; i >= 0; i -= 1) {
      const turn = this.turns[i];
      if (turn && isOpen(turn.status)) return true;
    }
    return false;
  }

  /** True when the runtime may automatically consume the queue head. */
  canAutoDrain(): boolean {
    if (this.stoppingValue || this.pausedAfterStopValue) return false;
    if (this.compactionActive || this.goalActive) return false;
    if (this.hasOpenTurn()) return false;
    // An immediate reservation preempts every normal queue item.
    if (this.hasImmediateReservation()) return false;
    const headId = this.pendingOrder[0];
    if (!headId) return false;
    const head = this.pending.get(headId);
    if (!head) return false;
    return head.view.delivery === "queue";
  }

  /** The explicit "send now" reservation, ready to promote after the barrier lifts. */
  promotionReady(): QueueItemView | null {
    if (this.stoppingValue || this.hasOpenTurn() || this.compactionActive || this.goalActive) return null;
    for (const requestId of this.pendingOrder) {
      const pending = this.pending.get(requestId);
      if (!pending || !pending.immediate) continue;
      return { requestId, text: pending.view.text, delivery: pending.view.delivery, immediate: true };
    }
    return null;
  }

  // ---------------------------------------------------------------------
  // Ordered apply
  // ---------------------------------------------------------------------

  private applyOrdered(event: WireEvent): void {
    this.lastSeqValue = event.seq;
    try {
      switch (event.kind) {
        case "user.message":
          this.onUserMessage(event);
          return;
        case "assistant.message":
          this.onAssistantMessage(event);
          return;
        case "tool.result":
          this.onToolResult(event);
          return;
        case "compaction":
          this.onCompaction(event);
          return;
        case "run.started":
          this.onRunStarted(event);
          return;
        case "run.finished":
          this.onRunFinished(event);
          return;
        case "decision":
        case "decision.opened":
        case "decision.answered":
        case "decision.timeout":
        case "decision.cancelled":
          this.onDecision(event);
          return;
        case "external.event":
          this.onExternal(event);
          return;
        default:
          if (event.kind.startsWith("goal")) this.onGoal(event);
          else if (event.kind.startsWith("decision")) this.onDecision(event);
          else this.onUnknown(event);
      }
    } catch (error) {
      this.onError?.(event, error);
    }
  }

  // ---------------------------------------------------------------------
  // user.message
  // ---------------------------------------------------------------------

  private onUserMessage(event: WireEvent): void {
    const parsed = parseUserMessage(event.payload);
    const requestId = parsed.requestId ?? `req_${event.seq}`;
    const timestamp = parsed.timestamp ?? event.seq;
    const pending = this.pending.get(requestId);
    const requested = pending?.requestedDelivery ?? parsed.delivery;

    let delivery: Delivery = parsed.delivery;
    if (delivery === "guide" && (!this.hasOpenTurn() || this.stoppingValue)) {
      // The server only admits a guide into an in-flight turn; a guide seen
      // while idle/stopping is a queue input for projection purposes.
      delivery = "queue";
    }
    const downgraded = requested === "guide" && delivery === "queue";

    let view = pending?.view;
    if (view) {
      view.seq = event.seq;
      view.text = parsed.text;
      view.delivery = delivery;
      if (downgraded) view.downgraded = true;
      view.createdAt = timestamp;
    } else {
      view = {
        seq: event.seq,
        requestId,
        text: parsed.text,
        delivery,
        downgraded: downgraded ? true : undefined,
        consumed: false,
        createdAt: timestamp,
      };
      this.pending.set(requestId, {
        view,
        requestedDelivery: requested,
        immediate: false,
        wasBusyAtSubmit: false,
      });
      this.pendingOrder.push(requestId);
    }
    this.inputsBySeq.set(event.seq, view);
    this.inputByRequest.set(requestId, view);
    this.rememberRequest(requestId);
    if (!this.replaying) this.pausedAfterStopValue = false;

    // A user prompt admitted while the session is ready starts its turn
    // immediately; while a run is active it stays in the queue/pending panel
    // until an assistant `inputSeqs` consumes it.
    if (!this.hasOpenTurn() && !this.compactionActive && !this.goalActive && delivery === "queue") {
      const origin: TurnOrigin = parsed.origin ?? (pending?.wasBusyAtSubmit ? "queue_drain" : "user_prompt");
      const turnId = parsed.turnId ?? `t_${event.seq}`;
      this.createTurn(turnId, origin, [view]);
      this.removePending(requestId);
    }
  }

  // ---------------------------------------------------------------------
  // assistant.message
  // ---------------------------------------------------------------------

  private onAssistantMessage(event: WireEvent): void {
    const parsed = parseAssistantMessage(event.payload);
    const inputs: InputView[] = [];
    for (const seq of parsed.inputSeqs) {
      const input = this.inputsBySeq.get(seq);
      if (input) inputs.push(input);
    }
    const busyHints = inputs.map((input) => this.pending.get(input.requestId)?.wasBusyAtSubmit === true);

    const turn = this.resolveAssistantTurn(event.seq, parsed, inputs, busyHints);

    for (const input of inputs) {
      if (!input.consumed) {
        input.consumed = true;
        this.removePending(input.requestId);
        if (!turn.visibleInputs.includes(input)) {
          const opensSegment = input.delivery === "guide" && turn.workSegments.length > 0;
          turn.visibleInputs.push(input);
          if (opensSegment) this.addSegment(turn, input);
          else if (turn.workSegments.length === 0) this.addSegment(turn, input);
        }
      } else {
        // A tool-loop assistant repeats the input seqs; keep the queue clean.
        this.removePending(input.requestId);
      }
    }

    const segment =
      turn.workSegments[turn.workSegments.length - 1] ??
      this.addSegment(turn, inputs[0]);

    if (parsed.text !== "") {
      segment.assistantText = segment.assistantText
        ? `${segment.assistantText}\n\n${parsed.text}`
        : parsed.text;
    }
    if (parsed.thinking !== "") {
      segment.assistantThinking = segment.assistantThinking
        ? `${segment.assistantThinking}\n\n${parsed.thinking}`
        : parsed.thinking;
    }
    turn.assistantOutput = recomputeAssistantOutput(turn);

    for (const call of parsed.toolCalls) {
      if (this.toolCallIndex.has(call.toolCallId)) continue;
      const view: ToolCallView = {
        toolCallId: call.toolCallId,
        name: call.name,
      };
      if (parsed.timestamp !== undefined) view.startedAt = parsed.timestamp;
      if (call.args !== undefined) view.args = call.args;
      segment.toolCalls.push(view);
      this.toolCallIndex.set(call.toolCallId, { turn, segment, call: view });
    }

    if (isOpen(turn.status)) {
      if (parsed.stopReason === "error") turn.status = "failed";
      else if (parsed.stopReason === "aborted") turn.status = "interrupted";
    }
  }

  private resolveAssistantTurn(
    seq: number,
    parsed: ReturnType<typeof parseAssistantMessage>,
    inputs: InputView[],
    busyHints: boolean[],
  ): TurnView {
    const explicit = parsed.turnId;
    // A new turn only owns the inputs that are not already part of an older
    // turn. inputSeqs is the whole model transcript, so a queue-drain turn
    // repeats the previous turn's input seqs; starting from those would attach
    // the new answer to the completed turn.
    const fresh: InputView[] = [];
    const freshBusy: boolean[] = [];
    for (let i = 0; i < inputs.length; i += 1) {
      if (inputs[i]!.consumed) continue;
      fresh.push(inputs[i]!);
      freshBusy.push(busyHints[i] === true);
    }

    if (explicit) {
      const existing = this.turnById.get(explicit);
      if (existing) return existing;
      const newest = this.newestOpenTurn();
      if (newest && inputs.length > 0 && inputs.every((input) => this.belongsToTurn(newest, input))) {
        // First identity wins: an explicit turnId that arrives inside an open
        // turn for the same inputs is an alias, not a second turn.
        this.turnById.set(explicit, newest);
        return newest;
      }
      return this.createTurn(explicit, parsed.origin ?? this.inferOrigin(fresh, freshBusy), fresh);
    }

    const newest = this.newestOpenTurn();
    if (newest) return newest;
    if (fresh.length > 0) {
      const first = fresh[0]!;
      const turnId = first.seq !== undefined ? `t_${first.seq}` : `t_${seq}`;
      const existing = this.turnById.get(turnId);
      if (existing && isOpen(existing.status)) return existing;
      return this.createTurn(turnId, parsed.origin ?? this.inferOrigin(fresh, freshBusy), fresh);
    }
    return this.createTurn(
      `t_${seq}`,
      parsed.origin ?? (this.turns.length > 0 ? "goal_continuation" : "user_prompt"),
      [],
    );
  }

  private belongsToTurn(turn: TurnView, input: InputView): boolean {
    if (turn.visibleInputs.includes(input)) return true;
    return input.delivery === "guide" && !input.consumed;
  }

  private inferOrigin(inputs: InputView[], busyHints: boolean[]): TurnOrigin {
    if (inputs.some((input) => input.delivery === "guide")) return "turn_steer";
    if (busyHints.some(Boolean)) return "queue_drain";
    return "user_prompt";
  }

  // ---------------------------------------------------------------------
  // tool.result
  // ---------------------------------------------------------------------

  private onToolResult(event: WireEvent): void {
    const parsed = parseToolResult(event.payload);
    if (!parsed.toolCallId) return;
    const timestamp = parsed.timestamp;
    const entry = this.toolCallIndex.get(parsed.toolCallId);
    if (entry) {
      if (parsed.result !== undefined) entry.call.result = parsed.result;
      if (parsed.error !== undefined) entry.call.error = parsed.error;
      if (timestamp !== undefined) {
        entry.call.finishedAt = timestamp;
        if (entry.call.startedAt === undefined && parsed.durationMs !== undefined) {
          entry.call.startedAt = timestamp - parsed.durationMs;
        }
      }
      return;
    }
    // Missing call (truncated window / foreign log): keep the result visible.
    const turn = this.newestOpenTurn() ?? this.turns[this.turns.length - 1];
    if (!turn) return;
    const segment = turn.workSegments[turn.workSegments.length - 1] ?? this.addSegment(turn, undefined);
    const view: ToolCallView = {
      toolCallId: parsed.toolCallId,
      name: parsed.toolName ?? "tool",
    };
    if (timestamp !== undefined) {
      view.finishedAt = timestamp;
      if (parsed.durationMs !== undefined) view.startedAt = timestamp - parsed.durationMs;
    }
    if (parsed.result !== undefined) view.result = parsed.result;
    if (parsed.error !== undefined) view.error = parsed.error;
    segment.toolCalls.push(view);
    this.toolCallIndex.set(parsed.toolCallId, { turn, segment, call: view });
  }

  // ---------------------------------------------------------------------
  // compaction
  // ---------------------------------------------------------------------

  private onCompaction(event: WireEvent): void {
    const parsed = parseCompaction(event.payload);
    this.compactionActive = parsed.active;

    let turn = this.newestOpenTurn();
    if (!turn && parsed.manual) {
      turn = this.createTurn(`t_${event.seq}`, "manual_compact", []);
      turn.status = parsed.active ? "running" : "completed";
    }
    if (turn) {
      this.pushPostTurn(turn, "compaction", event.seq, parsed.summary || parsed.status);
      if (turn.origin === "manual_compact" && !parsed.active) turn.status = "completed";
    }
    if (!parsed.active) this.confirmMaintenanceStopIfIdle();
  }

  // ---------------------------------------------------------------------
  // goal.* (post-turn verifier / continuation lifecycle)
  // ---------------------------------------------------------------------

  private onGoal(event: WireEvent): void {
    const p = asRecord(event.payload);
    const raw = (str(p.status) ?? str(p.phase) ?? "").toLowerCase();
    let active: boolean;
    if (raw !== "") {
      active = raw === "started" || raw === "running" || raw === "retrying";
    } else if (/(completed|failed|cancelled|canceled|passed)$/.test(event.kind)) {
      active = false;
    } else {
      active = true;
    }
    this.goalActive = active;

    const turn = this.newestOpenTurn() ?? this.turns[this.turns.length - 1];
    if (!turn) return;
    const note = payloadNote(event.payload);
    this.pushPostTurn(turn, event.kind, event.seq, note);
    if (active && turn.status === "running" && turn.assistantOutput !== undefined) {
      turn.status = "settling";
    }
    if (!active) this.confirmMaintenanceStopIfIdle();
  }

  /** A stop aimed at a maintenance activity (compaction/verifier) has no run.finished. */
  private confirmMaintenanceStopIfIdle(): void {
    if (this.replaying) return;
    if (!this.stoppingValue || this.hasOpenTurn() || this.compactionActive || this.goalActive) return;
    const userStop = this.stopRequestedByUser;
    this.stoppingValue = false;
    this.stopRequestedByUser = false;
    if (userStop) this.pausedAfterStopValue = true;
    this.downgradePendingGuides();
  }

  // ---------------------------------------------------------------------
  // run lifecycle
  // ---------------------------------------------------------------------

  private onRunStarted(event: WireEvent): void {
    const p = asRecord(event.payload);
    const turnId = str(p.turnId);
    if (turnId) {
      const existing = this.turnById.get(turnId);
      if (!existing) return; // the assistant message will materialize the turn
      if (existing.status === "interrupted" || existing.status === "failed") return;
      existing.status = "running";
      return;
    }
    const newest = this.newestOpenTurn();
    if (newest && newest.status === "settling") newest.status = "running";
  }

  private onRunFinished(event: WireEvent): void {
    const parsed = parseRunFinished(event.payload);
    const status = terminalStatus(parsed.stopReason);

    let target: TurnView | undefined;
    if (parsed.turnId) target = this.turnById.get(parsed.turnId);
    target ??= this.newestOpenTurn();
    target ??= this.turns[this.turns.length - 1];

    let openCount = 0;
    for (const turn of this.turns) {
      if (isOpen(turn.status)) {
        turn.status = status;
        openCount += 1;
      }
    }

    if (target) {
      target.runSummary = {
        turns: parsed.turns ?? Math.max(openCount, 1),
        tokensIn: parsed.tokensIn,
        tokensOut: parsed.tokensOut,
        stopReason: parsed.stopReason,
        durationMs: parsed.durationMs,
      };
    }

    const hadImmediateReservation = this.pendingOrder.some((requestId) => this.pending.get(requestId)?.immediate === true);
    if (!this.replaying) {
      const userStop = this.stopRequestedByUser;
      this.stoppingValue = false;
      this.stopRequestedByUser = false;
      // A user stop keeps the queue parked until an explicit send/resume; an
      // "immediate" reservation is itself the explicit follow-up.
      if (userStop && !hadImmediateReservation) this.pausedAfterStopValue = true;
    } else {
      void hadImmediateReservation;
    }

    // A turn that ends without consuming a pending guide falls back to queue
    // (same item, same order, same requestId).
    this.downgradePendingGuides();
  }

  // ---------------------------------------------------------------------
  // decision
  // ---------------------------------------------------------------------

  private onDecision(event: WireEvent): void {
    const fallback = event.kind === "decision.answered"
      ? "answered"
      : event.kind === "decision.timeout"
        ? "timeout"
        : event.kind === "decision.cancelled"
          ? "cancelled"
          : "open";
    const parsed = parseDecision(event.payload, fallback);
    if (!parsed.decisionId) return;
    const existing = this.decisionById.get(parsed.decisionId);
    if (!existing) {
      const row: DecisionView = {
        decisionId: parsed.decisionId,
        seq: event.seq,
        question: parsed.question,
        options: parsed.options,
        status: parsed.status,
        answer: parsed.answer,
      };
      if (parsed.recommendation !== undefined) row.recommendation = parsed.recommendation;
      this.decisions.push(row);
      this.decisionById.set(row.decisionId, row);
      return;
    }

    // Same decisionId => same row. Terminal statuses always win; the first
    // answer wins over a second answer.
    if (parsed.status === "answered") {
      if (existing.status === "open") {
        existing.status = "answered";
        existing.answer = parsed.answer;
      }
    } else if (parsed.status !== "open") {
      existing.status = parsed.status;
      if (parsed.answer !== undefined) existing.answer = parsed.answer;
    } else if (existing.status === "open") {
      if (parsed.question !== "") existing.question = parsed.question;
      if (parsed.options.length > 0) existing.options = parsed.options;
      if (parsed.recommendation !== undefined) existing.recommendation = parsed.recommendation;
    }
  }

  // ---------------------------------------------------------------------
  // external / unknown
  // ---------------------------------------------------------------------

  private onExternal(event: WireEvent): void {
    const turn = this.newestOpenTurn() ?? this.turns[this.turns.length - 1];
    if (!turn) return;
    this.pushPostTurn(turn, event.kind, event.seq, payloadNote(event.payload));
  }

  private onUnknown(event: WireEvent): void {
    const turn = this.newestOpenTurn() ?? this.turns[this.turns.length - 1];
    if (!turn) return;
    const p = asRecord(event.payload);
    if (p.state !== undefined) {
      this.pushPostTurn(turn, event.kind, event.seq, "state");
      return;
    }
    this.pushPostTurn(turn, event.kind, event.seq, payloadNote(event.payload));
  }

  // ---------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------

  private createTurn(turnId: string, origin: TurnOrigin, inputs: InputView[]): TurnView {
    const existing = this.turnById.get(turnId);
    if (existing) {
      for (const input of inputs) {
        if (!existing.visibleInputs.includes(input)) existing.visibleInputs.push(input);
      }
      return existing;
    }
    const turn: TurnView = {
      turnId,
      ordinal: (this.ordinal += 1),
      origin,
      status: "running",
      visibleInputs: [...inputs],
      workSegments: [],
      postTurnEvents: [],
    };
    turn.workSegments.push({
      segmentId: `${turnId}:s0`,
      triggerInput: inputs[0],
      toolCalls: [],
    });
    this.turns.push(turn);
    this.turnById.set(turnId, turn);
    this.trimTurns();
    return turn;
  }

  private addSegment(turn: TurnView, trigger: InputView | undefined): WorkSegment {
    const segment: WorkSegment = {
      segmentId: `${turn.turnId}:s${turn.workSegments.length}`,
      toolCalls: [],
    };
    if (trigger) segment.triggerInput = trigger;
    turn.workSegments.push(segment);
    return segment;
  }

  private pushPostTurn(turn: TurnView, kind: string, seq: number, note?: string): void {
    const entry: { kind: string; seq: number; note?: string } = { kind, seq };
    if (note !== undefined) entry.note = note;
    turn.postTurnEvents.push(entry);
  }

  private trimTurns(): void {
    if (this.windowTurns <= 0) return;
    if (this.turns.length <= this.windowTurns) return;
    const removeCount = this.turns.length - this.windowTurns;
    const removed = this.turns.splice(0, removeCount);
    for (const turn of removed) {
      if (this.turnById.get(turn.turnId) === turn) this.turnById.delete(turn.turnId);
      for (const input of turn.visibleInputs) {
        if (input.consumed && input.seq !== undefined && this.inputsBySeq.get(input.seq) === input) {
          this.inputsBySeq.delete(input.seq);
        }
      }
      for (const segment of turn.workSegments) {
        for (const call of segment.toolCalls) {
          const entry = this.toolCallIndex.get(call.toolCallId);
          if (entry && entry.turn === turn) this.toolCallIndex.delete(call.toolCallId);
        }
      }
    }
  }

  private newestOpenTurn(): TurnView | undefined {
    for (let i = this.turns.length - 1; i >= 0; i -= 1) {
      const turn = this.turns[i];
      if (turn && isOpen(turn.status)) return turn;
    }
    return undefined;
  }

  private hasOpenTurn(): boolean {
    return this.newestOpenTurn() !== undefined;
  }

  private addPending(entry: PendingInput): void {
    this.pending.set(entry.view.requestId, entry);
    this.pendingOrder.push(entry.view.requestId);
  }

  private removePending(requestId: string): void {
    if (!this.pending.has(requestId)) return;
    this.pending.delete(requestId);
    const index = this.pendingOrder.indexOf(requestId);
    if (index >= 0) this.pendingOrder.splice(index, 1);
  }

  private movePendingToFront(requestId: string): void {
    const index = this.pendingOrder.indexOf(requestId);
    if (index > 0) {
      this.pendingOrder.splice(index, 1);
      this.pendingOrder.unshift(requestId);
    }
  }

  private hasImmediateReservation(): boolean {
    for (const requestId of this.pendingOrder) {
      if (this.pending.get(requestId)?.immediate) return true;
    }
    return false;
  }

  private downgradePendingGuides(): void {
    for (const requestId of this.pendingOrder) {
      const pending = this.pending.get(requestId);
      if (pending && pending.view.delivery === "guide") {
        pending.view.delivery = "queue";
        pending.view.downgraded = true;
      }
    }
  }

  private rememberRequest(requestId: string): void {
    if (this.knownRequests.has(requestId)) return;
    this.knownRequests.add(requestId);
    this.knownRequestOrder.push(requestId);
    if (this.knownRequestOrder.length > KNOWN_REQUESTS_LIMIT) {
      const evicted = this.knownRequestOrder.shift();
      if (evicted !== undefined) {
        this.knownRequests.delete(evicted);
        this.inputByRequest.delete(evicted);
      }
    }
  }

  private isAscending(events: readonly WireEvent[]): boolean {
    for (let i = 1; i < events.length; i += 1) {
      const prev = events[i - 1];
      const next = events[i];
      if (!prev || !next || next.seq < prev.seq) return false;
    }
    return true;
  }
}

function recomputeAssistantOutput(turn: TurnView): string | undefined {
  let output: string | undefined;
  for (const segment of turn.workSegments) {
    if (!segment.assistantText) continue;
    output = output ? `${output}\n\n${segment.assistantText}` : segment.assistantText;
  }
  return output;
}
