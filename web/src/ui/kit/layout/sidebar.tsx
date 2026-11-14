// L · Sidebar — the workspace's left column, over the kit's Resizable panel.
//
// Parameters from the zcode reference (`WorkspaceShellLayout.tsx`), item by
// item:
//
//   | item                     | value                                                    |
//   |--------------------------|----------------------------------------------------------|
//   | default / minimum width   | 264px / 264px                                            |
//   | maximum width             | 50% of the container (runtime, ratio clamp)              |
//   | keyboard step             | 16px (arrows on the focused handle; Home/End = min/max)  |
//   | collapsed width           | 4px (6px on Windows), 200ms width/opacity transition     |
//   | persistence               | `zcode:workspace-shell:sidebar-width-px`, legacy key migrated |
//
// The width lives here (it is the sidebar's own), the collapse flag lives with
// the shell (window resizes and the single-column breakpoint decide it), hence
// `collapsed` + `onCollapsedChange`.
//
// Platform notes:
//   · terminal: percentage ceilings and 0.5px lines are laid out on the cell
//     grid (no pixel layout), hover does not exist — the collapsed strip's
//     hover-expand is then reached with a press (Enter/Space on it) instead.
//   · Windows: the collapsed strip is 6px and the panel radius 6px, as in the
//     reference (`resolveWorkspaceShellPanelRadiusPx`).
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { effect, platform, signal, Storage, Text, untracked, View, windowSize } from "nsh";
import { color, radius as kitRadius, typeScale } from "../tokens";
import { focusRing } from "../components/styles";
import {
  clampMaxSize,
  ResizableGroupWidth,
  ResizablePanel,
  ResizeHandleKeyBindings,
  RESIZE_HANDLE_KEYBOARD_STEP_PX,
} from "./resizable";

export const WORKSPACE_SIDEBAR_DEFAULT_WIDTH_PX = 264;
export const WORKSPACE_SIDEBAR_MIN_WIDTH_PX = 264;
export const WORKSPACE_SIDEBAR_MAX_RATIO = 0.5;
export const WORKSPACE_SIDEBAR_COLLAPSED_WIDTH_PX = 4;
export const WORKSPACE_SIDEBAR_COLLAPSED_WIDTH_WINDOWS_PX = 6;
/** zcode: `duration-200` on the collapsed panel's width/opacity. */
export const WORKSPACE_SIDEBAR_COLLAPSE_TRANSITION_MS = 200;
/** The width the user's drag is persisted under (zcode's key, verbatim). */
export const WORKSPACE_SIDEBAR_WIDTH_STORAGE_KEY = "zcode:workspace-shell:sidebar-width-px";
/** What earlier builds persisted (a percentage ratio under a layout key). */
export const LEGACY_WORKSPACE_SHELL_LAYOUT_STORAGE_KEY =
  "react-resizable-panels:workspace-shell-layout:sidebar:content";
/** zcode: the sidebar is the group's first panel, so its boundary is handle 0. */
export const SIDEBAR_HANDLE_INDEX = 0;

/** The collapsed strip's width: 4px, 6px on Windows (zcode's branch). */
export function workspaceSidebarCollapsedWidthPx(): number {
  return platform.os === "windows"
    ? WORKSPACE_SIDEBAR_COLLAPSED_WIDTH_WINDOWS_PX
    : WORKSPACE_SIDEBAR_COLLAPSED_WIDTH_PX;
}

/** Clamp a width into [264, container × 50%] (zcode's `clampWorkspaceSidebarWidth`). */
export function clampWorkspaceSidebarWidth(widthPx: number, containerWidthPx?: number): number {
  const maxWidthPx =
    containerWidthPx !== undefined && containerWidthPx > 0
      ? Math.max(WORKSPACE_SIDEBAR_MIN_WIDTH_PX, containerWidthPx * WORKSPACE_SIDEBAR_MAX_RATIO)
      : Number.POSITIVE_INFINITY;
  return Math.round(Math.max(WORKSPACE_SIDEBAR_MIN_WIDTH_PX, Math.min(widthPx, maxWidthPx)));
}

/** The ratio an old `react-resizable-panels` layout stored (`{ sidebar: 30 }`). */
export function readLegacySidebarWidthRatio(raw: string | null): number | null {
  if (raw === null) return null;
  try {
    const parsed = JSON.parse(raw) as { sidebar?: unknown };
    const ratio = Number(parsed.sidebar);
    return Number.isFinite(ratio) && ratio > 0 ? ratio / 100 : null;
  } catch {
    return null;
  }
}

