// L · ComposerDock — the conversation's input dock and its input surface.
//
// Parameters from the zcode reference (`ConversationTimeline.tsx`'s composer
// dock, `ChatPromptEditor.tsx`'s shell), item by item:
//
//   | item                | value                                                   |
//   |---------------------|---------------------------------------------------------|
//   | dock                | `sticky bottom-0`, full width, centered column           |
//   | column              | the conversation column's own inset, caps and shift      |
//   | shell inset         | px-4 pb-4 (16px), inside that column                     |
//   | input shell         | rounded-2xl (16px), 1px border, p-3 (12px), focus ring    |
//   | back to bottom      | a round button above the shell, centered on it            |
//
// The column resolves to the SAME width as the message column: the inset is
// taken off the wrapper's width (as the messages' block takes it off its own),
// and `px-4` is inner padding on the column — the reference puts one width
// class on both columns (ConversationTimeline.tsx:2043-2061, 2169-2174) and only
// `px-4 pb-4` on the dock's. Applying the inset as padding on a 100%-wide column
// would narrow the shell by it twice (the reviewer measured 800 vs 896 at a
// 1016px pane).
//
// `sticky` is a real NSH style (`ViewStyle.position`), so the dock sticks to the
// bottom of the scroll view around it — the reference's own arrangement: the
// dock is inside the timeline's scroll viewport. In the draft/empty layout the
// reference drops the sticky (the column is centered instead) and in the
// floating layout it is an absolute layer at the bottom edge; both are here.
// The one host where `sticky` is only approximated is the terminal (it is laid
// out on the cell grid): with no scroll viewport around it, the dock simply
// renders in flow at the end of the column, which is where it was scrolled to
// anyway.
//
// Platform notes:
//   · terminal: `px-4` and the 16px radius land on cell boundaries; the focus
//     ring is drawn as an outline, which the terminal shows as its own
//     selection/border treatment (it has no sub-cell rendering).
//   · web/desktop: as the reference.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { View, windowSize } from "nsh";
import { Icon } from "../../icons";
import { alpha, color, motion, zIndex } from "../tokens";
import {
  conversationColumnInsetPx,
  conversationColumnMaxWidthPx,
  conversationColumnOffsetPx,
} from "./conversation-column";

/** `rounded-2xl` on the input shell. */
export const COMPOSER_SHELL_RADIUS_PX = 16;
/** `p-3` on the input shell. */
export const COMPOSER_SHELL_PADDING_PX = 12;
/** `px-4 pb-4` around the dock's column. */
export const COMPOSER_DOCK_INSET_PX = 16;
/** The back-to-bottom button's diameter. */
export const COMPOSER_BACK_TO_BOTTOM_SIZE_PX = 32;
/** Its gap above the shell (`mb-2`). */
export const COMPOSER_BACK_TO_BOTTOM_GAP_PX = 8;

export interface ComposerDockProps {
  /** The draft/empty layout: the narrow column, no sticky (the reference
   * centers the composer in the empty state instead). */
  draft?: boolean;
  /** The floating layout: an absolute layer on the bottom edge, so the dock
   * does not scroll with the messages. */
  floating?: boolean;
  /** The pane has an inline status rail in its width budget (default `true`):
   * the column then carries the same 168px shift the messages do. A pane
   * without one passes `false` and the dock stays centred with them. */
  rail?: boolean;
  /** A viewport width other than the window's (a gallery or a test). */
  windowWidthPx?: number;
  /** Show the round "back to bottom" button above the shell. */
  backToBottomVisible?: boolean;
  onBackToBottom?: () => void;
  backToBottomLabel?: string;
  children: JSX.Element;
  style?: StyleProp<ViewStyle>;
  testID?: string;
}

