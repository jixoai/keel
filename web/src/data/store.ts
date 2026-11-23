// The app's one store: sessions, the active conversation, the connection state
// and every command the screens issue. Screens read signals; nothing outside
// the data layer talks to fetch. The fixture transport exists for development
// only (`?fixture=1` on the page, `host.params.fixture` elsewhere): a failed
// connection is an offline face with a retry, never a demo standing in for it.
import { host, signal } from "nsh";
import { createCoreEngine, type CoreEngine } from "./core";
import { FixtureClient } from "./fixture";
import { KeelApiError, KeelHttp, errorMessageOf, type FetchLike } from "./http";
import { setEffectivePermissions, resetPermissions } from "./permissions";
import { playApprovalCue } from "./sound";
import type { AuthSessionView, WhoAmI } from "./identity";
import type { IdentityApi } from "./identity-api";
import { selectionForSend } from "./selection";
import type {
  AttachmentRef,
  ConnectionState,
  ConversationView,
  Delivery,
  EventSource,
  FireResultView,
  McpAppAuditEntry,
  McpAppCallToolResult,
  McpAppCatalogueView,
  McpAppMessageOutcome,
  McpAppOpenLinkOutcome,
  McpAppResourceReadResult,
  McpAppToolResult,
  ModelsSettingsView,
  ModelSelection,
  ModelView,
  PluginsView,
  ProviderCredentialSaved,
  ProviderProbeView,
  ProviderView,
  SecretMeta,
  ScrollState,
  SendOptions,
  SessionSummary,
  SessionFilesQuery,
  SessionFilesView,
  SessionTasksView,
  SessionFileView,
  SessionTaskView,
  FilePreviewView,
  TaskPanelView,
  SettingsApi,
  SettingsIssue,
  SkillDetailView,
  SkillRecordView,
  SkillsSettingsView,
  TriggerStatusView,
  TriggersView,
  TurnErrorState,
  UiCommands,
  WireEvent,
} from "./contract";
import { asString, payloadOf, textOf, contentOf } from "./wire";
import { providerCredentialSecretName } from "./model-catalog";
import { refreshDeploymentCapabilities, setDeploymentCapabilities } from "./capabilities";
import {
  mcpAppContextBody,
  mcpAppMessageBody,
  mcpAppOpenLinkBody,
  mcpAppResourceReadBody,
  mcpAppToolCallBody,
  type McpAppContextBody,
  type McpAppMessageBody,
  type McpAppOpenLinkBody,
  type McpAppResourceReadBody,
  type McpAppToolCallBody,
} from "./mcp-apps";

/** The connection state: probing, a live service, or nothing to talk to. There
 * is no demo state — the fixture is a development transport, not a mode the
 * product can present. */
export type Mode = "checking" | "http" | "offline";

// The route model lives in `route.ts` (pure, no transport); the store holds its
// signals and re-exports the words the shell compares.
export type { IdentitySection, Route, RouteState } from "./route";
export { resolveRoute } from "./route";
import { resolveRoute, routeState, routePath, type IdentitySection, type Route, type RouteState } from "./route";

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

/** Everything the store's transport must answer. The identity/access surface
 * (A4.3/A4.4a) is optional: a transport that predates it answers nothing, and
 * the identity pages say so instead of inventing rows. */
type Client = SettingsApi &
  Partial<IdentityApi> & {
  probe(): Promise<"ok" | "auth" | "down">;
  capabilities(): Promise<Record<string, unknown>>;
  /** `GET /v1/mcp/apps`: the app catalogue. Optional because a transport that
   * predates the surface (or a fixture with no app documents) answers nothing;
   * the app host then shows its readable fallback instead of rows it did not
   * read. */
  mcpApps?(): Promise<McpAppCatalogueView>;
  /** Best-effort audit write for an app protocol violation (P3.7 wires the
   * durable store; a transport without it returns false). */
  auditMcpApp?(entry: McpAppAuditEntry): Promise<boolean>;
  /** The View's outbound proxies (P3/P5). Optional: a transport without the
   * surface answers a readable refusal instead of a fake result. */
  mcpAppToolCall?(appId: string, body: McpAppToolCallBody): Promise<McpAppCallToolResult>;
  mcpAppReadResource?(appId: string, body: McpAppResourceReadBody): Promise<McpAppResourceReadResult>;
  mcpAppMessage?(appId: string, body: McpAppMessageBody): Promise<McpAppMessageOutcome>;
  mcpAppOpenLink?(appId: string, body: McpAppOpenLinkBody): Promise<McpAppOpenLinkOutcome>;
  mcpAppUpdateContext?(appId: string, body: McpAppContextBody): Promise<unknown>;
  listSessions(limit?: number): Promise<SessionSummary[]>;
  /** The composer `$` completion sources. Optional: a transport without the
   * projections leaves the file/task groups out instead of inventing rows. */
  sessionFiles?(sessionId: string, query?: SessionFilesQuery): Promise<SessionFilesView>;
  sessionTasks?(sessionId: string): Promise<SessionTasksView>;
  taskPanel?(sessionId: string, memberLimit?: number, eventLimit?: number): Promise<TaskPanelView>;
  filePreview?(sessionId: string, path: string, referenceHash?: string, offset?: number): Promise<FilePreviewView>;
  createSession(): Promise<string>;
  deleteSession(sessionId: string): Promise<void>;
  source(sessionId: string): EventSource;
  commands(sessionId: string): UiCommands;
  tail?(sessionId: string, lastSeq: number, count?: number): Promise<WireEvent[]>;
};

/** The plugin/trigger read seam (`GET /v1/plugins`, `GET /v1/triggers`). The
 * harness answers it so the plugins page's 403 face is exercised through the
 * real store error path instead of a hand-set message. */
type PluginTransport = {
  plugins(): Promise<PluginsView>;
  triggers(): Promise<TriggersView>;
};

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

/** A 429 says how long to wait; every other return is the shared sentence. */
const retrySentenceOf = (error: KeelApiError, fallback: string): string =>
  error.status === 429
    ? error.retryAfterSeconds !== undefined
      ? `尝试次数过多，请在 ${error.retryAfterSeconds} 秒后重试。`
      : fallback
    : fallback;

/** The login gate's error. A 401 is deliberately the *same* sentence for an
 * unknown account and a wrong password: the server does not distinguish them
 * and the UI must not either. */
const loginErrorOf = (error: unknown): string => {
  if (!(error instanceof KeelApiError)) return errorMessageOf(error);
  switch (error.code) {
    case "invalid_credentials":
      return "用户名或密码不正确。请检查后重试。";
    case "csrf_failed":
      return "登录表单的安全令牌已失效，请刷新页面后重试。";
    case "bootstrap_required":
      return "这个部署还没有管理员，请先完成初始化。";
    case "tenant_not_configured":
    case "auth_unavailable":
      return "服务还没有配置好账号登录，请联系超级管理员。";
    case "rate_limited":
      return retrySentenceOf(error, "尝试次数过多，请稍后重试。");
    default:
      return errorMessageOf(error);
  }
};

