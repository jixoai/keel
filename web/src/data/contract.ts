// Keel UI ↔ headless contract.
//
// The single source of truth is `packages/chat-surface/src/types.ts` (frozen;
// another agent owns its implementation). This module only re-exports the
// types so every screen imports them from one place, and adds the few
// UI-side shapes the contract does not cover (session list rows, attachment
// chips, the extended command set). None of this changes the frozen contract.
export type {
  ConversationView,
  ConnectionState,
  DecisionView,
  Delivery,
  EventSource,
  InputView,
  QueueItemView,
  RunSummary,
  SurfaceCommands,
  SurfaceEngine,
  ToolCallView,
  TurnOrigin,
  TurnStatus,
  TurnView,
  WireEvent,
  WorkSegment,
} from "../../../packages/chat-surface/src/types";

import type { Delivery, SurfaceCommands, WireEvent } from "../../../packages/chat-surface/src/types";

/** A row of `GET /v1/sessions`. */
export interface SessionSummary {
  sessionId: string;
  createdAt?: string;
  updatedAt?: string;
  lastSeq?: number;
  runStatus?: string;
  lastMessageAt?: string;
  /** Optional enrichments (the API may add them; the client falls back to
   * loading the last events of a session). */
  title?: string;
  lastMessage?: string;
  /** The session's newest turn is parked on an open decision. Derived from the
   * ledger (the tail's last decision event), so the list badge and the
   * conversation agree on one fact. */
  pendingDecision?: boolean;
  /** The account named by the session's owner relation (the Bot's Account, or
   * the creator for an account-owned session). */
  ownerAccountId?: string;
  /** The initiator, which may differ from the owner. */
  createdByAccountId?: string;
  /** The owner's display name, for 「由 X 共享」. */
  ownerDisplayName?: string;
  /** The owner has shared this session with at least one viewer/editor. */
  shared?: boolean;
  /** This session is visible to the caller through a share (its owner is
   * another account). Computed by the server against the caller. */
  sharedWithMe?: boolean;
}

/** A failed turn the composer's error banner is showing (change
 * keel-workspace-polish C6). `retryable` is false when there is no committed
 * input to re-ask (a send that never reached the ledger). */
export interface TurnErrorState {
  sessionId: string;
  turnId?: string;
  text: string;
  retryable: boolean;
}

/** A conversation's scroll memory: the reader's intent (following the tail or
 * reading somewhere above) plus the offset the intent was last seen at. */
export interface ScrollState {
  intent: "following" | "detached";
  offset: number;
}

/** The answer to `POST /v1/sessions/{id}/retry`. */
export interface RetryResult {
  created: boolean;
  seq: number;
  requestId: string;
}

/** An attachment shown in the conversation (uploaded by the composer). */
export interface AttachmentRef {
  fileId?: string;
  name: string;
  mime?: string;
  size?: number;
  /** A blob: or https: URL, when the platform can show it. */
  url?: string;
}

/** One model identity: the tenant default selection, or a session's own input
 * fact. It carries no endpoint or credential data — those are resolved from
 * the tenant's configuration. */
export interface ModelSelection {
  providerId?: string;
  modelId: string;
  reasoningLevel?: string;
  /** The surface this selection opens (`keel-session-surfaces-v1`). The server
   * resolves it from the provider descriptor when absent; the client carries it
   * so a newly registered provider can render before a session starts. */
  surfaceKind?: string;
  /** The execution binding: the Bot this input runs as. The server resolves its
   * system prompt, tool grants and default model; the client never invents one.
   * The old `agentId` token is refused by the server (A4.2b3a). */
  botId?: string;
  /** The TeamBot this input runs as: the server resolves the captain and the
   * frozen roster into the run. A selection naming both ids is a team fact —
   * the team decides the captain. The old `teamId` token is refused by the
   * server (A4.2b3a). */
  teamBotId?: string;
}

/** What the composer sends: the contract's `send` plus attachments and the
 * session's model selection (both UI-side extensions; the server accepts the
 * `selection` object on `POST /v1/sessions/{id}/input`). */
