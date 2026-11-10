// L · WorkspaceShell — the workspace's three columns over one split.
//
// Parameters from the zcode reference (`WorkspaceShellLayout.tsx`), item by
// item:
//
//   | item                        | value                                              |
//   |-----------------------------|----------------------------------------------------|
//   | sidebar                     | 264px default/min, 50% container max (see `Sidebar`)|
//   | main column                 | min 320px                                           |
//   | right pane (`SidePane`)     | 45% expanded, 0 collapsed, min 240px, max 65%       |
//   | header / toolbar            | h-12 (48px)                                         |
//   | single column breakpoint    | ≤767px (`windowSize()`, no container queries)       |
//   | auto-collapse               | conversation <480px closes the right pane,          |
//   |                             | <360px closes the sidebar, 300ms resize debounce    |
//
// Two measurements live side by side on purpose: NSH's group measures itself
// for the ratio ceilings (`ResizableGroupWidth`), and the shell measures the
// same box to reason about the conversation column's width — the stand-in for
// the reference's `getBoundingClientRect()` on the conversation panel, which
// NSH has no container-query equivalent of.
//
// Platform notes:
//   · terminal: there is no hover and no container query; the single-column
//     branch, the drawer and every threshold are decided by `windowSize()`, in
//     cells. Widths below a cell are drawn as one cell.
//   · web/desktop: rows and columns as the reference has them.
//   · The panels' corners are not rounded here (NSH panels are the app's to
//     style); `workspacePanelRadiusPx()` from `./sidebar` is the value the
//     chrome rounds with when it draws a panel face.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { effect, signal, untracked, View, windowSize } from "nsh";
import { color, zIndex } from "../tokens";
import {
  ResizableGroupWidth,
  ResizablePanel,
  ResizablePanelGroup,
} from "./resizable";
import {
  Sidebar,
  SidebarCollapseStrip,
  WORKSPACE_SIDEBAR_DEFAULT_WIDTH_PX,
  workspaceResizeHandleInsetPx,
  workspaceSidebarCollapsedWidthPx,
} from "./sidebar";

/** The header/toolbar row: h-12. */
export const WORKSPACE_SHELL_HEADER_HEIGHT_PX = 48;
/** The main column never gets narrower than this. */
export const WORKSPACE_SHELL_MAIN_MIN_WIDTH_PX = 320;
/** ≤ this, the shell is one column with a drawer (zcode's 767px breakpoint). */
export const WORKSPACE_SHELL_SINGLE_COLUMN_MAX_PX = 767;
/** The right pane's expanded size, as a share of the shell. */
export const SIDE_PANE_EXPANDED_RATIO = 0.45;
export const SIDE_PANE_MIN_WIDTH_PX = 240;
export const SIDE_PANE_MAX_RATIO = 0.65;
/** The conversation width that closes the right pane. */
export const CONVERSATION_AUTO_COLLAPSE_SIDE_PANE_WIDTH_PX = 480;
/** The conversation width that closes the sidebar. */
export const CONVERSATION_AUTO_COLLAPSE_SIDEBAR_WIDTH_PX = 360;
/** Resize idle time before the auto-collapse policy runs. */
export const CONVERSATION_AUTO_COLLAPSE_RESIZE_IDLE_MS = 300;

export interface WorkspaceShellLayoutEvent {
  sidebarCollapsed: boolean;
  sidePaneOpen: boolean;
  sidebarWidthPx: number;
  sidePaneWidthPx: number;
  singleColumn: boolean;
}

/**
 * What the auto-collapse policy does with a conversation column `conversationWidthPx`
 * wide: the right pane closes below 480px, the sidebar below 360px, and a
 * single-column layout has neither to close. Pure, so the thresholds stay
 * checkable without a window.
 */
export function autoCollapseDecision(event: {
  conversationWidthPx: number;
  sidePaneOpen: boolean;
  sidebarCollapsed: boolean;
  singleColumn: boolean;
}): { closeSidePane: boolean; collapseSidebar: boolean } {
  if (event.singleColumn) return { closeSidePane: false, collapseSidebar: false };
  const tooNarrowForSidebar =
    !event.sidebarCollapsed &&
    event.conversationWidthPx < CONVERSATION_AUTO_COLLAPSE_SIDEBAR_WIDTH_PX;
  if (!event.sidePaneOpen) return { closeSidePane: false, collapseSidebar: tooNarrowForSidebar };
  if (event.conversationWidthPx >= CONVERSATION_AUTO_COLLAPSE_SIDE_PANE_WIDTH_PX) {
    return { closeSidePane: false, collapseSidebar: tooNarrowForSidebar };
  }
  // The pane goes first; the sidebar is judged on the same (pre-collapse)
  // width, as the reference does.
  return { closeSidePane: true, collapseSidebar: tooNarrowForSidebar };
}

