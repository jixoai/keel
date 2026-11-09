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
  ModelsResponse,
  ModelsSettingsReset,
  ModelsSettingsSaved,
  ModelsSettingsView,
  PluginsView,
  ProvidersResponse,
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

export class KeelHttp implements SettingsApi {
  readonly base: string;

  constructor(base = "") {
    this.base = base.replace(/\/$/, "");
  }

  url(path: string): string {
    return `${this.base}${path}`;
  }

  async request(path: string, init?: RequestInit): Promise<Response> {
    const response = await fetch(this.url(path), {
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
        const response = await fetch(
          this.http.url(`/v1/sessions/${encodeURIComponent(this.sessionId)}/events/stream?${query.toString()}`),
          { headers, signal: controller.signal, credentials: "same-origin" },
        );
        if (!response.ok) {
          const message = await readError(response);
          if (!retryable(response.status)) {
            stopped = true;
            opts.onState("closed");
            throw new KeelApiError(response.status, message);
          }
          schedule();
          return;
        }
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
