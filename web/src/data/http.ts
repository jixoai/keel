// The live transport: `fetch` + SSE against keel's Harness API, and the
// `SurfaceCommands` that write through it. Same origin by default (`keel
// serve` hosts the web build, the API and the stream), so no CORS and no
// tenant headers in the browser; a dev origin can be passed explicitly.
import type {
  AttachmentRef,
  ConnectionState,
  Delivery,
  EventSource,
  FireResultView,
  McpAppAuditEntry,
  McpAppCallToolResult,
  McpAppCatalogueView,
  McpAppMessageOutcome,
  McpAppOpenLinkOutcome,
  McpAppResourceReadResult,
  MediaCapabilitiesView,
  MediaCapabilityStatus,
  MediaCapabilityView,
  ModelsResponse,
  ModelsSettingsReset,
  ModelsSettingsSaved,
  ModelsSettingsView,
  PluginsView,
  ProviderCredentialSaved,
  ProviderProbeView,
  ProvidersResponse,
  RealtimeStatusView,
  RetryResult,
  SecretSaved,
  SecretsView,
  SendOptions,
  DeliverablesOptions,
  DeliverablesView,
  SessionExportOptions,
  SessionExportView,
  SessionFilesQuery,
  SessionFilesView,
  SessionSearchOptions,
  SessionSearchView,
  SessionSummary,
  SessionTasksView,
  SettingsApi,
  FilePreviewView,
  TaskPanelAttachmentView,
  TaskPanelCardView,
  TaskPanelEventView,
  TaskPanelView,
  SettingsIssue,
  SkillDetailView,
  SkillInstallRequest,
  SkillRecordView,
  SkillSourceView,
  SkillsSettingsView,
  SkillUpdateRequest,
  SkillUpdateResult,
  SkillVersionView,
  TeamTaskView,
  TeamTasksView,
  TriggersView,
  UiCommands,
  WireEvent,
} from "./contract";
import { parseQueueFacts } from "./queue";
import {
  APP_ID_PATTERN,
  McpAppRequestError,
  parseMcpAppCatalogue,
  type McpAppContextBody,
  type McpAppMessageBody,
  type McpAppOpenLinkBody,
  type McpAppResourceReadBody,
  type McpAppToolCallBody,
} from "./mcp-apps";
import {
  identityPage,
  parseAccount,
  parseAuditEvent,
  parseAuthSession,
  parseBot,
  parseBotKey,
  parsePermissionGroup,
  parsePermissionGroupDetail,
  parseShareView,
  parseTeamBot,
  parseWhoAmI,
  type AccountView,
  type AuditEventView,
  type AuditQuery,
  type AuthSessionView,
  type BotKeyView,
  type BotView,
  type CreateAccountInput,
  type CreateBotInput,
  type CreateTeamBotInput,
  type IdentityPage,
  type PermissionGroupDetail,
  type PermissionGroupSummary,
  type ShareInput,
  type ShareResourceKind,
  type ShareView,
  type ShareWriteResult,
  type TeamBotView,
  type UpdateBotInput,
  type UpdateTeamBotInput,
  type WhoAmI,
} from "./identity";
import {
  parseProfile,
  parseRole,
  parseSessionCancel,
  parseSessionEvent,
  parseSubagent,
  type ProfileView,
  type RoleView,
  type SessionCancelView,
  type SessionEventView,
  type SubagentView,
} from "./bots-wire";

/** The structured detail of a refusal: the permission engine's 403 names the
 * key and reason; a 429 names the wait. */
export interface KeelErrorDetail {
  reason?: string;
  requiredKey?: string;
  scope?: string;
  retryAfterSeconds?: number;
}

export class KeelApiError extends Error {
  readonly status: number;
  /** The server's field-level diagnostics (settings 400), verbatim. */
  readonly issues: SettingsIssue[];
  /** The current revision of a 409 conflict, when the server named one. */
  readonly revision: number | undefined;
  /** The server's stable failure code (`pinned`, `untrusted_source`, ...).
   * A page renders it as the specific state, not a generic error. */
  readonly code: string | undefined;
  /** A 403 reason (`missing_grant`, `missing_relation`, ...) and the key that
   * was required. Kept so a permission page can say what is missing. */
  readonly reason: string | undefined;
  readonly requiredKey: string | undefined;
  readonly scope: string | undefined;
  /** A 429's Retry-After, in whole seconds. */
  readonly retryAfterSeconds: number | undefined;
  /** The server's request id (`X-Request-ID`), when the edge answered one. A
   * failed page prints it so a reader can quote it to the administrator. */
  readonly requestId: string | undefined;
  constructor(
    status: number,
    message: string,
    issues: SettingsIssue[] = [],
    revision?: number,
    code?: string,
    detail: KeelErrorDetail = {},
    requestId?: string,
  ) {
    super(message);
    this.status = status;
    this.issues = issues;
    this.revision = revision;
    this.code = code;
    this.reason = detail.reason;
    this.requiredKey = detail.requiredKey;
    this.scope = detail.scope;
    this.retryAfterSeconds = detail.retryAfterSeconds;
    this.requestId = requestId;
  }
}

const retryable = (status: number) => status === 408 || status === 429 || status >= 500;

/** The slice of `fetch` this transport needs. Injectable so a test (or an
 * embedding host) can answer requests without a network; production always
 * uses the platform's `globalThis.fetch`. */
export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

const asRecord = (value: unknown): Record<string, unknown> =>
  value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};

const issueList = (value: unknown): SettingsIssue[] => {
  if (!Array.isArray(value)) return [];
  return value
    .map((item): SettingsIssue => {
      if (typeof item === "string") return { field: "", message: item };
      const row = asRecord(item);
      return { field: String(row.field ?? ""), message: String(row.message ?? "") };
    })
    .filter((issue) => issue.field || issue.message);
};

const numberOr = (value: unknown, fallback: number): number =>
  typeof value === "number" && Number.isFinite(value) ? value : fallback;

const listOf = (value: unknown): unknown[] => (Array.isArray(value) ? value : []);

const asText = (value: unknown): string => (typeof value === "string" ? value : "");

/** The share route family: sessions, bots and team bots use the same trio. */
const sharePath = (kind: ShareResourceKind, resourceId: string): string => {
  const family = kind === "session" ? "sessions" : kind === "bot" ? "bots" : "team-bots";
  return `/v1/${family}/${encodeURIComponent(resourceId)}/shares`;
};

/** One add/revoke answer. A malformed share is an error, not an empty success:
 * the dialog must not claim a tuple the server did not confirm. */
const shareWriteOf = (value: unknown): ShareWriteResult => {
  const body = asRecord(value);
  const share = parseShareView(body.share);
  if (!share) throw new KeelApiError(500, "服务端返回了无法识别的共享结果");
  const result: ShareWriteResult = { share };
  if (typeof body.created === "boolean") result.created = body.created;
  if (typeof body.revoked === "boolean") result.revoked = body.revoked;
  return result;
};

/** One immutable skill version as the wire sends it. */
function parseSkillVersion(value: unknown): SkillVersionView {
  const row = asRecord(value);
  return {
    hash: asText(row.hash),
    createdAt: asText(row.createdAt) || undefined,
    files: typeof row.files === "number" ? row.files : undefined,
    bytes: typeof row.bytes === "number" ? row.bytes : undefined,
  };
}

/** One installed skill. Unknown trust/conflict values travel verbatim (the
 * page renders the label it was given instead of guessing a safe one). */
function parseSkillRecord(value: unknown): SkillRecordView {
  const row = asRecord(value);
  return {
    name: asText(row.name),
    description: asText(row.description) || undefined,
    trust: asText(row.trust),
    sourceId: asText(row.sourceId) || undefined,
    sourceKind: asText(row.sourceKind) || undefined,
    sourceUrl: asText(row.sourceUrl) || undefined,
    currentHash: asText(row.currentHash),
    pinnedHash: asText(row.pinnedHash) || undefined,
    installedAt: asText(row.installedAt) || undefined,
    updatedAt: asText(row.updatedAt) || undefined,
    versions: listOf(row.versions).map(parseSkillVersion),
    path: asText(row.path) || undefined,
    fileCount: typeof row.fileCount === "number" ? row.fileCount : undefined,
    bytes: typeof row.bytes === "number" ? row.bytes : undefined,
    updateAvailable: row.updateAvailable === true,
    availableHash: asText(row.availableHash) || undefined,
    conflict: asText(row.conflict) || undefined,
    diagnostics: listOf(row.diagnostics).map((item) => asText(item)).filter((item) => item),
  };
}

function parseSkillSource(value: unknown): SkillSourceView {
  const row = asRecord(value);
  return {
    id: asText(row.id),
    kind: asText(row.kind),
    url: asText(row.url),
    trust: asText(row.trust),
    createdAt: asText(row.createdAt) || undefined,
  };
}

/** The error body's message, field issues, conflict revision, code and the
 * permission detail (reason/requiredKey/scope). */
async function readErrorBody(
  response: Response,
): Promise<{
  message: string;
  issues: SettingsIssue[];
  revision: number | undefined;
  code: string | undefined;
  detail: KeelErrorDetail;
}> {
  const issues: SettingsIssue[] = [];
  let revision: number | undefined;
  let code: string | undefined;
  let message = "";
  let reason: string | undefined;
  let requiredKey: string | undefined;
  let scope: string | undefined;
  try {
    const text = await response.text();
    try {
      const body = asRecord(JSON.parse(text));
      if (typeof body.error === "string" && body.error) message = body.error;
      else if (typeof body.message === "string" && body.message) message = body.message;
      issues.push(...issueList(body.issues));
      if (typeof body.revision === "number" && Number.isFinite(body.revision)) revision = body.revision;
      if (typeof body.code === "string" && body.code) code = body.code;
      if (typeof body.reason === "string" && body.reason) reason = body.reason;
      if (typeof body.requiredKey === "string" && body.requiredKey) requiredKey = body.requiredKey;
      if (typeof body.key === "string" && body.key) requiredKey = requiredKey ?? body.key;
      if (typeof body.scope === "string" && body.scope) scope = body.scope;
      if (!message && issues.length) message = issues[0]?.message ?? "";
    } catch {
      message = text;
    }
    if (!message) message = `HTTP ${response.status}`;
  } catch {
    message = `HTTP ${response.status}`;
  }
  return {
    message,
    issues,
    revision,
    code,
    detail: {
      reason,
      requiredKey,
      scope,
      retryAfterSeconds: parseRetryAfter(response.headers.get("Retry-After")),
    },
  };
}

/** Retry-After is either seconds or an HTTP date; the UI shows whole seconds. */
function parseRetryAfter(value: string | null): number | undefined {
  if (!value) return undefined;
  const trimmed = value.trim();
  if (/^\d+$/.test(trimmed)) {
    const seconds = Number(trimmed);
    return Number.isFinite(seconds) && seconds >= 0 ? seconds : undefined;
  }
  const at = Date.parse(trimmed);
  if (!Number.isFinite(at)) return undefined;
  return Math.max(0, Math.ceil((at - Date.now()) / 1000));
}

/** One sentence per refusal status, with the server's own message appended when
 * it adds information (a 403's reason, a 400's field). Never replaces the
 * server's code: the caller still holds the KeelApiError. */
