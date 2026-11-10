// C3 · Card — shadcn/ui `Card` (+ Header / Title / Description / Content /
// Footer): the one container every surface uses, so padding and borders do not
// drift per screen.
//
// Props — Card
// | prop | type | default | notes |
// |---|---|---|---|
// | size | "default" \| "sm" | "default" | sm drops the padding one step |
// | children | JSX.Element? | – | Header / Content / Footer |
// | accessibilityLabel | string? | – | what screen readers call the group |
// | testID | string? | – | `id:` selector for tests |
// | style | StyleProp<ViewStyle>? | – | overrides |
//
// Props — CardHeader
// | prop | type | default | notes |
// |---|---|---|---|
// | title | string? | – | the card's title |
// | description | string? | – | muted line under the title |
// | action | JSX.Element? | – | trailing control (a menu, a button) |
// | children | JSX.Element? | – | richer content |
//
// Props — CardContent / CardFooter: `children` + `style`. `CardFooter` is a row.
//
// States: default · with action · with description · footer-less. TUI: a bordered
// block whose title/description/content render as text rows.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { color, radius, space, typeScale, weight } from "../tokens";

export type CardSize = "default" | "sm";

export function Card(props: {
  size?: CardSize;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const pad = () => (props.size === "sm" ? space.lg : space.xl);
  return (
    <View
      testID={props.testID}
      accessibilityLabel={props.accessibilityLabel}
      style={[
        {
          flexDirection: "column",
          gap: props.size === "sm" ? space.md : space.lg,
          padding: pad(),
          borderRadius: radius.lg,
          borderWidth: 1,
          borderColor: color.border,
          backgroundColor: color.card,
          overflow: "hidden",
        },
        props.style ?? null,
      ]}
    >
      {props.children}
    </View>
  );
}

export function CardHeader(props: {
  title?: string;
  description?: string;
  action?: JSX.Element;
  children?: JSX.Element;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <View
      testID={props.testID}
      style={[
        { flexDirection: "row", alignItems: "flex-start", justifyContent: "space-between", gap: space.md },
        props.style ?? null,
      ]}
    >
      <View style={{ flexDirection: "column", gap: space.xxs, flex: 1 }}>
        {props.title !== undefined ? (
          <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.cardForeground }}>
            {props.title}
          </Text>
        ) : null}
        {props.description !== undefined ? (
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{props.description}</Text>
        ) : null}
        {props.children}
      </View>
      {props.action}
    </View>
  );
}

export function CardContent(props: { children?: JSX.Element; testID?: string; style?: StyleProp<ViewStyle> }) {
  return (
    <View testID={props.testID} style={[{ flexDirection: "column", gap: space.md }, props.style ?? null]}>
      {props.children}
    </View>
  );
}

export function CardFooter(props: { children?: JSX.Element; testID?: string; style?: StyleProp<ViewStyle> }) {
  return (
    <View
      testID={props.testID}
      style={[{ flexDirection: "row", alignItems: "center", gap: space.md }, props.style ?? null]}
    >
      {props.children}
    </View>
  );
}
