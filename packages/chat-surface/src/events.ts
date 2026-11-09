// Wire-payload parsing for keel events.
//
// The durable event log stores opaque JSON payloads per kind:
//
//   user.message      { text, requestId, delivery?, origin?, turnId?, timestamp? }
//   assistant.message AssistantMessage (pi shape: role/content/usage/stopReason)
//                     plus keel extensions: inputSeqs, turnId, origin
//   tool.result       ToolResultMessage (toolCallId/toolName/content/details/
//                     isError/durationMs/timestamp)
//   compaction        { summary, status?/phase?, tokensBefore?, operationId?,
//                       manual?, timestamp? }
//   run.started       { turnId?, runId?, origin? }
//   run.finished      { turns, tokensIn|tokensOut|tokens|usage, toolTimings,
//                       context, model, provider, durationMs, stopReason }
//   decision          { decisionId, question, options, recommendation?,
//                       status, answer?, timeoutAt? }
//   goal.*            { status/phase, target?, ... }
//   external.event    opaque backend fact (compat log)
//
// Parsing here is deliberately tolerant: unknown fields are ignored and every
// parser accepts the common alternates (text/content, thinking/reasoning,
// toolCallId/id, ...) so the projection never throws on an additive server
// change. All schema decisions are pinned in docs/CHAT-SURFACE.md.

import type { Delivery, TurnOrigin } from "./types";

export const TURN_ORIGINS: readonly TurnOrigin[] = [
  "user_prompt",
  "queue_drain",
  "turn_steer",
  "goal_continuation",
  "manual_compact",
];

const ORIGIN_SET = new Set<string>(TURN_ORIGINS);

export function asRecord(value: unknown): Record<string, unknown> {
  if (value && typeof value === "object" && !Array.isArray(value)) {
    return value as Record<string, unknown>;
  }
  if (typeof value === "string") {
    try {
      const parsed: unknown = JSON.parse(value);
      if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
        return parsed as Record<string, unknown>;
      }
    } catch {
      // Not JSON: an opaque payload. Callers fall back to string handling.
    }
  }
  return {};
}

