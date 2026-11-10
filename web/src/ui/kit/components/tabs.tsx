// C3 · Tabs — shadcn/ui `Tabs` (+ List / Trigger / Content) on NSH's native
// `nsh/base` Tabs + Tab: the tab-list semantics, the arrow-key movement, the
// selected state and the natively-placed indicator are the component's; the kit
// carries the list's shell, the trigger faces and the indicator.
//
// Props — Tabs (the list)
// | prop | type | default | notes |
// |---|---|---|---|
// | variant | "default" \| "line" | "default" | pill list on `muted`, or an underline list |
// | children | JSX.Element? | – | the Tab triggers |
// | accessibilityLabel | string? | – | what screen readers call the list (wrapper) |
// | testID | string? | – | `id:` selector for tests |
//
// Props — Tab (the trigger)
// | prop | type | default | notes |
// |---|---|---|---|
// | label | string? | – | the tab's text |
// | icon | IconName? | – | leading glyph |
// | selected | boolean? | false | the current tab (app-owned) |
// | disabled | boolean? | false | cannot be pressed; dimmed |
// | onPress | () => void? | – | select this tab (also fired by arrow keys) |
//
// Props — TabsContent: `value` (this panel's tab), `current` (the selected
// tab), `children` — renders only while they match (shadcn's `TabsContent`).
//
// States: selected · unselected · disabled · hover. TUI: the terminal draws the
// selected tab bold (an underline when the app underlines its label).
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Tab as NativeTab, Tabs as NativeTabs } from "nsh/base";
import { Icon, type IconName } from "../../icons";
import { color, radius, space, typeScale, weight } from "../tokens";

export type TabsVariant = "default" | "line";

export function Tabs(props: {
  variant?: TabsVariant;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const line = () => (props.variant ?? "default") === "line";
  return (
    <View
      testID={props.testID}
      role="tablist"
      accessibilityLabel={props.accessibilityLabel}
      style={{ flexDirection: "column" }}
    >
      <NativeTabs
        indicatorStyle={
          line()
            ? {
                height: 2,
                bottom: 0,
                borderRadius: radius.pill,
                backgroundColor: color.foreground,
              }
            : {
                top: 2,
                bottom: 2,
                borderRadius: radius.sm,
                backgroundColor: color.card,
              }
        }
        style={{
          flexDirection: "row",
          alignItems: "center",
          gap: line() ? space.md : space.xxs,
          padding: line() ? 0 : 3,
          borderRadius: line() ? 0 : radius.lg,
          backgroundColor: line() ? "transparent" : color.muted,
          borderBottomWidth: line() ? 1 : 0,
          borderBottomColor: color.border,
        }}
      >
        {props.children}
      </NativeTabs>
    </View>
  );
}

export function Tab(props: {
  label?: string;
  icon?: IconName;
  selected?: boolean;
  disabled?: boolean;
  onPress?: () => void;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <NativeTab
      testID={props.testID}
      selected={props.selected}
      disabled={props.disabled}
      onPress={props.onPress}
      accessibilityLabel={props.accessibilityLabel ?? props.label}
      hoverStyle={{ backgroundColor: props.selected ? undefined : color.hover }}
      style={[
        {
          flexDirection: "row",
          alignItems: "center",
          justifyContent: "center",
          gap: space.xs,
          height: 26,
          paddingHorizontal: space.lg,
          borderRadius: radius.md,
        },
        props.style ?? null,
      ]}
    >
      {props.icon ? (
        <Icon name={props.icon} size={14} tint={props.selected ? color.foreground : color.mutedForeground} />
      ) : null}
      {props.label !== undefined ? (
        <Text
          style={{
            fontSize: typeScale.base,
            fontWeight: props.selected ? weight.medium : weight.regular,
            color: props.disabled ? color.mutedForeground : props.selected ? color.foreground : color.mutedForeground,
          }}
        >
          {props.label}
        </Text>
      ) : null}
      {props.children}
    </NativeTab>
  );
}

export function TabsContent(props: { value: string; current?: string; children?: JSX.Element; testID?: string }) {
  return (
    <If when={props.value === props.current}>
      <View testID={props.testID} style={{ paddingTop: space.lg, flexDirection: "column" }}>
        {props.children}
      </View>
    </If>
  );
}
