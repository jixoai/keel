// The board page — keel-board-v1 tasks 14.1/15.6.
//
// One route, three views: the queue (the default), the task thread, and the
// two management faces. The page owns the five states, the header's live badge,
// the view tabs, the new-task sheet and the retired-address page. Every read
// and write goes through `data/board.ts`; the route is the only navigation
// state (the filters live in its search).
import { effect, keymap, Text, View } from "nsh";
import {
  Badge,
  Button,
  Checkbox,
  Input,
  ScrollArea,
  Select,
  Sheet,
  Textarea,
  color,
  radius,
  space,
  typeScale,
} from "../../ui/kit";
import {
  activeFilterCount,
  boardClock,
  boardFailureCopy,
  boardQueryFromSearch,
  boardSearchOf,
  boardStore,
  type BoardQuery,
} from "../../data/board";
import { can } from "../../data/permissions";
import { store } from "../../data/store";
import {
  PageEmpty,
  PagePermission,
  PageUnavailable,
  SheetHead,
  StateNote,
  copyText,
  isMobile,
  touchStyle,
} from "../books/common";
import { Icon } from "../../ui/icons";
import { BoardQueue, applyBoardQuery } from "./queue";
import { BoardTaskPage, TaskDialogs, bindTaskKeys } from "./task";
import { BoardSyncStatus } from "./sync";
import { BoardAutomation } from "./automation";
import {
  boardDialog,
  closeDialog,
  filterSheetOpen,
  newTaskChannel,
  newTaskClaim,
  newTaskFor,
  newTaskLabelKey,
  newTaskLabelValue,
  newTaskLease,
  newTaskNeeds,
  newTaskOpen,
  newTaskText,
  resetBoardUi,
  shortcutHelpOpen,
  openDialog,
} from "./state";

// ---- route helpers --------------------------------------------------------

const sameQuery = (left: BoardQuery, right: BoardQuery): boolean =>
  left.state === right.state &&
  left.channel === right.channel &&
  left.labelKey === right.labelKey &&
  left.labelValue === right.labelValue &&
  left.owner === right.owner &&
  left.for === right.for &&
  left.archived === right.archived;

// ---- header ---------------------------------------------------------------

function LiveBadge() {
  const status = () => boardStore.liveStatus();
  const label = () =>
    status() === "live"
      ? "实时"
      : status() === "connecting"
        ? "连接中"
        : status() === "reconnecting"
          ? "重连中"
          : status() === "offline"
            ? "离线"
            : "未连接";
  const variant = () =>
    status() === "live" ? "success" : status() === "offline" ? "destructive" : status() === "reconnecting" ? "warning" : "info";
  const icon = () => (status() === "offline" ? "wifi-off" : status() === "live" ? "wifi" : "refresh");
  return <Badge testID="board-live" variant={variant()} icon={icon()} label={label()} />;
}

function LastEventText() {
  const at = () => boardStore.lastEventAt();
  return (
    <Text testID="board-last-event" style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
      {at() ? `最后更新 ${boardClock(at())}` : boardStore.liveStatus() === "live" ? "等待事件" : "显示的是已加载的数据"}
    </Text>
  );
}

function IdentityBadge() {
  return (
    <If when={!boardStore.meLoaded() && boardStore.meFailure() !== null}>
      <Badge testID="board-identity-unknown" variant="warning" icon="alert" label="身份未知" />
    </If>
  );
}

function MovesContractBadge() {
  const missing = () => {
    const ids = boardStore.queueIds();
    if (!ids.length) return false;
    const tasks = boardStore.cache().tasks;
    return ids.every((id) => tasks[id] !== undefined && tasks[id]?.moves === undefined);
  };
  return (
    <If when={missing()}>
      <Badge testID="board-moves-missing" variant="warning" icon="alert" label="动作未开放（缺 moves）" />
    </If>
  );
}

function HealthBadge() {
  const dead = () => {
    const sync = boardStore.sync();
    if (!sync) return 0;
    return sync.dead.length + sync.failures.length;
  };
  const watcherIssues = () => {
    const auto = boardStore.automation();
    if (!auto) return 0;
    return auto.watchers.filter((watcher) => (watcher.dead ?? 0) > 0 || watcher.lastError).length;
  };
  const total = () => dead() + watcherIssues();
  return (
    <If when={total() > 0}>
      <Badge
        testID="board-health"
        variant="warning"
        icon="alert"
        label={`故障 ${total()}`}
        onPress={() => store.openBoard("sync")}
      />
    </If>
  );
}