export interface WorkspaceShellProps {
  /** The h-12 header row (title, tabs, actions) — fixed height, no wrap. */
  header?: JSX.Element;
  /** The sidebar's content; the shell owns the panel and its width. */
  sidebar?: JSX.Element;
  /** The conversation column (usually a `ConversationColumn`). */
  main: JSX.Element;
  /** The right pane's content; without it the shell has no third column. */
  sidePane?: JSX.Element;
  /** A controlled collapse flag; without it the shell owns it and starts from
   * `defaultSidebarCollapsed`. */
  sidebarCollapsed?: boolean;
  onSidebarCollapsedChange?: (collapsed: boolean) => void;
  /** A controlled right-pane flag; without it the shell owns it and starts from
   * `defaultSidePaneOpen`. */
  sidePaneOpen?: boolean;
  onSidePaneOpenChange?: (open: boolean) => void;
  /** The uncontrolled start of both flags. */
  defaultSidebarCollapsed?: boolean;
  defaultSidePaneOpen?: boolean;
  /** Every layout change the shell commits (auto-collapse included). */
  onLayoutChange?: (event: WorkspaceShellLayoutEvent) => void;
  style?: StyleProp<ViewStyle>;
  testID?: string;
}

/** Sidebar + main (min 320px) + optional right pane, one draggable split. */
export function WorkspaceShell(props: WorkspaceShellProps) {
  // Both flags are controllable-or-not: given `sidebarCollapsed` /
  // `sidePaneOpen`, the shell reports changes and lets the owner hold them;
  // without them the shell holds them itself (starting from the `default*`).
  const ownSidebarCollapsed = signal(props.defaultSidebarCollapsed ?? false);
  const ownSidePaneOpen = signal(props.defaultSidePaneOpen ?? false);
  const sidebarCollapsed = () => props.sidebarCollapsed ?? ownSidebarCollapsed();
  const sidePaneOpen = () => props.sidePaneOpen ?? ownSidePaneOpen();
  const setSidebarCollapsed = (next: boolean) => {
    if (props.sidebarCollapsed === undefined) ownSidebarCollapsed.set(next);
    props.onSidebarCollapsedChange?.(next);
  };
  const setSidePaneOpen = (next: boolean) => {
    if (props.sidePaneOpen === undefined) ownSidePaneOpen.set(next);
    props.onSidePaneOpenChange?.(next);
  };
  const shellWidthPx = signal(0);
  const sidebarWidthPx = signal(WORKSPACE_SIDEBAR_DEFAULT_WIDTH_PX);
  const sidePaneWidthPx = signal(0);
  const drawerOpen = signal(false);

  const singleColumn = () => windowSize().width <= WORKSPACE_SHELL_SINGLE_COLUMN_MAX_PX;
  // The pane's width as the pane itself resolves it (45% of the shell until the
  // user drags it), so the policy reasons about the space the conversation
  // really gets — the pane only reports a width once a drag pinned one.
  const paneWidthOrZero = () => {
    if (!sidePaneOpen()) return 0;
    if (sidePaneWidthPx() > 0) return sidePaneWidthPx();
    const groupWidthPx = shellWidthPx() > 0 ? shellWidthPx() : windowSize().width;
    return Math.round(Math.max(SIDE_PANE_MIN_WIDTH_PX, groupWidthPx * SIDE_PANE_EXPANDED_RATIO));
  };
  // The conversation column's width, as the reference measures it: everything
  // the shell shows that is not the sidebar or the right pane. The handles are
  // absolutely placed overlays and take no layout width.
  const conversationWidthPx = () =>
    Math.max(
      0,
      (shellWidthPx() > 0 ? shellWidthPx() : windowSize().width) -
        (sidebarCollapsed() ? workspaceSidebarCollapsedWidthPx() : sidebarWidthPx()) -
        paneWidthOrZero(),
    );

  // The auto-collapse policy: at mount, as soon as the shell has a width, and
  // then on window resizes (300ms of idle, as the reference). Mount matters on
  // its own — a pane restored at 1280 already costs the conversation 576px, so
  // the "conversation under 480px closes the pane" rule has to hold from the
  // first layout, not only after the user's first resize.
  const runAutoCollapse = () => {
    const decision = untracked(() =>
      autoCollapseDecision({
        conversationWidthPx: conversationWidthPx(),
        sidePaneOpen: sidePaneOpen(),
        sidebarCollapsed: sidebarCollapsed(),
        singleColumn: singleColumn(),
      }),
    );
    if (decision.closeSidePane) setSidePaneOpen(false);
    if (decision.collapseSidebar) setSidebarCollapsed(true);
  };
  // The mount pass waits on the shell's own measurement: before it, every width
  // is the window's and the conversation's share is a guess.
  let mountPolicyRan = false;
  effect(() => {
    if (mountPolicyRan) return;
    if (!(shellWidthPx() > 0)) return;
    mountPolicyRan = true;
    runAutoCollapse();
  });
  // The resize pass tracks the window width alone: a second tracked read (the
  // shell's) would re-run the effect mid-gesture, and that run's cleanup would
  // clear the pending timer — the policy would never see a resize.
  // The last width is a plain closure variable for the same reason.
  let lastViewportWidthPx = -1;
  effect(() => {
    const viewportWidthPx = windowSize().width;
    const previousWidthPx = lastViewportWidthPx;
    lastViewportWidthPx = viewportWidthPx;
    if (previousWidthPx <= 0 || previousWidthPx === viewportWidthPx) return;
    const timer = setTimeout(runAutoCollapse, CONVERSATION_AUTO_COLLAPSE_RESIZE_IDLE_MS);
    return () => clearTimeout(timer);
  });

  // Report every committed layout (also the ones the policy just made).
  effect(() => {
    props.onLayoutChange?.({
      sidebarCollapsed: sidebarCollapsed(),
      sidePaneOpen: sidePaneOpen(),
      sidebarWidthPx: sidebarWidthPx(),
      sidePaneWidthPx: paneWidthOrZero(),
      singleColumn: singleColumn(),
    });
  });

  return (
    <View
      testID={props.testID}
      style={[
        {
          flex: 1,
          minWidth: 0,
          position: "relative",
          backgroundColor: color.background,
          overflow: "hidden",
        },
        props.style ?? null,
      ]}
      onLayout={(event) => shellWidthPx.set(event.bounds.size.width)}
    >
      <If
        when={singleColumn()}
        else={
          <ResizablePanelGroup
            axis="horizontal"
            testID="workspace-split"
            handleInsetPx={workspaceResizeHandleInsetPx()}
            handleAccessibilityLabel="调整面板尺寸"
          >
            <If when={props.sidebar}>
              <Sidebar
                testID="workspace-sidebar"
                collapsed={sidebarCollapsed()}
                onCollapsedChange={(next) => setSidebarCollapsed(next)}
                onWidthChange={(widthPx) => sidebarWidthPx.set(widthPx)}
              >
                {props.sidebar}
              </Sidebar>
            </If>
            <ResizablePanel testID="workspace-main" minSize={WORKSPACE_SHELL_MAIN_MIN_WIDTH_PX}>
              <ShellColumn header={props.header}>{props.main}</ShellColumn>
            </ResizablePanel>
            <If when={props.sidePane}>
              <SidePane
                testID="workspace-side-pane"
                open={sidePaneOpen()}
                onWidthChange={(widthPx) => sidePaneWidthPx.set(widthPx)}
              >
                {props.sidePane}
              </SidePane>
            </If>
          </ResizablePanelGroup>
        }
      >
        {/* ≤767px: one column; the sidebar becomes a drawer over the main
            column, opened from a 4px leading strip. The drawer mounts the
            sidebar's children only in this branch, so crossing the breakpoint
            remounts them (documented, and the only way to keep one instance
            alive in both layouts). */}
        <View style={{ flex: 1, minWidth: 0 }}>
          <ShellColumn header={props.header}>{props.main}</ShellColumn>
          <If when={props.sidebar}>
            <If when={drawerOpen()}>
              <View
                testID="workspace-sidebar-drawer-backdrop"
                onPress={() => drawerOpen.set(false)}
                style={{
                  position: "absolute",
                  top: 0,
                  bottom: 0,
                  left: 0,
                  right: 0,
                  backgroundColor: color.glassShade,
                  zIndex: zIndex.backdrop,
                }}
              />
              <View
                testID="workspace-sidebar-drawer"
                style={{
                  position: "absolute",
                  top: 0,
                  bottom: 0,
                  left: 0,
                  width: "85%",
                  maxWidth: WORKSPACE_SHELL_DRAWER_MAX_WIDTH_PX,
                  backgroundColor: color.sidebar,
                  borderRightWidth: 1,
                  borderColor: color.sidebarBorder,
                  zIndex: zIndex.sheet,
                }}
              >
                {props.sidebar}
              </View>
            </If>
            <SidebarCollapseStrip
              testID="workspace-sidebar-strip"
              onExpand={() => drawerOpen.set(true)}
              style={{
                position: "absolute",
                top: 0,
                bottom: 0,
                left: 0,
                width: workspaceSidebarCollapsedWidthPx(),
                zIndex: zIndex.header,
              }}
            />
          </If>
        </View>
      </If>
      {/* Collapsed, wide layout: the 4px (Windows 6px) strip sits on the shell's
          left edge, where the collapsed panel was — the same affordance the
          single-column branch draws. */}
      <If when={props.sidebar && sidebarCollapsed() && !singleColumn()}>
        <SidebarCollapseStrip
          testID="sidebar-collapse-strip"
          onExpand={() => setSidebarCollapsed(false)}
          style={{
            position: "absolute",
            top: 0,
            bottom: 0,
            left: 0,
            width: workspaceSidebarCollapsedWidthPx(),
            zIndex: zIndex.header,
          }}
        />
      </If>
    </View>
  );
}