export interface SendOptions {
  requestId: string;
  delivery: Delivery;
  attachments?: AttachmentRef[];
  /** The session's model selection for this input; absent = tenant default. */
  selection?: ModelSelection;
}

export interface UiCommands extends SurfaceCommands {
  sendWithAttachments?(text: string, opts: SendOptions): Promise<void>;
}

// ---- settings face (change keel-model-settings) --------------------------

/** One configuration diagnostic, tied to a dotted field path. */
export interface SettingsIssue {
  field: string;
  message: string;
}

export interface ProviderApiView {
  type?: string;
  baseUrl?: string;
  headers?: Record<string, string>;
}

/** A credential reference: `env` is a variable NAME, `ref` a tenant secret
 * reference (`secret:NAME`). Neither is ever a credential value. */
export interface ProviderAccessView {
  type?: string;
  env?: string;
  ref?: string;
}

export interface MediaFormatView {
  text?: boolean;
  image?: boolean;
  video?: boolean;
  audio?: boolean;
  pdf?: boolean;
}

/** A model's capability fact sheet (`properties`). */
export interface ModelPropertiesView {
  contextWindow?: number;
  inputFormat?: MediaFormatView;
  outputFormat?: MediaFormatView;
  supportsToolCall?: boolean;
  supportsJsonSchemaOutput?: boolean;
  supportsNativeWebSearch?: boolean;
}

export interface ModelOptionSpecsView {
  reasoningLevel?: { values?: string[]; map?: Record<string, Record<string, unknown>> };
  maxOutputTokens?: { max?: number; map?: Record<string, Record<string, unknown>> };
}

/** One resolved model (`GET /v1/models`, and each provider's `models`). */
export interface ModelView {
  providerId: string;
  modelId: string;
  /** The provider family this model belongs to (`llm` / `realtime` / `image`
   * / `video` / `tts` / `stt`), resolved from the provider document. */
  providerType?: string;
  /** The session surface this model opens. It overrides the client's inference
   * when a provider plugin ships its own descriptor. */
  surfaceKind?: string;
  api?: ProviderApiView;
  properties?: ModelPropertiesView;
  options?: ModelOptionSpecsView;
  enabled?: boolean;
  executable?: boolean;
  selectable?: boolean;
  /** No rule named this model: its facts are a baseline heuristic. */
  baseline?: boolean;
  issues?: SettingsIssue[];
}

/** One resolved provider (`GET /v1/providers`). */
export interface ProviderView {
  providerId: string;
  name?: string;
  /** `llm` / `realtime` / `image` / `video` / `tts` / `stt`; absent means the
   * server's legacy text provider. */
  providerType?: string;
  surfaceKind?: string;
  visibility?: string;
  api?: ProviderApiView;
  access?: ProviderAccessView;
  models?: ModelView[];
  issues?: SettingsIssue[];
  executable?: boolean;
  selectable?: boolean;
}

export interface ProvidersResponse {
  providers?: ProviderView[];
  issues?: SettingsIssue[];
}

export interface ModelsResponse {
  models?: ModelView[];
  issues?: SettingsIssue[];
  enforcement?: { toolCall?: boolean; media?: boolean };
}

/** `GET /v1/settings/models` — the effective models document. */
export interface ModelsSettingsView {
  revision: number;
  source: "stored" | "deployment";
  /** The effective models document. The client treats it as opaque JSON (the
   * raw-document editor shows it verbatim); only `selection` is read. */
  document: Record<string, unknown>;
  issues?: SettingsIssue[];
}

/** `PUT /v1/settings/models`'s 200 summary. */
export interface ModelsSettingsSaved {
  revision: number;
  source?: string;
  selection?: ModelSelection;
  providersExecutable?: number;
  modelsSelectable?: number;
}

/** `DELETE /v1/settings/models`'s 200 body. */
export interface ModelsSettingsReset {
  revision: number;
  source: "deployment";
}

/** `GET /v1/settings/secrets` — names and times only, never values. */
export interface SecretMeta {
  name: string;
  createdAt?: string;
  updatedAt?: string;
}

