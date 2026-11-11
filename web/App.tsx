// The app root: the route picks the shell, the kit draws it.
//
//   workspace  the three columns — the session list (sidebar), the conversation
//              (main) and the status pane (right) — `WorkspaceShell`
//   settings   the standalone settings page
//   plugins    the plugin page (its own entry; a placeholder stands in here)
//   kit        the kit gallery — a developer route (the store maps `/kit` while
//              developing, and nothing in the product links to it)
//
// The h-12 header row, the sidebar panel, the right pane, the collapsed strip,
// the ≤767 single column and every threshold between them are the kit's blocks
// (change keel-ui-kit); this file fills the slots and holds only the state the
// shell takes from its owner: which screen the single column shows, whether the
// right pane is open, and the notices. Overlays come from the kit as well —
// `ToastViewport` for the store's notices, `AlertDialog` for the delete
// confirmation, the session model picker's own `Dialog`, and the global
// command palette (`CommandPalette`: Mod+K over every route).
//
// The workspace's right pane is its own screen
// (`src/screens/session-status-pane.tsx`) — the open session's running facts.
// The settings page's 外观 section can still switch the scheme at runtime; the
// launch appearance is locked to dark (`nsh.json`) because the light palette
// has not been through acceptance.
//
// NSH components run once: every reactive read lives in the vnode getters (JSX
// or an `<If>`), never in a body-level `if` or a local.
import { effect, mount, platform, signal, Storage, Text, View, windowSize } from "nsh";

import type { SessionSummary } from "./src/data/contract";
import { store, type Notice } from "./src/data/store";
import { KitGallery } from "./kit/KitGallery";
import { CommandPalette } from "./src/screens/command-palette";
import { Conversation } from "./src/screens/conversation";
import { deleteRequest } from "./src/screens/delete-dialog";
import { LoginScreen } from "./src/screens/login";
import { SessionModelChip, SessionModelPicker } from "./src/screens/model-picker";
import { PluginsScreen } from "./src/screens/plugins";
import { SessionList } from "./src/screens/session-list";
import { SessionStatusPane } from "./src/screens/session-status-pane";
import { SettingsScreen } from "./src/screens/settings";
import { restoreAppearance } from "./src/ui/appearance";
import { type IconName } from "./src/ui/icons";
import {
  AlertDialog,
  Badge,
  Button,
  color,
  IconButton,
  space,
  ToastViewport,
  type BadgeVariant,
  type ToastItem,
  type ToastTone,
  typeScale,
  weight,
  WORKSPACE_SHELL_MAIN_MIN_WIDTH_PX,
  WORKSPACE_SHELL_SINGLE_COLUMN_MAX_PX,
  WORKSPACE_SIDEBAR_DEFAULT_WIDTH_PX,
  SIDE_PANE_EXPANDED_RATIO,
  SIDE_PANE_MIN_WIDTH_PX,
  WorkspaceShell,
} from "./src/ui/kit";
import { conversationState, runStateLabel, runStateTone, type RunTone } from "./src/ui/run-state";
import { sessionTitle } from "./src/ui/session-title";

// ---- the route ------------------------------------------------------------

/** The routes the root knows how to render. The store's own `Route` gains
 * `plugins` and `kit` in its own entry; until that lands this file narrows the
 * store's value here (`as unknown as AppRoute`, at the one boundary between the
 * two) and every other value falls back to the workspace, so the two agree the
 * moment the store's union grows — same value space, no cross-file edit. */
type AppRoute = "workspace" | "settings" | "plugins" | "kit";

const appRoute = (): AppRoute => {
  const route = store.route() as unknown as AppRoute;
  return route === "settings" || route === "plugins" || route === "kit" ? route : "workspace";
};

// ---- the single column (≤767) ---------------------------------------------

/** The kit's own breakpoint, read only to know which screen the single column
 * shows: the shell decides the layout itself. */
const singleColumn = () => windowSize().width <= WORKSPACE_SHELL_SINGLE_COLUMN_MAX_PX;

/** Which screen the single column shows: the session list or the conversation.
 * A wide layout shows both at once and never reads it. Module-level, so leaving
 * to the settings page and coming back lands where the reader was. */
const screen = signal<"list" | "chat">("list");

