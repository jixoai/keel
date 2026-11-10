// C4 · Breadcrumb — shadcn/ui `Breadcrumb` (+ List / Item / Link / Page /
// Separator / Ellipsis): where the current view sits, in one language.
//
// Props — Breadcrumb (the nav) / BreadcrumbList / BreadcrumbItem: `children`
// (+ `accessibilityLabel`, `testID`).
// Props — BreadcrumbLink
// | prop | type | default | notes |
// |---|---|---|---|
// | label | string? | – | the crumb's text |
// | onPress | () => void? | – | navigates (a crumb with no handler is plain text) |
// | disabled | boolean? | false | not navigable right now |
// | accessibilityLabel | string? | label | what screen readers say |
// Props — BreadcrumbPage: `label` — the current page (`aria-current`); it is
// never pressable.
// Props — BreadcrumbSeparator: `icon` (default `chevron-right`) or children.
// Props — BreadcrumbEllipsis: a collapsed middle ("…", `dots`).
//
// States: link (hover/focus) · current page (foreground, no press) · disabled
// link (dimmed, rejects presses) · collapsed (ellipsis). TUI: one row of crumbs
// separated by `›`-like glyphs; crumbs are pressable by their label.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Button as NativeButton } from "nsh/base";
import { Icon, type IconName } from "../../icons";
import { color, radius, space, typeScale } from "../tokens";
import { disabledOpacity } from "./styles";

export function Breadcrumb(props: {
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <View
      testID={props.testID}
      role="navigation"
      accessibilityLabel={props.accessibilityLabel ?? "面包屑"}
      style={[{ flexDirection: "row", alignItems: "center" }, props.style ?? null]}
    >
      {props.children}
    </View>
  );
}

export function BreadcrumbList(props: { children?: JSX.Element; style?: StyleProp<ViewStyle>; testID?: string }) {
  return (
    <View
      testID={props.testID}
      style={[
        { flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: space.sm },
        props.style ?? null,
      ]}
    >
      {props.children}
    </View>
  );
}

export function BreadcrumbItem(props: { children?: JSX.Element; style?: StyleProp<ViewStyle>; testID?: string }) {
  return (
    <View
      testID={props.testID}
      style={[{ flexDirection: "row", alignItems: "center", gap: space.sm }, props.style ?? null]}
    >
      {props.children}
    </View>
  );
}

export function BreadcrumbLink(props: {
  label?: string;
  onPress?: () => void;
  disabled?: boolean;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const plain = () => props.onPress === undefined;
  return (
    <NativeButton
      testID={props.testID}
      disabled={props.disabled ?? plain()}
      onPress={props.onPress}
      accessibilityLabel={props.accessibilityLabel ?? props.label}
      role={plain() ? "text" : "link"}
      hoverStyle={{ backgroundColor: color.hover }}
      focusStyle={{ borderColor: color.ring }}
      disabledStyle={plain() ? {} : { opacity: disabledOpacity }}
      style={[
        {
          flexDirection: "row",
          alignItems: "center",
          gap: space.xs,
          height: 24,
          paddingHorizontal: space.xs,
          borderRadius: radius.sm,
        },
        props.style ?? null,
      ]}
    >
      {props.label !== undefined ? (
        <Text style={{ fontSize: typeScale.base, color: plain() ? color.foreground : color.mutedForeground }}>
          {props.label}
        </Text>
      ) : null}
      {props.children}
    </NativeButton>
  );
}

export function BreadcrumbPage(props: { label?: string; children?: JSX.Element; testID?: string; style?: StyleProp<ViewStyle> }) {
  return (
    <View
      testID={props.testID}
      role="text"
      accessibilityState={{ current: "page" }}
      style={[{ flexDirection: "row", alignItems: "center", height: 24, paddingHorizontal: space.xs }, props.style ?? null]}
    >
      {props.label !== undefined ? (
        <Text style={{ fontSize: typeScale.base, color: color.foreground }}>{props.label}</Text>
      ) : null}
      {props.children}
    </View>
  );
}

export function BreadcrumbSeparator(props: { icon?: IconName; children?: JSX.Element; testID?: string }) {
  return (
    <View role="text" accessibilityElementsHidden testID={props.testID} style={{ alignItems: "center" }}>
      {props.icon ? (
        <Icon name={props.icon} size={12} tint={color.mutedForeground} />
      ) : (
        <Icon name="chevron-right" size={12} tint={color.mutedForeground} />
      )}
      {props.children}
    </View>
  );
}

export function BreadcrumbEllipsis(props: { label?: string; testID?: string }) {
  return (
    <View
      testID={props.testID}
      role="button"
      accessibilityLabel={props.label ?? "更多层级"}
      style={{ flexDirection: "row", alignItems: "center", height: 24, paddingHorizontal: space.xs }}
    >
      <Icon name="dots" size={14} tint={color.mutedForeground} />
    </View>
  );
}
