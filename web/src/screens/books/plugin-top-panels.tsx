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
import { currentPluginTarget } from "../../plugins/forms";
import { pluginSlots } from "../../plugins";
import { PluginSlot, PluginTopBandEntry } from "../../plugins/slots";
import { isMobile } from "./common";

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

/** The live host nodes (the legacy `registerPluginTopPanel` seam). */
export const pluginTopPanels = panels;

/** True while the band has anything to show: a host node or a registered
 * `keel.ui.top-band` contribution. The shell reads it for the band's height;
 * with nothing, the band is the slim brand strip and never a fake panel. */
export function topBandHasContent(): boolean {
  if (panels().length > 0) return true;
  const view = pluginSlots.resolve("keel.ui.top-band", currentPluginTarget());
  return (view?.contributions.length ?? 0) > 0 || (view?.unavailable.length ?? 0) > 0;
}

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
  const hasContent = () => panels().length > 0 || topBandHasContent();
  return (
    <If when={hasContent()}>
      <View
        testID={props.testID ?? "plugin-top-panels"}
        accessibilityLabel="插件面板"
        style={{
          flex: 1,
          minWidth: 0,
          flexDirection: "row",
          alignItems: "center",
          gap: space.sm,
          overflow: "hidden",
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
                flexShrink: 1,
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
        <PluginSlot
          slot="keel.ui.top-band"
          testID="plugin-top-band-contributions"
          style={{ flexDirection: "row", alignItems: "center", gap: space.sm, minWidth: 0, overflow: "hidden" }}
          render={(entry) => <PluginTopBandEntry entry={entry} />}
          renderUnavailable={(entry) =>
            entry.status === "failed" ? (
              <Text style={{ fontSize: typeScale.xs, color: color.danger, numberOfLines: 1 }}>
                {`${entry.code ?? "ERR_PLUGIN"} · ${entry.declaration.pluginId}`}
              </Text>
            ) : (
              <Text style={{ fontSize: typeScale.xs, color: color.warning, numberOfLines: 1 }}>
                {`${entry.code ?? "ERR_TARGET_UNAVAILABLE"} · 插件面板不可用`}
              </Text>
            )
          }
        />
      </View>
    </If>
  );
}

/** The brand half of the band: a K tile and the wordmark. It doubles as the
 * window's drag region; it never grows a control of its own. */
export function BrandMark() {
  const compact = () => isMobile();
  return (
    <View
      accessibilityLabel="Keel"
      style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexShrink: 0 }}
    >
      <View
        style={{
          width: compact() ? 20 : 22,
          height: compact() ? 20 : 22,
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
      <If when={!compact()}>
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
