// C2 · Switch — shadcn/ui `Switch` on NSH's native `nsh/base` Switch (the
// switch role, keyboard activation, disabled state and AT value are native; the
// track and thumb are the kit's tokens).
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | checked | boolean? | false | the app owns the value |
// | onChange | (checked: boolean) => void? | – | press asks for the opposite |
// | disabled | boolean? | false | no presses, dimmed, unfocusable |
// | size | "sm" \| "default" | "default" | 28×16 / 32×18 px |
// | label | string? | – | text after the track (read by AT as the switch's name) |
// | children | JSX.Element? | – | richer label content |
// | accessibilityLabel | string? | label | what screen readers say |
// | testID | string? | – | `id:` selector for tests |
//
// States: off · on (primary track, thumb moved) · disabled (dimmed, rejects
// presses). TUI: the native switch renders `[on ]` / `[off]` in place of the
// track, so the state is readable in tests.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text } from "nsh";
import { Switch as NativeSwitch } from "nsh/base";
import { alpha, color, radius, space, typeScale } from "../tokens";

export type SwitchSize = "sm" | "default";

export function Switch(props: {
  checked?: boolean;
  onChange?: (checked: boolean) => void;
  disabled?: boolean;
  size?: SwitchSize;
  label?: string;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const size = () => props.size ?? "default";
  const travel = () => (size() === "sm" ? 12 : 14);
  const thumb = () => (size() === "sm" ? 14 : 16);
  return (
    <NativeSwitch
      testID={props.testID}
      checked={props.checked}
      disabled={props.disabled}
      onChange={(event) => props.onChange?.(event.checked)}
      accessibilityLabel={props.accessibilityLabel ?? props.label}
      trackStyle={{
        width: size() === "sm" ? 28 : 32,
        height: size() === "sm" ? 16 : 18,
        borderRadius: radius.pill,
        padding: 1,
        backgroundColor: props.checked ? color.primary : alpha(color.primary, 0.3),
        justifyContent: "center",
        borderWidth: props.checked ? 0 : 1,
        borderColor: alpha(color.primary, 0.4),
      }}
      thumbStyle={{
        width: thumb(),
        height: thumb(),
        borderRadius: radius.pill,
        backgroundColor: props.checked ? color.primaryForeground : color.mutedForeground,
        transform: props.checked ? `translateX(${travel()}px)` : "translateX(0px)",
        transitionProperty: "transform, background-color",
        transitionDuration: 120,
        transitionTimingFunction: "ease-out",
      }}
      hoverStyle={{ opacity: 0.92 }}
      disabledStyle={{ opacity: 0.5 }}
      style={[{ flexDirection: "row", alignItems: "center", gap: space.md }, props.style ?? null]}
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
    </NativeSwitch>
  );
}
