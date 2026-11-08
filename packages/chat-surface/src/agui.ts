// AG-UI mapping: keel committed events -> AG-UI event stream.
//
// AG-UI is a transport-neutral event protocol for agent UIs. This mapping is
// the public seam for renderers other than the NSH client (assistant-ui and
// future web renderers). The exact table is pinned in docs/CHAT-SURFACE.md and
// by test/agui.test.ts.
//
// Notes:
//   - AG-UI has no standalone HITL event. A keel `decision` becomes a CUSTOM
//     keel.decision card, and an open decision at run end turns RUN_FINISHED's
//     outcome into { type: "interrupt", interrupts: [...] } (the protocol's
//     documented pause/resume shape).
//   - Text/reasoning/tool-call events are emitted around each committed
//     assistant message. Token-level partials are intentionally not mapped:
//     AG-UI consumers can consume engine.partial() directly if they want them.
//   - The mapper is pure and deterministic; duplicate seqs are ignored.

import type { ConversationView, WireEvent } from "./types";
import {
  asRecord,
  asArray,
  num,
  parseAssistantMessage,
  parseCompaction,
  parseDecision,
  parseRunFinished,
  parseToolResult,
  parseUserMessage,
  str,
} from "./events";

export interface AgUiInterrupt {
  id: string;
  reason: string;
  message?: string;
  toolCallId?: string;
}

export type AgUiRunOutcome =
  | { type: "success" }
  | { type: "interrupt"; interrupts: AgUiInterrupt[] };

interface AgUiBase {
  type: string;
  timestamp?: number;
}

export interface RunStartedEvent extends AgUiBase {
  type: "RUN_STARTED";
  threadId: string;
  runId: string;
}
export interface RunFinishedEvent extends AgUiBase {
  type: "RUN_FINISHED";
  threadId: string;
  runId: string;
  outcome: AgUiRunOutcome;
  result?: unknown;
}
export interface RunErrorEvent extends AgUiBase {
  type: "RUN_ERROR";
  message: string;
  code?: string;
}
export interface StepStartedEvent extends AgUiBase {
  type: "STEP_STARTED";
  stepName: string;
}
export interface StepFinishedEvent extends AgUiBase {
  type: "STEP_FINISHED";
  stepName: string;
}
export interface TextMessageStartEvent extends AgUiBase {
  type: "TEXT_MESSAGE_START";
  messageId: string;
  role: "assistant" | "user";
}
export interface TextMessageContentEvent extends AgUiBase {
  type: "TEXT_MESSAGE_CONTENT";
  messageId: string;
  delta: string;
}
export interface TextMessageEndEvent extends AgUiBase {
  type: "TEXT_MESSAGE_END";
  messageId: string;
}
export interface ReasoningStartEvent extends AgUiBase {
  type: "REASONING_START";
  messageId: string;
}
export interface ReasoningMessageStartEvent extends AgUiBase {
  type: "REASONING_MESSAGE_START";
  messageId: string;
  role: "reasoning";
}
export interface ReasoningMessageContentEvent extends AgUiBase {
  type: "REASONING_MESSAGE_CONTENT";
  messageId: string;
  delta: string;
}
export interface ReasoningMessageEndEvent extends AgUiBase {
  type: "REASONING_MESSAGE_END";
  messageId: string;
}
export interface ReasoningEndEvent extends AgUiBase {
  type: "REASONING_END";
  messageId: string;
}
export interface ToolCallStartEvent extends AgUiBase {
  type: "TOOL_CALL_START";
  toolCallId: string;
  toolCallName: string;
  parentMessageId?: string;
}
export interface ToolCallArgsEvent extends AgUiBase {
  type: "TOOL_CALL_ARGS";
  toolCallId: string;
  delta: string;
}
export interface ToolCallEndEvent extends AgUiBase {
  type: "TOOL_CALL_END";
  toolCallId: string;
}
export interface ToolCallResultEvent extends AgUiBase {
  type: "TOOL_CALL_RESULT";
  messageId: string;
  toolCallId: string;
  content: string;
  role: "tool";
}
export interface StateSnapshotEvent extends AgUiBase {
  type: "STATE_SNAPSHOT";
  snapshot: unknown;
}
export interface StateDeltaEvent extends AgUiBase {
  type: "STATE_DELTA";
  delta: unknown[];
}
export interface MessagesSnapshotEvent extends AgUiBase {
  type: "MESSAGES_SNAPSHOT";
  messages: unknown[];
}
export interface CustomAgUiEvent extends AgUiBase {
  type: "CUSTOM";
  name: string;
  value: unknown;
}

