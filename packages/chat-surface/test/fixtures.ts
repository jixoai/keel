// Wire-shape fixtures for the keel event log.
//
// Payload shapes mirror the durable log contract documented in
// docs/CHAT-SURFACE.md: pi-shaped assistant/tool messages plus keel's
// inputSeqs/delivery/run.finished extensions.

import type { Delivery, TurnOrigin, WireEvent } from "../src/types";

export interface UserFixtureOptions {
  requestId?: string;
  delivery?: Delivery;
  origin?: TurnOrigin;
  turnId?: string;
  timestamp?: number;
}

export function userMessage(seq: number, text: string, opts: UserFixtureOptions = {}): WireEvent {
  const payload: Record<string, unknown> = {
    text,
    requestId: opts.requestId ?? `req-${seq}`,
    delivery: opts.delivery ?? "queue",
  };
  if (opts.origin !== undefined) payload.origin = opts.origin;
  if (opts.turnId !== undefined) payload.turnId = opts.turnId;
  if (opts.timestamp !== undefined) payload.timestamp = opts.timestamp;
  return { seq, kind: "user.message", payload };
}

export interface AssistantFixtureOptions {
  text?: string;
  thinking?: string;
  toolCalls?: Array<{ toolCallId: string; name: string; args?: unknown }>;
  inputSeqs?: number[];
  turnId?: string;
  origin?: TurnOrigin;
  stopReason?: string;
  errorMessage?: string;
  tokensIn?: number;
  tokensOut?: number;
  timestamp?: number;
}

export function assistantMessage(seq: number, opts: AssistantFixtureOptions = {}): WireEvent {
  const content: unknown[] = [];
  if (opts.thinking !== undefined) content.push({ type: "thinking", thinking: opts.thinking });
  if (opts.text !== undefined) content.push({ type: "text", text: opts.text });
  for (const call of opts.toolCalls ?? []) {
    content.push({ type: "toolCall", id: call.toolCallId, name: call.name, arguments: call.args ?? {} });
  }
  const payload: Record<string, unknown> = {
    role: "assistant",
    content,
  };
  if (opts.inputSeqs !== undefined) payload.inputSeqs = opts.inputSeqs;
  if (opts.turnId !== undefined) payload.turnId = opts.turnId;
  if (opts.origin !== undefined) payload.origin = opts.origin;
  if (opts.stopReason !== undefined) payload.stopReason = opts.stopReason;
  if (opts.errorMessage !== undefined) payload.errorMessage = opts.errorMessage;
  if (opts.tokensIn !== undefined || opts.tokensOut !== undefined) {
    payload.usage = { inputTokens: opts.tokensIn ?? 0, outputTokens: opts.tokensOut ?? 0 };
  }
  if (opts.timestamp !== undefined) payload.timestamp = opts.timestamp;
  return { seq, kind: "assistant.message", payload };
}

export interface ToolResultFixtureOptions {
  toolCallId: string;
  toolName?: string;
  text?: string;
  details?: unknown;
  isError?: boolean;
  error?: string;
  durationMs?: number;
  timestamp?: number;
}

export function toolResult(seq: number, opts: ToolResultFixtureOptions): WireEvent {
  const payload: Record<string, unknown> = {
    role: "toolResult",
    toolCallId: opts.toolCallId,
    toolName: opts.toolName ?? "echo",
    content: opts.text !== undefined ? [{ type: "text", text: opts.text }] : [],
    isError: opts.isError === true,
  };
  if (opts.details !== undefined) payload.details = opts.details;
  if (opts.error !== undefined) payload.error = opts.error;
  if (opts.durationMs !== undefined) payload.durationMs = opts.durationMs;
  if (opts.timestamp !== undefined) payload.timestamp = opts.timestamp;
  return { seq, kind: "tool.result", payload };
}

export interface CompactionFixtureOptions {
  summary?: string;
  status?: string;
  manual?: boolean;
  operationId?: string;
  tokensBefore?: number;
  timestamp?: number;
}

export function compaction(seq: number, opts: CompactionFixtureOptions = {}): WireEvent {
  const payload: Record<string, unknown> = {
    summary: opts.summary ?? "compacted",
  };
  if (opts.status !== undefined) payload.status = opts.status;
  if (opts.manual !== undefined) payload.manual = opts.manual;
  if (opts.operationId !== undefined) payload.operationId = opts.operationId;
  if (opts.tokensBefore !== undefined) payload.tokensBefore = opts.tokensBefore;
  if (opts.timestamp !== undefined) payload.timestamp = opts.timestamp;
  return { seq, kind: "compaction", payload };
}

export interface RunFinishedFixtureOptions {
  stopReason?: string;
  turns?: number;
  tokensIn?: number;
  tokensOut?: number;
  durationMs?: number;
  turnId?: string;
  runId?: string;
  errorMessage?: string;
  timestamp?: number;
}

export function runFinished(seq: number, opts: RunFinishedFixtureOptions = {}): WireEvent {
  const payload: Record<string, unknown> = {
    stopReason: opts.stopReason ?? "done",
  };
  if (opts.turns !== undefined) payload.turns = opts.turns;
  if (opts.tokensIn !== undefined) payload.tokensIn = opts.tokensIn;
  if (opts.tokensOut !== undefined) payload.tokensOut = opts.tokensOut;
  if (opts.durationMs !== undefined) payload.durationMs = opts.durationMs;
  if (opts.turnId !== undefined) payload.turnId = opts.turnId;
  if (opts.runId !== undefined) payload.runId = opts.runId;
  if (opts.errorMessage !== undefined) payload.errorMessage = opts.errorMessage;
  if (opts.timestamp !== undefined) payload.timestamp = opts.timestamp;
  return { seq, kind: "run.finished", payload };
}

export function runStarted(seq: number, payload: Record<string, unknown> = {}): WireEvent {
  return { seq, kind: "run.started", payload };
}

export interface DecisionFixtureOptions {
  decisionId?: string;
  question?: string;
  options?: Array<{ optionId: string; label: string; description?: string }>;
  recommendation?: string;
  status?: string;
  answer?: string;
  timestamp?: number;
}

export function decision(seq: number, opts: DecisionFixtureOptions = {}): WireEvent {
  const payload: Record<string, unknown> = {
    decisionId: opts.decisionId ?? `dec-${seq}`,
    question: opts.question ?? "Proceed?",
    options: opts.options ?? [
      { optionId: "yes", label: "Yes" },
      { optionId: "no", label: "No" },
    ],
    status: opts.status ?? "open",
  };
  if (opts.recommendation !== undefined) payload.recommendation = opts.recommendation;
  if (opts.answer !== undefined) payload.answer = opts.answer;
  if (opts.timestamp !== undefined) payload.timestamp = opts.timestamp;
  return { seq, kind: "decision", payload };
}

export function goalEvent(seq: number, kind: string, payload: Record<string, unknown>): WireEvent {
  return { seq, kind, payload };
}
