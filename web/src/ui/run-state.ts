// The one run-state vocabulary. Every surface (header pill, session rows,
// footer, TUI) projects these seven words, never a synonym: a state is shown
// with the same word wherever it appears on screen.
//
//   ready / waiting / running / stopping / stopped / failed / reconnecting
//
// `waiting` is not a kind of running: the run is parked on a question the
// human has to answer, so no loading motion belongs on it (C2).
import type { ConversationView, TurnStatus } from "../data/contract";

export type RunState = "ready" | "waiting" | "running" | "stopping" | "stopped" | "failed" | "reconnecting";

export const runStateLabel: Record<RunState, string> = {
  ready: "就绪",
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
  waiting: "warning",
  running: "brand",
  stopping: "warning",
  stopped: "neutral",
  failed: "danger",
  reconnecting: "warning",
};

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
