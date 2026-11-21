// TUI harness · the composer's `$` menu, on the fixture transport.
//
// The fixture is the dev launch's transport (`host.params.fixture === true`,
// the same switch App.tsx's tests use): it already serves the skill library,
// the session files and the session tasks, so the assertions exercise the real
// store, the real `CompletionMenu` and the real Composer — not a second fake
// client. The harness only makes sure a session is open (the menu's file/task
// groups are session-scoped) and mounts the composer with no conversation.
import { effect, mount, View } from "nsh";
import { store } from "../data/store";
import { Composer } from "./composer";

function Harness() {
  effect(() => {
    if (store.mode() === "checking") return;
    if (store.activeId()) return;
    const first = store.sessions()[0];
    if (first) store.activeId.set(first.sessionId);
  });
  return (
    <View style={{ flex: 1, flexDirection: "column", padding: 8 }}>
      <Composer view={null} />
    </View>
  );
}

mount(Harness);
