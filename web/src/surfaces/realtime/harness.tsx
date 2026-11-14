// Harness · realtime surface — mounts `RealtimeSurface` for the TUI/headless
// gate. Not a product screen: it exists so `realtime.test.tsx` can drive the
// duplex session without the app shell around it.
//
// The store facts the surface reads (the open session and its model selection)
// are seeded from the host's launch parameters, so one harness covers the
// model-bound case, the "no realtime model configured" case and the 390px
// layout without a second entry file.
//
// The session id is a real UUID shape on purpose: the bridge builds
// `/v1/realtime?session_id=…` from it and the server resolves the session by
// it, so a fixture with a made-up id would test a call that cannot exist.
import { host, mount, View, windowSize } from "nsh";
import { store } from "../../data/store";
import { RealtimeSurface } from "./index";

const params = host.params as Record<string, unknown>;
const text = (key: string, fallback: string): string =>
  typeof params[key] === "string" ? String(params[key]) : fallback;

/** A session always exists in this harness; `noModel` removes only the model,
 * so the panel can be driven into the configuration state with a session open,
 * and `noSession` removes the session itself (the panel must not invent one). */
if (params.noSession !== true) {
  store.activeId.set(text("sessionId", "6f1c2d3e-4a5b-4c6d-8e7f-901234567890"));
}

if (params.noModel !== true) {
  store.sessionSelection.set({
    modelId: text("model", "qwen3-omni-flash-realtime"),
    providerId: text("providerId", "qwen"),
    surfaceKind: "realtime",
  });
}

function Harness() {
  return (
    <View style={{ flex: 1, minWidth: 0 }}>
      <RealtimeSurface wide={windowSize().width >= 768} />
    </View>
  );
}

mount(Harness);
