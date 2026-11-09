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
  FireResultView,
  ModelsSettingsView,
  ModelSelection,
  ModelView,
  PluginsView,
  ProviderView,
  SecretMeta,
  ScrollState,
  SendOptions,
  SessionSummary,
  SettingsApi,
  SettingsIssue,
  TriggerStatusView,
  TurnErrorState,
  UiCommands,
  WireEvent,
} from "./contract";
import { asString, payloadOf, textOf, contentOf } from "./wire";

export type Mode = "checking" | "fixture" | "http";

/** The top-level route. The app shell renders the workspace (session list +
 * conversation) or the standalone settings page (change keel-model-settings). */
export type Route = "workspace" | "settings";

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

interface Client extends SettingsApi {
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

/** Newest first by createdAt (the API's own clock), stable for rows without
 * one — a listing order, never a re-sort of what is already on screen. */
const sortNewestFirst = (rows: SessionSummary[]): SessionSummary[] => {
  const stamp = (row: SessionSummary) => {
    const raw = row.createdAt ?? row.lastMessageAt ?? "";
    const at = Date.parse(raw);
    return Number.isFinite(at) ? at : 0;
  };
  return [...rows]
    .map((row, index) => ({ row, index }))
    .sort((a, b) => stamp(b.row) - stamp(a.row) || a.index - b.index)
    .map((item) => item.row);
};

/** The tail's newest decision, read for the list badge (C2): an open decision
 * parks the session on an answer; a settled one, or a tail that ends without
 * any decision, is not waiting. `undefined` = the tail says nothing either
 * way (keep what we had). */
const pendingDecisionIn = (events: WireEvent[]): boolean | undefined => {
  for (let index = events.length - 1; index >= 0; index -= 1) {
    const event = events[index];
    if (event.kind !== "decision") continue;
    const status = asString(payloadOf(event).status);
    if (status === undefined || status === "open") return true;
    return false;
  }
  const last = events[events.length - 1];
  if (last && (last.kind === "run.finished" || last.kind === "assistant.message")) return false;
  return undefined;
};

/** Does the open conversation wait on a human answer (C2)? A parked question is
 * not thinking, and it is not a reason for a spinner. */
const waitingIn = (view: ConversationView | null): boolean =>
  (view?.decisions ?? []).some((decision) => decision.status === "open");

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
  /** The composer's turn-level error banner (C6): the newest failure of the
   * open session, dismissible per turn, cleared by a retry. */
  readonly turnError = signal<TurnErrorState | null>(null);
  /** The turn whose retry is in flight (the retry button's pending state). */
  readonly retryingTurn = signal<string | null>(null);

  /** The top-level route: `/settings` deep-links straight into the settings
   * page; the workspace stays otherwise untouched. */
  readonly route = signal<Route>("workspace");

  // ---- settings face (change keel-model-settings) ------------------------

  /** The effective models document (`GET /v1/settings/models`). */
  readonly settings = signal<ModelsSettingsView | null>(null);
  readonly settingsLoading = signal(false);
  readonly settingsError = signal<string | null>(null);
  readonly settingsSaving = signal(false);
  /** The server's 400 field issues, verbatim. */
  readonly settingsIssues = signal<SettingsIssue[]>([]);
  /** The revision a 409 conflict reported (the one that won). */
  readonly settingsConflict = signal<number | null>(null);
  /** How many selectable models the last save reported. */
  readonly settingsSavedSummary = signal<string | null>(null);

  readonly providers = signal<ProviderView[]>([]);
  readonly providersIssues = signal<SettingsIssue[]>([]);
  readonly providersError = signal<string | null>(null);
  readonly models = signal<ModelView[]>([]);
  readonly modelIssues = signal<SettingsIssue[]>([]);
  readonly modelsError = signal<string | null>(null);
  readonly enforcement = signal<{ toolCall: boolean; media: boolean } | null>(null);
  readonly registryLoaded = signal(false);

  readonly secrets = signal<SecretMeta[]>([]);
  readonly secretsAvailable = signal(true);
  readonly secretsReason = signal<string | null>(null);
  readonly secretsLoading = signal(false);
  readonly secretsError = signal<string | null>(null);
  readonly secretsBusy = signal(false);
  /** A write/delete failure, shown next to the form (never swallowed). */
  readonly secretsActionError = signal<string | null>(null);

