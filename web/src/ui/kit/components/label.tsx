// C2 · Label — shadcn/ui `Label`: the text that names a control.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | label | string? | – | the text; omit when `children` carries it |
// | required | boolean? | false | appends the required marker (`*`) |
// | disabled | boolean? | false | dimmed (the control it names is disabled) |
// | invalid | boolean? | false | destructive ink (the control it names is in error) |
// | children | JSX.Element? | – | richer content |
// | testID | string? | – | `id:` selector for tests |
//
// States: default · required · disabled (muted) · invalid (destructive).
// TUI: one text row; tests read the label text.
import type { JSX, StyleProp, TextStyle } from "nsh";
import { Text } from "nsh";
import { color, typeScale, weight } from "../tokens";

export function Label(props: {
  label?: string;
  required?: boolean;
  disabled?: boolean;
  invalid?: boolean;
  children?: JSX.Element;
  testID?: string;
  style?: StyleProp<TextStyle>;
}) {
  const ink = () =>
    props.disabled ? color.mutedForeground : props.invalid ? color.destructive : color.foreground;
  return (
    <Text
      testID={props.testID}
      accessibilityLabel={props.label}
      style={[{ fontSize: typeScale.base, fontWeight: weight.medium, color: ink() }, props.style ?? null]}
    >
      {props.label ?? ""}
      {props.required ? <Text style={{ color: color.destructive }}>{" *"}</Text> : null}
      {props.children}
    </Text>
  );
}
