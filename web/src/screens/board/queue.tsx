// BoardQueue — keel-board-v1 task 15.1.
//
// The one task list: state tabs, the channel/label/owner/for filters, oldest
// first (the server's order, never re-sorted), a row per task with its state,
// needs, owner and lease/outcome. The five states keep the tabs and filters
// visible, so a retry never loses the reader's place.
import { Text, View, keymap } from "nsh";
import {
  Badge,
  Button,
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  ScrollArea,
  Select,
  Sheet,
  Skeleton,
  Tab,
  Tabs,
  color,
  font,
  lineHeight,
  radius,
  space,
  typeScale,
  type BadgeVariant,
} from "../../ui/kit";
import {
  BOARD_STATE_TABS,
  BOARD_STATE_TAB_LABEL,
  activeFilterCount,
  boardActorName,
  boardDeadlineText,
  boardLabelFacets,
  boardLabelFilterTitle,
  boardNeedsText,
  boardQuerySummary,
  boardSearchOf,
  boardStateIcon,
  boardStateLabel,
  boardStateTone,
  boardStore,
  boardVisibleLabels,
  effectiveTaskState,
  emptyQuery,
  visibleQueue,
  type BoardQuery,
  type BoardQueueRow,
  type BoardStateTab,
  type BoardTone,
} from "../../data/board";
import { store } from "../../data/store";
import { copyText, isMobile, PageEmpty, PressRow, touchStyle } from "../books/common";
import { boardDialog, filterSheetOpen, newTaskOpen, queueCursor } from "./state";

const toneVariant = (tone: BoardTone): BadgeVariant =>
  tone === "destructive"
    ? "destructive"
    : tone === "warning"
      ? "warning"
      : tone === "success"
        ? "success"
        : tone === "info"
          ? "info"
          : tone === "outline"
            ? "outline"
            : "secondary";

/** Apply a query patch to both the data layer and the address bar. Opening the
 * archive switches the state tab to all: archived rows are terminal history,
 * so an `open` state would hide the very rows the filter asks for. */
export function applyBoardQuery(patch: Partial<BoardQuery>): void {
  const merged = { ...boardStore.query(), ...patch };
  if (patch.archived === true && patch.state === undefined) merged.state = "all";
  const next = merged;
  void boardStore.setQuery({
    ...patch,
    ...(patch.archived === true && patch.state === undefined ? { state: "all" as BoardStateTab } : {}),
  });
  store.setBoardSearch(boardSearchOf(next));
}

function StateBadge(props: { state: string; testID?: string }) {
  return (
    <Badge
      testID={props.testID}
      variant={toneVariant(boardStateTone(props.state))}
      icon={boardStateIcon(props.state)}
      label={boardStateLabel(props.state)}
    />
  );
}

function StateTabs(props: { query: BoardQuery }) {
  return (
    <ScrollArea testID="board-state-tabs" horizontal style={{ width: "100%" }}>
      <View style={{ flexDirection: "row", paddingBottom: space.xs }}>
        <Tabs variant="line" accessibilityLabel="任务状态" testID="board-state-tablist">
          <Each of={BOARD_STATE_TABS} key={(state) => state}>
            {(state) => (
              <Tab
                testID={`board-state-tab-${state()}`}
                label={BOARD_STATE_TAB_LABEL[state()]}
                selected={props.query.state === state()}
                onPress={() => applyBoardQuery({ state: state() })}
                accessibilityLabel={`${BOARD_STATE_TAB_LABEL[state()]}${props.query.state === state() ? "，当前" : ""}`}
                style={{ minHeight: isMobile() ? 44 : 36, paddingHorizontal: space.md }}
              />
            )}
          </Each>
        </Tabs>
      </View>
    </ScrollArea>
  );
}