export interface SecretsView {
  secrets?: SecretMeta[];
  /** False when the process has no secret box (KEEL_SECRET_KEY): the server's
   * reason is shown and writing is disabled — never a plaintext fallback. */
  available: boolean;
  reason?: string;
}

export interface SecretSaved {
  name: string;
  updatedAt?: string;
}

// ---- skill library (change keel-harness-v2, W7 skills) ---------------------

/** Where a skill came from. `trusted`/`untrusted` are subscription sources;
 * `local` is a skill authored in this deployment (an agent's skills.manage
 * create, /learn, a manual install). */
export type SkillTrust = "trusted" | "untrusted" | "local";

/** The update conflict codes the server publishes. They are rendered as a
 * badge and a sentence, never collapsed into "error". */
export type SkillConflict =
  | "pinned"
  | "modified"
  | "source_missing"
  | "name_mismatch"
  | "missing_active";

/** One immutable bundle version. */
export interface SkillVersionView {
  hash: string;
  createdAt?: string;
  files?: number;
  bytes?: number;
}

/** One installed skill. */
export interface SkillRecordView {
  name: string;
  description?: string;
  trust: SkillTrust | string;
  sourceId?: string;
  sourceKind?: string;
  sourceUrl?: string;
  currentHash: string;
  pinnedHash?: string;
  installedAt?: string;
  updatedAt?: string;
  versions?: SkillVersionView[];
  path?: string;
  fileCount?: number;
  bytes?: number;
  updateAvailable?: boolean;
  availableHash?: string;
  conflict?: SkillConflict | string;
  diagnostics?: string[];
}

/** A subscription source. Only `local` is syncable in this build. */
export interface SkillSourceView {
  id: string;
  kind: string;
  url: string;
  trust: SkillTrust | string;
  createdAt?: string;
}

/** `GET /v1/settings/skills`. `checked` says whether the sources were read
 * (the update/conflict fields are only meaningful then). */
export interface SkillsSettingsView {
  revision: number;
  root?: string;
  checked?: boolean;
  skills?: SkillRecordView[];
  sources?: SkillSourceView[];
  limits?: Record<string, number>;
  policy?: {
    trustValues?: string[];
    sourceKinds?: string[];
    maxFiles?: number;
    maxBytes?: number;
    maxFileBytes?: number;
    scriptExecution?: string;
  };
}

/** One file of a skill bundle. */
export interface SkillFileView {
  path: string;
  size: number;
  hash?: string;
  mode?: number;
}

/** `GET /v1/settings/skills/{name}`. */
export interface SkillDetailView extends SkillRecordView {
  body?: string;
  files?: SkillFileView[];
}

/** `POST /v1/settings/skills/{name}/update`'s body. */
export interface SkillUpdateResult {
  status: "updated" | "up_to_date" | "pinned" | string;
  record?: SkillRecordView;
  previousHash?: string;
  availableHash?: string;
  pinnedHash?: string;
  updateAvailable?: boolean;
}

/** One in-memory bundle file on the wire. */
export interface SkillFileInput {
  path: string;
  content?: string;
  contentBase64?: string;
  mode?: number;
}

export interface SkillInstallRequest {
  name: string;
  description?: string;
  body?: string;
  files?: SkillFileInput[];
  sourceId?: string;
  trust?: SkillTrust | string;
  allowUntrusted?: boolean;
  pin?: boolean;
}

export interface SkillUpdateRequest {
  files?: SkillFileInput[];
  description?: string;
  body?: string;
  sourceId?: string;
  allowUntrusted?: boolean;
}

/** One file row of `GET /v1/sessions/{id}/files` (the `$` file group). */
export interface SessionFileView {
  path: string;
  size: number;
  updatedSeq: number;
  lastTaskId?: string;
}

export interface SessionFilesView {
  files?: SessionFileView[];
  count?: number;
  truncated?: boolean;
}

/** One task-card row of `GET /v1/sessions/{id}/tasks` (the `$` task group). */
export interface SessionTaskView {
  taskId: string;
  parentTaskId?: string;
  title: string;
  status: string;
  updatedAt?: string;
}

export interface SessionTasksView {
  tasks?: SessionTaskView[];
  count?: number;
  truncated?: boolean;
}

