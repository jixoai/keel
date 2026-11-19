// The app catalogue's client cache (change feat-mcp-apps-events, P2.7).
//
// The catalogue is the binding's single machine-readable source (spec「app
// 身份、缓存身份与目录」): the tool result only names an appId, and the
// document URL/hash/theme come from `GET /v1/mcp/apps`. This module is the one
// place that reads it; every app row shares the same fetch.
//
// It is loaded lazily (only when a tool result actually carries a binding and
// this client supports apps) and refreshed at most once for an appId that is
// missing from an otherwise-ready catalogue: an app published after the first
// fetch appears without polling.
import { signal } from "nsh";
import type { McpAppCatalogueView, McpAppView } from "../../data/contract";
import { deploymentCan } from "../../data/capabilities";
import { store } from "../../data/store";

export interface AppCatalogueState {
  status: "idle" | "loading" | "ready" | "error";
  generation: number;
  apps: Record<string, McpAppView>;
  theme?: Record<string, Record<string, string>>;
  sandboxOrigin?: string;
  error?: string;
}

const EMPTY: AppCatalogueState = { status: "idle", generation: 0, apps: {} };

const state = signal<AppCatalogueState>(EMPTY);
let inflight: Promise<void> | null = null;
/** The newest request generation; older answers are ignored. */
let requestToken = 0;
/** appIds already re-fetched once because the ready catalogue lacked them. */
const refreshedFor = new Set<string>();

const messageOf = (error: unknown): string => {
  if (error instanceof Error && error.message) return error.message;
  const value = error as { message?: unknown } | null;
  return typeof value?.message === "string" && value.message ? value.message : String(error);
};

/** Ask for the catalogue. `appId` is the binding the row wants to mount; a
 * missing app in a ready catalogue triggers one refresh (the app may have been
 * published after the first fetch). `force` is the UI's retry. */
export function ensureAppCatalogue(appId?: string, force = false): void {
  // The catalogue is a server fact, not a client capability: the TUI loads it
  // to show the app's title and document link even though it will not mount a
  // WebView.
  if (!deploymentCan("mcp.apps")) return;
  const current = state();
  const missing = appId !== undefined && current.status === "ready" && !current.apps[appId];
  // A render-path ask never duplicates an in-flight fetch; the retry button
  // (`force`) starts a new generation even while one is running, and the older
  // answer is ignored by the token check below.
  if (inflight && !force) return;
  if (!force) {
    if (current.status === "loading") return;
    // A failed fetch is not retried by the render path: the row's 重试 button
    // calls with `force`, so an unreachable catalogue cannot spin.
    if (current.status === "error") return;
    if (current.status === "ready" && !missing) return;
    if (missing && appId !== undefined && refreshedFor.has(appId)) return;
  }
  if (missing && appId !== undefined) refreshedFor.add(appId);
  const token = ++requestToken;
  state.set({ ...current, status: "loading", error: undefined });
  const request: Promise<McpAppCatalogueView> = store.mcpApps();
  inflight = request
    .then((catalogue) => {
      if (token !== requestToken) return;
      const apps: Record<string, McpAppView> = {};
      for (const app of catalogue.apps) apps[app.appId] = app;
      state.set({
        status: "ready",
        generation: catalogue.generation,
        apps,
        theme: catalogue.theme,
        sandboxOrigin: catalogue.sandboxOrigin,
      });
    })
    .catch((error: unknown) => {
      if (token !== requestToken) return;
      state.set({
        status: "error",
        generation: current.generation,
        apps: current.apps,
        theme: current.theme,
        sandboxOrigin: current.sandboxOrigin,
        error: messageOf(error),
      });
    })
    .finally(() => {
      if (token === requestToken) inflight = null;
    });
}

/** The mounted app's catalogue entry, if the catalogue has it. */
export function appCatalogueEntry(appId: string): McpAppView | undefined {
  return state().apps[appId];
}

export function appCatalogueState(): AppCatalogueState {
  return state();
}

/** The deployment's configured second origin (empty in the single-origin
 * form). The View's `src` stays the host-built same-origin path either way;
 * this is reported in diagnostics, never turned into an arbitrary URL. */
export function catalogueSandboxOrigin(): string | undefined {
  return state().sandboxOrigin;
}

/** Tests only. */
export function resetAppCatalogueForTest(): void {
  inflight = null;
  requestToken += 1;
  refreshedFor.clear();
  state.set(EMPTY);
}
