// TUI harness for the image surface's headless test: a full-screen mount that
// starts on the product's empty state (no seeded gallery).
// Not part of the app's routing (the main agent mounts ImageSurface itself).
//
// The terminal has no origin, so the surface's relative `/v1/...` paths cannot
// resolve on their own. `open(..., { network })` answers requests against a
// synthetic origin, and this file points the media client at the same one —
// the surface's own request building, parsing and error classification stay
// exactly the code the product runs.
import { mount, View } from "nsh";
import { setMediaBase } from "../../data/media";
import { ImageSurface } from "./index";

/** The origin the headless network stub answers on. */
export const HARNESS_ORIGIN = "http://keel.test";
setMediaBase(HARNESS_ORIGIN);

function Harness() {
  return (
    <View style={{ flex: 1, minWidth: 0 }}>
      <ImageSurface wide={false} />
    </View>
  );
}

mount(Harness);