export function errorMessageOf(error: unknown): string {
  if (!(error instanceof KeelApiError)) {
    return error instanceof Error && error.message ? error.message : String(error);
  }
  const detail = error.message && error.message !== "HTTP " + error.status ? `：${error.message}` : "";
  switch (error.status) {
    case 400:
      return `提交的内容无效${detail}`;
    case 401:
      return "登录已失效，请重新登录。";
    case 403:
      return error.requiredKey
        ? `权限不足：需要 ${error.requiredKey}${detail}`
        : `权限不足${detail}`;
    case 404:
      return "没有找到这条记录，它可能已被删除。";
    case 409:
      return `操作冲突${detail}`;
    case 429:
      return error.retryAfterSeconds !== undefined
        ? `操作太频繁，请在 ${error.retryAfterSeconds} 秒后重试。`
        : "操作太频繁，请稍后重试。";
    case 503:
      return `服务暂不可用${detail}`;
    default:
      return error.message || `请求失败（HTTP ${error.status}）`;
  }
}

async function readError(response: Response): Promise<string> {
  return (await readErrorBody(response)).message;
}

// ---- deployment capability normalization ---------------------------------
//
// `GET /v1/media/capabilities` and `GET /v1/realtime/status` describe the
// deployment's own process, so their bodies are operator facts, not model
// output. This layer keeps only what the settings page may print: documented
// KEEL_* environment NAMES, a short human reason and a model id. URLs, keys,
// stack traces and transport errors are dropped here, before any screen sees
// them.

/** The environment names the settings page may show. Anything else in a
 * server body is not a documented operator hint and is not displayed. */
const CAPABILITY_ENV_NAME = /^KEEL_(?:MEDIA|REALTIME)_[A-Z0-9_]+$/;

const envNamesOf = (value: unknown): string[] => {
  const raw = Array.isArray(value) ? value : typeof value === "string" ? value.split(/[,\s]+/) : [];
  const names: string[] = [];
  for (const item of raw) {
    if (typeof item !== "string") continue;
    const name = item.trim();
    if (!CAPABILITY_ENV_NAME.test(name) || names.includes(name)) continue;
    names.push(name);
  }
  return names;
};

/** A provider reason is free text from outside this process. A reader can act
 * on a sentence; a URL, a credential, a stack trace or a transport error is a
 * deployment internal (and printing it would leak what the page promises not
 * to). */