  readonly plugins = signal<PluginsView | null>(null);
  readonly pluginsLoading = signal(false);
  readonly pluginsError = signal<string | null>(null);
  readonly triggers = signal<TriggerStatusView[]>([]);
  readonly firingTrigger = signal<string | null>(null);
  /** The last manual fire's outcome, per trigger id. */
  readonly fireOutcome = signal<{ triggerId: string; ok: boolean; text: string } | null>(null);

  /** The open session's model selection (an input fact, separate from the
   * tenant default in the settings document). */
  readonly sessionSelection = signal<ModelSelection | null>(null);

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
  /** Per-session selections, kept while the session list lives in memory. */
  private sessionSelections = new Map<string, ModelSelection>();
  /** Per-session scroll memory (C4): intent plus the offset it was seen at. */
  private scrollStates = new Map<string, ScrollState>();
  /** Turn failures the reader closed (C6): a dismissed banner stays dismissed. */
  private dismissedTurnErrors = new Set<string>();
  /** Decisions already announced (C13): a reconnect replay must not re-notify. */
  private notifiedDecisions = new Set<string>();
  /** Set as soon as the user navigates by hand, so the boot route read never
   * overrides an explicit move. */
  private routeTouched = false;
  /** True once the host answered as the web page host (route plumbing on). */
  private routePlumbed = false;

