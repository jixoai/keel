// TUI harness · settings「模型接入」— the section alone, on the fixture
// transport.
//
// The launch parameter `host.params.fixture === true` is the one way the
// store swaps in the development transport (the same switch App.tsx's tests
// use), so the assertions exercise the real store, the real `ModelsSection`
// and the fixture's model document — not a second fake client.
import { cleanup, effect, mount, View } from "nsh";
import { store } from "../data/store";
import { ModelsSection } from "./settings";

function Harness() {
  // The transport is chosen by the store's boot probe; wait for it to settle,
  // then load the three resources the section shows (document, providers,
  // models). The load leaves the effect's own turn so a mount-time signal
  // write never lands inside the tree's first render.
  effect(() => {
    if (store.mode() === "checking") return;
    const timer = setTimeout(() => void store.loadModelsPanel(), 0);
    cleanup(() => clearTimeout(timer));
  });
  return (
    <View style={{ flex: 1, flexDirection: "column", padding: 8 }}>
      <ModelsSection />
    </View>
  );
}

mount(Harness);
