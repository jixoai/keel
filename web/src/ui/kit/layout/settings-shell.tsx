// L · SettingsShell — the settings page's rail + content card.
//
// Parameters from the zcode reference (`SettingsPage.tsx`), item by item:
//
//   | item                    | value                                        |
//   |-------------------------|----------------------------------------------|
//   | icon rail               | 68px, and 268px from lg (≥1024px viewport)   |
//   | rail header strip       | h-12 (48px, the window-drag/title band)      |
//   | content card            | rounded-xl (12px) + 1px border               |
//   | breadcrumb / toolbar    | h-12 (48px) above the scrolling content      |
//   | content column          | max-w-4xl (896px), centered; px-4 / lg:px-8  |
//   | section memory          | `zcode-settings-last-section`, read on mount |
//
// The section memory is the async `Storage` (NSH's device storage), where the
// reference uses `localStorage`: the read lands after the first paint, so the
// rail may switch its active section once, right after mount — the previous
// section then stays put for the rest of the session.
//
// Platform notes:
//   · terminal: 68px/896px are laid out on the cell grid (widths near a few
//     cells); the rail's labels are hidden in the rail mode either way, so the
//     narrow layout is legible there.
//   · web/desktop: as the reference.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { effect, Scroll, Storage, signal, Text, View, windowSize } from "nsh";
import { Icon, type IconName } from "../../icons";
import { color, motion, radius as kitRadius, typeScale } from "../tokens";

/** The rail's two widths (zcode's `grid-cols-[68px…] lg:grid-cols-[268px…]`). */
export const SETTINGS_RAIL_WIDTH_PX = 68;
export const SETTINGS_RAIL_WIDE_WIDTH_PX = 268;
/** Tailwind's `lg`, the breakpoint the reference widens the rail at. */
export const SETTINGS_RAIL_WIDE_MIN_WINDOW_PX = 1024;
/** The h-12 breadcrumb row. */
export const SETTINGS_BREADCRUMB_HEIGHT_PX = 48;
/** `max-w-4xl`: the content column's ceiling. */
export const SETTINGS_CONTENT_MAX_WIDTH_PX = 896;
/** The section the rail remembers (`SETTINGS_LAST_SECTION_STORAGE_KEY`). */
export const SETTINGS_SECTION_STORAGE_KEY = "zcode-settings-last-section";

export interface SettingsSectionEntry {
  id: string;
  label: string;
  icon?: IconName;
  /** A group label above the section, in the wide rail. */
  group?: string;
}

export interface SettingsShellProps {
  sections: SettingsSectionEntry[];
  /** The controlled section; without it the shell owns it and remembers it. */
  section?: string;
  /** Where it opens when nothing is remembered yet. */
  initialSection?: string;
  onSectionChange?: (sectionId: string) => void;
  /** The path shown in the h-12 row: section, then the page. */
  breadcrumb: string[];
  backLabel?: string;
  onBack?: () => void;
  children: JSX.Element;
  style?: StyleProp<ViewStyle>;
  testID?: string;
}

