// C4 · Empty — shadcn/ui `Empty` (+ Header / Media / Title / Description /
// Content): the one empty state every list and panel uses, so "nothing here"
// looks the same everywhere.
//
// Props — Empty
// | prop | type | default | notes |
// |---|---|---|---|
// | children | JSX.Element? | – | Media / Title / Description / Content |
// | accessibilityLabel | string? | – | what screen readers call the region |
// | testID | string? | – | `id:` selector for tests |
//
// Props — EmptyMedia: `icon` (IconName) or `children`; `tone` picks the disc's
// tint ("neutral" \| "brand" \| "danger").
// Props — EmptyTitle / EmptyDescription: `label` (or `children`).
// Props — EmptyContent: `children` — the actions row.
//
// States: default (icon + title + description + actions) · icon-less · text-only
// (title alone) · with a primary action. TUI: the title and description render
// as text rows and the action is pressable.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Icon, type IconName } from "../../icons";
import { alpha, color, lineHeight, radius, space, typeScale, weight } from "../tokens";

export function Empty(props: {
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <View
      testID={props.testID}
      accessibilityLabel={props.accessibilityLabel}
      style={[
        {
          flexDirection: "column",
          alignItems: "center",
          justifyContent: "center",
          gap: space.lg,
          padding: space["3xl"],
        },
        props.style ?? null,
      ]}
    >
      {props.children}
    </View>
  );
}

export function EmptyMedia(props: { icon?: IconName; tone?: "neutral" | "brand" | "danger"; children?: JSX.Element }) {
  const tint = () =>
    props.tone === "brand" ? color.brand : props.tone === "danger" ? color.danger : color.mutedForeground;
  const face = () =>
    props.tone === "brand" ? color.brandTint : props.tone === "danger" ? color.dangerTint : color.muted;
  return (
    <View
      style={{
        width: 44,
        height: 44,
        borderRadius: radius.pill,
        alignItems: "center",
        justifyContent: "center",
        backgroundColor: face(),
        borderWidth: 1,
        borderColor: alpha(tint(), 0.25),
      }}
    >
      {props.icon ? <Icon name={props.icon} size={20} tint={tint()} /> : null}
      {props.children}
    </View>
  );
}

export function EmptyTitle(props: { label?: string; children?: JSX.Element; testID?: string }) {
  return (
    <Text
      testID={props.testID}
      style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground, textAlign: "center" }}
    >
      {props.label ?? ""}
      {props.children}
    </Text>
  );
}

/** The description is prose that wraps on a narrow column, so it carries a 1.5
 * line height: the host's default lands near 1.25, which packs a two-line
 * sentence into a block of unreadable ink. */
export function EmptyDescription(props: { label?: string; children?: JSX.Element; testID?: string }) {
  return (
    <Text
      testID={props.testID}
      style={{
        fontSize: typeScale.sm,
        lineHeight: Math.round(typeScale.sm * lineHeight.normal),
        color: color.mutedForeground,
        textAlign: "center",
      }}
    >
      {props.label ?? ""}
      {props.children}
    </Text>
  );
}

export function EmptyContent(props: { children?: JSX.Element; style?: StyleProp<ViewStyle> }) {
  return (
    <View style={[{ flexDirection: "row", alignItems: "center", gap: space.md }, props.style ?? null]}>
      {props.children}
    </View>
  );
}