function FilterBar(props: { query: BoardQuery; disabled?: boolean }) {
  const channels = () => boardStore.channels();
  const facets = () => boardLabelFacets(boardStore.cache());
  const actors = () => boardStore.actors();
  const channelItems = () => [
    { value: "", title: "全部频道" },
    ...channels().map((channel) => ({ value: channel.path, title: channel.path })),
  ];
  const labelItems = () => {
    const items = [{ value: "", title: "全部标签" }];
    for (const facet of facets()) {
      for (const value of facet.values) {
        items.push({ value: `${facet.key}=${value}`, title: boardLabelFilterTitle(facet.key, value) });
      }
    }
    return items;
  };
  const ownerItems = () => [
    { value: "", title: "全部创建者" },
    ...actors().map((actor) => ({ value: actor.id, title: actor.name || actor.handle || actor.id })),
  ];
  const forItems = () => [
    { value: "all", title: "全部指派对象" },
    { value: "pool", title: "池中（未指派）" },
    ...actors().map((actor) => ({ value: actor.id, title: actor.name || actor.handle || actor.id })),
  ];
  const labelValue = () =>
    props.query.labelKey ? `${props.query.labelKey}${props.query.labelValue ? `=${props.query.labelValue}` : ""}` : "";
  return (
    <View
      testID="board-filter-bar"
      style={{ flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: space.sm }}
    >
      <View style={{ minWidth: isMobile() ? "100%" : 140, flexGrow: 1 }}>
        <Select
          testID="board-filter-channel"
          items={channelItems()}
          value={props.query.channel}
          placeholder="全部频道"
          disabled={props.disabled}
          height={isMobile() ? 44 : undefined}
          accessibilityLabel="按频道筛选"
          onChange={(value) => applyBoardQuery({ channel: value ?? "" })}
        />
      </View>
      <View style={{ minWidth: isMobile() ? "100%" : 120, flexGrow: 1 }}>
        <Select
          testID="board-filter-label"
          items={labelItems()}
          value={labelValue()}
          placeholder="全部标签"
          disabled={props.disabled}
          height={isMobile() ? 44 : undefined}
          accessibilityLabel="按标签筛选"
          onChange={(value) => {
            const raw = value ?? "";
            const cut = raw.indexOf("=");
            applyBoardQuery(
              cut >= 0
                ? { labelKey: raw.slice(0, cut), labelValue: raw.slice(cut + 1) }
                : { labelKey: raw, labelValue: "" },
            );
          }}
        />
      </View>
      <View style={{ minWidth: isMobile() ? "100%" : 120, flexGrow: 1 }}>
        <Select
          testID="board-filter-owner"
          items={ownerItems()}
          value={props.query.owner}
          placeholder="全部创建者"
          disabled={props.disabled}
          height={isMobile() ? 44 : undefined}
          accessibilityLabel="按创建者筛选"
          onChange={(value) => applyBoardQuery({ owner: value ?? "" })}
        />
      </View>
      <View style={{ minWidth: isMobile() ? "100%" : 120, flexGrow: 1 }}>
        <Select
          testID="board-filter-for"
          items={forItems()}
          value={props.query.for}
          placeholder="全部指派对象"
          disabled={props.disabled}
          height={isMobile() ? 44 : undefined}
          accessibilityLabel="按指派筛选"
          onChange={(value) => applyBoardQuery({ for: value ?? "all" })}
        />
      </View>
      <Button
        testID="board-filter-archive"
        variant={props.query.archived ? "default" : "outline"}
        size={isMobile() ? "default" : "sm"}
        icon="layers"
        label="归档"
        selected={props.query.archived}
        disabled={props.disabled}
        accessibilityLabel={props.query.archived ? "只看归档任务，已开启" : "只看归档任务"}
        onPress={() => applyBoardQuery({ archived: !props.query.archived })}
        style={isMobile() ? { width: "100%", minHeight: 44, height: 44 } : touchStyle()}
      />
      <If when={!emptyQuery(props.query) && !isMobile()}>
        <Button
          testID="board-filter-clear"
          variant="ghost"
          size="sm"
          label="清空"
          accessibilityLabel="清空筛选"
          onPress={() =>
            applyBoardQuery({ channel: "", labelKey: "", labelValue: "", owner: "", for: "all", archived: false })
          }
          style={touchStyle()}
        />
      </If>
    </View>
  );
}