/** The sticky bottom dock: the composer's column, aligned with the messages. */
export function ComposerDock(props: ComposerDockProps) {
  const geometry = () => ({
    draft: props.draft ?? false,
    windowWidthPx: props.windowWidthPx ?? windowSize().width,
    rail: props.rail ?? true,
  });
  return (
    <View
      testID={props.testID}
      // The dock's own layer takes no events (the reference's
      // `pointer-events-none` wrapper): only the column inside it does, which
      // is what lets a rail or a button behind the dock's side gutters stay
      // clickable.
      style={[
        {
          width: "100%",
          flexDirection: "row",
          justifyContent: "center",
          zIndex: zIndex.sticky,
          pointerEvents: "box-none",
        },
        props.floating
          ? { position: "absolute", left: 0, right: 0, bottom: 0 }
          : props.draft
            ? { marginTop: 12, flexShrink: 0 }
            : { position: "sticky", bottom: 0, flexShrink: 0 },
        props.style ?? null,
      ]}
    >
      <View
        testID="composer-dock-content"
        style={{
          width: "100%",
          flexShrink: 0,
          pointerEvents: "box-none",
          // The column's own inset — the same one the messages' column applies
          // to its width — so the dock's column and the message column resolve
          // to one width (the reference puts the same width class on both,
          // ConversationTimeline.tsx:2043-2061 and 2169-2174).
          paddingHorizontal: props.draft || props.floating ? 0 : conversationColumnInsetPx(geometry()),
          paddingBottom: props.draft || props.floating ? 0 : COMPOSER_DOCK_INSET_PX,
        }}
      >
        <View
          style={{
            position: "relative",
            width: "100%",
            maxWidth: conversationColumnMaxWidthPx(geometry()) ?? undefined,
            marginHorizontal: "auto",
            translate: [conversationColumnOffsetPx(geometry()), 0],
            // `px-4` inside the column (the reference's `px-4 pb-4` on the same
            // element as the width class): the shell sits 16px in from the
            // column's edges, never another column inset on top of it.
            paddingHorizontal: props.draft || props.floating ? 0 : COMPOSER_DOCK_INSET_PX,
          }}
        >
          <If when={props.backToBottomVisible}>
            <ComposerBackToBottomButton
              label={props.backToBottomLabel ?? "回到底部"}
              onPress={props.onBackToBottom}
            />
          </If>
          {props.children}
        </View>
      </View>
    </View>
  );
}

/** The round button that sits just above the shell (`bottom-full left-1/2`). */
export function ComposerBackToBottomButton(props: { label: string; onPress?: () => void }) {
  return (
    <View
      testID="composer-back-to-bottom"
      focusable
      role="button"
      accessibilityLabel={props.label}
      onPress={() => props.onPress?.()}
      hoverStyle={{ backgroundColor: color.hover }}
      activeStyle={{ backgroundColor: color.glassStateArmed }}
      style={{
        position: "absolute",
        bottom: "100%",
        left: "50%",
        translate: ["-50%", 0],
        marginBottom: COMPOSER_BACK_TO_BOTTOM_GAP_PX,
        width: COMPOSER_BACK_TO_BOTTOM_SIZE_PX,
        height: COMPOSER_BACK_TO_BOTTOM_SIZE_PX,
        borderRadius: COMPOSER_BACK_TO_BOTTOM_SIZE_PX / 2,
        alignItems: "center",
        justifyContent: "center",
        backgroundColor: color.panel,
        borderWidth: 1,
        borderColor: color.border,
        boxShadow: "0 1px 2px rgba(0, 0, 0, 0.2)", // kit-imports: allow-color (a shadow's own ink, not a theme colour)
        zIndex: zIndex.header,
      }}
    >
      <Icon name="arrow-down" size={16} tint={color.foreground} />
    </View>
  );
}

export interface ComposerSurfaceProps {
  /** The editing area, toolbar and the rest of the shell's content. */
  children: JSX.Element;
  /** Not editable: the shell dims and takes no input. */
  disabled?: boolean;
  /** The error state: the border carries the danger token as well as the
   * message the consumer renders inside (colour is never the only channel). */
  error?: boolean;
  style?: StyleProp<ViewStyle>;
  testID?: string;
}

/** The input shell: rounded-2xl, 1px border, p-3, focus ring. */
export function ComposerSurface(props: ComposerSurfaceProps) {
  return (
    <View
      testID={props.testID}
      style={[
        {
          flexDirection: "column",
          gap: COMPOSER_SHELL_PADDING_PX,
          padding: COMPOSER_SHELL_PADDING_PX,
          borderRadius: COMPOSER_SHELL_RADIUS_PX,
          borderWidth: 1,
          borderColor: props.error ? color.danger : color.border,
          backgroundColor: color.input,
          transitionProperty: "border-color, background-color",
          transitionDuration: motion.fast,
          transitionTimingFunction: "ease-out",
          opacity: props.disabled ? 0.5 : 1,
        },
        props.style ?? null,
      ]}
      // `focus-within:border-brand ring-1 ring-brand/30`: the shell lights up
      // while anything inside it has the focus, drawn as an outline so the
      // ring never shifts the layout.
      hoverStyle={{ borderColor: props.error ? color.danger : color.ring }}
      focusWithinStyle={{
        borderColor: props.error ? color.danger : color.brand,
        outlineWidth: 1,
        outlineStyle: "solid",
        outlineColor: alpha(color.brand, 0.3),
        outlineOffset: 0,
      }}
    >
      {props.children}
    </View>
  );
}
