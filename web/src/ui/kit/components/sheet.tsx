// C4 · Sheet — shadcn/ui `Sheet` (a panel that slides in from an edge) on
// NSH's native `nsh/base` Sheet: the slide-in, the backdrop, Escape/back/swipe
// closing are the runtime's; the kit carries the panel's face per side.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | open | boolean? | true | shown while true (the app owns it) |
// | side | "top" \| "bottom" \| "left" \| "right" | "bottom" | the edge it slides from |
// | onOpenChange | (e) => void? | – | reports a close attempt |
// | title / description | string? | – | header stack inside the panel |
// | children | JSX.Element? | – | the body |
// | footer | JSX.Element? | – | action row (right-aligned) |
// | showClose | boolean? | true | an × in the header |
// | closeOnEscape / closeOnBackdropPress / closeOnSwipe | boolean? | true | native policy |
// | accessibilityLabel | string? | title | what screen readers call the panel |
// | testID | string? | – | `id:` selector for tests |
//
// States: open · closing (native exit) · closed (`open={false}` renders
// nothing) · each of the four sides. TUI: the panel renders as a block with the
// title/description/body text.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Button as NativeButton, Sheet as NativeSheet, type OpenChangeReason } from "nsh/base";
import { Icon } from "../../icons";
import { alpha, color, radius, space, typeScale, weight } from "../tokens";
import { disabledOpacity, focusRing } from "./styles";

export type SheetSide = "top" | "bottom" | "left" | "right";

export function Sheet(props: {
  open?: boolean;
  side?: SheetSide;
  onOpenChange?: (event: { open: boolean; reason: OpenChangeReason }) => void;
  onClose?: () => void;
  title?: string;
  description?: string;
  children?: JSX.Element;
  footer?: JSX.Element;
  showClose?: boolean;
  closeOnEscape?: boolean;
  closeOnBackdropPress?: boolean;
  closeOnSwipe?: boolean;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const side = () => props.side ?? "bottom";
  const vertical = () => side() === "left" || side() === "right";
  return (
    <NativeSheet
      open={props.open ?? true}
      side={side()}
      testID={props.testID}
      closeOnEscape={props.closeOnEscape ?? true}
      closeOnBackdropPress={props.closeOnBackdropPress ?? true}
      closeOnSwipe={props.closeOnSwipe ?? true}
      onOpenChange={(event) => props.onOpenChange?.(event)}
      onClose={props.onClose}
      backdropStyle={{ backgroundColor: alpha(color.glassShade, 0.5) }}
      popupStyle={[
        {
          flexDirection: "column",
          gap: space.lg,
          padding: space.xl,
          backgroundColor: color.popover,
          borderColor: color.popoverEdge,
          boxShadow: `0 24px 64px ${alpha(color.glassShade, 1)}`,
          width: vertical() ? 380 : "100%",
          height: vertical() ? "100%" : undefined,
          minHeight: vertical() ? undefined : 200,
          borderRadius:
            side() === "bottom"
              ? radius.xl
              : side() === "top"
                ? radius.xl
                : radius.xl,
          borderWidth: 1,
          alignSelf: vertical() ? (side() === "left" ? "flex-start" : "flex-end") : "center",
        },
        props.style ?? null,
      ]}
    >
      <View
        role="region"
        accessibilityLabel={props.accessibilityLabel ?? props.title}
        style={{ flexDirection: "column", gap: space.lg, flex: 1 }}
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
            style={{ width: 24, height: 24, alignItems: "center", justifyContent: "center", borderRadius: radius.sm }}
          >
            <Icon name="x" size={14} tint={color.mutedForeground} />
          </NativeButton>
        ) : null}
      </View>
      <View style={{ flexDirection: "column", gap: space.md, flex: 1 }}>{props.children}</View>
      {props.footer !== undefined ? (
        <View style={{ flexDirection: "row", justifyContent: "flex-end", gap: space.md }}>{props.footer}</View>
      ) : null}
      </View>
    </NativeSheet>
  );
}
