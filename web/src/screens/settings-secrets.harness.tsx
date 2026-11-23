// TUI harness · settings「密钥」— the section alone, on the fixture transport.
//
// `host.params.fixture === true` swaps in the development transport (the same
// switch App.tsx's tests use), so the assertions exercise the real store and the
// real `SecretsSection`. The demo seeds one long-preview row, one fixed-mask row
// and one legacy row with no preview (the 「已保存」 state).
import { cleanup, effect, mount, View } from "nsh";
import { store } from "../data/store";
import { SecretsSection } from "./settings";

function Harness() {
  effect(() => {
    if (store.mode() === "checking") return;
    const timer = setTimeout(() => void store.loadSecrets(), 0);
    cleanup(() => clearTimeout(timer));
  });
  return (
    <View style={{ flex: 1, flexDirection: "column", padding: 8 }}>
      <SecretsSection />
    </View>
  );
}

mount(Harness);