function ActiveFilterChips(props: { query: BoardQuery }) {
  const chips = () => {
    const out: Array<{ id: string; label: string; clear: Partial<BoardQuery> }> = [];
    if (props.query.channel) out.push({ id: "channel", label: `频道 ${props.query.channel}`, clear: { channel: "" } });
    if (props.query.labelKey) {
      out.push({
        id: "label",
        label: `标签 ${boardLabelFilterTitle(props.query.labelKey, props.query.labelValue)}`,
        clear: { labelKey: "", labelValue: "" },
      });
    }
    if (props.query.owner) {
      out.push({
        id: "owner",
        label: `创建者 ${boardActorName(boardStore.cache(), props.query.owner)}`,
        clear: { owner: "" },
      });
    }
    if (props.query.for !== "all") {
      out.push({
        id: "for",
        label: props.query.for === "pool" ? "池中" : `指派 ${boardActorName(boardStore.cache(), props.query.for)}`,
        clear: { for: "all" },
      });
    }
    if (props.query.archived) out.push({ id: "archived", label: "只看归档", clear: { archived: false } });
    return out;
  };
  return (
    <If when={chips().length > 0}>
      <View testID="board-filter-chips" style={{ flexDirection: "row", flexWrap: "wrap", gap: space.xs }}>
        <Each of={chips()} key={(chip) => chip.id}>
          {(chip) => (
            <Badge
              testID={`board-filter-chip-${chip().id}`}
              variant="secondary"
              icon="x"
              label={chip().label}
              accessibilityLabel={`移除筛选 ${chip().label}`}
              onPress={() => applyBoardQuery(chip().clear)}
              style={{ minHeight: 32 }}
            />
          )}
        </Each>
      </View>
    </If>
  );
}

function RowMeta(props: { row: BoardQueueRow }) {
  const cache = () => boardStore.cache();
  const labels = () => boardVisibleLabels(props.row.labels).slice(0, 2);
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap", minWidth: 0 }}>
      <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.mutedForeground }}>
        {props.row.channel}
      </Text>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        创建者 {boardActorName(cache(), props.row.task.owner)}
      </Text>
      <If when={props.row.task.for !== null && props.row.task.for !== undefined}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          指派 {boardActorName(cache(), props.row.task.for)}
        </Text>
      </If>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        {boardNeedsText(cache(), props.row.task)}
      </Text>
      <If when={labels().length > 0}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {labels().map((label) => label.text).join(" · ")}
        </Text>
      </If>
    </View>
  );
}

function QueueRow(props: { row: BoardQueueRow; selected: boolean; onPress: () => void }) {
  const now = () => boardStore.clock();
  const state = () => effectiveTaskState(props.row.task, now());
  const firstLine = () => (props.row.message.text.split("\n")[0] || "未命名任务").trim();
  const deadline = () => boardDeadlineText(props.row.task, props.row.message, now(), props.row.last);
  const deadlineInk = () => {
    if (state() !== "claimed") return color.mutedForeground;
    const until = Date.parse(props.row.task.claim?.until ?? "");
    if (!Number.isFinite(until)) return color.mutedForeground;
    const remaining = until - now();
    return remaining <= 0 ? color.destructive : remaining < 60_000 ? color.warning : color.foreground;
  };
  return (
    <PressRow
      testID={`board-row-${props.row.id}`}
      accessibilityLabel={`打开任务 #${props.row.id} ${firstLine()}`}
      selected={props.selected}
      onPress={props.onPress}
      style={{ minHeight: isMobile() ? 92 : 60, paddingVertical: isMobile() ? space.sm : space.xs, gap: space.md }}
    >
      <View style={{ width: isMobile() ? 60 : 64, flexShrink: 0, flexDirection: "column", gap: 2 }}>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.foreground }}>#{props.row.id}</Text>
        <StateBadge state={state()} testID={`board-row-state-${props.row.id}`} />
      </View>
      <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: 4 }}>
        <Text
          style={{
            fontSize: typeScale.base,
            lineHeight: Math.round(typeScale.base * lineHeight.relaxed),
            color: color.foreground,
            numberOfLines: isMobile() ? 2 : 1,
            textOverflow: "ellipsis",
          }}
        >
          {firstLine()}
        </Text>
        <RowMeta row={props.row} />
      </View>
      <View style={{ width: isMobile() ? 92 : 160, flexShrink: 0, alignItems: "flex-end", gap: 2 }}>
        <Text
          style={{
            fontFamily: font.mono,
            fontSize: typeScale.sm,
            color: deadlineInk(),
            numberOfLines: 1,
            textOverflow: "ellipsis",
          }}
        >
          {deadline()}
        </Text>
        <If when={props.row.replies > 0}>
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{props.row.replies} 条回复</Text>
        </If>
        <If when={(props.row.task.result ?? "") !== ""}>
          <Text
            style={{
              fontSize: typeScale.xs,
              color: color.mutedForeground,
              numberOfLines: 1,
              textOverflow: "ellipsis",
              maxWidth: "100%",
            }}
          >
            {props.row.task.result}
          </Text>
        </If>
      </View>
    </PressRow>
  );
}