function ViewTabs(props: { view: string; manage: boolean }) {
  const tab = (id: "queue" | "sync" | "automation", label: string, icon: "inbox" | "refresh" | "clock") => (
    <Button
      testID={`board-view-${id}`}
      variant={props.view === id ? "default" : "ghost"}
      size="sm"
      icon={icon}
      label={label}
      selected={props.view === id}
      accessibilityLabel={`${label}${props.view === id ? "，当前" : ""}`}
      onPress={() => store.openBoard(id, null, id === "queue" ? boardSearchOf(boardStore.query()) : "")}
      style={touchStyle()}
    />
  );
  return (
    <View testID="board-view-tabs" style={{ flexDirection: "row", flexWrap: "wrap", gap: space.xs }}>
      {tab("queue", "队列", "inbox")}
      <If when={props.manage}>
        {tab("sync", "同步", "refresh")}
        {tab("automation", "自动化", "clock")}
      </If>
    </View>
  );
}

function BoardHeader(props: {
  view: "queue" | "sync" | "automation";
  taskId: string | null;
  title: string;
  purpose: string;
  kicker: string;
  onRefresh: () => void;
  refreshing: boolean;
}) {
  const canWrite = () => can("board.write");
  const canManage = () => can("board.manage");
  const filtered = () => activeFilterCount(boardStore.query());
  return (
    <SheetHead
      testID="board-header"
      kicker={{ icon: "inbox", text: props.kicker }}
      title={props.title}
      description={props.purpose}
      badges={
        <>
          <LiveBadge />
          <IdentityBadge />
          <MovesContractBadge />
          <If when={canManage()}>
            <HealthBadge />
          </If>
        </>
      }
      actions={
        <>
          <If when={!isMobile()}>
            <LastEventText />
          </If>
          <Button
            testID="board-refresh"
            variant="ghost"
            size="sm"
            icon="refresh"
            label="刷新"
            loading={props.refreshing}
            accessibilityLabel="刷新任务面"
            onPress={props.onRefresh}
            style={touchStyle()}
          />
          <If when={props.view === "queue" && !props.taskId && canWrite()}>
            <Button
              testID="board-new-task"
              size="sm"
              icon="plus"
              label="新建任务"
              accessibilityLabel="新建任务"
              onPress={() => {
                newTaskChannel.set(boardStore.channels()[0]?.path ?? "");
                newTaskText.set("");
                newTaskLabelKey.set("");
                newTaskLabelValue.set("");
                newTaskFor.set(null);
                newTaskClaim.set(false);
                newTaskLease.set(900);
                newTaskNeeds.set([]);
                newTaskOpen.set(true);
              }}
              style={touchStyle()}
            />
          </If>
          <Button
            testID="board-shortcuts"
            variant="ghost"
            size="sm"
            icon="keyboard"
            label={isMobile() ? undefined : "快捷键"}
            accessibilityLabel="查看快捷键"
            onPress={() => openDialog("shortcuts")}
            style={touchStyle()}
          />
        </>
      }
    >
      <View style={{ flexDirection: "row", flexWrap: "wrap", alignItems: "center", gap: space.sm }}>
        <ViewTabs view={props.view} manage={canManage()} />
        <If when={props.view === "queue" && filtered() > 0}>
          <Badge variant="outline" label={`${filtered()} 个筛选`} />
        </If>
      </View>
    </SheetHead>
  );
}

// ---- notice ---------------------------------------------------------------

function BoardNotice() {
  const notice = () => boardStore.notice();
  return (
    <If when={notice() !== null}>
      <View testID="board-notice" style={{ paddingBottom: space.sm }}>
        <StateNote
          tone={notice()?.tone === "warning" ? "warning" : notice()?.tone === "info" ? "info" : "success"}
          icon={notice()?.tone === "warning" ? "alert" : "check"}
          text={notice()?.text ?? ""}
        />
      </View>
    </If>
  );
}

// ---- the retired address --------------------------------------------------