const selectSession = (sessionId: string) => {
  void store.openSession(sessionId);
  // Set unconditionally: in the wide layout nothing reads it, but shrinking the
  // window to the single-column breakpoint must land on the session the reader
  // was in, not back on the list.
  screen.set("chat");
};

/** Back to the list; the conversation stays open (the list still marks it). */
const showSessionList = () => screen.set("list");

/** The open session's row — what the header's title comes from. */
const activeRow = (): SessionSummary | undefined => {
  const id = store.activeId();
  return id ? store.sessions().find((row) => row.sessionId === id) : undefined;
};

/** The header's title: the open session's (one source with the list), or the
 * app's own name while no session is open. */
const headerTitle = () => {
  const row = activeRow();
  return row ? sessionTitle(row) : "Keel";
};

/** The run state's badge family: the shared vocabulary's tone, in the kit's
 * variants. */
const stateBadge: Record<RunTone, BadgeVariant> = {
  neutral: "secondary",
  brand: "info",
  warning: "warning",
  danger: "destructive",
};

// ---- the right pane --------------------------------------------------------

/** The key the pane's own choice is kept under. */
const SIDE_PANE_STORAGE_KEY = "keel:workspace-shell:side-pane-open";

/** The pane starts CLOSED — the reference's rule for the right shared panel
 * (`useAppPanels.ts:200-208`: it must not eat the conversation's room before
 * the reader asks for it) — and only the stored choice or the header's toggle
 * opens it. So a 1280 window gives the conversation the whole pane width the
 * kit's column geometry is keyed on (1280 − 264 = 1016 → the 896/48 band). */
const sidePaneOpen = signal(false);

/** Can the shell hold three columns? The header's toggle appears only when the
 * sidebar, a conversation at the kit's own floor and the 45% pane fit at
 * once — below that the pane would squeeze the conversation past its minimum
 * and the header's own controls would clip (768px with the pane open is the
 * case that showed it). The shell's resize policy closes the pane on the way
 * down; this gates the way in. */
const sidePaneFits = () => {
  if (singleColumn()) return false;
  const width = windowSize().width;
  const pane = Math.max(SIDE_PANE_MIN_WIDTH_PX, Math.round(width * SIDE_PANE_EXPANDED_RATIO));
  return width - WORKSPACE_SIDEBAR_DEFAULT_WIDTH_PX - pane >= WORKSPACE_SHELL_MAIN_MIN_WIDTH_PX;
};

/** The pane's effective state: the reader's choice, only while it fits. */
const paneOpen = () => sidePaneOpen() && sidePaneFits();
/** The stored choice, once the host answered (`undefined` while it reads). */
let storedSidePane: string | null | undefined = undefined;
/** Someone decided: the user's toggle, or the shell's auto-collapse policy. */
let sidePaneTouched = false;
/** The stored choice was read (once per launch). */
let sidePaneRestored = false;
/** The stored appearance was applied. */
let appearanceApplied = false;

/** The header's toggle: the user's choice, kept for the next launch. */
const chooseSidePane = (open: boolean) => {
  sidePaneTouched = true;
  sidePaneOpen.set(open);
  void Storage.setItem(SIDE_PANE_STORAGE_KEY, open ? "1" : "0").catch(() => {
    /* a host without storage just forgets the choice */
  });
};

/** Boot: the stored choice (it is the user's) over the width's own default. */
const restoreSidePane = async () => {
  try {
    storedSidePane = await Storage.getItem(SIDE_PANE_STORAGE_KEY);
  } catch {
    /* no storage: the width's own answer stands */
  }
  if (sidePaneTouched) return;
  if (storedSidePane === "1" || storedSidePane === "0") sidePaneOpen.set(storedSidePane === "1");
};

// ---- the workspace header --------------------------------------------------

/** An action of the h-12 row. A terminal draws no icon: an icon-only button is
 * a view with no characters there — the TUI host neither shows it nor lets a
 * test address it — so a terminal gets the same action as a short text button,
 * the way the kit's own sidebar strip carries a glyph instead of its drawn
 * grip. One accessibility label either way. */