function QueueSkeleton() {
  const rows = () => (isMobile() ? 3 : 6);
  return (
    <View testID="board-queue-skeleton" accessibilityLabel="加载中" style={{ flexDirection: "column" }}>
      <Each of={Array.from({ length: rows() }, (_, index) => index)} key={(index) => index}>
        {(index) => (
          <View
            testID={`board-skeleton-row-${index()}`}
            style={{
              flexDirection: isMobile() ? "column" : "row",
              alignItems: isMobile() ? "flex-start" : "center",
              gap: space.sm,
              minHeight: isMobile() ? 92 : 60,
              paddingVertical: space.xs,
              borderBottomWidth: 1,
              borderBottomColor: color.border,
            }}
          >
            <View style={{ flexDirection: "row", gap: space.sm, alignItems: "center" }}>
              <Skeleton width={48} height={12} accessibilityLabel="加载中" />
              <Skeleton width={72} height={20} radius={radius.pill} accessibilityLabel="加载中" />
            </View>
            <Skeleton width={isMobile() ? "86%" : "40%"} height={12} accessibilityLabel="加载中" />
            <Skeleton width={isMobile() ? "62%" : "22%"} height={10} accessibilityLabel="加载中" />
            <View style={{ flex: 1 }} />
            <Skeleton width={isMobile() ? 90 : 140} height={10} accessibilityLabel="加载中" />
          </View>
        )}
      </Each>
    </View>
  );
}

function QueueError(props: { failure: NonNullable<ReturnType<typeof boardStore.queueError>> }) {
  return (
    <Empty testID="board-queue-error" accessibilityLabel={props.failure.message}>
      <EmptyMedia icon="alert" tone="danger" />
      <EmptyTitle label="没能读到任务队列" />
      <EmptyDescription
        label={
          boardStore.queueIds().length > 0
            ? `${props.failure.message.replace(/。$/, "")}。已加载的任务保留并标记为过期；筛选没有丢。`
            : `${props.failure.message.replace(/。$/, "")}。没有读到任何任务；筛选仍然保留，可以直接重试。`
        }
      />
      <If when={props.failure.requestId !== undefined}>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.mutedForeground }}>
          请求编号 {props.failure.requestId}
        </Text>
      </If>
      <EmptyContent>
        <Button
          testID="board-queue-retry"
          variant="outline"
          label="重试"
          accessibilityLabel="重试读取任务队列"
          onPress={() => void boardStore.refresh()}
          style={touchStyle()}
        />
      </EmptyContent>
    </Empty>
  );
}

