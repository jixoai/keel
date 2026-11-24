// The fixture transport: a local, scripted keel service for development runs
// (`?fixture=1` on the page, `host.params.fixture` elsewhere). It implements
// the same `EventSource` / `SurfaceCommands` / session-list surface as
// `KeelHttp`, so the UI is the same either way — but nothing in the product
// selects it: a failed connection is offline with its own reason and retry.
//
// The scripts reproduce the shapes the kernel writes (user.message with
// delivery, assistant.message with inputSeqs/toolCall blocks, tool.result,
// compaction, decision, run.finished) and the queue/guide rules: a guide sent
// to an idle session is queued; a guide sent while running joins the turn and
// is consumed by the next assistant message; stop leaves queued input alone.
import type {
  AttachmentRef,
  CliCapabilityView,
  ConnectionState,
  Delivery,
  EventSource,
  FireResultView,
  McpAppCatalogueView,
  ModelOptionSpecsView,
  ModelPricingView,
  ModelPropertiesView,
  ModelsResponse,
  ModelsSettingsReset,
  ModelsSettingsSaved,
  ModelsSettingsView,
  ModelSelection,
  ModelView,
  PluginsView,
  ProviderCredentialSaved,
  ProviderProbeView,
  ProviderView,
  ProvidersResponse,
  RetryResult,
  SecretMeta,
  SecretSaved,
  SecretsView,
  SendOptions,
  DeliverablesOptions,
  DeliverablesView,
  SessionExportOptions,
  SessionExportView,
  SessionFilesQuery,
  SessionFilesView,
  SessionFileView,
  SessionSearchOptions,
  SessionSearchView,
  SessionSummary,
  SessionTasksView,
  SettingsApi,
  FilePreviewView,
  TaskPanelView,
  SettingsIssue,
  SkillDetailView,
  SkillInstallRequest,
  SkillRecordView,
  SkillSourceView,
  SkillsSettingsView,
  SkillUpdateRequest,
  SkillUpdateResult,
  TriggersView,
  UiCommands,
  WireEvent,
} from "./contract";
import { KeelApiError } from "./http";
import { providerCredentialSecretName } from "./model-catalog";
import {
  FixtureIdentity,
  fixtureAccountId,
  fixtureCurrentAccountId,
  fixtureFileHash,
  fixtureForkSessionId,
  fixtureRunSessionId,
} from "./fixture-identity";
import { demoOarEvents, demoOarProjection, FIXTURE_OAR_SESSION_ID } from "./fixture-oar";
import type {
  AccountView,
  AuditEventView,
  AuditQuery,
  BotKeyView,
  BotView,
  CreateAccountInput,
  CreateBotInput,
  CreateTeamBotInput,
  IdentityPage,
  PermissionGroupDetail,
  PermissionGroupSummary,
  RosterEntry,
  ShareInput,
  ShareResourceKind,
  ShareView,
  ShareWriteResult,
  TeamBotView,
  UpdateBotInput,
  UpdateTeamBotInput,
  WhoAmI,
} from "./identity";
import type {
  ProfileView,
  RoleView,
  RunStartView,
  SessionCancelView,
  SessionEventView,
  SubagentView,
  TeamTasksView,
} from "./bots-wire";
import type { ExternalProjectionView } from "./oar-projection";
import demoShot from "../../assets/demo-shot.png";

type Subscriber = {
  cursor: number;
  partials: boolean;
  onEvent: (event: WireEvent) => void;
  onPartial?: (p: { text: string; turnId?: string }) => void;
  onState: (s: ConnectionState["status"]) => void;
};

type QueuedInput = { seq: number; requestId: string; text: string; delivery: Delivery };

type Peer = {
  id: string;
  meta: SessionSummary;
  events: WireEvent[];
  nextSeq: number;
  running: boolean;
  queue: QueuedInput[];
  timers: number[];
  subscribers: Subscriber[];
  drainPaused: boolean;
};

// ---- settings demo state --------------------------------------------------

const asRecord = (value: unknown): Record<string, unknown> =>
  value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};

/** The reasoning vocabulary (modelcfg's ascending set). */
const REASONING_LEVELS = ["off", "minimal", "low", "medium", "high", "xhigh", "max"];

/** Credentials this demo process can resolve (the analogue of env vars). */
const DEMO_CREDENTIALS = ["S2API_KEY", "A6API_KEY"];

/** The demo provider's live line-up (what a discovery probe would answer).
 * Two ids are deliberately not declared in the demo document, so the merge
 * action has something to add. */
const DEMO_LIVE_MODELS = ["claude-sonnet-5", "deepseek-v4.1-flash", "deepseek-v4.1-pro", "gemini-flash-3"];

/** The masked preview rule (server: store.SecretPreview): >18 runes keeps the
 * first and last six code points, anything shorter is the fixed mask. */
const demoSecretPreview = (value: string): string => {
  const runes = [...value];
  if (runes.length <= 18) return "••••••••";
  return `${runes.slice(0, 6).join("")}…${runes.slice(-6).join("")}`;
};

const DEMO_PROTOCOLS = ["openai-chat-completions", "openai-responses", "anthropic-messages"];

/** The deployment-default models document (what a startup file would hold). */
const demoDeploymentDocument = (): Record<string, unknown> => ({
  schemaVersion: 1,
  providers: [
    {
      id: "s2api",
      name: "S2API",
      api: { type: "openai-chat-completions", baseUrl: "https://s2api.top/v1" },
      access: { type: "api-key", env: "S2API_KEY" },
      modelIds: ["deepseek-v4.1-flash"],
      visibility: "visible",
    },
    {
      id: "a6api",
      name: "A6API",
      api: { type: "openai-chat-completions", baseUrl: "https://api.a6api.com/v1" },
      access: { type: "api-key", env: "A6API_KEY" },
      modelIds: ["deepseek-v4.1-flash"],
      visibility: "visible",
    },
    {
      id: "anthropic",
      name: "Anthropic",
      api: { type: "anthropic-messages", baseUrl: "https://api.anthropic.com" },
      access: { type: "api-key", env: "ANTHROPIC_API_KEY" },
      modelIds: ["claude-sonnet-5"],
      visibility: "visible",
    },
  ],
  modelRules: [
    {
      match: "*",
      config: {
        enabled: true,
        properties: {
          contextWindow: 128000,
          inputFormat: { text: true, image: false, video: false, audio: false, pdf: false },
          outputFormat: { text: true },
          supportsToolCall: true,
          supportsJsonSchemaOutput: true,
          supportsNativeWebSearch: false,
        },
      },
    },
  ],
  selection: { providerId: "s2api", modelId: "deepseek-v4.1-flash", reasoningLevel: "high" },
});

/** The demo capability facts of one model identity. */
const demoModelCapabilities = (modelId: string): { contextWindow: number; image: boolean; levels: string[] } =>
  modelId === "claude-sonnet-5"
    ? { contextWindow: 200000, image: true, levels: ["off", "minimal", "low", "medium", "high", "xhigh", "max"] }
    : { contextWindow: 128000, image: false, levels: ["off", "low", "medium", "high"] };

const demoOptions = (modelId: string): ModelOptionSpecsView => ({
  reasoningLevel: {
    values: demoModelCapabilities(modelId).levels,
    map: Object.fromEntries(demoModelCapabilities(modelId).levels.map((level) => [level, {}])),
  },
  maxOutputTokens: { max: 8192, map: {} },
});

/** The demo price list per model (rates, currency and the free marker). */
const demoPricing = (modelId: string): ModelPricingView | undefined => {
  if (modelId === "deepseek-v4.1-flash") {
    return { currency: "USD", input: 0.27, output: 1.1, cacheRead: 0.07, cacheWrite: 0.27 };
  }
  if (modelId === "claude-sonnet-5") {
    return { currency: "USD", input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75, notes: "list price" };
  }
  if (modelId === "deepseek-v4.1-pro") {
    return { currency: "USD", input: 0, output: 0, free: true, notes: "free tier" };
  }
  return undefined;
};

/** A document's pricing block → the view (numbers coerced, unknown keys
 * dropped). */
const demoPricingOf = (raw: Record<string, unknown>): ModelPricingView => ({
  currency: typeof raw.currency === "string" ? raw.currency : undefined,
  input: demoNumber(raw.input),
  output: demoNumber(raw.output),
  cacheRead: demoNumber(raw.cacheRead),
  cacheWrite: demoNumber(raw.cacheWrite),
  free: raw.free === true ? true : undefined,
  notes: typeof raw.notes === "string" ? raw.notes : undefined,
});

/** A decoded JSON number, or undefined for a missing/invalid one. */
const demoNumber = (value: unknown): number | undefined => {
  if (value === null || value === undefined) return undefined;
  const parsed = typeof value === "string" && value.trim() !== "" ? Number(value) : value;
  if (typeof parsed !== "number" || !Number.isFinite(parsed) || parsed < 0) return undefined;
  return parsed;
};

/** A declared input-format object → the view (only real booleans survive). */
const demoMediaIn = (raw: Record<string, unknown>): ModelPropertiesView["inputFormat"] => ({
  text: raw.text === true ? true : undefined,
  image: raw.image === true ? true : undefined,
  video: raw.video === true ? true : undefined,
  audio: raw.audio === true ? true : undefined,
  pdf: raw.pdf === true ? true : undefined,
});

/** A declared output-format object → the view (no pdf in the vocabulary). */
const demoMediaOut = (raw: Record<string, unknown>): ModelPropertiesView["outputFormat"] => ({
  text: raw.text === true ? true : undefined,
  image: raw.image === true ? true : undefined,
  video: raw.video === true ? true : undefined,
  audio: raw.audio === true ? true : undefined,
});

interface DemoModelResolution {
  providerId: string;
  providerName: string;
  modelId: string;
  credential: string;
  credentialKind: "env" | "ref";
  executable: boolean;
}

const nowISO = () => new Date().toISOString();

/** A deterministic 64-hex stand-in for a bundle hash (the demo never computes
 * real content identity; the shapes are what the UI consumes). */
const demoHash = (seed: string): string => {
  const hex = seed
    .split("")
    .map((character) => character.charCodeAt(0).toString(16).padStart(2, "0"))
    .join("");
  return (hex + "0".repeat(64)).slice(0, 64);
};

const cloneSkill = (record: SkillRecordView): SkillRecordView =>
  JSON.parse(JSON.stringify(record)) as SkillRecordView;

/** The full text a fixture message carries (search/export need the whole
 * sentence; `textPreview` is the list's 90-character line). */
const payloadText = (payload: unknown): string => {
  if (!payload || typeof payload !== "object") return "";
  const row = payload as { text?: unknown; content?: unknown };
  if (typeof row.text === "string") return row.text;
  if (Array.isArray(row.content)) {
    return row.content
      .map((part) =>
        part && typeof part === "object" && typeof (part as { text?: unknown }).text === "string"
          ? (part as { text: string }).text
          : "",
      )
      .join("");
  }
  return "";
};

/** A bounded one-line window around a fixture search match. */
const matchSnippet = (text: string, needle: string): string => {
  const flat = text.replace(/\s+/g, " ").trim();
  const at = flat.toLowerCase().indexOf(needle.toLowerCase());
  if (at < 0) return flat.slice(0, 160);
  const start = Math.max(0, at - 40);
  const end = Math.min(flat.length, start + 160);
  return `${start > 0 ? "…" : ""}${flat.slice(start, end)}${end < flat.length ? "…" : ""}`;
};

