// BoardSyncStatus — keel-board-v1 task 15.5 (sync half).
//
// The health face for external mirrors: entries, their inbox/outbox depth,
// credential state, targets, dead letters and outbox failures. Read-only facts
// except retry (idempotent); credentials and routes are deployment config, so
// the page never offers an OAuth wizard or an arbitrary URL.
import { Text, View } from "nsh";
import {
  Badge,
  Button,
  Card,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
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
  boardClock,
  boardEventRefLabel,
  boardFailureCopy,
  boardPlatformLabel,
  boardProviderErrorCopy,
  boardProviderErrorLine,
  boardStore,
  boardThreadRefLabel,
  type BoardSyncData,
} from "../../data/board";
import type {
  BoardSyncEntryView,
  BoardSyncFailure,
  BoardSyncInboxItem,
  BoardSyncLag,
  BoardSyncTargetView,
} from "../../data/board-wire";
import { store } from "../../data/store";
import { PageEmpty, PageError, PagePermission, StateNote, isMobile, touchStyle } from "../books/common";
import { syncInboxOpen } from "./state";

function lagAgeText(seconds: number | undefined): string {
  if (seconds === undefined) return "未知";
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分钟`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时`;
  return `${Math.floor(hours / 24)} 天`;
}

/** A measured lag object is real data; null/undefined is unknown. An object
 * with both queues null means the queues are empty, not that lag is zero. */
function lagLine(lag: BoardSyncLag | null | undefined): { text: string; tone?: string } {
  if (lag === undefined || lag === null) return { text: "滞后未知" };
  const parts: string[] = [];
  let worst = 0;
  for (const [label, queue] of [["入站", lag.inbox], ["出站", lag.outbox]] as const) {
    if (!queue) continue;
    worst = Math.max(worst, queue.ageSeconds ?? 0);
    parts.push(`${label} ${queue.outstanding} 待处理 · 最老 ${lagAgeText(queue.ageSeconds)}`);
  }
  if (parts.length === 0) return { text: "无积压" };
  return { text: parts.join(" · "), tone: worst > 300 ? color.warning : undefined };
}

function targetStatus(status: string): {
  label: string;
  variant: "success" | "warning" | "destructive" | "secondary";
} {
  switch (status) {
    case "synced":
      return { label: "已同步", variant: "success" };
    case "pending":
      return { label: "待投递", variant: "warning" };
    case "failed":
      return { label: "失败", variant: "destructive" };
    default:
      return { label: status || "未知", variant: "secondary" };
  }
}

function TargetRow(props: { target: BoardSyncTargetView }) {
  const status = () => targetStatus(props.target.status);
  const details = () => {
    const parts: string[] = [];
    if ((props.target.dead ?? 0) > 0) parts.push(`${props.target.dead} 死信`);
    if ((props.target.undelivered ?? 0) > 0) parts.push(`${props.target.undelivered} 待投递`);
    if (props.target.attempts) parts.push(`${props.target.attempts} 次尝试`);
    if (props.target.lastError) parts.push(boardProviderErrorCopy(props.target.lastError));
    return parts.join(" · ");
  };
  return (
    <View style={{ flexDirection: "column", gap: 2 }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, minWidth: 0 }}>
        <Text
          style={{
            flex: 1,
            minWidth: 0,
            fontSize: typeScale.sm,
            color: color.mutedForeground,
            numberOfLines: 1,
            textOverflow: "ellipsis",
          }}
        >
          {boardThreadRefLabel(props.target.target)}
        </Text>
        <If when={props.target.root !== undefined}>
          <Button
            variant="link"
            size="sm"
            label={`#${props.target.root}`}
            accessibilityLabel={`打开任务 #${props.target.root}`}
            onPress={() => store.openBoardTask(props.target.root!)}
            style={isMobile() ? { minHeight: 44 } : undefined}
          />
        </If>
        <Badge variant={status().variant} label={status().label} />
      </View>
      <If when={details() !== ""}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{details()}</Text>
      </If>
    </View>
  );
}