function HeaderAction(props: {
  icon: IconName;
  label: string;
  text: string;
  onPress: () => void;
  selected?: boolean;
  testID?: string;
}) {
  return (
    <If
      when={platform.idiom === "terminal"}
      else={
        <IconButton
          testID={props.testID}
          icon={props.icon}
          label={props.label}
          selected={props.selected}
          onPress={props.onPress}
        />
      }
    >
      <Button
        testID={props.testID}
        variant="ghost"
        size="sm"
        label={props.text}
        accessibilityLabel={props.label}
        selected={props.selected}
        onPress={props.onPress}
      />
    </If>
  );
}

/** What the shell's h-12 row holds: the narrow screen's back entry, the
 * session's title, its state word, the session's model chip and the right
 * pane's toggle. */
function WorkspaceHeader() {
  return (
    <View
      testID="workspace-header-content"
      style={{ flex: 1, minWidth: 0, flexDirection: "row", alignItems: "center", gap: space.md }}
    >
      <If when={singleColumn()}>
        <HeaderAction
          testID="workspace-header-back"
          icon="chevron-left"
          text="返回"
          label="返回会话列表"
          onPress={showSessionList}
        />
      </If>
      {/* The title's own column: a terminal lays a flexible text out at no
          width at all, so the flex belongs to this box, never to the Text.
          `minWidth` is the identity's floor: the title is the screen's name,
          so it keeps at least this much and the row's other occupants give way
          (the state word drops at the single column, the model chip becomes an
          icon there) instead of the title collapsing into "…". */}
      <View style={{ flex: 1, minWidth: 120 }}>
        <Text
          testID="workspace-header-title"
          style={{
            numberOfLines: 1,
            textOverflow: "ellipsis",
            fontSize: typeScale.base,
            fontWeight: weight.semibold,
            color: color.foreground,
          }}
        >
          {headerTitle()}
        </Text>
      </View>
      {/* The state word is the row's first concession: at the single column it
          goes (the run state still rides the pane and each row), so the title
          and the model control keep their room. */}
      <If when={!!store.activeId() && !singleColumn()}>
        <Badge
          testID="workspace-header-state"
          variant={stateBadge[runStateTone[conversationState(store.conversation())]]}
          label={runStateLabel[conversationState(store.conversation())]}
        />
      </If>
      {/* The model chip: its full label at the wide column; an icon-only button
          at the single column (the zcode mobile form), with the model's name in
          its accessibility label. A terminal keeps the text form — an icon-only
          view has no characters there — and the TUI acceptance reads the same
          label it always did. */}
      <If when={!!store.activeId()}>
        <If
          when={singleColumn() && platform.idiom !== "terminal"}
          else={<SessionModelChip />}
        >
          <SessionModelChip iconOnly />
        </If>
      </If>
      <If when={sidePaneFits()}>
        <HeaderAction
          testID="workspace-header-side-pane"
          icon="panel"
          text={paneOpen() ? "收起右栏" : "展开右栏"}
          label={paneOpen() ? "收起右侧状态栏" : "展开右侧状态栏"}
          selected={paneOpen()}
          onPress={() => chooseSidePane(!paneOpen())}
        />
      </If>
    </View>
  );
}

// ---- the workspace ---------------------------------------------------------

/** The workspace: the kit's shell with its three slots filled. On a single
 * column the sidebar slot stays empty on purpose — a drawer only the shell can
 * close would fight the app's own two screens — and the main column shows the
 * list or the conversation by `screen`. */
function Workspace() {
  return (
    <WorkspaceShell
      testID="workspace-shell"
      header={singleColumn() && screen() === "list" ? undefined : <WorkspaceHeader />}
      sidebar={
        singleColumn() ? undefined : <SessionList onSelect={selectSession} activeId={store.activeId()} wide />
      }
      main={
        singleColumn() && screen() === "list" ? (
          <SessionList onSelect={selectSession} activeId={store.activeId()} wide={false} />
        ) : (
          // `wide` is the layout word the conversation still speaks: off, its
          // composer and error banner go compact (the single column).
          <Conversation wide={!singleColumn()} />
        )
      }
      sidePane={
        // The pane's own screen (`src/screens/session-status-pane.tsx`): the
        // open session's model, usage, tool calls and plugins. Mounted only
        // while open, so its plugin read is not a boot-time request.
        <If when={paneOpen()}>
          <SessionStatusPane />
        </If>
      }
      sidePaneOpen={paneOpen()}
      onSidePaneOpenChange={(open) => {
        // The shell reports its own moves too (the auto-collapse after a
        // resize): the flag follows them, the stored choice does not.
        sidePaneTouched = true;
        sidePaneOpen.set(open);
      }}
    />
  );
}

