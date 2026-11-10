// L · Resizable — the kit's split primitives, thin wrappers over NSH's own
// `ResizablePanelGroup` / `ResizablePanel` (`nsh/base`), carrying the shell
// parameters the product spec fixed from the zcode reference:
//
//   | item                          | value                                  |
//   |-------------------------------|----------------------------------------|
//   | drag handle band (drawn)       | 4px (zcode `w-1`)                      |
//   | indicator line, hover/active…  | 0.5px (zcode `after:w-0.5`), fading    |
//   | cursor                         | `ew-resize`                            |
//   | keyboard step                  | 16px (arrows; Home/End to min/max)     |
//   | clamp                          | runtime, `maxRatio` of the group width |
//
// What NSH owns and this file does not: the drag itself runs in the core (frame
// by frame, no JS round trip), sizes are reported once a drag ends (`onResize`),
// and the native band is 9 points thick (more on touch screens) with the
// `adjustable` role, so screen readers can step it. The 4px band here is the
// *drawn* band inside NSH's native hit area — never a narrower target.
//
// Keyboard: NSH reports keys only to inputs and to app-level bindings, and a
// handle cannot tell that the arrows are meant for it, so the kit does what the
// reference app does by hand: the band is focusable and tracks its own focus
// (the group publishes the focused handle's index through
// `ResizableFocusedHandle`), and the panel's owner binds arrows/Home/End with
// `ResizeHandleKeyBindings` while its own handle is focused. Only the owner
// knows the panel's min/max and what to persist.
//
// Platform notes (the blocks below reference these):
//   · terminal (`platform.idiom === "terminal"`): the host draws the boundary
//     itself (a cell-wide divider) and does not lay out `renderHandle`'s content
//     or `handleStyle` at all (verified on the TUI host), and there is no pointer
//     hover, so `state === "hovered"` never arrives; the handle is reached by the
//     native handle's own name (`handleAccessibilityLabel`) and role
//     (`adjustable`), not by the drawn band.
//   · web/desktop: full fidelity — pointer hover, focus, the resize cursor.
import type { JSX, Signal, StyleProp, ViewStyle } from "nsh";
import { context, keymap, signal, View } from "nsh";
import type { ResizeHandleContext } from "nsh/base";
import {
  ResizablePanel as NshResizablePanel,
  ResizablePanelGroup as NshResizablePanelGroup,
} from "nsh/base";
import { color, motion } from "../tokens";

/** The drawn band, in points. zcode's handle is `w-1` (4px). */
export const RESIZE_HANDLE_BAND_PX = 4;
/** The indicator hairline shown while hovered/pressed/dragged/focused. */
export const RESIZE_HANDLE_INDICATOR_PX = 0.5;
/** One arrow-key step, in points (zcode's sidebar step). */
export const RESIZE_HANDLE_KEYBOARD_STEP_PX = 16;

/** A handle's keyboard command, in the axis' own terms. */
export type ResizeHandleCommand = "decrease" | "increase" | "min" | "max";

/** The group's own width, measured from its layout — 0 until the first layout
 * pass. A panel reads it to clamp a `maxRatio` at runtime. */
export const ResizableGroupWidth = context<number>(0);

/** The index of the handle the user is on (a boundary, after the `index`th
 * visible panel), or `null`. NSH's handle owns focus and keys but does not
 * report either, so the kit's band publishes the focus half here. */
const orphanFocusedHandle = signal<number | null>(null);
export const ResizableFocusedHandle = context<Signal<number | null>>(orphanFocusedHandle);

export interface KitResizablePanelGroupProps {
  axis?: "horizontal" | "vertical";
  /** No drags: the handles stay where they are. */
  disabled?: boolean;
  /** How far the indicator is inset from both ends of the boundary, in points
   * (zcode: the panel radius + 4; +6 on Windows). */
  handleInsetPx?: number;
  /** What screen readers say for each handle: what it resizes. */
  handleAccessibilityLabel?: string;
  /** A drag of a handle ended: every visible panel's size, in order. */
  onResize?: (event: { sizes: number[] }) => void;
  style?: StyleProp<ViewStyle>;
  testID?: string;
  children?: JSX.Element;
}

/** A row (or column) of panels with draggable boundaries between them. */
export function ResizablePanelGroup(props: KitResizablePanelGroupProps) {
  // Read in JSX only, so the body stays a once-only script.
  const widthPx = signal(0);
  const focusedHandleIndex = signal<number | null>(null);

  return (
    <NshResizablePanelGroup
      axis={props.axis ?? "horizontal"}
      disabled={props.disabled}
      testID={props.testID}
      // A split fills its parent (NSH's default) and never lets a panel's
      // minimum push the group wider.
      style={[{ flex: 1, minWidth: 0, minHeight: 0 }, props.style ?? null]}
      onLayout={(event) => widthPx.set(event.bounds.size.width)}
      onResize={props.onResize}
      handleAccessibilityLabel={props.handleAccessibilityLabel}
      // The indicator is the part the app draws, 1 point across by default:
      // widen it to the spec's 4px band so the drawn band (and the terminal's
      // divider glyph) have room inside the native hit area.
      handleStyle={
        props.axis === "vertical"
          ? { height: RESIZE_HANDLE_BAND_PX, width: "100%", backgroundColor: "transparent" }
          : { width: RESIZE_HANDLE_BAND_PX, height: "100%", backgroundColor: "transparent" }
      }
      renderHandle={(handle: ResizeHandleContext) => (
        <ResizeHandleBand
          handle={handle}
          focusedHandleIndex={focusedHandleIndex}
          insetPx={props.handleInsetPx ?? 0}
        />
      )}
    >
      <ResizableGroupWidth.Provider value={widthPx()}>
        <ResizableFocusedHandle.Provider value={focusedHandleIndex}>
          {props.children}
        </ResizableFocusedHandle.Provider>
      </ResizableGroupWidth.Provider>
    </NshResizablePanelGroup>
  );
}

