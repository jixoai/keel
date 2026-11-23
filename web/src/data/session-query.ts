// Bounded transcript search and export — change keel-harness-v2, W12
// `keel.session-query`.
//
// The session list's local filter only sees the rows it already holds. This
// module is the server-backed read: it asks `GET /v1/sessions/search` for a
// bounded page of message hits across the sessions the caller can read, and it
// exports one bounded page of a session's transcript. Neither call invents
// rows: the title is the server's derived title, the snippet is the server's
// window, and the export is exactly the messages the server returned.
import { signal } from "nsh";
import type {
  SessionExportView,
  SessionMessageHitView,
  SessionMessageSearchView,
  SessionSearchView,
} from "./contract";
import { can } from "./capabilities";
import { KeelApiError, errorMessageOf } from "./http";
import { store } from "./store";

/** A server-backed search this client and this deployment both serve. The
 * transport having the method is not the question: the capability gate is
 * (a fixture or an older transport must not switch the UI on by itself). */
export function searchAvailable(): boolean {
  return can("session.search");
}

/** The bounded export read this client and this deployment both serve. */
export function exportAvailable(): boolean {
  return can("session.export");
}

/** One exported transcript page, plus the JSONL a reader copies. */
export interface SessionExportResult {
  view: SessionExportView;
  /** The page as one JSONL document: one message per line, nothing else. */
  jsonl: string;
}

class SessionQueryAccess {
  /** The query the current result belongs to, so a late answer never replaces
   * a newer query's hits. */
  readonly query = signal("");
  readonly hits = signal<SessionMessageHitView[]>([]);
  readonly loading = signal(false);
  readonly error = signal<string | null>(null);
  readonly hasMore = signal(false);
  readonly nextCursor = signal<string | undefined>(undefined);
  /** True once a search for the current query landed (even with no hits), so
   * the list can tell "still searching" from "no message matches". */
  readonly searched = signal(false);

  private token = 0;

  clear(): void {
    this.token += 1;
    this.query.set("");
    this.hits.set([]);
    this.loading.set(false);
    this.error.set(null);
    this.hasMore.set(false);
    this.nextCursor.set(undefined);
    this.searched.set(false);
  }

  /** Search messages for `text`. A query shorter than two characters clears
   * the result instead of scanning for one letter. */
  async search(text: string): Promise<void> {
    const trimmed = text.trim();
    const api = store.identityApi();
    if (trimmed.length < 2 || !api?.sessionSearch) {
      this.clear();
      return;
    }
    const token = ++this.token;
    this.query.set(trimmed);
    this.loading.set(true);
    this.error.set(null);
    this.searched.set(false);
    try {
      const page = (await api.sessionSearch(trimmed, { scope: "messages", limit: 20 })) as SessionSearchView;
      if (token !== this.token) return;
      const messages = page.scope === "messages" ? ((page as SessionMessageSearchView).messages ?? []) : [];
      this.hits.set(messages);
      this.hasMore.set(page.hasMore === true);
      this.nextCursor.set(page.nextCursor ?? undefined);
      this.searched.set(true);
    } catch (error) {
      if (token !== this.token) return;
      this.hits.set([]);
      this.hasMore.set(false);
      this.nextCursor.set(undefined);
      this.error.set(
        error instanceof KeelApiError && error.status === 403
          ? "没有搜索历史消息的权限。"
          : `搜索失败：${errorMessageOf(error)}`,
      );
      this.searched.set(true);
    } finally {
      if (token === this.token) this.loading.set(false);
    }
  }

  /** The next page of the same query. */
  async more(): Promise<void> {
    const api = store.identityApi();
    const text = this.query();
    const after = this.nextCursor();
    if (!api?.sessionSearch || !text || !after || this.loading()) return;
    const token = this.token;
    this.loading.set(true);
    try {
      const page = (await api.sessionSearch(text, {
        scope: "messages",
        limit: 20,
        after,
      })) as SessionSearchView;
      if (token !== this.token) return;
      const messages = page.scope === "messages" ? ((page as SessionMessageSearchView).messages ?? []) : [];
      this.hits.set([...this.hits(), ...messages]);
      this.hasMore.set(page.hasMore === true);
      this.nextCursor.set(page.nextCursor ?? undefined);
    } catch (error) {
      if (token !== this.token) return;
      this.error.set(`继续搜索失败：${errorMessageOf(error)}`);
    } finally {
      if (token === this.token) this.loading.set(false);
    }
  }

  /** One bounded export page of `sessionId`, and its JSONL rendering. The UI
   * owns delivery (clipboard/download); this module owns the read and the
   * exact lines, so a caller never composes a second export shape. */
  async exportSession(sessionId: string): Promise<SessionExportResult> {
    const api = store.identityApi();
    if (!api?.sessionExport) throw new Error("这个部署没有提供会话导出。");
    const view = await api.sessionExport(sessionId, { limit: 200 });
    return { view, jsonl: exportJSONL(view) };
  }
}

/** The export's JSONL document: the page's messages, one JSON object per line.
 * It carries only what the page carries (no tool payloads, no selection). */
export function exportJSONL(view: SessionExportView): string {
  const lines = (view.messages ?? []).map((message) =>
    JSON.stringify({
      seq: message.seq,
      kind: message.kind,
      role: message.role,
      at: message.at,
      text: message.text,
      ...(message.textTruncated ? { textTruncated: true } : {}),
    }),
  );
  return lines.join("\n");
}

/** The page's one-line summary, in the reader's words. */
export function exportSummary(view: SessionExportView): string {
  const count = view.messages?.length ?? 0;
  if (count === 0) return "这个会话还没有可导出的消息。";
  const limit = view.limit ?? 200;
  if (view.hasMore) {
    return `已复制 ${count} 条消息（本页上限 ${limit} 条，还有更多消息未导出）。`;
  }
  return `已复制 ${count} 条消息。`;
}

export const sessionQueryAccess = new SessionQueryAccess();