export interface SidebarProps {
  /** Collapsed: the panel shrinks to its 4px (Windows 6px) strip. */
  collapsed: boolean;
  onCollapsedChange?: (collapsed: boolean) => void;
  /** The width the sidebar settled on (restored, dragged, stepped). */
  onWidthChange?: (widthPx: number) => void;
  children?: JSX.Element;
  style?: StyleProp<ViewStyle>;
  testID?: string;
}

/** The left column: a fixed panel of the shell's split, 264px by default. */
export function Sidebar(props: SidebarProps) {
  const widthPx = signal(WORKSPACE_SIDEBAR_DEFAULT_WIDTH_PX);
  // Width transitions run only for a collapse/expand, never during a drag: a
  // transition while the core moves the width frame by frame would make the
  // panel trail the pointer (the reference app keeps the same rule).
  const animatingWidth = signal(false);

  // Restore the persisted width once, migrating the old layout key when this
  // is the first run with the new one (zcode's migration, same semantics: the
  // stored ratio becomes pixels against the shell's width).
  effect(() => {
    void (async () => {
      const stored = await Storage.getItem(WORKSPACE_SIDEBAR_WIDTH_STORAGE_KEY);
      if (stored !== null) {
        const parsed = Number(stored);
        if (Number.isFinite(parsed) && parsed > 0) {
          widthPx.set(clampWorkspaceSidebarWidth(parsed));
        }
        return;
      }
      const legacy = await Storage.getItem(LEGACY_WORKSPACE_SHELL_LAYOUT_STORAGE_KEY);
      const ratio = readLegacySidebarWidthRatio(legacy);
      if (ratio === null) return;
      // The group's measured width is the truth; before its first layout (or
      // in a host that never lays out) the window is the stand-in.
      const containerWidthPx = untracked(() => {
        const measured = ResizableGroupWidth();
        return measured > 0 ? measured : windowSize().width;
      });
      const migratedWidthPx = clampWorkspaceSidebarWidth(ratio * containerWidthPx, containerWidthPx);
      widthPx.set(migratedWidthPx);
      void Storage.setItem(WORKSPACE_SIDEBAR_WIDTH_STORAGE_KEY, String(migratedWidthPx));
    })();
  });

  const commitWidth = (nextWidthPx: number, options: { persist: boolean }) => {
    const containerWidthPx = untracked(() => ResizableGroupWidth());
    const resolvedWidthPx = clampWorkspaceSidebarWidth(nextWidthPx, containerWidthPx);
    widthPx.set(resolvedWidthPx);
    props.onWidthChange?.(resolvedWidthPx);
    if (options.persist) {
      void Storage.setItem(WORKSPACE_SIDEBAR_WIDTH_STORAGE_KEY, String(resolvedWidthPx));
    }
  };

  // The keyboard step the spec fixed: arrows move 16px, Home/End go straight to
  // the minimum / the 50% ceiling.
  const stepWidth = (command: "decrease" | "increase" | "min" | "max") => {
    if (props.collapsed) return;
    const containerWidthPx = untracked(() => ResizableGroupWidth());
    const maxWidthPx = clampMaxSize(null, WORKSPACE_SIDEBAR_MAX_RATIO, containerWidthPx);
    if (command === "decrease") {
      commitWidth(widthPx() - RESIZE_HANDLE_KEYBOARD_STEP_PX, { persist: true });
    } else if (command === "increase") {
      commitWidth(widthPx() + RESIZE_HANDLE_KEYBOARD_STEP_PX, { persist: true });
    } else if (command === "min") {
      commitWidth(WORKSPACE_SIDEBAR_MIN_WIDTH_PX, { persist: true });
    } else if (maxWidthPx !== null) {
      commitWidth(maxWidthPx, { persist: true });
    }
  };

  const setCollapsed = (next: boolean) => {
    if (props.collapsed === next) return;
    animatingWidth.set(true);
    setTimeout(() => animatingWidth.set(false), WORKSPACE_SIDEBAR_COLLAPSE_TRANSITION_MS);
    props.onCollapsedChange?.(next);
  };

  // Collapsed, the panel is hidden (a hidden panel takes no space and no
  // handle) and the 4px strip is the shell's — see the note on
  // `SidebarCollapseStrip` for why the strip is not a 4px panel here: nsh 0.4.0
  // lays a panel's children out nowhere once the app drives its size down to a
  // hairline in a group of three or more panels (verified on the TUI host: the
  // panel keeps its space, its children get no layout at all, so the strip is
  // invisible and unreachable).
  return (
    <ResizablePanel
      testID={props.testID}
      // Fixed: the group's own resizes leave the sidebar at its width; only its
      // handle's drag (and the app's `size`) move it.
      fixed
      visible={!props.collapsed}
      size={widthPx()}
      minSize={WORKSPACE_SIDEBAR_MIN_WIDTH_PX}
      maxRatio={WORKSPACE_SIDEBAR_MAX_RATIO}
      onResize={(event) => {
        // A drag of the collapsed strip is meaningless: only the expand
        // affordance brings it back.
        if (props.collapsed) return;
        commitWidth(event.size, { persist: true });
      }}
      style={[
        {
          backgroundColor: color.sidebar,
          borderRightWidth: 1,
          borderColor: color.sidebarBorder,
          // 200ms width/opacity, only while a collapse/expand is running (a
          // transition during a drag would make the panel trail the pointer).
          transitionProperty: animatingWidth() ? "width, opacity" : undefined,
          transitionDuration: animatingWidth() ? WORKSPACE_SIDEBAR_COLLAPSE_TRANSITION_MS : undefined,
          transitionTimingFunction: animatingWidth() ? "ease-out" : undefined,
        },
        props.style ?? null,
      ]}
    >
      {props.children}
      <ResizeHandleKeyBindings handleIndex={SIDEBAR_HANDLE_INDEX} onCommand={stepWidth} />
    </ResizablePanel>
  );
}

