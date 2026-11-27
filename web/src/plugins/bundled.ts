// Chunks shipped with this build (W12).
//
// The fixture plugin's panel lives here so the development transport, the TUI
// test host and the browser fixture can render the same chunk without a server
// round trip. A runtime-installed plugin never enters this table: its entry is
// resolved through NSH's `__nsh_import` (see `loader.ts`).
import type { PluginChunkModule } from "./types";

type ChunkFactory = () => Promise<PluginChunkModule>;

export const BUNDLED_CHUNKS: Record<string, ChunkFactory> = {
  "fixture:w12-hello": () =>
    import("./fixtures/hello-chunk").then((module) => module as unknown as PluginChunkModule),
  // The attachment fallback tests' broken renderer: a throwing chunk that must
  // never remove the host's default line or affect another attachment.
  "fixture:w12-throwing": () =>
    import("./fixtures/throwing-chunk").then((module) => module as unknown as PluginChunkModule),
  // A chunk that never finishes loading, for the preview timeout face. It is
  // reachable only through the explicit `fixture:` prefix.
  "fixture:w12-loading": () => new Promise<PluginChunkModule>(() => {}),
  // The OAR external-run card (change keel-oar-renderer): a first-party builtin
  // contribution, not inventory. Its key is the contribution's `entry`.
  "keel.oar:tool-renderer": () =>
    import("./chunks/oar-run").then((module) => module as unknown as PluginChunkModule),
};
