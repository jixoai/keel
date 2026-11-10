// The global command palette (change keel-harness-shell, task S2): Mod+K —
// Command on Apple platforms, Control elsewhere (nsh's `keymap`) — opens one
// search over the four things the app acts on from anywhere: the sessions, the
// open session's model picker, the settings page and the plugin page.
//
// The rows are ONE data source. `paletteActions()` builds a `PaletteAction` per
// row — the `CommandItem` it shows and the handler it runs — so the list the
// kit draws and what a choice does cannot drift apart. Session rows come from
// the store's own list (their titles through `sessionTitle`, their previews as
// keywords, never a raw session id); the model, settings and plugin rows are
// the three route commands. `items` is that source mapped to `CommandItem`s and
// `onSelect` looks the same id up in it.
//
// All chrome is the kit's: `Dialog` is the modal card (no title and no close
// button — the empty palette's Escape and the `Command`'s own cancel close it),
// `Command` is the search field, the section headers and the virtualized rows.
// This file draws no list, no search box and no row, writes no width and no
// colour, and every size it names is a token (`scripts/kit-imports.mjs` gates
// the consumer surface).
//
// The search field takes the keyboard focus on open (`autoFocus`): the kit's
// `Command` forwards it to the native command, whose own `Input` focuses the
// field once shown, so Mod+K opens a palette that is typed into directly.
//
// Escape, stated as the platform has it: with an empty query one press closes
// the palette (the command reports cancel). With a query the native command's
// own Escape first clears it, and only a second press closes. That binding is
// a view-level shortcut (`nsh-base/src/command.rs:983-1005`), and the surface
// table chains JS's `keymap` bindings before the views' — "a view's wins over
// JS's of the same keystroke" (`nsh-core/src/core/components.rs:5580-5595`) —
// so the app cannot bind Escape over it, and the dialog layer hears nothing
// to close on. A one-press close needs the native command to give Escape up
// (a prop or an nsh change), which this screen cannot do.
import { keymap, platform, signal, View, windowSize } from "nsh";
import type { CommandItem } from "../ui/kit";
import { Command, Dialog, Empty, EmptyDescription, EmptyTitle, space } from "../ui/kit";
import { store } from "../data/store";
import { sessionTitle } from "../ui/session-title";
import { openModelPicker } from "./model-picker";

/** Whether the palette is up. Module-level like the app's other overlay
 * signals (`pickerOpen`, `deleteRequest`), so the shortcut and any future
 * trigger share one flag. */
export const paletteOpen = signal(false);

/** The filter text. Controlled, so opening always starts from an empty query
 * and a reopen never inherits the last search. */
const paletteQuery = signal("");

/** Open the palette on an empty query (what Mod+K does). */
export const openPalette = () => {
  if (paletteOpen()) return;
  paletteQuery.set("");
  paletteOpen.set(true);
};

export const closePalette = () => paletteOpen.set(false);

/** How long a chosen action waits after the palette closes before it runs, on
 * platforms whose modal owns a browser history entry: the runtime's modal exit
 * (350 ms) plus its history step. */
const CLOSE_SETTLE_MS = 400;

/** The four section headings, in the order the rows appear. */
const GROUP = {
  sessions: "会话",
  model: "模型",
  settings: "设置",
  plugins: "插件",
} as const;

/** A row and what choosing it does: the one record both the items and the
 * selection read. */
interface PaletteAction {
  item: CommandItem;
  run: () => void;
}

/** Back to the workspace before a workspace action. The settings and plugin
 * routes replace the shell, and both the session switch and the model picker
 * belong to the workspace (the picker's own Dialog only shows there). */
const toWorkspace = () => store.closeSettings();

/** How a session row opens its session. The app root passes its own
 * `selectSession` (it also moves the ≤767 single column from the list to the
 * conversation); without one the store's open alone is the action. */
export type OpenPaletteSession = (sessionId: string) => void;

const defaultOpenSession: OpenPaletteSession = (sessionId) => {
  void store.openSession(sessionId);
};

const chooseSession = (openSession: OpenPaletteSession, sessionId: string) => {
  toWorkspace();
  openSession(sessionId);
};

const chooseModel = () => {
  toWorkspace();
  // The same entry the header's model chip uses (`model-picker.tsx`): one
  // picker, two doors.
  openModelPicker();
};

const chooseSettings = () => store.openSettings();

/** The plugin page (`/plugins`): the store's own route, with the page URL and
 * the back button in step (the same entry the settings page uses). */
const choosePlugins = () => store.openPlugins();

/** Every row, from the one source: the store's sessions first (its own order),
 * then the model picker, the settings page and the plugin page. */
