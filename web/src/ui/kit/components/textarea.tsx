// C2 · Textarea — shadcn/ui `Textarea` on NSH's native multi-line field (the
// same `nsh/base` Input with `multiline`): wrapping, IME, Return-as-newline,
// the caret and the selection stay native.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | value / defaultValue | string? | – | controlled / first text |
// | placeholder | string? | – | shown while empty |
// | rows | number? | 3 | minimum visible lines (drives minHeight) |
// | autoGrow | boolean? | false | grows with the content (`onContentSizeChange`) |
// | maxLength | number? | – | native limit |
// | invalid | boolean? | false | destructive border + ring |
// | disabled | boolean? | false | dimmed, unfocusable |
// | onChange | (value: string) => void? | – | every edit |
// | accessibilityLabel | string? | placeholder | what screen readers say |
// | testID | string? | – | `id:` selector for tests |
//
// States: default · focus ring · disabled · invalid · empty (placeholder).
// TUI: a multi-row editable block; Return inserts a line break.
import type { StyleProp } from "nsh";
import type { InputStyle } from "nsh";
import { Input as NativeInput } from "nsh/base";
import { color, lineHeight, space, typeScale } from "../tokens";
import { controlRadius, focusRing, invalidRing } from "./styles";

export function Textarea(props: {
  value?: string;
  defaultValue?: string;
  placeholder?: string;
  rows?: number;
  autoGrow?: boolean;
  maxLength?: number;
  invalid?: boolean;
  disabled?: boolean;
  onChange?: (value: string) => void;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<InputStyle>;
}) {
  const minHeight = () => (props.rows ?? 3) * Math.round(typeScale.base * lineHeight.relaxed) + space.lg;
  return (
    <NativeInput
      testID={props.testID}
      value={props.value}
      defaultValue={props.defaultValue}
      placeholder={props.placeholder}
      multiline
      maxLength={props.maxLength}
      disabled={props.disabled}
      submitBehavior="newline"
      onChange={(event) => props.onChange?.(event.value)}
      accessibilityLabel={props.accessibilityLabel ?? props.placeholder}
      hoverStyle={{ borderColor: color.ring }}
      focusStyle={props.invalid ? invalidRing() : focusRing()}
      disabledStyle={{ opacity: 0.5 }}
      style={[
        {
          minHeight: minHeight(),
          borderRadius: controlRadius("default"),
          borderWidth: 1,
          borderColor: color.input,
          backgroundColor: color.field,
          paddingHorizontal: space.md,
          paddingVertical: space.sm,
          fontSize: typeScale.base,
          lineHeight: typeScale.base * lineHeight.relaxed,
          textAlignVertical: "top",
          color: color.foreground,
          placeholderColor: color.mutedForeground,
          caretColor: color.foreground,
          selectionColor: color.input,
        },
        props.invalid ? invalidRing() : null,
        props.style ?? null,
      ]}
    />
  );
}
