// The app's one store: sessions, the active conversation, the connection state
// and every command the screens issue. Screens read signals; nothing outside
// the data layer talks to fetch. The fixture transport exists for development
// only (`?fixture=1` on the page, `host.params.fixture` elsewhere): a failed
// connection is an offline face with a retry, never a demo standing in for it.
import { host, platform, signal } from "nsh";
import { createCoreEngine, type CoreEngine } from "./core";
import { FixtureClient } from "./fixture";
import { KeelApiError, KeelHttp, errorMessageOf, type FetchLike } from "./http";
import {
  applyTargetAvailability,
  commandFieldErrors,
  commandStatusLabel,
  commandTargetFor,
} from "./commands";
import { setEffectivePermissions, resetPermissions } from "./permissions";
import { playApprovalCue } from "./sound";
import type { AuthSessionView, WhoAmI } from "./identity";
import type { IdentityApi } from "./identity-api";
import { selectionForSend } from "./selection";
import type {
  AttachmentPart,
  AttachmentRef,
  CheckpointPolicyUpdate,
  CheckpointPolicyView,
  CommandApi,
  CommandCatalogView,
  CommandExecutionView,
  CommandLocalOutcome,
  CommandRunOutcome,
  CommandTarget,
  ConnectionState,
  ConversationView,
  DecisionView,
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
  QueueFacts,
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
import type { ExternalProjectionView } from "./oar-projection";
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
export type { IdentitySection, Route, RouteState, BoardView } from "./route";
export { resolveRoute } from "./route";
import {
  resolveRoute,
  routeState,
  routePath,
  type BoardView,
  type IdentitySection,
  type Route,
  type RouteState,
} from "./route";
import type { BoardApi } from "./board-wire";

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

/** One durable command waiting on its destructive decision. The server keeps
 * the invocation row but never the arguments (audit rule), so the client that
 * issued it is the only holder of the argument document: it persists that
 * document for the decision window, and replays the same requestId when the
 * decision is answered/times out (including after a reload or another
 * client's answer). */
interface PendingCommandRecord {
  sessionId: string;
  requestId: string;
  name: string;
  args: Record<string, unknown>;
  /** Empty while a lost 202 is being resent; the decision id arrives with it. */
  decisionId: string;
  /** The server's decision window in ms (0 until the 202 tells us). */
  timeoutMs: number;
  /** Epoch ms after which the decision is settled server-side. */
  deadline: number;
  createdAt: number;
}

interface PendingCommandTimers {
  poll?: number;
  deadline?: number;
  resend?: number;
}

/** The slice of Storage this store needs; nsh's TS lib has no DOM globals. */
interface PendingStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

/** sessionStorage is a per-tab store: closing the tab drops the arguments.
 * The server has no argument document to resume from by design, so a closed
 * initiating tab is the one case that cannot self-heal. */
const COMMAND_PENDING_STORAGE_KEY = "keel.commands.pending.v1";
const COMMAND_PENDING_TTL_MS = 30 * 60 * 1000;
/** The decision journal poll (another client may answer). */
const COMMAND_DECISION_POLL_MS = 1_000;
/** A lost/5xx command answer is replayed with the same requestId. */
const COMMAND_RESEND_MS = 1_500;
const COMMAND_MAX_RESEND_ATTEMPTS = 3;
const COMMAND_MAX_BUSY_RETRIES = 3;

/** Everything the store's transport must answer. The identity/access surface
 * (A4.3/A4.4a) is optional: a transport that predates it answers nothing, and
 * the identity pages say so instead of inventing rows. */
type Client = SettingsApi &
  Partial<IdentityApi> &
  Partial<BoardApi> & {
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
  /** `GET /v1/sessions/{id}/attachments/{fileId}/part?seq=` — the canonical
   * attachment projection. Optional: a transport that predates the route (the
   * development fixture) degrades to the event payload's display facts. */
  attachmentPart?(sessionId: string, fileId: string, seq: number): Promise<AttachmentPart>;
  createSession(botId?: string): Promise<string>;
  deleteSession(sessionId: string): Promise<void>;
  source(sessionId: string): EventSource;
  commands(sessionId: string): UiCommands;
  /** `GET /v1/sessions/{id}/decisions` — the host synthesizes the decision
   * events the chat core consumes (the decision journal is not a kernel event
   * kind). Optional: a transport without it keeps only streamed decisions. */
  decisions?(sessionId: string): Promise<DecisionView[]>;
  /** The chat command surface (change keel-plugin-commands). Optional: a
   * transport without it shows the catalog-error state instead of a fake row. */
  commandCatalog?(sessionId: string, target: CommandTarget): Promise<CommandCatalogView>;
  executeCommand?(
    sessionId: string,
    name: string,
    args: Record<string, unknown>,
    requestId: string,
  ): Promise<CommandExecutionView>;
  tail?(sessionId: string, lastSeq: number, count?: number): Promise<WireEvent[]>;
};

/** The command surface alone, for a test harness (`setCommandTransport`). */
type CommandTransport = CommandApi;

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

/** A server row that says the run is waiting in the placement queue. The
 * position/ETA is a client-side admission fact (the session listing never
 * carries it), so it is only valid while the server still says queued. */
const queuedRunStatus = (runStatus: string | undefined): boolean =>
  (runStatus ?? "").toLowerCase() === "queued";

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
  /** The board's view (`/board`, `/board/sync`, `/board/automation`). */
  readonly routeBoardView = signal<BoardView>("queue");
  /** The board's filter search, as the page URL carried it. */
  readonly routeSearch = signal("");

  // ---- settings face (change keel-model-settings) ------------------------

  /** The effective models document (`GET /v1/settings/models`). */
  readonly settings = signal<ModelsSettingsView | null>(null);
  readonly settingsLoading = signal(false);
  readonly settingsError = signal<string | null>(null);
  readonly settingsSaving = signal(false);
  /** The server's 400 field issues, verbatim. */
  readonly settingsIssues = signal<SettingsIssue[]>([]);

  // ---- checkpoint policy (change keel-checkpoint) ------------------------

  /** The session whose policy is open, the effective projection and the five
   * faces the section draws (loading/empty/error/configured/busy). */
  readonly checkpointPolicySessionId = signal("");
  readonly checkpointPolicy = signal<CheckpointPolicyView | null>(null);
  readonly checkpointPolicyLoading = signal(false);
  readonly checkpointPolicySaving = signal(false);
  readonly checkpointPolicyError = signal<string | null>(null);
  readonly checkpointPolicyErrorStatus = signal<number | null>(null);
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

  // ---- chat commands (change keel-plugin-commands) -----------------------

  /** The open session's catalog, the surface it was read for, and the five
   * faces the menu draws (loading/empty/error/permission/success). The catalog
   * is the only command list the client renders. */
  readonly commandCatalog = signal<CommandCatalogView | null>(null);
  readonly commandCatalogSession = signal<string | null>(null);
  readonly commandCatalogTarget = signal<CommandTarget>(
    commandTargetFor(platform.idiom, platform.os),
  );
  readonly commandCatalogLoading = signal(false);
  readonly commandCatalogError = signal<string | null>(null);
  readonly commandCatalogErrorStatus = signal<number | null>(null);
  /** The requestId of the invocation in flight (one at a time from this UI). */
  readonly commandExecuting = signal<string | null>(null);
  /** Local (non-durable) command failures by requestId: a request-phase 4xx
   * before a claim, a revoked permission/plugin, or an exhausted busy retry.
   * The row overlays its status with this outcome; it is never written to the
   * ledger. Cleared by a later successful replay or logout. */
  readonly commandLocalOutcomes = signal<Record<string, CommandLocalOutcome>>({});

  readonly plugins = signal<PluginsView | null>(null);
  readonly pluginsLoading = signal(false);
  readonly pluginsError = signal<string | null>(null);
  /** As `agentsErrorStatus`: the 403 face is driven by the response status,
   * not by matching a localized message. */
  readonly pluginsErrorStatus = signal<number | null>(null);
  readonly triggers = signal<TriggerStatusView[]>([]);
  /** Trigger status is a separate read (`trigger.read`); its failure is a
   * section face, never the plugins page's permission gate. */
  readonly triggersError = signal<string | null>(null);
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
  /** Test/host seam for the board (a harness answers it in memory so the
   * page's five states and write feedback run through the real data layer).
   * `null` uses the app's transport. */
  private boardClient: BoardApi | null = null;
  /** Test/host seam for the command surface (the states harness answers the
   * catalog/execution in memory). `null` uses the app's transport. */
  private commandClient: CommandTransport | null = null;
  /** Destructive invocations whose decision is still open, keyed by
   * `<session>/<requestId>`. Persisted to sessionStorage (web) so a session
   * switch or a reload still executes the command exactly once after its
   * approval; dropped at terminal state or after the decision window. */
  private pendingCommands = new Map<string, PendingCommandRecord>();
  private pendingTimers = new Map<string, PendingCommandTimers>();
  private pendingReplayAttempts = new Map<string, number>();
  private pendingResendAttempts = new Map<string, number>();
  private pendingReplayInFlight = new Set<string>();
  /** The catalog read generation: a late answer for a previous session/target
   * must not draw into the current menu. */
  private commandCatalogToken = 0;
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
  /** The decisions read generation: a late answer for a previous session must
   * not draw a card into the current one. */
  private decisionSyncToken = 0;
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
  /** The OAR external-run sample is requested (`?oar=1` / `host.params.oar`):
   * one external session, its records and its projection. Same rule — the
   * default sample stays unchanged. */
  private oarFixtures = false;
  /** `?fixture=1&stress=N`: the long-list sample (development transport only). */
  private stressMessages = 0;
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
      this.oarFixtures = host.params?.oar === true;
      const stress = Number(host.params?.stress ?? 0);
      this.stressMessages = Number.isFinite(stress) && stress > 0 ? Math.floor(stress) : 0;
      return true;
    }
    const page = await this.pageRoute();
    this.devSearch = page?.search ?? "";
    const params = new URLSearchParams(this.devSearch);
    this.dev = params.get("fixture") === "1";
    this.pluginFixtures = params.get("plugins") === "1";
    this.oarFixtures = params.get("oar") === "1";
    const stress = Number(params.get("stress") ?? 0);
    this.stressMessages = Number.isFinite(stress) && stress > 0 ? Math.floor(stress) : 0;
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
      if (!this.fixture) this.fixture = new FixtureClient(undefined, this.pluginFixtures, this.oarFixtures, this.stressMessages);
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
    this.clearPendingCommands();
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
    if (!this.routeTouched) this.applyRoute(page.path, page.search);
    try {
      // Browser back/forward: the URL is authoritative again.
      host.on<{ path?: string; search?: string }>("keel.route", (payload) => {
        const next = typeof payload?.path === "string" ? payload.path : "";
        const search = typeof payload?.search === "string" ? payload.search : "";
        this.applyRoute(next, search);
      });
    } catch {
      /* a host without events is fine: routes stay in-app */
    }
  }

  /** Land on a path: the chapter and its parameters move together, so a deep
   * link and the back button cannot leave a chapter showing another object. */
  private applyRoute(path: string, search = "") {
    const state = resolveRoute(path, this.dev);
    this.routeTouched = true;
    this.route.set(state.route);
    this.routeSection.set(state.section);
    this.routeObjectId.set(state.objectId);
    this.routeRunId.set(state.runId);
    this.routeBoardView.set(state.boardView);
    this.routeSearch.set(state.route === "board" ? search.replace(/^\?/, "") : "");
  }

  /** Move to a route in-app and keep the page URL in step. The state is set
   * before the host call: a host without a URL (TUI, desktop, tests) still
   * navigates, it just does not change an address bar. */
  private go(state: RouteState, search = "") {
    this.routeTouched = true;
    this.route.set(state.route);
    this.routeSection.set(state.section);
    this.routeObjectId.set(state.objectId);
    this.routeRunId.set(state.runId);
    this.routeBoardView.set(state.boardView);
    this.routeSearch.set(state.route === "board" ? search.replace(/^\?/, "") : "");
    this.navigate(`${routePath(state)}${search ? `?${search.replace(/^\?/, "")}` : ""}`);
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
  openChapter(route: "workspace" | "board" | "bots" | "teams" | "identity" | "records") {
    const section: IdentitySection = route === "identity" ? this.routeSection() : "accounts";
    this.go(routeState(route, section, null));
  }

  /** The board's own navigation: `view` picks the queue/sync/automation face,
   * `taskId` opens one task's thread, `search` carries the queue's filters. */
  openBoard(view: BoardView = "queue", taskId: string | null = null, search = "") {
    this.go(routeState("board", "accounts", taskId, null, view), search);
  }

  /** One task's thread (`/board/<id>`); the queue's filters stay in the URL. */
  openBoardTask(taskId: string) {
    const search = this.routeSearch();
    this.go(routeState("board", "accounts", taskId), search);
  }

  /** Back from a task to the queue, keeping the filters. */
  closeBoardTask() {
    if (this.route() !== "board") return;
    this.go(routeState("board"), this.routeSearch());
  }

  /** Replace only the queue's filters (the address bar follows). */
  setBoardSearch(search: string) {
    this.routeSearch.set(search.replace(/^\?/, ""));
    this.navigate(`/board${search ? `?${search.replace(/^\?/, "")}` : ""}`);
  }

  /** The retired board address: `/superagent*` is a 404 page, not a redirect. */
  openGone(objectId: string | null = null) {
    this.go(routeState("gone", "accounts", objectId));
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

  /** The settings page's 检查点 section. With no explicit id it uses the open
   * session, then the newest visible one; no sessions is an empty face. */
  async loadCheckpointPolicy(sessionId?: string) {
    const remembered = this.checkpointPolicySessionId();
    const chosen =
      sessionId ?? (remembered || this.activeId() || this.sessions()[0]?.sessionId || "");
    if (!chosen) {
      this.checkpointPolicySessionId.set("");
      this.checkpointPolicy.set(null);
      this.checkpointPolicyError.set(null);
      this.checkpointPolicyErrorStatus.set(null);
      return;
    }
    if (this.checkpointPolicyLoading()) return;
    this.checkpointPolicySessionId.set(chosen);
    this.checkpointPolicyLoading.set(true);
    this.checkpointPolicyError.set(null);
    this.checkpointPolicyErrorStatus.set(null);
    try {
      this.checkpointPolicy.set(await this.client.checkpointPolicy(chosen));
    } catch (error) {
      this.checkpointPolicy.set(null);
      this.checkpointPolicyError.set(messageOf(error));
      this.checkpointPolicyErrorStatus.set(error instanceof KeelApiError ? error.status : null);
    } finally {
      this.checkpointPolicyLoading.set(false);
    }
  }

  selectCheckpointPolicySession(sessionId: string) {
    this.checkpointPolicySessionId.set(sessionId);
    this.checkpointPolicy.set(null);
    void this.loadCheckpointPolicy(sessionId);
  }

  /** `PUT /v1/sessions/{id}/checkpoint-policy`: the session's narrowing
   * override. An empty object clears it. The response is the new effective
   * policy; on failure the current view stays so the reader can correct it. */
  async saveCheckpointPolicy(update: CheckpointPolicyUpdate): Promise<boolean> {
    const sessionId = this.checkpointPolicySessionId();
    if (!sessionId || this.checkpointPolicySaving()) return false;
    this.checkpointPolicySaving.set(true);
    this.checkpointPolicyError.set(null);
    this.checkpointPolicyErrorStatus.set(null);
    try {
      const view = await this.client.saveCheckpointPolicy(sessionId, update);
      this.checkpointPolicy.set(view);
      this.flash({
        tone: "success",
        text: view.source === "session" ? "会话检查点策略已保存（已按租户上限收窄）" : "会话检查点策略已恢复为租户默认",
      });
      return true;
    } catch (error) {
      this.checkpointPolicyError.set(messageOf(error));
      this.checkpointPolicyErrorStatus.set(error instanceof KeelApiError ? error.status : null);
      return false;
    } finally {
      this.checkpointPolicySaving.set(false);
    }
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

  // ---- chat commands (change keel-plugin-commands) -----------------------

  /** The transport the command surface reads: the harness seam, or the app's
   * own client when it offers the methods. `null` = unsupported transport. */
  private commandApi(): CommandTransport | null {
    if (this.commandClient) return this.commandClient;
    const client = this.client;
    const catalog = client.commandCatalog?.bind(client);
    const execute = client.executeCommand?.bind(client);
    if (!catalog || !execute) return null;
    const decisions = client.decisions?.bind(client);
    return { commandCatalog: catalog, executeCommand: execute, ...(decisions ? { decisions } : {}) };
  }

  /** The target this client renders: web, terminal or a native host. */
  commandTarget(): CommandTarget {
    return commandTargetFor(platform.idiom, platform.os);
  }

  /** The local failure overlay for a requestId, when one exists. */
  commandLocalOutcome(requestId: string): CommandLocalOutcome | undefined {
    return this.commandLocalOutcomes()[requestId];
  }

  private setCommandLocalOutcome(requestId: string, outcome: CommandLocalOutcome): void {
    this.commandLocalOutcomes.update((map) => ({ ...map, [requestId]: outcome }));
  }

  private clearCommandLocalOutcome(requestId: string): void {
    const current = this.commandLocalOutcomes();
    if (!(requestId in current)) return;
    const next = { ...current };
    delete next[requestId];
    this.commandLocalOutcomes.set(next);
  }

  /** A failure that produced no durable command.invoked row. It must be
   * visible: forgetting it silently would make the UI claim a command simply
   * vanished. The event row (when a 202 had created one) renders this overlay.
   */
  private markCommandLocalFailure(
    record: PendingCommandRecord,
    error: unknown,
    status: number | undefined,
    code: string | undefined,
  ): void {
    this.setCommandLocalOutcome(record.requestId, {
      status: "error",
      ...(code ? { errorCode: code } : {}),
      error: this.commandErrorMessage(record.name, error, status, code, false),
      local: true,
    });
    if (this.activeId() === record.sessionId) {
      this.flash({ tone: "danger", text: this.commandErrorMessage(record.name, error, status, code, false) });
    }
  }

  private commandErrorIsDurable(error: unknown): boolean {
    return error instanceof KeelApiError && typeof error.eventSeq === "number" && error.eventSeq > 0;
  }

  /**
   * Read the open session's command catalog. Cached per (session, target);
   * `force` re-reads after a lifecycle change (a plugin installed/disabled,
   * a terminal failure that may have changed availability). A cached catalog
   * stays visible while a refresh is in flight, so the menu never flashes
   * empty on its own; a failed refresh keeps the last good rows beside the
   * error.
   */
  async loadCommands(options: { force?: boolean } = {}): Promise<void> {
    const sessionId = this.activeId();
    const target = this.commandTarget();
    if (!sessionId) {
      this.commandCatalog.set(null);
      this.commandCatalogSession.set(null);
      this.commandCatalogError.set(null);
      this.commandCatalogErrorStatus.set(null);
      return;
    }
    if (
      !options.force &&
      this.commandCatalogSession() === sessionId &&
      this.commandCatalogTarget() === target &&
      (this.commandCatalog() !== null || this.commandCatalogLoading())
    ) {
      return;
    }
    const token = ++this.commandCatalogToken;
    this.commandCatalogTarget.set(target);
    this.commandCatalogLoading.set(true);
    this.commandCatalogError.set(null);
    this.commandCatalogErrorStatus.set(null);
    const api = this.commandApi();
    if (!api) {
      this.commandCatalog.set(null);
      this.commandCatalogSession.set(sessionId);
      this.commandCatalogLoading.set(false);
      this.commandCatalogError.set("当前传输不支持命令目录");
      this.commandCatalogErrorStatus.set(501);
      return;
    }
    try {
      const catalog = await api.commandCatalog(sessionId, target);
      if (token !== this.commandCatalogToken || this.activeId() !== sessionId) return;
      this.commandCatalog.set(applyTargetAvailability(catalog, target));
      this.commandCatalogSession.set(sessionId);
    } catch (error) {
      if (token !== this.commandCatalogToken || this.activeId() !== sessionId) return;
      const status = error instanceof KeelApiError ? error.status : null;
      // A failed REFRESH keeps the last good catalog on screen (with the error
      // beside it); only a first load with nothing to show is the bare error
      // face. The menu never flashes empty because a retry failed.
      this.commandCatalogSession.set(sessionId);
      this.commandCatalogErrorStatus.set(status);
      this.commandCatalogError.set(
        status === 403
          ? errorMessageOf(error)
          : `命令目录读取失败：${errorMessageOf(error)}`,
      );
    } finally {
      if (token === this.commandCatalogToken) this.commandCatalogLoading.set(false);
    }
  }

  /** Reset the catalog when the session boundary moves. Pending destructive
   * commands survive: their arguments are the only copy and the decision may
   * still be answered after the reader switches sessions. */
  private resetCommands(): void {
    this.commandCatalogToken += 1;
    this.commandCatalog.set(null);
    this.commandCatalogSession.set(null);
    this.commandCatalogError.set(null);
    this.commandCatalogErrorStatus.set(null);
    this.commandCatalogLoading.set(false);
    this.commandExecuting.set(null);
  }

  // ---- pending destructive commands -------------------------------------

  private pendingKey(sessionId: string, requestId: string): string {
    return `${sessionId}/${requestId}`;
  }

  private pendingStorage(): PendingStorage | null {
    try {
      return (globalThis as { sessionStorage?: PendingStorage }).sessionStorage ?? null;
    } catch {
      return null;
    }
  }

  /** The persisted pending records, defensively parsed. A malformed row is
   * dropped rather than resuming a command with invented arguments. */
  private readPendingStorage(): PendingCommandRecord[] {
    const storage = this.pendingStorage();
    if (!storage) return [];
    try {
      const raw = storage.getItem(COMMAND_PENDING_STORAGE_KEY);
      if (!raw) return [];
      const parsed: unknown = JSON.parse(raw);
      if (!Array.isArray(parsed)) return [];
      const rows: PendingCommandRecord[] = [];
      for (const value of parsed) {
        if (!value || typeof value !== "object") continue;
        const row = value as Record<string, unknown>;
        const args = row.args;
        if (
          typeof row.sessionId !== "string" ||
          typeof row.requestId !== "string" ||
          typeof row.name !== "string" ||
          !args ||
          typeof args !== "object" ||
          Array.isArray(args)
        ) {
          continue;
        }
        rows.push({
          sessionId: row.sessionId,
          requestId: row.requestId,
          name: row.name,
          args: args as Record<string, unknown>,
          decisionId: typeof row.decisionId === "string" ? row.decisionId : "",
          timeoutMs: typeof row.timeoutMs === "number" && row.timeoutMs > 0 ? row.timeoutMs : 0,
          deadline: typeof row.deadline === "number" && row.deadline > 0 ? row.deadline : 0,
          createdAt: typeof row.createdAt === "number" ? row.createdAt : Date.now(),
        });
      }
      return rows;
    } catch {
      return [];
    }
  }

  /** Write every pending record (the argument document included) for this tab.
   * The server deliberately never stores args, so this per-tab copy is what
   * makes a reload able to replay the same requestId; it is cleared at the
   * terminal state and on logout. */
  private persistPending(): void {
    const storage = this.pendingStorage();
    if (!storage) return;
    try {
      if (this.pendingCommands.size === 0) {
        storage.removeItem(COMMAND_PENDING_STORAGE_KEY);
        return;
      }
      storage.setItem(
        COMMAND_PENDING_STORAGE_KEY,
        JSON.stringify([...this.pendingCommands.values()]),
      );
    } catch {
      /* storage full or denied: the in-memory copy still works */
    }
  }

  private rememberPending(record: PendingCommandRecord): void {
    this.clearCommandLocalOutcome(record.requestId);
    this.pendingCommands.set(this.pendingKey(record.sessionId, record.requestId), record);
    this.persistPending();
    this.watchPending(record);
  }

  /** Restore this tab's pending commands for a session (a reload, or a
   * session switched back to). Expired decision windows are dropped instead of
   * executing an old approval late. */
  private restorePendingCommands(sessionId: string): void {
    const rows = this.readPendingStorage();
    let dirtied = false;
    for (const row of rows) {
      if (row.sessionId !== sessionId) continue;
      const key = this.pendingKey(row.sessionId, row.requestId);
      if (this.pendingCommands.has(key)) continue;
      if (row.deadline > 0 && Date.now() > row.deadline + COMMAND_PENDING_TTL_MS) {
        dirtied = true;
        continue;
      }
      this.pendingCommands.set(key, row);
      this.watchPending(row);
    }
    if (dirtied) this.persistPending();
  }

  private findPending(decisionId: string): PendingCommandRecord | null {
    for (const record of this.pendingCommands.values()) {
      if (record.decisionId === decisionId) return record;
    }
    return null;
  }

  private stopPendingTimers(key: string): void {
    const timers = this.pendingTimers.get(key);
    if (!timers) return;
    if (timers.poll !== undefined) clearTimeout(timers.poll);
    if (timers.deadline !== undefined) clearTimeout(timers.deadline);
    if (timers.resend !== undefined) clearTimeout(timers.resend);
    this.pendingTimers.delete(key);
  }

  private forgetPending(record: PendingCommandRecord): void {
    const key = this.pendingKey(record.sessionId, record.requestId);
    this.stopPendingTimers(key);
    this.pendingCommands.delete(key);
    this.pendingReplayAttempts.delete(key);
    this.pendingResendAttempts.delete(key);
    this.pendingReplayInFlight.delete(key);
    this.persistPending();
  }

  private schedulePendingResend(record: PendingCommandRecord, delay: number): void {
    const key = this.pendingKey(record.sessionId, record.requestId);
    const timers = this.pendingTimers.get(key) ?? {};
    if (timers.resend !== undefined) clearTimeout(timers.resend);
    timers.resend = setTimeout(() => {
      timers.resend = undefined;
      const current = this.pendingCommands.get(key);
      if (current) void this.replayPending(current, "resend");
    }, delay) as unknown as number;
    this.pendingTimers.set(key, timers);
  }

  /**
   * Watch one pending destructive command:
   *   - poll the durable decision journal (another client may answer it; the
   *     approval card and the command row both update), and
   *   - replay at the decision deadline as a fallback (a lost poll must not
   *     leave the invocation pending forever; the server answers 408 for a
   *     timed-out decision and records the terminal state).
   * A record without a decision id (a lost 202) is resent with the same
   * requestId instead.
   */
  private watchPending(record: PendingCommandRecord): void {
    const key = this.pendingKey(record.sessionId, record.requestId);
    this.stopPendingTimers(key);
    const api = this.commandApi();
    const read = api?.decisions;
    if (!record.decisionId || !read) {
      if (!record.decisionId) this.schedulePendingResend(record, COMMAND_RESEND_MS);
      return;
    }
    const timers: PendingCommandTimers = {};
    const tick = async () => {
      if (!this.pendingCommands.has(key)) return;
      try {
        const rows = await read(record.sessionId);
        const decision = rows.find((row) => row.decisionId === record.decisionId);
        if (decision && decision.status !== "open") {
          void this.syncDecisions(record.sessionId);
          const current = this.pendingCommands.get(key);
          if (current) void this.replayPending(current, "decision");
          return;
        }
      } catch {
        /* a read failure is transient; the deadline timer still settles it */
      }
      if (!this.pendingCommands.has(key)) return;
      timers.poll = setTimeout(tick, COMMAND_DECISION_POLL_MS) as unknown as number;
      this.pendingTimers.set(key, timers);
    };
    timers.poll = setTimeout(tick, COMMAND_DECISION_POLL_MS) as unknown as number;
    const deadline = record.deadline > 0 ? record.deadline : Date.now() + (record.timeoutMs || 5 * 60 * 1000);
    timers.deadline = setTimeout(() => {
      timers.deadline = undefined;
      const current = this.pendingCommands.get(key);
      if (current) void this.replayPending(current, "deadline");
    }, Math.max(250, deadline - Date.now() + 750)) as unknown as number;
    this.pendingTimers.set(key, timers);
  }

  /** Replay one pending requestId exactly as the first call did. Background
   * safe: it never touches the foreground `commandExecuting` guard, so a
   * decision answered while another command runs still settles this one. */
  private async replayPending(record: PendingCommandRecord, reason: string): Promise<void> {
    const key = this.pendingKey(record.sessionId, record.requestId);
    if (!this.pendingCommands.has(key) || this.pendingReplayInFlight.has(key)) return;
    const api = this.commandApi();
    if (!api) return;
    this.pendingReplayInFlight.add(key);
    const attempts = (this.pendingReplayAttempts.get(key) ?? 0) + 1;
    this.pendingReplayAttempts.set(key, attempts);
    try {
      const result = await api.executeCommand(record.sessionId, record.name, record.args, record.requestId);
      if (!this.pendingCommands.has(key)) return;
      if (result.status === "pending_decision" || result.pending) {
        const updated: PendingCommandRecord = {
          ...record,
          decisionId: result.decisionId || record.decisionId,
          timeoutMs: result.timeoutMs || record.timeoutMs,
          deadline: Date.now() + (result.timeoutMs || record.timeoutMs || 5 * 60 * 1000),
        };
        this.pendingCommands.set(key, updated);
        this.persistPending();
        this.watchPending(updated);
        return;
      }
      this.forgetPending(record);
      this.clearCommandLocalOutcome(record.requestId);
      if (this.activeId() === record.sessionId) {
        // The durable command.invoked row is the feedback; a second toast would
        // outlive an approval and read as a second outcome.
        void this.loadCommands({ force: true });
      }
    } catch (error) {
      if (!this.pendingCommands.has(key)) return;
      const status = error instanceof KeelApiError ? error.status : undefined;
      const code = error instanceof KeelApiError ? error.code : undefined;
      const retryableTransport = status === undefined || status === 500 || status === 504;
      if (retryableTransport) {
        const resend = (this.pendingResendAttempts.get(key) ?? 0) + 1;
        if (resend <= COMMAND_MAX_RESEND_ATTEMPTS) {
          this.pendingResendAttempts.set(key, resend);
          this.schedulePendingResend(record, COMMAND_RESEND_MS);
          return;
        }
        // Retries exhausted with no answer: the command's outcome is unknown.
        // Do not claim a durable row; surface the local failure explicitly.
        this.forgetPending(record);
        this.setCommandLocalOutcome(record.requestId, {
          status: "error",
          errorCode: code ?? "transport_error",
          error: "连接中断，命令结果未知；请检查会话后重新执行。",
          local: true,
        });
        if (this.activeId() === record.sessionId) {
          this.flash({ tone: "danger", text: "连接中断，命令结果未知；请检查会话后重新执行。" });
        }
        return;
      }
      if (code === "session_busy" || code === "command_in_progress") {
        if (attempts <= COMMAND_MAX_BUSY_RETRIES) {
          this.schedulePendingResend(record, 1_000 * attempts);
          return;
        }
        this.forgetPending(record);
        this.setCommandLocalOutcome(record.requestId, {
          status: "error",
          errorCode: code,
          error: "会话持续繁忙，命令仍未执行；请稍后重新执行。",
          local: true,
        });
        if (this.activeId() === record.sessionId) {
          this.flash({ tone: "danger", text: "会话持续繁忙，命令仍未执行；请稍后重新执行。" });
        }
        return;
      }
      const durable = this.commandErrorIsDurable(error);
      this.forgetPending(record);
      if (durable) {
        // The server wrote a terminal command.invoked row; the transcript is
        // the feedback and no local overlay is needed.
        this.clearCommandLocalOutcome(record.requestId);
        if (this.activeId() === record.sessionId) void this.loadCommands({ force: true });
        return;
      }
      // A request-phase refusal (403/404/409 before ClaimCommandInvocation) has
      // no command.invoked row. Keep it visible and never pretend it is durable.
      this.markCommandLocalFailure(record, error, status, code);
      if (this.activeId() === record.sessionId) void this.loadCommands({ force: true });
    } finally {
      this.pendingReplayInFlight.delete(key);
    }
  }

  private replayPendingForDecision(decisionId: string): void {
    const record = this.findPending(decisionId);
    if (record) void this.replayPending(record, "answer");
  }

  /** Drop every pending command (logout / account switch). The server rows
   * stay as the audit truth; this client no longer holds their arguments. */
  private clearPendingCommands(): void {
    for (const key of [...this.pendingTimers.keys()]) this.stopPendingTimers(key);
    this.pendingCommands.clear();
    this.pendingReplayAttempts.clear();
    this.pendingResendAttempts.clear();
    this.pendingReplayInFlight.clear();
    this.commandLocalOutcomes.set({});
    this.persistPending();
  }

  /**
   * Execute one command. The requestId is the idempotency identity: a
   * destructive command's 202 is replayed with the SAME id after its decision
   * is answered (`replayPending`), which is what makes the host execute it
   * once. Field-level 400s are returned as `fields` for the form; a lost
   * answer is retried with the same id (the row, when it exists, replays its
   * first result instead of executing again).
   */
  async runCommand(
    name: string,
    args: Record<string, unknown> = {},
    options: { requestId?: string } = {},
  ): Promise<CommandRunOutcome> {
    const sessionId = this.activeId();
    const requestId = options.requestId ?? `cmd_${newRequestId()}`;
    if (!sessionId) {
      return { status: "error", requestId, message: "先选择一个会话再执行命令。" };
    }
    if (this.commandExecuting()) {
      return { status: "error", requestId, message: "另一个命令正在执行，等它完成后再试。" };
    }
    const api = this.commandApi();
    if (!api) {
      return { status: "error", requestId, message: "当前传输不支持命令执行。" };
    }
    this.commandExecuting.set(requestId);
    try {
      const result = await api.executeCommand(sessionId, name, args, requestId);
      if (result.status === "pending_decision" || result.pending) {
        const timeoutMs = result.timeoutMs || 5 * 60 * 1000;
        const record: PendingCommandRecord = {
          sessionId,
          requestId,
          name,
          args,
          decisionId: result.decisionId ?? "",
          timeoutMs,
          deadline: Date.now() + timeoutMs,
          createdAt: Date.now(),
        };
        this.rememberPending(record);
        // The durable journal is where the approval card comes from; the
        // command's own streamed row already says 等待审批.
        void this.syncDecisions(sessionId);
        return { status: "pending", requestId, result };
      }
      this.forgetPending(this.pendingCommands.get(this.pendingKey(sessionId, requestId)) ?? {
        sessionId, requestId, name, args, decisionId: "", timeoutMs: 0, deadline: 0, createdAt: 0,
      });
      this.clearCommandLocalOutcome(requestId);
      if (result.status !== "ok") {
        return {
          status: "error",
          requestId,
          result,
          message: result.error || `命令${commandStatusLabel(result.status)}`,
          code: result.errorCode,
        };
      }
      // A plugin command may have changed its availability; refresh lazily so
      // the next menu open tells the truth.
      void this.loadCommands({ force: true });
      return { status: "ok", requestId, result };
    } catch (error) {
      const status = error instanceof KeelApiError ? error.status : undefined;
      const issues = error instanceof KeelApiError ? error.issues : [];
      const fields = commandFieldErrors(issues);
      const code = error instanceof KeelApiError ? error.code : undefined;
      const message = this.commandErrorMessage(name, error, status, code, Object.keys(fields).length > 0);
      const retryableTransport = status === undefined || status === 500 || status === 504;
      if (retryableTransport) {
        // The answer (possibly a 202) was lost: keep the same requestId and
        // replay it. The server either opens the same decision or returns the
        // first result; it never executes a second time.
        const record: PendingCommandRecord = {
          sessionId,
          requestId,
          name,
          args,
          decisionId: "",
          timeoutMs: 0,
          deadline: 0,
          createdAt: Date.now(),
        };
        this.rememberPending(record);
        this.flash({ tone: "info", text: "命令响应中断，正在用同一 requestId 重试…" });
        return { status: "pending", requestId, message };
      }
      const pending = this.pendingCommands.get(this.pendingKey(sessionId, requestId));
      if (pending && (code === "session_busy" || code === "command_in_progress")) {
        this.schedulePendingResend(pending, 1_000);
        return { status: "error", requestId, message, code, fields, statusCode: status };
      }
      this.forgetPending(pending ?? {
        sessionId, requestId, name, args, decisionId: "", timeoutMs: 0, deadline: 0, createdAt: 0,
      });
      const durable = this.commandErrorIsDurable(error);
      if (durable) this.clearCommandLocalOutcome(requestId);
      else {
        this.setCommandLocalOutcome(requestId, {
          status: "error",
          ...(code ? { errorCode: code } : {}),
          error: message,
          local: true,
        });
      }
      if (code === "plugin_unavailable" || status === 502 || status === 503) {
        void this.loadCommands({ force: true });
      }
      return {
        status: "error",
        requestId,
        message,
        ...(code ? { code } : {}),
        ...(Object.keys(fields).length ? { fields } : {}),
        ...(status !== undefined ? { statusCode: status } : {}),
        ...(error instanceof KeelApiError && error.requiredKey
          ? { requiredKey: error.requiredKey }
          : {}),
      };
    } finally {
      if (this.commandExecuting() === requestId) this.commandExecuting.set(null);
    }
  }

  /** The sentence a failed invocation shows. Stable codes win over a generic
   * status sentence; the server's message is appended when it adds detail. */
  private commandErrorMessage(
    name: string,
    error: unknown,
    status: number | undefined,
    code: string | undefined,
    hasFields: boolean,
  ): string {
    if (hasFields) return "命令参数不合法，请按字段提示修改。";
    switch (code) {
      case "session_busy":
        return "会话正忙：当前任务结束后再执行命令。";
      case "unknown_command":
        return `命令 /${name} 不在当前目录中。`;
      case "permission_denied":
        return `没有执行 /${name} 的权限。`;
      case "decision_refused":
        return "审批被拒绝，命令没有执行。";
      case "decision_timeout":
        return "审批超时，命令没有执行。";
      case "decision_cancelled":
        return "审批已取消，命令没有执行。";
      case "plugin_unavailable":
        return "插件当前不可用，命令没有执行。";
      case "tool_error":
        return "命令执行失败（工具错误）。";
      case "not_wired":
        return "此部署未接通命令执行。";
      case "compaction_disabled":
        return "压缩策略已关闭，无法手动压缩。";
      case "compaction_conflict":
        return "当前上下文无法安全压缩。";
      case "quota_exceeded":
        return "运行配额已用尽，命令没有执行。";
      case "request_id_conflict":
        return "requestId 已用于另一条命令或参数。";
      case "command_in_progress":
        return "同一 requestId 的命令仍在执行。";
      default:
        break;
    }
    if (status === 403) {
      return error instanceof KeelApiError && error.requiredKey
        ? `权限不足：需要 ${error.requiredKey}`
        : `没有执行 /${name} 的权限。`;
    }
    if (status === 409) return `命令冲突：${errorMessageOf(error)}`;
    return `命令执行失败：${errorMessageOf(error)}`;
  }

  // ---- plugins & triggers -------------------------------------------------

  async loadPlugins() {
    if (this.pluginsLoading()) return;
    this.pluginsLoading.set(true);
    this.pluginsError.set(null);
    this.pluginsErrorStatus.set(null);
    this.triggersError.set(null);
    try {
      const client = this.pluginClient ?? this.client;
      // The inventory is the page's data; trigger status is a separate read
      // with a different permission (trigger.read). A viewer holds plugin.read
      // but not trigger.read: the page must still show the inventory, with the
      // trigger section degraded, instead of claiming "no permission to read
      // plugins".
      const plugins = await client.plugins();
      this.plugins.set(plugins);
      try {
        const triggers = await client.triggers();
        this.triggers.set(triggers.triggers ?? []);
      } catch (error) {
        this.triggers.set([]);
        this.triggersError.set(messageOf(error));
      }
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

  /** Test only: fold a listing through the real merge path (promotion, finish
   * and stale-position cases). */
  mergeSessionsForTest(rows: SessionSummary[]): void {
    this.mergeSessions(rows);
  }

  /** Test only: deliver one event through the opened session's stream path,
   * exactly as attach() hydration and the live SSE tap do. */
  applyLiveEventForTest(event: WireEvent): void {
    this.onLiveEvent(event);
  }

  /** Test only: cancel a scheduled list refresh so a case owns the merge. */
  clearRefreshTimerForTest(): void {
    if (this.refreshTimer !== undefined) {
      clearTimeout(this.refreshTimer);
      this.refreshTimer = undefined;
    }
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

  /** The external run projection (`GET
   * /v1/sessions/{id}/external/projection`) or `null` when this transport has
   * none. The once-per-record cache is `data/oar.ts`; the store only names the
   * seam, so a harness answers it exactly like the live one. A transport
   * without the surface is not an error path of its own: the conversation keeps
   * its text row. */
  async externalProjection(sessionId: string): Promise<ExternalProjectionView | null> {
    const client = this.identityApi();
    if (!client?.externalProjection) return null;
    return await client.externalProjection(sessionId);
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

  /** Test/host seam: answer the board without a network. `null` restores the
   * app's own transport. */
  setBoardTransport(client: BoardApi | null): void {
    this.boardClient = client;
  }

  /** Test/host seam: answer the command catalog/execution without a network.
   * `null` restores the app's own transport. */
  setCommandTransport(client: CommandTransport | null): void {
    this.commandClient = client;
  }

  /** Test/host seam: clear pending destructive commands and their timers
   * (sessionStorage included). */
  resetCommandStateForTest(): void {
    this.clearPendingCommands();
    this.commandExecuting.set(null);
  }

  /** Test/host seam: the reload path — drop the in-memory pending set and
   * timers, keep sessionStorage, restore this session from it. */
  simulateCommandReloadForTest(sessionId: string): void {
    for (const key of [...this.pendingTimers.keys()]) this.stopPendingTimers(key);
    this.pendingCommands.clear();
    this.pendingReplayAttempts.clear();
    this.pendingResendAttempts.clear();
    this.pendingReplayInFlight.clear();
    this.restorePendingCommands(sessionId);
  }

  /** The board transport in force, or null when this transport does not serve
   * the surface (the page then says so instead of rendering invented rows).
   * The fixture and `KeelHttp` both implement it. */
  boardApi(): BoardApi | null {
    const client = this.boardClient ?? this.client;
    return typeof client.boardTasks === "function" ? (client as BoardApi) : null;
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
    this.checkpointPolicy.set(null);
    this.checkpointPolicySessionId.set("");
    this.checkpointPolicyError.set(null);
    this.checkpointPolicyErrorStatus.set(null);
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
      if (error instanceof KeelApiError && error.status === 401) {
        // The session expired: back to the login gate instead of an error list.
        this.auth.set("required");
        this.sessionsError.set(null);
      } else if (error instanceof KeelApiError && error.status === 403) {
        // Authenticated but not allowed to read sessions. This is a permission
        // face, not an expired login: keep the account signed in and say what
        // is missing (`session.read`), with no attachment or session content.
        this.sessionsError.set(`无权限读取会话（缺少 ${error.requiredKey ?? "session.read"}）。`);
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
    // A queue fact is only valid while the server says the run is queued. The
    // listing never carries the position/ETA, so an incoming non-queued row
    // (promotion, finish, cancellation) must drop the stale client fact instead
    // of leaving 排队中 on screen forever.
    const clean = (row: SessionSummary): SessionSummary =>
      queuedRunStatus(row.runStatus) ? row : { ...row, queue: undefined };
    if (current.length === 0) {
      this.sessions.set(sortNewestFirst(incoming.map(clean)));
      return;
    }
    const fresh = new Map(incoming.map((row) => [row.sessionId, row]));
    const next: SessionSummary[] = [];
    for (const row of current) {
      const update = fresh.get(row.sessionId);
      if (update) next.push({ ...row, ...clean(update) });
    }
    const known = new Set(current.map((row) => row.sessionId));
    const added = sortNewestFirst(incoming.filter((row) => !known.has(row.sessionId)).map(clean));
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

  /** The open session's last queue fact (a 202 admission's position/ETA). The
   * header uses it to say 排队中 instead of projecting an idle conversation.
   * It is only reported while the row still says queued: a promoted run may
   * keep the old fact for a refresh tick, and the header must not prefer it
   * over the server's newer status. */
  activeQueue(): QueueFacts | undefined {
    const id = this.activeId();
    if (!id) return undefined;
    const row = this.sessions().find((candidate) => candidate.sessionId === id);
    return queuedRunStatus(row?.runStatus) ? row?.queue : undefined;
  }

  async openSession(sessionId: string) {
    if (this.activeId() === sessionId) return;
    const token = ++this.openToken;
    this.closeStream();
    this.resetCommands();
    this.activeId.set(sessionId);
    // A reload / a switch back to this session restores the destructive
    // commands this tab still owes an answer to (their args never reach the
    // server's audit row, so this copy is the only one able to replay).
    this.restorePendingCommands(sessionId);
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
      void this.syncDecisions(sessionId);
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
      // A creation approval opens inside the turn and is durable in the
      // decision journal, not an event kind. Re-read the journal once the turn
      // settles so the chat surface can draw the card (docs/CHAT-SURFACE.md).
      void this.syncDecisions(sessionId);
    }
    const row = this.sessions().find((candidate) => candidate.sessionId === sessionId);
    const stillQueued = queuedRunStatus(row?.runStatus);
    if (event.kind === "run.finished") {
      if (stillQueued) {
        // A queued run cannot have finished: this is history replayed by
        // attach() (or an out-of-order event) and must not clear the queue
        // fact. The refresh scheduled below reconciles a real promotion.
        this.patchSession(sessionId, { lastSeq: Math.max(row?.lastSeq ?? 0, event.seq) });
      } else {
        this.patchSession(sessionId, {
          runStatus: String(payloadOf(event).stopReason ?? "done"),
          queue: undefined,
          lastSeq: event.seq,
          lastMessageAt: new Date().toISOString(),
          pendingDecision: false,
        });
      }
    } else if (event.kind === "user.message" || event.kind === "assistant.message") {
      if (stillQueued) {
        // attach() hydrates history through this same callback, and the
        // queueing input's live event can race its 202. Neither may pretend
        // the run started or wipe the position/ETA learned from the 202.
        this.patchSession(sessionId, { lastSeq: Math.max(row?.lastSeq ?? 0, event.seq) });
      } else {
        this.patchSession(sessionId, { runStatus: "running", queue: undefined, lastSeq: event.seq });
      }
    }
    if (event.kind !== "partial") {
      this.scheduleRefresh();
    }
  }

  private scheduleRefresh() {
    if (this.refreshTimer !== undefined) clearTimeout(this.refreshTimer);
    this.refreshTimer = setTimeout(() => void this.refreshSessions(), 600) as unknown as number;
  }

  /** Synthesize the `decision` events the chat core consumes from the durable
   * decision journal. The journal is product-layer state (harnessext), not a
   * kernel event kind; the host owns this projection, and a failure only
   * leaves streamed cards in place. */
  private async syncDecisions(sessionId: string) {
    const read = this.client.decisions?.bind(this.client);
    const engine = this.engine;
    if (!read || !engine || this.activeId() !== sessionId) return;
    const token = ++this.decisionSyncToken;
    try {
      const rows = await read(sessionId);
      if (token !== this.decisionSyncToken || this.activeId() !== sessionId || this.engine !== engine) return;
      for (const row of rows) {
        const kind =
          row.status === "answered"
            ? "decision.answered"
            : row.status === "timeout"
              ? "decision.timeout"
              : row.status === "cancelled"
                ? "decision.cancelled"
                : "decision";
        engine.applyDecision({
          seq: row.seq || 0,
          kind,
          payload: {
            decisionId: row.decisionId,
            question: row.question,
            prompt: row.question,
            options: row.options.map((option) => ({
              optionId: option.optionId,
              id: option.optionId,
              label: option.label,
              ...(option.description ? { description: option.description } : {}),
            })),
            status: row.status,
            ...(row.answer !== undefined ? { answer: row.answer } : {}),
            ...(row.recommendation ? { recommendation: row.recommendation } : {}),
          },
        });
      }
    } catch {
      /* A decision read is an enhancement: the streamed conversation remains. */
    }
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
    this.resetCommands();
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

  /** True when the active transport can answer the canonical attachment
   * projection. A fixture or an older deployment cannot; the row then keeps
   * the event payload's display facts and names the degradation. */
  attachmentPartAvailable(): boolean {
    return typeof this.client?.attachmentPart === "function";
  }

  /** `GET /v1/sessions/{id}/attachments/{fileId}/part?seq=` — the one read the
   * attachment row and its plugin preview consume. */
  async attachmentPart(sessionId: string, fileId: string, seq: number): Promise<AttachmentPart> {
    const client = this.client;
    if (!client?.attachmentPart) throw new Error("此传输不提供附件投影");
    return client.attachmentPart(sessionId, fileId, seq);
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
      // A 202 admission carries the FIFO position/ETA: patch the row now so the
      // list says 排队中 instead of "ready" (the run row is queued; nothing is
      // executing yet).
      sendOpts.onQueued = (queue) => {
        this.patchSession(sessionId, { runStatus: "queued", queue });
      };
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
      // A destructive command's question is answered: replay the SAME
      // requestId so the host executes it once (or records the refusal).
      this.replayPendingForDecision(decisionId);
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
      const result = await this.client.retryTurn(sessionId, turnId);
      if (this.dismissedTurnError === turnId) this.dismissedTurnError = null;
      if (this.turnError()?.turnId === turnId) this.turnError.set(null);
      // A retried input can land in the placement queue (202): report that
      // honestly instead of optimistically saying the run is already running.
      if (result.queue) {
        this.patchSession(sessionId, { runStatus: "queued", queue: result.queue, pendingDecision: false });
      } else {
        this.patchSession(sessionId, { runStatus: "running", queue: undefined, pendingDecision: false });
      }
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

  async createSession(botId?: string): Promise<string | null> {
    try {
      // A Session belongs to exactly one Bot. In the live transport an
      // unowned session is a dead end (the worker refuses to run it), so the
      // caller resolves a Bot before asking; the demo transport keeps its
      // scripted sessions.
      if (this.mode() === "http" && !botId) {
        this.notice.set({
          tone: "danger",
          text: "这个租户还没有可用的员工（Bot）：先到「员工」页创建或选择一位。",
        });
        return null;
      }
      // One empty "new session" is enough in the demo: creating again while an
      // untouched one exists reopens it instead of appending a twin row. The
      // live transport cannot tell whose Bot an empty row belongs to (the list
      // does not project ownership), so it always creates instead of reusing
      // a session that might be unowned.
      if (this.mode() !== "http") {
        const empty = this.sessions().find(
          (row) => !row.lastMessage && (row.title ?? "").trim() === "新会话",
        );
        if (empty) {
          await this.refreshSessions();
          if (this.activeId() !== empty.sessionId) await this.openSession(empty.sessionId);
          return empty.sessionId;
        }
      }
      const id = await this.client.createSession(botId);
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
