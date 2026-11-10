// C2 · Input — shadcn/ui `Input` on NSH's native text field (`nsh/base`
// Input): typing, caret, selection, IME, submit — all native; the kit carries
// the shell (height, radius, border, padding) and the sizes.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | size | "sm" \| "default" \| "lg" | "default" | 24 / 28 / 32 px |
// | value | string? | – | controlled text |
// | defaultValue | string? | – | first text when uncontrolled |
// | placeholder | string? | – | shown while empty |
// | masked | boolean? | false | password-style hiding |
// | invalid | boolean? | false | error state: destructive border + ring |
// | disabled | boolean? | false | no focus, no typing; dimmed |
// | onSubmit | (text: string) => void? | – | Return (with `submitBehavior`) |
// | onChange | (value: string) => void? | – | every edit |
// | maxLength / readonly / autoFocus | number/boolean? | – | native pass-through |
// | accessibilityLabel | string? | placeholder | what screen readers say |
// | testID | string? | – | `id:` selector for tests (also how `type` finds it) |
//
// States: default · hover (border lightens) · focus ring · disabled (dimmed,
// unfocusable) · invalid (destructive ring) · empty (placeholder shows). TUI:
// one editable row; `type("id:…")` types into it.
import type { StyleProp } from "nsh";
import type { InputStyle } from "nsh";
import { Input as NativeInput } from "nsh/base";
import { alpha, color, space, typeScale } from "../tokens";
import { controlHeight, controlPadding, controlRadius, focusRing, invalidRing, type ControlSize } from "./styles";

export function Input(props: {
  size?: ControlSize;
  value?: string;
  defaultValue?: string;
  placeholder?: string;
  masked?: boolean;
  invalid?: boolean;
  disabled?: boolean;
  multiline?: boolean;
  maxLength?: number;
  readonly?: boolean;
  autoFocus?: boolean;
  submitBehavior?: "submit" | "blurAndSubmit" | "newline";
  onChange?: (value: string) => void;
  onSubmit?: (text: string) => void;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<InputStyle>;
}) {
  const size = () => props.size ?? "default";
  return (
    <NativeInput
      testID={props.testID}
      value={props.value}
      defaultValue={props.defaultValue}
      placeholder={props.placeholder}
      masked={props.masked}
      disabled={props.disabled}
      multiline={props.multiline}
      maxLength={props.maxLength}
      readonly={props.readonly}
      autoFocus={props.autoFocus}
      submitBehavior={props.submitBehavior}
      onChange={(event) => props.onChange?.(event.value)}
      onSubmit={(event) => props.onSubmit?.(event.text)}
      accessibilityLabel={props.accessibilityLabel ?? props.placeholder}
      hoverStyle={{ borderColor: color.ring }}
      focusStyle={props.invalid ? invalidRing() : focusRing()}
      disabledStyle={{ opacity: 0.5 }}
      style={[
        {
          height: controlHeight(size()),
          borderRadius: controlRadius(size()),
          borderWidth: 1,
          borderColor: color.input,
          backgroundColor: color.field,
          paddingHorizontal: controlPadding(size()),
          fontSize: typeScale.base,
          color: color.foreground,
          placeholderColor: color.mutedForeground,
          caretColor: color.foreground,
          selectionColor: alpha(color.primary, 0.25),
        },
        props.invalid ? invalidRing() : null,
        props.style ?? null,
      ]}
    />
  );
}
