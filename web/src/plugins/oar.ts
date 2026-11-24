// The first-party OAR external-run renderer — change keel-oar-renderer.
//
// The conversation host consumes `keel.ui.tool-renderer` for `oar` records; the
// contribution is builtin (not inventory) because the renderer ships with this
// build and must work on a deployment whose plugin inventory never mentioned
// it. It targets every client: the graphical hosts draw the card, the terminal
// host degrades the record to its text row (a card is a graphical face).
import type { RegisterResult, SlotRegistry } from "./registry";

/** The plugin id the renderer declares itself under (`keel.oar` is the OAR
 * sidecar's package id; a later inventory entry replaces this declaration). */
export const OAR_PLUGIN_ID = "keel.oar";

/** The tool name the conversation host asks the slot for. */
export const OAR_TOOL_NAME = "oar";

/** The bundled chunk key (`BUNDLED_CHUNKS`), resolved without a fetch. */
export const OAR_RENDERER_ENTRY = "keel.oar:tool-renderer";

/** Declare the builtin renderer. Idempotent by replacement: registering the
 * same plugin id again swaps this contribution for the new one. */
export function registerOarToolRenderer(registry: SlotRegistry): RegisterResult {
  return registry.registerPlugin({
    pluginId: OAR_PLUGIN_ID,
    contributes: [
      {
        // Contribution ids are global across plugins (the registry refuses a
        // duplicate): this one is the OAR run card's own, never a slot name.
        id: "oar-run",
        slot: "keel.ui.tool-renderer",
        form: "chunk",
        entry: OAR_RENDERER_ENTRY,
        title: "外部运行",
        tool: OAR_TOOL_NAME,
        targets: ["web", "native", "tui"],
        // The card draws from the host's props; it never calls the bridge.
        capabilities: ["state"],
      },
    ],
  });
}
