// C4 · Dialog — shadcn/ui `Dialog` (+ Header / Title / Description / Footer) on
// NSH's native `nsh/base` Dialog: Escape / backdrop / (iOS) swipe closing, the
// modal layer and its semantics are the runtime's; the kit carries the popup
// card, the header stack and the footer row.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | open | boolean? | true | shown while true (the app owns it) |
// | onOpenChange | (e) => void? | – | reports a close attempt: set `open` from it |
// | title / description | string? | – | header stack inside the popup |
// | children | JSX.Element? | – | the body |
// | footer | JSX.Element? | – | action row (right-aligned Buttons) |
// | size | "sm" \| "default" \| "lg" | "default" | 360 / 440 / 560 px (capped at 90%) |
// | showClose | boolean? | true | an × in the corner (closes via `onOpenChange`) |
// | closeOnEscape / closeOnBackdropPress | boolean? | true | native close policy |
// | accessibilityLabel | string? | title | what screen readers call the modal |
// | testID | string? | – | `id:` selector for tests |
//
// States: open · closing (native exit) · closed (`open={false}` renders
// nothing) · with/without footer · scrollable body (`maxHeight`). TUI: the popup
// renders as a block with the title/description/body text.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Dialog as NativeDialog, Button as NativeButton } from "nsh/base";
import { Icon } from "../../icons";
import { alpha, color, radius, space, typeScale, weight } from "../tokens";
import { disabledOpacity, focusRing } from "./styles";

export type DialogSize = "sm" | "default" | "lg";

const dialogWidth = (size: DialogSize): number => (size === "sm" ? 360 : size === "lg" ? 560 : 440);

export function Dialog(props: {
  open?: boolean;
  onOpenChange?: (event: { open: boolean; reason: "cancel" | "backdropPress" | "swipe" }) => void;
  onClose?: () => void;
  title?: string;
  description?: string;
  children?: JSX.Element;
  footer?: JSX.Element;
  size?: DialogSize;
  showClose?: boolean;
  closeOnEscape?: boolean;
  closeOnBackdropPress?: boolean;
  maxHeight?: number;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <NativeDialog
      open={props.open ?? true}
      testID={props.testID}
      closeOnEscape={props.closeOnEscape ?? true}
      closeOnBackdropPress={props.closeOnBackdropPress ?? true}
      onOpenChange={(event) => props.onOpenChange?.(event)}
      onClose={props.onClose}
      backdropStyle={{ backgroundColor: alpha(color.glassShade, 0.55) }}
      backdropTestID={props.testID !== undefined ? `${props.testID}-backdrop` : undefined}
      style={[
        {
          width: dialogWidth(props.size ?? "default"),
          maxWidth: "92%",
          maxHeight: props.maxHeight,
          flexDirection: "column",
          gap: space.xl,
          padding: space.xl,
          borderRadius: radius.xl,
          borderWidth: 1,
          borderColor: color.popoverEdge,
          backgroundColor: color.popover,
          boxShadow: `0 24px 64px ${alpha(color.glassShade, 1)}`,
        },
        props.style ?? null,
      ]}
    >
      <View
        role="region"
        accessibilityLabel={props.accessibilityLabel ?? props.title}
        style={{ flexDirection: "column", gap: space.xl }}
      >
      <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.md }}>
        <View style={{ flex: 1, flexDirection: "column", gap: space.xs }}>
          {props.title !== undefined ? (
            <Text style={{ fontSize: typeScale.lg, fontWeight: weight.semibold, color: color.popoverForeground }}>
              {props.title}
            </Text>
          ) : null}
          {props.description !== undefined ? (
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{props.description}</Text>
          ) : null}
        </View>
        {(props.showClose ?? true) ? (
          <NativeButton
            accessibilityLabel="关闭"
            onPress={() => props.onOpenChange?.({ open: false, reason: "cancel" })}
            hoverStyle={{ backgroundColor: color.hover }}
            focusStyle={focusRing()}
            disabledStyle={{ opacity: disabledOpacity }}
            style={{
              width: 24,
              height: 24,
              alignItems: "center",
              justifyContent: "center",
              borderRadius: radius.sm,
            }}
          >
            <Icon name="x" size={14} tint={color.mutedForeground} />
          </NativeButton>
        ) : null}
      </View>
      <View style={{ flexDirection: "column", gap: space.md }}>{props.children}</View>
      {props.footer !== undefined ? (
        <View style={{ flexDirection: "row", justifyContent: "flex-end", gap: space.md }}>{props.footer}</View>
      ) : null}
      </View>
    </NativeDialog>
  );
}