function QueueEmpty(props: {
  /** -1: the channel read did not answer (not proof of no channels). */
  channels: number;
  filtered: boolean;
  archived: boolean;
  query: BoardQuery;
  onNewTask: () => void;
  onClear: () => void;
  onShowAll: () => void;
}) {
  if (props.channels === 0) {
    return (
      <PageEmpty
        testID="board-queue-empty-channels"
        icon="lock"
        title="你还没有可访问的频道"
        description="任务按频道隔离。找频道所有者或租户管理员给你查看或编辑权限。"
        primaryLabel="复制申请说明"
        onPrimary={() => void copyText("请给我一个任务面频道的查看权限。")}
        secondaryLabel="返回工作区"
        onSecondary={() => store.openChapter("workspace")}
      />
    );
  }
  if (props.archived) {
    return (
      <PageEmpty
        testID="board-queue-empty-archived"
        icon="layers"
        title="还没有归档任务"
        description={
          boardStore.archiveUnsupported()
            ? "归档标记暂未上报；这里不会伪造归档记录。"
            : "旧的超级助理工单归档后会出现在这里，只读保留；不会恢复旧看板。"
        }
        primaryLabel="返回队列"
        onPrimary={props.onClear}
      />
    );
  }
  if (props.filtered) {
    const summary = boardQuerySummary(props.query, (id) => boardActorName(boardStore.cache(), id));
    return (
      <PageEmpty
        testID="board-queue-empty-filtered"
        icon="search"
        title="没有符合筛选的任务"
        description={summary ? `当前筛选：${summary}。` : "当前筛选没有匹配到任务。"}
        primaryLabel="清空筛选"
        onPrimary={props.onClear}
        secondaryLabel="新建任务"
        onSecondary={props.onNewTask}
      />
    );
  }
  return (
    <PageEmpty
      testID="board-queue-empty"
      icon="inbox"
      title="队列是空的"
      description="没有打开的任务。新任务会实时出现在这里。"
      primaryLabel="新建任务"
      onPrimary={props.onNewTask}
      secondaryLabel="查看已完成"
      onSecondary={props.onShowAll}
    />
  );
}

