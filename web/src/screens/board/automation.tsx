// BoardAutomation — keel-board-v1 task 15.5 (watchers/schedules half).
//
// Watchers react to events with a deployed action; schedules send on a plan.
// The page observes and toggles them; it never edits an arbitrary runtime URL.
// A schedule whose creator lost the channel's editor permission is shown as
// stopped and can only be canceled — the server refuses a forced resume.
import { Text, View } from "nsh";
import {
  AlertDialog,
  Badge,
  Button,
  ScrollArea,
  Sheet,
  Skeleton,
  Switch,
  color,
  font,
  space,
  typeScale,
  weight,
} from "../../ui/kit";
import {
  boardActorName,
  boardClock,
  boardDateTime,
  boardFailureCopy,
  boardProviderErrorCopy,
  boardProviderErrorLine,
  boardStore,
  boardWatcherActionLabel,
} from "../../data/board";
import type { BoardScheduleView, BoardWatcherView } from "../../data/board-wire";
import { store } from "../../data/store";
import { PageEmpty, PageError, PagePermission, StateNote, isMobile, touchStyle } from "../books/common";
import { scheduleCancelId, watcherSheetId } from "./state";

/** The automation store's status set (board v0.1.1):
 * pending / disabled / done / canceled / dead. Unknown tokens stay verbatim. */
const scheduleStatusLabel = (status: string): string =>
  status === "pending"
    ? "运行中"
    : status === "disabled"
      ? "已暂停"
      : status === "done"
        ? "已完成"
        : status === "canceled"
          ? "已取消"
          : status === "dead"
            ? "已失败"
            : status || "未知";

const scheduleStatusTone = (status: string): "success" | "secondary" | "outline" | "destructive" =>
  status === "pending" ? "success" : status === "dead" ? "destructive" : status === "done" ? "outline" : "secondary";

/** The server records the one true revoked fact: CancelReason `revoked`.
 * Nothing is inferred from last_error text. */
const permissionRevoked = (schedule: BoardScheduleView): boolean => schedule.cancelReason === "revoked";

const deliveryStateLabel = (state: string): string => {
  switch (state) {
    case "done":
      return "已投递";
    case "pending":
      return "待投递";
    case "leased":
      return "投递中";
    case "dead":
      return "死信";
    default:
      return state ? "处理中" : "未知";
  }
};

const cadence = (schedule: BoardScheduleView): string => {
  if (!schedule.everySeconds) return schedule.untilAt ? `至 ${boardClock(schedule.untilAt)}` : "一次性";
  const seconds = schedule.everySeconds;
  if (seconds === 86400) return "每天";
  if (seconds === 604800) return "每周";
  if (seconds === 3600) return "每小时";
  if (seconds % 60 === 0) return `每 ${seconds / 60} 分钟`;
  return `每 ${seconds} 秒`;
};