/**
 * The collapsed sidebar's only affordance: a 4px (6px on Windows) strip that
 * expands on hover (a pointer) or press (keyboard, screen reader, terminal).
 * The shell lays it on its own left edge — where the collapsed panel was — so
 * it never takes a press meant for the main column: it is exactly the strip's
 * width.
 *
 * Why the shell draws it instead of the sidebar shrinking to 4px: nsh 0.4.0
 * lays a panel's children out nowhere once the app drives its `size` down to a
 * hairline inside a group of three or more panels (verified on the TUI host —
 * the panel keeps its space, its children get no layout, so a 4px strip would
 * be invisible and unreachable there). Hiding the panel and overlaying the
 * strip keeps the spec's 4–6px entry in every group size.
 */
export function SidebarCollapseStrip(props: {
  onExpand: () => void;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <View
      testID={props.testID ?? "sidebar-collapse-strip"}
      focusable
      role="button"
      accessibilityLabel="展开侧栏"
      onPress={() => props.onExpand()}
      onMouseEnter={() => props.onExpand()}
      // Hovered with a pointer it lifts to the hover wash, so the 4px target is
      // visible; pressed it arms (zcode's handle states).
      hoverStyle={{ backgroundColor: color.hover }}
      activeStyle={{ backgroundColor: color.glassStateArmed }}
      // A keyboard user must see where the focus is: this 4px strip is the only
      // way back to an expanded sidebar.
      focusStyle={focusRing()}
      style={[
        { alignItems: "center", justifyContent: "center", backgroundColor: color.sidebar },
        props.style ?? null,
      ]}
    >
      <If
        when={platform.idiom === "terminal"}
        else={
          /* A quiet grip mark: the strip is 4px wide, so it reads as a line. */
          <View
            style={{
              width: 2,
              height: 28,
              borderRadius: 1,
              backgroundColor: color.mutedForeground,
              opacity: 0.6,
            }}
          />
        }
      >
        {/* In a terminal a view with no characters is not addressable at all
            (no cells to hit or to name by testID; verified on the TUI host), so
            the strip carries a real glyph there instead of the drawn grip. */}
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>›</Text>
      </If>
    </View>
  );
}

/** The panel radius the workspace chrome rounds its panels with (zcode:
 * Windows 6px, elsewhere 12px — `resolveWorkspaceShellPanelRadiusPx`). */
export const workspacePanelRadiusPx = () =>
  platform.os === "windows" ? kitRadius.sm : kitRadius.inset;

/** The resize indicator's inset from both ends of a boundary (radius + 4,
 * radius + 6 on Windows — `resolveWorkspaceShellResizeHandleInsetPx`). */
export const workspaceResizeHandleInsetPx = () =>
  workspacePanelRadiusPx() + (platform.os === "windows" ? 6 : 4);
