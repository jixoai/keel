// The top band's plugin slot — change A4.4f.
//
// The product's top band is not navigation any more: the chapters live in the
// left rail (desktop) or the bottom bar (phone), and account / todo / live
// state / settings live on the settings page. What the band is for is plugins:
// a panel a plugin contributes to the top of the shell (a mini player, a
// recording strip, a task light — whatever the plugin can prove it needs).
//
// There is no plugin UI runtime yet, so the registry starts empty and the band
// renders no fake panel: with no contribution it is only the slim brand /
// window-drag strip. When the plugin contract lands, a plugin registers
// through `registerPluginTopPanel` (returning its own disposer) and the same
// slot renders its node; the kit's layout guarantees it cannot push the page
// sideways at 390/768/1024/1440.
import type { JSX } from "nsh";
import { signal, Text, View } from "nsh";
import { color, font, radius, space, typeScale, weight } from "../../ui/kit";

/** One panel a plugin puts in the shell's top band. */
export interface PluginTopPanel {
  /** Stable identity: one contribution per plugin surface. */
  id: string;
  /** What the panel is, for assistive tech and overflow. */
  label: string;
  /** The panel itself, made by the plugin's own renderer. */
  node: JSX.Element;
}

const panels = signal<PluginTopPanel[]>([]);

/** The live contributions (reactive). The shell reads it to decide whether
 * the band is a plugin row or the slim brand strip. */
export const pluginTopPanels = panels;

/** Register a panel in the shell's top band. Returns the disposer a plugin
 * calls when it unloads; a duplicate id replaces the earlier panel. */
export function registerPluginTopPanel(panel: PluginTopPanel): () => void {
  panels.update((list) => [...list.filter((entry) => entry.id !== panel.id), panel]);
  return () => {
    panels.update((list) => list.filter((entry) => entry.id !== panel.id));
  };
}

/** The band's plugin area. Renders nothing at all when no plugin contributes:
 * an empty contribution list must not leave a visual hole. */
export function PluginTopPanels(props: { testID?: string }) {
  return (
    <If when={panels().length > 0}>
      <View
        testID={props.testID ?? "plugin-top-panels"}
        accessibilityLabel="插件面板"
        style={{
          flex: 1,
          minWidth: 0,
          flexDirection: "row",
          alignItems: "center",
          gap: space.sm,
        }}
      >
        <Each of={panels()} key={(panel) => panel.id}>
          {(panel) => (
            <View
              testID={`plugin-top-panel-${panel().id}`}
              accessibilityLabel={panel().label}
              style={{
                minWidth: 0,
                maxWidth: "100%",
                height: 32,
                flexDirection: "row",
                alignItems: "center",
                gap: space.sm,
                paddingHorizontal: space.md,
                borderRadius: radius.md,
                backgroundColor: color.glassControl,
                borderWidth: 1,
                borderColor: color.glassControlRim,
                boxShadow: `inset 0 1px 0 ${color.glassRim}`,
              }}
            >
              {panel().node}
            </View>
          )}
        </Each>
      </View>
    </If>
  );
}

/** The brand half of the band: a K tile and the wordmark. It doubles as the
 * window's drag region; it never grows a control of its own. */
export function BrandMark(props: { compact?: boolean }) {
  return (
    <View
      accessibilityLabel="Keel"
      style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexShrink: 0 }}
    >
      <View
        style={{
          width: props.compact ? 20 : 22,
          height: props.compact ? 20 : 22,
          borderRadius: radius.sm,
          alignItems: "center",
          justifyContent: "center",
          borderWidth: 1,
          borderColor: color.glassEdge,
          backgroundColor: color.glassControl,
          boxShadow: `inset 0 1px 0 ${color.glassRim}`,
        }}
      >
        <Text
          style={{
            fontFamily: font.mono,
            fontSize: typeScale.key,
            fontWeight: weight.semibold,
            color: color.foreground,
          }}
        >
          K
        </Text>
      </View>
      <If when={!props.compact}>
        <Text
          style={{
            fontSize: typeScale.sm,
            fontWeight: weight.semibold,
            letterSpacing: 1.4,
            color: color.foreground,
          }}
        >
          KEEL
        </Text>
      </If>
    </View>
  );
}
