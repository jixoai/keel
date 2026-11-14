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
  AgentTeamsResponse,
  AgentTeamView,
  AgentTeamWriteRequest,
  AgentsResponse,
  AgentView,
  AgentWriteRequest,
  AttachmentRef,
  ConnectionState,
  Delivery,
  EventSource,
  FireResultView,
  ModelOptionSpecsView,
  ModelsResponse,
  ModelsSettingsReset,
  ModelsSettingsSaved,
  ModelsSettingsView,
  ModelSelection,
  ModelView,
  PluginsView,
  ProviderView,
  ProvidersResponse,
  RetryResult,
  SecretMeta,
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
import { KeelApiError } from "./http";
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

interface DemoModelResolution {
  providerId: string;
  providerName: string;
  modelId: string;
  credential: string;
  credentialKind: "env" | "ref";
  executable: boolean;
}

const nowISO = () => new Date().toISOString();

function textPreview(payload: unknown): string {
  if (!payload || typeof payload !== "object") return "";
  const row = payload as { text?: string; summary?: string; question?: string };
  const text = row.text ?? row.summary ?? row.question ?? "";
  return text.replace(/\s+/g, " ").slice(0, 90);
}

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
  /** Secret names and times only — the demo never keeps a value either. */
  private secretState: SecretMeta[] = [
    { name: "s2api", createdAt: new Date(Date.now() - 86_400_000).toISOString(), updatedAt: new Date(Date.now() - 3_600_000).toISOString() },
    { name: "webhook-token", createdAt: new Date(Date.now() - 172_800_000).toISOString(), updatedAt: new Date(Date.now() - 172_800_000).toISOString() },
  ];
  private triggerFires = 12;
  /** The demo agent registry (the settings Agents section) — editable in the
   * fixture so the demo's create/edit/delete paths are real, not stubs. */
  private agentState: AgentView[] = [
    {
      id: "agent_demo_design",
      name: "设计师",
      description: "负责界面与品牌视觉",
      avatar: { kind: "identicon", seed: "agent_demo_design" },
      defaultModel: { providerId: "s2api", modelId: "deepseek-v4.1-flash" },
      tools: [{ toolId: "workspace.read", enabled: true }],
      systemPrompt: { kind: "inline", text: "你是资深视觉设计师。" },
    },
    {
      id: "agent_demo_research",
      name: "研究员",
      description: "负责资料检索与整理",
      avatar: { kind: "identicon", seed: "agent_demo_research" },
      defaultModel: { providerId: "s2api", modelId: "deepseek-v4.1-flash" },
      tools: [{ toolId: "web.fetch", enabled: true }],
    },
  ];
  private teamState: AgentTeamView[] = [
    {
      id: "team_demo_brand",
      name: "品牌发布小组",
      description: "负责新品发布的全链路交付",
      captain: { agentId: "agent_demo_design" },
      members: [{ agentId: "agent_demo_research" }],
      status: "valid",
      revision: 1,
      hash: "demo-brand",
    },
  ];
  private agentSequence = 0;
  private teamSequence = 0;

  constructor(private readonly imageURL: string = demoShot.url) {
    this.seed();
  }

  async probe(): Promise<"ok" | "auth" | "down"> {
    return "ok";
  }

  async capabilities(): Promise<Record<string, unknown>> {
    return {
      version: "fixture",
      capabilities: { "sessions.list": true, decisions: true, partials: true },
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
      models: resolution.models,
      issues: [],
      enforcement: { toolCall: true, media: false },
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
    const existing = this.secretState.find((item) => item.name === trimmed);
    if (existing) existing.updatedAt = at;
    else this.secretState.push({ name: trimmed, createdAt: at, updatedAt: at });
    this.secretState.sort((a, b) => a.name.localeCompare(b.name));
    return { name: trimmed, updatedAt: at };
  }

  async deleteSecret(name: string): Promise<void> {
    if (!this.secretState.some((item) => item.name === name)) {
      throw new KeelApiError(404, `secret ${name} was not found`);
    }
    this.secretState = this.secretState.filter((item) => item.name !== name);
  }

  async plugins(): Promise<PluginsView> {
    const started = new Date(Date.now() - 3_600_000).toISOString();
    return {
      generation: 3,
      plugins: [
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
      ],
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

  // ---- agent & team registry (demo) --------------------------------------
  //
  // The same shapes the HTTP DTOs declare, kept in memory: the demo's Agents
  // and 团队 sections create, edit and delete for real, and the captain /
  // reference rules answer with the same readable refusals the server does.

  async listAgents(): Promise<AgentsResponse> {
    return { agents: this.agentState.map((agent) => ({ ...agent })) };
  }

  async createAgent(request: AgentWriteRequest): Promise<AgentView> {
    if (!request.name.trim()) throw new KeelApiError(400, "Agent 名字不能为空");
    const created: AgentView = {
      id: request.id || `agent_demo_new_${++this.agentSequence}`,
      name: request.name,
      description: request.description,
      avatar: request.avatar,
      defaultModel: request.defaultModel,
      tools: request.tools,
      systemPrompt: request.systemPrompt,
      skills: request.skills,
    };
    this.agentState = [...this.agentState, created];
    return created;
  }

  async updateAgent(id: string, request: AgentWriteRequest): Promise<AgentView> {
    const index = this.agentState.findIndex((agent) => agent.id === id);
    if (index < 0) throw new KeelApiError(404, `Agent ${id} 不存在`);
    const updated: AgentView = {
      ...this.agentState[index],
      id,
      name: request.name,
      description: request.description,
      avatar: request.avatar,
      defaultModel: request.defaultModel,
      tools: request.tools,
      systemPrompt: request.systemPrompt,
      skills: request.skills,
    };
    this.agentState = this.agentState.map((agent, at) => (at === index ? updated : agent));
    return updated;
  }

  async deleteAgent(id: string): Promise<void> {
    const referencing = this.teamState.find(
      (team) =>
        team.captain.agentId === id || (team.members ?? []).some((member) => member.agentId === id),
    );
    if (referencing) {
      throw new KeelApiError(409, `Agent ${id} 正被团队「${referencing.name}」引用，先移出团队再删除。`);
    }
    if (!this.agentState.some((agent) => agent.id === id)) throw new KeelApiError(404, `Agent ${id} 不存在`);
    this.agentState = this.agentState.filter((agent) => agent.id !== id);
  }

  async listTeams(): Promise<AgentTeamsResponse> {
    return { teams: this.teamState.map((team) => ({ ...team })) };
  }

  /** The demo's team rules: a captain is mandatory, exists, and is never a
   * member; the captain seat itself is not removable. */
  private teamViewOf(id: string, request: AgentTeamWriteRequest): AgentTeamView {
    if (!request.captain?.agentId) throw new KeelApiError(400, "团队必须有队长");
    if (!this.agentState.some((agent) => agent.id === request.captain.agentId)) {
      throw new KeelApiError(400, "队长必须是已存在的 Agent");
    }
    return {
      id,
      name: request.name,
      description: request.description,
      captain: { agentId: request.captain.agentId },
      members: (request.members ?? []).filter(
        (member) => member.agentId && member.agentId !== request.captain.agentId,
      ),
      defaults: request.defaults,
      capabilities: request.capabilities,
      featureBindings: request.featureBindings,
      status: "valid",
      revision: 1,
    };
  }

  async createTeam(request: AgentTeamWriteRequest): Promise<AgentTeamView> {
    if (!request.name.trim()) throw new KeelApiError(400, "团队名字不能为空");
    const created = this.teamViewOf(request.id || `team_demo_new_${++this.teamSequence}`, request);
    this.teamState = [...this.teamState, created];
    return created;
  }

  async updateTeam(id: string, request: AgentTeamWriteRequest): Promise<AgentTeamView> {
    if (!this.teamState.some((team) => team.id === id)) throw new KeelApiError(404, `团队 ${id} 不存在`);
    const updated = this.teamViewOf(id, request);
    this.teamState = this.teamState.map((team) => (team.id === id ? updated : team));
    return updated;
  }

  async deleteTeam(id: string): Promise<void> {
    if (!this.teamState.some((team) => team.id === id)) throw new KeelApiError(404, `团队 ${id} 不存在`);
    this.teamState = this.teamState.filter((team) => team.id !== id);
  }

  private mutateTeam(id: string, mutate: (team: AgentTeamView) => AgentTeamView): AgentTeamView {
    const team = this.teamState.find((item) => item.id === id);
    if (!team) throw new KeelApiError(404, `团队 ${id} 不存在`);
    const next = mutate(team);
    this.teamState = this.teamState.map((item) => (item.id === id ? next : item));
    return next;
  }

  async addTeamMember(id: string, agentId: string): Promise<AgentTeamView> {
    if (!this.agentState.some((agent) => agent.id === agentId)) {
      throw new KeelApiError(400, `Agent ${agentId} 不存在`);
    }
    return this.mutateTeam(id, (team) => {
      if (team.captain.agentId === agentId) {
        throw new KeelApiError(400, "队长已经在队内，不能作为普通成员重复添加");
      }
      const members = team.members ?? [];
      if (members.some((member) => member.agentId === agentId)) return team;
      return { ...team, members: [...members, { agentId }] };
    });
  }

  async removeTeamMember(id: string, agentId: string): Promise<AgentTeamView> {
    return this.mutateTeam(id, (team) => {
      if (team.captain.agentId === agentId) {
        throw new KeelApiError(400, "队长不可移除：请先改派队长");
      }
      return { ...team, members: (team.members ?? []).filter((member) => member.agentId !== agentId) };
    });
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
        const caps = demoModelCapabilities(modelId);
        const model: ModelView = {
          providerId,
          modelId,
          api: asRecord(provider.api) as ModelView["api"],
          properties: {
            contextWindow: caps.contextWindow,
            inputFormat: { text: true, image: caps.image, video: false, audio: false, pdf: false },
            outputFormat: { text: true },
            supportsToolCall: true,
            supportsJsonSchemaOutput: true,
            supportsNativeWebSearch: false,
          },
          options: demoOptions(modelId),
          enabled: true,
          executable: resolvable,
          selectable: resolvable,
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
        selectable: resolvable && providerModels.length > 0,
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
