// C4 · Popover — shadcn/ui `Popover` on NSH's native `nsh/base` Popover: the
// placement against the trigger, keeping inside the safe area, press-outside /
// Escape closing are the component's; the kit carries the popup surface.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | trigger | JSX.Element? | – | what opens it (a Button, an IconButton) |
// | children | JSX.Element? | – | the popup's content |
// | open | boolean? | – | controlled; omit to let the popover keep its own state |
// | placement | "top" \| "bottom" \| "left" \| "right" | "bottom" | preferred side |
// | align | "start" \| "center" \| "end" | "center" | alignment along that side |
// | width | number? | 280 | popup width (min width) |
// | closeOnEscape / closeOnPressOutside | boolean? | true | closing policy |
// | onOpenChange | (e: { open, reason }) => void? | – | reports every open/close (required when `open` is controlled) |
// | accessibilityLabel | string? | – | what screen readers call the popup |
// | testID | string? | – | `id:` selector for tests |
//
// States: closed (trigger only) · open · closing (native exit) · empty content
// (renders an empty popup box). TUI: the popup's text renders near the trigger.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Popover as NativePopover, type OpenChangeReason } from "nsh/base";
import { View } from "nsh";
import { alpha, color, radius, space } from "../tokens";

export function Popover(props: {
  trigger?: JSX.Element;
  children?: JSX.Element;
  open?: boolean | null;
  defaultOpen?: boolean;
  placement?: "top" | "bottom" | "left" | "right";
  align?: "start" | "center" | "end";
  width?: number;
  offset?: number;
  closeOnEscape?: boolean;
  closeOnPressOutside?: boolean;
  onOpenChange?: (event: { open: boolean; reason: OpenChangeReason }) => void;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <NativePopover
      open={props.open ?? null}
      defaultOpen={props.defaultOpen}
      trigger={props.trigger}
      placement={props.placement ?? "bottom"}
      align={props.align ?? "center"}
      offset={props.offset ?? 6}
      margin={8}
      closeOnEscape={props.closeOnEscape ?? true}
      closeOnPressOutside={props.closeOnPressOutside ?? true}
      onOpenChange={(event) => props.onOpenChange?.(event)}
      popupStyle={[
        {
          minWidth: props.width ?? 280,
          maxWidth: 420,
          padding: space.lg,
          borderRadius: radius.lg,
          borderWidth: 1,
          borderColor: color.popoverEdge,
          backgroundColor: color.popover,
          boxShadow: `0 16px 40px ${alpha(color.glassShade, 1)}`,
        },
        props.style ?? null,
      ]}
    >
      <View accessibilityLabel={props.accessibilityLabel} testID={props.testID} style={{ flexDirection: "column", gap: space.md }}>
        {props.children}
      </View>
    </NativePopover>
  );
}