function credentialBadge(entry: BoardSyncEntryView, data: BoardSyncData) {
  const credential = data.credentials[entry.key]?.credential ?? entry.credential ?? "unknown";
  if (credential === "invalid") return <Badge variant="destructive" icon="alert" label="凭据失效" />;
  if (credential === "missing") return <Badge variant="destructive" icon="alert" label="凭据缺失" />;
  if (credential === "valid") return <Badge variant="success" icon="check" label="凭据有效" />;
  return <Badge variant="outline" icon="info" label={data.credentialsKnown ? "凭据未知" : "凭据状态未知"} />;
}

function Metric(props: { label: string; value: string; tone?: string; testID?: string }) {
  return (
    <View style={{ flexDirection: "column", gap: 2, minWidth: 96 }}>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{props.label}</Text>
      <Text
        testID={props.testID}
        style={{ fontFamily: font.mono, fontSize: typeScale.base, color: props.tone ?? color.foreground }}
      >
        {props.value}
      </Text>
    </View>
  );
}

function HealthSummary(props: { data: BoardSyncData }) {
  const entries = () => (props.data.status.keys.length ? props.data.status.keys : props.data.entries);
  const active = () => entries().filter((entry) => entry.enabled).length;
  const credentialOf = (entry: BoardSyncEntryView) =>
    props.data.credentials[entry.key]?.credential ?? entry.credential;
  const invalid = () => entries().filter((entry) => credentialOf(entry) === "invalid").length;
  const missing = () => entries().filter((entry) => credentialOf(entry) === "missing").length;
  const credentialIssues = () => invalid() + missing();
  const deadLetters = () => props.data.dead.length;
  const outboxFailures = () => props.data.failures.length;
  const last = () => {
    const times = entries()
      .map((entry) => Date.parse(entry.lastDeliveryAt ?? ""))
      .filter((value) => Number.isFinite(value));
    return times.length ? new Date(Math.max(...times)).toISOString() : undefined;
  };
  const summary = () => {
    if (credentialIssues() > 0 && deadLetters() + outboxFailures() === 0) {
      return "凭据问题会让对应入口停止收发；轮换后自动恢复。";
    }
    if (credentialIssues() + deadLetters() + outboxFailures() === 0) {
      return `暂无已知故障${last() ? ` · 最后投递 ${boardClock(last())}` : ""}`;
    }
    return "故障只阻塞对应外部线程，任务主路径继续。";
  };
  return (
    <View testID="board-sync-health" style={{ flexDirection: "row", flexWrap: "wrap", alignItems: "center", gap: space.sm }}>
      <Badge variant="secondary" label={`${entries().length} 个入口`} />
      <Badge variant={active() === entries().length ? "success" : "outline"} label={`${active()} 个活跃`} />
      <If when={invalid() > 0}>
        <Badge variant="destructive" icon="alert" label={`${invalid()} 个凭据失效`} />
      </If>
      <If when={missing() > 0}>
        <Badge variant="destructive" icon="alert" label={`${missing()} 个凭据缺失`} />
      </If>
      <If when={deadLetters() > 0}>
        <Badge variant="destructive" icon="alert" label={`${deadLetters()} 条死信`} />
      </If>
      <If when={outboxFailures() > 0}>
        <Badge variant="destructive" icon="alert" label={`${outboxFailures()} 个出站失败`} />
      </If>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{summary()}</Text>
    </View>
  );
}