export function GonePage() {
  return (
    <View testID="board-gone" style={{ flex: 1, minHeight: 0, alignItems: "center", justifyContent: "center", padding: space.xl }}>
      <PageUnavailable
        icon="x"
        title="这个地址已经下线"
        description="旧超级助理看板已删除，任务在「任务队列」。旧工单 id 与任务 id 不是同一套编号，因此这里不做重定向。"
      >
        <View style={{ flexDirection: "row", gap: space.sm, paddingTop: space.md }}>
          <Button
            testID="board-gone-queue"
            label="打开任务队列"
            accessibilityLabel="打开任务队列"
            onPress={() => store.openBoard("queue")}
            style={touchStyle()}
          />
          <Button
            variant="outline"
            label="返回工作区"
            accessibilityLabel="返回工作区"
            onPress={() => store.openChapter("workspace")}
            style={touchStyle()}
          />
        </View>
      </PageUnavailable>
    </View>
  );
}

// ---- new task -------------------------------------------------------------

function NewTaskSheet(props: { onCreated: (id: string) => void }) {
  const channels = () => boardStore.channels().filter((channel) => channel.can?.write !== false);
  const candidates = () =>
    boardStore
      .queueIds()
      .filter((id) => id !== "")
      .map((id) => ({ id, task: boardStore.cache().tasks[id], message: boardStore.cache().messages[id] }))
      .filter((entry) => entry.task && entry.message)
      .slice(0, 40);
  const labelError = () =>
    newTaskLabelKey().trim().startsWith("board.") ? "系统标签（board.*）由任务状态维护，不能手动设置。" : "";
  const valid = () =>
    !!newTaskChannel().trim() &&
    !!newTaskText().trim() &&
    !labelError() &&
    !boardStore.actionInFlight();
  const submit = () => {
    if (!valid()) return;
    const labels = newTaskLabelKey().trim() ? { [newTaskLabelKey().trim()]: newTaskLabelValue().trim() } : {};
    void boardStore
      .createTask({
        channel: newTaskChannel(),
        text: newTaskText().trim(),
        labels,
        needs: newTaskNeeds(),
        for: newTaskFor(),
        claim: newTaskClaim(),
        lease: newTaskLease(),
      })
      .then((id) => {
        if (!id) return;
        newTaskOpen.set(false);
        props.onCreated(id);
      });
  };
  return (
    <Sheet
      testID="board-new-task-sheet"
      open={newTaskOpen()}
      side={isMobile() ? "bottom" : "right"}
      title="新建任务"
      description="任务是一个线程根；频道决定谁能看到它。"
      onOpenChange={(event) => !event.open && newTaskOpen.set(false)}
      onClose={() => newTaskOpen.set(false)}
      footer={
        <>
          <Button variant="ghost" label="取消" accessibilityLabel="取消新建任务" onPress={() => newTaskOpen.set(false)} style={touchStyle()} />
          <Button
            testID="board-new-task-submit"
            label={boardStore.actionInFlight() ? "创建中…" : "创建任务"}
            loading={boardStore.actionInFlight()?.move === "create"}
            disabled={!valid()}
            accessibilityLabel="创建任务"
            onPress={submit}
            style={touchStyle()}
          />
        </>
      }
    >
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
        <View style={{ flexDirection: "column", gap: space.md, paddingBottom: space.md }}>
          <View style={{ flexDirection: "column", gap: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>频道</Text>
            <Select
              testID="board-new-task-channel"
              items={channels().map((channel) => ({ value: channel.path, title: channel.path }))}
              value={newTaskChannel()}
              placeholder="选择频道"
              height={isMobile() ? 44 : undefined}
              accessibilityLabel="任务频道"
              onChange={(value) => newTaskChannel.set(value ?? "")}
            />
          </View>
          <View style={{ flexDirection: "column", gap: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>任务说明</Text>
            <Textarea
              testID="board-new-task-text"
              value={newTaskText()}
              placeholder="一句话说清要做什么、做到什么程度算完成。"
              rows={4}
              maxLength={10000}
              accessibilityLabel="任务说明"
              onChange={(value) => newTaskText.set(value)}
            />
          </View>
          <View style={{ flexDirection: "row", gap: space.sm }}>
            <Input
              testID="board-new-task-label-key"
              value={newTaskLabelKey()}
              placeholder="标签 key（可选）"
              invalid={!!labelError()}
              accessibilityLabel="标签 key"
              onChange={(value) => newTaskLabelKey.set(value)}
              style={{ flex: 1 }}
            />
            <Input
              testID="board-new-task-label-value"
              value={newTaskLabelValue()}
              placeholder="值"
              accessibilityLabel="标签值"
              onChange={(value) => newTaskLabelValue.set(value)}
              style={{ flex: 1 }}
            />
          </View>
          <If when={!!labelError()}>
            <Text testID="board-new-task-label-error" style={{ fontSize: typeScale.sm, color: color.destructive }}>
              {labelError()}
            </Text>
          </If>
          <View style={{ flexDirection: "column", gap: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>指派（可选）</Text>
            <Select
              testID="board-new-task-for"
              items={[
                { value: "", title: "池中（任何有编辑权限的人）" },
                ...boardStore.actors().map((actor) => ({ value: actor.id, title: actor.name || actor.handle || actor.id })),
              ]}
              value={newTaskFor() ?? ""}
              placeholder="池中"
              height={isMobile() ? 44 : undefined}
              accessibilityLabel="指派给"
              onChange={(value) => newTaskFor.set(value ? value : null)}
            />
          </View>
          <Checkbox
            testID="board-new-task-claim"
            checked={newTaskClaim()}
            label="创建后立即认领"
            accessibilityLabel="创建后立即认领"
            onChange={(checked) => newTaskClaim.set(checked)}
            style={{ minHeight: 36 }}
          />
          <If when={newTaskNeeds().length > 0}>
            <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.xs }}>
              <Each of={newTaskNeeds()} key={(id) => id}>
                {(id) => (
                  <Badge
                    variant="outline"
                    label={`#${id}`}
                    onPress={() => newTaskNeeds.update((list) => list.filter((value) => value !== id()))}
                  />
                )}
              </Each>
            </View>
          </If>
          <View style={{ flexDirection: "column", gap: space.xs }}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>前置依赖（可选，最多 20）</Text>
            <ScrollArea maxHeight={160} style={{ borderWidth: 1, borderColor: color.border, borderRadius: radius.md }}>
              <View style={{ flexDirection: "column", padding: space.sm, gap: 2 }}>
                <Each of={candidates().slice(0, 50)} key={(entry) => entry.id}>
                  {(entry) => (
                    <Checkbox
                      testID={`board-new-task-need-${entry().id}`}
                      checked={newTaskNeeds().includes(entry().id)}
                      disabled={!newTaskNeeds().includes(entry().id) && newTaskNeeds().length >= 20}
                      label={`#${entry().id} ${(entry().message?.text.split("\n")[0] ?? "").slice(0, 40)}`}
                      accessibilityLabel={`前置任务 #${entry().id}`}
                      onChange={(checked) =>
                        newTaskNeeds.update((list) =>
                          checked ? [...list.filter((value) => value !== entry().id), entry().id] : list.filter((value) => value !== entry().id),
                        )
                      }
                      style={{ minHeight: 36 }}
                    />
                  )}
                </Each>
              </View>
            </ScrollArea>
          </View>
          <If when={newTaskClaim()}>
            <View style={{ flexDirection: "column", gap: space.xs }}>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>认领租约</Text>
              <Select
                testID="board-new-task-lease"
                items={[
                  { value: "30", title: "30 秒" },
                  { value: "900", title: "15 分钟" },
                  { value: "3600", title: "60 分钟" },
                ]}
                value={String(newTaskLease())}
                height={isMobile() ? 44 : undefined}
                accessibilityLabel="认领租约"
                onChange={(value) => newTaskLease.set(Number(value ?? "900"))}
              />
            </View>
          </If>
          <If when={boardStore.actionError()?.move === "create"}>
            <StateNote
              testID="board-new-task-error"
              tone="danger"
              icon="alert"
              text={boardStore.actionError()?.failure.message ?? "创建失败"}
            />
          </If>
        </View>
      </ScrollArea>
    </Sheet>
  );
}

// ---- the page -------------------------------------------------------------

export function BoardPage() {
  const view = () => store.routeBoardView();
  const taskId = () => store.routeObjectId();
  let lastView: string | undefined;
  let lastTaskId: string | null | undefined;
  let routeToken = 0;

  effect(() => {
    const route = store.route();
    if (route !== "board") {
      lastView = undefined;
      lastTaskId = undefined;
      boardStore.closeThread();
      boardStore.stopLive();
      return;
    }
    const nextView = store.routeBoardView();
    const nextTask = store.routeObjectId();
    const search = store.routeSearch();
    const reset = nextView !== lastView || (nextTask ?? null) !== (lastTaskId ?? null);
    lastView = nextView;
    lastTaskId = nextTask;
    if (reset) resetBoardUi();
    const parsed = boardQueryFromSearch(search);
    const token = ++routeToken;
    void boardStore.open().then(() => {
      // A slow first boot must not overwrite a filter the reader set while it
      // was in flight.
      if (token !== routeToken) return;
      if (!sameQuery(parsed, boardStore.query())) void boardStore.setQuery(parsed);
      boardStore.resumeLive();
    });
    if (nextView === "queue" && nextTask) void boardStore.openThread(nextTask);
    else boardStore.closeThread();
    if (nextView === "sync") void boardStore.loadSync();
    if (nextView === "automation") void boardStore.loadAutomation();
  });

  keymap.bind(
    "shift+/",
    () => {
      if (boardDialog() || newTaskOpen()) return;
      openDialog("shortcuts");
    },
    { when: "notEditing" },
  );
  keymap.bind(
    "escape",
    () => {
      if (boardDialog()) return;
      if (newTaskOpen()) {
        newTaskOpen.set(false);
        return;
      }
      if (filterSheetOpen()) {
        filterSheetOpen.set(false);
        return;
      }
      if (view() !== "queue") store.openBoard("queue", null, boardSearchOf(boardStore.query()));
    },
    { when: "notEditing" },
  );
  // One binding set for the whole page; it reads the current route's task id
  // each time, so entering/leaving a task never duplicates or loses a key.
  bindTaskKeys(() => store.routeObjectId(), () => store.closeBoardTask(), (id) => store.openBoardTask(id));

  // The gates are an <If> chain, not a body-level return: NSH runs a component
  // body once, so a permission/phase read made in a body-level `if` would
  // freeze the first (often unknown) answer. The content below is a component
  // of its own with the same rule.
  return (
    <If when={!can("board.read")} else={<BoardPhaseGate />}>
      <DeniedGate />
    </If>
  );
}

function DeniedGate() {
  return (
    <If when={can("task.read")} else={<NotFoundPage />}>
      <MigrationPage />
    </If>
  );
}

function BoardPhaseGate() {
  return (
    <If
      when={boardStore.phase() === "disabled"}
      else={
        <If
          when={boardStore.phase() === "permission"}
          else={
            <If when={boardStore.phase() === "not_found"} else={<BoardContent />}>
              <NotFoundPage />
            </If>
          }
        >
          <View style={{ flex: 1, minHeight: 0, alignItems: "center", justifyContent: "center", padding: space.xl }}>
            <PagePermission capability="查看任务面" capabilityKey="board.read" testID="board-permission" />
          </View>
        </If>
      }
    >
      <DisabledPage />
    </If>
  );
}

function BoardContent() {
  const view = () => store.routeBoardView();
  const taskId = () => store.routeObjectId();
  const refresh = () => {
    if (view() === "sync") void boardStore.loadSync();
    else if (view() === "automation") void boardStore.loadAutomation();
    else if (taskId()) void boardStore.refreshThread();
    else void boardStore.refresh();
  };
  const refreshing = () =>
    boardStore.queueLoading() || boardStore.syncPhase() === "loading" || boardStore.automationPhase() === "loading";
  const title = () =>
    view() === "sync" ? "同步" : view() === "automation" ? "自动化" : taskId() ? `任务 #${taskId()}` : "任务队列";
  const purpose = () =>
    view() === "sync"
      ? "外部平台的线程与任务双向镜像。这里只看健康与故障处置；凭据与路由由部署配置管理。"
      : view() === "automation"
        ? "监视器按事件触发已声明的动作；定时任务按计划发消息。失败不阻塞任务主路径。"
        : taskId()
          ? "任务卡与线程：动作在卡里，工作日志在线程里。"
          : "最早优先的队列：谁可以接、谁在干、卡在哪里。实时更新，不需要刷新页面。";
  const kicker = () =>
    view() === "sync"
      ? "02 / 任务 · 同步"
      : view() === "automation"
        ? "02 / 任务 · 自动化"
        : taskId()
          ? "02 / 任务 · 任务"
          : "02 / 任务 · 队列";
  return (
    <View testID="board-page" style={{ flex: 1, minHeight: 0, flexDirection: "column" }}>
      <View
        style={{
          width: "100%",
          maxWidth: 1180,
          alignSelf: "center",
          flex: 1,
          minHeight: 0,
          paddingHorizontal: isMobile() ? space.lg : space["2xl"],
          paddingTop: space.lg,
          paddingBottom: isMobile() ? space["2xl"] : space["3xl"],
          flexDirection: "column",
          gap: space.md,
        }}
      >
        <BoardHeader
          view={view()}
          taskId={taskId()}
          title={title()}
          purpose={purpose()}
          kicker={kicker()}
          onRefresh={refresh}
          refreshing={refreshing()}
        />
        <BoardNotice />
        <If when={view() === "queue" && taskId() !== null}>
          <BoardTaskPage
            taskId={taskId()!}
            onBack={() => store.closeBoardTask()}
            onOpenTask={(id) => store.openBoardTask(id)}
          />
        </If>
        <If when={view() === "queue" && taskId() === null}>
          <BoardQueue onOpenTask={(id) => store.openBoardTask(id)} onNewTask={() => newTaskOpen.set(true)} />
        </If>
        <If when={view() === "sync"}>
          <BoardSyncStatus />
        </If>
        <If when={view() === "automation"}>
          <BoardAutomation />
        </If>
        <NewTaskSheet onCreated={(id) => store.openBoardTask(id)} />
        <TaskDialogs taskId={taskId() ?? ""} />
      </View>
    </View>
  );
}

function MigrationPage() {
  return (
    <View testID="board-migrate" style={{ flex: 1, minHeight: 0, alignItems: "center", justifyContent: "center", padding: space.xl }}>
      <PageEmpty
        icon="spark"
        title="任务面已升级，还差一步"
        description="旧 task.read 仍在、新 board.read 未生效。管理员启用 board 时会给默认角色加上 board.read；归档记录只读保留。"
        primaryLabel="复制申请说明"
        onPrimary={() => void copyText("我需要「查看任务面」（board.read）能力；请把我的账号加入有该能力的权限组。")}
        secondaryLabel="返回工作区"
        onSecondary={() => store.openChapter("workspace")}
      />
    </View>
  );
}

function NotFoundPage() {
  return (
    <View testID="board-not-found" style={{ flex: 1, minHeight: 0, alignItems: "center", justifyContent: "center", padding: space.xl }}>
      <PageUnavailable
        icon="lock"
        title="任务面不存在，或你没有访问权限"
        description="不区分不存在与无权限；它可能已被删除，或你的频道权限已变化。"
      >
        <View style={{ flexDirection: "row", gap: space.sm, paddingTop: space.md }}>
          <Button label="返回工作区" accessibilityLabel="返回工作区" onPress={() => store.openChapter("workspace")} style={touchStyle()} />
          <Button
            variant="outline"
            label="复制说明"
            accessibilityLabel="复制说明"
            onPress={() => void copyText("我打不开任务面：需要确认这个租户是否已启用任务看板，以及我的账号是否有 board.read。")}
            style={touchStyle()}
          />
        </View>
      </PageUnavailable>
    </View>
  );
}

function DisabledPage() {
  return (
    <View testID="board-disabled" style={{ flex: 1, minHeight: 0, alignItems: "center", justifyContent: "center", padding: space.xl }}>
      <PageUnavailable
        icon="layers"
        title="任务看板未启用"
        description="这个租户还没有开启任务看板。找租户管理员开启后再试；不是权限问题。"
      >
        <View style={{ flexDirection: "row", gap: space.sm, paddingTop: space.md }}>
          <Button label="返回工作区" accessibilityLabel="返回工作区" onPress={() => store.openChapter("workspace")} style={touchStyle()} />
        </View>
      </PageUnavailable>
    </View>
  );
}
