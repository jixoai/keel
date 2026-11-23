// The one run-state vocabulary. Every surface (header pill, session rows,
// footer, TUI) projects these eight words, never a synonym: a state is shown
// with the same word wherever it appears on screen.
//
//   ready / queued / waiting / running / stopping / stopped / failed / reconnecting
//
// `queued` is a placement wait, not "ready": the input is durable but no group
// slot has been granted yet, so the honest word is 排队中 (with the FIFO
// position and ETA when the admission reported them).
//
// `waiting` is not a kind of running: the run is parked on a question the
// human has to answer, so no loading motion belongs on it (C2).
import type { ConversationView, QueueFacts, TurnStatus } from "../data/contract";

export type { QueueFacts };

export type RunState = "ready" | "queued" | "waiting" | "running" | "stopping" | "stopped" | "failed" | "reconnecting";

export const runStateLabel: Record<RunState, string> = {
  ready: "就绪",
  queued: "排队中",
  waiting: "等待确认",
  running: "运行中",
  stopping: "停止中",
  stopped: "已停止",
  failed: "失败",
  reconnecting: "重连中",
};

export type RunTone = "neutral" | "brand" | "warning" | "danger";

export const runStateTone: Record<RunState, RunTone> = {
  ready: "neutral",
  queued: "neutral",
  waiting: "warning",
  running: "brand",
  stopping: "warning",
  stopped: "neutral",
  failed: "danger",
  reconnecting: "warning",
};

/** The row/header wording for a queued admission: 排队中 · 第 N 位 · 约 M 秒.
 * Without a queue fact it still says 排队中 (the status is a durable run row);
 * with an unknown ETA it omits the estimate instead of inventing one. */
export function queueDetail(queue: QueueFacts | undefined): string {
  const label = runStateLabel.queued;
  if (!queue || !Number.isFinite(queue.position) || queue.position <= 0) return label;
  const position = `第 ${queue.position} 位`;
  if (!queue.etaKnown || queue.etaSeconds == null || !Number.isFinite(queue.etaSeconds)) {
    return `${label} · ${position}`;
  }
  const seconds = Math.max(1, Math.ceil(queue.etaSeconds));
  return `${label} · ${position} · 约 ${seconds} 秒`;
}

/** `settling` is still the run that is wrapping up: one word, 运行中. */
export function turnState(status: TurnStatus | undefined): RunState {
  if (!status) return "ready";
  if (status === "running" || status === "settling") return "running";
  if (status === "interrupted") return "stopped";
  if (status === "failed") return "failed";
  return "ready";
}

/** The authoritative state of an open conversation. A parked question wins
 * over `running`: the run is not thinking, it waits for the human (C2). */
export function conversationState(view: ConversationView | null): RunState {
  if (!view) return "ready";
  if (view.connection.status === "reconnecting") return "reconnecting";
  if (view.decisions.some((decision) => decision.status === "open")) return "waiting";
  if (view.stopping) return "stopping";
  if (view.busy) return "running";
  const last = view.turns[view.turns.length - 1];
  return turnState(last?.status);
}

/** A session-list row's projection of `GET /v1/sessions` runStatus. */
export function sessionState(runStatus: string | undefined): RunState {
  switch ((runStatus ?? "").toLowerCase()) {
    case "queued":
      // A placement FIFO entry: durable, waiting for a group slot. Mapping this
      // to "ready" told the reader the run was idle when it was actually
      // waiting in line.
      return "queued";
    case "running":
    case "busy":
      return "running";
    case "stopping":
    case "cancelling":
    case "canceling":
      return "stopping";
    case "cancelled":
    case "canceled":
    case "interrupted":
    case "stopped":
      return "stopped";
    case "failed":
    case "error":
      return "failed";
    case "reconnecting":
      return "reconnecting";
    default:
      return "ready";
  }
}
