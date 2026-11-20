// MCP Apps data shapes (change feat-mcp-apps-events, P2).
//
// Pure: no NSH, no store, no fetch. The catalogue parser is strict on the
// security-bearing fields (derived appId, content hash, document URL, media
// type) and tolerant of additive server fields. The binding reader accepts the
// server's `details` object; it never trusts a server-provided URL.
import type {
  McpAppBinding,
  McpAppCatalogueView,
  McpAppToolResult,
  McpAppToolRef,
  McpAppView,
} from "./contract";

/** The only app identity shape the host derives (`mcpapp_` + 16 lowercase
 * hex). Anything else is a client bug or a probe and is not rendered. */
export const APP_ID_PATTERN = /^mcpapp_[0-9a-f]{16}$/;

/** A document's content identity is its sha256 (64 lowercase hex). */
export const CONTENT_HASH_PATTERN = /^[0-9a-f]{64}$/;

/** The only media type an app document may declare. */
export const APP_MIME_TYPE = "text/html;profile=mcp-app";

const asRecord = (value: unknown): Record<string, unknown> =>
  value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};

const text = (value: unknown): string => (typeof value === "string" ? value : "");

const stringList = (value: unknown): string[] | undefined =>
  Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : undefined;

const numberOr = (value: unknown): number | undefined =>
  typeof value === "number" && Number.isFinite(value) ? value : undefined;

function toolRefsOf(value: unknown): McpAppToolRef[] {
  if (!Array.isArray(value)) return [];
  const refs: McpAppToolRef[] = [];
  for (const raw of value) {
    const row = asRecord(raw);
    const tool = text(row.tool);
    if (!tool) continue;
    refs.push({ tool, visibility: stringList(row.visibility) ?? [] });
  }
  return refs;
}

function permissionsOf(value: unknown): Record<string, boolean> {
  const out: Record<string, boolean> = {};
  for (const [key, item] of Object.entries(asRecord(value))) {
    if (typeof item === "boolean") out[key] = item;
  }
  return out;
}

function cspDomainsOf(value: unknown): McpAppView["cspDomains"] | undefined {
  const block = asRecord(value);
  const keys = ["connectDomains", "resourceDomains", "frameDomains", "baseUriDomains"] as const;
  const out: NonNullable<McpAppView["cspDomains"]> = {};
  let any = false;
  for (const key of keys) {
    const list = stringList(block[key]);
    if (list) {
      out[key] = list;
      any = true;
    }
  }
  return any ? out : undefined;
}

/** One catalogue row, or null when a security-bearing field is malformed. A
 * malformed app is not renderable; the caller degrades to text. */
export function parseMcpApp(raw: unknown): McpAppView | null {
  const row = asRecord(raw);
  const appId = text(row.appId);
  const hash = text(row.hash);
  const documentUrl = text(row.documentUrl);
  const resourceUri = text(row.resourceUri);
  if (!APP_ID_PATTERN.test(appId)) return null;
  if (!CONTENT_HASH_PATTERN.test(hash)) return null;
  if (text(row.mimeType) !== APP_MIME_TYPE) return null;
  if (!resourceUri.startsWith("ui://")) return null;
  // The catalogue's URL must be exactly the host-built, hash-named form.
  // Anything else (absolute, another path, extra query) is refused rather
  // than loaded: a server never chooses where the View points.
  if (documentUrl !== `/v1/mcp/apps/${appId}/view?v=${hash}`) return null;
  return {
    appId,
    server: text(row.server),
    title: text(row.title) || undefined,
    resourceUri,
    mimeType: APP_MIME_TYPE,
    visibility: stringList(row.visibility) ?? [],
    tools: toolRefsOf(row.tools),
    csp: text(row.csp),
    cspDomains: cspDomainsOf(row.cspDomains),
    permissions: permissionsOf(row.permissions),
    prefersBorder: typeof row.prefersBorder === "boolean" ? row.prefersBorder : undefined,
    hash,
    fetchedAt: text(row.fetchedAt) || undefined,
    ttlMs: numberOr(row.ttlMs),
    diagnostics: stringList(row.diagnostics),
    documentUrl,
  };
}