function EntryCard(props: { entry: BoardSyncEntryView; data: BoardSyncData }) {
  const dead = () => props.data.dead.filter((item) => item.key === props.entry.key);
  const failures = () => props.data.failures.filter((item) => !item.key || item.key === props.entry.key);
  const targetsKnown = () => props.entry.targets !== undefined;
  const targets = () => props.entry.targets ?? [];
  const inbox = () => props.entry.inbox ?? { pending: 0, leased: 0, done: 0, dead: 0 };
  const outbox = () => props.entry.outbox ?? props.data.status.outbox;
  const missingStats = () => props.entry.failures24h === undefined;
  const lag = () => lagLine(props.entry.lag);
  const backendGap = () => {
    if (!targetsKnown() && missingStats()) return "外部目标与 24h 统计暂未上报，页面不伪造。";
    if (!targetsKnown()) return "外部目标暂未上报，页面不伪造。";
    if (missingStats()) return "24h 统计暂未上报，页面不伪造。";
    return "";
  };
  const retrying = (id: string) => boardStore.syncRetrying()[id] === true;
  return (
    <Card
      testID={`board-sync-entry-${props.entry.key}`}
      style={{ padding: space.lg, flexDirection: "column", gap: space.sm, backgroundColor: color.card }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: space.sm }}>
        <Badge variant="outline" label={boardPlatformLabel(props.entry.platform)} />
        <If when={props.entry.key !== props.entry.platform}>
          <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.foreground }}>{props.entry.key}</Text>
        </If>
        <Badge variant={props.entry.enabled ? "success" : "secondary"} label={props.entry.enabled ? "启用" : "已停用"} />
        {credentialBadge(props.entry, props.data)}
        <If when={props.entry.owned}>
          <Badge variant="info" label="本实例连接" />
        </If>
        <View style={{ flex: 1 }} />
        <If when={!!props.entry.lastDeliveryAt}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            最后投递 {boardClock(props.entry.lastDeliveryAt)}
          </Text>
        </If>
      </View>
      <If when={(props.data.credentials[props.entry.key]?.credential ?? props.entry.credential) === "invalid"}>
        <StateNote
          testID={`board-sync-credential-${props.entry.key}`}
          tone="danger"
          icon="alert"
          text="凭据失效，已停止收发。轮换凭据后会自动恢复；在轮换完成前不会重试投递。"
        />
      </If>
      <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.lg }}>
        <Metric
          label="入站"
          value={`${inbox().pending} 待处理`}
          testID={`board-sync-inbox-${props.entry.key}`}
        />
        <Metric
          label="出站"
          value={`${outbox().pending} 待投递`}
          testID={`board-sync-outbox-${props.entry.key}`}
        />
        <Metric
          label="滞后"
          value={lag().text}
          tone={lag().tone ?? (props.entry.lag === undefined || props.entry.lag === null ? color.mutedForeground : undefined)}
          testID={`board-sync-lag-${props.entry.key}`}
        />
        <If when={props.entry.failures24h !== undefined}>
          <Metric
            label="24h 失败"
            value={`${props.entry.failures24h} 次`}
            tone={(props.entry.failures24h ?? 0) > 0 ? color.destructive : undefined}
            testID={`board-sync-failures24h-${props.entry.key}`}
          />
        </If>
        <Metric label="死信" value={`${dead().length} 条`} tone={dead().length > 0 ? color.destructive : undefined} />
      </View>
      <If when={backendGap() !== ""}>
        <Text
          testID={`board-sync-backend-gap-${props.entry.key}`}
          style={{ fontSize: typeScale.sm, color: color.mutedForeground }}
        >
          {backendGap()}
        </Text>
      </If>
      <If when={(props.entry.routes ?? []).length > 0}>
        <View style={{ flexDirection: "column", gap: 2 }}>
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>路由</Text>
          <Each of={props.entry.routes ?? []} key={(route) => `${route.from}-${route.to}`}>
            {(route) => (
              <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.mutedForeground }}>
                {route().from}
                {route().to ? ` → ${route().to}` : ""}
              </Text>
            )}
          </Each>
        </View>
      </If>
      <If when={targetsKnown() && targets().length === 0}>
        <Text testID={`board-sync-targets-empty-${props.entry.key}`} style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          还没有外部目标；发起同步后，这里会列出对应的 board 线程。
        </Text>
      </If>
      <If when={targets().length > 0}>
        <View style={{ flexDirection: "column", gap: space.xs }}>
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>目标</Text>
          <Each of={targets().slice(0, 3)} key={(target) => `${target.target}-${target.root ?? ""}`}>
            {(target) => <TargetRow target={target()} />}
          </Each>
          <If when={targets().length > 3}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>还有 {targets().length - 3} 个目标</Text>
          </If>
        </View>
      </If>
      <If when={(props.entry.bridgeHost ?? "") !== ""}>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.mutedForeground }}>
          命令桥 {props.entry.bridgeHost}
          {props.entry.bridgeErrors ? ` · 校验失败 ${props.entry.bridgeErrors}` : ""}
        </Text>
      </If>
      <If when={props.entry.platform === "slack"}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          Slack 偶发重复消息：极端崩溃窗口可能重复一条，其他平台不受影响。
        </Text>
      </If>
      <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.sm }}>
        <If when={dead().length > 0}>
          <Button
            testID={`board-sync-retry-${props.entry.key}`}
            variant="outline"
            size="sm"
            icon="refresh"
            label="重试死信"
            accessibilityLabel={`重试 ${props.entry.key} 的死信`}
            onPress={() => void boardStore.retryAllDead()}
            style={touchStyle()}
          />
        </If>
        <If when={failures().length > 0}>
          <Button
            testID={`board-sync-retry-outbox-${props.entry.key}`}
            variant="outline"
            size="sm"
            icon="refresh"
            label="重试出站"
            accessibilityLabel={`重试 ${props.entry.key} 的出站失败`}
            loading={failures().some((failure) => retrying(failure.delivery))}
            onPress={() => {
              for (const failure of failures()) void boardStore.retryOutbox(failure.delivery);
            }}
            style={touchStyle()}
          />
        </If>
        <Button
          testID={`board-sync-inbox-open-${props.entry.key}`}
          variant="ghost"
          size="sm"
          icon="inbox"
          label="查看收件箱"
          accessibilityLabel={`查看 ${props.entry.key} 的收件箱`}
          onPress={() => {
            boardStore.syncInboxKey.set(props.entry.key);
            syncInboxOpen.set(props.entry.key);
          }}
          style={touchStyle()}
        />
      </View>
    </Card>
  );
}

