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
};
