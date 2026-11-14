// C2 · Field — shadcn/ui `Field`: the label / control / hint / error stack that
// every form row uses, so error placement and spacing are one thing.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | label | string? | – | names the control (a kit `Label`) |
// | hint | string? | – | quiet description under the control |
// | error | string? | – | replaces the hint; destructive ink + alert glyph, `role="alert"` |
// | required | boolean? | false | marks the label |
// | disabled | boolean? | false | dims the label and hint |
// | children | JSX.Element? | – | the control itself (Input, Select, Switch, …) |
// | orientation | "vertical" \| "horizontal" | "vertical" | horizontal keeps the label beside the control |
// | accessibilityLabel | string? | label | what screen readers call the group |
// | testID | string? | – | `id:` selector for tests |
//
// States: default · with hint · with error (invalid) · disabled. TUI: label row,
// control, then one hint or error row — all readable via `texts()`.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Icon } from "../../icons";
import { color, lineHeight, radius, space, typeScale } from "../tokens";
import { Label } from "./label";

export function Field(props: {
  label?: string;
  hint?: string;
  error?: string;
  required?: boolean;
  disabled?: boolean;
  children?: JSX.Element;
  orientation?: "vertical" | "horizontal";
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const invalid = () => props.error !== undefined && props.error !== "";
  const horizontal = () => props.orientation === "horizontal";
  return (
    <View
      testID={props.testID}
      accessibilityLabel={props.accessibilityLabel ?? props.label}
      style={[
        {
          flexDirection: horizontal() ? "row" : "column",
          alignItems: horizontal() ? "center" : "stretch",
          gap: horizontal() ? space.lg : space.xs,
        },
        props.style ?? null,
      ]}
    >
      {props.label !== undefined ? (
        <Label label={props.label} required={props.required} disabled={props.disabled} invalid={invalid()} />
      ) : null}
      <View style={{ flex: horizontal() ? 1 : undefined, flexDirection: "column", gap: space.xxs }}>
        {props.children}
        {invalid() ? (
          <View
            role="alert"
            accessibilityLabel={props.error}
            style={{ flexDirection: "row", alignItems: "center", gap: space.xs, paddingTop: space.xxs }}
          >
            <Icon name="alert" size={12} tint={color.destructive} />
            <Text
              style={{
                fontSize: typeScale.sm,
                // A hint or an error is prose that wraps: at least 1.4 (the
                // host's default lands near 1.25), so a two-line message stays
                // readable.
                lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                color: color.destructive,
                borderRadius: radius.none,
              }}
            >
              {props.error ?? ""}
            </Text>
          </View>
        ) : props.hint !== undefined ? (
          <Text
            style={{
              fontSize: typeScale.sm,
              lineHeight: Math.round(typeScale.sm * lineHeight.normal),
              color: color.mutedForeground,
              paddingTop: space.xxs,
            }}
          >
            {props.hint}
          </Text>
        ) : null}
      </View>
    </View>
  );
}

/** A stack of `Field`s (shadcn's `FieldGroup`): the form body. */
export function FieldGroup(props: { children?: JSX.Element; style?: StyleProp<ViewStyle>; testID?: string }) {
  return (
    <View testID={props.testID} style={[{ flexDirection: "column", gap: space.lg }, props.style ?? null]}>
      {props.children}
    </View>
  );
}