function DeadLetters(props: { data: BoardSyncData }) {
  const dead = () => props.data.dead;
  const retrying = (id: string) => boardStore.syncRetrying()[id] === true;
  return (
    <View testID="board-sync-dead" style={{ flexDirection: "column", gap: space.sm }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
        <Text style={{ fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground }}>
          死信 · {dead().length}
        </Text>
        <View style={{ flex: 1 }} />
        <If when={dead().length > 0}>
          <Button
            testID="board-sync-retry-all"
            variant="outline"
            size="sm"
            icon="refresh"
            label="重试全部"
            accessibilityLabel="重试全部死信"
            onPress={() => void boardStore.retryAllDead()}
            style={touchStyle()}
          />
        </If>
      </View>
      <If when={dead().length === 0}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          没有死信 · 所有入站事件都已处理或正在重试
        </Text>
      </If>
      <If when={dead().length > 0}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          死信只阻塞该外部线程；其他线程和任务主路径继续。
        </Text>
        <Each of={dead()} key={(item) => item.id}>
          {(item) => (
            <View
              testID={`board-dead-${item().id}`}
              style={{
                flexDirection: isMobile() ? "column" : "row",
                alignItems: isMobile() ? "flex-start" : "center",
                gap: space.sm,
                paddingVertical: space.sm,
                borderBottomWidth: 1,
                borderBottomColor: color.border,
              }}
            >
              <Text style={{ fontSize: typeScale.sm, color: color.foreground, minWidth: 140 }}>
                {boardEventRefLabel(item().dedupe ?? item().id)}
              </Text>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, flex: 1, minWidth: 0 }}>
                {boardProviderErrorLine(item().lastError)}
              </Text>
              <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.mutedForeground }}>
                {item().attempts} 次
              </Text>
              <Button
                testID={`board-dead-retry-${item().id}`}
                variant="outline"
                size="sm"
                label={retrying(item().id) ? "重新排队中…" : "重试"}
                loading={retrying(item().id)}
                accessibilityLabel={`重试死信 ${boardEventRefLabel(item().dedupe ?? item().id)}`}
                onPress={() => void boardStore.retryInbox(item().id, item().key)}
                style={touchStyle()}
              />
            </View>
          )}
        </Each>
      </If>
    </View>
  );
}

