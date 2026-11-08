// Wire payload helpers: the event log stores payloads as JSON objects
// (`GET /v1/sessions/{id}/events`) or JSON strings (SSE `data:` lines).
// These helpers read the shapes `internal/agentcore` writes without trusting
// optional fields, so a partial/older payload never breaks projection.
import type { AttachmentRef, Delivery, RunSummary, ToolCallView, WireEvent } from "./contract";

export interface ContentBlock {
  type?: string;
  text?: string;
  thinking?: string;
  id?: string;
  name?: string;
  arguments?: unknown;
  data?: string;
  mimeType?: string;
}

export interface UserPayload {
  text?: string;
  requestId?: string;
  delivery?: Delivery;
  attachments?: AttachmentRef[];
  content?: ContentBlock[];
  timestamp?: number;
}

export interface AssistantPayload {
  content?: ContentBlock[];
  inputSeqs?: number[];
  stopReason?: string;
  errorMessage?: string;
  usage?: { inputTokens?: number; outputTokens?: number };
  timestamp?: number;
}

export interface ToolResultPayload {
  toolCallId?: string;
  toolName?: string;
  content?: ContentBlock[];
  details?: unknown;
  isError?: boolean;
  durationMs?: number;
  timestamp?: number;
}

export interface RunFinishedPayload {
  turns?: number;
  tokensIn?: number;
  tokensOut?: number;
  tokens?: { input?: number; output?: number; total?: number };
  stopReason?: string;
  durationMs?: number;
  toolTimings?: unknown;
}

export interface DecisionPayload {
  decisionId?: string;
  id?: string;
  question?: string;
  prompt?: string;
  title?: string;
  options?: Array<{ optionId?: string; id?: string; label?: string; title?: string; description?: string }>;
  recommendation?: string;
  status?: string;
  answer?: string;
  timeoutAt?: number | string;
  timestamp?: number;
}

/** A payload, whether the transport gave an object or a JSON string. */
export function payloadOf(event: WireEvent): Record<string, unknown> {
  const raw = event.payload;
  if (typeof raw === "string") {
    try {
      return JSON.parse(raw) as Record<string, unknown>;
    } catch {
      return { text: raw };
    }
  }
  if (raw && typeof raw === "object") return raw as Record<string, unknown>;
  return {};
}

export function asNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

export function asString(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

/** A payload timestamp in milliseconds (agentcore writes ms; tolerate s). */
export function timestampOf(payload: Record<string, unknown>, fallback: number): number {
  const raw = asNumber(payload.timestamp);
  if (raw === undefined) return fallback;
  return raw < 1e12 ? raw * 1000 : raw;
}

export function contentOf(payload: Record<string, unknown>): ContentBlock[] {
  const content = payload.content;
  return Array.isArray(content) ? (content as ContentBlock[]) : [];
}

export function textOf(content: ContentBlock[] | undefined): string {
  if (!content) return "";
  return content
    .filter((block) => block.type === "text" && typeof block.text === "string")
    .map((block) => block.text as string)
    .join("");
}

export function thinkingOf(content: ContentBlock[] | undefined): string {
  if (!content) return "";
  return content
    .filter((block) => block.type === "thinking" && typeof block.thinking === "string")
    .map((block) => block.thinking as string)
    .join("");
}

export function toolCallsOf(content: ContentBlock[] | undefined, at: number): ToolCallView[] {
  if (!content) return [];
  return content
    .filter((block) => block.type === "toolCall")
    .map((block, index) => ({
      toolCallId: block.id ?? `tool-${at}-${index}`,
      name: block.name ?? "tool",
      args: block.arguments,
      startedAt: at,
    }));
}

export function imagesOf(content: ContentBlock[] | undefined): AttachmentRef[] {
  if (!content) return [];
  return content
    .filter((block) => block.type === "image" && typeof block.data === "string")
    .map((block, index) => ({
      name: `image-${index + 1}.${(block.mimeType ?? "image/png").split("/").pop() ?? "png"}`,
      mime: block.mimeType ?? "image/png",
      url: `data:${block.mimeType ?? "image/png"};base64,${block.data}`,
    }));
}

/** The attachments of a user message: an explicit `attachments` array and/or
 * image content blocks (the kernel's transcript form). */
export function attachmentsOf(payload: Record<string, unknown>): AttachmentRef[] {
  const explicit = Array.isArray(payload.attachments) ? (payload.attachments as AttachmentRef[]) : [];
  const images = imagesOf(contentOf(payload));
  return [...explicit, ...images].filter((item) => item && typeof item.name === "string");
}

export function inputSeqsOf(payload: Record<string, unknown>): number[] {
  const seqs = payload.inputSeqs;
  return Array.isArray(seqs) ? seqs.filter((n): n is number => typeof n === "number") : [];
}

export function summaryOf(payload: Record<string, unknown>): RunSummary {
  const tokens = (payload.tokens ?? {}) as Record<string, unknown>;
  const usage = (payload.usage ?? {}) as Record<string, unknown>;
  return {
    turns: asNumber(payload.turns) ?? 1,
    tokensIn:
      asNumber(payload.tokensIn) ??
      asNumber(tokens.input) ??
      asNumber(usage.inputTokens) ??
      0,
    tokensOut:
      asNumber(payload.tokensOut) ??
      asNumber(tokens.output) ??
      asNumber(usage.outputTokens) ??
      0,
    stopReason: asString(payload.stopReason) ?? "done",
    durationMs: asNumber(payload.durationMs) ?? 0,
  };
}

/** A tool result's display text: its content blocks, else its details. */
export function toolResultText(payload: Record<string, unknown>): string {
  const text = textOf(contentOf(payload));
  if (text) return text;
  if (payload.details !== undefined) {
    try {
      return typeof payload.details === "string" ? payload.details : JSON.stringify(payload.details, null, 2);
    } catch {
      return String(payload.details);
    }
  }
  return "";
}

/** A short one-line preview of a value (arguments, results). */
export function preview(value: unknown, max = 120): string {
  if (value === undefined || value === null) return "";
  let text: string;
  if (typeof value === "string") text = value;
  else {
    try {
      text = JSON.stringify(value);
    } catch {
      text = String(value);
    }
  }
  text = text.replace(/\s+/g, " ").trim();
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

export function formatDuration(ms: number | undefined): string {
  if (!ms || ms < 0) return "";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  const minutes = Math.floor(ms / 60_000);
  const seconds = Math.round((ms % 60_000) / 1000);
  return `${minutes}m ${seconds}s`;
}

export function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

export function formatTime(value: string | number | undefined, now = Date.now()): string {
  if (value === undefined || value === "") return "";
  const ms = typeof value === "number" ? value : Date.parse(value);
  if (!Number.isFinite(ms)) return "";
  const diff = now - ms;
  if (diff >= 0 && diff < 60_000) return "刚刚";
  if (diff >= 0 && diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
  const date = new Date(ms);
  const sameDay = new Date(now).toDateString() === date.toDateString();
  const hh = String(date.getHours()).padStart(2, "0");
  const mm = String(date.getMinutes()).padStart(2, "0");
  if (sameDay) return `${hh}:${mm}`;
  return `${date.getMonth() + 1}/${date.getDate()} ${hh}:${mm}`;
}
