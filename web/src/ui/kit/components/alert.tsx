// C3 · Alert — shadcn/ui `Alert` (+ Title / Description / Action): an inline
// notice, not a modal. The material is the vendor's quiet pane: the thinnest
// glass for the neutral kind, the same pane tinted the colour of failure for
// `destructive`. It never invents a second colour language: the icon, the title
// and the description all read the kit's tokens.
//
// Props — Alert
// | prop | type | default | notes |
// |---|---|---|---|
// | variant | "default" \| "destructive" | "default" | neutral pane or failure tint |
// | icon | IconName? | – | a leading glyph ("info", "alert", …) |
// | title / description | string? | – | the notice's words |
// | children | JSX.Element? | – | richer body |
// | action | JSX.Element? | – | a trailing control |
// | accessibilityLabel | string? | title | what screen readers call the notice |
// | testID | string? | – | `id:` selector for tests |
//
// Props — AlertTitle / AlertDescription: `label` (or `children`).
//
// States: default · destructive · title-only · with action. TUI: the title and
// description render as text rows and the action is pressable.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Icon, type IconName } from "../../icons";
import { alpha, color, lineHeight, radius, space, typeScale, weight } from "../tokens";
import { glassPanel } from "./styles";

export type AlertVariant = "default" | "destructive";

const alertFace = (variant: AlertVariant): StyleProp<ViewStyle> =>
  variant === "destructive"
    ? {
        backgroundColor: alpha(color.destructive, 0.1),
        borderWidth: 1,
        borderColor: alpha(color.destructive, 0.3),
        boxShadow: `inset 0 1px 0 ${alpha(color.destructive, 0.3)}`,
      }
    : { ...glassPanel(), borderRadius: radius.lg };

export function Alert(props: {
  variant?: AlertVariant;
  icon?: IconName;
  title?: string;
  description?: string;
  children?: JSX.Element;
  action?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const destructive = () => (props.variant ?? "default") === "destructive";
  const ink = () => (destructive() ? color.destructive : color.foreground);
  return (
    <View
      testID={props.testID}
      accessibilityLabel={props.accessibilityLabel ?? props.title}
      style={[
        {
          flexDirection: "row",
          alignItems: "flex-start",
          gap: space.md,
          padding: space.lg,
          borderRadius: radius.lg,
        },
        alertFace(props.variant ?? "default"),
        props.style ?? null,
      ]}
    >
      {props.icon !== undefined ? (
        <View style={{ paddingTop: 1 }}>
          <Icon name={props.icon} size={16} tint={ink()} />
        </View>
      ) : null}
      <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: space.xxs }}>
        {props.title !== undefined ? (
          <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: ink() }}>{props.title}</Text>
        ) : null}
        {props.description !== undefined ? (
          <Text
            style={{
              fontSize: typeScale.sm,
              lineHeight: Math.round(typeScale.sm * lineHeight.normal),
              color: destructive() ? color.destructive : color.mutedForeground,
            }}
          >
            {props.description}
          </Text>
        ) : null}
        {props.children}
      </View>
      {props.action !== undefined ? <View style={{ flexShrink: 0 }}>{props.action}</View> : null}
    </View>
  );
}

export function AlertTitle(props: { label?: string; children?: JSX.Element; testID?: string }) {
  return (
    <Text testID={props.testID} style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground }}>
      {props.label ?? ""}
      {props.children}
    </Text>
  );
}

export function AlertDescription(props: { label?: string; children?: JSX.Element; testID?: string }) {
  return (
    <Text
      testID={props.testID}
      style={{
        fontSize: typeScale.sm,
        lineHeight: Math.round(typeScale.sm * lineHeight.normal),
        color: color.mutedForeground,
      }}
    >
      {props.label ?? ""}
      {props.children}
    </Text>
  );
}
