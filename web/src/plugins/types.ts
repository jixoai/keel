// Plugin UI slots (change keel-harness-v2, W12): the shared vocabulary.
//
// A slot is declared by a plugin (`provides.slots`), a contribution is what a
// plugin puts into somebody else's slot (`contributes`), and the four forms
// are the only ways a contribution runs:
//
//   chunk    NSH dynamic import, same VM, rendered in the host tree
//   package  a signed NSH package in its own surface/VM (HostLimits)
//   iframe   a hosted page in a browser-level frame (explicit fallback)
//   data     JSON the host consumes (a nav item, an action), no code
//
// These types are the client's projection of the documented manifest shape
// (`layer` / `provides.slots` / `contributes` / `extensibility.json`). The
// registry keeps unknown optional fields out of its way, so the W0.5 contract
// can grow without a client migration; the fields it does read are validated
// strictly.
import type { JSX } from "nsh";

/** The four contribution forms, closed. */
export type SlotForm = "chunk" | "package" | "iframe" | "data";

/** How many contributions a slot accepts. */
export type SlotArity = "single" | "multi" | "ordered";

/** The targets a slot/contribution can name. `native` covers every packaged
 * NSH target (macos/windows/linux/ios/android); `web` the browser. */
export type SlotTarget = "web" | "native" | "tui";

export const SLOT_FORMS: readonly SlotForm[] = ["chunk", "package", "iframe", "data"];
export const SLOT_ARITIES: readonly SlotArity[] = ["single", "multi", "ordered"];
export const SLOT_TARGETS: readonly SlotTarget[] = ["web", "native", "tui"];

/** The bridge verbs a contribution may be granted (`keel.plugin.*`). A
 * contribution without a grant gets `state` only; `callTool` also needs the
 * tool to be declared in `provides.tools`. */
export type BridgeCapability = "state" | "call" | "approve" | "subscribe";
export const BRIDGE_CAPABILITIES: readonly BridgeCapability[] = ["state", "call", "approve", "subscribe"];

/** One declared slot. */
export interface SlotDeclaration {
  /** Namespaced id (`<plugin>.<name>`). */
  id: string;
  /** The plugin that declared it. */
  provider: string;
  forms: SlotForm[];
  arity: SlotArity;
  targets: SlotTarget[];
  /** The JSON schema a `data` contribution is validated against. */
  schema?: Record<string, unknown>;
  /** The lifecycle hooks the slot owner asked for (accepted, not executed by
   * the client yet; kept so a declaration round-trips). */
  lifecycle?: string[];
  /** Where the declaration came from: the first-party client plugin's built-in
   * declarations, or a manifest. */
  builtin?: boolean;
}

/** One contribution as declared, before it loads. */
export interface SlotContribution {
  /** Stable contribution id (`<plugin>.<slot>.<name>` by convention). */
  id: string;
  pluginId: string;
  slot: string;
  form: SlotForm;
  title?: string;
  description?: string;
  /** `chunk`/`package`/`iframe` entrypoint; absent for `data`. */
  entry?: string;
  /** The host-consumed JSON of a `data` contribution. */
  payload?: unknown;
  /** Narrowing of the slot's target list. */
  targets: SlotTarget[];
  /** The bridge verbs this contribution is granted. */
  capabilities: BridgeCapability[];
  /** Only this tool's calls use a `tool-renderer` contribution. */
  toolName?: string;
  /** Only this part kind uses a `message-part` contribution. An attachment
   * renderer MUST declare `partKind: "attachment"`; a contribution without a
   * part kind is not an attachment renderer (it stays a wildcard only for the
   * host's non-attachment part kinds). */
  partKind?: string;
  /** Attachment-only MIME narrowing: `type/subtype` or `type/*`, lowercase.
   * Absent/empty means "every attachment MIME" for an attachment claim. */
  mime?: string[];
  /** The contribution's index in its plugin's `contributes[]`, recorded by
   * validation. Selection uses it (with the inventory's plugin order) when a
   * re-registration moves the mounted entry in the client's array. */
  order?: number;
}

export type ContributionStatus = "declared" | "loading" | "ready" | "failed" | "unavailable";

