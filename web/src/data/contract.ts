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

export interface PluginStatusView {
  id: string;
  version?: string;
  state?: string;
  provides?: string[];
  requires?: string[];
  tools?: string[];
  hooks?: string[];
  triggers?: TriggerSpecView[];
  effects?: string[];
  diagnostics?: string[];
  startedAt?: string;
  stoppedAt?: string;
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

/** The settings face of a transport. Both `KeelHttp` and the fixture client
 * implement it, so the settings screens and the session picker work against
 * the same store in live and demo mode. */
export interface SettingsApi {
  modelsSettings(): Promise<ModelsSettingsView>;
  saveModelsSettings(document: Record<string, unknown>, expectedRevision?: number): Promise<ModelsSettingsSaved>;
  resetModelsSettings(expectedRevision?: number): Promise<ModelsSettingsReset>;
  providers(): Promise<ProvidersResponse>;
  models(): Promise<ModelsResponse>;
  secrets(): Promise<SecretsView>;
  putSecret(name: string, value: string): Promise<SecretSaved>;
  deleteSecret(name: string): Promise<void>;
  plugins(): Promise<PluginsView>;
  triggers(): Promise<TriggersView>;
  fireTrigger(id: string, reason?: string): Promise<FireResultView>;
  /** Re-ask the input an earlier turn answered: a new user.message carrying
   * retryOf, plus a run. The ledger only grows. */
  retryTurn(sessionId: string, turnId: string): Promise<RetryResult>;
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
