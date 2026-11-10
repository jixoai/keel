// C1 · Button — shadcn/ui `Button` (radix-mira), built on NSH's native press
// target (`nsh/base` Button: presses, keyboard activation, disabled/selected
// state, the button role) with the kit's tokens carrying the look.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | variant | "default" \| "secondary" \| "ghost" \| "outline" \| "destructive" \| "link" | "default" | shadcn's set; `link` is text-only |
// | size | "sm" \| "default" \| "lg" \| "icon" | "default" | 24 / 28 / 32 px; `icon` is square at 28 |
// | label | string? | – | text content; omit when `children` carries it |
// | icon | IconName? | – | leading lucide glyph, tinted with the variant's ink |
// | iconTrailing | IconName? | – | trailing glyph (menus, disclosure) |
// | children | JSX.Element? | – | richer content (badges, two-line labels) |
// | disabled | boolean? | false | no presses; `disabledStyle` + dimmed for AT |
// | invalid | boolean? | false | error state: destructive border + ring |
// | selected | boolean? | false | toggled trigger / current tab |
// | loading | boolean? | false | swaps the leading icon for a Spinner |
// | onPress | () => void? | – | press/click/Space/Return |
// | accessibilityLabel | string? | label | what screen readers say |
// | testID | string? | – | `id:` selector for tests |
// | style / hoverStyle / activeStyle / focusStyle | StyleProp<ViewStyle>? | – | overrides |
//
// States: default · hover (variant tint) · active · focus ring · disabled
// (dimmed, unfocusable) · invalid (destructive ring). TUI: the label renders as
// one row and disabled buttons reject `press` with ERR_NSH_TEST_DISABLED.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Button as NativeButton } from "nsh/base";
import { Icon, type IconName } from "../../icons";
import { alpha, color, motion, radius, space, typeScale, weight } from "../tokens";
import {
  controlHeight,
  controlIconSize,
  controlPadding,
  controlRadius,
  disabledOpacity,
  focusRing,
  invalidRing,
  type ControlSize,
} from "./styles";
import { Spinner } from "./spinner";

export type ButtonVariant = "default" | "secondary" | "ghost" | "outline" | "destructive" | "link";
export type ButtonSize = ControlSize | "icon";

/** One place that knows a variant's face, so hover/disabled/ink stay in step. */
export const buttonFace = (
  variant: ButtonVariant,
): { base: StyleProp<ViewStyle>; hover: StyleProp<ViewStyle>; ink: string; border: StyleProp<ViewStyle> } => {
  switch (variant) {
    case "secondary":
      return {
        base: { backgroundColor: color.secondary },
        hover: { backgroundColor: alpha(color.secondary, 0.8) },
        ink: color.secondaryForeground,
        border: {},
      };
    case "ghost":
      return { base: {}, hover: { backgroundColor: color.hover }, ink: color.foreground, border: {} };
    case "outline":
      return {
        base: { borderWidth: 1, borderColor: color.border, backgroundColor: "transparent" },
        hover: { backgroundColor: color.hover },
        ink: color.foreground,
        border: { borderWidth: 1, borderColor: color.border },
      };
    case "destructive":
      return {
        base: { backgroundColor: color.destructive },
        hover: { backgroundColor: alpha(color.destructive, 0.9) },
        ink: color.destructiveForeground,
        border: {},
      };
    case "link":
      return { base: {}, hover: { opacity: 0.8 }, ink: color.primary, border: {} };
    case "default":
    default:
      return {
        base: { backgroundColor: color.primary },
        hover: { backgroundColor: alpha(color.primary, 0.85) },
        ink: color.primaryForeground,
        border: {},
      };
  }
};

export function Button(props: {
  variant?: ButtonVariant;
  size?: ButtonSize;
  label?: string;
  icon?: IconName;
  iconTrailing?: IconName;
  children?: JSX.Element;
  disabled?: boolean;
  invalid?: boolean;
  selected?: boolean;
  loading?: boolean;
  onPress?: () => void;
  accessibilityLabel?: string;
  testID?: string;
  tooltip?: JSX.Element;
  style?: StyleProp<ViewStyle>;
  hoverStyle?: StyleProp<ViewStyle>;
  activeStyle?: StyleProp<ViewStyle>;
  focusStyle?: StyleProp<ViewStyle>;
}) {
  const variant = () => props.variant ?? "default";
  const size = () => props.size ?? "default";
  const face = () => buttonFace(variant());
  const square = () => size() === "icon";
  const dimension = () => (square() ? controlHeight("default") : controlHeight(size() as ControlSize));
  const iconPx = () => (square() ? controlIconSize("default") : controlIconSize(size() as ControlSize));

  return (
    <NativeButton
      testID={props.testID}
      disabled={props.disabled}
      selected={props.selected}
      onPress={props.onPress}
      accessibilityLabel={props.accessibilityLabel ?? props.label}
      tooltip={props.tooltip}
      hoverStyle={[face().hover, props.hoverStyle ?? null]}
      activeStyle={props.activeStyle ?? { opacity: 0.85 }}
      focusStyle={[focusRing(), props.focusStyle ?? null]}
      style={[
        {
          flexDirection: "row",
          alignItems: "center",
          justifyContent: "center",
          gap: space.xs,
          height: dimension(),
          minWidth: square() ? dimension() : undefined,
          paddingHorizontal: square() ? space.none : controlPadding(size() as ControlSize),
          borderRadius: controlRadius(square() ? "default" : (size() as ControlSize)),
        },
        variant() === "link" ? { backgroundColor: "transparent" } : null,
        face().base,
        props.invalid ? invalidRing() : null,
        props.style ?? null,
      ]}
      disabledStyle={{ opacity: disabledOpacity }}
    >
      {props.loading ? (
        <Spinner size={iconPx()} tint={face().ink} />
      ) : props.icon ? (
        <Icon name={props.icon} size={iconPx()} tint={face().ink} />
      ) : null}
      {props.label ? (
        <Text
          style={{
            fontSize: typeScale.base,
            fontWeight: weight.medium,
            color: face().ink,
            textDecorationLine: variant() === "link" ? "underline" : undefined,
          }}
        >
          {props.label}
        </Text>
      ) : null}
      {props.children}
      {props.iconTrailing ? <Icon name={props.iconTrailing} size={iconPx()} tint={face().ink} /> : null}
    </NativeButton>
  );
}

/** A vertical group of buttons that share edges (shadcn's `ButtonGroup` idea),
 * rendered as a row of kit Buttons with the middle radii cut. */
export function ButtonGroup(props: { children?: JSX.Element; style?: StyleProp<ViewStyle>; accessibilityLabel?: string }) {
  return (
    <View
      accessibilityLabel={props.accessibilityLabel}
      style={[{ flexDirection: "row", gap: space.xs, alignItems: "center" }, props.style ?? null]}
    >
      {props.children}
    </View>
  );
}

/** The primary icon affordance: a square, quiet Button at 28px. */
export function IconButton(props: {
  icon: IconName;
  label: string;
  onPress?: () => void;
  disabled?: boolean;
  selected?: boolean;
  variant?: ButtonVariant;
  size?: ControlSize;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <Button
      variant={props.variant ?? "ghost"}
      size={props.size === undefined ? "icon" : props.size}
      icon={props.icon}
      accessibilityLabel={props.label}
      disabled={props.disabled}
      selected={props.selected}
      onPress={props.onPress}
      testID={props.testID}
      style={props.style}
    />
  );
}