const paletteActions = (openSession: OpenPaletteSession): PaletteAction[] => {
  const sessions: PaletteAction[] = store.sessions().map((row) => ({
    item: {
      id: `session:${row.sessionId}`,
      label: sessionTitle(row),
      // The preview is how a session is searched besides its title (two
      // untitled sessions both read「新会话 · …」; their previews tell them
      // apart). A raw id never enters the UI, label or keyword.
      keywords: [row.lastMessage ?? ""].filter((text) => text.length > 0),
      group: GROUP.sessions,
    },
    run: () => chooseSession(openSession, row.sessionId),
  }));
  return [
    ...sessions,
    {
      item: {
        id: "model:session",
        label: "切换本会话模型",
        keywords: ["模型", "model", "provider", "思考档", "reasoning"],
        group: GROUP.model,
        // No open session, no session model: the row reads dimmed and the
        // keys pass over it.
        disabled: !store.activeId(),
      },
      run: chooseModel,
    },
    {
      item: {
        id: "route:settings",
        label: "打开设置",
        keywords: ["设置", "settings", "偏好", "外观"],
        group: GROUP.settings,
      },
      run: chooseSettings,
    },
    {
      item: {
        id: "route:plugins",
        label: "打开插件页",
        keywords: ["插件", "plugins", "扩展", "extension"],
        group: GROUP.plugins,
      },
      run: choosePlugins,
    },
  ];
};

/** The `Command`'s rows: the same actions, without their handlers. */
const paletteItems = (openSession: OpenPaletteSession): CommandItem[] =>
  paletteActions(openSession).map((action) => action.item);

export function CommandPalette(props: {
  /** How a session row opens its session; the app root passes `selectSession`
   * so the ≤767 single column follows the choice into the conversation. */
  onOpenSession?: OpenPaletteSession;
} = {}) {
  const openSession = props.onOpenSession ?? defaultOpenSession;

  /** The list's height cap: the palette grows until every row shows and only
   * then scrolls. A fixed 320px cap cut the last row against the card's rounded
   * bottom edge; the window's own height is the honest ceiling here (the card
   * is centred with `margin: auto`). */
  const listCap = () => Math.max(240, Math.min(560, windowSize().height - 200));

  // The app's shortcut. Bound while this component renders (nsh removes a
  // binding with the component that made it); `mod` is Command on Apple
  // platforms and Control elsewhere.
  keymap.bind("mod+k", () => openPalette());

  // The row chosen while the modal goes. On the web a modal owns a browser
  // history entry, and the close steps back over it — the host reports the
  // previous URL, and the store's route listener follows it. A route action
  // (settings, plugins, a session) run in that breath is undone by it, so the
  // action waits here until the close has settled (the runtime's modal exit,
  // 350 ms, plus its history step); its `pushState` is then the last word on
  // the URL. A terminal has no history entry to step over, so its action runs
  // at once.
  const select = (id: string) => {
    const action = paletteActions(openSession).find((entry) => entry.item.id === id);
    const run = action?.run ?? null;
    closePalette();
    if (platform.idiom === "terminal") {
      run?.();
      return;
    }
    if (run) setTimeout(run, CLOSE_SETTLE_MS);
  };

  return (
    <Dialog
      open={paletteOpen()}
      onOpenChange={(event) => {
        if (!event.open) closePalette();
      }}
      size="lg"
      // A palette is its search field: no title row and no close button. The
      // card's own padding would inset the search bar from the surface; the
      // content fills it instead (the Command below is the card's body).
      showClose={false}
      style={{ padding: 0, margin: "auto" }}
      accessibilityLabel="命令面板"
      testID="command-palette"
    >
      <View style={{ flexDirection: "column" }}>
      <Command
        // `id:` also finds the search field for `type` in tests.
        testID="command-palette"
        // A palette opens to be typed in: the field takes the focus at once.
        autoFocus
        items={paletteItems(openSession)}
        query={paletteQuery()}
        onQueryChange={(query) => paletteQuery.set(query)}
        onSelect={select}
        // Escape with an empty query: the platform's close path and the
        // command's own cancel both end at the same flag. With a query the
        // native command clears it first (the header note).
        onCancel={closePalette}
        placeholder="搜索会话、模型、设置或插件…"
        accessibilityLabel="命令面板"
        searchAccessibilityLabel="搜索命令"
        maxHeight={listCap()}
        // The kit's own empty slot, in the kit's empty-state vocabulary: an
        // empty query lists everything, so this is only a query that matches
        // nothing.
        empty={
          <Empty accessibilityLabel="没有匹配的命令">
            <EmptyTitle label="没有匹配的命令" />
            <EmptyDescription label="换个关键词试试：会话、模型、设置、插件。" />
          </Empty>
        }
        // The Dialog is the card; a standalone Command draws its own surface.
        // Clearing it here keeps the border and the shadow to one layer.
        style={{ backgroundColor: "transparent", borderWidth: 0, boxShadow: "none" }}
      />
      {/* The card's own bottom inset: the list stops above the rounded corner,
          so the last row — and its highlight — never touches the edge. */}
      <View style={{ height: space.md }} />
      </View>
    </Dialog>
  );
}
