// L · ConversationColumn — the conversation's content column, plus the status
// rail that shares its width budget.
//
// Parameters from the zcode reference (`conversationLayout.ts`,
// `ConversationTimeline.tsx`), item by item:
//
//   | item                       | value                                             |
//   |----------------------------|---------------------------------------------------|
//   | draft / empty state        | `max-w-2xl` (672px)                                |
//   | ≥864px                     | `max-w-4xl` (896px), inset 6rem (96px)             |
//   | ≥1280px                    | `max-w-6xl` (1152px), inset 24rem (384px), shifted |
//   |                            | left 168px (`-translate-x-42`) for the rail        |
//   | status rail                | 320px (`w-80`) at the pane's top right from 1280px |
//
// The shift exists to make room for the inline rail: the reference applies it
// only when the status panel's layout is not `"none"`
// (conversationLayout.ts:43-50), so the block takes a `rail` switch (default
// `true`) — a pane whose status surface is not an inline rail passes `false`
// and the column stays centred.
//
// Breakpoints are read from `windowSize()`, not from container queries: the
// reference keys off the conversation *container* (`@min-[864px]/conversation`),
// and NSH has no container queries. The shells here are single panes, so the
// window is the container in practice; a consumer that splits a pane further
// should keep the column's `windowWidthPx`-style inputs in mind.
//
// Platform notes:
//   · terminal: caps and insets are laid out on the cell grid; the 168px shift
//     is a whole number of cells, so the rail never overlaps the column there.
//   · web/desktop: as the reference (including the rail's max height, 512px =
//     32rem, the `dvh` half of `min(64dvh, 32rem)` becoming a plain cap).
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Scroll, Text, View, windowSize } from "nsh";
import { color, radius as kitRadius, typeScale, zIndex } from "../tokens";

/** `max-w-2xl`: the draft/empty column. */
export const CONVERSATION_DRAFT_MAX_WIDTH_PX = 672;
/** `max-w-4xl`: from the medium breakpoint on. */
export const CONVERSATION_MEDIUM_MAX_WIDTH_PX = 896;
/** `max-w-6xl`: with the status rail. */
export const CONVERSATION_WIDE_MAX_WIDTH_PX = 1152;
/** The medium breakpoint (`@min-[864px]/conversation`). */
export const CONVERSATION_MEDIUM_MIN_VIEWPORT_PX = 864;
/** The wide breakpoint, where the rail becomes inline and the column shifts. */
export const CONVERSATION_WIDE_MIN_VIEWPORT_PX = 1280;
/** The leftward shift that makes room for the rail (`-translate-x-42`). */
export const CONVERSATION_WIDE_OFFSET_PX = 168;
/** `w-80`: the inline rail. */
export const CONVERSATION_STATUS_RAIL_WIDTH_PX = 320;
/** `max-h-[min(64dvh,32rem)]`: the rail's own ceiling (32rem). */
export const CONVERSATION_STATUS_RAIL_MAX_HEIGHT_PX = 512;
/** The rail's distance from the pane's right/top edges (`right-3 top-3`). */
export const CONVERSATION_STATUS_RAIL_INSET_PX = 12;

/** The 6rem/24rem insets: each side's padding, which is what keeps a narrow
 * enough pane from letting the column touch its edges (the reference's
 * `w-[calc(100%_-_6rem)]` / `w-[calc(100%_-_24rem)]`). */
export function conversationColumnInsetPx(event: {
  draft: boolean;
  windowWidthPx: number;
}): number {
  if (event.draft) return 0;
  if (event.windowWidthPx >= CONVERSATION_WIDE_MIN_VIEWPORT_PX) return 192;
  if (event.windowWidthPx >= CONVERSATION_MEDIUM_MIN_VIEWPORT_PX) return 48;
  return 0;
}

/** The column's ceiling, or `null` for "full width" (the reference's `w-full`
 * under 864px). */
export function conversationColumnMaxWidthPx(event: {
  draft: boolean;
  windowWidthPx: number;
}): number | null {
  if (event.draft) return CONVERSATION_DRAFT_MAX_WIDTH_PX;
  if (event.windowWidthPx >= CONVERSATION_WIDE_MIN_VIEWPORT_PX)
    return CONVERSATION_WIDE_MAX_WIDTH_PX;
  if (event.windowWidthPx >= CONVERSATION_MEDIUM_MIN_VIEWPORT_PX)
    return CONVERSATION_MEDIUM_MAX_WIDTH_PX;
  return null;
}

