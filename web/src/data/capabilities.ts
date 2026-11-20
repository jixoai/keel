// The capability gate (`can(x)`): every conditional render that depends on
// which capabilities this deployment or this client has asks here, never the
// account/identity shape. Two sources, deliberately separate:
//
//   1. the deployment's `GET /v1/capabilities` (server facts: which features
//      this keel serve wires — `mcp.apps`, `mcp.events`, ...);
//   2. the client's own capabilities (this build's facts: the terminal host
//      has no WebView, so it does not support MCP Apps even when the server
//      does).
//
// A feature is on only when both sides say yes. A missing feature is off (fail
// closed): the UI never guesses a capability from a server version.
import { signal } from "nsh";

/** The MCP Apps block of `/v1/capabilities` (`mcpApps`). `sandboxOrigin` is
 * the deployment's optional second origin; empty in the single-origin form. */
export interface McpAppsCapability {
  sandboxOrigin: string;
  generation: number;
  apps: number;
}

export interface DeploymentCapabilities {
  features: Record<string, boolean>;
  mcpApps: McpAppsCapability;
}

const EMPTY: DeploymentCapabilities = {
  features: {},
  mcpApps: { sandboxOrigin: "", generation: 0, apps: 0 },
};

const deployment = signal<DeploymentCapabilities>(EMPTY);
/** What this build can do, independent of the deployment (`"mcp.apps"` is off
 * on the terminal host: no WebView, and the spec's TUI rule says text only). */
const client = signal<Record<string, boolean>>({});

const asRecord = (value: unknown): Record<string, unknown> =>
  value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};

const text = (value: unknown): string => (typeof value === "string" ? value.trim() : "");

const count = (value: unknown): number =>
  typeof value === "number" && Number.isFinite(value) && value >= 0 ? Math.floor(value) : 0;

/** The boolean feature map: only literal `true` counts. The flat
 * `capabilities` object is tolerated because the scripted fixture used it
 * before the real discovery document was wired; the served document's key is
 * `features`. */
function parseFeatures(raw: Record<string, unknown>): Record<string, boolean> {
  const source = asRecord(raw.features);
  const fallback = Object.keys(source).length ? {} : asRecord(raw.capabilities);
  const features: Record<string, boolean> = {};
  for (const [key, value] of Object.entries(source)) {
    if (typeof value === "boolean") features[key] = value;
  }
  for (const [key, value] of Object.entries(fallback)) {
    if (typeof value === "boolean") features[key] = value;
  }
  return features;
}

/** A deployment discovery document → the facts the gate reads. Unknown fields
 * are dropped; a malformed block leaves the capability off. */
export function parseDeploymentCapabilities(raw: unknown): DeploymentCapabilities {
  const body = asRecord(raw);
  const mcpApps = asRecord(body.mcpApps);
  return {
    features: parseFeatures(body),
    mcpApps: {
      sandboxOrigin: text(mcpApps.sandboxOrigin),
      generation: count(mcpApps.generation),
      apps: count(mcpApps.apps),
    },
  };
}

/** Record the answer of `GET /v1/capabilities` (or the fixture's). */
export function setDeploymentCapabilities(raw: unknown): void {
  deployment.set(parseDeploymentCapabilities(raw));
}

/** Read the deployment document after an auth transition and record it.
 *
 * The boot probe stops at the login gate (401), so the first capability read a
 * deployment can answer happens after `login()`. Without this refresh the gate
 * keeps the pre-login (off) snapshot until a full page reload, and an app-bearing
 * tool result already on screen stays on the text fallback.
 *
 * On failure the previous snapshot is kept (the gate stays fail-closed: it
 * never invents capabilities) and the reason is returned for the caller to
 * record. The read is bounded so a stalled endpoint cannot hold the login. */
export const CAPABILITIES_REFRESH_TIMEOUT_MS = 3000;

export async function refreshDeploymentCapabilities(
  source: { capabilities(): Promise<Record<string, unknown>> },
  timeoutMs = CAPABILITIES_REFRESH_TIMEOUT_MS,
): Promise<{ ok: true } | { ok: false; error: string }> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    const timeout = new Promise<never>((_, reject) => {
      timer = setTimeout(() => {
        reject(new Error(`capabilities request timed out after ${timeoutMs}ms`));
      }, timeoutMs);
    });
    setDeploymentCapabilities(await Promise.race([source.capabilities(), timeout]));
    return { ok: true };
  } catch (error) {
    const message = error instanceof Error && error.message ? error.message : String(error);
    return { ok: false, error: message };
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

/** Record what this client build supports. Called once at boot. */
export function setClientCapabilities(features: Record<string, boolean>): void {
  client.set({ ...features });
}

/** The four app proxy routes ship with the app surface. `/v1/capabilities`
 * reports each one explicitly (`mcp.apps.tools`, `.resources`, `.message`,
 * `.links`); an explicit bit always wins when the deployment sends it. An older
 * deployment that predates the per-action bits reports only `mcp.apps`, so the
 * gate derives the four from the parent when a child key is absent (`context`
 * is deliberately NOT derived: its route has a real deployment bit
 * `mcp.apps.context`, and an unwired store must stay false). An explicit
 * `false` for a child key always wins over the parent. */
const APP_PROXY_FEATURES = new Set([
  "mcp.apps.tools",
  "mcp.apps.resources",
  "mcp.apps.message",
  "mcp.apps.links",
]);

const deploymentFeature = (feature: string): boolean => {
  const features = deployment().features;
  if (features[feature] === true) return true;
  if (features[feature] === false) return false;
  if (APP_PROXY_FEATURES.has(feature)) return features["mcp.apps"] === true;
  return false;
};

/** The one capability question. Reactive: a screen that reads it re-renders
 * when the deployment is probed or the client's facts change. A feature this
 * client has not registered is off (undefined is not "trust the deployment"):
 * both sides must say `true`, and an unknown feature is off. */
export function can(feature: string): boolean {
  if (client()[feature] !== true) return false;
  return deploymentFeature(feature);
}

/** The deployment's half of the gate, for work the **server** can do even when
 * this client cannot render it (the TUI reads the app catalogue so it can show
 * the app's title and document link, then keeps the text result). */
export function deploymentCan(feature: string): boolean {
  return deploymentFeature(feature);
}

/** The MCP Apps deployment block (`sandboxOrigin` included). */
export function mcpAppsCapability(): McpAppsCapability {
  return deployment().mcpApps;
}

/** The whole deployment snapshot, for diagnostics. */
export function deploymentCapabilities(): DeploymentCapabilities {
  return deployment();
}

/** Tests only: forget both sides so a case starts from "everything off". */
export function resetCapabilitiesForTest(): void {
  deployment.set(EMPTY);
  client.set({});
}
