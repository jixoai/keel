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

/** The URL a `WebView` actually loads. The web build resolves a relative path
 * against its own page (and keeps `frame-ancestors 'self'` satisfied); a
 * desktop/mobile NSH web view requires an absolute http(s) URL with a host, so
 * it uses the deployment's configured sandbox origin. Without one, the
 * platform has no address to load — the caller falls back to text instead of
 * mounting a frame that cannot work. */
export function appDocumentSource(
  app: McpAppView,
  options: { sandboxOrigin?: string; web: boolean },
): { url: string } | { error: string } {
  const path = appDocumentUrl(app);
  if (!path) return { error: "目录中的文档地址无效" };
  if (options.web) return { url: path };
  const base = options.sandboxOrigin?.trim();
  if (!base) return { error: "此部署未配置沙箱域，桌面客户端不挂载交互界面" };
  let baseUrl: URL;
  try {
    baseUrl = new URL(base);
  } catch {
    return { error: "沙箱域不是合法地址" };
  }
  if (baseUrl.protocol !== "https:" && baseUrl.protocol !== "http:") {
    return { error: "沙箱域只接受 http(s)" };
  }
  if (baseUrl.username || baseUrl.password) return { error: "沙箱域不得携带凭据" };
  const url = new URL(path, baseUrl);
  // The path is fixed by `appDocumentUrl`; this guards the join itself.
  if (url.origin !== baseUrl.origin || url.pathname !== `/v1/mcp/apps/${app.appId}/view`) {
    return { error: "文档地址越出沙箱域" };
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
