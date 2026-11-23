// TUI harness · settings「模型接入」— the section alone, on the fixture
// transport.
//
// The launch parameter `host.params.fixture === true` is the one way the
// store swaps in the development transport (the same switch App.tsx's tests
// use), so the assertions exercise the real store, the real `ModelsSection`
// and the fixture's model document — not a second fake client.
import { Scroll, cleanup, effect, mount, View } from "nsh";
import { store } from "../data/store";
import { ModelsSection } from "./settings";

function Harness() {
  // The transport is chosen by the store's boot probe; wait for it to settle,
  // then load the four resources the section shows (document, providers,
  // models, secrets — the editor's direct-credential preview reads the
  // secret list). The load leaves the effect's own turn so a mount-time signal
  // write never lands inside the tree's first render.
  effect(() => {
    if (store.mode() === "checking") return;
    const timer = setTimeout(() => {
      void store.loadModelsPanel();
      void store.loadSecrets();
    }, 0);
    cleanup(() => clearTimeout(timer));
  });
  // The section is taller than the test viewport (each provider card carries
  // its resolved facts); the real page scrolls, so the harness does too — a
  // bottom action is reached the way a reader reaches it.
  return (
    <Scroll testID="settings-models-scroll" style={{ flex: 1 }}>
      <View style={{ flexDirection: "column", padding: 8 }}>
        <ModelsSection />
      </View>
    </Scroll>
  );
}

mount(Harness);