// ---- the other routes ------------------------------------------------------

/** `/settings`, `/plugins` and `/kit` — the workspace's else. */
function Routed() {
  return (
    <If when={appRoute() === "workspace"} else={<NotWorkspace />}>
      <Workspace />
    </If>
  );
}

function NotWorkspace() {
  return (
    <If when={appRoute() === "settings"} else={<NotSettings />}>
      <SettingsScreen />
    </If>
  );
}

function NotSettings() {
  return (
    <If when={appRoute() === "plugins"} else={<KitGallery />}>
      <PluginsRoute />
    </If>
  );
}

/** The plugin route: the plugin page's own screen (`src/screens/plugins.tsx`),
 * the same one the settings page's 「打开插件页」 sets the route to. */
function PluginsRoute() {
  return <PluginsScreen />;
}

// ---- overlays --------------------------------------------------------------

/** The store's notices, drawn by the kit's viewport (the stack's placement is
 * the kit's own; nothing here offsets a toast). The store keeps one notice at a
 * time, so a toast goes when its notice does — the store's six-second timer or
 * a dismissal. */
const toasts = signal<ToastItem[]>([]);
let toastSeq = 0;
let lastNotice: Notice | null = null;

const noticeTone = (tone: Notice["tone"]): ToastTone =>
  tone === "warn" ? "warning" : tone === "info" ? "default" : tone;

function NoticeToasts() {
  effect(() => {
    const notice = store.notice();
    if (notice === lastNotice) return;
    const previous = lastNotice;
    lastNotice = notice;
    if (notice) {
      const id = `notice-${++toastSeq}`;
      toasts.update((list) => [...list, { id, title: notice.text, tone: noticeTone(notice.tone) }]);
    } else if (previous) {
      toasts.set([]);
    }
  });
  return (
    <ToastViewport
      testID="app-toasts"
      toasts={toasts()}
      onDismiss={(id) => {
        toasts.update((list) => list.filter((item) => item.id !== id));
        store.dismissNotice();
      }}
    />
  );
}

/** The one destructive confirmation, at the root: the kit's AlertDialog over
 * the request a list row raises, so it covers the sidebar and the conversation
 * both and never pushes the list around. */
function DeleteConfirm() {
  const confirmText = () => {
    const item = deleteRequest();
    return item ? `「${item.title}」及其消息将被删除，此操作不可撤销。` : "";
  };
  return (
    <AlertDialog
      testID="delete-session-dialog"
      open={!!deleteRequest()}
      title="删除会话？"
      description={confirmText()}
      confirmLabel="删除"
      cancelLabel="取消"
      destructive
      onConfirm={() => {
        const item = deleteRequest();
        deleteRequest.set(null);
        if (item) void store.deleteSession(item.sessionId);
      }}
      onCancel={() => deleteRequest.set(null)}
    />
  );
}

// The auth gate is an If, not an early return: a component body in NSH runs once
// and its reactive reads live in the vnode getters, so a body-level `if` would
// freeze the decision made before the async probe finished.
function App() {
  // Boot: the appearance the last session chose (the settings page's 外观
  // section writes it through the same source) …
  effect(() => {
    if (appearanceApplied) return;
    appearanceApplied = true;
    void restoreAppearance();
  });
  // … and the right pane: only the stored choice reopens it — no width-driven
  // default (the reference keeps the shared right panel closed until the reader
  // opens it; the kit's own policy then closes it again if the conversation
  // would fall under its 480px floor).
  effect(() => {
    if (sidePaneRestored) return;
    sidePaneRestored = true;
    void restoreSidePane();
  });
  return (
    <View style={{ flex: 1, backgroundColor: color.background }}>
      <If when={store.auth() === "required"} else={<Routed />}>
        <LoginScreen />
      </If>
      <NoticeToasts />
      <DeleteConfirm />
      <SessionModelPicker />
      {/* The global palette: mounted at the root so Mod+K opens it over every
          route, and a choice can leave the settings/plugin page for the
          workspace. It opens a session through the same `selectSession` the
          sidebar uses, so the ≤767 single column follows the choice into the
          conversation. */}
      <CommandPalette onOpenSession={selectSession} />
    </View>
  );
}

mount(App);