/** `GET /v1/mcp/apps` → the catalogue the app host reads. Malformed rows are
 * dropped, never rendered. */
export function parseMcpAppCatalogue(raw: unknown): McpAppCatalogueView {
  const body = asRecord(raw);
  const apps: McpAppView[] = [];
  for (const item of Array.isArray(body.apps) ? body.apps : []) {
    const app = parseMcpApp(item);
    if (app) apps.push(app);
  }
  const theme: Record<string, Record<string, string>> = {};
  for (const [scheme, palette] of Object.entries(asRecord(body.theme))) {
    const variables: Record<string, string> = {};
    for (const [name, value] of Object.entries(asRecord(palette))) {
      if (typeof value === "string" && value.trim() !== "") variables[name] = value;
    }
    if (Object.keys(variables).length) theme[scheme] = variables;
  }
  return {
    generation: numberOr(body.generation) ?? 0,
    apps,
    diagnostics: Array.isArray(body.diagnostics) ? body.diagnostics : undefined,
    sandboxOrigin: text(body.sandboxOrigin) || undefined,
    theme: Object.keys(theme).length ? theme : undefined,
    limits: Object.keys(asRecord(body.limits)).length
      ? Object.fromEntries(
          Object.entries(asRecord(body.limits)).flatMap(([key, value]) => {
            const n = numberOr(value);
            return n === undefined ? [] : [[key, n] as const];
          }),
        )
      : undefined,
  };
}

/** The app binding a `tool.result` carries (`details.app`). It is read from
 * the server's details object only; a top-level `app` is accepted for a
 * server-shaped payload. The appId must have the derived shape. */
export function appBindingOf(value: unknown): McpAppBinding | null {
  const row = asRecord(value);
  const details = asRecord(row.details);
  // Either the details object itself (the ledger's shape) or a payload that
  // nests it. `details.app` wins when both are present.
  const binding = asRecord(details.app).appId ? asRecord(details.app) : asRecord(row.app);
  const appId = text(binding.appId);
  const resourceUri = text(binding.resourceUri);
  if (!APP_ID_PATTERN.test(appId)) return null;
  if (!resourceUri.startsWith("ui://")) return null;
  return {
    appId,
    resourceUri,
    visibility: stringList(binding.visibility) ?? [],
  };
}

/** The document URL the iframe may load: only the catalogue's own host-built
 * same-origin path, with its content hash. There is deliberately no branch
 * that accepts an arbitrary URL. */
export function appDocumentUrl(app: McpAppView): string | null {
  if (!APP_ID_PATTERN.test(app.appId) || !CONTENT_HASH_PATTERN.test(app.hash)) return null;
  return app.documentUrl === `/v1/mcp/apps/${app.appId}/view?v=${app.hash}` ? app.documentUrl : null;
}

/** A per-View transport nonce: `&instance=<nonce>` on the document URL, used
 * by the web transport to map an inbound `message`'s `source` back to the one
 * instance that owns the frame. It is not a secret; it is a routing key. */
export const INSTANCE_PATTERN = /^[a-z0-9]{16,64}$/;

export interface AppDocumentSourceOptions {
  web: boolean;
  /** The page's origin (`keel.info`), required on the web target: NSH's web
   * runtime refuses a relative `src` (`it names no host a grant could add`). */
  origin?: string;
  /** The deployment's second origin, required on desktop/mobile (a native
   * WebView also needs an absolute URL with a host). */
  sandboxOrigin?: string;
  /** The per-instance routing nonce. */
  instance?: string;
  /** A reload generation: changing it makes NSH load the app document again
   * (the server ignores the parameter; the hash URL stays the identity). */
  reload?: number;
}

