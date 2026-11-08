// The app's one store: sessions, the active conversation, the transport mode
// and every command the screens issue. Screens read signals; nothing outside
// the data layer talks to fetch or to the fixture.
import { host, signal } from "nsh";
import { createCoreEngine, type CoreEngine } from "./core";
import { FixtureClient } from "./fixture";
import { KeelApiError, KeelHttp } from "./http";
import type {
  AttachmentRef,
  ConnectionState,
  ConversationView,
  Delivery,
  EventSource,
  SessionSummary,
  UiCommands,
  WireEvent,
} from "./contract";
import { asString, payloadOf, textOf, contentOf } from "./wire";

export type Mode = "checking" | "fixture" | "http";

export interface Notice {
  tone: "info" | "warn" | "danger" | "success";
  text: string;
}

export interface PendingAttachment {
  name: string;
  mime: string;
  size: number;
  /** A blob: URL for the local preview. */
  preview?: string;
  file?: Blob;
}

interface Client {
  probe(): Promise<"ok" | "auth" | "down">;
  capabilities(): Promise<Record<string, unknown>>;
  listSessions(limit?: number): Promise<SessionSummary[]>;
  createSession(): Promise<string>;
  deleteSession(sessionId: string): Promise<void>;
  source(sessionId: string): EventSource;
  commands(sessionId: string): UiCommands;
  tail?(sessionId: string, lastSeq: number, count?: number): Promise<WireEvent[]>;
}