const bootstrapErrorOf = (error: unknown): string => {
  if (!(error instanceof KeelApiError)) return errorMessageOf(error);
  switch (error.code) {
    case "invalid_setup_token":
      return "设置令牌无效、已过期或已经用过。请在服务器上重新运行 keel admin bootstrap。";
    case "bootstrap_complete":
      return "这个部署已经有管理员了，请直接登录。";
    case "username_taken":
      return "这个用户名已被占用，请换一个。";
    case "weak_password":
      return "密码不符合要求：至少 8 个字符，最多 1024 字节。";
    case "tenant_not_configured":
      return "服务还没有配置租户，请联系超级管理员。";
    case "csrf_failed":
      return "设置表单的安全令牌已失效，请刷新页面后重试。";
    default:
      return errorMessageOf(error);
  }
};

const passwordChangeErrorOf = (error: unknown): string => {
  if (!(error instanceof KeelApiError)) return errorMessageOf(error);
  switch (error.code) {
    case "invalid_credentials":
      return "当前密码不正确。";
    case "weak_password":
      return "新密码不符合要求：至少 8 个字符，最多 1024 字节。";
    case "password_reused":
      return "新密码不能和当前密码相同。";
    case "invalid_session":
      return "登录已失效，请重新登录后再改密码。";
    case "rate_limited":
      return retrySentenceOf(error, "尝试次数过多，请稍后重试。");
    default:
      return errorMessageOf(error);
  }
};

/** The 12-character hash a skill row shows; the full hash stays available in
 * the detail dialog (a bundle identity is 64 hex characters). */
const shortHash = (hash: string): string => (hash.length > 12 ? hash.slice(0, 12) : hash);

/** A skill write failure with the server's code (pinned/untrusted_source/...),
 * so the page can name the state instead of a generic error. */
const actionErrorOf = (error: unknown): { code?: string; message: string } =>
  error instanceof KeelApiError
    ? { code: error.code, message: error.message }
    : { message: messageOf(error) };

/** Newest first by `lastMessageAt` — the same clock the row displays, so the
 * order and the "3 分钟前" beside it can never contradict each other (sorting by
 * `createdAt` while printing `lastMessageAt` put a session that moved five
 * seconds ago at the bottom). Ties keep the server's order. */
const sortNewestFirst = (rows: SessionSummary[]): SessionSummary[] => {
  const stamp = (row: SessionSummary) => {
    const raw = row.lastMessageAt ?? row.createdAt ?? "";
    const at = Date.parse(raw);
    return Number.isFinite(at) ? at : 0;
  };
  return [...rows]
    .map((row, index) => ({ row, index }))
    .sort((a, b) => stamp(b.row) - stamp(a.row) || a.index - b.index)
    .map((item) => item.row);
};

/** A run status that may still be waiting on an answer: only those rows are
 * re-read for the pending-decision badge. */