/** The drawer (single column) never grows past the sidebar's default width. */
export const WORKSPACE_SHELL_DRAWER_MAX_WIDTH_PX = WORKSPACE_SIDEBAR_DEFAULT_WIDTH_PX;

/** A column with the fixed h-12 header slot on top and the content below. */
function ShellColumn(props: { header?: JSX.Element; children: JSX.Element }) {
  return (
    <View style={{ flex: 1, minWidth: 0, flexDirection: "column" }}>
      <If when={props.header}>
        <View
          testID="workspace-header"
          style={{
            height: WORKSPACE_SHELL_HEADER_HEIGHT_PX,
            flexShrink: 0,
            flexDirection: "row",
            alignItems: "center",
            paddingHorizontal: 12,
            gap: 8,
            backgroundColor: color.panel,
            borderBottomWidth: 1,
            borderColor: color.headerBorder,
          }}
        >
          {props.header}
        </View>
      </If>
      <View style={{ flex: 1, minHeight: 0 }}>{props.children}</View>
    </View>
  );
}

export interface SidePaneProps {
  /** Expanded (45% by default) or collapsed (0: no space, no handle). */
  open: boolean;
  /** A width in px the pane's owner fixed; without one the pane remembers the
   * user's drag, else takes 45% of the shell. */
  widthPx?: number;
  onWidthChange?: (widthPx: number) => void;
  children?: JSX.Element;
  style?: StyleProp<ViewStyle>;
  testID?: string;
}

