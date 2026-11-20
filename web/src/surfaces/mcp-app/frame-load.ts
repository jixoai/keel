// First-load vs self-navigation verdict for one WebView element
// (change feat-mcp-apps-events, P6A2a follow-up).
//
// A conforming app may send `ui/initialize` while its document is still
// parsing, i.e. before the frame's `load` event. At that moment the bridge is
// already `handshaking` (or `ready`), so "initialized/handshaking implies the
// document was replaced" is wrong for the first load. Only a *later* load of
// the same element can be an in-frame navigation: it must drop the old
// instance so a third-party page cannot inherit the authorized channel.
import type { BridgePhase } from "./bridge";

export type FrameLoadVerdict = "start" | "reload" | "ignore";

/** Decide what a frame `load` event means. `loads` is the number of load
 * events already seen for this WebView element (0 for the first). The first
 * load always starts the bridge, even when a pre-load `ui/initialize` already
 * moved it to `handshaking`/`ready`; `start()` is idempotent and never resets
 * the handshake deadline. A destroyed instance ignores every load. A later
 * load with a live handshake is a document replacement and reloads. */
export function frameLoadVerdict(
  loads: number,
  phase: BridgePhase,
  initialized: boolean,
): FrameLoadVerdict {
  if (phase === "destroyed") return "ignore";
  if (loads <= 0) return "start";
  if (initialized || phase === "handshaking") return "reload";
  return "start";
}
