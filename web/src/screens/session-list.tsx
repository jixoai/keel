// The session list — the *content* of the kit's Sidebar panel (change
// keel-harness-shell, tasks L1–L3).
//
// The shell owns the panel this screen fills: the sidebar's default and minimum
// width, its container-relative ceiling, the keyboard step, the collapsed strip
// and the persisted width key all live in
// `web/src/ui/kit/layout/sidebar.tsx`. This file draws no width, no breakpoint
// and no other shell number of its own (`scripts/kit-imports.mjs` gates that),
// and it never mounts a `Sidebar` — the shell does.
//
// What the panel holds, top to bottom:
//   · the header row — h-12 (`WORKSPACE_SHELL_HEADER_HEIGHT_PX`, the height the
//     shell gives its own header): the logo tile (12px-rounded, the reference's
//     titlebar mark), the wordmark, and two `IconButton`s — 新建会话 / 刷新会话.
//     The settings entry is the shell's (the desktop rail's bottom row, the
//     phone's bottom-bar gear), so the list does not repeat it (A4.4f).
//   · the search field — the kit's `Input` (the hand-drawn glass pill is gone).
//   · the rows — `Each` over the store's own order, so a refresh that only
//     changes state updates rows in place (the polish change's C5). A row is a
//     clickable composition in the kit's vocabulary: a `View` that takes the
//     press (`role="button"`, one `accessibilityLabel`,
//     `accessibilityState.selected`), flat like the reference's
//     (`TaskListItem.tsx:570-577`: no fill, no outline, `radius.md`) with the
//     theme's washes for the pointer and for the selection, and the title /
//     preview / relative time and the one run-state Badge. Its actions are a
//     `DropdownMenu` that shows while the pointer is on the row (or the row is
//     the open one — a terminal has no hover), and the right click — or a
//     held finger — is the kit's `ContextMenu`; the row is that menu's
//     *content*, never its anchored element, so its own name stays its own.
//     An untitled session
//     reads「新会话 · <相对时间>」(`src/ui/session-title.ts` is the title's single
//     source — the rows, the workspace header and the conversation read one
//     name), and a raw session id is never rendered.
//   · no footer: the connection status moved to the settings page's account
//     card (A4.4f), together with the token-level switch. The product surface
//     carries no mode wording at all (task X1).
//
// Registered deviations from the zcode reference:
//   · the selected row's face is `color.sidebarAccent`. The reference paints
//     `bg-selected` (`styles.css:698`), which has no Keel token;
//     `sidebar-accent` is the theme's own wash over the sidebar and is the only
//     selection face this file draws.
//   · the rows are flat like the reference's (`TaskListItem.tsx:570-577`): no
//     fill, no outline, `radius.md`, and their actions appear on the pointer
//     (or on the open row in a terminal). The washes are the only faces: a
//     card face made the selected row the darkest one on the column (round-2
//     review), and an outline around every row turned the column into eleven
//     competing cards that starved the titles.
//   · the panel header names the list only (会话 / AI 员工会话): the product
//     brand lives in the global system bar, not twice on one screen.
import { Clipboard, cleanup, effect, signal, Text, type TextStyle, View } from "nsh";
import type { SessionSummary } from "../data/contract";
import { store } from "../data/store";
import { formatTime } from "../data/wire";
import {
  Badge,
  Button,
  ContextMenu,
  ContextMenuItem,
  DropdownMenu,
  DropdownMenuItem,
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  IconButton,
  Input,
  ScrollArea,
  Skeleton,
  Tooltip,
  WORKSPACE_SHELL_HEADER_HEIGHT_PX,
  color,
  focusRing,
  radius,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../ui/kit";
import {
  conversationState,
  queueDetail,
  runStateLabel,
  sessionState,
  type QueueFacts,
  type RunState,
} from "../ui/run-state";
import { oneLine, sessionTitle } from "../ui/session-title";
import { exportSummary, searchAvailable, sessionQueryAccess } from "../data/session-query";
import { PluginSidebarSlot } from "../plugins/slots";
import { requestDelete } from "./delete-dialog";

const query = signal("");
const refreshing = signal(false);
/** The list's source filter: every visible session, only mine, or the ones
 * shared with me (direct, group or team — the server's `sharedWithMe` already
 * folds those into one caller relative fact). */
const source = signal<"all" | "mine" | "shared">("all");

const isMine = (row: SessionSummary): boolean => {
  const me = store.identity()?.accountId;
  return !!me && row.ownerAccountId === me;
};

/** The row's preview line: the last message, or the fact that there is none.
 * A session shared with this reader says who shared it first — that is the one
 * fact the reader needs before opening it. The title is not derived here:
 * `../ui/session-title` is its single source (the list rows, the workspace
 * header and the conversation read one name). */
const summaryOf = (row: SessionSummary): string => {
  const preview = oneLine(row.lastMessage) || "还没有消息";
  if (row.sharedWithMe && row.ownerDisplayName) return `由 ${row.ownerDisplayName} 共享 · ${preview}`;
  return preview;
};

/** The one-word state of a row: the server's queued status wins (the
 * conversation view has no queue fact), then the active row projects the open
 * conversation (which knows about stopping), every other row projects
 * runStatus. A client-side queue fact never speaks for the server: only a row
 * whose runStatus is still queued may render 排队中. */
const rowStateOf = (row: SessionSummary, active: boolean): RunState => {
  const state = sessionState(row.runStatus);
  if (state === "queued") return "queued";
  if (active && store.conversation()) return conversationState(store.conversation());
  return state;
};

/** The single status word a row shows, or none at all.
 *
 * A parked question is not a run: `waiting` gets the badge and never motion
 * (the polish change's C2). A queued admission always gets its badge — even on
 * the active row — with the FIFO position/ETA the server reported; otherwise
 * the active row stays quiet about its own run because the conversation's
 * header already says what it is doing. A failure is never hidden. */
const rowBadgeOf = (
  state: RunState,
  waiting: boolean,
  active: boolean,
  queue?: QueueFacts,
): { variant: BadgeVariant; label: string; icon?: "clock" | "alert" } | null => {
  if (waiting) return { variant: "warning", label: runStateLabel.waiting, icon: "clock" };
  if (state === "queued") return { variant: "info", label: queueDetail(queue), icon: "clock" };
  if (active) return null;
  if (state === "failed") return { variant: "destructive", label: runStateLabel.failed, icon: "alert" };
  if (state === "running") return { variant: "info", label: runStateLabel.running };
  if (state === "stopping" || state === "reconnecting") return { variant: "warning", label: runStateLabel[state] };
  return null;
};

/** The search's own emphasis: the matched run of a title/preview, in the theme's
 * mark pair (the kit has no highlight primitive; this is one `Text` span). */
function Highlighted(props: { text: string; style: TextStyle }) {
  const parts = () => {
    const needle = query().trim().toLowerCase();
    if (!needle) return [{ text: props.text, match: false }];
    const haystack = props.text.toLowerCase();
    const out: Array<{ text: string; match: boolean }> = [];
    let index = 0;
    while (index < props.text.length) {
      const at = haystack.indexOf(needle, index);
      if (at < 0) {
        out.push({ text: props.text.slice(index), match: false });
        break;
      }
      if (at > index) out.push({ text: props.text.slice(index, at), match: false });
      out.push({ text: props.text.slice(at, at + needle.length), match: true });
      index = at + needle.length;
    }
    return out;
  };
  return (
    <Text style={props.style}>
      {parts().map((part) =>
        part.match ? (
          <Text style={{ backgroundColor: color.mark, color: color.markForeground }}>{part.text}</Text>
        ) : (
          part.text
        ),
      )}
    </Text>
  );
}

/** One row of the list: pressable, selectable, and the subject of two menus —
 * the trailing `DropdownMenu` (a pointer, the wide layout) and the
 * `ContextMenu` a right click (or a held finger) opens over it. */
function SessionRow(props: { row: () => SessionSummary; active: boolean; wide: boolean; onSelect: () => void }) {
  const row = () => props.row();
  const title = () => sessionTitle(row());
  const preview = () => summaryOf(row());
  const time = () => formatTime(row().lastMessageAt);
  const waiting = () => store.waiting(row().sessionId);
  const badge = () => rowBadgeOf(rowStateOf(row(), props.active), waiting(), props.active, row().queue);
  const openDelete = () => requestDelete(row().sessionId, title());
  /** Export the session's transcript (one bounded page) to the clipboard. The
   * read is the server's export; this only delivers the exact lines it
   * returned, so no second export shape exists in the client. */
  const openExport = async () => {
    try {
      const { view, jsonl } = await sessionQueryAccess.exportSession(row().sessionId);
      if (jsonl) await Clipboard.writeText(jsonl);
      store.flash({ tone: view.messages?.length ? "success" : "info", text: exportSummary(view) });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      store.flash({ tone: "danger", text: `导出失败：${message}` });
    }
  };
  /** The pointer is on the row: the wide layout's overflow action only shows
   * then (or while the row is the open one — a terminal has no hover). */
  const hovered = signal(false);
  /** The keyboard focus is on (or inside) the row. The overflow action is
   * pointer-only unless this is tracked too: a row is `focusable`, so Tab
   * lands on it, and without this the one control that reaches 「删除会话」
   * would stay unmounted for every keyboard user (2.1.1: a pointer path with
   * no keyboard path). */
  const focused = signal(false);

  const trigger = (
    // The row is the trigger's *content*, never the trigger's element: the
    // `ContextMenu` gives its slot's accessibility, focus and press props to
    // the one element it is anchored to (and takes them back as the slot
    // reconciles), so the row keeps its own name (`打开会话 <标题>`), role and
    // press handler on a node of its own.
    <View style={{ flexDirection: "column" }}>
      <View
        testID={`session-row-${row().sessionId}`}
        role="button"
        focusable
        onPress={props.onSelect}
        accessibilityLabel={`打开会话 ${title()}`}
        accessibilityState={{ selected: props.active }}
        onMouseEnter={() => hovered.set(true)}
        onMouseLeave={() => hovered.set(false)}
        // Tabbing onto the row reveals the same action the pointer does.
        onFocus={() => focused.set(true)}
        onBlur={() => focused.set(false)}
        // The selection is the accessibility state itself (`selectedStyle`), so
        // the face and `accessibilityState.selected` can never drift apart. The
        // row is flat like the reference's (`TaskListItem.tsx:570-577`, no
        // fill, no outline): the washes are the only faces, so a selected row
        // is a highlight over the sidebar — never a darker card than its
        // neighbours, and never a bordered one (that card look is what made
        // eleven outlined tiles fight over the column's width).
        selectedStyle={{ backgroundColor: color.sidebarAccent }}
        hoverStyle={props.active ? undefined : { backgroundColor: color.hover }}
        focusStyle={focusRing()}
        style={{
          flexDirection: "row",
          alignItems: "center",
          gap: space.sm,
          paddingHorizontal: space.md,
          paddingVertical: space.sm,
          borderRadius: radius.md,
        }}
      >
        <View style={{ flexDirection: "column", flex: 1, minWidth: 0, gap: space.xxs }}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
            <Highlighted
              text={title()}
              style={{
                flex: 1,
                numberOfLines: 1,
                textOverflow: "ellipsis",
                fontSize: typeScale.base,
                fontWeight: props.active ? weight.semibold : weight.medium,
                color: color.sidebarForeground,
              }}
            />
            <If when={badge()}>
              {(current) => <Badge variant={current().variant} label={current().label} icon={current().icon} />}
            </If>
          </View>
          {/* The second line is the row's metadata line: the last message, and
              when it was — the title's line keeps the room the timestamp would
              take (a 264px column: a CJK title needs two cells a character, and
              the status word alone eats ten). */}
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
            <Highlighted
              text={preview()}
              style={{
                flex: 1,
                numberOfLines: 1,
                textOverflow: "ellipsis",
                fontSize: typeScale.sm,
                color: color.mutedForeground,
              }}
            />
            <If when={time() !== ""}>
              {/* 12px, not 10: UI text below 12px stops being readable at 1×,
                  and a CJK timestamp at 10px is a smear. `flexShrink: 0` keeps
                  the time whole — the preview next to it is the value allowed
                  to truncate (truncation with no way back is the worse
                  failure). */}
              <Text
                style={{
                  flexShrink: 0,
                  fontSize: typeScale.sm,
                  color: color.mutedForeground,
                  fontVariant: ["tabular-nums"],
                }}
              >
                {time()}
              </Text>
            </If>
          </View>
        </View>
        {/* The row's actions are a pointer's — or the keyboard's, once the row
            holds the focus (a terminal has no hover, so the open row keeps
            them too). The title keeps the room they would take. */}
        <If when={props.wide && (hovered() || focused() || props.active)}>
          <DropdownMenu
            accessibilityLabel={`会话操作 ${title()}`}
            trigger={
              <Button
                icon="dots"
                size="icon"
                variant="ghost"
                accessibilityLabel="会话操作"
                tooltip={<Tooltip label="会话操作" />}
              />
            }
          >
            <DropdownMenuItem label="导出会话" icon="download" onSelect={() => void openExport()} />
            <DropdownMenuItem label="删除会话" icon="trash" danger onSelect={openDelete} />
          </DropdownMenu>
        </If>
      </View>
    </View>
  );

  return (
    <ContextMenu
      accessibilityLabel={`会话操作 ${title()}`}
      trigger={trigger}
      menu={
        <View style={{ flexDirection: "column" }}>
          <ContextMenuItem label="导出会话" icon="download" onSelect={() => void openExport()} />
          <ContextMenuItem label="删除会话" icon="trash" danger onSelect={openDelete} />
        </View>
      }
    />
  );
}

/** The list while the first load runs: the kit's pulsing blocks, one per row. */
function SessionListSkeleton() {
  return (
    <View style={{ flexDirection: "column", gap: space.lg, padding: space.md }}>
      <Skeleton lines={2} accessibilityLabel="正在加载会话" />
      <Skeleton lines={2} />
      <Skeleton lines={2} />
      <Skeleton lines={2} />
    </View>
  );
}

/** A list that could not be read: the reason the store kept, and one retry. */
function SessionListError(props: { message: string; retrying: boolean; onRetry: () => void }) {
  return (
    <View style={{ padding: space.md }}>
      <Empty accessibilityLabel="会话列表加载失败" testID="session-list-error">
        <EmptyMedia icon="alert" tone="danger" />
        <EmptyTitle label="会话列表加载失败" />
        <EmptyDescription label={props.message} />
        <EmptyContent>
          <Button
            variant="outline"
            icon="refresh"
            label="重试"
            loading={props.retrying}
            onPress={props.onRetry}
            testID="session-list-retry"
          />
        </EmptyContent>
      </Empty>
    </View>
  );
}

/** No rows at all, or none left after the search: the kit's empty state, with
 * the one action that makes sense in each case. */
function SessionListEmpty(props: { searching: boolean }) {
  return (
    <Empty
      accessibilityLabel={props.searching ? "没有匹配的会话" : "还没有会话"}
      testID="session-list-empty"
    >
      <EmptyMedia icon="message" />
      <EmptyTitle label={props.searching ? "没有匹配的会话" : "还没有会话"} />
      <EmptyDescription
        label={props.searching ? "换个关键词，或者清除搜索看看全部会话。" : "新建一个会话，Keel 会从第一条消息开始。"}
      />
      <EmptyContent>
        <If
          when={props.searching}
          else={<Button icon="plus" label="新建会话" onPress={() => void store.createSession()} />}
        >
          <Button variant="outline" icon="x" label="清除搜索" onPress={() => query.set("")} />
        </If>
      </EmptyContent>
    </Empty>
  );
}

/** The server-backed message hits for the search box. It sits above the
 * session rows because it can reach sessions the first list page does not
 * hold; a deployment without the read renders nothing here and the local
 * filter still works. */
function MessageHits(props: { onSelect: (id: string) => void }) {
  const hits = () => sessionQueryAccess.hits();
  const searching = () => sessionQueryAccess.searched() && !sessionQueryAccess.loading();
  return (
    <View testID="session-message-hits" style={{ flexDirection: "column", gap: space.xs, paddingHorizontal: space.md }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <Text style={{ flex: 1, fontSize: typeScale.sm, color: color.mutedForeground }}>
          消息结果{hits().length > 0 ? ` · ${hits().length} 条` : ""}
        </Text>
        <If when={sessionQueryAccess.loading()}>
          <Text testID="session-message-search-loading" style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
            搜索中…
          </Text>
        </If>
      </View>
      <If when={sessionQueryAccess.error() !== null}>
        <Text testID="session-message-search-error" style={{ fontSize: typeScale.sm, color: color.danger }}>
          {sessionQueryAccess.error() ?? ""}
        </Text>
      </If>
      <If when={searching() && hits().length === 0 && sessionQueryAccess.error() === null}>
        <Text testID="session-message-search-empty" style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          没有匹配的历史消息。
        </Text>
      </If>
      <Each of={hits()} key={(hit) => `${hit.sessionId}:${hit.seq}`}>
        {(hit) => (
          <View
            testID={`session-message-hit-${hit().sessionId}-${hit().seq}`}
            role="button"
            focusable
            onPress={() => props.onSelect(hit().sessionId)}
            accessibilityLabel={`打开消息 ${hit().sessionTitle}`}
            hoverStyle={{ backgroundColor: color.hover }}
            style={{
              flexDirection: "column",
              gap: space.xxs,
              paddingVertical: space.xs,
              borderBottomWidth: 1,
              borderBottomColor: color.border,
            }}
          >
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
              {hit().sessionTitle || "未命名会话"}
            </Text>
            <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground, numberOfLines: 2 }}>
              {hit().snippet}
            </Text>
          </View>
        )}
      </Each>
      <If when={sessionQueryAccess.hasMore()}>
        <Button
          testID="session-message-search-more"
          variant="ghost"
          size="sm"
          label="继续搜索"
          onPress={() => void sessionQueryAccess.more()}
        />
      </If>
    </View>
  );
}