function WatcherRow(props: { watcher: BoardWatcherView }) {
  const busy = () => boardStore.automationBusy()[props.watcher.id] === true;
  const dead = () => props.watcher.dead ?? 0;
  const stats = () => {
    const parts: string[] = [];
    if (props.watcher.lastDeliveryAt) parts.push(`最近 ${boardClock(props.watcher.lastDeliveryAt)}`);
    if (props.watcher.successes24h !== undefined) {
      parts.push(
        props.watcher.failures24h === undefined
          ? `24h ${props.watcher.successes24h} 成功`
          : `24h ${props.watcher.successes24h} 成功 · ${props.watcher.failures24h} 失败`,
      );
    }
    return parts.join(" · ");
  };
  return (
    <View
      testID={`board-watcher-${props.watcher.id}`}
      style={{
        flexDirection: isMobile() ? "column" : "row",
        alignItems: isMobile() ? "flex-start" : "center",
        gap: space.md,
        paddingVertical: space.md,
        borderBottomWidth: 1,
        borderBottomColor: color.border,
      }}
    >
      <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: 3 }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
          <Text style={{ fontFamily: font.mono, fontSize: typeScale.base, color: color.foreground }}>{props.watcher.id}</Text>
          <Badge variant={props.watcher.enabled ? "success" : "secondary"} label={props.watcher.enabled ? "启用" : "已停用"} />
          <If when={dead() > 0}>
            <Badge variant="destructive" icon="alert" label={`${dead()} 死信`} />
          </If>
          <If when={dead() === 0 && !!props.watcher.lastError}>
            <Badge variant="destructive" icon="alert" label="最近失败" />
          </If>
        </View>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 2 }}>
          {props.watcher.channel} · 动作 {boardWatcherActionLabel(props.watcher.action)} · Bot{" "}
          {props.watcher.bot ? boardActorName(boardStore.cache(), props.watcher.bot) : "未记录"}
        </Text>
        <If when={stats() !== ""}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{stats()}</Text>
        </If>
        <If when={!!props.watcher.lastError}>
          <Text style={{ fontSize: typeScale.sm, color: color.destructive }}>
            最近一次失败：{boardProviderErrorLine(props.watcher.lastError)}
          </Text>
        </If>
      </View>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <Button
          variant="ghost"
          size="sm"
          icon="activity"
          label="查看投递"
          accessibilityLabel={`查看 ${props.watcher.id} 的投递`}
          onPress={() => {
            watcherSheetId.set(props.watcher.id);
            void boardStore.loadWatcherDeliveries(props.watcher.id);
          }}
          style={touchStyle()}
        />
        <If when={dead() > 0}>
          <Button
            testID={`board-watcher-retry-${props.watcher.id}`}
            variant="outline"
            size="sm"
            icon="refresh"
            label="重试死信"
            accessibilityLabel={`重试 ${props.watcher.id} 的死信`}
            loading={busy()}
            onPress={() => void boardStore.retryWatcherDead(props.watcher.id)}
            style={touchStyle()}
          />
        </If>
        <Switch
          testID={`board-watcher-switch-${props.watcher.id}`}
          checked={props.watcher.enabled}
          disabled={busy()}
          accessibilityLabel={`${props.watcher.enabled ? "停用" : "启用"} ${props.watcher.id}`}
          onChange={(checked) => void boardStore.setWatcherEnabled(props.watcher.id, checked)}
          style={isMobile() ? { minHeight: 44 } : undefined}
        />
      </View>
    </View>
  );
}

function ScheduleRow(props: { schedule: BoardScheduleView }) {
  const busy = () => boardStore.automationBusy()[props.schedule.id] === true;
  const revoked = () => permissionRevoked(props.schedule);
  const canSwitch = () => props.schedule.status === "pending" || props.schedule.status === "disabled";
  const canCancel = () => props.schedule.status === "pending" || props.schedule.status === "disabled";
  const canClear = () => props.schedule.status === "dead";
  return (
    <View
      testID={`board-schedule-${props.schedule.id}`}
      style={{
        flexDirection: isMobile() ? "column" : "row",
        alignItems: isMobile() ? "flex-start" : "center",
        gap: space.md,
        paddingVertical: space.md,
        borderBottomWidth: 1,
        borderBottomColor: color.border,
      }}
    >
      <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: 3 }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
          <Text style={{ fontFamily: font.mono, fontSize: typeScale.base, color: color.foreground }}>{props.schedule.id}</Text>
          <Badge
            variant={revoked() ? "destructive" : scheduleStatusTone(props.schedule.status)}
            label={scheduleStatusLabel(props.schedule.status)}
          />
        </View>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 2 }}>
          {props.schedule.channel} · {cadence(props.schedule)} · 下次 {boardDateTime(props.schedule.nextRunAt)} · 已发{" "}
          {props.schedule.sent} 次
        </Text>
        <If when={revoked()}>
          <Text testID={`board-schedule-revoked-${props.schedule.id}`} style={{ fontSize: typeScale.sm, color: color.destructive }}>
            创建者已失去 {props.schedule.channel} 的编辑权限，已自动停发。
          </Text>
        </If>
        <If when={!revoked() && !!props.schedule.lastError}>
          <Text style={{ fontSize: typeScale.sm, color: color.destructive }}>
            最近一次失败：{boardProviderErrorLine(props.schedule.lastError)}
          </Text>
        </If>
      </View>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <If when={canClear()}>
          <Button
            testID={`board-schedule-cancel-${props.schedule.id}`}
            variant="destructive"
            size="sm"
            icon="trash"
            label="清除"
            accessibilityLabel={`清除 ${props.schedule.id}`}
            disabled={busy()}
            onPress={() => scheduleCancelId.set(props.schedule.id)}
            style={touchStyle()}
          />
        </If>
        <If when={canCancel()}>
          <Button
            testID={`board-schedule-cancel-${props.schedule.id}`}
            variant="destructive"
            size="sm"
            icon="x"
            label="取消"
            accessibilityLabel={`取消 ${props.schedule.id}`}
            disabled={busy()}
            onPress={() => scheduleCancelId.set(props.schedule.id)}
            style={touchStyle()}
          />
        </If>
        <Switch
          testID={`board-schedule-switch-${props.schedule.id}`}
          checked={props.schedule.status === "pending"}
          disabled={busy() || revoked() || !canSwitch()}
          accessibilityLabel={`${props.schedule.status === "disabled" ? "继续" : "暂停"} ${props.schedule.id}`}
          onChange={(checked) => void boardStore.setScheduleEnabled(props.schedule.id, checked)}
          style={isMobile() ? { minHeight: 44 } : undefined}
        />
      </View>
    </View>
  );
}