/** The URL a `WebView` actually loads: an absolute http(s) URL whose origin is
 * the page (web) or the configured sandbox origin (desktop/mobile), and whose
 * path is exactly the host-built hash-named app document. No caller-supplied
 * URL ever reaches the frame. */
export function appDocumentSource(
  app: McpAppView,
  options: AppDocumentSourceOptions,
): { url: string } | { error: string } {
  const path = appDocumentUrl(app);
  if (!path) return { error: "目录中的文档地址无效" };
  const base = (options.web ? options.origin : options.sandboxOrigin)?.trim();
  if (!base) {
    return {
      error: options.web
        ? "无法读取页面来源，交互界面不能加载"
        : "此部署未配置沙箱域，桌面客户端不挂载交互界面",
    };
  }
  let baseUrl: URL;
  try {
    baseUrl = new URL(base);
  } catch {
    return { error: options.web ? "页面来源不是合法地址" : "沙箱域不是合法地址" };
  }
  if (baseUrl.protocol !== "https:" && baseUrl.protocol !== "http:") {
    return { error: options.web ? "页面来源只接受 http(s)" : "沙箱域只接受 http(s)" };
  }
  if (baseUrl.username || baseUrl.password) {
    return { error: options.web ? "页面来源不得携带凭据" : "沙箱域不得携带凭据" };
  }
  const url = new URL(path, baseUrl);
  // The path is fixed by `appDocumentUrl`; this guards the join itself.
  if (url.origin !== baseUrl.origin || url.pathname !== `/v1/mcp/apps/${app.appId}/view`) {
    return { error: "文档地址越出宿主来源" };
  }
  if (options.instance !== undefined) {
    if (!INSTANCE_PATTERN.test(options.instance)) return { error: "实例标识形状非法" };
    url.searchParams.set("instance", options.instance);
  }
  if (options.reload !== undefined && options.reload > 0) {
    url.searchParams.set("reload", String(options.reload));
  }
  return { url: url.toString() };
}

/** The apps standard CSS variables for one appearance, from the catalogue's
 * mapped palettes. Empty/absent means "send none": the View keeps its own
 * defaults rather than receiving invented values. */
export function themeVariablesFor(
  theme: Record<string, Record<string, string>> | undefined,
  scheme: "light" | "dark",
): Record<string, string> | undefined {
  const palette = theme?.[scheme];
  if (!palette) return undefined;
  const variables: Record<string, string> = {};
  for (const [name, value] of Object.entries(palette)) {
    if (typeof value === "string" && value.trim() !== "") variables[name] = value;
  }
  return Object.keys(variables).length ? variables : undefined;
}

// ---- host bridge helpers (change feat-mcp-apps-events, F2) ----------------
//
// The bridge's outbound requests (tools/call, resources/read, ui/message,
// ui/open-link, ui/update-model-context) all share one wire discipline: build
// the exact proxy body, and turn any refusal into a JSON-RPC error without
// inventing success. These are pure so the bridge tests and the HTTP client
// exercise the same code.

/** The JSON-RPC error code for a host failure that carries none. */
export const MCP_APP_RPC_INTERNAL_ERROR = -32603;
/** The error code for an action still waiting on the user's confirmation. */
export const MCP_APP_RPC_PENDING = -32000;
/** One error message is bounded before it reaches the View. */
export const MCP_APP_RPC_MESSAGE_MAX = 500;

const integerOr = (value: unknown): number | undefined =>
  typeof value === "number" && Number.isInteger(value) ? value : undefined;

/** One bounded, control-free error line. An upstream error must not be able to
 * push an unbounded payload (or a newline-log forgery) through the bridge. */
function boundedRpcMessage(value: string): string {
  const clean = value.replace(/[\u0000-\u001f\u007f\u2028\u2029]/g, " ").trim();
  if (!clean) return "MCP app request failed";
  return clean.length > MCP_APP_RPC_MESSAGE_MAX
    ? `${clean.slice(0, MCP_APP_RPC_MESSAGE_MAX - 1)}…`
    : clean;
}

