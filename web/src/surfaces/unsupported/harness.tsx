// TUI harness for the placeholder surface: mounts it with a video descriptor
// and records which action fired, so the test can prove each button is wired.
import { mount, signal, Text, View } from "nsh";
import { SURFACE_DESCRIPTORS } from "../registry";
import { UnsupportedSurface } from "./index";

const pressed = signal("");

function Harness() {
  return (
    <View style={{ flex: 1, minWidth: 0 }}>
      <If when={pressed() === "settings"}>
        <Text testID="pressed-settings">settings</Text>
      </If>
      <If when={pressed() === "leave"}>
        <Text testID="pressed-leave">leave</Text>
      </If>
      <UnsupportedSurface
        wide={false}
        kind="video"
        descriptor={SURFACE_DESCRIPTORS.video}
        onOpenSettings={() => pressed.set("settings")}
        onLeave={() => pressed.set("leave")}
      />
    </View>
  );
}

mount(Harness);