/** The two generic demo paths the fixture manifest always holds. Their preview
 * content is fixed demo text; the hash is the same deterministic stand-in the
 * deliverable row carries. */
const fixtureFileContent: Record<string, string> = {
  "src/composer.tsx": "// 发布准备周报的输入组件（演示）\nexport const Composer = () => null;\n",
  "docs/release.md": "# 发布说明（演示）\n\n- 检查清单已更新\n- 回滚步骤已演练\n",
};

function textPreview(payload: unknown): string {
  if (!payload || typeof payload !== "object") return "";
  const row = payload as { text?: string; summary?: string; question?: string };
  const text = row.text ?? row.summary ?? row.question ?? "";
  return text.replace(/\s+/g, " ").slice(0, 90);
}

/** The builtin keel.team manifest's declared CLI tree, mirrored here so the
 * demo's Team CLI panel renders the same declaration the server serves
 * (`internal/pluginpkg/builtin/keel.team.yaml`). The fixture adds no command:
 * these are exactly the manifest's nine paths. */
const demoTeamCli = (): CliCapabilityView[] => [
  {
    path: ["team", "list"],
    summary: "List teams: id, name, captain, member count.",
    permissions: ["team.read"],
    danger: "read",
    idempotent: true,
    callable_by: ["ui", "agent", "team_captain"],
  },
  {
    path: ["team", "show"],
    summary: "Show one team's full document as JSON, captain first.",
    permissions: ["team.read"],
    danger: "read",
    idempotent: true,
    callable_by: ["ui", "agent", "team_captain"],
  },
  {
    path: ["team", "create"],
    summary: "Create a team; --captain is mandatory (a team without a captain is invalid).",
    permissions: ["team.write"],
    danger: "write",
    idempotent: false,
    callable_by: ["ui"],
  },
  {
    path: ["team", "update"],
    summary: "Update a team; fields not passed are preserved.",
    permissions: ["team.write"],
    danger: "write",
    idempotent: true,
    callable_by: ["ui"],
  },
  {
    path: ["team", "delete"],
    summary: "Delete a team (the agents it seated are untouched).",
    permissions: ["team.write"],
    danger: "destructive",
    idempotent: true,
    callable_by: ["ui"],
  },
  {
    path: ["team", "member", "add"],
    summary: "Seat an agent on a team (a captain may seat members through its own team CLI).",
    permissions: ["team.write"],
    danger: "write",
    idempotent: false,
    callable_by: ["ui", "team_captain"],
  },
  {
    path: ["team", "member", "remove"],
    summary: "Unseat an agent from a team (a captain may unseat members through its own team CLI).",
    permissions: ["team.write"],
    danger: "destructive",
    idempotent: true,
    callable_by: ["ui", "team_captain"],
  },
  {
    path: ["team", "save"],
    summary: "Upsert a team from a YAML or JSON configuration file.",
    permissions: ["team.write"],
    danger: "write",
    idempotent: true,
    callable_by: ["ui"],
  },
  {
    path: ["team", "start"],
    summary: "Start a team session; the captain answers and members run as their own Bots.",
    permissions: ["team.run"],
    danger: "write",
    idempotent: false,
    callable_by: ["ui", "team_captain"],
  },
];

export class FixtureClient implements SettingsApi {
  private sessions = new Map<string, Peer>();
  private sequence = 0;
  /** The tenant's effective models document (demo: the deployment default
   * until a save, a saved override after one). */
  private settingsState: {
    revision: number;
    source: "stored" | "deployment";
    document: Record<string, unknown>;
  } = { revision: 3, source: "deployment", document: demoDeploymentDocument() };
  /** Secret names, masked previews and times only — the demo never keeps a
   * value either. The third row predates the preview column (empty preview),
   * which is the legacy state the settings list renders as 「已保存」. */
  private secretState: SecretMeta[] = [
    {
      name: "s2api",
      preview: "sk-liv…-a1b2-c",
      createdAt: new Date(Date.now() - 86_400_000).toISOString(),
      updatedAt: new Date(Date.now() - 3_600_000).toISOString(),
    },
    {
      name: "webhook-token",
      preview: "••••••••",
      createdAt: new Date(Date.now() - 172_800_000).toISOString(),
      updatedAt: new Date(Date.now() - 172_800_000).toISOString(),
    },
    {
      name: "legacy-env-key",
      createdAt: new Date(Date.now() - 172_800_000).toISOString(),
      updatedAt: new Date(Date.now() - 172_800_000).toISOString(),
    },
  ];
  /** The demo skill library (settings「技能」): one locally authored skill and
   * one trusted subscription source. It is editable so the demo's install,
   * update, pin and remove paths are real state transitions, not stubs. */
  private skillRevision = 2;
  private skillState: SkillRecordView[] = [
    {
      name: "deploy-flow",
      description: "发布检查清单与回滚步骤",
      trust: "local",
      currentHash: demoHash("deploy-flow-v1"),
      installedAt: new Date(Date.now() - 86_400_000).toISOString(),
      updatedAt: new Date(Date.now() - 3_600_000).toISOString(),
      versions: [
        {
          hash: demoHash("deploy-flow-v1"),
          createdAt: new Date(Date.now() - 86_400_000).toISOString(),
          files: 2,
          bytes: 512,
        },
      ],
      fileCount: 2,
      bytes: 512,
      path: "active/deploy-flow",
      updateAvailable: false,
    },
  ];
  private skillSourceState: SkillSourceView[] = [
    {
      id: "team",
      kind: "local",
      url: "team",
      trust: "trusted",
      createdAt: new Date(Date.now() - 604_800_000).toISOString(),
    },
  ];
  private triggerFires = 12;

  constructor(
    private readonly imageURL: string = demoShot.url,
    /** Include W12's slot fixture plugin (a test/evidence launch asks for it
     * with `?fixture=1&plugins=1`, or `host.params.plugins === true`). The
     * default fixture stays the product-shaped sample. */
    private readonly pluginFixtures = false,
    /** Include the OAR external-run sample (`?oar=1` / `host.params.oar`):
     * one external session, its three records and its projection. */
    private readonly oarFixtures = false,
  ) {
    this.seed();
  }

  async probe(): Promise<"ok" | "auth" | "down"> {
    return "ok";
  }

  async capabilities(): Promise<Record<string, unknown>> {
    return {
      apiVersion: "v1",
      serviceVersion: "fixture",
      features: {
        "sessions.list": true,
        decisions: true,
        partials: true,
        "mcp.apps": true,
        "mcp.apps.sandboxOrigin": false,
        "mcp.events": false,
        // The development transport answers the task panel (the fixture's run
        // has a real in-memory task tree), so the run surface's entry renders
        // exactly as a deployment with the projection wired.
        "task.panel": true,
        "session.filePreview": true,
        // The W12 reads are answered by this transport (bounded search,
        // export, and the manifest-backed deliverables page), so the fixture
        // advertises the same deployment half a live keel serve does.
        "session.search": true,
        "session.export": true,
        "session.deliverables": true,
        // The demo answers the same settings surface a live server does, so
        // the editor's discovery and direct-entry actions are exercised.
        "settings.models": true,
        "settings.secrets": true,
      },
      limits: {},
      hooks: [],
    };
  }

  /** The scripted catalogue: one app, matching the binding the fixture's tool
   * result carries. `documentUrl` is a host-built path; the development server
   * has no document behind it, so a browser fixture run exercises exactly the
   * load-failure fallback (never a fabricated success). */
  async mcpApps(): Promise<McpAppCatalogueView> {
    const appId = "mcpapp_0123456789abcdef";
    const hash = "a".repeat(64);
    return {
      generation: 1,
      apps: [
        {
          appId,
          server: "weather",
          title: "天气面板",
          resourceUri: "ui://weather/dashboard",
          mimeType: "text/html;profile=mcp-app",
          visibility: ["model", "app"],
          tools: [{ tool: "dashboard", visibility: ["model", "app"] }],
          csp: "default-src 'none'",
          permissions: {},
          hash,
          documentUrl: `/v1/mcp/apps/${appId}/view?v=${hash}`,
          // The fixture is a client-only transport: this path exists only when
          // the browser E2E server serves it. A plain dev server's real answer
          // is a transport error (404), which the app slot reports verbatim —
          // never a fabricated "no document" state.
          diagnostics: ["fixture：文档路径由浏览器 E2E 服务器提供；普通 dev server 没有该路由"],
        },
      ],
      diagnostics: [],
      limits: { documentBytes: 2097152, cacheBytes: 67108864 },
    };
  }

  async listSessions(): Promise<SessionSummary[]> {
    return [...this.sessions.values()]
      .map((peer) => ({ ...peer.meta }))
      .sort((a, b) => (b.lastMessageAt ?? "").localeCompare(a.lastMessageAt ?? ""));
  }

  async createSession(): Promise<string> {
    const id = `demo-${++this.sequence}`;
    const peer = this.peer(id, "新会话");
    this.sessions.set(id, peer);
    return id;
  }

  async deleteSession(sessionId: string): Promise<void> {
    const peer = this.sessions.get(sessionId);
    if (peer) this.clearTimers(peer);
    this.sessions.delete(sessionId);
  }

  source(sessionId: string): EventSource {
    const peer = this.need(sessionId);
    return {
      list: async (after: number, limit = 500) =>
        peer.events.filter((event) => event.seq > after).slice(0, limit),
      stream: (opts) => this.stream(peer, opts),
    };
  }

  /** The last `count` events at or before `lastSeq`, the same window the HTTP
   * transport reads: the store's list enrichment (titles, previews, the
   * pending-decision badge) must work in the demo too, not only live. */
  async tail(sessionId: string, lastSeq: number, count = 6): Promise<WireEvent[]> {
    const peer = this.need(sessionId);
    const upto = lastSeq > 0 ? lastSeq : peer.nextSeq - 1;
    return peer.events
      .filter((event) => event.seq <= upto)
      .slice(-count);
  }

  commands(sessionId: string): UiCommands {
    const peer = this.need(sessionId);
    return {
      send: (text, opts: SendOptions) =>
        this.send(peer, text, opts.delivery, opts.requestId, opts.attachments, opts.selection),
      sendWithAttachments: (text, opts: SendOptions) =>
        this.send(peer, text, opts.delivery, opts.requestId, opts.attachments, opts.selection),
      stop: async () => this.stop(peer),
      answerDecision: async (decisionId, answer) => this.answer(peer, decisionId, answer),
      upload: async () => ({ fileId: `fixture-file-${++this.sequence}` }),
    };
  }

  // ---- settings demo -----------------------------------------------------

  async modelsSettings(): Promise<ModelsSettingsView> {
    return {
      revision: this.settingsState.revision,
      source: this.settingsState.source,
      document: JSON.parse(JSON.stringify(this.settingsState.document)) as Record<string, unknown>,
      issues: [],
    };
  }