const isSafeReason = (text: string): boolean => {
  if (!text || text.length > 160) return false;
  if (/https?:\/\//i.test(text)) return false;
  if (/\b(?:api[_-]?key|access[_-]?key|token|secret|bearer|authorization|password)\b\s*[:=]/i.test(text)) {
    return false;
  }
  if (/\b(?:panic|goroutine|runtime error|segmentation fault|nil pointer)\b/i.test(text)) return false;
  if (/\b[\w.-]+\.go:\d+\b/.test(text)) return false;
  if (/\b(?:dial tcp|connection refused|connection reset|no such host|i\/o timeout)\b/i.test(text)) return false;
  if (/[A-Za-z0-9_-]{32,}/.test(text)) return false;
  return true;
};

const reasonOf = (row: Record<string, unknown>): string | undefined => {
  const raw = row.reason ?? row.detail ?? row.message;
  if (typeof raw !== "string") return undefined;
  const text = raw.trim().replace(/\s+/g, " ");
  return text && isSafeReason(text) ? text : undefined;
};

const modelIdOf = (row: Record<string, unknown>): string | undefined => {
  const raw = row.model ?? row.modelId ?? row.defaultModel;
  if (typeof raw !== "string") return undefined;
  const text = raw.trim();
  if (!text || text.length > 120 || /\s/.test(text) || text.includes("://")) return undefined;
  return text;
};

const capabilityStatusOf = (row: Record<string, unknown>): MediaCapabilityStatus => {
  const token = typeof row.status === "string" ? row.status.trim().toLowerCase().replace(/_/g, "-") : "";
  if (["available", "configured", "ready", "ok", "enabled", "supported", "on"].includes(token)) {
    return "available";
  }
  if (["unsupported", "not-supported", "unavailable", "disabled", "unimplemented", "off"].includes(token)) {
    return "unsupported";
  }
  if (["unconfigured", "not-configured", "missing", "needs-config", "needs-configuration"].includes(token)) {
    return "unconfigured";
  }
  if (typeof row.available === "boolean") return row.available ? "available" : "unconfigured";
  if (typeof row.configured === "boolean") return row.configured ? "available" : "unconfigured";
  if (typeof row.supported === "boolean") return row.supported ? "available" : "unsupported";
  if (row.enabled === true) return "available";
  return "unconfigured";
};

/** `imageEdit` / `image_edit` / `image.edit` all name the same row. */
const canonicalCapabilityId = (value: unknown): string => {
  if (typeof value !== "string") return "";
  return value
    .trim()
    .replace(/([a-z0-9])([A-Z])/g, "$1-$2")
    .toLowerCase()
    .replace(/[_.\s]+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "");
};

const rowRecordOf = (value: unknown): Record<string, unknown> | null =>
  value !== null && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : null;

const capabilityViewOf = (id: string, row: Record<string, unknown>): MediaCapabilityView => ({
  id: canonicalCapabilityId(id),
  status: capabilityStatusOf(row),
  reason: reasonOf(row),
  env: envNamesOf(row.env ?? row.envs ?? row.missingEnv ?? row.missingEnvVars ?? row.requiredEnv ?? row.requiredEnvVars),
  model: modelIdOf(row),
});

const CAPABILITY_CONTAINERS = ["capabilities", "media", "mediaCapabilities", "items"] as const;

/** Keys of the flat report that are not capabilities themselves. */
const FLAT_REPORT_KEYS = new Set(["configured", "ready", "models", "env", "envs", "missingEnv", "requiredEnv"]);

/** The gateway's own report is a flat map: `{configured, llm, image, decision,
 * video, tts, stt, realtime, models}`. `true` means the deployment can serve
 * it, `false` means it cannot; `models` names the model behind each one. A
 * false is「不支持」, not「未配置」: the gateway answered, and the answer was no. */
const flatCapabilityRowsOf = (body: Record<string, unknown>): MediaCapabilityView[] => {
  const models = rowRecordOf(body.models) ?? {};
  const rows: MediaCapabilityView[] = [];
  for (const [key, value] of Object.entries(body)) {
    if (typeof value !== "boolean" || FLAT_REPORT_KEYS.has(key)) continue;
    const id = canonicalCapabilityId(key);
    if (!id) continue;
    const model = typeof models[key] === "string" ? (models[key] as string) : undefined;
    rows.push({ id, status: value ? "available" : "unsupported", env: [], ...(model ? { model } : {}) });
  }
  // Editing rides on the image model: a deployment that can generate can also
  // edit with the same provider, while the screen lists the two operations apart.
  const image = rows.find((row) => row.id === "image" && row.status === "available");
  if (image && !rows.some((row) => row.id === "image-edit")) {
    rows.push({ ...image, id: "image-edit" });
  }
  return rows;
};

/** Tolerates the three natural wire shapes: a map keyed by capability id, an
 * array of rows carrying `id`/`capability`/`kind`, and the gateway's own flat
 * boolean report. */
const mediaCapabilityRowsOf = (body: Record<string, unknown>): MediaCapabilityView[] => {
  const containerKey = CAPABILITY_CONTAINERS.find((key) => key in body);
  if (containerKey === undefined) return flatCapabilityRowsOf(body);
  const container = body[containerKey];
  const rows: MediaCapabilityView[] = [];
  if (Array.isArray(container)) {
    for (const item of container) {
      const row = rowRecordOf(item);
      if (!row) continue;
      const id = canonicalCapabilityId(row.id ?? row.capability ?? row.kind ?? row.name ?? row.type);
      if (id) rows.push(capabilityViewOf(id, row));
    }
    return rows;
  }
  const map = rowRecordOf(container);
  if (!map) return rows;
  for (const [key, value] of Object.entries(map)) {
    if (typeof value === "boolean") {
      const id = canonicalCapabilityId(key);
      if (id) rows.push({ id, status: value ? "available" : "unconfigured", env: [] });
      continue;
    }
    const row = rowRecordOf(value);
    if (!row) continue;
    const id = canonicalCapabilityId(row.id ?? row.capability ?? key);
    if (id) rows.push(capabilityViewOf(id, row));
  }
  return rows;
};

const mediaCapabilitiesOf = (body: Record<string, unknown>): MediaCapabilitiesView => ({
  configured:
    typeof body.configured === "boolean" ? body.configured : typeof body.ready === "boolean" ? body.ready : true,
  capabilities: mediaCapabilityRowsOf(body),
  env: envNamesOf(body.env ?? body.envs ?? body.missingEnv ?? body.requiredEnv),
});

/** A provider/kind name is a display label; a URL smuggled into one is not. */
const safeLabelOf = (value: unknown): string | undefined => {
  if (typeof value !== "string") return undefined;
  const text = value.trim();
  if (!text || text.length > 80 || text.includes("://")) return undefined;
  return text;
};

const realtimeStatusOf = (body: Record<string, unknown>): RealtimeStatusView => ({
  configured: body.configured === true,
  provider: safeLabelOf(body.provider),
  kind: safeLabelOf(body.kind),
  models: Array.isArray(body.models)
    ? body.models.map((model) => safeLabelOf(model)).filter((model): model is string => model !== undefined)
    : [],
  inputSampleRate: typeof body.inputSampleRate === "number" ? body.inputSampleRate : undefined,
  outputSampleRate: typeof body.outputSampleRate === "number" ? body.outputSampleRate : undefined,
});

/** `POST /v1/providers/{id}/test` → the view. The error text is the server's
 * measured reason and never carries a credential (the server composes it from
 * field paths); the model ids are plain strings. */
const providerProbeOf = (body: Record<string, unknown>): ProviderProbeView => ({
  providerId: String(body.providerId ?? ""),
  providerType: textOr(body.providerType),
  apiType: textOr(body.apiType),
  endpoint: textOr(body.endpoint),
  ok: body.ok === true,
  status: String(body.status ?? "unreachable"),
  latencyMs: typeof body.latencyMs === "number" ? body.latencyMs : undefined,
  modelCount: typeof body.modelCount === "number" ? body.modelCount : undefined,
  models: Array.isArray(body.models)
    ? body.models
        .map((model) => (typeof model === "string" ? model.trim() : ""))
        .filter((model) => model !== "")
    : [],
  error: textOr(body.error),
  issues: issueList(body.issues),
});

// ---- agent & team registry normalization ---------------------------------
//
// The shapes are `internal/httpapi/agentteam_dto.go`. A listing may arrive as
// `{agents:[...]}` / `{teams:[...]}` or as a bare array (the envelope is the
// DTO's; a bare collection is accepted so a route that answers the array
// directly still renders). Every field is coerced here so a malformed row can
// never crash a card; ids and names survive even when the rest is missing.

const textOr = (value: unknown): string | undefined =>
  typeof value === "string" && value.trim() !== "" ? value : undefined;

const objectOr = (value: unknown): Record<string, unknown> | undefined =>
  value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : undefined;

/** The CSRF token of the app's live session. KeelHttp keeps it current; the
 * media module (which writes through its own fetch) reads it so every non-GET
 * `/v1` request carries the header, whichever code path sends it. */
let activeCSRFToken: string | null = null;

export const currentCSRFToken = (): string | null => activeCSRFToken;

/** Attach the session's CSRF token to a non-GET request's headers. */
export function withCSRF(
  headers: Record<string, string> | undefined,
  method: string,
): Record<string, string> {
  const merged = headers ? { ...headers } : {};
  const upper = method.toUpperCase();
  if (activeCSRFToken && upper !== "GET" && upper !== "HEAD" && upper !== "OPTIONS") {
    merged["X-Keel-CSRF"] = activeCSRFToken;
  }
  return merged;
}

export class KeelHttp implements SettingsApi {
  readonly base: string;
  private readonly fetchImpl: FetchLike;
  /** The double-submit CSRF token (`/v1/auth/session` and every login/password
   * answer carry it). Writes attach it as X-Keel-CSRF. */
  private csrfToken: string | null = null;
  /** Called on a 401 that means "the session is no longer valid" (an expired,
   * revoked or disabled session). The store turns that into the login gate; a
   * failed login attempt is not routed here. */
  private unauthorizedHandler: ((error: KeelApiError, path: string) => void) | null = null;

  constructor(base = "", fetchImpl?: FetchLike) {
    this.base = base.replace(/\/$/, "");
    // Deferred, so constructing a client never touches a platform without
    // `fetch`; only a request does.
    this.fetchImpl = fetchImpl ?? ((input, init) => fetch(input, init));
  }

  url(path: string): string {
    return `${this.base}${path}`;
  }

  /** The request transport. The event stream needs it too, so the type stays
   * usable by a test or a host that supplies its own fetch. */
  transport(): FetchLike {
    return this.fetchImpl;
  }

  /** Install the session-expiry callback (the store's login gate). */
  onUnauthorized(handler: ((error: KeelApiError, path: string) => void) | null): void {
    this.unauthorizedHandler = handler;
  }

  /** The CSRF token in force, if any (a test can assert the header). */
  csrf(): string | null {
    return this.csrfToken;
  }

  /** Forget the session's CSRF token (logout, or a rejected session). */
  clearAuth(): void {
    this.csrfToken = null;
    activeCSRFToken = null;
  }

  /** Parse a session document and remember its CSRF token. */
  private captureSession(value: unknown): AuthSessionView {
    const session = parseAuthSession(value);
    if (session.csrfToken) {
      this.csrfToken = session.csrfToken;
      activeCSRFToken = session.csrfToken;
    }
    return session;
  }

  /** The headers a request runs with: the caller's own plus the CSRF token for
   * every non-GET write. A write without a token is still sent (the server's
   * 403 is the test of the browser's session); it does not silently succeed.
   * Plain objects keep this working on a host with no `Headers` global. */
  private headersFor(init: RequestInit | undefined, method: string): Record<string, string> {
    const headers: Record<string, string> = {};
    const raw = init?.headers;
    if (raw && typeof Headers !== "undefined" && raw instanceof Headers) {
      raw.forEach((value, key) => {
        headers[key] = value;
      });
    } else if (Array.isArray(raw)) {
      for (const [key, value] of raw) headers[key] = value;
    } else if (raw) {
      Object.assign(headers, raw as Record<string, string>);
    }
    const token = this.csrfToken ?? activeCSRFToken;
    if (token && method !== "GET" && method !== "HEAD" && method !== "OPTIONS") {
      headers["X-Keel-CSRF"] = token;
    }
    return headers;
  }

  async request(path: string, init?: RequestInit): Promise<Response> {
    const method = (init?.method ?? "GET").toUpperCase();
    const response = await this.fetchImpl(this.url(path), {
      credentials: "same-origin",
      ...init,
      headers: this.headersFor(init, method),
    });
    if (!response.ok) {
      const { message, issues, revision, code, detail } = await readErrorBody(response);
      const requestId = response.headers.get("x-request-id") ?? response.headers.get("x-keel-request-id") ?? undefined;
      const error = new KeelApiError(response.status, message, issues, revision, code, detail, requestId ?? undefined);
      // A 401 from a real endpoint is the session ending. Login/bootstrap 401s
      // are credential answers for the form, and a password change's 401 is the
      // current password being wrong: neither may throw the reader out.
      if (
        response.status === 401 &&
        this.unauthorizedHandler &&
        path !== "/v1/auth/login" &&
        path !== "/v1/auth/bootstrap" &&
        !(path === "/v1/auth/password" && code === "invalid_credentials")
      ) {
        this.unauthorizedHandler(error, path);
      }
      throw error;
    }
    return response;
  }

  /**
   * Probe the API: 2xx means a live, authenticated keel serve; 401/403 means a
   * live server that wants a session (the login gate); anything else (network
   * error, 404 from a static host) means there is no live API.
   */
  async probe(timeoutMs = 1600): Promise<"ok" | "auth" | "down"> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    try {
      const response = await fetch(this.url("/v1/capabilities"), {
        credentials: "same-origin",
        signal: controller.signal,
      });
      if (response.ok) return "ok";
      if (response.status === 401 || response.status === 403) return "auth";
      return "down";
    } catch {
      return "down";
    } finally {
      clearTimeout(timer);
    }
  }

  /**
   * The session document. It is also the anonymous CSRF source: a browser that
   * has never logged in needs the token this answer issues before it can POST
   * the login form (the server checks CSRF on every auth write).
   */
  async authSession(): Promise<AuthSessionView> {
    const response = await this.request("/v1/auth/session");
    return this.captureSession(await response.json());
  }

  /** Exchange username+password for the server session. The answer carries
   * `mustChangePassword`; a 401 is the same shape for a missing account and a
   * wrong password (the server does not say which). */
  async login(username: string, password: string): Promise<AuthSessionView> {
    if (!this.csrfToken) await this.authSession();
    const response = await this.request("/v1/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password }),
    });
    return this.captureSession(await response.json());
  }

  /** Redeem the one-time setup token. The server does not log the browser in:
   * the account starts in the must-change state, so the caller logs in next. */
  async bootstrap(token: string, username: string, password: string, displayName?: string): Promise<void> {
    if (!this.csrfToken) await this.authSession();
    await this.request("/v1/auth/bootstrap", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        token,
        username,
        password,
        ...(displayName && displayName.trim() ? { displayName: displayName.trim() } : {}),
      }),
    });
  }

  /** Change the password; the server revokes every old session and answers the
   * fresh one (its CSRF token included). */
  async changePassword(currentPassword: string, newPassword: string): Promise<AuthSessionView> {
    const response = await this.request("/v1/auth/password", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ currentPassword, newPassword }),
    });
    return this.captureSession(await response.json());
  }

  async logout(): Promise<void> {
    await this.request("/v1/auth/logout", { method: "POST" });
    this.clearAuth();
  }

  /** The caller's identity and effective permissions. The UI's `can()` reads
   * this answer; the server remains the authority for every request. */
  async whoami(): Promise<WhoAmI> {
    const response = await this.request("/v1/auth/whoami");
    return parseWhoAmI(await response.json());
  }

  async capabilities(): Promise<Record<string, unknown>> {
    const response = await this.request("/v1/capabilities");
    return (await response.json()) as Record<string, unknown>;
  }

  /** `GET /v1/mcp/apps`: the app catalogue (plus the theme palettes and the
   * optional second origin). A deployment without the surface answers 501; the
   * caller reports the server's reason and falls back to text. */
  async mcpApps(): Promise<McpAppCatalogueView> {
    const response = await this.request("/v1/mcp/apps");
    return parseMcpAppCatalogue(await response.json());
  }

  /** Best-effort audit write (P3.7's durable store is not wired yet): a
   * deployment without the route answers 404/405, which is reported as `false`
   * and never blocks the UI. `detail` comes from the caller and carries no URL
   * query. */
  async auditMcpApp(entry: McpAppAuditEntry): Promise<boolean> {
    try {
      await this.request("/v1/mcp/apps/audit", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(entry),
      });
      return true;
    } catch {
      return false;
    }
  }

  /** One View-initiated app request. The proxy is the only path: no app ever
   * reaches an MCP server or the product API directly. A non-2xx body is kept
   * whole (`{jsonrpc,error:{code,message}}` or `{error:string}`) so the bridge
   * can answer the View with the server's own code. Cookie/session auth is the
   * ordinary same-origin one. */
  private async mcpAppProxy<T>(appId: string, action: string, body: unknown): Promise<T> {
    if (!APP_ID_PATTERN.test(appId)) throw new Error("invalid app id");
    const response = await this.fetchImpl(
      this.url(`/v1/mcp/apps/${encodeURIComponent(appId)}/${action}`),
      {
        method: "POST",
        credentials: "same-origin",
        headers: this.headersFor({ headers: { "Content-Type": "application/json" } }, "POST"),
        body: JSON.stringify(body),
      },
    );
    let payload: unknown = null;
    try {
      payload = await response.json();
    } catch {
      payload = null;
    }
    // 202 is a confirmation still waiting on the user: never a success frame.
    if (!response.ok || response.status === 202) {
      throw new McpAppRequestError(response.status, payload);
    }
    return payload as T;
  }

  /** `POST /v1/mcp/apps/{appId}/tools/call`: a standard `CallToolResult`. */
  async mcpAppToolCall(appId: string, body: McpAppToolCallBody): Promise<McpAppCallToolResult> {
    return this.mcpAppProxy<McpAppCallToolResult>(appId, "tools/call", body);
  }

  /** `POST /v1/mcp/apps/{appId}/resources/read`: `{contents:[...]}`. */
  async mcpAppReadResource(
    appId: string,
    body: McpAppResourceReadBody,
  ): Promise<McpAppResourceReadResult> {
    return this.mcpAppProxy<McpAppResourceReadResult>(appId, "resources/read", body);
  }

  /** `POST /v1/mcp/apps/{appId}/ui/message`: the confirmed send/edit/discard
   * branch. The decision wait happens on the server, bounded by its timeout. */
  async mcpAppMessage(appId: string, body: McpAppMessageBody): Promise<McpAppMessageOutcome> {
    return this.mcpAppProxy<McpAppMessageOutcome>(appId, "ui/message", body);
  }

  /** `POST /v1/mcp/apps/{appId}/ui/open-link`: `{opened,url}` after the
   * confirmation; the caller opens it with `noopener,noreferrer`. */
  async mcpAppOpenLink(appId: string, body: McpAppOpenLinkBody): Promise<McpAppOpenLinkOutcome> {
    return this.mcpAppProxy<McpAppOpenLinkOutcome>(appId, "ui/open-link", body);
  }

  /** `POST /v1/mcp/apps/{appId}/context`: `ui/update-model-context`. The route
   * answers a JSON-RPC envelope; the bridge answers the View with `{}`. */
  async mcpAppUpdateContext(appId: string, body: McpAppContextBody): Promise<unknown> {
    return this.mcpAppProxy<unknown>(appId, "context", body);
  }

  // ---- sessions ---------------------------------------------------------

  async listSessions(limit = 50, after?: string): Promise<SessionSummary[]> {
    const query = new URLSearchParams({ limit: String(limit) });
    if (after) query.set("after", after);
    const response = await this.request(`/v1/sessions?${query.toString()}`);
    const body = (await response.json()) as unknown;
    const rows = Array.isArray(body)
      ? body
      : Array.isArray((body as { sessions?: unknown }).sessions)
        ? (body as { sessions: unknown[] }).sessions
        : [];
    return rows.map((row) => this.sessionOf(row as Record<string, unknown>));
  }

  private sessionOf(row: Record<string, unknown>): SessionSummary {
    return {
      sessionId: String(row.sessionId ?? row.id ?? ""),
      createdAt: row.createdAt as string | undefined,
      updatedAt: row.updatedAt as string | undefined,
      lastSeq: typeof row.lastSeq === "number" ? row.lastSeq : undefined,
      runStatus: row.runStatus as string | undefined,
      lastMessageAt: row.lastMessageAt as string | undefined,
      title: row.title as string | undefined,
      lastMessage: (row.lastMessage ?? row.summary) as string | undefined,
      ownerAccountId: row.ownerAccountId as string | undefined,
      createdByAccountId: row.createdByAccountId as string | undefined,
      ownerDisplayName: row.ownerDisplayName as string | undefined,
      shared: row.shared === true,
      sharedWithMe: row.sharedWithMe === true,
    };
  }

  async createSession(): Promise<string> {
    const response = await this.request("/v1/sessions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    const body = (await response.json()) as { sessionId?: string; id?: string };
    const id = body.sessionId ?? body.id;
    if (!id) throw new KeelApiError(500, "服务端没有返回 sessionId");
    return id;
  }

  async deleteSession(sessionId: string): Promise<void> {
    await this.request(`/v1/sessions/${encodeURIComponent(sessionId)}`, { method: "DELETE" });
  }

  /** The member task cards linked to a captain session. A card is read as the
   * projection's own fields; `result`/`summary` do not exist on this wire, so a
   * missing one stays missing instead of becoming an invented sentence. */
  async teamTasks(sessionId: string, limit = 50, after?: string): Promise<TeamTasksView> {
    const query = new URLSearchParams({ limit: String(limit) });
    if (after) query.set("after", after);
    const response = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/team-tasks?${query.toString()}`,
    );
    const body = (await response.json()) as {
      tasks?: unknown[];
      nextCursor?: unknown;
      hasMore?: unknown;
    };
    const rows = Array.isArray(body.tasks) ? body.tasks : [];
    const tasks = rows.flatMap((row): TeamTaskView[] => {
      const record = asRecord(row);
      if (typeof record.taskId !== "string" || !record.taskId) return [];
      return [
        {
          taskId: record.taskId,
          parentTaskId: typeof record.parentTaskId === "string" ? record.parentTaskId : undefined,
          sessionId: typeof record.sessionId === "string" ? record.sessionId : "",
          title: typeof record.title === "string" ? record.title : "",
          status: typeof record.status === "string" ? record.status : "open",
          origin: typeof record.origin === "string" ? record.origin : undefined,
          createdAt: typeof record.createdAt === "string" ? record.createdAt : undefined,
          updatedAt: typeof record.updatedAt === "string" ? record.updatedAt : undefined,
          closedSeq: typeof record.closedSeq === "number" ? record.closedSeq : undefined,
        },
      ];
    });
    return {
      tasks,
      nextCursor: typeof body.nextCursor === "string" && body.nextCursor ? body.nextCursor : undefined,
      hasMore: body.hasMore === true,
    };
  }

  /** Load the last few events of a session for the list's summary/title. */
  async tail(sessionId: string, lastSeq: number, count = 6): Promise<WireEvent[]> {
    const after = Math.max(0, lastSeq - count);
    const response = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/events?after=${after}&limit=${count}`,
    );
    const body = (await response.json()) as { events?: unknown[] };
    return (body.events ?? []).map((row) => {
      const event = row as { seq?: number; kind?: string; payload?: unknown };
      return {
        seq: Number(event.seq ?? 0),
        kind: String(event.kind ?? "message"),
        payload: event.payload,
      };
    });
  }

  // ---- per session ------------------------------------------------------

  source(sessionId: string): HttpEventSource {
    return new HttpEventSource(this, sessionId);
  }

  commands(sessionId: string): HttpCommands {
    return new HttpCommands(this, sessionId);
  }

  // ---- settings (change keel-model-settings) ----------------------------

  /** `GET /v1/settings/models` — the effective models document. */
  async modelsSettings(): Promise<ModelsSettingsView> {
    const response = await this.request("/v1/settings/models");
    const body = asRecord(await response.json());
    return {
      revision: numberOr(body.revision, 0),
      source: body.source === "stored" ? "stored" : "deployment",
      document: asRecord(body.document),
      issues: issueList(body.issues),
    };
  }

  /** `PUT /v1/settings/models` — validate + store + hot-reload in one write.
   * 400 carries field issues, 409 the revision that won. */
  async saveModelsSettings(
    document: Record<string, unknown>,
    expectedRevision?: number,
  ): Promise<ModelsSettingsSaved> {
    const payload: Record<string, unknown> = { ...document };
    if (expectedRevision !== undefined) payload.expectedRevision = expectedRevision;
    const response = await this.request("/v1/settings/models", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    const body = asRecord(await response.json());
    const selectionRow = asRecord(body.selection);
    return {
      revision: numberOr(body.revision, expectedRevision ?? 0),
      source: typeof body.source === "string" ? body.source : "stored",
      selection:
        typeof selectionRow.modelId === "string" && selectionRow.modelId
          ? {
              providerId: typeof selectionRow.providerId === "string" ? selectionRow.providerId : undefined,
              modelId: selectionRow.modelId,
              reasoningLevel:
                typeof selectionRow.reasoningLevel === "string" ? selectionRow.reasoningLevel : undefined,
            }
          : undefined,
      providersExecutable:
        typeof body.providersExecutable === "number" ? body.providersExecutable : undefined,
      modelsSelectable: typeof body.modelsSelectable === "number" ? body.modelsSelectable : undefined,
    };
  }

  /** `DELETE /v1/settings/models` — clear the override, back to the
   * deployment document. */
  async resetModelsSettings(expectedRevision?: number): Promise<ModelsSettingsReset> {
    const query =
      expectedRevision !== undefined ? `?expectedRevision=${encodeURIComponent(String(expectedRevision))}` : "";
    const response = await this.request(`/v1/settings/models${query}`, { method: "DELETE" });
    const body = asRecord(await response.json());
    return { revision: numberOr(body.revision, 0), source: "deployment" };
  }

  /** `POST /v1/sessions/{id}/retry {turnId}` — the server re-asks the input the
   * turn answered; the ledger only grows. Deliberately not idempotent: the
   * same turn retried twice is two intentional attempts. */
  async retryTurn(sessionId: string, turnId: string): Promise<RetryResult> {
    const response = await this.request(`/v1/sessions/${encodeURIComponent(sessionId)}/retry`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ turnId }),
    });
    const body = asRecord(await response.json());
    const queue = parseQueueFacts(body);
    return {
      created: body.created === true,
      seq: numberOr(body.seq, 0),
      requestId: typeof body.requestId === "string" ? body.requestId : "",
      ...(queue ? { queue } : {}),
    };
  }

  /** `GET /v1/providers` — the resolved settings view (visible providers). */
  async providers(): Promise<ProvidersResponse> {
    const response = await this.request("/v1/providers");
    const body = asRecord(await response.json());
    return {
      providers: Array.isArray(body.providers) ? (body.providers as ProvidersResponse["providers"]) : [],
      issues: issueList(body.issues),
    };
  }

  /** `GET /v1/models` — the flat selectable list a picker consumes. */
  async models(): Promise<ModelsResponse> {
    const response = await this.request("/v1/models");
    const body = asRecord(await response.json());
    const enforcement = asRecord(body.enforcement);
    return {
      models: Array.isArray(body.models) ? (body.models as ModelsResponse["models"]) : [],
      issues: issueList(body.issues),
      enforcement: {
        toolCall: enforcement.toolCall === true,
        media: enforcement.media === true,
      },
    };
  }

  /** `GET /v1/media/capabilities` — what this deployment's media gateway can
   * actually do. A 503 means the gateway itself is not configured: an ordinary
   * operator state the page renders as「未配置」, so it is folded into the view
   * instead of thrown. */
  async mediaCapabilities(): Promise<MediaCapabilitiesView> {
    let response: Response;
    try {
      response = await this.request("/v1/media/capabilities");
    } catch (error) {
      if (error instanceof KeelApiError && error.status === 503) {
        return { configured: false, capabilities: [], env: [] };
      }
      throw error;
    }
    return mediaCapabilitiesOf(asRecord(await response.json()));
  }

  /** `GET /v1/realtime/status` — the voice bridge's preflight report. 503 is
   * the bridge's own "not configured" answer, never a page error. */
  async realtimeStatus(): Promise<RealtimeStatusView> {
    let response: Response;
    try {
      response = await this.request("/v1/realtime/status");
    } catch (error) {
      if (error instanceof KeelApiError && error.status === 503) {
        return { configured: false, models: [] };
      }
      throw error;
    }
    return realtimeStatusOf(asRecord(await response.json()));
  }

  /** `POST /v1/providers/{id}/test` — the measured probe. A provider that ran
   * and failed answers 200 with `ok: false`, so this resolves either way; a
   * 404/501 rejects. The body never carries a credential: only the measured
   * state and the live model ids. */
  async testProvider(providerId: string): Promise<ProviderProbeView> {
    const response = await this.request(`/v1/providers/${encodeURIComponent(providerId)}/test`, {
      method: "POST",
    });
    return providerProbeOf(asRecord(await response.json()));
  }

  /** `GET /v1/settings/secrets` — names, masked previews and times, never
   * values. A row without a preview (one that predates the column) keeps `""`. */
  async secrets(): Promise<SecretsView> {
    const response = await this.request("/v1/settings/secrets");
    const body = asRecord(await response.json());
    const rows = Array.isArray(body.secrets) ? body.secrets : [];
    return {
      secrets: rows.map((row) => {
        const item = asRecord(row);
        return {
          name: String(item.name ?? ""),
          preview: typeof item.preview === "string" && item.preview ? item.preview : undefined,
          createdAt: item.createdAt ? String(item.createdAt) : undefined,
          updatedAt: item.updatedAt ? String(item.updatedAt) : undefined,
        };
      }),
      available: body.available !== false,
      reason: typeof body.reason === "string" && body.reason ? body.reason : undefined,
    };
  }

  /** `PUT /v1/settings/providers/{id}/credential` — the direct-entry path:
   * write the value once, keep the reference. The value is never read back. */
  async saveProviderCredential(providerId: string, value: string): Promise<ProviderCredentialSaved> {
    const response = await this.request(
      `/v1/settings/providers/${encodeURIComponent(providerId)}/credential`,
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ value }),
      },
    );
    const body = asRecord(await response.json());
    return {
      providerId: String(body.providerId ?? providerId),
      secretName: String(body.secretName ?? ""),
      secretRef: String(body.secretRef ?? ""),
      preview: String(body.preview ?? ""),
      updatedAt: body.updatedAt ? String(body.updatedAt) : undefined,
    };
  }

  /** `PUT /v1/settings/secrets/{name}` — write or overwrite a secret. The
   * value is sent once and never read back. */
  async putSecret(name: string, value: string): Promise<SecretSaved> {
    const response = await this.request(`/v1/settings/secrets/${encodeURIComponent(name)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ value }),
    });
    const body = asRecord(await response.json());
    return { name: String(body.name ?? name), updatedAt: body.updatedAt ? String(body.updatedAt) : undefined };
  }

  /** `DELETE /v1/settings/secrets/{name}`. */
  async deleteSecret(name: string): Promise<void> {
    await this.request(`/v1/settings/secrets/${encodeURIComponent(name)}`, { method: "DELETE" });
  }

  // ---- skill library (change keel-harness-v2, W7 skills) ------------------

  /** `GET /v1/settings/skills` — installed skills, sources and the update /
   * conflict state. `check` reads each source (the server's default); false
   * skips it for a fast local read. */
  async skillsSettings(check = true): Promise<SkillsSettingsView> {
    const response = await this.request(`/v1/settings/skills${check ? "" : "?check=0"}`);
    const body = asRecord(await response.json());
    const limits: Record<string, number> = {};
    for (const [key, value] of Object.entries(asRecord(body.limits))) {
      limits[key] = numberOr(value, 0);
    }
    const policyRow = asRecord(body.policy);
    return {
      revision: numberOr(body.revision, 0),
      root: asText(body.root) || undefined,
      checked: body.checked === true,
      skills: listOf(body.skills).map(parseSkillRecord),
      sources: listOf(body.sources).map(parseSkillSource),
      limits,
      policy: {
        trustValues: listOf(policyRow.trustValues).map((item) => asText(item)).filter((item) => item),
        sourceKinds: listOf(policyRow.sourceKinds).map((item) => asText(item)).filter((item) => item),
        maxFiles: typeof policyRow.maxFiles === "number" ? policyRow.maxFiles : undefined,
        maxBytes: typeof policyRow.maxBytes === "number" ? policyRow.maxBytes : undefined,
        maxFileBytes: typeof policyRow.maxFileBytes === "number" ? policyRow.maxFileBytes : undefined,
        scriptExecution: asText(policyRow.scriptExecution) || undefined,
      },
    };
  }

  /** `GET /v1/settings/skills/{name}` — one skill with its body and manifest. */
  async showSkill(name: string): Promise<SkillDetailView> {
    const response = await this.request(`/v1/settings/skills/${encodeURIComponent(name)}`);
    const body = asRecord(await response.json());
    const record = parseSkillRecord(body);
    return {
      ...record,
      body: asText(body.body),
      files: listOf(body.files).map((item) => {
        const file = asRecord(item);
        return {
          path: asText(file.path),
          size: numberOr(file.size, 0),
          hash: asText(file.hash) || undefined,
          mode: typeof file.mode === "number" ? file.mode : undefined,
        };
      }),
    };
  }

  /** `POST /v1/settings/skills` — install a bundle or the convenience
   * description/body form. The server returns the versioned record. */
  async installSkill(request: SkillInstallRequest): Promise<SkillRecordView> {
    const response = await this.request("/v1/settings/skills", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });
    return parseSkillRecord(await response.json());
  }

  /** `POST /v1/settings/skills/{name}/update` — advance to the recorded
   * source's current bundle. A pinned skill answers 409 with code "pinned". */
  async updateSkill(name: string, request: SkillUpdateRequest = {}): Promise<SkillUpdateResult> {
    const response = await this.request(`/v1/settings/skills/${encodeURIComponent(name)}/update`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });
    const body = asRecord(await response.json());
    return {
      status: asText(body.status) || "updated",
      record: body.record ? parseSkillRecord(body.record) : undefined,
      previousHash: asText(body.previousHash) || undefined,
      availableHash: asText(body.availableHash) || undefined,
      pinnedHash: asText(body.pinnedHash) || undefined,
      updateAvailable: body.updateAvailable === true,
    };
  }

  /** `POST /v1/settings/skills/{name}/pin` — activate and hold a version. */
  async pinSkill(name: string, hash?: string): Promise<SkillRecordView> {
    const response = await this.request(`/v1/settings/skills/${encodeURIComponent(name)}/pin`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(hash ? { hash } : {}),
    });
    return parseSkillRecord(await response.json());
  }

  /** `POST /v1/settings/skills/{name}/unpin` — release the hold. */
  async unpinSkill(name: string): Promise<SkillRecordView> {
    const response = await this.request(`/v1/settings/skills/${encodeURIComponent(name)}/unpin`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    return parseSkillRecord(await response.json());
  }

  /** `DELETE /v1/settings/skills/{name}`. */
  async removeSkill(name: string): Promise<void> {
    await this.request(`/v1/settings/skills/${encodeURIComponent(name)}`, { method: "DELETE" });
  }

  /** `POST /v1/settings/skills/sources` — subscribe a local source. */
  async subscribeSkillSource(source: {
    id: string;
    kind?: string;
    url: string;
    trust?: string;
  }): Promise<SkillSourceView> {
    const response = await this.request("/v1/settings/skills/sources", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(source),
    });
    return parseSkillSource(await response.json());
  }

  /** `DELETE /v1/settings/skills/sources/{id}`. */
  async unsubscribeSkillSource(id: string): Promise<void> {
    await this.request(`/v1/settings/skills/sources/${encodeURIComponent(id)}`, { method: "DELETE" });
  }

  /** `GET /v1/sessions/{id}/files` — the `$` file group's source and the task
   * panel's manifest read. A path-cursor page: `cursor` is the previous page's
   * `nextCursor`. Optional in the Client seam; the live transport always serves
   * it. */
  async sessionFiles(sessionId: string, query: SessionFilesQuery = {}): Promise<SessionFilesView> {
    const params = new URLSearchParams();
    if (query.limit !== undefined) params.set("limit", String(query.limit));
    if (query.cursor) params.set("cursor", query.cursor);
    const suffix = params.toString();
    const response = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/files${suffix ? `?${suffix}` : ""}`,
    );
    const body = asRecord(await response.json());
    return {
      files: listOf(body.files).map((item) => {
        const file = asRecord(item);
        return {
          path: asText(file.path),
          hash: asText(file.hash) || undefined,
          size: numberOr(file.size, 0),
          updatedSeq: numberOr(file.updatedSeq, 0),
          lastTaskId: asText(file.lastTaskId) || undefined,
        };
      }),
      count: numberOr(body.count, 0),
      limit: numberOr(body.limit, 0) || undefined,
      truncated: body.truncated === true,
      hasMore: body.hasMore === true,
      nextCursor: asText(body.nextCursor) || undefined,
    };
  }

  /** `GET /v1/sessions/search` — bounded transcript/session search. The
   * response keeps the server's scope: a caller that asked for messages never
   * receives a fabricated sessions list. */
  async sessionSearch(query: string, opts: SessionSearchOptions = {}): Promise<SessionSearchView> {
    const params = new URLSearchParams({ q: query });
    params.set("scope", opts.scope ?? "messages");
    if (opts.sessionId) params.set("sessionId", opts.sessionId);
    if (opts.kind) params.set("kind", opts.kind);
    if (opts.limit !== undefined) params.set("limit", String(opts.limit));
    if (opts.after) params.set("after", opts.after);
    const response = await this.request(`/v1/sessions/search?${params.toString()}`);
    const body = asRecord(await response.json());
    const scope = body.scope === "sessions" ? "sessions" : "messages";
    if (scope === "sessions") {
      return {
        scope,
        query: asText(body.query) || undefined,
        sessions: listOf(body.sessions).map((item) => {
          const row = asRecord(item);
          return {
            sessionId: asText(row.sessionId),
            title: asText(row.title),
            createdAt: asText(row.createdAt) || undefined,
            updatedAt: asText(row.updatedAt) || undefined,
            lastMessageAt: asText(row.lastMessageAt) || undefined,
            snippet: asText(row.snippet),
          };
        }),
        count: numberOr(body.count, 0),
        limit: numberOr(body.limit, 0),
        nextCursor: asText(body.nextCursor) || undefined,
        hasMore: body.hasMore === true,
      };
    }
    return {
      scope,
      query: asText(body.query) || undefined,
      messages: listOf(body.messages).map((item) => {
        const row = asRecord(item);
        return {
          sessionId: asText(row.sessionId),
          sessionTitle: asText(row.sessionTitle),
          seq: numberOr(row.seq, 0),
          kind: asText(row.kind),
          role: asText(row.role),
          at: asText(row.at) || undefined,
          snippet: asText(row.snippet),
        };
      }),
      count: numberOr(body.count, 0),
      limit: numberOr(body.limit, 0),
      nextCursor: asText(body.nextCursor) || undefined,
      hasMore: body.hasMore === true,
    };
  }

  /** `GET /v1/sessions/{id}/export` — one bounded transcript page. */
  async sessionExport(sessionId: string, opts: SessionExportOptions = {}): Promise<SessionExportView> {
    const params = new URLSearchParams();
    if (opts.afterSeq !== undefined) params.set("afterSeq", String(opts.afterSeq));
    if (opts.limit !== undefined) params.set("limit", String(opts.limit));
    const suffix = params.toString();
    const response = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/export${suffix ? `?${suffix}` : ""}`,
    );
    const body = asRecord(await response.json());
    return {
      sessionId: asText(body.sessionId),
      title: asText(body.title) || undefined,
      format: asText(body.format) || undefined,
      messages: listOf(body.messages).map((item) => {
        const row = asRecord(item);
        return {
          seq: numberOr(row.seq, 0),
          kind: asText(row.kind),
          role: asText(row.role),
          at: asText(row.at) || undefined,
          text: asText(row.text),
          textTruncated: row.textTruncated === true,
        };
      }),
      count: numberOr(body.count, 0),
      limit: numberOr(body.limit, 0),
      afterSeq: numberOr(body.afterSeq, 0),
      nextAfterSeq: numberOr(body.nextAfterSeq, 0),
      hasMore: body.hasMore === true,
      truncated: body.truncated === true,
      limits: typeof body.limits === "object" && body.limits !== null ? (body.limits as Record<string, number>) : undefined,
    };
  }

  /** `GET /v1/sessions/{id}/deliverables` — the manifest-backed workspace
   * changes, one bounded page. */
  async sessionDeliverables(sessionId: string, opts: DeliverablesOptions = {}): Promise<DeliverablesView> {
    const params = new URLSearchParams();
    if (opts.limit !== undefined) params.set("limit", String(opts.limit));
    if (opts.after) params.set("after", opts.after);
    const suffix = params.toString();
    const response = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/deliverables${suffix ? `?${suffix}` : ""}`,
    );
    const body = asRecord(await response.json());
    return {
      sessionId: asText(body.sessionId) || undefined,
      deliverables: listOf(body.deliverables).map((item) => {
        const row = asRecord(item);
        return {
          path: asText(row.path),
          hash: asText(row.hash),
          size: numberOr(row.size, 0),
          mode: typeof row.mode === "number" ? row.mode : undefined,
          updatedSeq: numberOr(row.updatedSeq, 0),
          lastTaskId: asText(row.lastTaskId) || undefined,
          previewUrl: asText(row.previewUrl),
        };
      }),
      count: numberOr(body.count, 0),
      limit: numberOr(body.limit, 0),
      nextCursor: asText(body.nextCursor) || undefined,
      hasMore: body.hasMore === true,
      limits: typeof body.limits === "object" && body.limits !== null ? (body.limits as Record<string, number>) : undefined,
    };
  }

  /** `GET /v1/sessions/{id}/tasks` — the `$` task group's source. */
  async sessionTasks(sessionId: string): Promise<SessionTasksView> {
    const response = await this.request(`/v1/sessions/${encodeURIComponent(sessionId)}/tasks`);
    const body = asRecord(await response.json());
    return {
      tasks: listOf(body.tasks).map((item) => {
        const task = asRecord(item);
        return {
          taskId: asText(task.taskId),
          parentTaskId: asText(task.parentTaskId) || undefined,
          title: asText(task.title),
          status: asText(task.status),
          updatedAt: asText(task.updatedAt) || undefined,
        };
      }),
      count: numberOr(body.count, 0),
      truncated: body.truncated === true,
    };
  }

  /** `GET /v1/sessions/{id}/task-panel` — the run surface's task tree, event
   * stream and resource references. A field the projection did not send stays
   * absent; the panel never fills one in. */
  async taskPanel(sessionId: string, memberLimit?: number, eventLimit?: number): Promise<TaskPanelView> {
    const query = new URLSearchParams();
    if (memberLimit !== undefined) query.set("memberLimit", String(memberLimit));
    if (eventLimit !== undefined) query.set("eventLimit", String(eventLimit));
    const suffix = query.toString();
    const response = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/task-panel${suffix ? `?${suffix}` : ""}`,
    );
    const body = asRecord(await response.json());
    const card = (value: unknown): TaskPanelCardView => {
      const row = asRecord(value);
      const parsed: TaskPanelCardView = {
        taskId: asText(row.taskId),
        sessionId: asText(row.sessionId),
        title: asText(row.title),
        status: asText(row.status),
        parentTaskId: asText(row.parentTaskId) || undefined,
        origin: asText(row.origin) || undefined,
        originSeq: numberOr(row.originSeq, 0) || undefined,
        createdSeq: numberOr(row.createdSeq, 0) || undefined,
        updatedSeq: numberOr(row.updatedSeq, 0) || undefined,
        closedSeq: numberOr(row.closedSeq, 0) || undefined,
        createdAt: asText(row.createdAt) || undefined,
        updatedAt: asText(row.updatedAt) || undefined,
      };
      if (row.member === true) parsed.member = true;
      return parsed;
    };
    const event = (value: unknown): TaskPanelEventView => {
      const row = asRecord(value);
      const parsed: TaskPanelEventView = {
        taskId: asText(row.taskId),
        kind: asText(row.kind),
        seq: numberOr(row.seq, 0),
        at: asText(row.at) || undefined,
        title: asText(row.title) || undefined,
        status: asText(row.status) || undefined,
        parentTaskId: asText(row.parentTaskId) || undefined,
        origin: asText(row.origin) || undefined,
        originSeq: numberOr(row.originSeq, 0) || undefined,
        path: asText(row.path) || undefined,
        hash: asText(row.hash) || undefined,
        text: asText(row.text) || undefined,
        reason: asText(row.reason) || undefined,
      };
      if (row.unparsed === true) parsed.unparsed = true;
      return parsed;
    };
    const limits = asRecord(body.limits);
    return {
      sessionId: asText(body.sessionId),
      cards: listOf(body.cards).map(card),
      members: listOf(body.members).map(card),
      ancestors: listOf(body.ancestors).map(card),
      attachments: listOf(body.attachments).flatMap((value): TaskPanelAttachmentView[] => {
        const row = asRecord(value);
        const path = asText(row.path);
        if (!path) return [];
        return [{ taskId: asText(row.taskId), path, hash: asText(row.hash) }];
      }),
      events: listOf(body.events).map(event),
      limits: {
        cardLimit: numberOr(limits.cardLimit, 0) || undefined,
        cardsTruncated: limits.cardsTruncated === true,
        memberLimit: numberOr(limits.memberLimit, 0),
        truncated: limits.truncated === true,
        truncatedMemberCount: numberOr(limits.truncatedMemberCount, 0) || undefined,
        eventLimit: numberOr(limits.eventLimit, 0),
        eventsTruncated: limits.eventsTruncated === true,
        attachmentLimit: numberOr(limits.attachmentLimit, 0) || undefined,
        attachmentsTruncated: limits.attachmentsTruncated === true,
        ancestorsTruncated: limits.ancestorsTruncated === true,
      },
    };
  }

  /** `GET /v1/sessions/{id}/file-preview` — one bounded page of a path the
   * session's manifest holds. The path is passed exactly as the event recorded
   * it; no path is invented. */
  async filePreview(sessionId: string, path: string, referenceHash?: string, offset = 0): Promise<FilePreviewView> {
    const query = new URLSearchParams({ path });
    if (referenceHash) query.set("hash", referenceHash);
    if (offset > 0) query.set("offset", String(offset));
    const response = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/file-preview?${query.toString()}`,
    );
    const body = asRecord(await response.json());
    return {
      path: asText(body.path),
      hash: asText(body.hash),
      size: numberOr(body.size, 0),
      seq: numberOr(body.seq, 0) || undefined,
      offset: numberOr(body.offset, 0),
      limit: numberOr(body.limit, 0),
      totalLines: numberOr(body.totalLines, 0),
      returnedLines: numberOr(body.returnedLines, 0),
      nextOffset: numberOr(body.nextOffset, 0),
      truncated: body.truncated === true,
      content: asText(body.content),
      referenceHash: asText(body.referenceHash) || undefined,
      matchesReference:
        typeof body.matchesReference === "boolean" ? body.matchesReference : undefined,
    };
  }

  /** `GET /v1/plugins` — the assembled extension surface. */
  async plugins(): Promise<PluginsView> {
    const response = await this.request("/v1/plugins");
    return (await response.json()) as PluginsView;
  }

  /** `GET /v1/triggers` — the trigger engine's registry state. */
  async triggers(): Promise<TriggersView> {
    const response = await this.request("/v1/triggers");
    const body = asRecord(await response.json());
    return { triggers: Array.isArray(body.triggers) ? (body.triggers as TriggersView["triggers"]) : [] };
  }

  /** `POST /v1/triggers/{id}/fire` — a manual fire; the audit result or the
   * server's refusal comes back verbatim. */
  async fireTrigger(id: string, reason = "manual"): Promise<FireResultView> {
    const response = await this.request(`/v1/triggers/${encodeURIComponent(id)}/fire`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ reason }),
    });
    const body = asRecord(await response.json());
    return asRecord(body.result).triggerId ? (asRecord(body.result) as FireResultView) : (body as FireResultView);
  }

  // ---- identity & access (A4.3) -----------------------------------------
  //
  // The server's authority, exposed one method per route. Every listing is
  // bounded (`limit`, 1..500); `hasMore` says a full page came back, not that
  // another page exists. A refusal keeps its status, code and — for a 403 —
  // the required key and reason (KeelApiError).

  private async readAccountList(path: string, limit: number): Promise<IdentityPage<AccountView>> {
    const response = await this.request(path);
    const body = asRecord(await response.json());
    return identityPage<AccountView>(listOf(body.accounts).map(parseAccount), identityLimitOf(limit));
  }

  async listAccounts(limit = 100): Promise<IdentityPage<AccountView>> {
    const bounded = identityLimitOf(limit);
    return this.readAccountList(`/v1/accounts?limit=${bounded}`, bounded);
  }

  async createAccount(input: CreateAccountInput): Promise<AccountView> {
    const response = await this.request("/v1/accounts", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: input.username,
        ...(input.displayName ? { displayName: input.displayName } : {}),
        ...(input.kind ? { kind: input.kind } : {}),
        ...(input.password ? { password: input.password } : {}),
      }),
    });
    return parseAccount(asRecord(await response.json()).account);
  }

  async getAccount(accountId: string): Promise<{ account: AccountView; groups: PermissionGroupSummary[] }> {
    const response = await this.request(`/v1/accounts/${encodeURIComponent(accountId)}`);
    const body = asRecord(await response.json());
    return {
      account: parseAccount(body.account),
      groups: listOf(body.groups).map(parsePermissionGroup),
    };
  }

  async setAccountDisabled(accountId: string, disabled: boolean, reason?: string): Promise<AccountView> {
    const response = await this.request(`/v1/accounts/${encodeURIComponent(accountId)}/disable`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ disabled, ...(reason ? { reason } : {}) }),
    });
    return parseAccount(asRecord(await response.json()).account);
  }

  async resetAccountPassword(accountId: string, password: string): Promise<void> {
    await this.request(`/v1/accounts/${encodeURIComponent(accountId)}/password-reset`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password }),
    });
  }

  async listAccountGroups(accountId: string): Promise<PermissionGroupSummary[]> {
    const response = await this.request(`/v1/accounts/${encodeURIComponent(accountId)}/groups`);
    return listOf(asRecord(await response.json()).groups).map(parsePermissionGroup);
  }

  async addAccountGroup(accountId: string, groupId: string): Promise<void> {
    await this.request(`/v1/accounts/${encodeURIComponent(accountId)}/groups`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ groupId }),
    });
  }

  async removeAccountGroup(accountId: string, groupId: string): Promise<void> {
    await this.request(
      `/v1/accounts/${encodeURIComponent(accountId)}/groups/${encodeURIComponent(groupId)}`,
      { method: "DELETE" },
    );
  }

  async listPermissionGroups(limit = 100): Promise<IdentityPage<PermissionGroupSummary>> {
    const bounded = identityLimitOf(limit);
    const response = await this.request(`/v1/permission-groups?limit=${bounded}`);
    const body = asRecord(await response.json());
    return identityPage<PermissionGroupSummary>(listOf(body.groups).map(parsePermissionGroup), bounded);
  }

  async getPermissionGroup(groupId: string): Promise<PermissionGroupDetail> {
    const response = await this.request(`/v1/permission-groups/${encodeURIComponent(groupId)}`);
    return parsePermissionGroupDetail(await response.json());
  }

  async createPermissionGroup(name: string): Promise<PermissionGroupSummary> {
    const response = await this.request("/v1/permission-groups", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
    return parsePermissionGroup(asRecord(await response.json()).group);
  }

  async renamePermissionGroup(groupId: string, name: string): Promise<PermissionGroupSummary> {
    const response = await this.request(`/v1/permission-groups/${encodeURIComponent(groupId)}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
    return parsePermissionGroup(asRecord(await response.json()).group);
  }

  async deletePermissionGroup(groupId: string): Promise<void> {
    await this.request(`/v1/permission-groups/${encodeURIComponent(groupId)}`, { method: "DELETE" });
  }

  async addPermissionGroupMember(groupId: string, accountId: string): Promise<void> {
    await this.request(`/v1/permission-groups/${encodeURIComponent(groupId)}/members`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ accountId }),
    });
  }

  async removePermissionGroupMember(groupId: string, accountId: string): Promise<void> {
    await this.request(
      `/v1/permission-groups/${encodeURIComponent(groupId)}/members/${encodeURIComponent(accountId)}`,
      { method: "DELETE" },
    );
  }

  async addPermissionGroupGrant(groupId: string, key: string, scope: string): Promise<void> {
    await this.request(`/v1/permission-groups/${encodeURIComponent(groupId)}/grants`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ key, scope }),
    });
  }

  async removePermissionGroupGrant(groupId: string, key: string, scope: string): Promise<void> {
    const query = new URLSearchParams({ scope });
    await this.request(
      `/v1/permission-groups/${encodeURIComponent(groupId)}/grants/${encodeURIComponent(key)}?${query.toString()}`,
      { method: "DELETE" },
    );
  }

  async listBots(limit = 100, includeArchived = false): Promise<IdentityPage<BotView>> {
    const bounded = identityLimitOf(limit);
    const query = new URLSearchParams({ limit: String(bounded) });
    if (includeArchived) query.set("includeArchived", "1");
    const response = await this.request(`/v1/bots?${query.toString()}`);
    const body = asRecord(await response.json());
    return identityPage<BotView>(listOf(body.bots).map(parseBot), bounded);
  }

  async getBot(botId: string): Promise<BotView> {
    const response = await this.request(`/v1/bots/${encodeURIComponent(botId)}`);
    return parseBot(asRecord(await response.json()).bot);
  }

  async createBot(input: CreateBotInput): Promise<BotView> {
    const response = await this.request("/v1/bots", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        ...(input.botId ? { botId: input.botId } : {}),
        name: input.name,
        profileId: input.profileId,
        modelKind: input.modelKind,
        ...(input.defaultModel ? { defaultModel: input.defaultModel } : {}),
        ...(input.harness ? { harness: input.harness } : {}),
      }),
    });
    return parseBot(asRecord(await response.json()).bot);
  }

  async updateBot(botId: string, input: UpdateBotInput): Promise<BotView> {
    const response = await this.request(`/v1/bots/${encodeURIComponent(botId)}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        ...(input.name !== undefined ? { name: input.name } : {}),
        ...(input.profileId !== undefined ? { profileId: input.profileId } : {}),
        ...(input.modelKind !== undefined ? { modelKind: input.modelKind } : {}),
        ...(input.defaultModel !== undefined ? { defaultModel: input.defaultModel } : {}),
        ...(input.harness !== undefined ? { harness: input.harness } : {}),
        ...(input.expectedArchived !== undefined ? { expectedArchived: input.expectedArchived } : {}),
      }),
    });
    return parseBot(asRecord(await response.json()).bot);
  }

  async archiveBot(botId: string): Promise<void> {
    await this.request(`/v1/bots/${encodeURIComponent(botId)}/archive`, { method: "POST" });
  }

  /** Invoke a Bot: the server checks `bot.invoke` on that Bot; the Bot then
   * runs with its own Account ∩ Harness permissions (not the caller's). */
  async invokeBot(
    botId: string,
    text: string,
    sessionId?: string,
    requestId?: string,
  ): Promise<{ botId: string; sessionId: string; seq?: number; requestId?: string; deduped?: boolean }> {
    const response = await this.request(`/v1/bots/${encodeURIComponent(botId)}/invoke`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        text,
        ...(sessionId ? { sessionId } : {}),
        ...(requestId ? { requestId } : {}),
      }),
    });
    const body = asRecord(await response.json());
    return {
      botId: String(body.botId ?? botId),
      sessionId: String(body.sessionId ?? ""),
      seq: typeof body.seq === "number" ? body.seq : undefined,
      requestId: typeof body.requestId === "string" ? body.requestId : undefined,
      deduped: body.deduped === true,
    };
  }

  async listBotKeys(botId: string): Promise<BotKeyView[]> {
    const response = await this.request(`/v1/bots/${encodeURIComponent(botId)}/keys`);
    return listOf(asRecord(await response.json()).keys).map(parseBotKey);
  }

  /** Issue a bot credential. The plaintext appears in this answer only; the
   * caller must show it once and never keep it. */
  async issueBotKey(
    botId: string,
    label = "",
    accountId?: string,
  ): Promise<{ key: BotKeyView; plaintext: string; notice: string }> {
    const response = await this.request(`/v1/bots/${encodeURIComponent(botId)}/keys`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ label, ...(accountId ? { accountId } : {}) }),
    });
    const body = asRecord(await response.json());
    return {
      key: parseBotKey(body.key),
      plaintext: String(body.plaintext ?? ""),
      notice: String(body.notice ?? ""),
    };
  }

  async revokeBotKey(botId: string, keyId: string): Promise<void> {
    await this.request(
      `/v1/bots/${encodeURIComponent(botId)}/keys/${encodeURIComponent(keyId)}`,
      { method: "DELETE" },
    );
  }

  async listTeamBots(limit = 100, includeArchived = false): Promise<IdentityPage<TeamBotView>> {
    const bounded = identityLimitOf(limit);
    const query = new URLSearchParams({ limit: String(bounded) });
    if (includeArchived) query.set("includeArchived", "1");
    const response = await this.request(`/v1/team-bots?${query.toString()}`);
    const body = asRecord(await response.json());
    return identityPage<TeamBotView>(listOf(body.teamBots).map(parseTeamBot), bounded);
  }

  async getTeamBot(teamBotId: string): Promise<TeamBotView> {
    const response = await this.request(`/v1/team-bots/${encodeURIComponent(teamBotId)}`);
    return parseTeamBot(asRecord(await response.json()).teamBot);
  }

  async createTeamBot(input: CreateTeamBotInput): Promise<TeamBotView> {
    const response = await this.request("/v1/team-bots", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        ...(input.teamBotId ? { teamBotId: input.teamBotId } : {}),
        name: input.name,
        mission: input.mission,
        ...(input.roster ? { roster: input.roster } : {}),
      }),
    });
    return parseTeamBot(asRecord(await response.json()).teamBot);
  }

  async updateTeamBot(teamBotId: string, input: UpdateTeamBotInput): Promise<TeamBotView> {
    const response = await this.request(`/v1/team-bots/${encodeURIComponent(teamBotId)}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        ...(input.name !== undefined ? { name: input.name } : {}),
        ...(input.mission !== undefined ? { mission: input.mission } : {}),
        ...(input.roster !== undefined ? { roster: input.roster } : {}),
        expectedRevision: input.expectedRevision,
      }),
    });
    return parseTeamBot(asRecord(await response.json()).teamBot);
  }

  /** Replace the roster (CAS on `expectedRevision`); every seat re-checks the
   * caller's `bot.invoke` server-side. */
  async updateTeamBotRoster(
    teamBotId: string,
    roster: UpdateTeamBotInput["roster"],
    expectedRevision: number,
  ): Promise<TeamBotView> {
    const response = await this.request(`/v1/team-bots/${encodeURIComponent(teamBotId)}/roster`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ roster: roster ?? [], expectedRevision }),
    });
    return parseTeamBot(asRecord(await response.json()).teamBot);
  }

  async archiveTeamBot(teamBotId: string): Promise<void> {
    await this.request(`/v1/team-bots/${encodeURIComponent(teamBotId)}/archive`, { method: "POST" });
  }

  async startTeamBot(
    teamBotId: string,
    text?: string,
    sessionId?: string,
    requestId?: string,
  ): Promise<{ teamBotId: string; captainBotId: string; sessionId: string; seq?: number; requestId?: string; deduped?: boolean }> {
    const response = await this.request(`/v1/team-bots/${encodeURIComponent(teamBotId)}/start`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        ...(text ? { text } : {}),
        ...(sessionId ? { sessionId } : {}),
        ...(requestId ? { requestId } : {}),
      }),
    });
    const body = asRecord(await response.json());
    return {
      teamBotId: String(body.teamBotId ?? teamBotId),
      captainBotId: String(body.captainBotId ?? ""),
      sessionId: String(body.sessionId ?? ""),
      seq: typeof body.seq === "number" ? body.seq : undefined,
      requestId: typeof body.requestId === "string" ? body.requestId : undefined,
      deduped: body.deduped === true,
    };
  }

  // ---- resource sharing (A6) ---------------------------------------------

  async listShares(kind: ShareResourceKind, resourceId: string): Promise<ShareView[]> {
    const response = await this.request(sharePath(kind, resourceId));
    const body = asRecord(await response.json());
    return listOf(body.shares).flatMap((item) => {
      const share = parseShareView(item);
      return share ? [share] : [];
    });
  }

  async addShare(kind: ShareResourceKind, resourceId: string, input: ShareInput): Promise<ShareWriteResult> {
    const response = await this.request(sharePath(kind, resourceId), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(input),
    });
    return shareWriteOf(await response.json());
  }

  /** Revoke is a POST with the tuple as its body (no DELETE body). */
  async revokeShare(kind: ShareResourceKind, resourceId: string, input: ShareInput): Promise<ShareWriteResult> {
    const response = await this.request(`${sharePath(kind, resourceId)}/revoke`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(input),
    });
    return shareWriteOf(await response.json());
  }

  // ---- profiles & roles (the Bot page's identity facts) -------------------

  /** `GET /v1/profiles` — a Profile is (Account + Role); the Bot page reads it
   * to name the Bot's owner and persona. A caller without `profile.read` gets
   * the server's 403 and the page falls back to the Bot's own accountId. */
  async listProfiles(limit = 100): Promise<IdentityPage<ProfileView>> {
    const bounded = identityLimitOf(limit);
    const response = await this.request(`/v1/profiles?limit=${bounded}`);
    const body = asRecord(await response.json());
    return identityPage<ProfileView>(listOf(body.profiles).map(parseProfile), bounded);
  }

  async getProfile(profileId: string): Promise<ProfileView> {
    const response = await this.request(`/v1/profiles/${encodeURIComponent(profileId)}`);
    return parseProfile(asRecord(await response.json()).profile);
  }

  /** `GET /v1/roles` — the persona documents a Profile points at. A Role never
   * grants authority; the page only shows its name/instruction. */
  async listRoles(limit = 100): Promise<IdentityPage<RoleView>> {
    const bounded = identityLimitOf(limit);
    const response = await this.request(`/v1/roles?limit=${bounded}`);
    const body = asRecord(await response.json());
    return identityPage<RoleView>(listOf(body.roles).map(parseRole), bounded);
  }

  // ---- the run projection's reads -----------------------------------------

  /** One session's ledger events. The run page reads them for tool results,
   * usage and the final stop reason; the events are the server's, never a
   * reconstruction. */
  async sessionEvents(sessionId: string, limit = 500): Promise<SessionEventView[]> {
    const bounded = Math.min(1000, Math.max(1, Math.floor(limit)));
    const response = await this.request(
      `/v1/sessions/${encodeURIComponent(sessionId)}/events?after=0&limit=${bounded}`,
    );
    const body = asRecord(await response.json());
    return listOf(body.events)
      .flatMap((row) => {
        const event = parseSessionEvent(row);
        return event ? [event] : [];
      })
      .sort((left, right) => left.seq - right.seq);
  }

  /** `GET /v1/subagents` — the spawned/forked child runs, optionally under one
   * parent session. The list is relation-filtered by the server; a child the
   * caller cannot read is not returned. */
  async listSubagents(parentSessionId?: string, limit = 200): Promise<SubagentView[]> {
    // The server bounds this read to 1..200.
    const bounded = Math.min(200, Math.max(1, Math.floor(limit)));
    const query = new URLSearchParams({ limit: String(bounded) });
    if (parentSessionId) query.set("parentSessionId", parentSessionId);
    const response = await this.request(`/v1/subagents?${query.toString()}`);
    const body = asRecord(await response.json());
    return listOf(body.subagents).flatMap((row) => {
      const record = parseSubagent(row);
      return record ? [record] : [];
    });
  }

  /** `POST /v1/sessions/{id}/cancel` — the run page's stop. A deployment whose
   * cancel seam is not wired answers 501; the page prints that instead of
   * pretending the run stopped. */
  async cancelSession(sessionId: string): Promise<SessionCancelView> {
    const response = await this.request(`/v1/sessions/${encodeURIComponent(sessionId)}/cancel`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    return parseSessionCancel(await response.json());
  }

  /** `GET /v1/audit/events` — the audit ledger's read projection. The route is
   * the intended contract (A4.4a draws the page; the server slice lands with
   * it); until a deployment serves it the request answers the default-deny 404
   * and the caller renders its 待接入 state. No row is ever synthesized here.
   * `since`/`kind`/`result` narrow the ledger; `limit` is bounded like every
   * other identity listing. */
  async listAuditEvents(query: AuditQuery = {}): Promise<IdentityPage<AuditEventView>> {
    const bounded = identityLimitOf(query.limit ?? 100);
    const params = new URLSearchParams({ limit: String(bounded) });
    if (query.since) params.set("since", query.since);
    if (query.kind) params.set("kind", query.kind);
    if (query.result) params.set("result", query.result);
    const response = await this.request(`/v1/audit/events?${params.toString()}`);
    const body = asRecord(await response.json());
    return identityPage<AuditEventView>(
      listOf(body.events).flatMap((item) => {
        const event = parseAuditEvent(item);
        return event ? [event] : [];
      }),
      bounded,
    );
  }
}

/** The API's listing bound: 1..500, default 100 (server defaults agree). */
const identityLimitOf = (limit: number): number => {
  if (!Number.isFinite(limit)) return 100;
  return Math.min(500, Math.max(1, Math.floor(limit)));
};

export class HttpEventSource implements EventSource {
  private readonly http: KeelHttp;
  readonly sessionId: string;

  constructor(http: KeelHttp, sessionId: string) {
    this.http = http;
    this.sessionId = sessionId;
  }

  async list(after: number, limit = 500): Promise<WireEvent[]> {
    const response = await this.http.request(
      `/v1/sessions/${encodeURIComponent(this.sessionId)}/events?after=${after}&limit=${limit}`,
    );
    const body = (await response.json()) as { events?: unknown[] };
    return (body.events ?? [])
      .map((row) => {
        const event = row as { seq?: number; kind?: string; payload?: unknown };
        return {
          seq: Number(event.seq ?? 0),
          kind: String(event.kind ?? "message"),
          payload: event.payload,
        };
      })
      .filter((event) => Number.isFinite(event.seq) && event.seq > 0)
      .sort((a, b) => a.seq - b.seq);
  }

  stream(opts: {
    after: number;
    partials?: boolean;
    onEvent: (e: WireEvent) => void;
    onPartial?: (p: { text: string; turnId?: string }) => void;
    onState: (s: ConnectionState["status"]) => void;
  }): () => void {
    let stopped = false;
    let cursor = opts.after;
    let attempt = 0;
    // A 404 from the stream route is not proof that the session is gone: the
    // session may have been committed a moment ago, and a proxy in front of the
    // server can answer before the row is visible. The request is an idempotent
    // GET, so a bounded number of retries is safe — and it is what keeps a
    // transient miss from terminating the live channel for the rest of the
    // session (a "closed" stream never re-subscribes).
    let notFoundRetries = 0;
    const maxNotFoundRetries = 3;
    let controller: AbortController | null = null;
    let retryTimer: number | undefined;

    const schedule = () => {
      if (stopped) return;
      const delay = Math.min(8000, 400 * Math.pow(1.7, attempt));
      attempt += 1;
      opts.onState("reconnecting");
      retryTimer = setTimeout(connect, delay) as unknown as number;
    };

    const connect = async () => {
      if (stopped) return;
      controller = new AbortController();
      const query = new URLSearchParams({ after: String(cursor) });
      if (opts.partials) query.set("partials", "1");
      const headers: Record<string, string> = { Accept: "text/event-stream" };
      if (cursor > 0) headers["Last-Event-ID"] = String(cursor);
      opts.onState(attempt === 0 ? "connecting" : "reconnecting");
      try {
        // The injected transport, like every other call: a test (or a host with
        // its own fetch) must be able to drive the stream too, and reading the
        // global here silently bypassed it.
        const response = await this.http.transport()(
          this.http.url(`/v1/sessions/${encodeURIComponent(this.sessionId)}/events/stream?${query.toString()}`),
          { headers, signal: controller.signal, credentials: "same-origin" },
        );
        if (!response.ok) {
          const message = await readError(response);
          if (response.status === 404 && notFoundRetries < maxNotFoundRetries) {
            notFoundRetries += 1;
            schedule();
            return;
          }
          if (!retryable(response.status)) {
            stopped = true;
            opts.onState("closed");
            throw new KeelApiError(response.status, message);
          }
          schedule();
          return;
        }
        notFoundRetries = 0;
        if (!response.body) {
          schedule();
          return;
        }
        attempt = 0;
        opts.onState("open");
        await this.pump(response.body, {
          onEvent: (event) => {
            if (event.seq > cursor) cursor = event.seq;
            opts.onEvent(event);
          },
          onPartial: opts.onPartial,
          onState: opts.onState,
        });
        if (stopped) return;
        // A normal end (a drain frame, a proxy closing): resume from the
        // cursor; the server replays what we missed.
        schedule();
      } catch (error) {
        if (stopped) return;
        if (error instanceof KeelApiError && !retryable(error.status)) {
          stopped = true;
          opts.onState("closed");
          return;
        }
        schedule();
      }
    };

    void connect();
    return () => {
      stopped = true;
      controller?.abort();
      if (retryTimer !== undefined) clearTimeout(retryTimer);
      opts.onState("closed");
    };
  }

  /** Reads an SSE body, splitting frames on blank lines. */
  private async pump(
    body: ReadableStream<Uint8Array>,
    handlers: {
      onEvent: (event: WireEvent) => void;
      onPartial?: (p: { text: string; turnId?: string }) => void;
      onState: (s: ConnectionState["status"]) => void;
    },
  ): Promise<void> {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        buffer = buffer.replace(/\r\n/g, "\n");
        let boundary = buffer.indexOf("\n\n");
        while (boundary >= 0) {
          const frame = buffer.slice(0, boundary);
          buffer = buffer.slice(boundary + 2);
          this.frame(frame, handlers);
          boundary = buffer.indexOf("\n\n");
        }
      }
      // A final frame without its blank line (a proxy cut the stream).
      if (buffer.trim()) this.frame(buffer, handlers);
    } finally {
      try {
        reader.releaseLock();
      } catch {
        /* already released */
      }
    }
  }

  private frame(
    text: string,
    handlers: {
      onEvent: (event: WireEvent) => void;
      onPartial?: (p: { text: string; turnId?: string }) => void;
    },
  ) {
    let id = "";
    let name = "message";
    const data: string[] = [];
    for (const line of text.split("\n")) {
      if (!line || line.startsWith(":")) continue;
      const colon = line.indexOf(":");
      const field = colon >= 0 ? line.slice(0, colon) : line;
      const value = colon >= 0 ? line.slice(colon + 1).replace(/^ /, "") : "";
      if (field === "id") id = value;
      else if (field === "event") name = value;
      else if (field === "data") data.push(value);
    }
    if (!data.length) return;
    const raw = data.join("\n");
    if (name === "partial" || name === "partials" || name === "assistant.partial") {
      let parsed: unknown = raw;
      try {
        parsed = JSON.parse(raw);
      } catch {
        /* plain text */
      }
      if (typeof parsed === "string") {
        handlers.onPartial?.({ text: parsed });
      } else if (parsed && typeof parsed === "object") {
        const payload = parsed as { text?: string; delta?: string; turnId?: string; turn_id?: string };
        handlers.onPartial?.({
          text: payload.text ?? payload.delta ?? "",
          turnId: payload.turnId ?? payload.turn_id,
        });
      }
      return;
    }
    let payload: unknown = raw;
    try {
      payload = JSON.parse(raw);
    } catch {
      /* keep the raw string */
    }
    const seq = Number(id);
    if (!Number.isFinite(seq) || seq <= 0) return;
    handlers.onEvent({ seq, kind: name, payload });
  }
}

export class HttpCommands implements UiCommands {
  private readonly http: KeelHttp;
  readonly sessionId: string;

  constructor(http: KeelHttp, sessionId: string) {
    this.http = http;
    this.sessionId = sessionId;
  }

  async send(text: string, opts: SendOptions): Promise<void> {
    await this.sendWithAttachments(text, opts);
  }

  async sendWithAttachments(text: string, opts: SendOptions): Promise<void> {
    const response = await this.http.request(`/v1/sessions/${encodeURIComponent(this.sessionId)}/input`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        text,
        requestId: opts.requestId,
        delivery: opts.delivery,
        ...(opts.attachments?.length ? { attachments: opts.attachments } : {}),
        // The session's model selection travels as the input's own fact; an
        // absent selection means "the tenant default" (the server decides).
        ...(opts.selection ? { selection: opts.selection } : {}),
      }),
    });
    if (!opts.onQueued) return;
    // A queued admission is a 202 with a `queued` block; report it so the
    // session row says 排队中 (with position/ETA) instead of "ready". A body
    // without the block (200/201) simply carries no queue fact.
    try {
      const queued = parseQueueFacts(await response.json());
      if (!queued) return;
      opts.onQueued(queued);
    } catch {
      /* an empty or non-JSON body carries no queue fact */
    }
  }

  async stop(): Promise<void> {
    await this.http.request(`/v1/sessions/${encodeURIComponent(this.sessionId)}/cancel`, {
      method: "POST",
    });
  }

  async answerDecision(decisionId: string, answer: string): Promise<void> {
    await this.http.request(
      `/v1/sessions/${encodeURIComponent(this.sessionId)}/decisions/${encodeURIComponent(decisionId)}`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ optionId: answer, answer }),
      },
    );
  }

  async upload(file: Blob): Promise<{ fileId: string }> {
    const response = await this.http.request("/v1/files", {
      method: "POST",
      headers: { "Content-Type": file.type || "application/octet-stream" },
      body: file,
    });
    const body = (await response.json()) as { fileId?: string };
    if (!body.fileId) throw new KeelApiError(500, "上传没有返回 fileId");
    return { fileId: body.fileId };
  }
}