  constructor() {
    void this.detect();
    void this.initRoute();
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

  /**
   * Probe the server again from a fixture view (the settings page's "重试连接").
   * The answer decides where we land: a live server, the login gate, or back
   * to the demo fixture — never a silently fabricated one.
   */
  async reconnect() {
    if (this.mode() === "checking") return;
    this.closeStream();
    this.activeId.set(null);
    this.conversation.set(null);
    this.mode.set("checking");
    this.resetSettingsState();
    await this.detect();
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
    this.resetSettingsState();
    await this.refreshSessions();
  }

  // ---- route --------------------------------------------------------------

  /**
   * Read the route once at boot: a web page served at `/settings` (the SPA
   * fallback) starts on the settings page; every other host starts on the
   * workspace. The read is the host's (`keel.route` in host.js), because the
   * app's JS sandbox has no `location` of its own — and it is gated on
   * `keel.info` so only the real web host (which answers both) takes part:
   * native hosts and test hosts keep their routes fully in-app.
   */
  private async initRoute() {
    if (!host.available()) return;
    try {
      const info = await host.call<{ origin?: string }>("keel.info", {}, { timeout: 1200 });
      if (typeof info?.origin !== "string" || !info.origin) return;
    } catch {
      return;
    }
    this.routePlumbed = true;
    try {
      const info = await host.call<{ path?: string }>("keel.route", {}, { timeout: 1200 });
      const path = typeof info?.path === "string" ? info.path : "/";
      if (!this.routeTouched) this.route.set(path.startsWith("/settings") ? "settings" : "workspace");
    } catch {
      /* keep the in-app route */
    }
    try {
      // Browser back/forward: the URL is authoritative again.
      host.on<{ path?: string }>("keel.route", (payload) => {
        const next = typeof payload?.path === "string" ? payload.path : "";
        this.route.set(next.startsWith("/settings") ? "settings" : "workspace");
      });
    } catch {
      /* a host without events is fine: routes stay in-app */
    }
  }

  /** Keep the page URL in step with the shell (`/settings` is a real route;
   * a host without a URL — TUI, desktop, tests — just ignores it). */
  private navigate(path: string) {
    if (!this.routePlumbed) return;
    // The call goes through the same channel as the route read: the web host
    // module answers `keel.navigate` with a pushState (a `host.post` would be
    // dropped — the host module's returned object only wires `call`).
    void host.call("keel.navigate", { path }, { timeout: 1200 }).catch(() => {
      /* the route is still correct in-app */
    });
  }

  openSettings() {
    this.routeTouched = true;
    if (this.route() === "settings") return;
    this.route.set("settings");
    this.navigate("/settings");
  }

  /** Back to the workspace; the open session is preserved (the store keeps it
   * across the shell swap). */
  closeSettings() {
    this.routeTouched = true;
    if (this.route() === "workspace") return;
    this.route.set("workspace");
    this.navigate("/");
  }

  // ---- model settings -----------------------------------------------------

  /** `GET /v1/settings/models` alone (the effective document + its issues). */
  async loadModelsSettings() {
    if (this.settingsLoading()) return;
    this.settingsLoading.set(true);
    this.settingsError.set(null);
    try {
      this.settings.set(await this.client.modelsSettings());
    } catch (error) {
      this.settingsError.set(messageOf(error));
    } finally {
      this.settingsLoading.set(false);
    }
  }

  /** `GET /v1/providers` + `GET /v1/models` — the resolved registry views. */
  async loadRegistry() {
    try {
      const [providers, models] = await Promise.all([this.client.providers(), this.client.models()]);
      this.providers.set(providers.providers ?? []);
      this.providersIssues.set(providers.issues ?? []);
      this.providersError.set(null);
      this.models.set(models.models ?? []);
      this.modelIssues.set(models.issues ?? []);
      this.enforcement.set(
        models.enforcement
          ? { toolCall: models.enforcement.toolCall === true, media: models.enforcement.media === true }
          : null,
      );
      this.modelsError.set(null);
      this.registryLoaded.set(true);
    } catch (error) {
      this.providersError.set(messageOf(error));
      this.modelsError.set(messageOf(error));
    }
  }

  /** The settings page's 模型 section: one refresh of all three resources. */
  async loadModelsPanel() {
    await Promise.all([this.loadModelsSettings(), this.loadRegistry()]);
  }

  /** `PUT /v1/settings/models` — the server validates, stores and reloads.
   * 400 issues land in `settingsIssues` per field; a 409 lands in
   * `settingsConflict` with the revision that won. */
  async saveModelsSettings(document: Record<string, unknown>) {
    if (this.settingsSaving()) return;
    this.settingsSaving.set(true);
    this.settingsIssues.set([]);
    this.settingsConflict.set(null);
    this.settingsSavedSummary.set(null);
    try {
      const saved = await this.client.saveModelsSettings(document, this.settings()?.revision);
      this.settingsSavedSummary.set(
        `revision ${saved.revision} · 可执行 provider ${saved.providersExecutable ?? "-"} · 可选模型 ${
          saved.modelsSelectable ?? "-"
        }`,
      );
      this.flash({ tone: "success", text: `模型配置已保存并生效（revision ${saved.revision}）` });
      await this.loadModelsPanel();
    } catch (error) {
      if (error instanceof KeelApiError) {
        if (error.status === 400) this.settingsIssues.set(error.issues);
        if (error.status === 409) this.settingsConflict.set(error.revision ?? null);
        this.flash({ tone: error.status === 409 ? "warn" : "danger", text: `保存失败：${error.message}` });
      } else {
        this.flash({ tone: "danger", text: `保存失败：${messageOf(error)}` });
      }
    } finally {
      this.settingsSaving.set(false);
    }
  }

  /** `DELETE /v1/settings/models` — back to the deployment document. */
  async resetModelsSettings() {
    if (this.settingsSaving()) return;
    this.settingsSaving.set(true);
    this.settingsIssues.set([]);
    this.settingsConflict.set(null);
    try {
      const reset = await this.client.resetModelsSettings(this.settings()?.revision);
      this.settingsSavedSummary.set(`revision ${reset.revision} · 部署默认`);
      this.flash({ tone: "success", text: `已重置为部署默认（revision ${reset.revision}）` });
      await this.loadModelsPanel();
    } catch (error) {
      if (error instanceof KeelApiError) {
        if (error.status === 409) this.settingsConflict.set(error.revision ?? null);
        this.flash({ tone: error.status === 409 ? "warn" : "danger", text: `重置失败：${error.message}` });
      } else {
        this.flash({ tone: "danger", text: `重置失败：${messageOf(error)}` });
      }
    } finally {
      this.settingsSaving.set(false);
    }
  }

  /** The tenant default selection (the effective document's `selection`),
   * shown next to the session's own selection. */
  tenantSelection(): ModelSelection | null {
    const document = this.settings()?.document;
    const row = document && typeof document === "object" ? (document as Record<string, unknown>).selection : undefined;
    if (!row || typeof row !== "object") return null;
    const selection = row as Record<string, unknown>;
    if (typeof selection.modelId !== "string" || !selection.modelId) return null;
    return {
      providerId: typeof selection.providerId === "string" ? selection.providerId : undefined,
      modelId: selection.modelId,
      reasoningLevel: typeof selection.reasoningLevel === "string" ? selection.reasoningLevel : undefined,
    };
  }

  // ---- session model selection -------------------------------------------

  /** Set (or clear) the open session's model selection. It is an input fact:
   * nothing here writes the tenant document. */
  setSessionSelection(selection: ModelSelection | null) {
    const sessionId = this.activeId();
    if (!sessionId) return;
    if (selection) this.sessionSelections.set(sessionId, selection);
    else this.sessionSelections.delete(sessionId);
    this.sessionSelection.set(selection);
  }

  // ---- secrets ------------------------------------------------------------

  async loadSecrets() {
    if (this.secretsLoading()) return;
    this.secretsLoading.set(true);
    this.secretsError.set(null);
    try {
      const view = await this.client.secrets();
      this.secrets.set(view.secrets ?? []);
      this.secretsAvailable.set(view.available !== false);
      this.secretsReason.set(view.reason ?? null);
    } catch (error) {
      this.secretsError.set(messageOf(error));
    } finally {
      this.secretsLoading.set(false);
    }
  }

  /** Write or overwrite a secret. The value goes one way only: it is never
   * kept, echoed or rendered again. */
  async saveSecret(name: string, value: string): Promise<boolean> {
    if (this.secretsBusy()) return false;
    this.secretsBusy.set(true);
    this.secretsActionError.set(null);
    try {
      const saved = await this.client.putSecret(name.trim(), value);
      await this.loadSecrets();
      this.flash({ tone: "success", text: `密钥已保存：${saved.name}（值不回显）` });
      return true;
    } catch (error) {
      this.secretsActionError.set(messageOf(error));
      return false;
    } finally {
      this.secretsBusy.set(false);
    }
  }

  async deleteSecret(name: string) {
    if (this.secretsBusy()) return;
    this.secretsBusy.set(true);
    this.secretsActionError.set(null);
    try {
      await this.client.deleteSecret(name);
      await this.loadSecrets();
      this.flash({ tone: "success", text: `密钥已删除：${name}` });
    } catch (error) {
      this.secretsActionError.set(messageOf(error));
    } finally {
      this.secretsBusy.set(false);
    }
  }

  // ---- plugins & triggers -------------------------------------------------

  async loadPlugins() {
    if (this.pluginsLoading()) return;
    this.pluginsLoading.set(true);
    this.pluginsError.set(null);
    try {
      const [plugins, triggers] = await Promise.all([this.client.plugins(), this.client.triggers()]);
      this.plugins.set(plugins);
      this.triggers.set(triggers.triggers ?? []);
    } catch (error) {
      this.pluginsError.set(messageOf(error));
    } finally {
      this.pluginsLoading.set(false);
    }
  }

  /** `POST /v1/triggers/{id}/fire` — a manual fire; the audit result (or the
   * server's refusal) is shown in the trigger's row. */
  async fireTrigger(id: string) {
    if (this.firingTrigger()) return;
    this.firingTrigger.set(id);
    this.fireOutcome.set(null);
    try {
      const result: FireResultView = await this.client.fireTrigger(id, "manual");
      const parts = [
        result.status ? `状态 ${result.status}` : "",
        result.sessionId ? `会话 ${result.sessionId}` : "",
        result.seq ? `seq ${result.seq}` : "",
        result.deduped ? "已去重" : "",
        result.error ? `错误 ${result.error}` : "",
      ].filter(Boolean);
      this.fireOutcome.set({ triggerId: id, ok: true, text: parts.join(" · ") || "已触发" });
      const refreshed = await this.client.triggers();
      this.triggers.set(refreshed.triggers ?? []);
    } catch (error) {
      this.fireOutcome.set({ triggerId: id, ok: false, text: messageOf(error) });
    } finally {
      this.firingTrigger.set(null);
    }
  }

  private resetSettingsState() {
    this.settings.set(null);
    this.settingsError.set(null);
    this.settingsIssues.set([]);
    this.settingsConflict.set(null);
    this.settingsSavedSummary.set(null);
    this.providers.set([]);
    this.providersIssues.set([]);
    this.providersError.set(null);
    this.models.set([]);
    this.modelIssues.set([]);
    this.modelsError.set(null);
    this.enforcement.set(null);
    this.registryLoaded.set(false);
    this.secrets.set([]);
    this.secretsError.set(null);
    this.secretsActionError.set(null);
    this.plugins.set(null);
    this.pluginsError.set(null);
    this.triggers.set([]);
    this.fireOutcome.set(null);
  }

  async refreshSessions() {
    if (this.mode() === "checking") return;
    try {
      const rows = await this.client.listSessions(60);
      this.mergeSessions(rows);
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

  /** Fold a server listing into the visible order without reshuffling it: a
   * refresh carries state changes, not a new sort. Rows already on screen keep
   * their place (updated in place); only rows never seen before are inserted,
   * at the top, newest first (C5). */
  private mergeSessions(incoming: SessionSummary[]) {
    const current = this.sessions();
    if (current.length === 0) {
      this.sessions.set(sortNewestFirst(incoming));
      return;
    }
    const fresh = new Map(incoming.map((row) => [row.sessionId, row]));
    const next: SessionSummary[] = [];
    for (const row of current) {
      const update = fresh.get(row.sessionId);
      if (update) next.push({ ...row, ...update });
    }
    const known = new Set(current.map((row) => row.sessionId));
    const added = sortNewestFirst(incoming.filter((row) => !known.has(row.sessionId)));
    this.sessions.set([...added, ...next]);
  }

  /** Fill title/summary from the tail of the log when the list endpoint does
   * not carry them (the API spec only requires ids and timestamps). */
  private async enrich(rows: SessionSummary[]) {
    const tail = this.client.tail?.bind(this.client);
    if (!tail) return;
    for (const row of rows.slice(0, 12)) {
      if (row.title && row.lastMessage && row.pendingDecision !== undefined) continue;
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
        // Never a raw id as a title (C1): an unnamed session stays unnamed and
        // the list renders "新会话 + 时间" instead.
        const title = firstUser ? textOf(contentOf(payloadOf(firstUser))).slice(0, 32) : undefined;
        const pending = pendingDecisionIn(events);
        this.patchSession(row.sessionId, {
          ...(row.title ?? title ? { title: row.title ?? title } : {}),
          ...(row.lastMessage ?? summary ? { lastMessage: row.lastMessage ?? summary.slice(0, 90) } : {}),
          lastSeq: Math.max(row.lastSeq ?? 0, last.seq),
          ...(pending !== undefined ? { pendingDecision: pending } : {}),
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
    // The session's own model selection (kept in memory while the app runs).
    this.sessionSelection.set(this.sessionSelections.get(sessionId) ?? null);
    // The header's model chip shows the session's choice against the tenant
    // default: read the effective document once.
    if (!this.settings()) void this.loadModelsSettings();
    this.connection.set({ status: "connecting", lastSeq: 0 });
    this.partial.set(undefined);
    this.decisionDeadlines.set({});
    // The banner belongs to the session that produced it (C6).
    if (this.turnError()?.sessionId !== sessionId) this.turnError.set(null);
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
      this.syncTurnError(sessionId, view);
      this.patchSession(sessionId, { pendingDecision: waitingIn(view) });
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
        this.patchSession(sessionId, { pendingDecision: true });
        // One notification per decision (C13): a reconnect replay of the same
        // question must not pop it again.
        const key = `${sessionId}:${decisionId ?? question}`;
        if (!this.notifiedDecisions.has(key)) {
          this.notifiedDecisions.add(key);
          void notify("Keel · 需要你的审批", question);
        }
      } else {
        this.patchSession(sessionId, { pendingDecision: false });
      }
    }
    if (event.kind === "run.finished") {
      this.patchSession(sessionId, {
        runStatus: String(payloadOf(event).stopReason ?? "done"),
        lastSeq: event.seq,
        lastMessageAt: new Date().toISOString(),
        pendingDecision: false,
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
    this.sessionSelection.set(null);
    this.connection.set({ status: "closed", lastSeq: 0 });
    this.decisionDeadlines.set({});
  }

  attachmentsFor(requestId: string): AttachmentRef[] | undefined {
    return this.engine?.attachmentsFor(requestId);
  }

  decisionTimeout(decisionId: string): number | undefined {
    return this.decisionDeadlines()[decisionId];
  }

  /** Commit one input. Returns false when nothing was committed (no session,
   * empty input, or a refused write) — the composer restores its draft then. */
  async send(text: string, mode: Delivery, pending: PendingAttachment[] = []): Promise<boolean> {
    const sessionId = this.activeId();
    const engine = this.engine;
    if (!sessionId || !engine) return false;
    const trimmed = text.trim();
    if (!trimmed && pending.length === 0) return false;
    const delivery: Delivery = this.busy() ? mode : "queue";
    const requestId = newRequestId();
    const commands = this.client.commands(sessionId);
    // The session's model selection rides every input; without one the server
    // resolves the tenant default (the field is then absent on the wire).
    const selection = this.sessionSelection() ?? undefined;
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
      const sendOpts: SendOptions = { requestId, delivery, attachments };
      if (selection) sendOpts.selection = selection;
      if (commands.sendWithAttachments) {
        await commands.sendWithAttachments(body, sendOpts);
      } else {
        await commands.send(body, sendOpts);
      }
      this.turnError.set(null);
      return true;
    } catch (error) {
      engine.removeOptimistic(requestId);
      this.turnError.set({
        sessionId,
        text: `发送失败：${messageOf(error)}`,
        retryable: false,
      });
      return false;
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

  // ---- turn failures, retry, waiting, scroll memory (C2/C4/C6/C3) ---------

  /** Does the session wait on a human answer? The open session reads its own
   * projection; any other row reads the ledger-derived list badge. A parked
   * question is not a running task: no spinner belongs on it. */
  waiting(sessionId?: string): boolean {
    const id = sessionId ?? this.activeId();
    if (!id) return false;
    if (this.activeId() === id) return waitingIn(this.conversation());
    return this.sessions().find((row) => row.sessionId === id)?.pendingDecision === true;
  }

  /** The banner follows the open session's newest failed turn. A dismissed turn
   * stays dismissed until a newer failure or an explicit retry. */
  private syncTurnError(sessionId: string, view: ConversationView) {
    const failed = [...view.turns].reverse().find((turn) => turn.status === "failed" && !!turn.error);
    if (!failed || !failed.error) {
      if (this.turnError()?.sessionId === sessionId && this.turnError()?.turnId === undefined) {
        this.turnError.set(null);
      }
      return;
    }
    if (this.dismissedTurnErrors.has(failed.turnId) || this.turnError()?.turnId === failed.turnId) return;
    this.turnError.set({
      sessionId,
      turnId: failed.turnId,
      text: failed.error,
      retryable: failed.visibleInputs.some((input) => input.seq !== undefined),
    });
  }

  /** The banner's close button: a dismissal is per turn — the same failure must
   * not pop back on the next refresh. */
  dismissTurnError() {
    const current = this.turnError();
    if (current?.turnId) this.dismissedTurnErrors.add(current.turnId);
    this.turnError.set(null);
  }

  /** `POST /v1/sessions/{id}/retry {turnId}` — re-ask the input the turn
   * answered, as a new commit. The ledger only grows; history is untouched. */
  async retryTurn(turnId: string): Promise<boolean> {
    const sessionId = this.activeId();
    if (!sessionId || this.retryingTurn()) return false;
    this.retryingTurn.set(turnId);
    try {
      await this.client.retryTurn(sessionId, turnId);
      this.dismissedTurnErrors.delete(turnId);
      if (this.turnError()?.turnId === turnId) this.turnError.set(null);
      this.patchSession(sessionId, { runStatus: "running", pendingDecision: false });
      this.scheduleRefresh();
      return true;
    } catch (error) {
      this.turnError.set({ sessionId, turnId, text: `重试失败：${messageOf(error)}`, retryable: true });
      return false;
    } finally {
      this.retryingTurn.set(null);
    }
  }

  /** Per-session scroll memory (C4): where the reader was and whether they were
   * following the tail. In-memory only, like the rest of the view state. */
  scrollState(sessionId: string): ScrollState | undefined {
    return this.scrollStates.get(sessionId);
  }

  saveScrollState(sessionId: string, state: ScrollState) {
    this.scrollStates.set(sessionId, state);
  }

  async createSession(): Promise<string | null> {
    try {
      // One empty "new session" is enough: creating again while an untouched
      // one exists reopens it instead of appending a twin row.
      const empty = this.sessions().find(
        (row) => !row.lastMessage && (row.title ?? "").trim() === "新会话",
      );
      if (empty) {
        await this.refreshSessions();
        if (this.activeId() !== empty.sessionId) await this.openSession(empty.sessionId);
        return empty.sessionId;
      }
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