export function BoardQueue(props: { onOpenTask: (id: string) => void; onNewTask: () => void }) {
  const query = () => boardStore.query();
  const rows = () => visibleQueue(boardStore.cache(), boardStore.queueIds(), query(), boardStore.clock());
  const loading = () => (boardStore.queueLoading() || boardStore.phase() === "loading") && boardStore.queueIds().length === 0;
  const error = () => boardStore.queueError();
  const stale = () => boardStore.queueStale();
  const cursor = () => {
    const current = queueCursor();
    return rows().length ? Math.min(current, rows().length - 1) : 0;
  };
  const openAt = (index: number) => {
    const row = rows()[index];
    if (row) props.onOpenTask(row.id);
  };
  const move = (delta: number) => {
    if (!rows().length) return;
    const next = (cursor() + delta + rows().length) % rows().length;
    queueCursor.set(next);
  };
  const guard = () => !boardDialog() && !newTaskOpen() && !filterSheetOpen();
  keymap.bind("j", () => guard() && move(1), { when: "notEditing" });
  keymap.bind("k", () => guard() && move(-1), { when: "notEditing" });
  keymap.bind("down", () => guard() && move(1), { when: "notEditing" });
  keymap.bind("up", () => guard() && move(-1), { when: "notEditing" });
  keymap.bind("home", () => guard() && rows().length > 0 && queueCursor.set(0), { when: "notEditing" });
  keymap.bind("end", () => guard() && rows().length > 0 && queueCursor.set(rows().length - 1), { when: "notEditing" });
  keymap.bind("enter", () => guard() && openAt(cursor()), { when: "notEditing" });
  keymap.bind(
    "[",
    () => {
      if (!guard()) return;
      const index = BOARD_STATE_TABS.indexOf(query().state);
      applyBoardQuery({ state: BOARD_STATE_TABS[(index - 1 + BOARD_STATE_TABS.length) % BOARD_STATE_TABS.length] });
    },
    { when: "notEditing" },
  );
  keymap.bind(
    "]",
    () => {
      if (!guard()) return;
      const index = BOARD_STATE_TABS.indexOf(query().state);
      applyBoardQuery({ state: BOARD_STATE_TABS[(index + 1) % BOARD_STATE_TABS.length] });
    },
    { when: "notEditing" },
  );
  keymap.bind("/", () => guard() && filterSheetOpen.set(true), { when: "notEditing" });
  return (
    <View testID="board-queue" style={{ flexDirection: "column", flex: 1, minHeight: 0 }}>
      <StateTabs query={query()} />
      <If
        when={isMobile()}
        else={
          <View style={{ paddingTop: space.md, paddingBottom: space.sm }}>
            <FilterBar query={query()} disabled={loading()} />
          </View>
        }
      >
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, paddingTop: space.sm, paddingBottom: space.sm }}>
          <Button
            testID="board-filter-open"
            variant="outline"
            size="default"
            icon="search"
            label={activeFilterCount(query()) > 0 ? `筛选 · ${activeFilterCount(query())}` : "筛选"}
            accessibilityLabel="打开任务筛选"
            onPress={() => filterSheetOpen.set(true)}
            style={touchStyle()}
          />
        </View>
        <ActiveFilterChips query={query()} />
      </If>
      <Sheet
        testID="board-filter-sheet"
        open={filterSheetOpen()}
        side="bottom"
        title="筛选任务"
        description="按频道、标签、创建者或指派人收窄队列；条件可随时移除。"
        onOpenChange={(event) => !event.open && filterSheetOpen.set(false)}
        onClose={() => filterSheetOpen.set(false)}
        footer={
          <>
            <Button
              testID="board-filter-reset"
              variant="ghost"
              label="重置"
              accessibilityLabel="重置筛选"
              disabled={activeFilterCount(query()) === 0}
              onPress={() =>
                applyBoardQuery({ channel: "", labelKey: "", labelValue: "", owner: "", for: "all", archived: false })
              }
              style={touchStyle()}
            />
            <Text
              testID="board-filter-selected-count"
              style={{ alignSelf: "center", fontSize: typeScale.sm, color: color.mutedForeground }}
            >
              已选 {activeFilterCount(query())} 项
            </Text>
            <Button
              label="完成"
              accessibilityLabel="关闭筛选"
              onPress={() => filterSheetOpen.set(false)}
              style={touchStyle()}
            />
          </>
        }
      >
        <ActiveFilterChips query={query()} />
        <FilterBar query={query()} />
      </Sheet>
      <If when={stale() && error() !== null}>
        <View testID="board-queue-stale" style={{ paddingBottom: space.sm }}>
          <Text style={{ fontSize: typeScale.sm, color: color.warning }}>
            显示的是上次读取的内容；{error()?.message ?? "刷新失败"}
          </Text>
        </View>
      </If>
      <If when={loading()}>
        <QueueSkeleton />
      </If>
      <If when={!loading() && error() !== null && boardStore.queueIds().length === 0}>
        <QueueError failure={error()!} />
      </If>
      <If when={!loading() && !(error() !== null && boardStore.queueIds().length === 0)}>
        <If
          when={rows().length > 0}
          else={
            <QueueEmpty
              channels={boardStore.channelsLoaded() ? boardStore.channels().length : -1}
              filtered={!emptyQuery(query())}
              archived={query().archived}
              query={query()}
              onNewTask={props.onNewTask}
              onClear={() =>
                applyBoardQuery({ channel: "", labelKey: "", labelValue: "", owner: "", for: "all", archived: false })
              }
              onShowAll={() => applyBoardQuery({ state: "all" })}
            />
          }
        >
          <ScrollArea testID="board-queue-scroll" style={{ flex: 1, minHeight: 0 }}>
            <View style={{ flexDirection: "column" }}>
              <Each of={rows()} key={(row) => row.id}>
                {(row) => (
                  <QueueRow
                    row={row()}
                    selected={rows()[cursor()]?.id === row().id}
                    onPress={() => props.onOpenTask(row().id)}
                  />
                )}
              </Each>
            </View>
          </ScrollArea>
        </If>
      </If>
      <If when={rows().length > 0}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, paddingTop: space.md }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            最早优先 · 本页 {rows().length} 条
          </Text>
          <View style={{ flex: 1 }} />
          <If when={boardStore.hasMore()}>
            <Button
              testID="board-load-more"
              variant="outline"
              size="sm"
              label={boardStore.queueMore() ? "加载中…" : "加载更多"}
              loading={boardStore.queueMore()}
              accessibilityLabel="加载更多任务"
              onPress={() => void boardStore.loadMore()}
              style={touchStyle()}
            />
          </If>
        </View>
      </If>
    </View>
  );
}
