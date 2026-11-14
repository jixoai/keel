// The live transport: `fetch` + SSE against keel's AgentAPI, and the
// `SurfaceCommands` that write through it. Same origin by default (`keel
// serve` hosts the web build, the API and the stream), so no CORS and no
// tenant headers in the browser; a dev origin can be passed explicitly.
import type {
  AttachmentRef,
  ConnectionState,
  Delivery,
  EventSource,
  FireResultView,
  MediaCapabilitiesView,
  MediaCapabilityStatus,
  MediaCapabilityView,
  ModelsResponse,
  ModelsSettingsReset,
  ModelsSettingsSaved,
  ModelsSettingsView,
  PluginsView,
  ProvidersResponse,
  RealtimeStatusView,
  RetryResult,
  SecretSaved,
  SecretsView,
  SendOptions,
  SessionSummary,
  SettingsApi,
  SettingsIssue,
  TriggersView,
  UiCommands,
  WireEvent,
} from "./contract";

export class KeelApiError extends Error {
  readonly status: number;
  /** The server's field-level diagnostics (settings 400), verbatim. */
  readonly issues: SettingsIssue[];
  /** The current revision of a 409 conflict, when the server named one. */
  readonly revision: number | undefined;
  constructor(status: number, message: string, issues: SettingsIssue[] = [], revision?: number) {
    super(message);
    this.status = status;
    this.issues = issues;
    this.revision = revision;
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

/** The error body's message, field issues and conflict revision. */
async function readErrorBody(
  response: Response,
): Promise<{ message: string; issues: SettingsIssue[]; revision: number | undefined }> {
  const issues: SettingsIssue[] = [];
  let revision: number | undefined;
  let message = "";
  try {
    const text = await response.text();
    try {
      const body = asRecord(JSON.parse(text));
      if (typeof body.error === "string" && body.error) message = body.error;
      else if (typeof body.message === "string" && body.message) message = body.message;
      issues.push(...issueList(body.issues));
      if (typeof body.revision === "number" && Number.isFinite(body.revision)) revision = body.revision;
      if (!message && issues.length) message = issues[0]?.message ?? "";
    } catch {
      message = text;
    }
    if (!message) message = `HTTP ${response.status}`;
  } catch {
    message = `HTTP ${response.status}`;
  }
  return { message, issues, revision };
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

export class KeelHttp implements SettingsApi {
  readonly base: string;
  private readonly fetchImpl: FetchLike;

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

  async request(path: string, init?: RequestInit): Promise<Response> {
    const response = await this.fetchImpl(this.url(path), {
      credentials: "same-origin",
      ...init,
    });
    if (!response.ok) {
      const { message, issues, revision } = await readErrorBody(response);
      throw new KeelApiError(response.status, message, issues, revision);
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

  /** Exchange the local passphrase for the server's session cookie. */
  async login(password: string): Promise<void> {
    await this.request("/v1/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password }),
    });
  }

  async logout(): Promise<void> {
    await this.request("/v1/auth/logout", { method: "POST" });
  }

  async capabilities(): Promise<Record<string, unknown>> {
    const response = await this.request("/v1/capabilities");
    return (await response.json()) as Record<string, unknown>;
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
    return {
      created: body.created === true,
      seq: numberOr(body.seq, 0),
      requestId: typeof body.requestId === "string" ? body.requestId : "",
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

  /** `GET /v1/settings/secrets` — names and times only, never values. */
  async secrets(): Promise<SecretsView> {
    const response = await this.request("/v1/settings/secrets");
    const body = asRecord(await response.json());
    const rows = Array.isArray(body.secrets) ? body.secrets : [];
    return {
      secrets: rows.map((row) => {
        const item = asRecord(row);
        return {
          name: String(item.name ?? ""),
          createdAt: item.createdAt ? String(item.createdAt) : undefined,
          updatedAt: item.updatedAt ? String(item.updatedAt) : undefined,
        };
      }),
      available: body.available !== false,
      reason: typeof body.reason === "string" && body.reason ? body.reason : undefined,
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
}

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
    await this.http.request(`/v1/sessions/${encodeURIComponent(this.sessionId)}/input`, {
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