/** One card of `GET /v1/sessions/{id}/task-panel`. `member` is true for a
 * card whose row lives in another session (a sub-agent or a fork). */
export interface TaskPanelCardView {
  taskId: string;
  parentTaskId?: string;
  sessionId: string;
  title: string;
  status: string;
  origin?: string;
  originSeq?: number;
  createdSeq?: number;
  updatedSeq?: number;
  closedSeq?: number;
  createdAt?: string;
  updatedAt?: string;
  member?: boolean;
}

/** One task event of the panel's stream. A field the event's kind did not
 * carry is absent; `unparsed` marks a kind this build does not decode. */
export interface TaskPanelEventView {
  taskId: string;
  kind: string;
  seq: number;
  at?: string;
  title?: string;
  status?: string;
  parentTaskId?: string;
  origin?: string;
  originSeq?: number;
  path?: string;
  hash?: string;
  text?: string;
  reason?: string;
  unparsed?: boolean;
}

/** One live (path, hash) reference a card carries. The bytes stay in the file
 * layer; a preview resolves it by path through the session's manifest. */
export interface TaskPanelAttachmentView {
  taskId: string;
  path: string;
  hash: string;
}

export interface TaskPanelLimitsView {
  memberLimit: number;
  truncated?: boolean;
  truncatedMemberCount?: number;
  eventLimit: number;
  eventsTruncated?: boolean;
  ancestorsTruncated?: boolean;
}

export interface TaskPanelView {
  sessionId: string;
  cards?: TaskPanelCardView[];
  members?: TaskPanelCardView[];
  ancestors?: TaskPanelCardView[];
  attachments?: TaskPanelAttachmentView[];
  events?: TaskPanelEventView[];
  limits?: TaskPanelLimitsView;
}

/** One bounded page of `GET /v1/sessions/{id}/file-preview`. `hash` is the
 * manifest's current hash; `matchesReference` is false when the file changed
 * since the reference was recorded. */
export interface FilePreviewView {
  path: string;
  hash: string;
  size: number;
  seq?: number;
  offset: number;
  limit: number;
  totalLines: number;
  returnedLines: number;
  nextOffset: number;
  truncated?: boolean;
  content: string;
  referenceHash?: string;
  matchesReference?: boolean;
}

/** A trigger as the plugin manifest declares it. */
export interface TriggerSpecView {
  id: string;
  kind?: string;
  schedule?: string;
  prompt?: string;
  enabled?: boolean;
  owner?: string;
  maxAttempts?: number;
}

/** One node of a plugin's declared CLI capability tree (`provides.cli`): the
 * command below `keel`, its summary, the argument/output schemas the agent tool
 * face publishes, and the risk attributes --help and the settings page render. */
export interface CliCapabilityView {
  path: string[];
  summary: string;
  description?: string;
  args_schema?: Record<string, unknown>;
  output_schema?: Record<string, unknown>;
  permissions?: string[];
  danger?: string;
  idempotent?: boolean;
  callable_by?: string[];
  examples?: string[];
}

/** What one plugin declares it contributes (`provides`). The server sends the
 * manifest section verbatim; a client renders only the fields it knows. */
export interface PluginProvidesView {
  tools?: string[];
  hooks?: string[];
  triggers?: string[];
  cli?: CliCapabilityView[];
  agents?: string[];
  agent_teams?: string[];
  feature_ports?: string[];
  /** The slots this plugin declares (`provides.slots`). */
  slots?: unknown[];
}

/** One contribution a plugin makes to a slot (`contributes[]`). The server
 * sends the declaration; the client validates it before loading. */
export interface PluginContributionView {
  id?: string;
  slot: string;
  form: string;
  entry?: string;
  title?: string;
  description?: string;
  payload?: unknown;
  capabilities?: string[];
  targets?: string[];
  tool?: string;
  toolName?: string;
  part?: string;
  partKind?: string;
}

export interface PluginExtensibilityView {
  usable?: boolean;
  doc?: string;
  schema?: string;
  reason?: string;
}