/** The right pane: 45% expanded, 0 collapsed, min 240px, max 65%. */
export function SidePane(props: SidePaneProps) {
  const draggedWidthPx = signal(0);
  const expandedSizePx = () => {
    if (props.widthPx !== undefined) return props.widthPx;
    if (draggedWidthPx() > 0) return draggedWidthPx();
    // The group reports its width on its first layout; until then the window is
    // the shell's own stand-in (the same fallback `paneWidthOrZero` uses), so
    // the pane opens at its 45% instead of clamping to the 240px minimum — the
    // shell already reports that 45% in its layout event, so the drawn pane and
    // the reported one agree.
    const groupWidthPx = ResizableGroupWidth() > 0 ? ResizableGroupWidth() : windowSize().width;
    return Math.round(Math.max(SIDE_PANE_MIN_WIDTH_PX, groupWidthPx * SIDE_PANE_EXPANDED_RATIO));
  };
  return (
    <ResizablePanel
      testID={props.testID}
      visible={props.open}
      size={props.open ? expandedSizePx() : 0}
      minSize={SIDE_PANE_MIN_WIDTH_PX}
      maxRatio={SIDE_PANE_MAX_RATIO}
      onResize={(event) => {
        draggedWidthPx.set(event.size);
        props.onWidthChange?.(event.size);
      }}
      style={[
        {
          backgroundColor: color.panel,
          borderLeftWidth: 1,
          borderColor: color.border,
        },
        props.style ?? null,
      ]}
    >
      {props.children}
    </ResizablePanel>
  );
}
