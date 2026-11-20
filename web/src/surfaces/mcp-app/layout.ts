// The MCP App View's two display modes as layout values (P6V). Inline is the
// app's own size: the frame uses the height the View reported through
// `ui/notifications/size-changed`. Fullscreen is the host's: the dialog covers
// the whole modal layer, and the frame takes the panel's remaining height
// instead of keeping the inline height the app chose before the mode change.
//
// Pure module: the props are the values, so a unit test pins the choice
// without a browser (the Dialog itself offers no view-style query).
import type { ViewStyle } from "nsh";
import { INLINE_MIN_HEIGHT } from "./bridge";

/** The fullscreen Dialog's box: the modal layer is fixed at the window's
 * top-left, so 100% is the window. (92% x 88% left a strip of the page
 * visible at the right and bottom.) */
export function fullscreenDialogStyle(): ViewStyle {
  return {
    width: "100%",
    maxWidth: "100%",
    height: "100%",
    maxHeight: "100%",
  };
}

/** The frame's wrapper: inline lays out at its content; fullscreen is the
 * growing band between the panel's header and its resource line. */
export function frameWrapperStyle(fullscreen: boolean): ViewStyle {
  return fullscreen
    ? { width: "100%", flexDirection: "column", flex: 1, minHeight: 0 }
    : { width: "100%", flexDirection: "column" };
}

/** The WebView's own box: fullscreen stretches with the wrapper (`height`
 * 100% plus `flex: 1`; the reported height belongs to inline). */
export function frameStyle(fullscreen: boolean, reportedHeight: number): ViewStyle {
  return fullscreen
    ? { width: "100%", height: "100%", flex: 1, minHeight: 0 }
    : { width: "100%", height: reportedHeight, minHeight: INLINE_MIN_HEIGHT };
}