export function asArray(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

export function str(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

export function num(value: unknown): number | undefined {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  if (typeof value === "string" && value.trim() !== "" && Number.isFinite(Number(value))) {
    return Number(value);
  }
  return undefined;
}

export function bool(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined;
}

export function parseOrigin(value: unknown): TurnOrigin | undefined {
  return typeof value === "string" && ORIGIN_SET.has(value) ? (value as TurnOrigin) : undefined;
}

export function parseDelivery(value: unknown): Delivery | undefined {
  return value === "guide" || value === "queue" ? value : undefined;
}

/** Joins the text blocks of a pi content list (or a plain string). */
export function contentText(value: unknown): string {
  if (typeof value === "string") return value;
  const blocks = asArray(value);
  const parts: string[] = [];
  for (const block of blocks) {
    const rec = asRecord(block);
    const type = str(rec.type);
    if (type === "text" || type === undefined) {
      const text = str(rec.text);
      if (text !== undefined) parts.push(text);
    }
  }
  return parts.join("\n\n");
}

/** Joins the thinking blocks of a pi content list. */
export function contentThinking(value: unknown): string {
  const blocks = asArray(value);
  const parts: string[] = [];
  for (const block of blocks) {
    const rec = asRecord(block);
    const type = str(rec.type);
    if (type === "thinking" || type === "reasoning") {
      const text = str(rec.thinking) ?? str(rec.text) ?? str(rec.reasoning);
      if (text !== undefined) parts.push(text);
    }
  }
  return parts.join("\n\n");
}

export interface ParsedUserMessage {
  text: string;
  requestId?: string;
  delivery: Delivery;
  origin?: TurnOrigin;
  turnId?: string;
  timestamp?: number;
}

export function parseUserMessage(payload: unknown): ParsedUserMessage {
  const p = asRecord(payload);
  const text = str(p.text) ?? contentText(p.content);
  const delivery = parseDelivery(p.delivery) ?? "queue";
  return {
    text,
    requestId: str(p.requestId) ?? str(p.clientId),
    delivery,
    origin: parseOrigin(p.origin),
    turnId: str(p.turnId),
    timestamp: num(p.timestamp) ?? num(p.createdAt),
  };
}

export interface ParsedToolCall {
  toolCallId: string;
  name: string;
  args?: unknown;
}

export interface ParsedAssistantMessage {
  /** All text blocks joined with a blank line. */
  text: string;
  /** Text blocks in order (AG-UI emits one message per block). */
  textBlocks: string[];
  thinking: string;
  thinkingBlocks: string[];
  toolCalls: ParsedToolCall[];
  inputSeqs: number[];
  turnId?: string;
  origin?: TurnOrigin;
  stopReason?: string;
  errorMessage?: string;
  timestamp?: number;
  tokensIn?: number;
  tokensOut?: number;
}

function toolCallFromBlock(rec: Record<string, unknown>): ParsedToolCall | undefined {
  const toolCallId = str(rec.id) ?? str(rec.toolCallId) ?? str(rec.tool_call_id);
  const name = str(rec.name) ?? str(rec.toolName) ?? str(rec.tool_name);
  if (!toolCallId || !name) return undefined;
  const args = rec.arguments ?? rec.args ?? rec.input ?? rec.parameters;
  return { toolCallId, name, args };
}

export function parseAssistantMessage(payload: unknown): ParsedAssistantMessage {
  const p = asRecord(payload);
  const content = p.content;

  const textBlocks: string[] = [];
  const thinkingBlocks: string[] = [];
  const toolCalls: ParsedToolCall[] = [];

  if (typeof content === "string") {
    textBlocks.push(content);
  } else {
    for (const block of asArray(content)) {
      const rec = asRecord(block);
      const type = str(rec.type);
      if (type === "text") {
        const text = str(rec.text);
        if (text !== undefined && text !== "") textBlocks.push(text);
      } else if (type === "thinking" || type === "reasoning") {
        const thinking = str(rec.thinking) ?? str(rec.text) ?? str(rec.reasoning);
        if (thinking !== undefined && thinking !== "") thinkingBlocks.push(thinking);
      } else if (type === "toolCall" || type === "tool_use" || type === "tool_call") {
        const call = toolCallFromBlock(rec);
        if (call) toolCalls.push(call);
      }
    }
  }

  // Flat alternates (agent-api convenience shape / external backends).
  const flatText = str(p.text);
  if (flatText !== undefined && textBlocks.length === 0) textBlocks.push(flatText);
  const flatThinking = str(p.thinking) ?? str(p.reasoning);
  if (flatThinking !== undefined && thinkingBlocks.length === 0) thinkingBlocks.push(flatThinking);
  for (const raw of asArray(p.toolCalls)) {
    const call = toolCallFromBlock(asRecord(raw));
    if (call && !toolCalls.some((c) => c.toolCallId === call.toolCallId)) toolCalls.push(call);
  }

  const inputSeqs: number[] = [];
  for (const raw of asArray(p.inputSeqs)) {
    const seq = num(raw);
    if (seq !== undefined) inputSeqs.push(seq);
  }
  inputSeqs.sort((a, b) => a - b);

  const usage = asRecord(p.usage);
  const tokensIn = num(usage.inputTokens) ?? num(usage.input) ?? num(p.tokensIn);
  const tokensOut = num(usage.outputTokens) ?? num(usage.output) ?? num(p.tokensOut);

  return {
    text: textBlocks.join("\n\n"),
    textBlocks,
    thinking: thinkingBlocks.join("\n\n"),
    thinkingBlocks,
    toolCalls,
    inputSeqs,
    turnId: str(p.turnId),
    origin: parseOrigin(p.origin),
    stopReason: str(p.stopReason) ?? str(p.stop_reason),
    errorMessage: str(p.errorMessage) ?? str(p.error),
    timestamp: num(p.timestamp),
    tokensIn,
    tokensOut,
  };
}

export interface ParsedToolResult {
  toolCallId: string;
  toolName?: string;
  /** Plain text of the result content (or the flat text field). */
  text: string;
  /** Structured result: details > result > content text. */
  result?: unknown;
  isError: boolean;
  error?: string;
  durationMs?: number;
  timestamp?: number;
}

export function parseToolResult(payload: unknown): ParsedToolResult {
  const p = asRecord(payload);
  const content = p.content;
  const text = str(p.text) ?? contentText(content);
  const isError = bool(p.isError) ?? p.error !== undefined;
  const explicitError = str(p.error) ?? str(p.errorMessage);
  const result = p.result ?? p.details ?? (text !== "" ? text : undefined);
  return {
    toolCallId: str(p.toolCallId) ?? str(p.tool_call_id) ?? str(p.id) ?? "",
    toolName: str(p.toolName) ?? str(p.name),
    text,
    result,
    isError,
    error: explicitError ?? (isError ? text || "tool error" : undefined),
    durationMs: num(p.durationMs) ?? num(p.duration),
    timestamp: num(p.timestamp),
  };
}

export type CompactionStatus = "started" | "retrying" | "completed" | "failed" | "cancelled";

export interface ParsedCompaction {
  summary: string;
  status: CompactionStatus;
  active: boolean;
  operationId?: string;
  manual: boolean;
  tokensBefore?: number;
  timestamp?: number;
}

const ACTIVE_COMPACTION = new Set(["started", "retrying", "running", "in_progress"]);

export function parseCompaction(payload: unknown): ParsedCompaction {
  const p = asRecord(payload);
  const rawStatus = (str(p.status) ?? str(p.phase) ?? "completed").toLowerCase();
  let status: CompactionStatus;
  if (ACTIVE_COMPACTION.has(rawStatus)) status = rawStatus === "retrying" ? "retrying" : "started";
  else if (rawStatus === "failed" || rawStatus === "error") status = "failed";
  else if (rawStatus === "cancelled" || rawStatus === "canceled" || rawStatus === "aborted") status = "cancelled";
  else status = "completed";
  return {
    summary: str(p.summary) ?? str(p.text) ?? "",
    status,
    active: status === "started" || status === "retrying",
    operationId: str(p.operationId) ?? str(p.id),
    manual: bool(p.manual) ?? p.reason === "manual",
    tokensBefore: num(p.tokensBefore),
    timestamp: num(p.timestamp),
  };
}

export type RunOutcome = "completed" | "interrupted" | "failed";

export interface ParsedRunFinished {
  stopReason: string;
  outcome: RunOutcome;
  turnId?: string;
  runId?: string;
  turns?: number;
  tokensIn: number;
  tokensOut: number;
  durationMs: number;
  errorMessage?: string;
  /** The execution identity the attempt resolved to (kernel-checked shape). */
  provider?: string;
  model?: string;
  timestamp?: number;
}

export function outcomeForStopReason(stopReason: string): RunOutcome {
  const r = stopReason.toLowerCase();
  if (r === "cancelled" || r === "canceled" || r === "aborted" || r === "interrupted" || r === "user_cancel") {
    return "interrupted";
  }
  if (r === "error" || r === "failed" || r === "failure") return "failed";
  return "completed";
}

export function parseRunFinished(payload: unknown): ParsedRunFinished {
  const p = asRecord(payload);
  const stopReason = str(p.stopReason) ?? str(p.status) ?? "done";
  const tokens = asRecord(p.tokens);
  const usage = asRecord(p.usage);
  return {
    stopReason,
    outcome: outcomeForStopReason(stopReason),
    turnId: str(p.turnId),
    runId: str(p.runId),
    turns: num(p.turns),
    tokensIn: num(p.tokensIn) ?? num(tokens.input) ?? num(tokens.in) ?? num(usage.inputTokens) ?? 0,
    tokensOut: num(p.tokensOut) ?? num(tokens.output) ?? num(tokens.out) ?? num(usage.outputTokens) ?? 0,
    durationMs: num(p.durationMs) ?? num(p.duration) ?? 0,
    errorMessage: str(p.errorMessage) ?? str(p.error),
    provider: str(p.provider),
    model: str(p.model),
    timestamp: num(p.timestamp),
  };
}

export type DecisionStatus = "open" | "answered" | "timeout" | "cancelled";

export interface ParsedDecision {
  decisionId: string;
  question: string;
  options: Array<{ optionId: string; label: string; description?: string }>;
  recommendation?: string;
  status: DecisionStatus;
  answer?: string;
  timestamp?: number;
}

export function parseDecisionStatus(value: unknown): DecisionStatus | undefined {
  if (typeof value !== "string") return undefined;
  switch (value.toLowerCase()) {
    case "open":
    case "pending":
    case "running":
    case "awaiting":
      return "open";
    case "answered":
    case "approved":
    case "accepted":
    case "submitted":
      return "answered";
    case "timeout":
    case "timed_out":
    case "timedout":
    case "expired":
      return "timeout";
    case "cancelled":
    case "canceled":
    case "aborted":
    case "rejected":
    case "denied":
      return "cancelled";
    default:
      return undefined;
  }
}

export function parseDecision(payload: unknown, fallbackStatus: DecisionStatus = "open"): ParsedDecision {
  const p = asRecord(payload);
  const rawOptions = asArray(p.options);
  const options: ParsedDecision["options"] = [];
  for (const raw of rawOptions) {
    const rec = asRecord(raw);
    if (typeof raw === "string") {
      options.push({ optionId: raw, label: raw });
      continue;
    }
    const optionId = str(rec.optionId) ?? str(rec.id) ?? str(rec.value);
    if (!optionId) continue;
    const label = str(rec.label) ?? str(rec.title) ?? str(rec.name) ?? optionId;
    const description = str(rec.description) ?? str(rec.detail);
    options.push(description === undefined ? { optionId, label } : { optionId, label, description });
  }
  // The product layer records answers as `DecisionAnswer {optionId,text,at}`
  // (agentext); older/fixture shapes use a plain string or a top-level field.
  const answer = answerTextOf(p.answer) ?? str(p.optionId) ?? str(p.selected) ?? str(p.text);
  return {
    decisionId: str(p.decisionId) ?? str(p.id) ?? str(p.decision_id) ?? "",
    question: str(p.question) ?? str(p.prompt) ?? str(p.title) ?? "",
    options,
    recommendation: str(p.recommendation) ?? str(p.recommended) ?? str(p.recommendedOptionId),
    status: parseDecisionStatus(p.status) ?? fallbackStatus,
    answer: answer === undefined ? undefined : String(answer),
    timestamp: num(p.timestamp),
  };
}

function answerTextOf(value: unknown): string | undefined {
  if (typeof value === "string") return value;
  if (typeof value === "number") return String(value);
  if (value && typeof value === "object") {
    const rec = asRecord(value);
    return str(rec.optionId) ?? str(rec.text) ?? str(rec.value) ?? str(rec.answer);
  }
  return undefined;
}
