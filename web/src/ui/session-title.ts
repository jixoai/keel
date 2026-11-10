// The session's title, one source (C1): the list rows, the workspace header and
// the conversation all read this, so one session never has two names.
//
// A session without a derived title — the server leaves it empty, and "新会话"
// is also what the store's "one untouched session" rule reads as untouched —
// reads "新会话" plus when it was created, so two fresh sessions never look
// alike and a raw session id never reaches the UI.
import type { SessionSummary } from "../data/contract";
import { formatTime } from "../data/wire";

/** One line, trimmed: a derived title/preview is user text and must not push a
 * row around. */
export const oneLine = (text: string | undefined): string => (text ?? "").replace(/\s+/g, " ").trim();

/** The title of `row`; an unnamed session reads "新会话 · <相对时间>". */
export function sessionTitle(row: SessionSummary | null | undefined): string {
  const title = oneLine(row?.title);
  if (title && title !== "新会话") return title;
  const time = formatTime(row?.createdAt ?? row?.lastMessageAt);
  return time ? `新会话 · ${time}` : "新会话";
}
