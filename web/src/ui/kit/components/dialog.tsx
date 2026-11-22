// C4 · Dialog — shadcn/ui `Dialog` (+ Header / Title / Description / Footer) on
// NSH's native `nsh/base` Dialog: Escape / backdrop / (iOS) swipe closing, the
// modal layer and its semantics are the runtime's; the kit carries the popup
// card, the header stack and the footer row.
//
// Placement: NSH gives the dialog layer a plain `position: fixed; inset: 0`
// box with no flex parent, so the popup would land in the top-left corner. The
// native popup here is a transparent full-window flex container with
// `pointerEvents="box-none"` (presses outside the card fall through to the
// backdrop); the visible card is its single child, centered on both axes. This
// is the wrapper form the runtime supports: no transform is used, so the
// native open/close animation cannot overwrite the centering, and the card's
// width / max height / scrolling are its own.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | open | boolean? | true | shown while true (the app owns it) |
// | onOpenChange | (e) => void? | – | reports a close attempt: set `open` from it |
// | title / description | string? | – | header stack inside the popup |
// | children | JSX.Element? | – | the body (scrolls when the card overflows) |
// | footer | JSX.Element? | – | action row (right-aligned Buttons) |
// | size | "sm" \| "default" \| "lg" | "default" | 360 / 440 / 560 px (capped at 90%) |
// | showClose | boolean? | true | an × in the corner (closes via `onOpenChange`) |
// | closeOnEscape / closeOnBackdropPress | boolean? | true | native close policy |
// | fill | boolean? | false | the body takes the dialog's remaining height |
// | accessibilityLabel | string? | title | what screen readers call the modal |
// | testID | string? | – | `id:` selector for tests |
//
// States: open · closing (native exit) · closed (`open={false}` renders
// nothing) · with/without footer · scrollable body (`maxHeight`). TUI: the popup
// renders as a block with the title/description/body text.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Scroll, safeAreaInsets, Text, View, windowSize } from "nsh";
import { Dialog as NativeDialog, Button as NativeButton } from "nsh/base";
import { Icon } from "../../icons";
import { color, radius, space, typeScale, weight } from "../tokens";
import { disabledOpacity, focusRing, glassPopover } from "./styles";

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
  /** The body takes the dialog's remaining height (a fullscreen surface): the
   * region and body wrappers grow, so a `flex: 1` child can fill them. */
  fill?: boolean;
  maxHeight?: number;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const fill = props.fill === true ? { flex: 1, minHeight: 0 } : null;
  const insets = safeAreaInsets();
  // A max-height is not carried through NSH's style pipeline on this target.
  // The card therefore sits in a fixed-height Scroll viewport with
  // `centerContent`: short cards are centered, tall cards scroll as one card.
  // The viewport is transparent and `box-none`, so a press outside the card is
  // still the backdrop's.
  const cardMaxHeight = () => {
    const windowCap = Math.max(240, Math.round(windowSize().height * 0.9) - space["2xl"]);
    return props.maxHeight === undefined ? windowCap : Math.min(props.maxHeight, windowCap);
  };
  return (
    <NativeDialog
      open={props.open ?? true}
      testID={props.testID}
      // Fade, not the native slide: the popup is the full-window centering
      // wrapper, and sliding that wrapper would move the scrim's card from the
      // bottom edge instead of floating it in place.
      animation="fade"
      closeOnEscape={props.closeOnEscape ?? true}
      closeOnBackdropPress={props.closeOnBackdropPress ?? true}
      onOpenChange={(event) => props.onOpenChange?.(event)}
      onClose={props.onClose}
      // The scrim: the theme's own shade at full strength (60% black in the
      // dark appearance), plus the reference's `backdrop-blur-xs` so what is
      // behind the modal visibly recedes instead of merely dimming.
      backdropStyle={{ backgroundColor: color.glassShade, backdropFilter: "blur(4px)" }}
      backdropTestID={props.testID !== undefined ? `${props.testID}-backdrop` : undefined}
      style={{
        position: "absolute",
        top: 0,
        right: 0,
        bottom: 0,
        left: 0,
        // A definite height lets the card's own max-height resolve against it
        // (a percentage cap of an inset-only box is indefinite in layout).
        height: windowSize().height,
        alignItems: "center",
        justifyContent: "center",
        paddingHorizontal: space.lg,
        paddingVertical: space["2xl"],
        // The wrapper draws nothing and only its card takes presses: a press
        // on the transparent margin is the backdrop's (close) press.
        pointerEvents: "box-none",
      }}
    >
      <View
        style={{
          width: "100%",
          height: cardMaxHeight(),
          minHeight: 0,
          flexShrink: 0,
          overflow: "hidden",
          pointerEvents: "box-none",
          alignItems: "center",
          justifyContent: "center",
        }}
      >
      <Scroll
        centerContent
        style={{ flex: 1, width: "100%", pointerEvents: "box-none" }}
        contentStyle={{
          flexDirection: "column",
          alignItems: "center",
          justifyContent: "center",
          width: "100%",
          minHeight: "100%",
          pointerEvents: "none",
        }}
      >
      <View
        testID={props.testID !== undefined ? `${props.testID}-card` : undefined}
        style={[
          {
            width: "100%",
            maxWidth: dialogWidth(props.size ?? "default"),
            minWidth: 0,
            pointerEvents: "auto",
            flexDirection: "column",
            gap: space.xl,
            padding: space.xl,
            // A phone's bottom safe area is inside the dialog layer; the card
            // keeps its last action above the home indicator.
            paddingBottom: space.xl + insets().bottom,
            borderRadius: radius.xl,
            ...glassPopover(),
            overflow: "hidden",
          },
          props.style ?? null,
        ]}
      >
        <View
          role="region"
          accessibilityLabel={props.accessibilityLabel ?? props.title}
          style={[{ flexDirection: "column", gap: space.xl }, fill]}
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
          {/* The card's Scroll viewport owns the scroll for a long dialog. */}
          <View style={[{ flexDirection: "column", gap: space.md }, fill]}>{props.children}</View>
          {props.footer !== undefined ? (
            <View style={{ flexDirection: "row", justifyContent: "flex-end", gap: space.md }}>{props.footer}</View>
          ) : null}
        </View>
      </View>
      </Scroll>
      </View>
    </NativeDialog>
  );
}