function Failures(props: { failures: BoardSyncFailure[] }) {
  const retrying = (id: string) => boardStore.syncRetrying()[id] === true;
  return (
    <If when={props.failures.length > 0}>
      <View testID="board-sync-failures" style={{ flexDirection: "column", gap: space.sm }}>
        <Text style={{ fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground }}>
          出站失败 · {props.failures.length}
        </Text>
        <Each of={props.failures} key={(failure) => failure.delivery}>
          {(failure) => (
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, paddingVertical: space.sm, borderBottomWidth: 1, borderBottomColor: color.border }}>
              <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.foreground, minWidth: 120 }}>
                #{failure().message}
              </Text>
              <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.sm, color: color.mutedForeground }}>
                {boardThreadRefLabel(failure().target)}
              </Text>
              <Text style={{ fontSize: typeScale.sm, color: color.destructive }}>{boardProviderErrorLine(failure().error)}</Text>
              <Button
                variant="outline"
                size="sm"
                label={retrying(failure().delivery) ? "重新排队中…" : "重试"}
                loading={retrying(failure().delivery)}
                accessibilityLabel={`重试出站失败 ${failure().message}`}
                onPress={() => void boardStore.retryOutbox(failure().delivery)}
                style={touchStyle()}
              />
            </View>
          )}
        </Each>
      </View>
    </If>
  );
}

function InboxSheet(props: { data: BoardSyncData }) {
  const key = () => syncInboxOpen();
  const items = () => props.data.dead.filter((item) => !key() || item.key === key());
  return (
    <Sheet
      testID="board-sync-inbox-sheet"
      open={key() !== null}
      side="right"
      title={`收件箱 · ${boardPlatformLabel(key() ?? "")}`}
      description="只显示事件、尝试次数和错误类型；不显示正文或凭据。"
      onOpenChange={(event) => {
        if (event.open) return;
        syncInboxOpen.set(null);
        boardStore.syncInboxKey.set("");
      }}
      onClose={() => {
        syncInboxOpen.set(null);
        boardStore.syncInboxKey.set("");
      }}
      footer={
        <Button
          label="关闭"
          accessibilityLabel="关闭收件箱"
          onPress={() => {
            syncInboxOpen.set(null);
            boardStore.syncInboxKey.set("");
          }}
          style={touchStyle()}
        />
      }
    >
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
        <View style={{ flexDirection: "column", gap: space.sm }}>
          <If when={items().length === 0}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>这个入口没有死信。</Text>
          </If>
          <Each of={items()} key={(item) => item.id}>
            {(item) => (
              <View style={{ flexDirection: "column", gap: 2, paddingVertical: space.sm, borderBottomWidth: 1, borderBottomColor: color.border }}>
                <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>{boardEventRefLabel(item().dedupe ?? item().id)}</Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  死信 · {item().attempts} 次 · {boardProviderErrorLine(item().lastError)}
                </Text>
              </View>
            )}
          </Each>
        </View>
      </ScrollArea>
    </Sheet>
  );
}