const newRequestId = () => {
  const cryptoAny = globalThis.crypto as { randomUUID?: () => string } | undefined;
  if (cryptoAny?.randomUUID) return cryptoAny.randomUUID();
  return `req-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
};

/** A local notification when the platform has a host that grants it; web
 * builds answer `keel.notify` in host.js. Silently a no-op elsewhere. */
const notify = async (title: string, body: string) => {
  try {
    if (!host.available()) return;
    await host.call("keel.notify", { title, body, tag: "keel-approval" }, { timeout: 5000 });
  } catch {
    /* notifications are best-effort */
  }
};

const messageOf = (error: unknown): string => {
  if (error instanceof KeelApiError) return error.message;
  if (error instanceof Error) return error.message;
  return String(error);
};

class KeelStore {
  readonly mode = signal<Mode>("checking");
  /** Live API auth gate: the server wants a session (login) before /v1 works. */
  readonly auth = signal<"unknown" | "ok" | "required">("unknown");
  readonly authError = signal<string | null>(null);
  readonly sessions = signal<SessionSummary[]>([]);
  readonly sessionsLoading = signal(true);
  readonly sessionsError = signal<string | null>(null);
  readonly activeId = signal<string | null>(null);
  readonly conversation = signal<ConversationView | null>(null);
  readonly partial = signal<string | undefined>(undefined);
  readonly notice = signal<Notice | null>(null);
  readonly showPartials = signal(true);
  readonly busy = signal(false);
  readonly stopping = signal(false);
  readonly connection = signal<ConnectionState>({ status: "closed", lastSeq: 0 });
  readonly decisionDeadlines = signal<Record<string, number>>({});

  private readonly fixture = new FixtureClient();
  private http: KeelHttp | null = null;
  private client: Client = this.fixture;
  private engine: CoreEngine | null = null;
  private unsubscribeEngine: (() => void) | null = null;
  private unsubscribeStream: (() => void) | null = null;
  private openToken = 0;
  private noticeTimer: number | undefined;
  private refreshTimer: number | undefined;
  private answered = new Set<string>();

  constructor() {
    void this.detect();
  }

  private async detect() {
    this.sessionsLoading.set(true);
    const http = new KeelHttp("");
    let status: "ok" | "auth" | "down" = "down";
    try {
      status = await http.probe();
      if (status === "ok") {
        // A static server may answer every path with index.html; the API's
        // capabilities endpoint must be JSON to count as a live service.
        const capabilities = await http.capabilities().catch(() => null);
        if (!capabilities) status = "down";
      }
    } catch {
      status = "down";
    }
    if (status === "down") {
      this.client = this.fixture;
      this.mode.set("fixture");
      await this.refreshSessions();
      return;
    }
    // A live server: authenticated, or waiting for the login gate.
    this.http = http;
    this.client = http;
    this.mode.set("http");
    this.auth.set(status === "ok" ? "ok" : "required");
    if (status === "auth") {
      this.sessionsLoading.set(false);
      return;
    }
    await this.refreshSessions();
  }

  /** Exchange the local passphrase for the server session (login gate). */
  async login(password: string): Promise<boolean> {
    const http = this.http ?? new KeelHttp("");
    this.authError.set(null);
    try {
      await http.login(password);
    } catch (error) {
      // The server's 401 body is terse English; the gate speaks the app's
      // language. Anything else (network, 5xx) is surfaced verbatim.
      this.authError.set(
        error instanceof KeelApiError && error.status === 401 ? "口令不正确" : messageOf(error),
      );
      return false;
    }
    this.http = http;
    this.client = http;
    this.auth.set("ok");
    this.sessionsLoading.set(true);
    await this.refreshSessions();
    return true;
  }

  /** Switch transports by hand (the sidebar footer's badge). */
  async switchMode(mode: Mode) {
    if (mode === this.mode() || mode === "checking") return;
    this.closeStream();
    this.activeId.set(null);
    this.conversation.set(null);
    if (mode === "http") {
      this.http = this.http ?? new KeelHttp("");
      this.client = this.http;
    } else {
      this.client = this.fixture;
    }
    this.mode.set(mode);
    await this.refreshSessions();
  }

  async refreshSessions() {
    if (this.mode() === "checking") return;
    try {
      const rows = await this.client.listSessions(60);
      this.sessions.set(rows);
      this.sessionsError.set(null);
      void this.enrich(rows);
    } catch (error) {
      if (error instanceof KeelApiError && (error.status === 401 || error.status === 403)) {
        // The session expired: back to the login gate instead of an error list.
        this.auth.set("required");
        this.sessionsError.set(null);
      } else {
        this.sessionsError.set(messageOf(error));
      }
    } finally {
      this.sessionsLoading.set(false);
    }
  }

  /** Fill title/summary from the tail of the log when the list endpoint does
   * not carry them (the API spec only requires ids and timestamps). */
  private async enrich(rows: SessionSummary[]) {
    const tail = this.client.tail?.bind(this.client);
    if (!tail) return;
    for (const row of rows.slice(0, 12)) {
      if (row.title && row.lastMessage) continue;
      try {
        const events = await tail(row.sessionId, row.lastSeq ?? 0, 8);
        if (!events.length) continue;
        const firstUser = events.find((event) => event.kind === "user.message");
        const last = [...events].reverse().find((event) => event.kind !== "run.finished") ?? events[events.length - 1];
        const payload = payloadOf(last);
        const summary =
          asString(payload.text) ??
          textOf(contentOf(payload)) ??
          asString(payload.question) ??
          asString(payload.toolName) ??
          "";
        const title = firstUser ? textOf(contentOf(payloadOf(firstUser))).slice(0, 32) : undefined;
        this.patchSession(row.sessionId, {
          title: row.title ?? (title || row.sessionId.slice(0, 8)),
          lastMessage: row.lastMessage ?? summary.slice(0, 90),
          lastSeq: last.seq,
          runStatus: last.kind === "run.finished" ? "done" : row.runStatus,
        });
      } catch {
        /* enrichment is best-effort */
      }
    }
  }

  private patchSession(sessionId: string, patch: Partial<SessionSummary>) {
    this.sessions.update((rows) =>
      rows.map((row) => (row.sessionId === sessionId ? { ...row, ...patch } : row)),
    );
  }

  async openSession(sessionId: string) {
    if (this.activeId() === sessionId) return;
    const token = ++this.openToken;
    this.closeStream();
    this.activeId.set(sessionId);
    this.connection.set({ status: "connecting", lastSeq: 0 });
    this.partial.set(undefined);
    this.decisionDeadlines.set({});
    this.answered = new Set();
    const engine = createCoreEngine(sessionId);
    this.engine = engine;
    this.unsubscribeEngine = engine.subscribe(() => {
      const view = engine.view();
      this.conversation.set(view);
      this.busy.set(view.busy);
      this.stopping.set(view.stopping);
      this.connection.set(view.connection);
      this.partial.set(engine.partialText());
    });
    try {
      const detach = await engine.attach(
        this.client.source(sessionId),
        this.showPartials(),
        (event) => {
          if (token === this.openToken) this.onLiveEvent(event);
        },
      );
      if (token !== this.openToken) {
        detach();
        return;
      }
      this.unsubscribeStream = detach;
      this.conversation.set(engine.view());
    } catch (error) {
      if (token !== this.openToken) return;
      this.notice.set({ tone: "danger", text: `打开会话失败：${messageOf(error)}` });
      this.connection.set({ status: "closed", lastSeq: 0 });
    }
  }

  private onLiveEvent(event: WireEvent) {
    const sessionId = this.activeId();
    if (!sessionId) return;
    if (event.kind === "decision") {
      const payload = payloadOf(event);
      const status = asString(payload.status);
      const question = asString(payload.question) ?? asString(payload.prompt) ?? "需要你的确认";
      const decisionId = asString(payload.decisionId) ?? asString(payload.id);
      const rawDeadline = payload.timeoutAt ?? payload.expiresAt;
      let deadline: number | undefined;
      if (typeof rawDeadline === "number" && Number.isFinite(rawDeadline)) {
        deadline = rawDeadline < 1e12 ? rawDeadline * 1000 : rawDeadline;
      } else if (typeof rawDeadline === "string") {
        const parsed = Date.parse(rawDeadline);
        if (Number.isFinite(parsed)) deadline = parsed;
      }
      if (decisionId && deadline !== undefined) {
        this.decisionDeadlines.update((map) => ({ ...map, [decisionId]: deadline! }));
      }
      if (status === "open" || status === undefined) {
        void notify("Keel · 需要你的审批", question);
      }
    }
    if (event.kind === "run.finished") {
      this.patchSession(sessionId, {
        runStatus: String(payloadOf(event).stopReason ?? "done"),
        lastSeq: event.seq,
        lastMessageAt: new Date().toISOString(),
      });
    } else if (event.kind === "user.message" || event.kind === "assistant.message") {
      this.patchSession(sessionId, { runStatus: "running", lastSeq: event.seq });
    }
    if (event.kind !== "partial") this.scheduleRefresh();
  }

  private scheduleRefresh() {
    if (this.refreshTimer !== undefined) clearTimeout(this.refreshTimer);
    this.refreshTimer = setTimeout(() => void this.refreshSessions(), 600) as unknown as number;
  }

  closeStream() {
    this.unsubscribeStream?.();
    this.unsubscribeStream = null;
    this.unsubscribeEngine?.();
    this.unsubscribeEngine = null;
    this.engine?.detach();
    this.engine = null;
  }

  closeSession() {
    this.openToken += 1;
    this.closeStream();
    this.activeId.set(null);
    this.conversation.set(null);
    this.partial.set(undefined);
    this.busy.set(false);
    this.stopping.set(false);
    this.connection.set({ status: "closed", lastSeq: 0 });
    this.decisionDeadlines.set({});
  }

  attachmentsFor(requestId: string): AttachmentRef[] | undefined {
    return this.engine?.attachmentsFor(requestId);
  }

  decisionTimeout(decisionId: string): number | undefined {
    return this.decisionDeadlines()[decisionId];
  }

  async send(text: string, mode: Delivery, pending: PendingAttachment[] = []) {
    const sessionId = this.activeId();
    const engine = this.engine;
    if (!sessionId || !engine) return;
    const trimmed = text.trim();
    if (!trimmed && pending.length === 0) return;
    const delivery: Delivery = this.busy() ? mode : "queue";
    const requestId = newRequestId();
    const commands = this.client.commands(sessionId);
    const optimistic: AttachmentRef[] = pending.map((item) => ({
      name: item.name,
      mime: item.mime,
      size: item.size,
      url: item.preview,
    }));
    engine.submit(trimmed || "（附件）", delivery, requestId, optimistic);
    try {
      let attachments: AttachmentRef[] = [];
      if (pending.length) {
        if (!commands.upload) throw new Error("当前传输不支持附件上传");
        for (const item of pending) {
          if (!item.file) continue;
          const uploaded = await commands.upload(item.file);
          attachments.push({
            fileId: uploaded.fileId,
            name: item.name,
            mime: item.mime,
            size: item.size,
            url: item.preview,
          });
        }
      }
      const body = trimmed || (attachments.length ? "（附件）" : "");
      if (commands.sendWithAttachments) {
        await commands.sendWithAttachments(body, { requestId, delivery, attachments });
      } else {
        await commands.send(body, { requestId, delivery });
      }
    } catch (error) {
      engine.removeOptimistic(requestId);
      this.notice.set({ tone: "danger", text: `发送失败：${messageOf(error)}` });
    }
  }

  async stop() {
    const sessionId = this.activeId();
    const engine = this.engine;
    if (!sessionId || !engine) return;
    engine.requestStop();
    try {
      await this.client.commands(sessionId).stop();
    } catch (error) {
      engine.resetStopping();
      this.notice.set({ tone: "danger", text: `停止失败：${messageOf(error)}` });
    }
  }

  async answerDecision(decisionId: string, answer: string) {
    const sessionId = this.activeId();
    if (!sessionId || this.answered.has(decisionId)) return;
    this.answered.add(decisionId);
    try {
      await this.client.commands(sessionId).answerDecision(decisionId, answer);
      this.engine?.answerDecisionLocal(decisionId, answer);
    } catch (error) {
      this.answered.delete(decisionId);
      this.notice.set({ tone: "danger", text: `提交答案失败：${messageOf(error)}` });
    }
  }

  isAnswering(decisionId: string): boolean {
    return this.answered.has(decisionId);
  }

  async createSession(): Promise<string | null> {
    try {
      const id = await this.client.createSession();
      await this.refreshSessions();
      await this.openSession(id);
      return id;
    } catch (error) {
      this.notice.set({ tone: "danger", text: `新建会话失败：${messageOf(error)}` });
      return null;
    }
  }

  async deleteSession(sessionId: string) {
    try {
      await this.client.deleteSession(sessionId);
      if (this.activeId() === sessionId) this.closeSession();
      await this.refreshSessions();
    } catch (error) {
      this.notice.set({ tone: "danger", text: `删除会话失败：${messageOf(error)}` });
    }
  }

  async setPartials(enabled: boolean) {
    this.showPartials.set(enabled);
    const sessionId = this.activeId();
    const engine = this.engine;
    if (!sessionId || !engine) return;
    // Restart the stream so the server's partial side channel follows.
    this.unsubscribeStream?.();
    this.unsubscribeStream = null;
    const detach = await engine.attach(
      this.client.source(sessionId),
      enabled,
      (event) => this.onLiveEvent(event),
    );
    if (this.engine === engine) this.unsubscribeStream = detach;
    else detach();
  }

  simulateDrop() {
    const sessionId = this.activeId();
    if (sessionId && this.client === this.fixture) this.fixture.simulateDrop(sessionId);
  }

  dismissNotice() {
    if (this.noticeTimer !== undefined) clearTimeout(this.noticeTimer);
    this.notice.set(null);
  }

  flash(notice: Notice) {
    if (this.noticeTimer !== undefined) clearTimeout(this.noticeTimer);
    this.notice.set(notice);
    this.noticeTimer = setTimeout(() => this.notice.set(null), 6000) as unknown as number;
  }
}

export const store = new KeelStore();
export type { KeelStore };