function WatcherSheet() {
  const id = () => watcherSheetId();
  const deliveries = () => (id() ? boardStore.watcherDeliveries()[id()!] ?? [] : []);
  const busy = () => boardStore.watcherDeliveryBusy() === id();
  return (
    <Sheet
      testID="board-watcher-sheet"
      open={id() !== null}
      side="right"
      title={`投递 · ${id() ?? ""}`}
      description="最近投递记录；只显示事件、状态、尝试次数和错误类型。"
      onOpenChange={(event) => !event.open && watcherSheetId.set(null)}
      onClose={() => watcherSheetId.set(null)}
      footer={<Button label="关闭" accessibilityLabel="关闭投递" onPress={() => watcherSheetId.set(null)} style={touchStyle()} />}
    >
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
        <View style={{ flexDirection: "column", gap: space.sm }}>
          <If when={busy() && deliveries().length === 0}>
            <Skeleton width="70%" height={12} accessibilityLabel="加载中" />
          </If>
          <If when={!busy() && deliveries().length === 0}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>还没有投递记录。</Text>
          </If>
          <Each of={deliveries()} key={(delivery) => `${delivery.event}-${delivery.state}`}>
            {(delivery) => (
              <View style={{ flexDirection: "column", gap: 2, paddingVertical: space.sm, borderBottomWidth: 1, borderBottomColor: color.border }}>
                <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.foreground }}>
                  #{delivery().event} · {deliveryStateLabel(delivery().state)}
                </Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  尝试 {delivery().attempts} 次 · 下次 {boardClock(delivery().nextAt)}
                  {delivery().lastError ? ` · ${boardProviderErrorLine(delivery().lastError)}` : ""}
                </Text>
              </View>
            )}
          </Each>
        </View>
      </ScrollArea>
    </Sheet>
  );
}

function CancelScheduleDialog() {
  const id = () => scheduleCancelId();
  const schedule = () => (id() ? boardStore.automation()?.schedules.find((row) => row.id === id()) : undefined);
  return (
    <AlertDialog
      testID="board-schedule-cancel-dialog"
      open={id() !== null}
      title={`取消定时任务 ${id() ?? ""}？`}
      description={
        schedule()?.cancelReason === "revoked"
          ? "创建者已失去频道的编辑权限，服务端已自动停发；这里只清除这条计划，不会恢复发送。"
          : `取消后不再发送；已发消息不会撤回。${schedule()?.lastError ? ` 当前失败：${boardProviderErrorCopy(schedule()?.lastError)}` : ""}`
      }
      confirmLabel={schedule()?.status === "dead" ? "清除" : "取消任务"}
      cancelLabel="保留"
      destructive
      onCancel={() => scheduleCancelId.set(null)}
      onConfirm={() => {
        const target = id();
        if (!target) return;
        void boardStore.cancelSchedule(target).then((ok) => {
          if (ok) scheduleCancelId.set(null);
        });
      }}
    />
  );
}

