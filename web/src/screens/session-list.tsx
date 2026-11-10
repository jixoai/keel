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
//     titlebar mark), the wordmark, and three `IconButton`s — 新建会话 / 刷新会话 /
//     the *one* settings entry (打开设置). The footer's second settings control
//     is gone.
//   · the search field — the kit's `Input` (the hand-drawn glass pill is gone).
//   · the rows — `Each` over the store's own order, so a refresh that only
//     changes state updates rows in place (the polish change's C5). A row is a
//     clickable composition in the kit's vocabulary: a `View` that takes the
//     press (`role="button"`, one `accessibilityLabel`,
//     `accessibilityState.selected`), the Card's geometry (radius + hairline)
//     with the theme's washes for the pointer and for the selection, the title /
//     preview / relative time and the one run-state Badge. Its actions are a
//     `DropdownMenu` (no opacity-faded button any more), and the secondary click
//     — or a held finger — is the kit's `ContextMenu`. An untitled session
//     reads「新会话 · <相对时间>」(`src/ui/session-title.ts` is the title's single
//     source — the rows, the workspace header and the conversation read one
//     name), and a raw session id is never rendered.
//   · the footer — the connection status Badge and nothing else: the mode
//     switches, the token-level switch (moved to the settings page) and the
//     data-source prose are gone (task X1: the product surface carries no mode
//     wording at all).
//
// Registered deviations from the zcode reference:
//   · the selected row's face is `color.sidebarAccent`. The reference paints
//     `bg-selected` (`styles.css:698`), which has no Keel token;
//     `sidebar-accent` is the theme's own wash over the sidebar and is the only
//     selection face this file draws.
//   · the rows are flat like the reference's (`TaskListItem.tsx:570-578`): the
//     Card's contribution is its geometry (radius + hairline), never a fill. A
//     card face made the selected row the darkest one on the column (round-2
//     review), so the washes are the only faces.
//   · the header's logo is a token tile with `radius.lg` (12px) — the shape the
//     reference's titlebar logo has (`DesktopTopOverlay.tsx:144`,
//     `rounded-lg`). The kit `Avatar`'s pill is outside the 6/12/16 ladder.
import { signal, Text, type TextStyle, View } from "nsh";
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
  Separator,
  Skeleton,
  Spinner,
  Tooltip,
  WORKSPACE_SHELL_HEADER_HEIGHT_PX,
  color,
  controlHeight,
  focusRing,
  iconSize,
  radius,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../ui/kit";
import { Icon } from "../ui/icons";
import { conversationState, runStateLabel, sessionState, type RunState } from "../ui/run-state";
import { oneLine, sessionTitle } from "../ui/session-title";
import { requestDelete } from "./delete-dialog";

const query = signal("");
const refreshing = signal(false);

/** The row's preview line: the last message, or the fact that there is none.
 * The title is not derived here: `../ui/session-title` is its single source
 * (the list rows, the workspace header and the conversation read one name). */
const summaryOf = (row: SessionSummary): string => oneLine(row.lastMessage) || "还没有消息";

/** The one-word state of a row: the active row projects the open conversation
 * (which knows about stopping), every other row projects runStatus. */
const rowStateOf = (row: SessionSummary, active: boolean): RunState => {
  if (active && store.conversation()) return conversationState(store.conversation());
  return sessionState(row.runStatus);
};

/** The single status word a row shows, or none at all.
 *
 * A parked question is not a run: `waiting` gets the badge and never motion
 * (the polish change's C2). The active row stays quiet about its own run — the
 * conversation's header already says what it is doing — but it still reports a
 * wait, and a failure is never hidden. */