/** The drawn band + indicator inside NSH's own handle band. */
function ResizeHandleBand(props: {
  handle: ResizeHandleContext;
  focusedHandleIndex: Signal<number | null>;
  insetPx: number;
}) {
  return (
    <View
      // Focusing the band is what puts the arrow keys on it: the panel's owner
      // binds them only while its own handle is the focused one.
      focusable
      role="adjustable"
      accessibilityLabel={`调整面板尺寸（分隔条 ${props.handle.index + 1}）`}
      onFocus={() => props.focusedHandleIndex.set(props.handle.index)}
      onBlur={() => props.focusedHandleIndex.set(null)}
      style={{
        width: props.handle.axis === "horizontal" ? RESIZE_HANDLE_BAND_PX : "100%",
        height: props.handle.axis === "horizontal" ? "100%" : RESIZE_HANDLE_BAND_PX,
        alignItems: "center",
        justifyContent: "center",
        cursor: props.handle.axis === "horizontal" ? "ew-resize" : "ns-resize",
      }}
      hoverStyle={{ backgroundColor: color.hover }}
    >
      <View
        style={{
          width: props.handle.axis === "horizontal" ? RESIZE_HANDLE_INDICATOR_PX : "100%",
          height: props.handle.axis === "horizontal" ? "100%" : RESIZE_HANDLE_INDICATOR_PX,
          marginVertical: props.handle.axis === "horizontal" ? props.insetPx : 0,
          marginHorizontal: props.handle.axis === "horizontal" ? 0 : props.insetPx,
          borderRadius: RESIZE_HANDLE_INDICATOR_PX,
          backgroundColor: color.mutedForeground,
          // Idle handles draw nothing (zcode's `after:opacity-0`); hover, press,
          // drag and keyboard focus all show the hairline.
          opacity: props.handle.state === "idle" ? 0 : 1,
          transitionProperty: "opacity",
          transitionDuration: motion.fast,
          transitionTimingFunction: "ease-out",
        }}
      />
    </View>
  );
}

/**
 * Arrow / Home / End bindings for one handle, alive only while that handle has
 * the focus: a binding made while a component renders is removed with the
 * component. Render it from the panel's owner, next to the panel:
 *
 * ```tsx
 * <ResizeHandleKeyBindings handleIndex={0} onCommand={(c) => stepSidebar(c)} />
 * ```
 */
export function ResizeHandleKeyBindings(props: {
  /** The handle's index: the boundary after the `index`th visible panel. */
  handleIndex: number;
  onCommand: (command: ResizeHandleCommand) => void;
}) {
  return (
    <If when={ResizableFocusedHandle()() === props.handleIndex}>
      <ResizeHandleKeyScope onCommand={props.onCommand} />
    </If>
  );
}

/** The bindings themselves, made only while its handle is focused. */
function ResizeHandleKeyScope(props: { onCommand: (command: ResizeHandleCommand) => void }) {
  keymap.bind(
    {
      left: () => props.onCommand("decrease"),
      right: () => props.onCommand("increase"),
      home: () => props.onCommand("min"),
      end: () => props.onCommand("max"),
    },
    { repeat: true },
  );
  return null;
}

export interface KitResizablePanelProps {
  /** The size it wants along the axis, in points (within its range). */
  size?: number | null;
  minSize?: number;
  /** A size ceiling in points (NSH's native prop). */
  maxSize?: number | null;
  /** A ceiling as a share of the group's measured width: zcode's sidebar max is
   * 50%, the side pane's 65%. Whichever of `maxSize` / `maxRatio` is smaller
   * wins; the ratio is ignored until the group has been laid out once. */
  maxRatio?: number;
  /** Keeps its size when the group grows or shrinks (a sidebar). */
  fixed?: boolean;
  /** A hidden panel takes no space and gives the others its share. */
  visible?: boolean;
  onResize?: (event: { size: number }) => void;
  style?: StyleProp<ViewStyle>;
  testID?: string;
  children?: JSX.Element;
}

/** One panel of a `ResizablePanelGroup`. */
export function ResizablePanel(props: KitResizablePanelProps) {
  return (
    <NshResizablePanel
      size={props.size ?? null}
      minSize={props.minSize}
      maxSize={clampMaxSize(props.maxSize, props.maxRatio, ResizableGroupWidth())}
      fixed={props.fixed}
      visible={props.visible}
      onResize={props.onResize}
      testID={props.testID}
      style={[{ overflow: "hidden" }, props.style ?? null]}
    >
      {props.children}
    </NshResizablePanel>
  );
}

/** The smaller of the point ceiling and the ratio of the measured width. */
export function clampMaxSize(
  maxSize: number | null | undefined,
  maxRatio: number | undefined,
  groupWidthPx: number,
): number | null {
  const ratioMax =
    maxRatio !== undefined && groupWidthPx > 0 ? Math.round(groupWidthPx * maxRatio) : null;
  if (ratioMax === null) return maxSize ?? null;
  if (maxSize === null || maxSize === undefined) return ratioMax;
  return Math.min(maxSize, ratioMax);
}