function SyncSkeleton() {
  return (
    <View testID="board-sync-skeleton" style={{ flexDirection: "column", gap: space.md }}>
      <View style={{ flexDirection: "row", gap: space.md }}>
        <Skeleton width={120} height={20} accessibilityLabel="加载中" />
        <Skeleton width={90} height={20} accessibilityLabel="加载中" />
        <Skeleton width={110} height={20} accessibilityLabel="加载中" />
      </View>
      <Each of={[0, 1]} key={(index) => index}>
        {(index) => (
          <Card style={{ padding: space.lg, flexDirection: "column", gap: space.md }}>
            <Skeleton width="32%" height={16} accessibilityLabel="加载中" />
            <Skeleton width="62%" height={10} accessibilityLabel="加载中" />
            <Skeleton width="48%" height={10} accessibilityLabel="加载中" />
          </Card>
        )}
      </Each>
    </View>
  );
}

export function BoardSyncStatus() {
  return (
    <If
      when={!boardStore.canManage()}
      else={<SyncContent />}
    >
      <View testID="board-sync-permission" style={{ flex: 1, alignItems: "center", justifyContent: "center", paddingTop: space["2xl"] }}>
        <PagePermission capability="管理任务面" capabilityKey="board.manage" />
      </View>
    </If>
  );
}

function SyncContent() {
  const phase = () => boardStore.syncPhase();
  const data = () => boardStore.sync();
  const error = () => boardStore.syncError();
  const stale = () => boardStore.syncStale();
  return (
    <View testID="board-sync" style={{ flex: 1, minHeight: 0, flexDirection: "column" }}>
      <If when={phase() === "permission" && !data()}>
        <View testID="board-sync-server-permission" style={{ flex: 1, alignItems: "center", justifyContent: "center", paddingTop: space["2xl"] }}>
          <PagePermission capability="管理任务面" capabilityKey="board.manage" />
        </View>
      </If>
      <If when={phase() === "loading" && !data()}>
        <SyncSkeleton />
      </If>
      <If when={phase() === "error" && !data()}>
        <PageError
          testID="board-sync-error"
          title={boardFailureCopy(error() ?? { kind: "network", message: "读取失败。" }).title}
          message={`${boardFailureCopy(error() ?? { kind: "network", message: "读取失败。" }).description} 已加载的入口会保留。`}
          requestId={error()?.requestId}
          onRetry={() => void boardStore.loadSync()}
        />
      </If>
      <If when={data() !== null}>
        <If when={stale()}>
          <StateNote testID="board-sync-stale" tone="warning" icon="refresh" text="显示的是上次读取的内容；正在重试健康状态。" />
        </If>
        <If when={phase() === "empty" || (data()!.entries.length === 0 && data()!.status.keys.length === 0)}>
          <PageEmpty
            testID="board-sync-empty"
            icon="layers"
            title={data()!.status.syncEnabled === false ? "同步没有启用" : "还没有同步入口"}
            description={
              data()!.status.syncEnabled === false
                ? "这个租户还没有开启外部同步；任务与线程不受影响。"
                : "入口由部署配置声明，这里只观察健康与处置故障。"
            }
            primaryLabel="返回队列"
            onPrimary={() => store.openBoard("queue")}
          />
        </If>
        <If when={data()!.entries.length > 0 || data()!.status.keys.length > 0}>
          <ScrollArea testID="board-sync-scroll" style={{ flex: 1, minHeight: 0 }}>
            <View style={{ flexDirection: "column", gap: space.lg, paddingBottom: space.xl }}>
              <HealthSummary data={data()!} />
              <Each of={data()!.status.keys.length ? data()!.status.keys : data()!.entries} key={(entry) => entry.key}>
                {(entry) => <EntryCard entry={entry()} data={data()!} />}
              </Each>
              <DeadLetters data={data()!} />
              <Failures failures={data()!.failures} />
            </View>
          </ScrollArea>
        </If>
      </If>
      <InboxSheet
        data={
          data() ?? {
            entries: [],
            status: { keys: [], outbox: { pending: 0, leased: 0, done: 0, dead: 0 } },
            dead: [],
            failures: [],
            credentials: {},
            credentialsKnown: false,
          }
        }
      />
    </View>
  );
}