export interface PluginStatusView {
  id: string;
  version?: string;
  state?: string;
  provides?: PluginProvidesView;
  requires?: string[];
  tools?: string[];
  hooks?: string[];
  triggers?: TriggerSpecView[];
  effects?: string[];
  diagnostics?: string[];
  startedAt?: string;
  stoppedAt?: string;
  /** `kernel|harness|provider|ui|service`, when the manifest declares it. */
  layer?: string;
  /** The slots this plugin declares (a tolerance alias of `provides.slots`). */
  slots?: unknown[];
  /** What this plugin contributes to other plugins' slots. */
  contributes?: PluginContributionView[];
  /** The machine-readable extension document's presence. */
  extensibility?: PluginExtensibilityView;
}

export interface HookStatsView {
  subscriptions?: number;
  emitted?: number;
  delivered?: number;
  dropped?: number;
  failed?: number;
  queued?: number;
  capacity?: number;
}

/** `GET /v1/plugins` — the assembled extension surface. */
export interface PluginsView {
  generation?: number;
  plugins?: PluginStatusView[];
  hooks?: HookStatsView;
  tools?: string[];
  hookStats?: HookStatsView;
}

export interface TriggerAuditView {
  at?: string;
  triggerId?: string;
  reason?: string;
  idempotencyKey?: string;
  sessionId?: string;
  seq?: number;
  status?: string;
  attempts?: number;
  error?: string;
}

/** One trigger's observable state (`GET /v1/triggers`). */
export interface TriggerStatusView extends TriggerSpecView {
  nextFireAt?: string;
  lastFireAt?: string;
  lastStatus?: string;
  fires?: number;
  failures?: number;
  audit?: TriggerAuditView[];
}

export interface TriggersView {
  triggers?: TriggerStatusView[];
}

/** `POST /v1/triggers/{id}/fire`'s audit result. */
export interface FireResultView {
  triggerId?: string;
  status?: string;
  idempotencyKey?: string;
  sessionId?: string;
  seq?: number;
  deduped?: boolean;
  attempts?: number;
  error?: string;
}

// ---- deployment capabilities (settings「能力」) ----------------------------

/** One media capability's status as this deployment reports it.
 *
 * `available` is a working configuration; `unconfigured` is the ordinary
 * "the operator has not set the environment yet" state (writable only by the
 * server's process); `unsupported` is the provider declining the capability
 * itself. The client never invents a status. */
export type MediaCapabilityStatus = "available" | "unconfigured" | "unsupported";

/** One row of `GET /v1/media/capabilities`. It carries facts an operator can
 * act on — a status, an optional provider reason, environment variable NAMES
 * and a model id — and never a URL, a key or a runtime error. */
export interface MediaCapabilityView {
  /** The operational id: `chat`, `image`, `image-edit`, `decision`, `video`,
   * `tts`, `stt` (plugin families may report others; the settings page only
   * renders the rows it knows). */
  id: string;
  status: MediaCapabilityStatus;
  /** Why a provider does not offer the capability. The transport sanitizes it
   * before it reaches the page: a URL, a credential or a Go error is dropped. */
  reason?: string;
  /** Environment variable NAMES that enable the capability (never values). */
  env?: string[];
  /** The model id this deployment routes the capability to, when resolved. */
  model?: string;
}

/** `GET /v1/media/capabilities` — what this deployment's media gateway can do.
 * A 503 from the endpoint means "the gateway itself is not configured" and is
 * reported as `configured: false`, not as an error. */
export interface MediaCapabilitiesView {
  configured: boolean;
  capabilities: MediaCapabilityView[];
  /** Gateway-level environment variable NAMES (base URL / credential). */
  env?: string[];
}

/** `GET /v1/realtime/status` — the voice bridge's preflight facts. The
 * provider URL and key stay on the server; only the protocol family and the
 * allowlisted model names cross the wire. */
export interface RealtimeStatusView {
  configured: boolean;
  /** The provider's display name (a protocol family, not an endpoint). */
  provider?: string;
  kind?: string;
  models: string[];
  inputSampleRate?: number;
  outputSampleRate?: number;
}

/** The deployment-capability probes the settings page reads. `KeelHttp`
 * implements it; the scripted fixture does not (these are facts of the live
 * process, not of the fixture). */
