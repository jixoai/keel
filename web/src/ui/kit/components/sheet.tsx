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
import { safeAreaInsets, Text, View, windowSize } from "nsh";
import { Button as NativeButton, Sheet as NativeSheet, type OpenChangeReason } from "nsh/base";
import { Icon } from "../../icons";
import { alpha, color, radius, space, typeScale, weight } from "../tokens";
import { disabledOpacity, focusRing, glassPopover } from "./styles";

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
  const insets = safeAreaInsets();
  // Percentage caps do not survive the style pipeline; points from the window
  // keep a bottom sheet inside a short viewport (and a side sheet full-height).
  const panelMaxHeight = () =>
    vertical() ? windowSize().height : Math.max(240, Math.round(windowSize().height * 0.92) - space.lg);
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
          // A phone's bottom sheet keeps its last action above the home
          // indicator; the runtime gives the panel the window's safe area.
          paddingBottom: space.xl + insets().bottom,
          width: vertical() ? "92%" : "100%",
          maxWidth: vertical() ? 380 : undefined,
          maxHeight: panelMaxHeight(),
          height: vertical() ? "100%" : undefined,
          minHeight: vertical() ? undefined : 200,
          borderRadius: radius.xl,
          ...glassPopover(),
          overflow: "hidden",
          alignSelf: vertical() ? (side() === "left" ? "flex-start" : "flex-end") : "center",
        },
        props.style ?? null,
        // The window's own cap wins over a caller's percentage string (which
        // NSH drops), so a sheet can never grow past the screen.
        { maxHeight: panelMaxHeight() },
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
      {/* The panel body keeps a flex column; a long body brings its own
          `ScrollArea` (the callers' pattern), so the panel never nests two
          vertical scrollers. */}
      <View style={{ flexDirection: "column", gap: space.md, flex: 1, minHeight: 0 }}>{props.children}</View>
      {props.footer !== undefined ? (
        <View style={{ flexDirection: "row", flexWrap: "wrap", justifyContent: "flex-end", alignItems: "center", gap: space.md, rowGap: space.sm }}>{props.footer}</View>
      ) : null}
      </View>
    </NativeSheet>
  );
}