export type AgUiEvent =
  | RunStartedEvent
  | RunFinishedEvent
  | RunErrorEvent
  | StepStartedEvent
  | StepFinishedEvent
  | TextMessageStartEvent
  | TextMessageContentEvent
  | TextMessageEndEvent
  | ReasoningStartEvent
  | ReasoningMessageStartEvent
  | ReasoningMessageContentEvent
  | ReasoningMessageEndEvent
  | ReasoningEndEvent
  | ToolCallStartEvent
  | ToolCallArgsEvent
  | ToolCallEndEvent
  | ToolCallResultEvent
  | StateSnapshotEvent
  | StateDeltaEvent
  | MessagesSnapshotEvent
  | CustomAgUiEvent;

export interface AgUiOptions {
  /** AG-UI thread id (usually the keel sessionId). Defaults to the view's session. */
  threadId?: string;
  /** When provided, a leading STATE_SNAPSHOT mirrors the current projection. */
  view?: ConversationView;
}

function withTimestamp<T extends AgUiEvent>(event: T, payload: unknown): T {
  const ts = num(asRecord(payload).timestamp);
  if (ts !== undefined) (event as AgUiBase).timestamp = ts;
  return event;
}

function safeJson(value: unknown): string {
  try {
    const json = JSON.stringify(value);
    return json === undefined ? "null" : json;
  } catch {
    return JSON.stringify(String(value));
  }
}

function toolResultContent(payload: unknown): string {
  const parsed = parseToolResult(payload);
  if (parsed.text !== "") return parsed.text;
  if (parsed.result !== undefined) return typeof parsed.result === "string" ? parsed.result : safeJson(parsed.result);
  return "";
}

