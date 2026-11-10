// C1 · Badge — shadcn/ui `Badge` (radix-mira): a pill of one short status
// token, never a paragraph.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | variant | "default" \| "secondary" \| "destructive" \| "outline" \| "success" \| "warning" \| "info" \| "ghost" | "default" | tint families from the theme |
// | label | string? | – | text; omit when `children` carries it |
// | icon | IconName? | – | leading glyph in the tint's ink |
// | children | JSX.Element? | – | richer content (counters, dots) |
// | onPress | () => void? | – | interactive badge (filters, remove chips) |
// | disabled | boolean? | false | only meaningful with `onPress` |
// | accessibilityLabel | string? | label | what screen readers say |
// | testID | string? | – | `id:` selector for tests |
//
// States: default · hover (when pressable) · disabled (dimmed, unfocusable) ·
// empty (`label`/`children` absent renders the outline only). TUI: one short
// row; the label is what tests read.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Icon, type IconName } from "../../icons";
import { alpha, color, radius, space, typeScale, weight } from "../tokens";

export type BadgeVariant =
  | "default"
  | "secondary"
  | "destructive"
  | "outline"
  | "success"
  | "warning"
  | "info"
  | "ghost";

const badgeTint = (variant: BadgeVariant): { face: string; ink: string; border: string } => {
  switch (variant) {
    case "secondary":
      return { face: color.secondary, ink: color.secondaryForeground, border: "transparent" };
    case "destructive":
      return { face: alpha(color.destructive, 0.14), ink: color.destructive, border: alpha(color.destructive, 0.3) };
    case "success":
      return { face: color.successTint, ink: color.success, border: alpha(color.success, 0.3) };
    case "warning":
      return { face: color.warningTint, ink: color.warning, border: alpha(color.warning, 0.3) };
    case "info":
      return { face: color.infoTint, ink: color.info, border: alpha(color.info, 0.3) };
    case "outline":
      return { face: "transparent", ink: color.foreground, border: color.border };
    case "ghost":
      return { face: "transparent", ink: color.mutedForeground, border: "transparent" };
    case "default":
    default:
      return { face: color.primary, ink: color.primaryForeground, border: "transparent" };
  }
};

export function Badge(props: {
  variant?: BadgeVariant;
  label?: string;
  icon?: IconName;
  children?: JSX.Element;
  onPress?: () => void;
  disabled?: boolean;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const variant = () => props.variant ?? "default";
  const tint = () => badgeTint(variant());
  const clickable = () => props.onPress !== undefined;
  return (
    <View
      testID={props.testID}
      disabled={props.disabled}
      onPress={props.disabled || !clickable() ? undefined : props.onPress}
      role={clickable() ? "button" : "text"}
      accessibilityLabel={props.accessibilityLabel ?? props.label}
      accessibilityState={{ disabled: props.disabled }}
      hoverStyle={clickable() && !props.disabled ? { backgroundColor: alpha(tint().face, 0.85) } : undefined}
      focusStyle={clickable() ? { borderColor: color.ring } : undefined}
      style={[
        {
          flexDirection: "row",
          alignItems: "center",
          justifyContent: "center",
          gap: space.xxs,
          height: 20,
          paddingHorizontal: space.sm,
          borderRadius: radius.pill,
          backgroundColor: tint().face,
          borderWidth: 1,
          borderColor: tint().border,
        },
        props.disabled ? { opacity: 0.5 } : null,
        props.style ?? null,
      ]}
    >
      {props.icon ? <Icon name={props.icon} size={11} tint={tint().ink} /> : null}
      {props.label ? (
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: tint().ink }}>{props.label}</Text>
      ) : null}
      {props.children}
    </View>
  );
}