export interface DeploymentCapabilitiesApi {
  mediaCapabilities(): Promise<MediaCapabilitiesView>;
  realtimeStatus(): Promise<RealtimeStatusView>;
}

// ---- MCP Apps (change feat-mcp-apps-events, P2) ---------------------------

/** One tool that references an app resource (the catalogue's per-tool
 * visibility). The catalogue is the binding's single source: a client reads
 * this, never recomputes it from tool results. */
export interface McpAppToolRef {
  tool: string;
  visibility: string[];
}

/** One renderable app from `GET /v1/mcp/apps` (the server's `AppSpec`). The
 * document URL is immutable and hash-named. `csp` is the response header the
 * server will send; `cspDomains` is what the resource declared (for the
 * bridge's `hostCapabilities.sandbox`). */
export interface McpAppView {
  appId: string;
  server: string;
  title?: string;
  resourceUri: string;
  mimeType: string;
  visibility: string[];
  tools: McpAppToolRef[];
  csp: string;
  cspDomains?: {
    connectDomains?: string[];
    resourceDomains?: string[];
    frameDomains?: string[];
    baseUriDomains?: string[];
  };
  permissions: Record<string, boolean>;
  prefersBorder?: boolean;
  /** The resource's `_meta.ui.domain` (a bare hostname the server declared).
   * Validated by the host; the client passes it through for display. */
  domain?: string;
  hash: string;
  fetchedAt?: string;
  ttlMs?: number;
  diagnostics?: string[];
  documentUrl: string;
}

/** `GET /v1/mcp/apps`: the catalogue plus the two deployment inputs a View
 * needs (the optional second origin and the host theme palettes mapped onto
 * the apps standard variables). */
export interface McpAppCatalogueView {
  generation: number;
  apps: McpAppView[];
  diagnostics?: unknown[];
  sandboxOrigin?: string;
  theme?: Record<string, Record<string, string>>;
  limits?: Record<string, number>;
}

/** The `details.app` binding a `tool.result` carries (P1.4): the ledger alone
 * says which app a call belongs to, without the catalogue. */
export interface McpAppBinding {
  appId: string;
  resourceUri: string;
  visibility: string[];
}

/** The raw `tool.result` facts the app host needs. The frozen
 * `ToolCallView` keeps only one `result` value, so the core adapter indexes the
 * complete payload per call id: the View gets a real `CallToolResult`, and the
 * binding is read from `details.app` even when `result` is something else. */
export interface McpAppToolResult {
  content?: unknown;
  structuredContent?: unknown;
  isError?: boolean;
  /** The server's own `details` object (untrusted; only `app` is read). */
  details?: unknown;
}

/** One best-effort audit write for an app protocol violation. `detail` is a
 * bounded summary: no URL query, no message body, no credential (the server's
 * audit table shape is `mcp_app_audit`; P3.7 wires the durable store). */
export interface McpAppAuditEntry {
  appId: string;
  server?: string;
  sessionId?: string;
  action: "protocol.violation";
  result: "refused";
  detail: string;
}

/** A standard `CallToolResult` from the host's `tools/call` proxy. The View
 * receives exactly this object: content, structuredContent and isError, never
 * a server response header. */
export interface McpAppCallToolResult {
  content?: unknown;
  structuredContent?: unknown;
  isError?: boolean;
}

/** A `resources/read` result from the host's proxy (`{contents:[...]}` plus
 * the modern cache hints). */
export interface McpAppResourceReadResult {
  contents: unknown[];
  ttlMs?: number;
  cacheScope?: string;
}

/** One successful `ui/message` outcome. `send` reached the ledger, `edit`
 * only prefills the composer, `discard` wrote nothing. */
export interface McpAppMessageOutcome {
  result: "send" | "edit" | "discard";
  prefilled?: { text?: string };
}

/** One `ui/open-link` outcome: the host only opens an approved URL. */
export interface McpAppOpenLinkOutcome {
  opened: boolean;
  url?: string;
  reason?: string;
}

