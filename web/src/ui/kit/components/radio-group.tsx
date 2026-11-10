// C2 · RadioGroup — shadcn/ui `RadioGroup` on NSH's native `nsh/base`
// RadioGroup + Radio (the radiogroup role, arrow-key movement, the checked
// report and the AT state are native; the app owns which value is checked).
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | value | string? | – | the checked option's value (app-owned) |
// | options | { value, label, disabled? }[] | – | the set, in order |
// | onChange | (value: string) => void? | – | a press or an arrow asks for this value |
// | disabled | boolean? | false | the whole group is disabled |
// | orientation | "vertical" \| "horizontal" | "vertical" | layout of the options |
// | accessibilityLabel | string? | – | what screen readers call the set (on the wrapper) |
// | testID | string? | – | `id:` selector for tests |
//
// States: unchecked · checked (primary ring + dot) · disabled (dimmed, rejects
// presses) · empty (`options` empty renders nothing). TUI: each option renders
// `(•)` / `( )` before its label.
import type { StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Radio, RadioGroup as NativeRadioGroup } from "nsh/base";
import { color, radius, space, typeScale } from "../tokens";

export function RadioGroup(props: {
  value?: string;
  options?: { value: string; label: string; disabled?: boolean }[];
  onChange?: (value: string) => void;
  disabled?: boolean;
  orientation?: "vertical" | "horizontal";
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const horizontal = () => props.orientation === "horizontal";
  return (
    <View
      testID={props.testID}
      role="radiogroup"
      accessibilityLabel={props.accessibilityLabel}
      style={{ flexDirection: "column" }}
    >
      <NativeRadioGroup
        style={{
          flexDirection: horizontal() ? "row" : "column",
          gap: horizontal() ? space.xl : space.sm,
        }}
      >
        {(props.options ?? []).map((option) => {
          const checked = () => props.value === option.value;
          const disabled = () => !!props.disabled || !!option.disabled;
          return (
            <Radio
              checked={checked()}
              disabled={disabled()}
              accessibilityLabel={option.label}
              onChange={() => props.onChange?.(option.value)}
              hoverStyle={{ opacity: 0.92 }}
              disabledStyle={{ opacity: 0.5 }}
              style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}
            >
              <View
                style={{
                  width: 16,
                  height: 16,
                  borderRadius: radius.pill,
                  borderWidth: 1,
                  borderColor: checked() ? color.primary : color.input,
                  alignItems: "center",
                  justifyContent: "center",
                }}
              >
                {checked() ? (
                  <View style={{ width: 8, height: 8, borderRadius: radius.pill, backgroundColor: color.primary }} />
                ) : null}
              </View>
              <Text style={{ fontSize: typeScale.base, color: disabled() ? color.mutedForeground : color.foreground }}>
                {option.label}
              </Text>
            </Radio>
          );
        })}
      </NativeRadioGroup>
    </View>
  );
}