const runningLike = (runStatus: string | undefined): boolean => {
  switch ((runStatus ?? "").toLowerCase()) {
    case "running":
    case "busy":
    case "stopping":
    case "cancelling":
    case "canceling":
      return true;
    default:
      return false;
  }
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
  /** The login gate's stage (change keel-bot-identity-access, A4.3):
   *   unknown      the boot probe has not answered yet
   *   ok           an authenticated session; the app renders
   *   required     username+password login
   *   bootstrap    no super admin: the one-time setup form
   *   must-change  logged in, but the server requires a new password first
   * A 401 anywhere turns this back to `required`; nothing renders the app
   * behind a dead session. */
  readonly auth = signal<"unknown" | "ok" | "required" | "bootstrap" | "must-change">("unknown");
  readonly authError = signal<string | null>(null);
  /** A sentence for the gate that is not an error (session expired, setup
   * finished, password changed). */
  readonly authNotice = signal<string | null>(null);
  /** A 429's Retry-After, so the gate can count the wait down. */
  readonly authRetryAfter = signal<number | null>(null);
  /** The last gate failure was a permission refusal (403, not CSRF): the
   * account is known but may not use this surface. */
  readonly authDenied = signal(false);
  /** The caller's identity and effective permissions (whoami); `null` until
   * the first successful read, and on the login gate. */
  readonly identity = signal<WhoAmI | null>(null);
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

  /** The top-level route: `/settings`, `/plugins` and the dev-only `/kit`
   * deep-link straight into their screens; the workspace stays otherwise
   * untouched. The books chapters (`/identity`, `/records`, `/bots`, `/teams`)
   * arrive in A4.4a. */
  readonly route = signal<Route>("workspace");
  /** The route's own parameters (the identity sub-page and the open object).
   * Kept apart from `route` so the chapter comparisons above stay one word. */
  readonly routeSection = signal<IdentitySection>("accounts");
  readonly routeObjectId = signal<string | null>(null);
  /** The open team run (the captain session id) below a team page. */
  readonly routeRunId = signal<string | null>(null);

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

  /** The provider probe's last measured answer, per provider id
   * (`POST /v1/providers/{id}/test`): the live line-up and the health state.
   * Absent = never probed this session. */
  readonly providerProbes = signal<Record<string, ProviderProbeView>>({});
  /** The provider whose probe is in flight, and the last probe that failed to
   * run at all (a 404/501/transport failure, distinct from a measured
   * `ok:false`). */
  readonly probingProvider = signal<string | null>(null);
  readonly probeError = signal<{ providerId: string; message: string } | null>(null);
  /** The provider whose direct credential write is in flight, and its last
   * failure. */
  readonly credentialSaving = signal<string | null>(null);
  readonly credentialError = signal<{ providerId: string; message: string } | null>(null);

  // ---- skill library (change keel-harness-v2, W7 skills) ------------------

  readonly skills = signal<SkillsSettingsView | null>(null);
  readonly skillsLoading = signal(false);
  readonly skillsError = signal<string | null>(null);
  readonly skillsErrorCode = signal<string | null>(null);
  readonly skillsBusy = signal(false);
  /** A write failure with the server's stable code (`pinned`,
   * `untrusted_source`, ...), so the page renders the specific state. */
  readonly skillsActionError = signal<{ code?: string; message: string } | null>(null);
  readonly skillsDetail = signal<SkillDetailView | null>(null);
  readonly skillsDetailName = signal<string | null>(null);
  readonly skillsDetailLoading = signal(false);
  readonly skillsDetailError = signal<string | null>(null);
  /** The composer `$` menu's three groups. They are loaded lazily, keep the
   * last session's answer, and never invent a row when a source is absent. */
  readonly completionSkills = signal<SkillRecordView[]>([]);
  readonly completionFiles = signal<SessionFileView[]>([]);
  readonly completionTasks = signal<SessionTaskView[]>([]);
  readonly completionLoading = signal(false);
  readonly completionError = signal<string | null>(null);
  readonly completionSession = signal<string | null>(null);

  readonly plugins = signal<PluginsView | null>(null);
  readonly pluginsLoading = signal(false);
  readonly pluginsError = signal<string | null>(null);
  /** As `agentsErrorStatus`: the 403 face is driven by the response status,
   * not by matching a localized message. */
  readonly pluginsErrorStatus = signal<number | null>(null);
  readonly triggers = signal<TriggerStatusView[]>([]);
  readonly firingTrigger = signal<string | null>(null);
  /** The last manual fire's outcome, per trigger id. */
  readonly fireOutcome = signal<{ triggerId: string; ok: boolean; text: string } | null>(null);

  /** The open session's model selection (an input fact, separate from the
   * tenant default in the settings document). */
  readonly sessionSelection = signal<ModelSelection | null>(null);

  /** The development-only transport; constructed only when a dev launch asked
   * for it, so a product run never seeds any demo state. */
  private fixture: FixtureClient | null = null;
  private http: KeelHttp | null = null;
  /** Test/host seam for the plugin reads; `null` uses the app's transport. */
  private pluginClient: PluginTransport | null = null;
  /** Test/host seam for the skill library (the settings harness answers it in
   * memory so the real `KeelHttp` parsing runs without a network). `null` uses
   * the app's transport. */
  private skillsClient: SettingsApi | null = null;
  /** Bumped per boot probe; a probe that loses the race must not overwrite a
   * stage a caller (a harness, a reconnect) has since decided. */
  private detectToken = 0;
  /** Test/host seam: the transport the boot probe reads. `null` builds the
   * real one, exactly as the product does. */
  private probeClient: KeelHttp | null = null;
  /** The transport every call goes through. A real HTTP client from the start:
   * before (and without) a live service its calls fail with the network's own
   * reason — the store never swaps in a fabricated one. */
  private client: Client = new KeelHttp("");
  /** Test/host seam for the identity/access surface (the identity harness
   * answers it in memory so the real parsing runs without a network). `null`
   * uses the app's own transport. */
  private identityClient: IdentityApi | null = null;
  /** The identity invalidation stream's lifecycle hooks (identity-stream.ts
   * installs them when it loads). The store owns the session: it starts the
   * stream after whoami answers (any route, not just the books shell) and closes
   * it on every path that ends a session — logout, a 401, an account switch —
   * without importing the stream module (which imports us). */
  private identityStreamCloser: (() => void) | null = null;
  private identityStreamStarter: (() => void) | null = null;
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
  /** The turn whose failure the reader closed (C6): dismissal hides the banner
   * for the open session view; re-opening the session shows the failure again,
   * because the turn is still the session's newest state. */
  private dismissedTurnError: string | null = null;
  /** Decisions already announced (C13): a reconnect replay must not re-notify. */
  private notifiedDecisions = new Set<string>();
  /** Set as soon as the user navigates by hand, so the boot route read never
   * overrides an explicit move. */
  private routeTouched = false;
  /** True once the host answered as the web page host (route plumbing on). */
  private routePlumbed = false;
  /** The page's route as the web host answered it (`keel.route`), read once and
   * shared by the route boot and the dev-launch read. `null` = not the web page
   * host (TUI, desktop, tests), where the route stays in-app. */
  private pageRouteRequest: Promise<{ path: string; search: string } | null> | null = null;
  /** A development launch (`?fixture=1` / `host.params.fixture`): the one way
   * the fixture transport is used. */
  private dev = false;
  /** The W12 slot fixture plugin is requested (`?plugins=1` /
   * `host.params.plugins`): only the slot tests and the browser evidence ask
   * for it; the default fixture stays the product-shaped sample. */
  private pluginFixtures = false;
  /** The web page's query as read for the dev launch (`?fixture=1`). */
  private devSearch = "";
  private devRequest: Promise<boolean> | null = null;

  constructor() {
    void this.detect();
    void this.initRoute();
  }

  /** The page's route from the web host, read once: `keel.info` gates it so
   * only the real web host (which answers both) takes part — native hosts and
   * test hosts keep their routes fully in-app. */
  private pageRoute(): Promise<{ path: string; search: string } | null> {
    this.pageRouteRequest ??= this.readPageRoute();
    return this.pageRouteRequest;
  }

  private async readPageRoute(): Promise<{ path: string; search: string } | null> {
    if (!host.available()) return null;
    try {
      const info = await host.call<{ origin?: string }>("keel.info", {}, { timeout: 1200 });
      if (typeof info?.origin !== "string" || !info.origin) return null;
    } catch {
      return null;
    }
    try {
      const answer = await host.call<{ path?: string; search?: string }>("keel.route", {}, { timeout: 1200 });
      return {
        path: typeof answer?.path === "string" ? answer.path : "/",
        search: typeof answer?.search === "string" ? answer.search : "",
      };
    } catch {
      return null;
    }
  }

  /** Did this launch ask for the development transport? The embedding app says
   * so in its launch parameters (`host.params.fixture === true` — the TUI host
   * and the tests); the web page says so in its own query string
   * (`?fixture=1`). Nothing else turns it on. */
  private devLaunch(): Promise<boolean> {
    this.devRequest ??= this.readDevLaunch();
    return this.devRequest;
  }

  private async readDevLaunch(): Promise<boolean> {
    if (host.params?.fixture === true) {
      this.dev = true;
      this.pluginFixtures = host.params?.plugins === true;
      return true;
    }
    const page = await this.pageRoute();
    this.devSearch = page?.search ?? "";
    const params = new URLSearchParams(this.devSearch);
    this.dev = params.get("fixture") === "1";
    this.pluginFixtures = params.get("plugins") === "1";
    return this.dev;
  }

  private async detect() {
    const token = ++this.detectToken;
    this.sessionsLoading.set(true);
    this.authError.set(null);
    this.authDenied.set(false);
    this.authNotice.set(null);
    this.authRetryAfter.set(null);
    // Development only: the scripted transport stands in for a live service.
    // The product never reaches this branch — it is asked for at launch.
    if (await this.devLaunch()) {
      if (token !== this.detectToken) return;
      if (!this.fixture) this.fixture = new FixtureClient(undefined, this.pluginFixtures);
      this.http = null;
      this.client = this.fixture;
      resetPermissions();
      // The fixture also answers whoami, so the identity chapters, `can()` and
      // the ledger's signatory line are exercised on the development transport
      // exactly as they are against a live deployment. Nothing here is product
      // state: a product launch never reaches this branch.
      const me = await this.fixture.whoami().catch(() => null);
      if (me) {
        this.identity.set(me);
        setEffectivePermissions(me.permissions, me.isSuperAdmin);
      } else {
        this.identity.set(null);
      }
      this.auth.set("ok");
      this.mode.set("http");
      // The fixture answers the same discovery document shape a live server
      // does, so capability-gated UI is exercised (and can honestly say what
      // this development transport can do).
      setDeploymentCapabilities(await this.fixture.capabilities().catch(() => null));
      await this.refreshSessions();
      return;
    }
    const http = this.probeClient ?? new KeelHttp("");
    this.installUnauthorized(http);
    this.http = http;
    this.client = http;
    let session: AuthSessionView;
    try {
      // The session document is the one public read that says both "is there a
      // live service?" and "is this browser logged in / does the deployment
      // still need bootstrap?". A 401 from /v1/capabilities could not tell the
      // third state apart from an outage.
      session = await http.authSession();
    } catch (error) {
      if (token !== this.detectToken) return;
      // A stale/invalid cookie is not an outage: the server answered, and the
      // login form is the way forward (the server already cleared its cookies).
      if (error instanceof KeelApiError && error.status === 401) {
        this.mode.set("http");
        this.auth.set("required");
        this.authNotice.set("会话已过期，请重新登录。");
        this.sessionsLoading.set(false);
        setDeploymentCapabilities(null);
        return;
      }
      // An honest offline face: the transport stays a real one (its calls fail
      // with the network's own reason) and the reason stands with a retry.
      this.mode.set("offline");
      this.auth.set("unknown");
      this.identity.set(null);
      resetPermissions();
      setDeploymentCapabilities(null);
      this.sessionsError.set(`无法连接 Keel 服务：${errorMessageOf(error)}。确认服务已启动后重试。`);
      this.sessionsLoading.set(false);
      return;
    }
    this.mode.set("http");
    this.sessionsError.set(null);
    if (session.authenticated) {
      await this.finishSignIn(http, session, token);
      return;
    }
    if (token !== this.detectToken) return;
    this.sessionsLoading.set(false);
    setDeploymentCapabilities(null);
    this.identity.set(null);
    resetPermissions();
    if (session.bootstrapRequired) {
      this.auth.set("bootstrap");
      return;
    }
    this.auth.set("required");
  }

  /** Record the session as the app's identity while whoami is still loading
   * (and behind the must-change gate). Permissions stay empty until whoami
   * answers; the gate is fail-closed. */
  private setSessionIdentity(session: AuthSessionView) {
    this.identity.set({
      tenantId: session.tenantId ?? "",
      accountId: session.accountId ?? "",
      username: session.username,
      displayName: session.displayName,
      kind: "human",
      disabled: false,
      isSuperAdmin: session.isSuperAdmin,
      botId: session.botId,
      permissions: {},
    });
  }

  /** A successful login/password answer: route the must-change gate, or load
   * the app (capabilities, whoami, the first session list). */
  private async finishSignIn(http: KeelHttp, session: AuthSessionView, token?: number) {
    if (token !== undefined && token !== this.detectToken) return;
    this.http = http;
    this.client = http;
    this.mode.set("http");
    this.sessionsError.set(null);
    this.authError.set(null);
    this.authDenied.set(false);
    this.authRetryAfter.set(null);
    this.setSessionIdentity(session);
    if (session.mustChangePassword) {
      this.auth.set("must-change");
      this.sessionsLoading.set(false);
      resetPermissions();
      return;
    }
    this.authNotice.set(null);
    this.auth.set("ok");
    this.sessionsLoading.set(true);
    // The boot probe stopped at the login gate, so it never read
    // /v1/capabilities (401). Read it now, before the auth gate succeeds and
    // re-renders: an app-bearing tool result already in a session must mount
    // its View without a full page reload. A failed read keeps the previous
    // (fail-closed) snapshot and is recorded — it never pretends.
    const capabilities = await refreshDeploymentCapabilities(http);
    if (token !== undefined && token !== this.detectToken) return;
    if (!capabilities.ok) {
      console.warn(`[keel] deployment capabilities unavailable after login: ${capabilities.error}`);
      this.flash({ tone: "warn", text: "登录成功，但未能读取服务能力，交互界面暂以文本显示。" });
    }
    await this.loadIdentity();
    if (this.auth() !== "ok") return; // whoami said the session had ended
    if (token !== undefined && token !== this.detectToken) return;
    await this.refreshSessions();
  }

  /** Read whoami and record the effective permissions `can()` reads. A failed
   * read clears them (fail closed: no action is offered from a guess). */
  async loadIdentity(): Promise<void> {
    const http = this.http;
    if (!http) return;
    try {
      const me = await http.whoami();
      this.identity.set(me);
      setEffectivePermissions(me.permissions, me.isSuperAdmin);
    } catch (error) {
      this.identity.set(null);
      resetPermissions();
      if (!(error instanceof KeelApiError && error.status === 401)) {
        console.warn(`[keel] whoami unavailable: ${errorMessageOf(error)}`);
      }
    } finally {
      // whoami is the authoritative `app.read` answer: the stream starts once
      // it is held and stops if it was revoked (the stream's own flush calls
      // this too, through the same hook).
      this.identityStreamStarter?.();
    }
  }

  /** Exchange username+password for the server session (login gate). */
  async login(username: string, password: string): Promise<boolean> {
    const http = this.http ?? new KeelHttp("");
    this.installUnauthorized(http);
    this.http = http;
    this.client = http;
    this.authError.set(null);
    this.authDenied.set(false);
    this.authNotice.set(null);
    this.authRetryAfter.set(null);
    try {
      const session = await http.login(username.trim(), password);
      await this.finishSignIn(http, session);
      return true;
    } catch (error) {
      this.authError.set(loginErrorOf(error));
      this.authDenied.set(error instanceof KeelApiError && error.status === 403 && error.code !== "csrf_failed");
      if (error instanceof KeelApiError) {
        if (error.status === 429) this.authRetryAfter.set(error.retryAfterSeconds ?? null);
        if (error.code === "bootstrap_required") this.auth.set("bootstrap");
      }
      return false;
    }
  }

  /** Redeem the one-time setup token and open the first super admin. The
   * password chosen here is final, so the immediate login lands in the app
   * (`auth=ok`); only a CLI/admin reset produces a must-change account. */
  async bootstrap(token: string, username: string, password: string, displayName?: string): Promise<boolean> {
    const http = this.http ?? new KeelHttp("");
    this.installUnauthorized(http);
    this.http = http;
    this.client = http;
    this.authError.set(null);
    this.authDenied.set(false);
    this.authNotice.set(null);
    this.authRetryAfter.set(null);
    try {
      await http.bootstrap(token.trim(), username.trim(), password, displayName);
    } catch (error) {
      this.authError.set(bootstrapErrorOf(error));
      this.authDenied.set(error instanceof KeelApiError && error.status === 403 && error.code !== "csrf_failed");
      if (error instanceof KeelApiError && error.code === "bootstrap_complete") this.auth.set("required");
      return false;
    }
    const signedIn = await this.login(username, password);
    if (!signedIn) {
      this.auth.set("required");
      this.authNotice.set("管理员已创建，但自动登录没有成功。请用刚设置的密码登录。");
      return true;
    }
    return true;
  }

  /** The must-change gate's one write. The server revokes the old session and
   * answers a fresh one, so the gate opens on the same response. A failure
   * leaves the stage at must-change: the way forward is a new password. */
  async changePassword(currentPassword: string, newPassword: string): Promise<boolean> {
    const http = this.http;
    if (!http) {
      this.authError.set("还没有连接到 Keel 服务，请刷新页面后重试。");
      return false;
    }
    this.authError.set(null);
    this.authDenied.set(false);
    this.authRetryAfter.set(null);
    try {
      const session = await http.changePassword(currentPassword, newPassword);
      await this.finishSignIn(http, session);
      this.flash({ tone: "success", text: "密码已更新。" });
      return true;
    } catch (error) {
      this.authError.set(passwordChangeErrorOf(error));
      this.authDenied.set(error instanceof KeelApiError && error.status === 403 && error.code !== "csrf_failed");
      if (error instanceof KeelApiError && error.status === 429) {
        this.authRetryAfter.set(error.retryAfterSeconds ?? null);
      }
      return false;
    }
  }

  /** Clear the server session and every local trace of it. The server owns the
   * cookies; the local state is cleared even if the request fails. */
  async logout(): Promise<void> {
    const http = this.http;
    try {
      await http?.logout();
    } catch {
      /* the local gate still closes: cookies are the server's, not ours */
    }
    this.clearSessionState();
    this.auth.set("required");
    this.authNotice.set("已安全退出登录。");
    // The logout response cleared the CSRF cookie with the session; a fresh
    // anonymous token is what the next login POST needs.
    try {
      await http?.authSession();
    } catch {
      /* the login form will read it again */
    }
  }

  /** Drop everything scoped to the signed-in account, so a login page never
   * shows the previous account's rows behind it. */
  private clearSessionState() {
    this.identityStreamCloser?.();
    this.closeStream();
    this.sessions.set([]);
    this.activeId.set(null);
    this.conversation.set(null);
    this.identity.set(null);
    resetPermissions();
    this.sessionsError.set(null);
    this.sessionsLoading.set(false);
    this.resetSettingsState();
    this.http?.clearAuth();
  }

  /** Any 401 from a real endpoint is the session ending (expired, revoked,
   * disabled). The app returns to the login gate with a reason instead of
   * leaving a blank list behind. */
  private installUnauthorized(http: KeelHttp) {
    http.onUnauthorized((error) => {
      if (error.status !== 401) return;
      if (this.auth() === "unknown") return; // the boot probe owns that answer
      this.clearSessionState();
      this.auth.set("required");
      this.authNotice.set("会话已过期，请重新登录。");
    });
  }

  /**
   * Probe the server again (the settings page's "重试连接", the list's 重试).
   * The answer decides where we land: a live server, the login gate, or the
   * offline face — never a silently fabricated one.
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

  // ---- route --------------------------------------------------------------

  /**
   * Read the route once at boot: a web page served at `/settings` (the SPA
   * fallback), `/plugins` or `/kit` (dev) starts there; every other host starts
   * on the workspace. The read is the host's (`keel.route` in host.js), because
   * the app's JS sandbox has no `location` of its own — `pageRoute` gates it on
   * `keel.info`, so only the real web host takes part.
   */
  private async initRoute() {
    const [page] = await Promise.all([this.pageRoute(), this.devLaunch()]);
    if (!page) return;
    this.routePlumbed = true;
    if (!this.routeTouched) this.applyRoute(page.path);
    try {
      // Browser back/forward: the URL is authoritative again.
      host.on<{ path?: string; search?: string }>("keel.route", (payload) => {
        const next = typeof payload?.path === "string" ? payload.path : "";
        this.applyRoute(next);
      });
    } catch {
      /* a host without events is fine: routes stay in-app */
    }
  }

  /** Land on a path: the chapter and its parameters move together, so a deep
   * link and the back button cannot leave a chapter showing another object. */
  private applyRoute(path: string) {
    const state = resolveRoute(path, this.dev);
    this.route.set(state.route);
    this.routeSection.set(state.section);
    this.routeObjectId.set(state.objectId);
    this.routeRunId.set(state.runId);
  }

  /** Move to a route in-app and keep the page URL in step. The state is set
   * before the host call: a host without a URL (TUI, desktop, tests) still
   * navigates, it just does not change an address bar. */
  private go(state: RouteState) {
    this.routeTouched = true;
    this.route.set(state.route);
    this.routeSection.set(state.section);
    this.routeObjectId.set(state.objectId);
    this.routeRunId.set(state.runId);
    this.navigate(routePath(state));
  }

  /** Keep the page URL in step with the shell (`/settings`, `/plugins` and the
   * dev-only `/kit` are real routes; a host without a URL — TUI, desktop,
   * tests — just ignores it). */
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
    this.go(routeState("settings"));
  }

  /** The plugin page (`/plugins`), the same route the settings page's
   * 「打开插件页」 enters: the address bar and the back button follow it, and a
   * `/plugins` deep link opens it. */
  openPlugins() {
    this.routeTouched = true;
    if (this.route() === "plugins") return;
    this.go(routeState("plugins"));
  }

  /** Back to the workspace; the open session is preserved (the store keeps it
   * across the shell swap). Works from every non-workspace route. */
  closeSettings() {
    this.routeTouched = true;
    if (this.route() === "workspace") return;
    this.go(routeState("workspace"));
  }

  // ---- the books chapters (change keel-bot-identity-access, A4.4a/A4.4b) ---

  /** Enter a chapter. Identity keeps whichever sub-page the reader was on; the
   * object chapters open their index (a deep link keeps its own object). */
  openChapter(route: "workspace" | "bots" | "teams" | "identity" | "records") {
    const section: IdentitySection = route === "identity" ? this.routeSection() : "accounts";
    this.go(routeState(route, section, null));
  }

  /** One Bot page (`/bots/<id>`). */
  openBot(botId: string) {
    this.go(routeState("bots", "accounts", botId));
  }

  /** Back from a Bot page to the Bot index. */
  closeBot() {
    this.go(routeState("bots"));
  }

  /** One TeamBot page (`/teams/<id>`). */
  openTeam(teamId: string) {
    this.go(routeState("teams", "accounts", teamId));
  }

  /** One team run (`/teams/<id>/runs/<captainSessionId>`). The run id is the
   * captain session's id: it is the only stable name the server gives a run. */
  openTeamRun(teamId: string, runId: string) {
    this.go(routeState("teams", "accounts", teamId, runId));
  }

  /** Back from a run to the team page. */
  closeTeamRun() {
    if (this.route() !== "teams") return;
    this.go(routeState("teams", "accounts", this.routeObjectId(), null));
  }

  /** Back from a TeamBot page to the team index. */
  closeTeam() {
    this.go(routeState("teams"));
  }

  /** 账号 / 权限组 — the identity index column's segmented control. */
  openIdentitySection(section: IdentitySection) {
    if (this.route() === "identity" && this.routeSection() === section && this.routeObjectId() === null) return;
    this.go(routeState("identity", section, null));
  }

  /** One account/group page (`/identity/<section>/<id>`). */
  openIdentityObject(section: IdentitySection, objectId: string) {
    this.go(routeState("identity", section, objectId));
  }

  /** Back from an object page to its index (the chapter stays). */
  closeIdentityObject() {
    this.go(routeState("identity", this.routeSection(), null));
  }

  /** One record (`/records/<id>`). */
  openRecord(objectId: string | null) {
    this.go(routeState("records", "accounts", objectId));
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
        `配置版本 ${saved.revision} · 可执行 Provider ${saved.providersExecutable ?? "-"} · 可选模型 ${
          saved.modelsSelectable ?? "-"
        }`,
      );
      this.flash({ tone: "success", text: `模型配置已保存并生效（配置版本 ${saved.revision}）` });
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
      this.settingsSavedSummary.set(`配置版本 ${reset.revision} · 部署默认`);
      this.flash({ tone: "success", text: `已重置为部署默认（配置版本 ${reset.revision}）` });
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

  // ---- provider probe + direct credential ---------------------------------

  /** `POST /v1/providers/{id}/test`: one measured probe. A provider that ran
   * and failed is a RESULT (`ok:false` with the measured status and issues),
   * not an error; a probe that could not run (404/501/transport) is recorded
   * in `probeError` so the dialog can say which. The credential never reaches
   * the client — the server resolves it. */
  async probeProvider(providerId: string): Promise<ProviderProbeView | null> {
    if (this.probingProvider()) return null;
    this.probingProvider.set(providerId);
    this.probeError.set(null);
    try {
      const probe = await this.client.testProvider(providerId);
      this.providerProbes.set({ ...this.providerProbes(), [providerId]: probe });
      return probe;
    } catch (error) {
      this.probeError.set({ providerId, message: messageOf(error) });
      return null;
    } finally {
      this.probingProvider.set(null);
    }
  }

  /** `PUT /v1/settings/providers/{id}/credential`: store a directly pasted
   * credential and get the reference back. The value is sent once and is never
   * kept in a signal, echoed or rendered; the returned reference is what the
   * editor puts into the document. */
  async saveProviderCredential(providerId: string, value: string): Promise<ProviderCredentialSaved | null> {
    if (this.credentialSaving()) return null;
    this.credentialSaving.set(providerId);
    this.credentialError.set(null);
    try {
      const saved = await this.client.saveProviderCredential(providerId, value);
      // The secrets list may now carry a new row (provider_<id>): refresh it
      // so the 密钥 section and the editor's preview agree.
      void this.loadSecrets();
      this.flash({ tone: "success", text: `凭据已保存：${saved.secretRef}（值不回显）` });
      return saved;
    } catch (error) {
      this.credentialError.set({ providerId, message: messageOf(error) });
      return null;
    } finally {
      this.credentialSaving.set(null);
    }
  }

  /** The stored preview of a provider's direct credential (the row the server
   * derives for the id: `provider_<id>`, with a digest suffix when the id had
   * to be sanitized), or undefined when nothing is stored under that name. */
  providerCredentialPreview(providerId: string): string | undefined {
    const expected = providerCredentialSecretName(providerId);
    return this.secrets().find((item) => item.name === expected)?.preview;
  }

  // ---- skill library ------------------------------------------------------

  /** `GET /v1/settings/skills`. `check` reads each source (the server's
   * default) and fills update/conflict state; false is the fast local read the
   * composer uses. */
  async loadSkillsPanel(check = true) {
    if (this.skillsLoading()) return;
    this.skillsLoading.set(true);
    this.skillsError.set(null);
    this.skillsErrorCode.set(null);
    try {
      this.skills.set(await this.skillsApi().skillsSettings(check));
    } catch (error) {
      // A failed read is not a stale list: clear it so the error face is the
      // whole answer.
      this.skills.set(null);
      this.skillsError.set(messageOf(error));
      if (error instanceof KeelApiError) this.skillsErrorCode.set(error.code ?? null);
    } finally {
      this.skillsLoading.set(false);
    }
  }

  /** One skill's body and manifest (`GET /v1/settings/skills/{name}`). */
  async openSkillDetail(name: string) {
    this.skillsDetailName.set(name);
    this.skillsDetailLoading.set(true);
    this.skillsDetailError.set(null);
    this.skillsDetail.set(null);
    try {
      this.skillsDetail.set(await this.skillsApi().showSkill(name));
    } catch (error) {
      this.skillsDetailError.set(messageOf(error));
    } finally {
      this.skillsDetailLoading.set(false);
    }
  }

  closeSkillDetail() {
    this.skillsDetail.set(null);
    this.skillsDetailName.set(null);
    this.skillsDetailError.set(null);
  }

  /** Install the convenience form (name/description/body). A bundle upload is
   * the CLI's `--dir`; the page's create form is the same skills.manage create
   * shape, so the two surfaces cannot drift. */
  async installSkillFromForm(name: string, description: string, body: string): Promise<boolean> {
    if (this.skillsBusy()) return false;
    this.skillsBusy.set(true);
    this.skillsActionError.set(null);
    try {
      const record = await this.skillsApi().installSkill({ name: name.trim(), description, body });
      this.flash({ tone: "success", text: `技能已保存：${record.name}（${shortHash(record.currentHash)}）` });
      await this.loadSkillsPanel(false);
      return true;
    } catch (error) {
      this.skillsActionError.set(actionErrorOf(error));
      return false;
    } finally {
      this.skillsBusy.set(false);
    }
  }

  /** Install a skill that already exists in a subscribed source. It is the
   * settings face of `keel skills install <name> --source <id>`; an untrusted
   * source still needs the explicit acknowledgement. */
  async installSkillFromSource(name: string, sourceId: string, allowUntrusted: boolean): Promise<boolean> {
    if (this.skillsBusy()) return false;
    this.skillsBusy.set(true);
    this.skillsActionError.set(null);
    try {
      const record = await this.skillsApi().installSkill({
        name: name.trim(),
        sourceId,
        allowUntrusted,
      });
      this.flash({ tone: "success", text: `已从 ${sourceId} 安装 ${record.name}` });
      await this.loadSkillsPanel(true);
      return true;
    } catch (error) {
      this.skillsActionError.set(actionErrorOf(error));
      return false;
    } finally {
      this.skillsBusy.set(false);
    }
  }

  /** Advance a skill to its source's current bundle. A pinned skill answers
   * 409 `pinned`: the page shows that code instead of a generic failure. */
  async updateSkill(name: string): Promise<boolean> {
    if (this.skillsBusy()) return false;
    this.skillsBusy.set(true);
    this.skillsActionError.set(null);
    try {
      const result = await this.skillsApi().updateSkill(name);
      const tone = result.status === "up_to_date" ? "info" : "success";
      const text =
        result.status === "up_to_date"
          ? `${name} 已是最新版本`
          : `${name} 已更新到 ${shortHash(result.record?.currentHash ?? "")}`;
      this.flash({ tone, text });
      await this.loadSkillsPanel(true);
      return true;
    } catch (error) {
      this.skillsActionError.set(actionErrorOf(error));
      return false;
    } finally {
      this.skillsBusy.set(false);
    }
  }

  async pinSkill(name: string, hash?: string): Promise<boolean> {
    if (this.skillsBusy()) return false;
    this.skillsBusy.set(true);
    this.skillsActionError.set(null);
    try {
      const record = await this.skillsApi().pinSkill(name, hash);
      this.flash({ tone: "success", text: `${name} 已固定在 ${shortHash(record.pinnedHash ?? "")}` });
      await this.loadSkillsPanel(false);
      return true;
    } catch (error) {
      this.skillsActionError.set(actionErrorOf(error));
      return false;
    } finally {
      this.skillsBusy.set(false);
    }
  }

  async unpinSkill(name: string): Promise<boolean> {
    if (this.skillsBusy()) return false;
    this.skillsBusy.set(true);
    this.skillsActionError.set(null);
    try {
      await this.skillsApi().unpinSkill(name);
      this.flash({ tone: "success", text: `${name} 已解除固定` });
      await this.loadSkillsPanel(false);
      return true;
    } catch (error) {
      this.skillsActionError.set(actionErrorOf(error));
      return false;
    } finally {
      this.skillsBusy.set(false);
    }
  }

  async removeSkill(name: string): Promise<boolean> {
    if (this.skillsBusy()) return false;
    this.skillsBusy.set(true);
    this.skillsActionError.set(null);
    try {
      await this.skillsApi().removeSkill(name);
      this.flash({ tone: "success", text: `技能已移除：${name}` });
      await this.loadSkillsPanel(false);
      return true;
    } catch (error) {
      this.skillsActionError.set(actionErrorOf(error));
      return false;
    } finally {
      this.skillsBusy.set(false);
    }
  }

  async subscribeSkillSource(id: string, url: string, trust: string): Promise<boolean> {
    if (this.skillsBusy()) return false;
    this.skillsBusy.set(true);
    this.skillsActionError.set(null);
    try {
      const source = await this.skillsApi().subscribeSkillSource({ id: id.trim(), kind: "local", url: url.trim(), trust });
      this.flash({ tone: "success", text: `已订阅 ${source.id}（${source.trust}）` });
      await this.loadSkillsPanel(true);
      return true;
    } catch (error) {
      this.skillsActionError.set(actionErrorOf(error));
      return false;
    } finally {
      this.skillsBusy.set(false);
    }
  }

  async unsubscribeSkillSource(id: string): Promise<boolean> {
    if (this.skillsBusy()) return false;
    this.skillsBusy.set(true);
    this.skillsActionError.set(null);
    try {
      await this.skillsApi().unsubscribeSkillSource(id);
      this.flash({ tone: "success", text: `已取消订阅 ${id}` });
      await this.loadSkillsPanel(true);
      return true;
    } catch (error) {
      this.skillsActionError.set(actionErrorOf(error));
      return false;
    } finally {
      this.skillsBusy.set(false);
    }
  }

  /** The composer `$` menu's data: skills (fast read), files and tasks of the
   * open session. A missing transport leaves that group out; a failed read is
   * recorded, never presented as "no rows". */
  async loadCompletion(sessionId: string) {
    if (!sessionId) return;
    this.completionLoading.set(true);
    this.completionError.set(null);
    this.completionSession.set(sessionId);
    try {
      const [skillsView, filesView, tasksView] = await Promise.all([
        this.skillsApi().skillsSettings(false).catch(() => null),
        this.client.sessionFiles ? this.client.sessionFiles(sessionId).catch(() => null) : Promise.resolve(null),
        this.client.sessionTasks ? this.client.sessionTasks(sessionId).catch(() => null) : Promise.resolve(null),
      ]);
      // A slow answer for a session the reader already left must not land: the
      // menu's rows are session-scoped, exactly like the transcript.
      if (this.completionSession() !== sessionId) return;
      this.completionSkills.set(skillsView?.skills ?? []);
      this.completionFiles.set(filesView?.files ?? []);
      this.completionTasks.set(tasksView?.tasks ?? []);
      if (!skillsView && !filesView && !tasksView) {
        this.completionError.set("补全数据不可用");
      }
    } finally {
      this.completionLoading.set(false);
    }
  }

  // ---- plugins & triggers -------------------------------------------------

  async loadPlugins() {
    if (this.pluginsLoading()) return;
    this.pluginsLoading.set(true);
    this.pluginsError.set(null);
    this.pluginsErrorStatus.set(null);
    try {
      const client = this.pluginClient ?? this.client;
      const [plugins, triggers] = await Promise.all([client.plugins(), client.triggers()]);
      this.plugins.set(plugins);
      this.triggers.set(triggers.triggers ?? []);
    } catch (error) {
      this.pluginsError.set(messageOf(error));
      this.pluginsErrorStatus.set(error instanceof KeelApiError ? error.status : null);
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
        result.sessionId ? `会话 …${result.sessionId.slice(-8)}` : "",
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

  // ---- test seams ---------------------------------------------------------

  /** Test/host seam: install the transport a harness answers in memory. The
   * real `KeelHttp` parsing runs (headers, CSRF, error mapping); only `fetch`
   * is stubbed. `null` restores a plain live client. */
  useHTTPForTest(http: KeelHttp | null): void {
    this.detectToken += 1; // the boot probe must not overwrite the harness state
    this.probeClient = http;
    this.http = http;
    this.client = http ?? new KeelHttp("");
    this.mode.set("http");
    if (http) this.installUnauthorized(http);
  }

  /** Test only: forget the auth/identity state so a case starts at the gate. */
  resetAuthForTest(): void {
    this.detectToken += 1;
    this.clearSessionState();
    this.auth.set("unknown");
    this.authError.set(null);
    this.authDenied.set(false);
    this.authNotice.set(null);
    this.authRetryAfter.set(null);
    this.mode.set("http");
  }

  /** Test only: cancel the boot probe and settle as a live service. A harness
   * owns its own transport and permissions; a late probe answer must not reset
   * them (the probe checks its token after every await). */
  skipBootForTest(): void {
    this.detectToken += 1;
    this.mode.set("http");
    this.sessionsLoading.set(false);
  }

  /** Test/host seam: answer the identity/access surface without a network.
   * `null` restores the app's own transport. */
  setIdentityTransport(client: IdentityApi | null): void {
    this.identityClient = client;
  }

  /** The identity/access transport in force, or null when the transport does
   * not serve that surface (the pages then say so instead of inventing rows). */
  identityApi(): IdentityApi | null {
    const client = this.identityClient ?? this.client;
    return typeof client.listAccounts === "function" ? (client as IdentityApi) : null;
  }

  /** The live transport's streaming seam for GET /v1/identity/events. The
   * fixture and a hand-written test IdentityApi do not expose it, so the
   * stream module simply stays off there. */
  identityStreamTransport(): { url(path: string): string; transport(): FetchLike } | null {
    const client = this.identityClient ?? this.client;
    const candidate = client as unknown as { url?: unknown; transport?: unknown };
    if (typeof candidate.url !== "function" || typeof candidate.transport !== "function") return null;
    return candidate as { url(path: string): string; transport(): FetchLike };
  }

  /** The session manifest (`GET /v1/sessions/{id}/files`) or `null` when this
   * transport has none. The run surface's task panel reads it beside the task
   * projection, so a file's `lastTaskId` attribution and `hash` come from the
   * manifest the file plugin wrote — never from a guessed link or digest. The
   * query is the route's path-cursor page. It reads through the same identity
   * seam as the panel, so a harness/fixture transport answers it exactly like
   * the live one. */
  async sessionFileManifest(sessionId: string, query?: SessionFilesQuery): Promise<SessionFilesView | null> {
    const client = this.identityApi();
    if (!client?.sessionFiles) return null;
    return await client.sessionFiles(sessionId, query);
  }

  /** identity-stream.ts installs its stop function here; `null` clears it. */
  setIdentityStreamCloser(closer: (() => void) | null): void {
    this.identityStreamCloser = closer;
  }

  /** identity-stream.ts installs its sync function here; the store calls it
   * after every whoami answer, so a permission change starts/stops the stream
   * on any route (settings, plugins, workspace), not only inside the books
   * shell. */
  setIdentityStreamStarter(starter: (() => void) | null): void {
    this.identityStreamStarter = starter;
  }

  /** The invalidation stream answers a 401 outside KeelHttp's request path
   * (it streams a body), so it reports the expired session here: close every
   * local trace and return to the login gate, exactly like a 401 from a route. */
  identityStreamUnauthorized(): void {
    if (this.auth() === "unknown") return;
    this.clearSessionState();
    this.auth.set("required");
    this.authNotice.set("会话已过期，请重新登录。");
  }

  /** Test/host seam: answer the skill library without a network. `null`
   * restores the app's own transport. */
  setSkillsTransport(client: SettingsApi | null): void {
    this.skillsClient = client;
  }

  private skillsApi(): SettingsApi {
    return this.skillsClient ?? this.client;
  }

  /** Test/host seam: answer the plugin and trigger reads without a network.
   * `null` restores the app's own transport. */
  setPluginTransport(client: PluginTransport | null): void {
    this.pluginClient = client;
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
    this.pluginsErrorStatus.set(null);
    this.triggers.set([]);
    this.fireOutcome.set(null);
  }

  async refreshSessions() {
    if (this.mode() === "checking") return;
    if (this.mode() === "offline") {
      // The list's 重试 is a connection retry, not a re-read of nothing: probe
      // again, and read the list when the probe lands on a live service.
      await this.detect();
      return;
    }
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
      // The tail read fills what the listing lacks and keeps the badge honest.
      // A row that is not running-like and already carries its title, preview
      // and badge answer is complete: reading its tail again would be a
      // request per refresh for nothing.
      if (row.title && row.lastMessage && row.pendingDecision !== undefined && !runningLike(row.runStatus)) continue;
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
    // The banner belongs to the session that produced it (C6); a dismissal is
    // per opening — coming back shows the session's newest state again.
    if (this.turnError()?.sessionId !== sessionId) this.turnError.set(null);
    this.dismissedTurnError = null;
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
          // The system bar's sound preference gates this cue; a silent host
          // (terminal, refused autoplay) simply answers false.
          void playApprovalCue();
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
    if (event.kind !== "partial") {
      this.scheduleRefresh();
    }
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

  /** The complete `tool.result` payload one call produced. Reading
   * `conversation()` first makes the caller track the projection: the result
   * arrives after the call row is on screen, so the app slot must re-render
   * when it lands. */
  toolResultFor(toolCallId: string): McpAppToolResult | undefined {
    this.conversation();
    return this.engine?.toolResultFor(toolCallId);
  }

  /** `GET /v1/mcp/apps`: the app catalogue, read through the active transport
   * (live or fixture). A transport without the surface rejects with a readable
   * reason; the caller shows that reason, never an invented catalogue. */
  async mcpApps(): Promise<McpAppCatalogueView> {
    const client = this.client;
    if (!client?.mcpApps) {
      throw new Error("此传输不提供 MCP Apps 目录");
    }
    return client.mcpApps();
  }

  /** Best effort: a deployment without `/v1/mcp/apps/audit` (P3.7) answers a
   * refusal; the caller's UI must not change because of it. */
  async reportMcpAppViolation(entry: McpAppAuditEntry): Promise<boolean> {
    const client = this.client;
    if (!client?.auditMcpApp) return false;
    try {
      return await client.auditMcpApp(entry);
    } catch {
      return false;
    }
  }

  /** A View's `tools/call`, through the host proxy. The open session is the
   * call's scope (MRTR attribution and audit); no request bypasses the proxy's
   * ownership check or budget. A transport without the surface refuses; it
   * never returns an invented result. */
  async mcpAppToolCall(appId: string, name: string, args: unknown): Promise<McpAppCallToolResult> {
    const client = this.client;
    if (!client?.mcpAppToolCall) throw new Error("此传输不提供 MCP App 工具代理");
    return client.mcpAppToolCall(appId, mcpAppToolCallBody(this.activeId(), name, args));
  }

  /** A View's `resources/read`, scoped to the same server by the proxy. */
  async mcpAppReadResource(appId: string, uri: string): Promise<McpAppResourceReadResult> {
    const client = this.client;
    if (!client?.mcpAppReadResource) throw new Error("此传输不提供 MCP App 资源代理");
    return client.mcpAppReadResource(appId, mcpAppResourceReadBody(this.activeId(), uri));
  }

  /** A View's `ui/message`. The server confirms it and decides the branch;
   * this method returns the outcome so the caller can prefill on `edit`. */
  async mcpAppMessage(
    appId: string,
    content: unknown,
    requestId: string,
  ): Promise<McpAppMessageOutcome> {
    const client = this.client;
    if (!client?.mcpAppMessage) throw new Error("此传输不提供 MCP App 消息通路");
    return client.mcpAppMessage(appId, mcpAppMessageBody(this.activeId(), content, requestId));
  }

  /** A View's `ui/open-link`. Opening the returned URL is the platform layer's
   * job (the store never navigates the host page). */
  async mcpAppOpenLink(appId: string, url: string): Promise<McpAppOpenLinkOutcome> {
    const client = this.client;
    if (!client?.mcpAppOpenLink) throw new Error("此传输不提供 MCP App 链接通路");
    return client.mcpAppOpenLink(appId, mcpAppOpenLinkBody(this.activeId(), url));
  }

  /** A View's `ui/update-model-context`: overwrite-only, next-attempt scope. */
  async mcpAppUpdateModelContext(
    appId: string,
    content: unknown,
    structuredContent: unknown,
    requestId: string,
  ): Promise<void> {
    const client = this.client;
    if (!client?.mcpAppUpdateContext) throw new Error("此传输不提供 MCP App 上下文通路");
    await client.mcpAppUpdateContext(
      appId,
      mcpAppContextBody(this.activeId(), content, structuredContent, requestId),
    );
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
    // The session's selection fact rides every input: the model choice only.
    // Bot/TeamBot ownership is carried server-side, not as a selection token.
    const selection = selectionForSend(this.sessionSelection(), null) ?? undefined;
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

  /** The banner follows the open session's **newest** turn: an older failure
   * that a later turn moved past is history, not a live problem. A dismissed
   * turn stays dismissed until it fails again or is explicitly retried. */
  private syncTurnError(sessionId: string, view: ConversationView) {
    const last = view.turns[view.turns.length - 1];
    if (!last || last.status !== "failed" || !last.error) {
      if (this.turnError()?.sessionId === sessionId && this.turnError()?.turnId === undefined) {
        this.turnError.set(null);
      }
      return;
    }
    if (this.dismissedTurnError === last.turnId || this.turnError()?.turnId === last.turnId) return;
    this.turnError.set({
      sessionId,
      turnId: last.turnId,
      text: last.error,
      retryable: last.visibleInputs.some((input) => input.seq !== undefined),
    });
  }

  /** The banner's close button: a dismissal is per turn — the same failure must
   * not pop back on the next refresh of this view. Re-opening the session shows
   * it again (the turn is still the newest state). */
  dismissTurnError() {
    const current = this.turnError();
    if (current?.turnId) this.dismissedTurnError = current.turnId;
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
      if (this.dismissedTurnError === turnId) this.dismissedTurnError = null;
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