export function toAgUi(events: readonly WireEvent[], options: AgUiOptions = {}): AgUiEvent[] {
  const out: AgUiEvent[] = [];
  const threadId = options.threadId ?? options.view?.sessionId ?? "keel";

  if (options.view) {
    out.push({ type: "STATE_SNAPSHOT", snapshot: options.view });
  }

  let runOpen = false;
  let runId = "";
  let stepName: string | null = null;
  const openDecisions = new Map<string, { question: string }>();
  let lastSeq = 0;

  const startedRun = (seq: number, explicitId?: string): void => {
    if (runOpen) return;
    runOpen = true;
    runId = explicitId ?? `run_${seq}`;
    out.push({ type: "RUN_STARTED", threadId, runId });
  };

  const ensureStep = (name: string): void => {
    if (stepName === name) return;
    if (stepName !== null) out.push({ type: "STEP_FINISHED", stepName });
    stepName = name;
    out.push({ type: "STEP_STARTED", stepName });
  };

  const closeStep = (): void => {
    if (stepName === null) return;
    out.push({ type: "STEP_FINISHED", stepName });
    stepName = null;
  };

  const ordered = [...events].sort((a, b) => a.seq - b.seq);

  for (const event of ordered) {
    if (!event || typeof event.seq !== "number" || event.seq <= lastSeq) continue;
    lastSeq = event.seq;
    const seq = event.seq;
    const payload = event.payload;

    switch (event.kind) {
      case "user.message": {
        startedRun(seq);
        const parsed = parseUserMessage(payload);
        const turnKey = parsed.turnId ?? `t_${seq}`;
        ensureStep(`turn:${turnKey}`);
        out.push(
          withTimestamp(
            {
              type: "CUSTOM",
              name: "keel.user.message",
              value: {
                seq,
                requestId: parsed.requestId,
                text: parsed.text,
                delivery: parsed.delivery,
                origin: parsed.origin,
              },
            },
            payload,
          ),
        );
        break;
      }

      case "assistant.message": {
        startedRun(seq);
        const parsed = parseAssistantMessage(payload);
        const turnKey =
          parsed.turnId ??
          (parsed.inputSeqs.length > 0
            ? `t_${parsed.inputSeqs[0]}`
            : (stepName as string | null)?.replace(/^turn:/, "") ?? `t_${seq}`);
        ensureStep(`turn:${turnKey}`);

        let firstMessageId: string | undefined;
        for (let i = 0; i < parsed.textBlocks.length; i += 1) {
          const block = parsed.textBlocks[i]!;
          if (block === "") continue;
          const messageId = parsed.textBlocks.length === 1 ? `msg_${seq}` : `msg_${seq}_${i}`;
          firstMessageId ??= messageId;
          out.push({ type: "TEXT_MESSAGE_START", messageId, role: "assistant" });
          out.push({ type: "TEXT_MESSAGE_CONTENT", messageId, delta: block });
          out.push({ type: "TEXT_MESSAGE_END", messageId });
        }

        for (let i = 0; i < parsed.thinkingBlocks.length; i += 1) {
          const block = parsed.thinkingBlocks[i]!;
          if (block === "") continue;
          const messageId = parsed.thinkingBlocks.length === 1 ? `reason_${seq}` : `reason_${seq}_${i}`;
          out.push({ type: "REASONING_START", messageId });
          out.push({ type: "REASONING_MESSAGE_START", messageId, role: "reasoning" });
          out.push({ type: "REASONING_MESSAGE_CONTENT", messageId, delta: block });
          out.push({ type: "REASONING_MESSAGE_END", messageId });
          out.push({ type: "REASONING_END", messageId });
        }

        for (const call of parsed.toolCalls) {
          out.push({
            type: "TOOL_CALL_START",
            toolCallId: call.toolCallId,
            toolCallName: call.name,
            ...(firstMessageId !== undefined ? { parentMessageId: firstMessageId } : {}),
          });
          out.push({
            type: "TOOL_CALL_ARGS",
            toolCallId: call.toolCallId,
            delta: safeJson(call.args ?? {}),
          });
          out.push({ type: "TOOL_CALL_END", toolCallId: call.toolCallId });
        }
        break;
      }

      case "tool.result": {
        startedRun(seq);
        const parsed = parseToolResult(payload);
        if (!parsed.toolCallId) break;
        out.push({
          type: "TOOL_CALL_RESULT",
          messageId: `tool_${seq}`,
          toolCallId: parsed.toolCallId,
          content: toolResultContent(payload),
          role: "tool",
        });
        if (parsed.isError && parsed.error) {
          out.push({
            type: "CUSTOM",
            name: "keel.tool.error",
            value: { toolCallId: parsed.toolCallId, error: parsed.error },
          });
        }
        break;
      }

      case "run.started": {
        const p = asRecord(payload);
        startedRun(seq, str(p.runId));
        break;
      }

      case "run.finished": {
        const parsed = parseRunFinished(payload);
        if (!runOpen) startedRun(seq, parsed.runId);
        closeStep();
        if (parsed.outcome === "failed") {
          out.push({
            type: "RUN_ERROR",
            message: parsed.errorMessage ?? `run ${parsed.stopReason}`,
            code: parsed.stopReason,
          });
          runOpen = false;
          runId = "";
          break;
        }
        const interrupts: AgUiInterrupt[] = [];
        if (parsed.outcome === "completed") {
          for (const [id, decision] of openDecisions) {
            interrupts.push({ id, reason: "decision", message: decision.question });
          }
        }
        out.push({
          type: "RUN_FINISHED",
          threadId,
          runId,
          outcome:
            interrupts.length > 0
              ? { type: "interrupt", interrupts }
              : { type: "success" },
          result: {
            stopReason: parsed.stopReason,
            turns: parsed.turns,
            durationMs: parsed.durationMs,
          },
        });
        if (interrupts.length > 0) openDecisions.clear();
        runOpen = false;
        runId = "";
        break;
      }

      case "compaction": {
        const parsed = parseCompaction(payload);
        out.push({
          type: "CUSTOM",
          name: "keel.compaction",
          value: {
            status: parsed.status,
            summary: parsed.summary,
            operationId: parsed.operationId,
            tokensBefore: parsed.tokensBefore,
          },
        });
        break;
      }

      case "decision":
      case "decision.opened":
      case "decision.answered":
      case "decision.timeout":
      case "decision.cancelled": {
        const parsed = parseDecision(payload);
        if (!parsed.decisionId) break;
        out.push({
          type: "CUSTOM",
          name: "keel.decision",
          value: {
            decisionId: parsed.decisionId,
            question: parsed.question,
            options: parsed.options,
            recommendation: parsed.recommendation,
            status: parsed.status,
            answer: parsed.answer,
          },
        });
        if (parsed.status === "open") {
          openDecisions.set(parsed.decisionId, { question: parsed.question });
        } else {
          openDecisions.delete(parsed.decisionId);
        }
        break;
      }

      case "external.event": {
        out.push({
          type: "CUSTOM",
          name: "keel.external",
          value: { seq, kind: event.kind, payload },
        });
        break;
      }

      default: {
        if (event.kind.startsWith("goal")) {
          out.push({
            type: "CUSTOM",
            name: "keel.goal",
            value: { seq, kind: event.kind, payload },
          });
        } else {
          out.push({
            type: "CUSTOM",
            name: "keel.event",
            value: { seq, kind: event.kind, payload },
          });
        }
      }
    }

    // State events are carried as payload.state / payload.patch on any kind.
    const p = asRecord(payload);
    if (p.state !== undefined) {
      out.push({ type: "STATE_SNAPSHOT", snapshot: p.state });
    }
    if (Array.isArray(p.patch)) {
      out.push({ type: "STATE_DELTA", delta: asArray(p.patch) });
    }
  }

  return out;
}