export function SessionList(props: { onSelect: (id: string) => void; activeId: string | null; wide: boolean }) {
  // The store's order, filtered only by the search box (never re-sorted, never
  // re-created): a refresh that changes state must not move a row (C5). The
  // search reads what the rows show — title and preview, never the id — so a
  // match is always visible in the row it keeps.
  // The local filter is instant; the server read is debounced so typing does
  // not scan the transcript for every keystroke. It is the one entry into
  // bounded message search.
  effect(() => {
    const text = query().trim();
    if (!searchAvailable() || text.length < 2) {
      sessionQueryAccess.clear();
      return;
    }
    const timer = setTimeout(() => void sessionQueryAccess.search(text), 250);
    cleanup(() => clearTimeout(timer));
  });
  const rows = () => {
    const needle = query().trim().toLowerCase();
    const filter = source();
    const all = store.sessions().filter((row) =>
      filter === "mine" ? isMine(row) : filter === "shared" ? row.sharedWithMe === true : true,
    );
    if (!needle) return all;
    return all.filter((row) => `${sessionTitle(row)} ${summaryOf(row)}`.toLowerCase().includes(needle));
  };
  const refresh = async () => {
    refreshing.set(true);
    await store.refreshSessions();
    refreshing.set(false);
  };
  return (
    <View testID="session-list" style={{ flexDirection: "column", flex: 1, minWidth: 0 }}>
      {/* L1 · the panel's own header, h-12 — the shell's header height. */}
      <View
        style={{
          height: WORKSPACE_SHELL_HEADER_HEIGHT_PX,
          flexShrink: 0,
          flexDirection: "row",
          alignItems: "center",
          gap: space.sm,
          paddingHorizontal: space.md,
          borderBottomWidth: 1,
          borderColor: color.sidebarBorder,
        }}
      >
        {/* The global system bar carries the product's brand and the window's
            drag area; this panel's header names the panel only. */}
        <View style={{ flexDirection: "column", flex: 1, minWidth: 0 }}>
          <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.sidebarForeground }}>
            会话
          </Text>
          {/* 12px is the UI floor; 9px CJK ink read as a solid block. */}
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>AI 员工会话</Text>
        </View>
        {/* The two icon buttons are borderless and gapless-looking at 6px: a
            row gap one step wider (8px) plus their own padding keeps them apart
            as two targets, so 「新建会话」 and 「刷新会话」 are not one press
            away from each other by accident. */}
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
          <IconButton icon="plus" label="新建会话" onPress={() => void store.createSession()} />
          <IconButton
            icon="refresh"
            label="刷新会话"
            disabled={refreshing()}
            onPress={() => void refresh()}
          />
        </View>
      </View>
      {/* L1 · the search: the kit's Input, nothing drawn around it. */}
      <View style={{ paddingHorizontal: space.md, paddingVertical: space.md }}>
        <Input
          value={query()}
          onChange={(value) => query.set(value)}
          placeholder="搜索会话或消息"
          accessibilityLabel="搜索会话"
          testID="session-search"
        />
      </View>
      {/* The source filter: the server already decided visibility and computed
          `sharedWithMe` against this account, so these are projections of one
          list, never three reads. */}
      <View
        testID="session-source-filter"
        style={{ flexDirection: "row", alignItems: "center", gap: space.xs, paddingHorizontal: space.md, paddingBottom: space.md }}
      >
        <Button
          testID="session-filter-all"
          variant={source() === "all" ? "secondary" : "ghost"}
          size="sm"
          label="全部"
          selected={source() === "all"}
          onPress={() => source.set("all")}
        />
        <Button
          testID="session-filter-mine"
          variant={source() === "mine" ? "secondary" : "ghost"}
          size="sm"
          label="我的"
          selected={source() === "mine"}
          onPress={() => source.set("mine")}
        />
        <Button
          testID="session-filter-shared"
          variant={source() === "shared" ? "secondary" : "ghost"}
          size="sm"
          label="共享给我"
          selected={source() === "shared"}
          onPress={() => source.set("shared")}
        />
      </View>
      <ScrollArea
        testID="session-list-scroll"
        accessibilityLabel="会话列表"
        style={{ flex: 1, minHeight: 0 }}
      >
        {/* The branches are separate `If`s, never one ternary over `rows()`:
            a ternary whose condition reads `rows().length` is one computation,
            so every refresh — a new array of the same sessions — re-ran it and
            replaced the rows (a new element each run), deleting and re-creating
            every row's native view. A press that landed in that window found a
            row's label with no native view. Each `If` mounts once for its
            boolean, and the rows below it stay the same nodes. */}
        <If when={searchAvailable() && query().trim().length >= 2}>
          <MessageHits onSelect={props.onSelect} />
        </If>
        <If when={store.sessionsLoading()}>
          <SessionListSkeleton />
        </If>
        <If when={!store.sessionsLoading() && store.sessionsError() !== null}>
          <SessionListError
            message={store.sessionsError() ?? ""}
            retrying={refreshing()}
            onRetry={() => void refresh()}
          />
        </If>
        <If when={!store.sessionsLoading() && store.sessionsError() === null && rows().length === 0}>
          <SessionListEmpty searching={query().trim().length > 0} />
        </If>
        <If when={!store.sessionsLoading() && store.sessionsError() === null && rows().length > 0}>
          <View style={{ flexDirection: "column", gap: space.sm, padding: space.md }}>
            <Each of={rows()} key={(row) => row.sessionId}>
              {(row) => (
                <SessionRow
                  row={row}
                  active={props.activeId === row().sessionId}
                  wide={props.wide}
                  onSelect={() => props.onSelect(row().sessionId)}
                />
              )}
            </Each>
          </View>
        </If>
      </ScrollArea>
      {/* The sidebar slot: plugin contributions that belong beside the session
          list. It renders nothing at all when no plugin contributes (no empty
          strip, no fake row) — the same rule as the top band. */}
      <PluginSidebarSlot />
    </View>
  );
}

