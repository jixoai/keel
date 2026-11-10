// C2 · Checkbox — shadcn/ui `Checkbox` on NSH's native `nsh/base` Checkbox
// (checkbox role, Space/Return activation, mixed state for AT are native).
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | checked | boolean? | false | the app owns the value |
// | indeterminate | boolean? | false | "select all" over a partial selection (`[-]`) |
// | onChange | (checked: boolean) => void? | – | press asks for the next value |
// | disabled | boolean? | false | no presses, dimmed, indicator dimmed |
// | label | string? | – | text after the box |
// | children | JSX.Element? | – | richer label content |
// | accessibilityLabel | string? | label | what screen readers say |
// | testID | string? | – | `id:` selector for tests |
//
// States: unchecked · checked (primary box + check) · indeterminate (dash) ·
// disabled (dimmed). TUI: renders `[x]` / `[ ]` / `[-]` in place of the box.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Checkbox as NativeCheckbox } from "nsh/base";
import { Icon } from "../../icons";
import { alpha, color, radius, space, typeScale } from "../tokens";

export function Checkbox(props: {
  checked?: boolean;
  indeterminate?: boolean;
  onChange?: (checked: boolean) => void;
  disabled?: boolean;
  label?: string;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const filled = () => !!props.checked || !!props.indeterminate;
  return (
    <NativeCheckbox
      testID={props.testID}
      checked={props.checked}
      indeterminate={props.indeterminate}
      disabled={props.disabled}
      onChange={(event) => props.onChange?.(event.checked)}
      accessibilityLabel={props.accessibilityLabel ?? props.label}
      indicatorStyle={{
        width: 16,
        height: 16,
        borderRadius: radius.xs,
        borderWidth: 1,
        // Same rule as the radio ring: an unchecked box needs an edge that
        // reads on every card face (`color.input` is that face itself).
        borderColor: filled() ? color.primary : alpha(color.mutedForeground, 0.55),
        backgroundColor: filled() ? color.primary : "transparent",
        alignItems: "center",
        justifyContent: "center",
      }}
      indicator={
        <View style={{ width: 16, height: 16, alignItems: "center", justifyContent: "center" }}>
          {props.indeterminate ? (
            <View style={{ width: 8, height: 2, borderRadius: 1, backgroundColor: color.primaryForeground }} />
          ) : props.checked ? (
            <Icon name="check" size={11} tint={color.primaryForeground} />
          ) : null}
        </View>
      }
      hoverStyle={{ opacity: 0.92 }}
      disabledStyle={{ opacity: 0.5 }}
      style={[{ flexDirection: "row", alignItems: "center", gap: space.sm }, props.style ?? null]}
    >
      {props.label !== undefined ? (
        <Text
          style={{
            fontSize: typeScale.base,
            color: props.disabled ? color.mutedForeground : color.foreground,
          }}
        >
          {props.label}
        </Text>
      ) : null}
      {props.children}
    </NativeCheckbox>
  );
}
