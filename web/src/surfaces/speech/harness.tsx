// TUI harness · speech workbench — mounts `SpeechSurface` so `speech.test.tsx`
// can drive the real microphone path (headless) or its no-recognizer
// degradation (terminal), the text fallback and the TTS composer on NSH's
// terminal host. Not a product screen: it exists so the surface is testable
// without the app shell around it.
//
// The TTS probe reads `/v1/media/capabilities`; a headless terminal has no
// origin, so the harness points the media client at the stub's origin (the same
// pattern as the image harness). Tests that never stub the network get the
// configuration state.
import { host, mount, View, windowSize } from "nsh";
import { setMediaBase } from "../../data/media";
import { SpeechSurface } from "./index";

setMediaBase("http://keel.test");

function Harness() {
  return (
    <View style={{ flex: 1 }}>
      <SpeechSurface
        wide={windowSize().width >= 768}
        mode={host.params.mode === "tts" ? "tts" : "stt"}
      />
    </View>
  );
}

mount(Harness);