  async saveModelsSettings(
    document: Record<string, unknown>,
    expectedRevision?: number,
  ): Promise<ModelsSettingsSaved> {
    this.guardRevision(expectedRevision);
    const issues = this.validateDocument(document);
    if (issues.length) {
      throw new KeelApiError(400, `invalid models document: ${issues[0]?.message ?? ""}`, issues);
    }
    const stored: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(document)) {
      if (key !== "expectedRevision") stored[key] = value;
    }
    this.settingsState = {
      revision: this.settingsState.revision + 1,
      source: "stored",
      document: stored,
    };
    const selection = asRecord(stored.selection);
    const resolution = this.resolveSelection(stored);
    return {
      revision: this.settingsState.revision,
      source: "stored",
      selection: {
        providerId: typeof selection.providerId === "string" ? selection.providerId : undefined,
        modelId: String(selection.modelId ?? ""),
        reasoningLevel: typeof selection.reasoningLevel === "string" ? selection.reasoningLevel : undefined,
      },
      providersExecutable: (resolution.providers ?? []).filter((row) => row.executable).length,
      modelsSelectable: resolution.models.filter((row) => row.selectable).length,
    };
  }

  async resetModelsSettings(expectedRevision?: number): Promise<ModelsSettingsReset> {
    this.guardRevision(expectedRevision);
    this.settingsState = {
      revision: this.settingsState.revision + 1,
      source: "deployment",
      document: demoDeploymentDocument(),
    };
    return { revision: this.settingsState.revision, source: "deployment" };
  }

  async providers(): Promise<ProvidersResponse> {
    const resolution = this.resolveSelection(this.settingsState.document);
    return { providers: resolution.providers, issues: resolution.issues };
  }

  async models(): Promise<ModelsResponse> {
    const resolution = this.resolveSelection(this.settingsState.document);
    return {
      // The executable registry: only complete, enabled models of a provider
      // whose credential resolves here. `/v1/providers` keeps the rest — the
      // settings view is where their issues are read (server: ModelViews vs
      // ProviderViews).
      models: resolution.models.filter(
        (model) => model.enabled === true && model.executable === true,
      ),
      issues: [],
      enforcement: { toolCall: true, media: true },
    };
  }

  async secrets(): Promise<SecretsView> {
    return { secrets: this.secretState.map((item) => ({ ...item })), available: true };
  }

  async putSecret(name: string, value: string): Promise<SecretSaved> {
    const trimmed = name.trim();
    if (!trimmed) throw new KeelApiError(400, "secret name is required");
    if (!value) throw new KeelApiError(400, "secret value is required");
    const at = nowISO();
    const preview = demoSecretPreview(value);
    const existing = this.secretState.find((item) => item.name === trimmed);
    if (existing) {
      existing.updatedAt = at;
      existing.preview = preview;
    } else {
      this.secretState.push({ name: trimmed, preview, createdAt: at, updatedAt: at });
    }
    this.secretState.sort((a, b) => a.name.localeCompare(b.name));
    return { name: trimmed, updatedAt: at };
  }

  /** The direct-entry path (server: PUT /v1/settings/providers/{id}/credential):
   * the demo stores the value under the name the server derives
   * (`provider_<id>`, collision-resistant suffix after sanitization) and
   * answers the reference plus the masked preview. It never returns the
   * value. */
  async saveProviderCredential(providerId: string, value: string): Promise<ProviderCredentialSaved> {
    const id = providerId.trim();
    if (!id) throw new KeelApiError(400, "provider id is required");
    if (!value.trim()) throw new KeelApiError(400, "value is required: a blank field never erases a stored credential");
    const secretName = providerCredentialSecretName(id);
    const at = nowISO();
    const preview = demoSecretPreview(value);
    const existing = this.secretState.find((item) => item.name === secretName);
    if (existing) {
      existing.updatedAt = at;
      existing.preview = preview;
    } else {
      this.secretState.push({ name: secretName, preview, createdAt: at, updatedAt: at });
    }
    this.secretState.sort((a, b) => a.name.localeCompare(b.name));
    return {
      providerId: id,
      secretName,
      secretRef: `secret:${secretName}`,
      preview,
      updatedAt: at,
    };
  }

  async deleteSecret(name: string): Promise<void> {
    if (!this.secretState.some((item) => item.name === name)) {
      throw new KeelApiError(404, `secret ${name} was not found`);
    }
    this.secretState = this.secretState.filter((item) => item.name !== name);
  }

  /** The provider probe (server: POST /v1/providers/{id}/test). The demo
   * answers a measured result for a known provider: `ready` with the live
   * line-up when its credential resolves, `needs_auth` when it does not.
   * Unknown ids are a 404, exactly like the server. */
  async testProvider(providerId: string): Promise<ProviderProbeView> {
    const provider = this.resolveSelection(this.settingsState.document).providers.find(
      (row) => row.providerId === providerId,
    );
    if (!provider) throw new KeelApiError(404, `provider ${providerId} is not served by this deployment`);
    const started = Date.now();
    if (provider.executable !== true) {
      return {
        providerId,
        providerType: provider.providerType,
        apiType: provider.api?.type,
        endpoint: provider.api?.baseUrl,
        ok: false,
        status: "needs_auth",
        latencyMs: 0,
        models: [],
        error: "no credential could be resolved; no request was sent",
      };
    }
    return {
      providerId,
      providerType: provider.providerType,
      apiType: provider.api?.type,
      endpoint: provider.api?.baseUrl,
      ok: true,
      status: "ready",
      latencyMs: 12 + (Date.now() - started),
      modelCount: DEMO_LIVE_MODELS.length,
      models: [...DEMO_LIVE_MODELS],
    };
  }

  // ---- skill library ------------------------------------------------------

  async skillsSettings(check = true): Promise<SkillsSettingsView> {
    return {
      revision: this.skillRevision,
      root: "managed",
      checked: check,
      skills: this.skillState.map((item) => cloneSkill(item)),
      sources: this.skillSourceState.map((item) => ({ ...item })),
      limits: { maxFiles: 256, maxBytes: 8 * 1024 * 1024, maxFileBytes: 2 * 1024 * 1024 },
      policy: {
        trustValues: ["trusted", "untrusted", "local"],
        sourceKinds: ["local"],
        maxFiles: 256,
        maxBytes: 8 * 1024 * 1024,
        maxFileBytes: 2 * 1024 * 1024,
        scriptExecution: "bash",
      },
    };
  }

  async showSkill(name: string): Promise<SkillDetailView> {
    const record = this.skillState.find((item) => item.name === name);
    if (!record) throw new KeelApiError(404, `skill ${name} was not found`, [], undefined, "not_found");
    return {
      ...cloneSkill(record),
      body: `# ${name}\n\n${record.description ?? ""}\n\n1. 运行发布检查\n2. 记录结果\n3. 需要时回滚\n`,
      files: [
        { path: "SKILL.md", size: 256, hash: demoHash(`${name}-skill`), mode: 0o644 },
        { path: "references/checklist.md", size: 256, hash: demoHash(`${name}-checklist`), mode: 0o644 },
      ],
    };
  }

  async installSkill(request: SkillInstallRequest): Promise<SkillRecordView> {
    const name = request.name.trim();
    if (!name) throw new KeelApiError(400, "skill name is required");
    const existing = this.skillState.find((item) => item.name === name);
    if (existing?.pinnedHash) {
      throw new KeelApiError(409, `skill ${name} is pinned; unpin before installing`, [], undefined, "pinned");
    }
    const hash = demoHash(`${name}-${this.skillRevision + 1}`);
    const at = nowISO();
    this.skillRevision += 1;
    if (existing) {
      existing.currentHash = hash;
      existing.updatedAt = at;
      existing.description = request.description || existing.description;
      existing.versions = [...(existing.versions ?? []), { hash, createdAt: at, files: 1, bytes: 256 }];
      if (request.pin) existing.pinnedHash = hash;
      return cloneSkill(existing);
    }
    const record: SkillRecordView = {
      name,
      description: request.description,
      trust: request.sourceId ? (this.skillSourceState.find((item) => item.id === request.sourceId)?.trust ?? "untrusted") : (request.trust ?? "local"),
      sourceId: request.sourceId,
      currentHash: hash,
      pinnedHash: request.pin ? hash : undefined,
      installedAt: at,
      updatedAt: at,
      versions: [{ hash, createdAt: at, files: 1, bytes: 256 }],
      fileCount: 1,
      bytes: 256,
      path: `active/${name}`,
      updateAvailable: false,
    };
    this.skillState = [...this.skillState, record];
    return cloneSkill(record);
  }

  async updateSkill(name: string, _request: SkillUpdateRequest = {}): Promise<SkillUpdateResult> {
    const record = this.skillState.find((item) => item.name === name);
    if (!record) throw new KeelApiError(404, `skill ${name} was not found`, [], undefined, "not_found");
    if (record.pinnedHash) {
      throw new KeelApiError(409, `skill ${name} is pinned; unpin before updating`, [], undefined, "pinned");
    }
    const previous = record.currentHash;
    const next = demoHash(`${name}-${this.skillRevision + 1}`);
    const at = nowISO();
    this.skillRevision += 1;
    record.currentHash = next;
    record.updatedAt = at;
    record.versions = [...(record.versions ?? []), { hash: next, createdAt: at, files: 1, bytes: 256 }];
    record.updateAvailable = false;
    record.availableHash = undefined;
    return { status: "updated", record: cloneSkill(record), previousHash: previous };
  }

  async pinSkill(name: string, hash?: string): Promise<SkillRecordView> {
    const record = this.skillState.find((item) => item.name === name);
    if (!record) throw new KeelApiError(404, `skill ${name} was not found`, [], undefined, "not_found");
    const target = hash || record.currentHash;
    if (!(record.versions ?? []).some((version) => version.hash === target)) {
      throw new KeelApiError(404, `skill ${name} has no version ${target.slice(0, 12)}`, [], undefined, "version_not_found");
    }
    record.currentHash = target;
    record.pinnedHash = target;
    return cloneSkill(record);
  }

  async unpinSkill(name: string): Promise<SkillRecordView> {
    const record = this.skillState.find((item) => item.name === name);
    if (!record) throw new KeelApiError(404, `skill ${name} was not found`, [], undefined, "not_found");
    record.pinnedHash = undefined;
    return cloneSkill(record);
  }

  async removeSkill(name: string): Promise<void> {
    if (!this.skillState.some((item) => item.name === name)) {
      throw new KeelApiError(404, `skill ${name} was not found`, [], undefined, "not_found");
    }
    this.skillState = this.skillState.filter((item) => item.name !== name);
  }

  async subscribeSkillSource(source: {
    id: string;
    kind?: string;
    url: string;
    trust?: string;
  }): Promise<SkillSourceView> {
    if (this.skillSourceState.some((item) => item.id === source.id)) {
      throw new KeelApiError(409, `source ${source.id} already exists`, [], undefined, "already_exists");
    }
    const stored: SkillSourceView = {
      id: source.id,
      kind: source.kind ?? "local",
      url: source.url,
      trust: source.trust ?? "untrusted",
      createdAt: nowISO(),
    };
    this.skillSourceState = [...this.skillSourceState, stored];
    return { ...stored };
  }

  async unsubscribeSkillSource(id: string): Promise<void> {
    const inUse = this.skillState.find((item) => item.sourceId === id);
    if (inUse) {
      throw new KeelApiError(409, `source ${id} is still the origin of skill ${inUse.name}`, [], undefined, "source_in_use");
    }
    if (!this.skillSourceState.some((item) => item.id === id)) {
      throw new KeelApiError(404, `source ${id} was not found`, [], undefined, "not_found");
    }
    this.skillSourceState = this.skillSourceState.filter((item) => item.id !== id);
  }

  /** The `$` file group's demo source (a session's manifest paths). The two
   * run-surface reports also carry their writing card (`lastTaskId`), which is
   * the attribution the task panel's resource view reads; the composer group's
   * own sample paths stay for the `$` menu. The CSV is a manifest-only write by
   * the summary card: no attach event names it, so the panel's file group is
   * the one place it appears. */
  async sessionFiles(sessionId?: string, query: SessionFilesQuery = {}): Promise<SessionFilesView> {
    const files: SessionFileView[] = [
      { path: "src/composer.tsx", size: 1200, updatedSeq: 8 },
      { path: "docs/release.md", size: 640, updatedSeq: 5 },
    ];
    if (sessionId === fixtureRunSessionId || sessionId === fixtureForkSessionId) {
      files.push(
        {
          path: "reports/refund-analysis.md",
          hash: fixtureFileHash("reports/refund-analysis.md"),
          size: 2048,
          updatedSeq: 19,
          lastTaskId: "task-captain",
        },
        {
          path: "reports/refund-report.json",
          hash: fixtureFileHash("reports/refund-report.json"),
          size: 180,
          updatedSeq: 21,
          lastTaskId: "task-captain",
        },
      );
    }
    if (sessionId === fixtureRunSessionId) {
      files.push({
        path: "reports/refund-by-channel.csv",
        hash: fixtureFileHash("reports/refund-by-channel.csv"),
        size: 48,
        updatedSeq: 22,
        lastTaskId: "task-captain-summary",
      });
    }
    // The route's own page contract: path-sorted, an exclusive path cursor and
    // a bounded limit, so the fixture transport is paged exactly like the live
    // one instead of growing a one-shot answer the panel cannot page.
    const sorted = [...files].sort((left, right) => (left.path < right.path ? -1 : left.path > right.path ? 1 : 0));
    const cursor = query.cursor ?? "";
    const limit = Math.max(1, query.limit ?? 200);
    const remaining = sorted.filter((file) => file.path > cursor);
    const page = remaining.slice(0, limit);
    const hasMore = remaining.length > page.length;
    return {
      files: page,
      count: page.length,
      limit,
      truncated: hasMore,
      hasMore,
      nextCursor: hasMore ? page[page.length - 1]?.path : undefined,
    };
  }

  /** `GET /v1/sessions/search` in memory: bounded message hits across the
   * fixture's sessions, or session rows matched on title/preview. */
  async sessionSearch(query: string, opts: SessionSearchOptions = {}): Promise<SessionSearchView> {
    const needle = query.trim();
    if (opts.scope === "sessions") {
      const rows = [...this.sessions.values()]
        .filter((peer) => {
          const haystack = `${peer.meta.title ?? ""} ${peer.meta.lastMessage ?? ""}`.toLowerCase();
          return haystack.includes(needle.toLowerCase());
        })
        .sort((left, right) => (right.meta.updatedAt ?? "").localeCompare(left.meta.updatedAt ?? ""))
        .slice(0, opts.limit ?? 20)
        .map((peer) => ({
          sessionId: peer.id,
          title: peer.meta.title ?? "",
          createdAt: peer.meta.createdAt,
          updatedAt: peer.meta.updatedAt,
          lastMessageAt: peer.meta.lastMessageAt,
          snippet: matchSnippet(peer.meta.lastMessage ?? peer.meta.title ?? "", needle),
        }));
      return { scope: "sessions", query: needle, sessions: rows, count: rows.length, limit: opts.limit ?? 20, hasMore: false };
    }
    const hits: Array<{ sessionId: string; sessionTitle: string; seq: number; kind: string; role: string; at?: string; snippet: string }> = [];
    for (const peer of this.sessions.values()) {
      if (opts.sessionId && peer.id !== opts.sessionId) continue;
      for (const event of peer.events) {
        if (event.kind !== "user.message" && event.kind !== "assistant.message") continue;
        if (opts.kind && event.kind !== `${opts.kind}.message`) continue;
        const text = payloadText(event.payload);
        if (!text.toLowerCase().includes(needle.toLowerCase())) continue;
        const timestamp = (event.payload as { timestamp?: number } | undefined)?.timestamp;
        hits.push({
          sessionId: peer.id,
          sessionTitle: peer.meta.title ?? "",
          seq: event.seq,
          kind: event.kind,
          role: event.kind === "user.message" ? "user" : "assistant",
          at: typeof timestamp === "number" ? new Date(timestamp).toISOString() : peer.meta.updatedAt,
          snippet: matchSnippet(text, needle),
        });
      }
    }
    hits.sort((left, right) => (right.at ?? "").localeCompare(left.at ?? ""));
    const limit = opts.limit ?? 20;
    const page = hits.slice(0, limit);
    return {
      scope: "messages",
      query: needle,
      messages: page,
      count: page.length,
      limit,
      hasMore: hits.length > limit,
    };
  }

  /** `GET /v1/sessions/{id}/export` in memory: the transcript's text messages,
   * bounded by `limit`, keyset by seq. */
  async sessionExport(sessionId: string, opts: SessionExportOptions = {}): Promise<SessionExportView> {
    const peer = this.need(sessionId);
    const limit = Math.max(1, Math.min(500, opts.limit ?? 200));
    const after = opts.afterSeq ?? 0;
    const all = peer.events
      .filter((event) => event.seq > after && (event.kind === "user.message" || event.kind === "assistant.message"))
      .map((event) => ({
        seq: event.seq,
        kind: event.kind,
        role: event.kind === "user.message" ? "user" : "assistant",
        at: peer.meta.updatedAt,
        text: payloadText(event.payload),
      }))
      .filter((message) => message.text !== "");
    const page = all.slice(0, limit);
    return {
      sessionId,
      title: peer.meta.title ?? "",
      format: "json",
      messages: page,
      count: page.length,
      limit,
      afterSeq: after,
      nextAfterSeq: page.length > 0 ? page[page.length - 1].seq : 0,
      hasMore: all.length > limit,
      truncated: false,
    };
  }

  /** `GET /v1/sessions/{id}/deliverables` in memory: the same manifest rows the
   * `$` group reads, each with the deterministic hash and the preview link. */
  async sessionDeliverables(sessionId: string, opts: DeliverablesOptions = {}): Promise<DeliverablesView> {
    const source = await this.sessionFiles(sessionId);
    const files = [...(source.files ?? [])].sort((left, right) => left.path.localeCompare(right.path));
    const start = opts.after ? files.findIndex((file) => file.path > opts.after!) : 0;
    const from = start < 0 ? files.length : start;
    const limit = opts.limit ?? 100;
    const page = files.slice(from, from + limit);
    const rows = page.map((file) => {
      const hash = `sha256:${demoHash(file.path)}`;
      return {
        path: file.path,
        hash,
        size: file.size,
        mode: 0o644,
        updatedSeq: file.updatedSeq,
        lastTaskId: file.lastTaskId,
        previewUrl: `/v1/sessions/${sessionId}/file-preview?path=${encodeURIComponent(file.path)}&hash=${encodeURIComponent(hash)}`,
      };
    });
    return {
      sessionId,
      deliverables: rows,
      count: rows.length,
      limit,
      hasMore: from + limit < files.length,
      nextCursor: from + limit < files.length ? files[from + limit - 1].path : undefined,
    };
  }

  /** The `$` task group's demo source (the session's cards). */
  async sessionTasks(_sessionId: string): Promise<SessionTasksView> {
    return {
      tasks: [
        { taskId: "task-demo-1", title: "整理发布清单", status: "open", updatedAt: nowISO() },
        { taskId: "task-demo-2", title: "回滚演练", status: "done", updatedAt: nowISO() },
      ],
      count: 2,
      truncated: false,
    };
  }

  async plugins(): Promise<PluginsView> {
    const started = new Date(Date.now() - 3_600_000).toISOString();
    const plugins = [
        {
          id: "keel.team",
          version: "1.0.0",
          state: "enabled",
          provides: { cli: demoTeamCli() },
          requires: [],
          tools: [],
          hooks: [],
          triggers: [],
          diagnostics: [],
          startedAt: started,
        },
        {
          id: "demo.workflow",
          version: "0.4.1",
          state: "enabled",
          // The server sends `provides` as the manifest's object (tools/hooks/
          // triggers/cli…), never as a bare list; the fixture mirrors that.
          provides: {
            tools: ["workflow.plan", "workflow.status"],
            hooks: ["turn.start", "turn.end"],
            triggers: ["demo.workflow.daily"],
            cli: [
              {
                path: ["workflow", "plan"],
                summary: "把一段目标拆成可执行的计划并入库。",
                description: "写入计划，可被后续运行读取；不修改既有计划。",
                permissions: ["workflow.write"],
                danger: "write",
                idempotent: false,
                callable_by: ["ui"],
                examples: ["keel workflow plan --goal 发布"],
              },
              {
                path: ["workflow", "status"],
                summary: "读取当前计划与各步骤状态。",
                permissions: ["workflow.read"],
                danger: "read",
                idempotent: true,
                callable_by: ["ui", "agent"],
              },
            ],
          },
          requires: [],
          tools: ["workflow.plan", "workflow.status"],
          hooks: ["turn.start", "turn.end"],
          triggers: [
            {
              id: "demo.workflow.daily",
              kind: "interval",
              schedule: "24h",
              prompt: "生成每日工作摘要并发到工作区。",
              enabled: true,
              owner: "demo.workflow",
            },
          ],
          diagnostics: [],
          startedAt: started,
        },
        {
          // W12's fixture contribution (change keel-harness-v2): one enabled
          // plugin whose chunk draws in the host tree, plus the data nav and
          // action forms. The development transport answers the same inventory
          // shape the server does, so the registry's sync is exercised by the
          // real path with no second installer.
          id: "keel.w12.fixture",
          version: "1.0.0",
          state: "enabled",
          layer: "ui",
          provides: {},
          requires: [],
          tools: [],
          hooks: [],
          triggers: [],
          diagnostics: [],
          startedAt: started,
          extensibility: { usable: true, doc: "EXTENSIBILITY.md", schema: "extensibility.json" },
          contributes: [
            { id: "top-band", slot: "keel.ui.top-band", form: "chunk", entry: "fixture:w12-hello", title: "W12 插件条" },
            { id: "sidebar", slot: "keel.ui.sidebar", form: "chunk", entry: "fixture:w12-hello", title: "W12 插件侧栏" },
            {
              id: "settings-section",
              slot: "keel.ui.settings-section",
              form: "chunk",
              entry: "fixture:w12-hello",
              title: "W12 插件设置区",
            },
            { id: "route", slot: "keel.ui.route", form: "chunk", entry: "fixture:w12-hello", title: "W12 插件路由" },
            {
              id: "tool-renderer",
              slot: "keel.ui.tool-renderer",
              form: "chunk",
              entry: "fixture:w12-hello",
              title: "W12 工具渲染器",
              tool: "deploy.prepare",
            },
            {
              id: "message-part",
              slot: "keel.ui.message-part",
              form: "chunk",
              entry: "fixture:w12-hello",
              title: "W12 消息部件",
              part: "assistant",
            },
            {
              id: "nav",
              slot: "keel.ui.nav",
              form: "data",
              payload: { label: "W12 插件导航", path: "/plugins", keywords: ["插件", "w12", "slot"] },
            },
            {
              id: "action",
              slot: "keel.ui.action",
              form: "data",
              payload: { label: "W12 插件动作", action: "navigate", path: "/settings", keywords: ["设置", "w12"] },
            },
          ],
        },
        {
          id: "demo.webhook",
          version: "0.2.0",
          state: "failed",
          provides: { hooks: ["turn.end"] },
          requires: ["demo.workflow"],
          tools: [],
          hooks: ["turn.end"],
          triggers: [
            {
              id: "demo.webhook.oncall",
              kind: "webhook",
              prompt: "处理一条值班 webhook：{{payload}}",
              enabled: false,
              owner: "demo.webhook",
            },
          ],
          diagnostics: ["start failed: listen tcp 127.0.0.1:0: permission denied"],
          stoppedAt: new Date(Date.now() - 3_590_000).toISOString(),
        },
    ];
    return {
      generation: 3,
      plugins: this.pluginFixtures ? plugins : plugins.filter((plugin) => plugin.id !== "keel.w12.fixture"),
      hooks: { subscriptions: 6, emitted: 42, delivered: 42, dropped: 0, failed: 1, queued: 0, capacity: 256 },
      tools: ["workflow.plan", "workflow.status", "clock.now"],
      hookStats: { subscriptions: 6, emitted: 42, delivered: 42, dropped: 0, failed: 1, queued: 0, capacity: 256 },
    };
  }

  async triggers(): Promise<TriggersView> {
    return {
      triggers: [
        {
          id: "demo.workflow.daily",
          kind: "interval",
          schedule: "24h",
          prompt: "生成每日工作摘要并发到工作区。",
          enabled: true,
          owner: "demo.workflow",
          nextFireAt: new Date(Date.now() + 6 * 3_600_000).toISOString(),
          lastFireAt: new Date(Date.now() - 18 * 3_600_000).toISOString(),
          lastStatus: "started",
          fires: this.triggerFires,
          failures: 1,
        },
        {
          id: "demo.webhook.oncall",
          kind: "webhook",
          prompt: "处理一条值班 webhook：{{payload}}",
          enabled: false,
          owner: "demo.webhook",
          fires: 3,
          failures: 3,
          lastStatus: "disabled",
        },
      ],
    };
  }

  async fireTrigger(id: string, reason = "manual"): Promise<FireResultView> {
    const registry = await this.triggers();
    const trigger = (registry.triggers ?? []).find((item) => item.id === id);
    if (!trigger) throw new KeelApiError(404, `trigger ${id} was not found`);
    if (trigger.enabled === false) throw new KeelApiError(409, `trigger ${id} is disabled`);
    this.triggerFires += 1;
    this.sequence += 1;
    void reason;
    return {
      triggerId: id,
      status: "started",
      idempotencyKey: `demo-fire-${this.sequence}`,
      sessionId: `demo-trigger-${this.sequence}`,
      seq: 1,
      deduped: false,
      attempts: 1,
    };
  }

  /** The retry seam, same contract as the server: resolve the turn's own input
   * from the log and re-ask it as a NEW queued message (never a rewrite). */
  async retryTurn(sessionId: string, turnId: string): Promise<RetryResult> {
    const peer = this.need(sessionId);
    const raw = turnId.startsWith("t_") ? turnId.slice(2) : "";
    const seq = Number.parseInt(raw, 10);
    if (!raw || !Number.isFinite(seq) || seq <= 0) {
      throw new KeelApiError(400, 'turnId must be the client turn id, "t_<seq>"');
    }
    const event = peer.events.find((item) => item.seq === seq);
    if (!event) throw new KeelApiError(404, "turn not found");
    let source: { text?: string; selection?: ModelSelection } | undefined;
    if (event.kind === "user.message") {
      source = event.payload as { text?: string; selection?: ModelSelection };
    } else if (event.kind === "assistant.message") {
      const inputSeqs = (event.payload as { inputSeqs?: number[] }).inputSeqs ?? [];
      const input = peer.events.find((item) => item.seq === inputSeqs[inputSeqs.length - 1]);
      if (!input || input.kind !== "user.message") {
        throw new KeelApiError(400, "turn is not a user turn");
      }
      source = input.payload as { text?: string; selection?: ModelSelection };
    } else {
      throw new KeelApiError(400, "turn is not a user turn");
    }
    const requestId = `retry:${this.sequence++}-${Math.random().toString(36).slice(2, 8)}`;
    await this.send(peer, source.text ?? "", "queue", requestId, undefined, source.selection);
    return { created: true, seq: peer.nextSeq - 1, requestId };
  }

  // ---- identity & access (change keel-bot-identity-access, A4.4a) ---------
  //
  // The development fixture's sample deployment. Every method delegates to
  // `FixtureIdentity`, whose in-memory transitions are the same writes a live
  // deployment performs, so the books shell renders real state (and its five
  // states) without a server. A product launch never reaches this class.

  private readonly identity = new FixtureIdentity();

  whoami(): Promise<WhoAmI> {
    return this.identity.whoami();
  }

  listAccounts(limit = 100): Promise<IdentityPage<AccountView>> {
    return this.identity.listAccounts(limit);
  }

  createAccount(input: CreateAccountInput): Promise<AccountView> {
    return this.identity.createAccount(input);
  }

  getAccount(accountId: string): Promise<{ account: AccountView; groups: PermissionGroupSummary[] }> {
    return this.identity.getAccount(accountId);
  }

  setAccountDisabled(accountId: string, disabled: boolean, reason?: string): Promise<AccountView> {
    return this.identity.setAccountDisabled(accountId, disabled, reason);
  }

  resetAccountPassword(accountId: string, password: string): Promise<void> {
    return this.identity.resetAccountPassword(accountId, password);
  }

  addAccountGroup(accountId: string, groupId: string): Promise<void> {
    return this.identity.addAccountGroup(accountId, groupId);
  }

  removeAccountGroup(accountId: string, groupId: string): Promise<void> {
    return this.identity.removeAccountGroup(accountId, groupId);
  }

  listPermissionGroups(limit = 100): Promise<IdentityPage<PermissionGroupSummary>> {
    return this.identity.listPermissionGroups(limit);
  }

  getPermissionGroup(groupId: string): Promise<PermissionGroupDetail> {
    return this.identity.getPermissionGroup(groupId);
  }

  createPermissionGroup(name: string): Promise<PermissionGroupSummary> {
    return this.identity.createPermissionGroup(name);
  }

  addPermissionGroupMember(groupId: string, accountId: string): Promise<void> {
    return this.identity.addPermissionGroupMember(groupId, accountId);
  }

  removePermissionGroupMember(groupId: string, accountId: string): Promise<void> {
    return this.identity.removePermissionGroupMember(groupId, accountId);
  }

  addPermissionGroupGrant(groupId: string, key: string, scope: string): Promise<void> {
    return this.identity.addPermissionGroupGrant(groupId, key, scope);
  }

  removePermissionGroupGrant(groupId: string, key: string, scope: string): Promise<void> {
    return this.identity.removePermissionGroupGrant(groupId, key, scope);
  }

  listBots(limit = 100, includeArchived = false): Promise<IdentityPage<BotView>> {
    return this.identity.listBots(limit, includeArchived);
  }

  getBot(botId: string): Promise<BotView> {
    return this.identity.getBot(botId);
  }

  createBot(input: CreateBotInput): Promise<BotView> {
    return this.identity.createBot(input);
  }

  updateBot(botId: string, input: UpdateBotInput): Promise<BotView> {
    return this.identity.updateBot(botId, input);
  }

  archiveBot(botId: string): Promise<void> {
    return this.identity.archiveBot(botId);
  }

  invokeBot(botId: string, text: string): Promise<RunStartView> {
    return this.identity.invokeBot(botId, text);
  }

  listBotKeys(botId: string): Promise<BotKeyView[]> {
    return this.identity.listBotKeys(botId);
  }

  issueBotKey(botId: string, label = "", accountId?: string): Promise<{ key: BotKeyView; plaintext: string; notice: string }> {
    void accountId;
    return this.identity.issueBotKey(botId, label);
  }

  revokeBotKey(botId: string, keyId: string): Promise<void> {
    return this.identity.revokeBotKey(botId, keyId);
  }

  listProfiles(limit = 100): Promise<IdentityPage<ProfileView>> {
    return this.identity.listProfiles(limit);
  }

  getProfile(profileId: string): Promise<ProfileView> {
    return this.identity.getProfile(profileId);
  }

  listRoles(limit = 100): Promise<IdentityPage<RoleView>> {
    return this.identity.listRoles(limit);
  }

  listTeamBots(limit = 100, includeArchived = false): Promise<IdentityPage<TeamBotView>> {
    return this.identity.listTeamBots(limit, includeArchived);
  }

  getTeamBot(teamBotId: string): Promise<TeamBotView> {
    return this.identity.getTeamBot(teamBotId);
  }

  createTeamBot(input: CreateTeamBotInput): Promise<TeamBotView> {
    return this.identity.createTeamBot(input);
  }

  updateTeamBot(teamBotId: string, input: UpdateTeamBotInput): Promise<TeamBotView> {
    return this.identity.updateTeamBot(teamBotId, input);
  }

  updateTeamBotRoster(
    teamBotId: string,
    roster: RosterEntry[] | undefined,
    expectedRevision: number,
  ): Promise<TeamBotView> {
    return this.identity.updateTeamBotRoster(teamBotId, roster, expectedRevision);
  }

  archiveTeamBot(teamBotId: string): Promise<void> {
    return this.identity.archiveTeamBot(teamBotId);
  }

  startTeamBot(teamBotId: string, text?: string): Promise<RunStartView> {
    return this.identity.startTeamBot(teamBotId, text);
  }

  sessionEvents(sessionId: string): Promise<SessionEventView[]> {
    return this.identity.sessionEvents(sessionId);
  }

  teamTasks(sessionId: string): Promise<TeamTasksView> {
    return this.identity.teamTasks(sessionId);
  }

  taskPanel(sessionId: string, memberLimit?: number, eventLimit?: number): Promise<TaskPanelView> {
    return this.identity.taskPanel(sessionId, memberLimit, eventLimit);
  }

  /** `GET /v1/sessions/{id}/external/projection` (change keel-oar-renderer):
   * the demo's one external session, folded server-side exactly like the live
   * route. The typed refusals mirror it too — a launch without the OAR sample
   * is a deployment without the journal (501), and every other session is not
   * external (404). */
  async externalProjection(sessionId: string): Promise<ExternalProjectionView> {
    if (!this.oarFixtures) {
      throw new KeelApiError(
        501,
        "this deployment has not wired the OAR journal",
        [],
        undefined,
        "oar_not_wired",
      );
    }
    if (sessionId !== FIXTURE_OAR_SESSION_ID) {
      throw new KeelApiError(404, "not an external session", [], undefined, "not_external_session");
    }
    return demoOarProjection(sessionId);
  }

  async filePreview(sessionId: string, path: string, referenceHash?: string, offset = 0): Promise<FilePreviewView> {
    try {
      return await this.identity.filePreview(sessionId, path, referenceHash, offset);
    } catch (error) {
      // The generic demo paths are not in the run session's file map; the
      // manifest above still holds them, so the preview reads their fixed demo
      // text instead of claiming the file is missing.
      const content = fixtureFileContent[path];
      if (content === undefined) throw error;
      const hash = `sha256:${demoHash(path)}`;
      const lines = content.split("\n").length;
      return {
        path,
        hash,
        size: content.length,
        offset,
        limit: 200,
        totalLines: lines,
        returnedLines: lines,
        nextOffset: lines,
        truncated: false,
        content,
        referenceHash,
        matchesReference: referenceHash === undefined ? undefined : referenceHash === hash,
      };
    }
  }

  listSubagents(parentSessionId?: string): Promise<SubagentView[]> {
    return this.identity.listSubagents(parentSessionId);
  }

  cancelSession(sessionId: string): Promise<SessionCancelView> {
    return this.identity.cancelSession(sessionId);
  }

  listShares(kind: ShareResourceKind, resourceId: string): Promise<ShareView[]> {
    return this.identity.listShares(kind, resourceId);
  }

  addShare(kind: ShareResourceKind, resourceId: string, input: ShareInput): Promise<ShareWriteResult> {
    return this.identity.addShare(kind, resourceId, input);
  }

  revokeShare(kind: ShareResourceKind, resourceId: string, input: ShareInput): Promise<ShareWriteResult> {
    return this.identity.revokeShare(kind, resourceId, input);
  }

  listAuditEvents(query?: AuditQuery): Promise<IdentityPage<AuditEventView>> {
    return this.identity.listAuditEvents(query);
  }

  private guardRevision(expectedRevision: number | undefined) {
    if (expectedRevision === undefined) return;
    if (expectedRevision !== this.settingsState.revision) {
      throw new KeelApiError(
        409,
        `model settings revision conflict: expected revision ${expectedRevision} does not match the current revision`,
        [],
        this.settingsState.revision,
      );
    }
  }

  /** The demo body of the resolver: provider/model views and the default
   * selection's executability share one derivation, like the server's. */
  private resolveSelection(document: Record<string, unknown>): {
    providers: ProviderView[];
    models: ModelView[];
    issues: SettingsIssue[];
  } {
    const rows = Array.isArray(document.providers) ? document.providers : [];
    const models: ModelView[] = [];
    const providers: ProviderView[] = [];
    for (const raw of rows) {
      const provider = asRecord(raw);
      const providerId = String(provider.id ?? "");
      if (!providerId) continue;
      const access = asRecord(provider.access);
      const env = typeof access.env === "string" ? access.env : "";
      const ref = typeof access.ref === "string" ? access.ref : "";
      const refName = ref.startsWith("secret:") ? ref.slice("secret:".length) : "";
      const credential = env || ref;
      const resolvable = env
        ? DEMO_CREDENTIALS.includes(env)
        : refName
          ? this.secretState.some((item) => item.name === refName)
          : false;
      const modelIds = Array.isArray(provider.modelIds) ? provider.modelIds.map(String) : [];
      const providerIssues: SettingsIssue[] = [];
      if (!resolvable) {
        providerIssues.push({
          field: `providers.${providerId}.access`,
          message: `credential ${credential || "(missing)"} is not set in this process`,
        });
      }
      const providerModels: ModelView[] = modelIds.map((modelId) => {
        // The demo's exact per-model rule (what the model editor saves) wins
        // over the capability seed, exactly as the server's providerModelRules
        // layer does.
        const rule = asRecord(
          (Array.isArray(document.providerModelRules) ? document.providerModelRules : []).find((entry) => {
            const candidate = asRecord(entry);
            return candidate.providerId === providerId && candidate.modelId === modelId;
          }),
        );
        const ruleConfig = asRecord(rule.config);
        const ruleProperties = asRecord(ruleConfig.properties);
        const ruleOptions = asRecord(ruleConfig.options);
        const ruleMaxOutput = asRecord(ruleOptions.maxOutputTokens);
        const ruleReasoning = asRecord(ruleOptions.reasoningLevel);
        const hasRuleReasoning = Object.prototype.hasOwnProperty.call(ruleOptions, "reasoningLevel");
        const declaredReasoning = Array.isArray(ruleReasoning.values)
          ? ruleReasoning.values.filter((value): value is string => typeof value === "string")
          : [];
        const rulePricing = asRecord(ruleConfig.pricing);
        const caps = demoModelCapabilities(modelId);
        const declaredInput = asRecord(ruleProperties.inputFormat);
        const declaredOutput = asRecord(ruleProperties.outputFormat);
        const baseOptions = demoOptions(modelId);
        // Merge patch semantics (server: mergeJSONObjects): a rule's modality
        // object patches the capability seed key by key; a null key clears the
        // inherited fact, the other keys stay.
        const mergedInput: Record<string, unknown> = {
          text: true,
          image: caps.image,
          video: false,
          audio: false,
          pdf: false,
          ...declaredInput,
        };
        for (const [key, value] of Object.entries(declaredInput)) {
          if (value === null) delete mergedInput[key];
        }
        const mergedOutput: Record<string, unknown> = { text: true, ...declaredOutput };
        for (const [key, value] of Object.entries(declaredOutput)) {
          if (value === null) delete mergedOutput[key];
        }
        // The price list merges the same way: a cleared field (null) is gone
        // from this model's list, the fields the dialog did not edit stay; a
        // rule's `pricing: null` removes the whole block.
        const pricingCleared =
          Object.prototype.hasOwnProperty.call(ruleConfig, "pricing") && ruleConfig.pricing === null;
        const mergedPricing: Record<string, unknown> = pricingCleared
          ? {}
          : { ...asRecord(demoPricing(modelId)), ...rulePricing };
        for (const [key, value] of Object.entries(rulePricing)) {
          if (value === null) delete mergedPricing[key];
        }
        const modelEnabled = ruleConfig.enabled === false ? false : true;
        const model: ModelView = {
          providerId,
          modelId,
          providerType: typeof ruleConfig.providerType === "string" ? ruleConfig.providerType : undefined,
          api: asRecord(provider.api) as ModelView["api"],
          properties: {
            contextWindow:
              ruleProperties.contextWindow === null
                ? 0
                : demoNumber(ruleProperties.contextWindow) ?? caps.contextWindow,
            inputFormat: demoMediaIn(mergedInput),
            outputFormat: demoMediaOut(mergedOutput),
            supportsToolCall: ruleProperties.supportsToolCall === undefined ? true : ruleProperties.supportsToolCall === true,
            supportsJsonSchemaOutput:
              ruleProperties.supportsJsonSchemaOutput === undefined
                ? true
                : ruleProperties.supportsJsonSchemaOutput === true,
            supportsNativeWebSearch: ruleProperties.supportsNativeWebSearch === true,
          },
          options: {
            ...baseOptions,
            reasoningLevel: hasRuleReasoning
              ? {
                  values: declaredReasoning,
                  map: asRecord(ruleReasoning.map) as Record<string, Record<string, unknown>>,
                }
              : baseOptions.reasoningLevel,
            maxOutputTokens:
              Object.keys(ruleMaxOutput).length > 0
                ? {
                    max: ruleMaxOutput.max === null ? 0 : demoNumber(ruleMaxOutput.max) ?? 8192,
                    map: {},
                  }
                : baseOptions.maxOutputTokens,
          },
          pricing:
            Object.keys(mergedPricing).length > 0 ? demoPricingOf(mergedPricing) : undefined,
          enabled: modelEnabled,
          executable: resolvable,
          selectable: resolvable && modelEnabled,
        };
        models.push(model);
        return model;
      });
      const providerView: ProviderView = {
        providerId,
        name: typeof provider.name === "string" ? provider.name : undefined,
        visibility: "visible",
        api: asRecord(provider.api) as ProviderView["api"],
        access: { type: "api-key", env: env || undefined, ref: ref || undefined },
        models: providerModels,
        issues: providerIssues.length ? providerIssues : undefined,
        executable: resolvable,
        selectable: resolvable && providerModels.some((model) => model.selectable === true),
      };
      providers.push(providerView);
    }
    return { providers, models, issues: [] };
  }

  /** The demo validator: the same shape checks the server's modelcfg makes,
   * with field paths. */
  private validateDocument(document: Record<string, unknown>): SettingsIssue[] {
    const issues: SettingsIssue[] = [];
    if (document.schemaVersion !== 1) {
      issues.push({ field: "schemaVersion", message: "schemaVersion must be 1" });
    }
    const rows = document.providers;
    if (!Array.isArray(rows) || rows.length === 0) {
      issues.push({ field: "providers", message: "providers must be a non-empty array" });
    } else {
      rows.forEach((raw, index) => {
        const provider = asRecord(raw);
        if (typeof provider.id !== "string" || !provider.id) {
          issues.push({ field: `providers[${index}].id`, message: "provider id is required" });
        }
        const api = asRecord(provider.api);
        if (!DEMO_PROTOCOLS.includes(String(api.type ?? ""))) {
          issues.push({
            field: `providers[${index}].api.type`,
            message: `unknown api type ${String(api.type ?? "(missing)")}`,
          });
        }
        const access = asRecord(provider.access);
        if (!access.env && !access.ref) {
          issues.push({ field: `providers[${index}].access`, message: "access needs env or ref" });
        }
      });
    }
    const selection = asRecord(document.selection);
    const modelId = typeof selection.modelId === "string" ? selection.modelId : "";
    if (!modelId) {
      issues.push({ field: "selection.modelId", message: "a default selection is required" });
    } else {
      const resolution = this.resolveSelection(document);
      const providerId = typeof selection.providerId === "string" ? selection.providerId : "";
      const target = resolution.models.find(
        (model) => model.modelId === modelId && (!providerId || model.providerId === providerId),
      );
      if (!target) {
        issues.push({
          field: "selection.modelId",
          message: `selection ${providerId ? `${providerId}/` : ""}${modelId} does not match any declared model`,
        });
      } else if (!target.executable) {
        const provider = resolution.providers.find((item) => item.providerId === target.providerId);
        issues.push({
          field: `providers.${target.providerId}.access`,
          message: provider?.issues?.[0]?.message ?? `credential for ${target.providerId} is not resolvable`,
        });
      } else {
        const level = typeof selection.reasoningLevel === "string" ? selection.reasoningLevel : "";
        const levels = target.options?.reasoningLevel?.values ?? [];
        if (level && !levels.includes(level)) {
          issues.push({
            field: "selection.reasoningLevel",
            message: REASONING_LEVELS.includes(level)
              ? `reasoning level ${level} is not declared by ${target.providerId}/${target.modelId}`
              : `unknown reasoning level ${level}`,
          });
        }
      }
    }
    return issues;
  }

  // ---- seeds ------------------------------------------------------------

  private peer(id: string, title: string, runStatus = "done"): Peer {
    return {
      id,
      meta: {
        sessionId: id,
        title,
        lastMessage: "",
        lastSeq: 0,
        runStatus,
        createdAt: nowISO(),
        updatedAt: nowISO(),
        lastMessageAt: nowISO(),
        // The fixture's sessions belong to the current user, like a live
        // deployment's own workspace. The shared sample below overrides one
        // row so 「共享给我」 has a real member.
        ownerAccountId: fixtureCurrentAccountId,
        createdByAccountId: fixtureCurrentAccountId,
        ownerDisplayName: "李娜",
        shared: false,
        sharedWithMe: false,
      },
      events: [],
      nextSeq: 1,
      running: false,
      queue: [],
      timers: [],
      subscribers: [],
      drainPaused: false,
    };
  }

  private seed() {
    const base = Date.now();
    this.add("demo-chat", "发布准备周报", [
      { kind: "user.message", payload: { text: "帮我总结一下这周的发布准备情况。", requestId: "seed-chat-1", delivery: "queue", timestamp: base - 540_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [
            { type: "thinking", thinking: "先看发布清单，再确认验收与剩余项。" },
            {
              type: "text",
              text: "本周发布准备已基本就绪：\n\n1. 构建与签名流程已跑通，web 产物可同源托管；\n2. 端到端验收覆盖了会话、审批、工具卡与断线重连；\n3. 还差一项——发布说明需要补齐关键截图。\n\n建议今天完成截图与说明，明天进入发布窗口。",
            },
          ],
          inputSeqs: [1],
          stopReason: "end_turn",
          timestamp: base - 532_000,
        },
      },
      {
        kind: "run.finished",
        payload: { turns: 1, tokensIn: 1820, tokensOut: 364, stopReason: "done", durationMs: 6400, provider: "s2api", model: "deepseek-v4.1-flash", timestamp: base - 531_000 },
      },
    ]);

    this.add("demo-tools", "并行工具检查", [
      { kind: "user.message", payload: { text: "检查 api、worker、web 三个服务的健康状态。", requestId: "seed-tools-1", delivery: "queue", timestamp: base - 1_500_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [
            { type: "thinking", thinking: "三个检查彼此独立，可以并行执行。" },
            { type: "toolCall", id: "call-api", name: "health.check", arguments: { service: "api", timeoutMs: 2000 } },
            { type: "toolCall", id: "call-worker", name: "health.check", arguments: { service: "worker", timeoutMs: 2000 } },
            { type: "toolCall", id: "call-web", name: "health.check", arguments: { service: "web", timeoutMs: 2000 } },
          ],
          inputSeqs: [1],
          stopReason: "tool_use",
          timestamp: base - 1_494_000,
        },
      },
      { kind: "tool.result", payload: { toolCallId: "call-api", toolName: "health.check", content: [{ type: "text", text: "status=ok version=0.1.0 p95=42ms" }], isError: false, durationMs: 320, timestamp: base - 1_492_000 } },
      { kind: "tool.result", payload: { toolCallId: "call-worker", toolName: "health.check", content: [{ type: "text", text: "status=ok queue=0 workers=3" }], isError: false, durationMs: 410, timestamp: base - 1_491_500 } },
      { kind: "tool.result", payload: { toolCallId: "call-web", toolName: "health.check", content: [{ type: "text", text: "status=degraded upstream=503 retry=2" }], isError: true, durationMs: 280, timestamp: base - 1_491_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [{ type: "text", text: "api 与 worker 正常；web 上游返回 503（已重试 2 次），建议先切到只读降级页。" }],
          inputSeqs: [1],
          stopReason: "end_turn",
          timestamp: base - 1_489_000,
        },
      },
      {
        kind: "run.finished",
        payload: { turns: 1, tokensIn: 2210, tokensOut: 268, stopReason: "done", durationMs: 5100, provider: "s2api", model: "deepseek-v4.1-flash", timestamp: base - 1_488_000 },
      },
    ]);

    this.add("demo-mcp-app", "天气面板", [
      { kind: "user.message", payload: { text: "用天气面板看看上海今天的天气。", requestId: "seed-mcp-app-1", delivery: "queue", timestamp: base - 700_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [
            { type: "thinking", thinking: "天气工具带交互面板，先调用再让面板展示。" },
            {
              type: "toolCall",
              id: "call-mcp-app",
              name: "mcp",
              arguments: { action: "call", server: "weather", tool: "dashboard", arguments: { city: "上海" } },
            },
          ],
          inputSeqs: [1],
          stopReason: "tool_use",
          timestamp: base - 696_000,
        },
      },
      {
        kind: "tool.result",
        payload: {
          toolCallId: "call-mcp-app",
          toolName: "mcp",
          content: [{ type: "text", text: "上海 24°C 多云，湿度 62%" }],
          details: {
            server: "weather",
            tool: "dashboard",
            app: { appId: "mcpapp_0123456789abcdef", resourceUri: "ui://weather/dashboard", visibility: ["model", "app"] },
            structuredContent: { city: "上海", tempC: 24 },
          },
          isError: false,
          durationMs: 420,
          timestamp: base - 694_000,
        },
      },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [{ type: "text", text: "上海现在 24°C、多云，湿度 62%。面板里可以按小时看变化。" }],
          inputSeqs: [1],
          stopReason: "end_turn",
          timestamp: base - 692_000,
        },
      },
      {
        kind: "run.finished",
        payload: { turns: 1, tokensIn: 1640, tokensOut: 220, stopReason: "done", durationMs: 3800, provider: "s2api", model: "deepseek-v4.1-flash", timestamp: base - 690_000 },
      },
    ]);

    this.add("demo-approval", "发布审批", [
      {
        kind: "user.message",
        payload: {
          text: "把 0.1.0 发布到生产环境，并附上截图。",
          requestId: "seed-approval-1",
          delivery: "queue",
          timestamp: base - 240_000,
          attachments: [
            { name: "demo-shot.png", mime: "image/png", size: 39037, url: this.imageURL },
          ],
        },
      },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [
            { type: "thinking", thinking: "发布是不可逆动作，需要人工确认窗口。" },
            { type: "text", text: "发布包已构建并签名，准备进入生产窗口。需要你确认发布时间。" },
            { type: "toolCall", id: "call-deploy", name: "deploy.prepare", arguments: { version: "0.1.0", channel: "production" } },
          ],
          inputSeqs: [1],
          stopReason: "tool_use",
          timestamp: base - 234_000,
        },
      },
      { kind: "tool.result", payload: { toolCallId: "call-deploy", toolName: "deploy.prepare", content: [{ type: "text", text: "artifact=keel-0.1.0.nshpkg sha256=9f3c…c21a" }], isError: false, durationMs: 2400, timestamp: base - 230_000 } },
      {
        kind: "decision",
        payload: {
          decisionId: "dec-release",
          question: "是否在 22:00–23:00 窗口发布 Keel 0.1.0？",
          options: [
            { optionId: "approve", label: "批准发布", description: "立即进入发布窗口，发布后自动跑冒烟" },
            { optionId: "hold", label: "暂缓", description: "保持当前版本，明天再评估" },
            { optionId: "notify", label: "先通知团队", description: "发送发布预告，收集意见后再决定" },
          ],
          recommendation: "approve",
          status: "open",
          timeoutAt: base + 120_000,
          timestamp: base - 229_000,
        },
      },
    ]);

    this.add("demo-guide", "会议纪要整理", [
      { kind: "user.message", payload: { text: "整理这周的会议纪要，按主题归档。", requestId: "seed-guide-1", delivery: "queue", timestamp: base - 900_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [
            { type: "thinking", thinking: "先读日历与录音，再按主题聚类。" },
            { type: "text", text: "好的，我先读取日历和录音文件，再把纪要按主题归档。" },
            { type: "toolCall", id: "call-calendar", name: "calendar.read", arguments: { range: "this-week" } },
            { type: "toolCall", id: "call-transcripts", name: "transcript.load", arguments: { source: "meeting-notes" } },
          ],
          inputSeqs: [1],
          stopReason: "tool_use",
          timestamp: base - 894_000,
        },
      },
      { kind: "tool.result", payload: { toolCallId: "call-calendar", toolName: "calendar.read", content: [{ type: "text", text: "events=7" }], isError: false, durationMs: 180, timestamp: base - 893_000 } },
      {
        kind: "user.message",
        payload: { text: "另外，把行动项按负责人分组。", requestId: "seed-guide-2", delivery: "guide", timestamp: base - 890_000 },
      },
    ]);

    this.add("demo-compact", "重构方案续传", [
      {
        kind: "compaction",
        payload: {
          summary: "此前 42 条消息已压缩：重构方案确定采用无状态投影；迁移分两步；主要风险是事件序号乱序与断线缺口。",
          manual: true,
          tokensBefore: 48000,
          timestamp: base - 2_000_000,
        },
      },
      { kind: "user.message", payload: { text: "继续上一轮的重构方案，先做第一步。", requestId: "seed-compact-1", delivery: "queue", timestamp: base - 1_990_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [
            { type: "thinking", thinking: "从压缩摘要恢复上下文，确认第一步是无状态投影。" },
            { type: "text", text: "从压缩摘要恢复：第一步实现无状态投影（事件 → Turn 视图），保持零 DOM、可单测；第二步再接渲染器。现在先落地第一步。" },
          ],
          inputSeqs: [2],
          stopReason: "end_turn",
          timestamp: base - 1_984_000,
        },
      },
      {
        kind: "run.finished",
        payload: { turns: 1, tokensIn: 980, tokensOut: 210, stopReason: "done", durationMs: 3200, provider: "s2api", model: "deepseek-v4.1-flash", timestamp: base - 1_983_000 },
      },
    ]);

    this.add("demo-failed", "生成发布说明", [
      { kind: "user.message", payload: { text: "调用模型生成发布说明。", requestId: "seed-failed-1", delivery: "queue", timestamp: base - 3_600_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [],
          inputSeqs: [1],
          stopReason: "error",
          errorMessage: "upstream 429: rate limit exceeded (retry-after 30s)",
          timestamp: base - 3_594_000,
        },
      },
      {
        kind: "run.finished",
        payload: { turns: 1, tokensIn: 0, tokensOut: 0, stopReason: "failed", durationMs: 1200, provider: "s2api", model: "deepseek-v4.1-flash", timestamp: base - 3_593_000 },
      },
    ], "failed");

    // Two state samples the review asked for: a question that arrives while a
    // tool call is still unfinished (the tool row must not spin while the
    // conversation is parked on the answer), and a session whose newest turn is
    // a maintenance compaction with no input to re-ask (no retry button, no
    // empty assistant shell).
    this.add("demo-parked", "部署卡在确认", [
      { kind: "user.message", payload: { text: "把发布包推到预发环境，然后等我确认。", requestId: "seed-parked-1", delivery: "queue", timestamp: base - 620_000 } },
      {
        kind: "assistant.message",
        payload: {
          role: "assistant",
          content: [{ type: "toolCall", id: "call-parked", name: "deploy.prepare", arguments: { channel: "staging" } }],
          inputSeqs: [1],
          stopReason: "tool_use",
          timestamp: base - 614_000,
        },
      },
      {
        kind: "decision",
        payload: {
          decisionId: "dec-parked",
          question: "预发包已就绪：继续推生产，还是先回滚？",
          options: [
            { optionId: "continue", label: "继续推生产", description: "进入生产窗口并跑冒烟" },
            { optionId: "rollback", label: "先回滚", description: "撤掉预发包，明天再评估" },
          ],
          recommendation: "continue",
          status: "open",
          timeoutAt: base + 180_000,
          timestamp: base - 600_000,
        },
      },
    ], "running");

    this.add("demo-compact-only", "仅压缩记录", [
      {
        kind: "compaction",
        payload: {
          summary: "上一轮 68 条消息已压缩：只剩摘要，没有可重问的输入。",
          manual: true,
          tokensBefore: 61000,
          timestamp: base - 1_200_000,
        },
      },
    ]);

    this.add("demo-empty", "新会话", []);

    // The OAR sample (change keel-oar-renderer): one external session whose
    // turn carries three `oar.record` events and whose projection is a fake
    // runtime. Asked for with `?oar=1` / `host.params.oar`, so the default
    // sample (and every existing test's expectations) stays unchanged.
    if (this.oarFixtures) {
      this.add(FIXTURE_OAR_SESSION_ID, "Codex 外部运行", [
        {
          kind: "user.message",
          payload: {
            text: "让 Codex 看一眼构建发布脚本，确认包内容没有漏项。",
            requestId: "seed-oar-1",
            delivery: "queue",
            timestamp: base - 300_000,
          },
        },
        {
          kind: "assistant.message",
          payload: {
            role: "assistant",
            content: [
              { type: "thinking", thinking: "外部工具的记录不建任务卡；归属挂在发起它的卡上。" },
              {
                type: "text",
                text: "已把这次检查交给 Codex（外部运行时）。它的记录经 OAR 汇成下方这张运行卡，任务归属仍是发起它的那张卡。",
              },
            ],
            inputSeqs: [1],
            stopReason: "end_turn",
            timestamp: base - 296_000,
          },
        },
        ...demoOarEvents(),
        {
          kind: "run.finished",
          payload: {
            turns: 1,
            tokensIn: 0,
            tokensOut: 0,
            stopReason: "done",
            durationMs: 4200,
            metering: "external",
            timestamp: base - 290_000,
          },
        },
      ]);
    }

    // One session shared with 李娜 by 陈墨, so 「共享给我」 and 「由 陈墨 共享」
    // are a real row rather than a fabricated filter.
    const sharedSession = this.sessions.get("demo-chat");
    if (sharedSession) {
      sharedSession.meta.ownerAccountId = fixtureAccountId("chenmo");
      sharedSession.meta.ownerDisplayName = "陈墨";
      sharedSession.meta.sharedWithMe = true;
    }
  }

  private add(id: string, title: string, events: Array<{ kind: string; payload: unknown }>, runStatus = "done") {
    const peer = this.peer(id, title, runStatus);
    for (const event of events) {
      peer.events.push({ seq: peer.nextSeq++, kind: event.kind, payload: event.payload });
    }
    const last = peer.events[peer.events.length - 1];
    peer.meta.lastSeq = last?.seq ?? 0;
    const lastMessage = [...peer.events]
      .reverse()
      .find((event) => event.kind !== "run.finished" && textPreview(event.payload));
    peer.meta.lastMessage = lastMessage ? textPreview(lastMessage.payload) : "";
    const lastAt = [...peer.events]
      .reverse()
      .map((event) => (event.payload as { timestamp?: number } | undefined)?.timestamp)
      .find((value): value is number => typeof value === "number" && Number.isFinite(value));
    peer.meta.lastMessageAt = lastAt ? new Date(lastAt).toISOString() : undefined;
    const lastIsRunFinished = last?.kind === "run.finished";
    peer.running = peer.events.length > 0 && !lastIsRunFinished && runStatus !== "failed";
    if (peer.running) peer.meta.runStatus = "running";
    this.sessions.set(id, peer);
  }

  private need(sessionId: string): Peer {
    const peer = this.sessions.get(sessionId);
    if (!peer) throw new Error(`未知会话 ${sessionId}`);
    return peer;
  }

  // ---- events -----------------------------------------------------------

  private append(peer: Peer, kind: string, payload: unknown): WireEvent {
    const event: WireEvent = { seq: peer.nextSeq++, kind, payload };
    peer.events.push(event);
    peer.meta.lastSeq = event.seq;
    const preview = textPreview(payload);
    if (preview) peer.meta.lastMessage = preview;
    peer.meta.lastMessageAt = nowISO();
    peer.meta.updatedAt = nowISO();
    this.deliver(peer, event);
    return event;
  }

  private deliver(peer: Peer, event: WireEvent) {
    for (const sub of peer.subscribers) {
      if (event.seq > sub.cursor) {
        sub.cursor = event.seq;
        sub.onEvent(event);
      }
    }
  }

  private flush(peer: Peer) {
    for (const sub of peer.subscribers) {
      for (const event of peer.events) {
        if (event.seq > sub.cursor) {
          sub.cursor = event.seq;
          sub.onEvent(event);
        }
      }
    }
  }

  private partial(peer: Peer, text: string, turnId?: string) {
    for (const sub of peer.subscribers) {
      if (sub.partials) sub.onPartial?.({ text, turnId });
    }
  }

  private stream(
    peer: Peer,
    opts: {
      after: number;
      partials?: boolean;
      onEvent: (event: WireEvent) => void;
      onPartial?: (p: { text: string; turnId?: string }) => void;
      onState: (s: ConnectionState["status"]) => void;
    },
  ): () => void {
    const sub: Subscriber = {
      cursor: opts.after,
      partials: !!opts.partials,
      onEvent: opts.onEvent,
      onPartial: opts.onPartial,
      onState: opts.onState,
    };
    peer.subscribers.push(sub);
    const open = setTimeout(() => {
      sub.onState("open");
      this.flush(peer);
    }, 20) as unknown as number;
    void open;
    return () => {
      peer.subscribers = peer.subscribers.filter((item) => item !== sub);
      sub.onState("closed");
    };
  }

  // ---- commands ---------------------------------------------------------

  private async send(
    peer: Peer,
    text: string,
    delivery: Delivery,
    requestId: string,
    attachments?: AttachmentRef[],
    selection?: ModelSelection,
  ) {
    const extras = {
      ...(attachments?.length ? { attachments } : {}),
      // The selection is the input's own fact (never written to the tenant
      // document), exactly as the server persists it.
      ...(selection ? { selection } : {}),
    };
    if (peer.running) {
      const seq = peer.nextSeq;
      const accepted: Delivery = delivery === "guide" ? "guide" : "queue";
      const event = this.append(peer, "user.message", {
        text,
        requestId,
        delivery: accepted,
        timestamp: Date.now(),
        ...extras,
      });
      peer.meta.runStatus = "running";
      if (accepted === "queue") {
        peer.queue.push({ seq: event.seq, requestId, text, delivery: accepted });
      } else {
        // A guide is consumed inside the running turn.
        this.schedule(peer, 1100, () => {
          if (!peer.running) return;
          this.append(peer, "assistant.message", {
            role: "assistant",
            content: [
              { type: "text", text: `已按引导调整：${text}` },
            ],
            inputSeqs: [seq],
            stopReason: "end_turn",
            timestamp: Date.now(),
          });
          this.finish(peer, "done");
        });
      }
      void seq;
      return;
    }
    const event = this.append(peer, "user.message", {
      text,
      requestId,
      delivery: "queue",
      timestamp: Date.now(),
      ...extras,
    });
    peer.meta.runStatus = "running";
    this.reply(peer, event.seq, text);
  }

  private async stop(peer: Peer) {
    if (!peer.running) return;
    this.clearTimers(peer);
    peer.drainPaused = true;
    peer.running = false;
    peer.meta.runStatus = "cancelled";
    const timer = setTimeout(() => {
      this.append(peer, "run.finished", {
        turns: 1,
        tokensIn: 0,
        tokensOut: 0,
        stopReason: "cancelled",
        durationMs: 800,
        timestamp: Date.now(),
      });
    }, 320) as unknown as number;
    peer.timers.push(timer);
  }

  private async answer(peer: Peer, decisionId: string, answer: string) {
    this.append(peer, "decision", {
      decisionId,
      status: "answered",
      answer,
      timestamp: Date.now(),
    });
    this.schedule(peer, 600, () => {
      this.append(peer, "assistant.message", {
        role: "assistant",
        content: [{ type: "text", text: answer === "hold" ? "已暂缓发布，当前版本保持不变。" : "已记录你的选择，我会继续执行并汇报结果。" }],
        stopReason: "end_turn",
        timestamp: Date.now(),
      });
      this.finish(peer, "done");
    });
  }

  // ---- scripted runs ----------------------------------------------------

  private reply(peer: Peer, inputSeq: number, prompt: string) {
    peer.running = true;
    peer.meta.runStatus = "running";
    peer.drainPaused = false;
    const withTools = /工具|检查|health|部署|发布/i.test(prompt);
    const thinking = "收到。先确认上下文，再给出可执行的下一步。";
    const toolA = `call-${peer.id}-${peer.nextSeq}-a`;
    const toolB = `call-${peer.id}-${peer.nextSeq}-b`;
    this.schedule(peer, 260, () => {
      this.append(peer, "assistant.message", {
        role: "assistant",
        content: [
          { type: "thinking", thinking },
          ...(withTools
            ? [
                { type: "toolCall", id: toolA, name: "context.read", arguments: { scope: "session" } },
                { type: "toolCall", id: toolB, name: "workspace.list", arguments: { path: "." } },
              ]
            : []),
        ],
        inputSeqs: [inputSeq],
        stopReason: withTools ? "tool_use" : "end_turn",
        timestamp: Date.now(),
      });
    });
    let start = 900;
    if (withTools) {
      this.schedule(peer, 420, () => {
        this.append(peer, "tool.result", {
          toolCallId: toolA,
          toolName: "context.read",
          content: [{ type: "text", text: "loaded 2 files" }],
          isError: false,
          durationMs: 160,
          timestamp: Date.now(),
        });
      });
      this.schedule(peer, 460, () => {
        this.append(peer, "tool.result", {
          toolCallId: toolB,
          toolName: "workspace.list",
          content: [{ type: "text", text: "src/  web/  docs/" }],
          isError: false,
          durationMs: 90,
          timestamp: Date.now(),
        });
      });
      start = 1200;
    }
    const full = withTools
      ? "已检查当前工作区：src/、web/、docs/ 均可读，没有阻塞项。\n\n下一步建议：\n1. 先跑一遍 web 构建；\n2. 再用真实服务验证审批与停止；\n3. 最后补发布说明。"
      : `已收到：「${prompt}」。这是开发态本地脚本传输的流式回复——\n\n- 会话、工具卡、审批与停止都可以操作；\n- 打开「partial」可以看 token 级增量；\n- 接入真实服务后，同一份界面直接消费 Harness API。`;
    const chunks = full.match(/.{1,12}/gs) ?? [full];
    chunks.forEach((_, index) => {
      this.schedule(peer, start + index * 70, () => {
        this.partial(peer, chunks.slice(0, index + 1).join(""), `live-${inputSeq}`);
      });
    });
    const end = start + chunks.length * 70 + 120;
    this.schedule(peer, end, () => {
      this.append(peer, "assistant.message", {
        role: "assistant",
        content: [{ type: "text", text: full }],
        inputSeqs: [inputSeq],
        stopReason: "end_turn",
        timestamp: Date.now(),
      });
      this.finish(peer, "done");
    });
  }

  private finish(peer: Peer, stopReason: string) {
    peer.running = false;
    peer.meta.runStatus = stopReason === "done" ? "done" : stopReason;
    this.append(peer, "run.finished", {
      turns: 1,
      tokensIn: 640,
      tokensOut: 180,
      stopReason,
      durationMs: 2400,
      provider: "s2api",
      model: "deepseek-v4.1-flash",
      timestamp: Date.now(),
    });
    if (peer.queue.length > 0 && !peer.drainPaused) {
      const next = peer.queue.shift()!;
      this.reply(peer, next.seq, next.text);
    }
  }

  private schedule(peer: Peer, ms: number, fn: () => void) {
    const timer = setTimeout(() => {
      peer.timers = peer.timers.filter((item) => item !== timer);
      try {
        fn();
      } catch (error) {
        console.warn("fixture script failed", error);
      }
    }, ms) as unknown as number;
    peer.timers.push(timer);
  }

  private clearTimers(peer: Peer) {
    for (const timer of peer.timers) clearTimeout(timer);
    peer.timers = [];
  }
}