/** The settings page: icon rail (68 → 268 from lg) + one content card. */
export function SettingsShell(props: SettingsShellProps) {
  const ownSection = signal(props.initialSection ?? props.sections[0]?.id ?? "general");
  const activeSection = () => props.section ?? ownSection();
  const wideRail = () => windowSize().width >= SETTINGS_RAIL_WIDE_MIN_WINDOW_PX;

  // The remembered section, read once on mount (async storage).
  effect(() => {
    if (props.initialSection !== undefined) return;
    void (async () => {
      const stored = await Storage.getItem(SETTINGS_SECTION_STORAGE_KEY);
      if (stored === null || stored.length === 0) return;
      if (!props.sections.some((entry) => entry.id === stored)) return;
      ownSection.set(stored);
    })();
  });

  const selectSection = (sectionId: string) => {
    if (props.section === undefined) {
      ownSection.set(sectionId);
      void Storage.setItem(SETTINGS_SECTION_STORAGE_KEY, sectionId);
    }
    props.onSectionChange?.(sectionId);
  };

  return (
    <View
      testID={props.testID}
      style={[
        { flex: 1, minWidth: 0, flexDirection: "row", backgroundColor: color.background },
        props.style ?? null,
      ]}
    >
      <View
        testID="settings-rail"
        style={{
          width: wideRail() ? SETTINGS_RAIL_WIDE_WIDTH_PX : SETTINGS_RAIL_WIDTH_PX,
          flexShrink: 0,
          flexDirection: "column",
          backgroundColor: color.sidebar,
          borderRightWidth: 1,
          borderColor: color.sidebarBorder,
        }}
      >
        {/* The window-drag/title band: h-12, empty on the web and in a
            terminal. */}
        <View style={{ height: SETTINGS_BREADCRUMB_HEIGHT_PX, flexShrink: 0 }} />
        <Scroll style={{ flex: 1, minHeight: 0 }}>
          <View style={{ paddingHorizontal: 8, paddingBottom: 12, gap: 4 }}>
            <If when={props.onBack}>
              <SettingsRailButton
                icon="chevron-left"
                label={props.backLabel ?? "返回工作区"}
                testID="settings-back"
                wide={wideRail()}
                onPress={() => props.onBack?.()}
              />
            </If>
            <Each of={props.sections} key={(entry) => entry.id}>
              {(entry) => (
                <SettingsRailEntry
                  entry={entry()}
                  wide={wideRail()}
                  selected={activeSection() === entry().id}
                  onSelect={selectSection}
                />
              )}
            </Each>
          </View>
        </Scroll>
      </View>

      <View style={{ flex: 1, minWidth: 0, flexDirection: "column" }}>
        <View
          testID="settings-content-card"
          style={{
            flex: 1,
            minHeight: 0,
            borderWidth: 1,
            borderColor: color.border,
            // rounded-xl on the web, rounded-md on Windows (the reference's
            // panel-radius branch).
            borderRadius: kitRadius.lg,
            backgroundColor: color.background,
            overflow: "hidden",
          }}
        >
          <View
            testID="settings-breadcrumb"
            style={{
              height: SETTINGS_BREADCRUMB_HEIGHT_PX,
              flexShrink: 0,
              flexDirection: "row",
              alignItems: "center",
              paddingHorizontal: 16,
              gap: 6,
            }}
          >
            <Each of={props.breadcrumb} key="index">
              {(crumb, index) => (
                <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
                  <If when={index() > 0}>
                    <Icon name="chevron-right" size={12} tint={color.mutedForeground} />
                  </If>
                  <Text
                    style={{
                      fontSize: typeScale.sm,
                      fontWeight: index() === props.breadcrumb.length - 1 ? 600 : 400,
                      color:
                        index() === props.breadcrumb.length - 1
                          ? color.foreground
                          : color.mutedForeground,
                    }}
                  >
                    {crumb()}
                  </Text>
                </View>
              )}
            </Each>
          </View>
          <Scroll testID="settings-content" style={{ flex: 1, minHeight: 0 }}>
            <View
              style={{
                width: "100%",
                maxWidth: SETTINGS_CONTENT_MAX_WIDTH_PX,
                marginHorizontal: "auto",
                paddingHorizontal: wideRail() ? 32 : 16,
                paddingBottom: 40,
                gap: 32,
              }}
            >
              {props.children}
            </View>
          </Scroll>
        </View>
      </View>
    </View>
  );
}

/** One rail entry: icon always, label only in the wide rail (the reference's
 * `max-lg:sr-only`). */
function SettingsRailEntry(props: {
  entry: SettingsSectionEntry;
  wide: boolean;
  selected: boolean;
  onSelect: (sectionId: string) => void;
}) {
  return (
    <View style={{ gap: 4 }}>
      <If when={props.wide && (props.entry.group ?? "") !== ""}>
        <Text
          style={{
            paddingHorizontal: 10,
            paddingBottom: 2,
            fontSize: typeScale.xs,
            fontWeight: 500,
            color: color.mutedForeground,
            textTransform: "uppercase",
          }}
        >
          {props.entry.group ?? ""}
        </Text>
      </If>
      <SettingsRailButton
        icon={props.entry.icon ?? "panel"}
        label={props.entry.label}
        testID={`settings-section-${props.entry.id}`}
        wide={props.wide}
        selected={props.selected}
        onPress={() => props.onSelect(props.entry.id)}
      />
    </View>
  );
}

/** A rail row: 40px tall in the wide rail, a 40px square in the icon rail. */
function SettingsRailButton(props: {
  icon: IconName;
  label: string;
  testID: string;
  wide: boolean;
  selected?: boolean;
  onPress: () => void;
}) {
  return (
    <View
      testID={props.testID}
      focusable
      role="button"
      accessibilityLabel={props.label}
      accessibilityState={{ selected: props.selected ?? false }}
      onPress={props.onPress}
      hoverStyle={{ backgroundColor: color.hover }}
      activeStyle={{ backgroundColor: color.glassStateArmed }}
      style={{
        height: 40,
        flexDirection: "row",
        alignItems: "center",
        justifyContent: props.wide ? "flex-start" : "center",
        gap: 8,
        paddingHorizontal: props.wide ? 10 : 0,
        borderRadius: kitRadius.md,
        backgroundColor: props.selected ? color.glassState : undefined,
        borderWidth: props.selected ? 1 : 0,
        borderColor: props.selected ? color.glassStateRim : undefined,
        transitionProperty: "background-color",
        transitionDuration: motion.fast,
      }}
    >
      <Icon
        name={props.icon}
        size={16}
        tint={props.selected ? color.foreground : color.mutedForeground}
      />
      <If when={props.wide}>
        <Text
          style={{
            fontSize: typeScale.base,
            fontWeight: props.selected ? 600 : 400,
            color: props.selected ? color.foreground : color.mutedForeground,
          }}
        >
          {props.label}
        </Text>
      </If>
    </View>
  );
}