const rowBadgeOf = (
  state: RunState,
  waiting: boolean,
  active: boolean,
): { variant: BadgeVariant; label: string; icon?: "clock" | "alert" } | null => {
  if (waiting) return { variant: "warning", label: runStateLabel.waiting, icon: "clock" };
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
 * `ContextMenu` the row itself is the trigger of (the secondary click on a
 * desktop, a held finger on a touch screen). */
function SessionRow(props: { row: () => SessionSummary; active: boolean; wide: boolean; onSelect: () => void }) {
  const row = () => props.row();
  const title = () => sessionTitle(row());
  const preview = () => summaryOf(row());
  const time = () => formatTime(row().lastMessageAt);
  const waiting = () => store.waiting(row().sessionId);
  const badge = () => rowBadgeOf(rowStateOf(row(), props.active), waiting(), props.active);
  const openDelete = () => requestDelete(row().sessionId, title());

  const trigger = (
    <View
      testID={`session-row-${row().sessionId}`}
      role="button"
      focusable
      onPress={props.onSelect}
      accessibilityLabel={`打开会话 ${title()}`}
      accessibilityState={{ selected: props.active }}
      // The selection is the accessibility state itself (`selectedStyle`), so
      // the face and `accessibilityState.selected` can never drift apart. The
      // row is flat like the reference's (`TaskListItem.tsx:570`, no fill): the
      // washes are the only faces, so a selected row is a highlight over the
      // sidebar — never a darker card than its neighbours.
      selectedStyle={{ backgroundColor: color.sidebarAccent }}
      hoverStyle={props.active ? undefined : { backgroundColor: color.hover }}
      focusStyle={focusRing()}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.sm,
        paddingHorizontal: space.md,
        paddingVertical: space.sm,
        borderRadius: radius.lg,
        borderWidth: 1,
        borderColor: color.border,
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
          <If when={time() !== ""}>
            <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground, fontVariant: ["tabular-nums"] }}>
              {time()}
            </Text>
          </If>
        </View>
        <Highlighted
          text={preview()}
          style={{
            numberOfLines: 1,
            textOverflow: "ellipsis",
            fontSize: typeScale.sm,
            color: color.mutedForeground,
          }}
        />
      </View>
      <If when={props.wide}>
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
          <DropdownMenuItem label="删除会话" icon="trash" danger onSelect={openDelete} />
        </DropdownMenu>
      </If>
    </View>
  );

  return (
    <ContextMenu
      accessibilityLabel={`会话操作 ${title()}`}
      trigger={trigger}
      menu={<ContextMenuItem label="删除会话" icon="trash" danger onSelect={openDelete} />}
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

export function SessionList(props: { onSelect: (id: string) => void; activeId: string | null; wide: boolean }) {
  // The store's order, filtered only by the search box (never re-sorted, never
  // re-created): a refresh that changes state must not move a row (C5). The
  // search reads what the rows show — title and preview, never the id — so a
  // match is always visible in the row it keeps.
  const rows = () => {
    const needle = query().trim().toLowerCase();
    const all = store.sessions();
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
        <View
          accessibilityLabel="Keel"
          style={{
            // The reference's titlebar logo is a 12px-rounded tile
            // (`DesktopTopOverlay.tsx:144`, `rounded-lg`); the kit's Avatar
            // face is a pill, so the mark is drawn in the tokens instead.
            width: controlHeight("lg"),
            height: controlHeight("lg"),
            borderRadius: radius.lg,
            alignItems: "center",
            justifyContent: "center",
            backgroundColor: color.muted,
            borderWidth: 1,
            borderColor: color.border,
          }}
        >
          <Icon name="spark" size={iconSize.md} tint={color.brand} />
        </View>
        <View style={{ flexDirection: "column", flex: 1, minWidth: 0 }}>
          <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.sidebarForeground }}>
            Keel
          </Text>
          <Text style={{ fontSize: typeScale["2xs"], color: color.mutedForeground }}>AI 员工会话</Text>
        </View>
        <IconButton icon="plus" label="新建会话" onPress={() => void store.createSession()} />
        <IconButton
          icon="refresh"
          label="刷新会话"
          disabled={refreshing()}
          onPress={() => void refresh()}
        />
        <IconButton icon="settings" label="打开设置" onPress={() => store.openSettings()} />
      </View>
      {/* L1 · the search: the kit's Input, nothing drawn around it. */}
      <View style={{ paddingHorizontal: space.md, paddingVertical: space.md }}>
        <Input
          value={query()}
          onChange={(value) => query.set(value)}
          placeholder="搜索会话"
          accessibilityLabel="搜索会话"
          testID="session-search"
        />
      </View>
      <ScrollArea
        testID="session-list-scroll"
        accessibilityLabel="会话列表"
        style={{ flex: 1, minHeight: 0 }}
      >
        {store.sessionsLoading() ? (
          <SessionListSkeleton />
        ) : store.sessionsError() !== null ? (
          <SessionListError
            message={store.sessionsError() ?? ""}
            retrying={refreshing()}
            onRetry={() => void refresh()}
          />
        ) : rows().length === 0 ? (
          <SessionListEmpty searching={query().trim().length > 0} />
        ) : (
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
        )}
      </ScrollArea>
      {/* L1 · the footer: the connection status Badge, and nothing else. */}
      <Separator />
      <View style={{ flexDirection: "row", alignItems: "center", paddingHorizontal: space.md, paddingVertical: space.sm }}>
        <ConnectionBadge />
      </View>
    </View>
  );
}

/** The panel footer's one readout: what the live channel is really doing. It
 * reports the transport's own state (probing / connected / connecting /
 * reconnecting / offline) — never a mode name, and no sequence number. */
export function ConnectionBadge() {
  const state = () => store.connection();
  const probing = () => store.mode() === "checking";
  const label = () =>
    probing()
      ? "探测中"
      : state().status === "open"
        ? "已连接"
        : state().status === "connecting"
          ? "连接中"
          : state().status === "reconnecting"
            ? "重连中"
            : "未连接";
  const variant = (): BadgeVariant =>
    probing()
      ? "secondary"
      : state().status === "open"
        ? "success"
        : state().status === "connecting" || state().status === "reconnecting"
          ? "warning"
          : "destructive";
  return (
    <Badge
      testID="connection-badge"
      variant={variant()}
      label={label()}
      icon={state().status === "open" ? "wifi" : "wifi-off"}
      accessibilityLabel={`连接状态：${label()}`}
    >
      <If when={state().status === "connecting" || state().status === "reconnecting"}>
        <Spinner
          size={iconSize.xs}
          tint={variant() === "warning" ? color.warning : color.mutedForeground}
          accessibilityLabel="正在连接"
        />
      </If>
    </Badge>
  );
}