/** The column's leftward shift: 168px only in the wide, non-draft layout of a
 * pane that actually carries an inline status rail. The reference keys the
 * offset off the status panel's layout and returns none for `"none"`
 * (`getConversationStatusPanelOffsetClassName`, conversationLayout.ts:43-50);
 * without a rail the shift would move the column 168px off the pane's centre
 * for a rail that is not there. */
export function conversationColumnOffsetPx(event: {
  draft: boolean;
  windowWidthPx: number;
  /** An inline status rail shares this pane's width budget (default `true`,
   * the reference's layout). A pane without one passes `false`. */
  rail?: boolean;
}): number {
  return !event.draft && (event.rail ?? true) && event.windowWidthPx >= CONVERSATION_WIDE_MIN_VIEWPORT_PX
    ? -CONVERSATION_WIDE_OFFSET_PX
    : 0;
}

export interface ConversationColumnProps {
  /** The draft/empty state: `max-w-2xl`, no shift (the reference's centered
   * draft layout). */
  draft?: boolean;
  /** The pane has an inline status rail in its width budget (default `true`):
   * the 1280px layout then shifts the column left by 168px to make room for it.
   * Pass `false` for a pane whose status surface is not an inline rail (a
   * side pane, a chip), and the column stays centred. */
  rail?: boolean;
  /** A viewport width other than the window's (a gallery or a test). */
  windowWidthPx?: number;
  children: JSX.Element;
  style?: StyleProp<ViewStyle>;
  testID?: string;
}

/** The centered column the conversation's messages, header and composer
 * align to. */
export function ConversationColumn(props: ConversationColumnProps) {
  const viewportWidthPx = () => props.windowWidthPx ?? windowSize().width;
  const geometry = () => ({
    draft: props.draft ?? false,
    windowWidthPx: viewportWidthPx(),
    rail: props.rail ?? true,
  });
  return (
    <View
      testID={props.testID}
      style={[
        {
          width: "100%",
          flexDirection: "column",
          paddingHorizontal: conversationColumnInsetPx(geometry()),
        },
        props.style ?? null,
      ]}
    >
      <View
        style={{
          width: "100%",
          maxWidth: conversationColumnMaxWidthPx(geometry()) ?? undefined,
          marginHorizontal: "auto",
          translate: [conversationColumnOffsetPx(geometry()), 0],
        }}
      >
        {props.children}
      </View>
    </View>
  );
}

export interface ConversationStatusRailProps {
  /** `auto` (the default): the 320px rail from 1280px up, the compact chip
   * below it. `inline` / `mini` force one of the two. */
  variant?: "auto" | "inline" | "mini";
  /** The chip's headline (mini only; the rail shows its children). */
  label?: string;
  children?: JSX.Element;
  style?: StyleProp<ViewStyle>;
  testID?: string;
}

/** The conversation's status surface: a rail pinned to the pane's top right
 * from 1280px up, a compact chip under it. Render it inside a positioned
 * container (the pane), next to the column. */
export function ConversationStatusRail(props: ConversationStatusRailProps) {
  const inline = () =>
    props.variant === "inline" ||
    (props.variant !== "mini" && windowSize().width >= CONVERSATION_WIDE_MIN_VIEWPORT_PX);
  return (
    <If
      when={inline()}
      else={
        <View
          testID={props.testID}
          style={[
            {
              flexDirection: "row",
              alignItems: "center",
              gap: 6,
              height: 34,
              paddingHorizontal: 12,
              borderRadius: kitRadius.pill,
              borderWidth: 1,
              borderColor: color.border,
              backgroundColor: color.panel,
              maxWidth: CONVERSATION_STATUS_RAIL_WIDTH_PX,
            },
            props.style ?? null,
          ]}
        >
          <If when={props.label}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              {props.label ?? ""}
            </Text>
          </If>
          {props.children}
        </View>
      }
    >
      <View
        testID={props.testID}
        style={[
          {
            position: "absolute",
            top: CONVERSATION_STATUS_RAIL_INSET_PX,
            right: CONVERSATION_STATUS_RAIL_INSET_PX,
            width: CONVERSATION_STATUS_RAIL_WIDTH_PX,
            maxHeight: CONVERSATION_STATUS_RAIL_MAX_HEIGHT_PX,
            borderRadius: kitRadius.lg,
            borderWidth: 1,
            borderColor: color.popoverEdge,
            backgroundColor: color.popover,
            padding: 12,
            zIndex: zIndex.content,
          },
          props.style ?? null,
        ]}
      >
        <Scroll style={{ flexGrow: 1 }}>{props.children}</Scroll>
      </View>
    </If>
  );
}