/** The settings face of a transport. Both `KeelHttp` and the fixture client
 * implement it, so the settings screens and the session picker work against
 * the same store in live and fixture mode. */
export interface SettingsApi {
  modelsSettings(): Promise<ModelsSettingsView>;
  saveModelsSettings(document: Record<string, unknown>, expectedRevision?: number): Promise<ModelsSettingsSaved>;
  resetModelsSettings(expectedRevision?: number): Promise<ModelsSettingsReset>;
  providers(): Promise<ProvidersResponse>;
  models(): Promise<ModelsResponse>;
  secrets(): Promise<SecretsView>;
  putSecret(name: string, value: string): Promise<SecretSaved>;
  deleteSecret(name: string): Promise<void>;
  skillsSettings(check?: boolean): Promise<SkillsSettingsView>;
  showSkill(name: string): Promise<SkillDetailView>;
  installSkill(request: SkillInstallRequest): Promise<SkillRecordView>;
  updateSkill(name: string, request?: SkillUpdateRequest): Promise<SkillUpdateResult>;
  pinSkill(name: string, hash?: string): Promise<SkillRecordView>;
  unpinSkill(name: string): Promise<SkillRecordView>;
  removeSkill(name: string): Promise<void>;
  subscribeSkillSource(source: { id: string; kind?: string; url: string; trust?: string }): Promise<SkillSourceView>;
  unsubscribeSkillSource(id: string): Promise<void>;
  plugins(): Promise<PluginsView>;
  triggers(): Promise<TriggersView>;
  fireTrigger(id: string, reason?: string): Promise<FireResultView>;
  /** Re-ask the input an earlier turn answered: a new user.message carrying
   * retryOf, plus a run. The ledger only grows. */
  retryTurn(sessionId: string, turnId: string): Promise<RetryResult>;
}

/** One member task card (`GET /v1/sessions/{id}/team-tasks`). The card is the
 * tasklog projection: title/status/time only. There is deliberately no result
 * or summary field — the UI must say "member task status", not invent a
 * member's result text. */
export interface TeamTaskView {
  taskId: string;
  parentTaskId?: string;
  sessionId: string;
  title: string;
  status: string;
  origin?: string;
  createdAt?: string;
  updatedAt?: string;
  closedSeq?: number;
}

/** One keyset page of a captain session's member task cards. */
export interface TeamTasksView {
  tasks: TeamTaskView[];
  nextCursor?: string;
  hasMore?: boolean;
}

/** The engine the UI runs against. `setConnection` is a client-side seam: the
 * headless core owns the view's connection state, but the transport learns
 * about it first. The local projection uses it; a future `@keel/chat-surface`
 * implementation can ignore it if it wires the transport itself. */
export interface UiEngine {
  view(): import("../../../packages/chat-surface/src/types").ConversationView;
  submit(text: string, delivery: Delivery, requestId: string, attachments?: AttachmentRef[]): void;
  apply(event: WireEvent, attachments?: AttachmentRef[]): void;
  applyPartial(partial: { text: string; turnId?: string }): void;
  hydrate(events: WireEvent[], attachmentIndex?: Map<string, AttachmentRef[]>): void;
  requestStop(): void;
  subscribe(fn: () => void): () => void;
  setConnection(status: import("../../../packages/chat-surface/src/types").ConnectionState["status"], lastSeq: number): void;
  setSessionId(sessionId: string): void;
  attachmentsFor(requestId: string): AttachmentRef[] | undefined;
  /** The complete `tool.result` payload a call produced (content/details/
   * structuredContent), indexed by the adapter because the frozen
   * `ToolCallView` keeps only one `result` value. `undefined` until it lands. */
  toolResultFor(toolCallId: string): McpAppToolResult | undefined;
  /** A decision's deadline (ms since epoch), when the event carried one. */
  decisionTimeout(decisionId: string): number | undefined;
  removeOptimistic(requestId: string): void;
  /** Undo a stop request the server refused (keeps the composer usable). */
  resetStopping(): void;
  /** Partial text for the turn currently streaming (UI-only). */
  partialText(): string | undefined;
  /** The turn the partial belongs to. */
  partialTurnId(): string | undefined;
}