/** The runtime face of one contribution: what the slot host renders. */
export interface MountedContribution {
  declaration: SlotContribution;
  status: ContributionStatus;
  /** Why it is unavailable/failed (field-level when validation failed). */
  reason?: string;
  /** The stable error code (`ERR_*`). */
  code?: string;
  /** The loaded module (chunk form, ready). */
  module?: PluginChunkModule;
}

/** A chunk's public contract: `render` returns the host-tree vnode. The host
 * calls it inside a guarded getter, so a throwing renderer shows a failure
 * state and leaves the rest of the page alone. */
export interface PluginChunkContext {
  pluginId: string;
  contribution: SlotContribution;
  slot: SlotDeclaration;
  target: SlotTarget;
  /** The scoped `keel.plugin.*` bridge (capability-filtered). */
  bridge: PluginBridge;
  /** Whatever the host's slot passes (a tool call, a message part, ...). */
  props?: unknown;
}

export interface PluginChunkModule {
  render(context: PluginChunkContext): JSX.Element | null;
  /** Optional activation for a nav/action contribution (a palette row or an
   * action button). */
  activate?(context: PluginChunkContext): void | Promise<void>;
  /** Optional cleanup, run before the contribution is withdrawn. */
  unload?(): void;
}

/** The scoped bridge a contribution sees. Every method fails with a typed
 * `PluginFault`; nothing here can reach another plugin's tools or events. */
export interface PluginBridge {
  readonly pluginId: string;
  state(): Promise<PluginStateView>;
  callTool(tool: string, args?: unknown): Promise<unknown>;
  requestApproval(request: ApprovalRequest): Promise<ApprovalView>;
  onEvent(listener: (event: PluginEvent) => void): () => void;
  /** The grants this bridge was opened with (never a host secret). */
  readonly capabilities: readonly BridgeCapability[];
  /** Withdraw the plugin's event stream and every listener. Called when the
   * plugin is disabled/uninstalled or its declaration is replaced; it must not
   * leave a listener, a host subscription or a cached scope behind. */
  dispose?(): void;
}

/** The bridge's state document (a projection of the plugin's inventory row and
 * the host theme). */
export interface PluginStateView {
  id: string;
  version?: string;
  kind?: string;
  description?: string;
  state?: string;
  bundled?: boolean;
  provides?: Record<string, unknown>;
  tools?: string[];
  hooks?: string[];
  triggers?: string[];
  diagnostics?: string[];
  layer?: string;
  slots?: unknown[];
  contributes?: unknown[];
  extensibility?: { usable?: boolean; doc?: string; schema?: string; reason?: string };
  theme?: unknown;
  bridge?: { call?: boolean; approve?: boolean; subscribe?: boolean };
}

export interface ApprovalRequest {
  requestId?: string;
  prompt: string;
  options?: { id: string; label?: string; description?: string; recommended?: boolean }[];
  allowFreeText?: boolean;
  timeoutMs?: number;
  waitMs?: number;
}

export interface ApprovalView {
  decision?: { decisionId?: string; status?: string; answer?: unknown };
  status?: string;
  approvalUrl?: string;
}

export interface PluginEvent {
  pluginId: string;
  event: string;
  payload?: unknown;
}

/** What an attachment preview contribution receives (`props`). It is the
 * canonical read-only projection plus the host's display facts; a renderer
 * never sees bytes, base64, a CAS path or a tenant header. */
export interface AttachmentPreviewProps {
  part: import("../data/contract").AttachmentPart;
  /** The host's target, repeated for a renderer that draws differently. */
  target: SlotTarget;
  /** `true` when the host had to draw the default attachment line under this
   * preview (the renderer never hides it). */
  withDefaultRow: true;
}

/** One field-level validation finding. `warning` does not reject; `error`
 * fails the declaration/contribution closed. */
export interface SlotIssue {
  severity: "error" | "warning";
  code: string;
  pluginId: string;
  field: string;
  message: string;
}

/** The resolved slot view a host component draws. */
export interface SlotView {
  slot: SlotDeclaration;
  contributions: MountedContribution[];
  /** Contributions that exist but cannot run on this target (explicit). */
  unavailable: MountedContribution[];
  /** True when the slot itself cannot run on this target. */
  unsupported?: { code: string; reason: string };
}