export interface McpAppRpcError {
  code: number;
  message: string;
}

/** Normalize one rejected host request. The host's own `code` wins (a proxy's
 * -32602 stays -32602); anything else is -32603 with a bounded message. Both a
 * thrown `{code,message}` and an `Error` with a `code` work. */
export function mcpAppRpcErrorOf(error: unknown): McpAppRpcError {
  const row = asRecord(error);
  const nested = asRecord(row.error);
  const direct = typeof error === "string" ? error : "";
  const code = integerOr(row.code) ?? integerOr(nested.code) ?? MCP_APP_RPC_INTERNAL_ERROR;
  const message =
    text(row.message) || text(nested.message) || text(row.error) || direct || `MCP app request failed (${code})`;
  return { code, message: boundedRpcMessage(message) };
}

/** Map a non-2xx proxy response body. A JSON-RPC-shaped refusal keeps its code
 * (`{jsonrpc:"2.0",error:{code,message}}`); a plain `{error:string}` becomes
 * -32603. A 202 means the confirmation is still pending, which is not success:
 * the View must not be told a message was sent. */
export function mcpAppRpcErrorOfResponse(status: number, payload: unknown): McpAppRpcError {
  const row = asRecord(payload);
  const nested = asRecord(row.error);
  const nestedCode = integerOr(nested.code);
  if (nestedCode !== undefined && text(nested.message)) {
    return { code: nestedCode, message: boundedRpcMessage(text(nested.message)) };
  }
  if (status === 202) {
    return { code: MCP_APP_RPC_PENDING, message: "交互操作仍在等待用户确认" };
  }
  const message = text(row.error) || text(row.message) || `HTTP ${status}`;
  return { code: MCP_APP_RPC_INTERNAL_ERROR, message: boundedRpcMessage(message) };
}

/** One host-request failure that keeps the proxy's JSON-RPC code. The bridge
 * reads `code`/`message`; the HTTP status stays for diagnostics. */
export class McpAppRequestError extends Error {
  readonly status: number;
  readonly code: number;

  constructor(status: number, payload: unknown) {
    const mapped = mcpAppRpcErrorOfResponse(status, payload);
    super(mapped.message);
    this.name = "McpAppRequestError";
    this.status = status;
    this.code = mapped.code;
  }
}

/** `sessionId` is omitted when no session is open: the proxy answers its own
 * readable "sessionId is required", rather than receiving an empty string. */
const sessionField = (sessionId: string | null | undefined): { sessionId?: string } => {
  const value = typeof sessionId === "string" ? sessionId.trim() : "";
  return value ? { sessionId: value } : {};
};

export interface McpAppToolCallBody {
  sessionId?: string;
  name: string;
  arguments: unknown;
}

export function mcpAppToolCallBody(
  sessionId: string | null | undefined,
  name: string,
  args: unknown,
): McpAppToolCallBody {
  return {
    ...sessionField(sessionId),
    name,
    arguments: args === undefined || args === null ? {} : args,
  };
}

export interface McpAppResourceReadBody {
  sessionId?: string;
  uri: string;
}

export function mcpAppResourceReadBody(
  sessionId: string | null | undefined,
  uri: string,
): McpAppResourceReadBody {
  return { ...sessionField(sessionId), uri };
}

export interface McpAppMessageBody {
  sessionId?: string;
  role: "user";
  content: unknown;
  delivery: "queue";
  requestId: string;
}

/** The View never chooses the delivery mode; the host always queues. */
export function mcpAppMessageBody(
  sessionId: string | null | undefined,
  content: unknown,
  requestId: string,
): McpAppMessageBody {
  return { ...sessionField(sessionId), role: "user", content, delivery: "queue", requestId };
}

export interface McpAppOpenLinkBody {
  sessionId?: string;
  url: string;
}