function AutomationSkeleton() {
  return (
    <View testID="board-automation-skeleton" style={{ flexDirection: "column", gap: space.md }}>
      <Skeleton width={160} height={20} accessibilityLabel="加载中" />
      <Each of={[0, 1, 2]} key={(index) => index}>
        {(index) => (
          <View style={{ flexDirection: "column", gap: space.sm, paddingVertical: space.md, borderBottomWidth: 1, borderBottomColor: color.border }}>
            <Skeleton width="30%" height={14} accessibilityLabel="加载中" />
            <Skeleton width="62%" height={10} accessibilityLabel="加载中" />
          </View>
        )}
      </Each>
    </View>
  );
}

export function BoardAutomation() {
  return (
    <If
      when={!boardStore.canManage()}
      else={<AutomationContent />}
    >
      <View testID="board-automation-permission" style={{ flex: 1, alignItems: "center", justifyContent: "center", paddingTop: space["2xl"] }}>
        <PagePermission capability="管理任务面" capabilityKey="board.manage" />
      </View>
    </If>
  );
}

function AutomationContent() {
  const phase = () => boardStore.automationPhase();
  const data = () => boardStore.automation();
  const error = () => boardStore.automationError();
  const stale = () => boardStore.automationStale();
  return (
    <View testID="board-automation" style={{ flex: 1, minHeight: 0, flexDirection: "column" }}>
      <If when={phase() === "permission" && !data()}>
        <View testID="board-automation-server-permission" style={{ flex: 1, alignItems: "center", justifyContent: "center", paddingTop: space["2xl"] }}>
          <PagePermission capability="管理任务面" capabilityKey="board.manage" />
        </View>
      </If>
      <If when={phase() === "loading" && !data()}>
        <AutomationSkeleton />
      </If>
      <If when={phase() === "error" && !data()}>
        <PageError
          testID="board-automation-error"
          title={boardFailureCopy(error() ?? { kind: "network", message: "读取失败。" }).title}
          message={`${boardFailureCopy(error() ?? { kind: "network", message: "读取失败。" }).description} 已加载的自动化会保留。`}
          requestId={error()?.requestId}
          onRetry={() => void boardStore.loadAutomation()}
        />
      </If>
      <If when={data() !== null}>
        <If when={stale()}>
          <StateNote testID="board-automation-stale" tone="warning" icon="refresh" text="显示的是上次读取的内容；正在重试自动化状态。" />
        </If>
        <If when={phase() === "empty" || (data()!.watchers.length === 0 && data()!.schedules.length === 0)}>
          <PageEmpty
            testID="board-automation-empty"
            icon="clock"
            title="还没有监视器或定时任务"
            description="自动化在部署配置里声明；这里只显示运行状态与启停。"
            primaryLabel="返回队列"
            onPrimary={() => store.openBoard("queue")}
          />
        </If>
        <If when={data()!.watchers.length > 0 || data()!.schedules.length > 0}>
          <ScrollArea testID="board-automation-scroll" style={{ flex: 1, minHeight: 0 }}>
            <View style={{ flexDirection: "column", gap: space.xl, paddingBottom: space.xl }}>
              <View style={{ flexDirection: "column", gap: space.sm }}>
                <Text style={{ fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground }}>监视器</Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  动作在部署时声明，界面只做启停；停用立即生效，可再切换。
                </Text>
                <If when={data()!.watchers.length === 0}>
                  <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>没有监视器。</Text>
                </If>
                <Each of={data()!.watchers} key={(watcher) => watcher.id}>
                  {(watcher) => <WatcherRow watcher={watcher()} />}
                </Each>
              </View>
              <View style={{ flexDirection: "column", gap: space.sm }}>
                <Text style={{ fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground }}>定时任务</Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  失败通常是永久权限或配置问题；只展示原因与处置，不提供强制重试。
                </Text>
                <If when={data()!.schedules.length === 0}>
                  <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>没有定时任务。</Text>
                </If>
                <Each of={data()!.schedules} key={(schedule) => schedule.id}>
                  {(schedule) => <ScheduleRow schedule={schedule()} />}
                </Each>
              </View>
            </View>
          </ScrollArea>
        </If>
      </If>
      <WatcherSheet />
      <CancelScheduleDialog />
    </View>
  );
}
