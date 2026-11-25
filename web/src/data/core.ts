// Adapter: the landed `@keel/chat-surface` core behind the UI's `UiEngine`.
//
// The core (packages/chat-surface/src) owns the projection and the
// snapshot→stream→resync state machine. This adapter adds only what the frozen
// contract leaves to the host: attachment indexing (the contract's
// `InputView`/`send` do not carry files), and the UI's partial accessor.
// `types.ts` is not modified.
import { createSurfaceEngine } from "../../../packages/chat-surface/src/index";
import type {
  AttachmentRef,
  ConnectionState,
  Delivery,
  EventSource,
  McpAppToolResult,
  UiEngine,
  WireEvent,
} from "./contract";
import { asString, attachmentsOf, payloadOf } from "./wire";

export interface CoreEngine extends UiEngine {
  /** Snapshot then subscribe; the core resyncs after every reconnect. */
  attach(
    source: EventSource,
    partials: boolean,
    onEvent?: (event: WireEvent) => void,
  ): Promise<() => void>;
  detach(): void;
  answerDecisionLocal(decisionId: string, answer: string): boolean;
  resumeAutoDrain(): void;
}

export function createCoreEngine(sessionId: string): CoreEngine {
  // The P2 long-conversation target is 1000 messages (500 user/assistant
  // turns); the projection's default 200-turn window would drop the first
  // 300 turns before the list ever saw them.
  const core = createSurfaceEngine({ sessionId, windowTurns: 500 });
  const attachmentsByRequest = new Map<string, AttachmentRef[]>();
  /** The complete tool.result payload per call: the core's frozen
   * `ToolCallView` keeps one `result` value, and the MCP Apps host needs the
   * whole `CallToolResult` plus the server's `details` object. */
  const toolResultsByCall = new Map<string, McpAppToolResult>();
  let detach: (() => void) | null = null;

  const asRecord = (value: unknown): Record<string, unknown> =>
    value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};

  const index = (event: WireEvent) => {
    const payload = payloadOf(event);
    if (event.kind === "user.message") {
      const requestId = asString(payload.requestId);
      const attachments = attachmentsOf(payload);
      if (requestId && attachments.length) attachmentsByRequest.set(requestId, attachments);
      return;
    }
    if (event.kind !== "tool.result") return;
    const toolCallId =
      asString(payload.toolCallId) ?? asString(payload.tool_call_id) ?? asString(payload.id);
    if (!toolCallId) return;
    const details = payload.details;
    const structured = payload.structuredContent;
    toolResultsByCall.set(toolCallId, {
      content: payload.content,
      // A ledger written by the MCP tool carries structuredContent inside
      // `details`; a server-shaped payload may carry it at the top level.
      structuredContent: structured !== undefined ? structured : asRecord(details).structuredContent,
      isError: payload.isError === true || payload.error !== undefined,
      details,
    });
  };

  const tapped = (source: EventSource, onEvent?: (event: WireEvent) => void): EventSource => ({
    list: async (after, limit) => {
      const events = await source.list(after, limit);
      for (const event of events) {
        index(event);
        onEvent?.(event);
      }
      return events;
    },
    stream: (opts) =>
      source.stream({
        ...opts,
        onEvent: (event) => {
          index(event);
          onEvent?.(event);
          opts.onEvent(event);
        },
      }),
  });

  const engine: CoreEngine = {
    view: () => core.view(),
    submit(text, delivery: Delivery, requestId, attachments) {
      if (attachments?.length) attachmentsByRequest.set(requestId, attachments);
      core.submit(text, delivery, requestId);
    },
    apply(event, attachments) {
      if (attachments?.length) {
        const requestId = asString(payloadOf(event).requestId);
        if (requestId) attachmentsByRequest.set(requestId, attachments);
      }
      index(event);
      core.apply(event);
    },
    applyPartial(partial) {
      core.applyPartial(partial);
    },
    hydrate(events, attachmentIndex) {
      if (attachmentIndex) {
        for (const [key, value] of attachmentIndex) attachmentsByRequest.set(key, value);
      }
      for (const event of events) index(event);
      core.hydrate(events);
    },
    requestStop() {
      core.requestStop();
    },
    subscribe(fn) {
      return core.subscribe(fn);
    },
    setConnection(status: ConnectionState["status"], _lastSeq: number) {
      core.setConnection(status);
    },
    setSessionId(_sessionId: string) {
      /* the core is constructed with its session id */
    },
    attachmentsFor(requestId) {
      return attachmentsByRequest.get(requestId);
    },
    toolResultFor(toolCallId) {
      return toolResultsByCall.get(toolCallId);
    },
    decisionTimeout(_decisionId) {
      /* timeouts are indexed by the store from the raw decision events */
      return undefined;
    },
    removeOptimistic(_requestId) {
      /* the core keeps submitted inputs until the server answers */
    },
    resetStopping() {
      /* stopping is the core's intent; a failed cancel keeps the barrier */
    },
    partialText() {
      return core.partial()?.text;
    },
    partialTurnId() {
      return core.partial()?.turnId;
    },
    async attach(source, partials, onEvent) {
      detach?.();
      const stop = await core.attach(tapped(source, onEvent), { partials, limit: 2000 });
      detach = stop;
      return () => {
        stop();
        if (detach === stop) detach = null;
      };
    },
    detach() {
      detach?.();
      detach = null;
      core.disconnect();
    },
    answerDecisionLocal(decisionId, answer) {
      return core.answerDecision(decisionId, answer);
    },
    resumeAutoDrain() {
      core.resumeAutoDrain();
    },
  };

  return engine;
}