export function mcpAppOpenLinkBody(
  sessionId: string | null | undefined,
  url: string,
): McpAppOpenLinkBody {
  return { ...sessionField(sessionId), url };
}

export interface McpAppContextBody {
  sessionId?: string;
  content?: unknown;
  structuredContent?: unknown;
  requestId: string;
}

export function mcpAppContextBody(
  sessionId: string | null | undefined,
  content: unknown,
  structuredContent: unknown,
  requestId: string,
): McpAppContextBody {
  const body: McpAppContextBody = { ...sessionField(sessionId), requestId };
  if (content !== undefined) body.content = content;
  if (structuredContent !== undefined) body.structuredContent = structuredContent;
  return body;
}

/** The text an `edit` branch places into the composer. `send` and `discard`
 * carry no prefill (the ledger alone records a send). */
export function mcpAppMessagePrefill(outcome: unknown): string | undefined {
  const row = asRecord(outcome);
  if (row.result !== "edit") return undefined;
  const prefill = asRecord(row.prefilled).text;
  return typeof prefill === "string" && prefill.trim() !== "" ? prefill : undefined;
}

/** The URL the host may open: only `opened:true` with a non-empty target. A
 * denial or a missing URL is not an open (the View gets `isError`). */
export function mcpAppOpenLinkTarget(outcome: unknown): { opened: boolean; url?: string } {
  const row = asRecord(outcome);
  const url = text(row.url).trim();
  return row.opened === true && url ? { opened: true, url } : { opened: false };
}

/** Which app capabilities this deployment actually serves. In the frozen
 * server surface the four proxy routes ship with `mcp.apps`; only the model
 * context route has its own deployment bit (`mcp.apps.context`). */
export interface McpAppHostCapabilityFlags {
  serverTools: boolean;
  serverResources: boolean;
  message: boolean;
  openLinks: boolean;
  updateModelContext: boolean;
}

/** `McpUiInitializeResult.hostCapabilities`, built only from declared facts.
 * `logging` is always real (one-way notifications/message); `sandbox.csp` is
 * the resource's declared domains. Nothing else is advertised speculatively. */
export function mcpAppHostCapabilities(
  flags: McpAppHostCapabilityFlags,
  app: Pick<McpAppView, "cspDomains">,
): Record<string, unknown> {
  const capabilities: Record<string, unknown> = { logging: {} };
  if (flags.serverTools) capabilities.serverTools = {};
  if (flags.serverResources) capabilities.serverResources = {};
  if (flags.message) capabilities.message = {};
  if (flags.openLinks) capabilities.openLinks = {};
  if (flags.updateModelContext) capabilities.updateModelContext = {};
  if (app.cspDomains) capabilities.sandbox = { csp: app.cspDomains };
  return capabilities;
}

export interface CallToolResultView {
  content: unknown[];
  structuredContent?: unknown;
  isError: boolean;
}

/** A `tool.result` payload as a standard `CallToolResult` for the View. The
 * content is passed through unchanged (it is display data, never HTML the host
 * injects into its own DOM). */
export function callToolResultFor(result: McpAppToolResult | undefined): CallToolResultView | null {
  if (!result) return null;
  const content = Array.isArray(result.content)
    ? result.content
    : result.content === undefined
      ? []
      : [result.content];
  const out: CallToolResultView = { content, isError: result.isError === true };
  if (result.structuredContent !== undefined) out.structuredContent = result.structuredContent;
  return out;
}

/** The text blocks of a `CallToolResult.content`, joined for display. A tool
 * whose `details` carries an app binding still shows its human text first; the
 * details object is the binding, not the result's prose. */
export function contentTextOf(content: unknown): string {
  if (!Array.isArray(content)) return "";
  const lines: string[] = [];
  for (const raw of content) {
    const block = asRecord(raw);
    if (block.type !== "text" || typeof block.text !== "string") continue;
    const text = block.text.trim();
    if (text) lines.push(text);
  }
  return lines.join("\n\n");
}
