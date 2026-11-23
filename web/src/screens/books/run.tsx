// The team run page — change keel-bot-identity-access, A4.4b.
//
// 运行 is a projection of the server's own ledger: the captain session's events,
// the member task cards and the sub-agent registry. The swimlane is the default
// view (time × executor); 图 is the same facts as topology. No node and no edge
// exists unless one of those reads contained it.
//
// Failure is a first-class fact: the first failed node is selected by default,
// a red banner states its time and its own error text, and the inspector puts
// the refusal above the conversation facts. Where the server has no field yet
// (a team-run list, cost, a permission-decision read), the page says 待接入 and
// names the contract in its data-notes section.
import { effect, signal, Text, View, windowSize } from "nsh";
import {
  AlertDialog,
  Badge,
  Button,
  IconButton,
  ScrollArea,
  Sheet,
  Skeleton,
  alpha,
  color,
  font,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
} from "../../ui/kit";
import { Icon } from "../../ui/icons";
import { store } from "../../data/store";
import { identityAccess } from "../../data/identity-access";
import { botAccess, formatRunDuration, runStatusLabel, type RunNode, type RunNodeStatus, type RunProjection } from "../../data/bots";
import { capabilityOf } from "../../data/permission-catalog";
import { deploymentCapabilities } from "../../data/capabilities";
import { isTaskPanelReadable, shouldShowTaskPanel, taskPanelAccess } from "../../data/tasks";
import { formatWhen } from "./common";
import {
  BookAvatar,
  PageEmpty,
  PageError,
  PageLoading,
  PagePermission,
  PressRow,
  StateNote,
  Hairline,
  Kicker,
  Segmented,
  isMobile,
  isNarrow,
  isWide,
  touchStyle,
  ObjectSpine,
} from "./common";
import { showTeamSection } from "./teams";
import { TaskPanelBody, TaskPanelEntry, TaskPanelSheet } from "./task-panel";

const stopOpen = signal(false);
const rerunOpen = signal(false);
const inspectorOpen = signal(false);
const graphSheetOpen = signal(false);
const inspectorTab = signal<"summary" | "tools" | "permissions">("summary");
/** The task panel's own open state on the run paper (desktop: a band under the
 * run facts; phone: the bottom Sheet). It starts closed: the panel is an
 * entry, not a sixth chapter. */
const taskPanelOpen = signal(false);
const taskPanelSheetOpen = signal(false);

const STATUS_VARIANT: Record<RunNodeStatus, "info" | "success" | "destructive" | "secondary" | "outline"> = {
  running: "info",
  done: "success",
  failed: "destructive",
  stopped: "secondary",
  pending: "outline",
  unknown: "outline",
};

const statusBadge = (status: RunNodeStatus) => <Badge variant={STATUS_VARIANT[status]} label={runStatusLabel(status)} />;

const EDGE_LABEL: Record<string, string> = {
  mission: "下发使命",
  invoke: "调用",
  create: "创建",
  fork: "分叉",
  unknown: "关联",
};

const edgeLabel = (node: RunNode): string =>
  node.status === "failed" && (node.edge === "invoke" || node.edge === "fork" || node.edge === "create")
    ? "调用被拒"
    : EDGE_LABEL[node.edge ?? "unknown"] ?? "关联";

const MIN_BAR_PX = 24;

/** Whether this deployment serves the run surface's task panel: the
 * `task.panel` capability bit is the gate, and a deployment that did not
 * answer capabilities falls back to the transport's own read. */
const showTaskPanel = (): boolean =>
  shouldShowTaskPanel(deploymentCapabilities().features, isTaskPanelReadable());

/** The bar's own duration word: 38s / 1m06s, not a sentence. */
const barDuration = (ms: number | undefined): string => {
  if (!ms || ms < 0) return "—";
  const seconds = Math.max(1, Math.round(ms / 1000));
  if (seconds < 60) return `${seconds}s`;
  return `${Math.floor(seconds / 60)}m${String(seconds % 60).padStart(2, "0")}s`;
};

const two = (value: number): string => (value < 10 ? `0${value}` : String(value));

const clockOf = (iso: string | undefined): string => {
  if (!iso) return "—";
  const at = new Date(iso);
  if (!Number.isFinite(at.getTime())) return "—";
  return `${two(at.getHours())}:${two(at.getMinutes())}:${two(at.getSeconds())}`;
};

const clockOfDate = (at: Date): string => `${two(at.getHours())}:${two(at.getMinutes())}:${two(at.getSeconds())}`;

const shortSpan = (start: string | undefined, end: string | undefined): string => {
  if (!start) return "开始时间未返回";
  return `${clockOf(start)}${end ? ` → ${clockOf(end)}` : " 起"}`;
};

const tokensText = (tokens: number | undefined): string =>
  tokens === undefined ? "用量未返回" : tokens >= 1000 ? `${(tokens / 1000).toFixed(1)}k tokens` : `${tokens} tokens`;

/** The bar label's short form: 4m15s · 1.6k. A 24px bar cannot hold a sentence. */
const tokensShort = (tokens: number | undefined): string =>
  tokens === undefined ? "" : tokens >= 1000 ? `${(tokens / 1000).toFixed(1)}k` : String(tokens);

const accountName = (accountId: string | undefined): string => {
  if (!accountId) return "";
  return botAccess.accountNameOf(accountId) || "";
};

const capNameOf = (key: string | undefined): string => (key ? capabilityOf(key).displayName : "");

const KEY_PATTERN = /\b([a-z][a-z0-9]*(?:_[a-z0-9]+)*\.[a-z][a-z0-9_.]*)\b/;

/** A refused node's own error, split into the sentence and the key it named. */
function refusalOf(node: RunNode | undefined): { text: string; key?: string; capability?: string; owner?: string } | undefined {
  if (!node) return undefined;
  const text = node.error?.trim();
  if (!text) return undefined;
  const found = KEY_PATTERN.exec(text);
  return {
    text,
    key: found?.[1],
    capability: found?.[1] ? capNameOf(found[1]) : undefined,
    owner: accountName(node.ownerAccountId) || undefined,
  };
}

/** Drop the reader's transient run UI when the session changes. */
export function resetRunUi(): void {
  stopOpen.set(false);
  rerunOpen.set(false);
  inspectorOpen.set(false);
  graphSheetOpen.set(false);
  inspectorTab.set("summary");
  taskPanelOpen.set(false);
  taskPanelSheetOpen.set(false);
}

// ---- the page -------------------------------------------------------------

export function TeamRunPage(props: { teamId: string; runId: string }) {
  const team = () => botAccess.teamDetail();
  const run = () => botAccess.run();
  const loading = () => botAccess.runLoading() && run() === null;
  const error = () => botAccess.runError();
  const failed = () => error() !== null;
  const selected = () => run()?.nodes.find((node) => node.id === botAccess.runSelected()) ?? run()?.nodes[0];
  effect(() => {
    botAccess.ensureRun(props.teamId, props.runId);
  });
  effect(() => {
    const node = selected();
    if (node?.sessionId) void botAccess.ensureNodeEvents(node);
  });
  // The panel's read is one per session, and only when the deployment serves
  // it: the entry renders from the same capability answer.
  effect(() => {
    if (!showTaskPanel()) return;
    const runId = run()?.runId;
    if (runId) taskPanelAccess.ensure(runId);
  });
  const teamName = () => run()?.teamName ?? team()?.name ?? "团队";
  const status = () => run()?.status ?? "unknown";
  const isRunning = () => status() === "running" || status() === "pending";
  const failures = () => run()?.failures ?? [];
  return (
    <View testID="run-page" style={{ flex: 1, minHeight: 0, flexDirection: isMobile() ? "column" : "row" }}>
      <If when={!isNarrow()}>
        <ObjectSpine
          testID="run-spine"
          testIDPrefix="run-team-section"
          kicker={{ icon: "users", text: "团队 / 运行" }}
          title={teamName()}
          subtitle={run() ? `运行 · ${run()!.runId.slice(0, 8)}` : "运行"}
          description={run()?.mission}
          facts={
            run()
              ? [
                  { label: "队长", value: run()!.captainName ?? "未返回" },
                  { label: "成员", value: `${Math.max(0, run()!.nodes.length - 1)} 个节点 · ${failures().length} 个失败` },
                  { label: "用时", value: formatRunDuration(run()!.durationMs) },
                  { label: "发起", value: run()?.startedAt ? formatWhen(run()!.startedAt) : "未返回" },
                ]
              : []
          }
          items={[
            { id: "mission", num: "01", label: "使命" },
            { id: "roster", num: "02", label: "成员" },
            { id: "runs", num: "03", label: "运行" },
            { id: "records", num: "04", label: "记录" },
          ]}
          section="runs"
          onSection={(next) => {
            showTeamSection(next as "mission" | "roster" | "runs" | "records");
            store.openTeam(props.teamId);
          }}
          actions={
            <>
              <Button
                testID="run-refresh"
                variant="ghost"
                size="sm"
                icon="refresh"
                label="刷新"
                accessibilityLabel="刷新运行"
                onPress={() => void botAccess.loadRun(props.teamId, props.runId)}
                style={{ width: "100%" }}
              />
              <If
                when={isRunning()}
                else={
                  <Button
                    testID="run-again"
                    size="sm"
                    icon="play"
                    label="再跑一次"
                    accessibilityLabel="再跑一次"
                    onPress={() => rerunOpen.set(true)}
                    style={{ width: "100%" }}
                  />
                }
              >
                <StopButton testID="run-stop" onPress={() => stopOpen.set(true)} />
              </If>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, lineHeight: Math.round(typeScale.sm * lineHeight.normal) }}>
                {run()?.startedAt ? `读取于 ${formatWhen(run()!.startedAt)}` : "读取时快照；刷新可获取最新事件。"}
              </Text>
            </>
          }
        />
      </If>
      <View style={{ flex: 1, minWidth: 0, minHeight: 0, flexDirection: "row" }}>
        <ScrollArea testID="run-scroll" accessibilityLabel={`运行 ${props.runId}`} style={{ flex: 1, minHeight: 0 }}>
          <View
            style={{
              width: "100%",
              maxWidth: 1040,
              alignSelf: "center",
              paddingHorizontal: isMobile() ? space.lg : space["2xl"],
              paddingTop: space.lg,
              paddingBottom: space.lg,
              flexDirection: "column",
              gap: space.lg,
            }}
          >
            <If when={isNarrow()}>
              <RunSummary runId={props.runId} teamId={props.teamId} />
            </If>
            <If when={loading()}>
              <RunLoading />
            </If>
            <If when={failed() && !loading()}>
              <If when={error()?.status === 403}>
                <PagePermission
                  capability={capabilityOf(error()?.requiredKey ?? "session.read").displayName}
                  capabilityKey={error()?.requiredKey ?? "session.read"}
                />
              </If>
              <If when={error()?.status !== 403}>
                <PageError
                  title="没能读到这次运行"
                  message={error()?.message ?? ""}
                  requestId={error()?.requestId}
                  onRetry={() => void botAccess.loadRun(props.teamId, props.runId)}
                  retrying={botAccess.runLoading()}
                  testID="run-error"
                />
              </If>
            </If>
            <If when={run() !== null && !failed()}>
              <If when={botAccess.runStopNotice() !== null}>
                <StateNote tone="success" icon="check" text={botAccess.runStopNotice() ?? ""} testID="run-stop-notice" />
              </If>
              <If when={botAccess.runStopError() !== null}>
                <StateNote tone="danger" icon="alert" text={botAccess.runStopError() ?? ""} testID="run-stop-error" />
              </If>
              <RunBody run={run()!} isRunning={isRunning()} />
            </If>
          </View>
        </ScrollArea>
        <If when={!isNarrow() && run() !== null && !failed()}>
          <RunInspector node={selected()} width={isWide() ? 330 : 300} onClose={() => undefined} />
        </If>
      </View>
      <If when={isMobile() && run() !== null && !failed()}>
        <RunMobileBar isRunning={isRunning()} onInspector={() => inspectorOpen.set(true)} onStop={() => stopOpen.set(true)} runId={props.runId} />
      </If>
      <StopDialog runId={props.runId} teamId={props.teamId} />
      <RerunDialog teamId={props.teamId} />
      <RunInspectorSheet onStop={() => stopOpen.set(true)} />
      <GraphSheet />
      <If when={isMobile() && run() !== null}>
        <TaskPanelSheet
          open={taskPanelSheetOpen()}
          sessionId={props.runId}
          onClose={() => taskPanelSheetOpen.set(false)}
        />
      </If>
    </View>
  );
}

// ---- the summary / head ---------------------------------------------------

function RunSummary(props: { runId: string; teamId: string }) {
  const run = () => botAccess.run();
  const failures = () => run()?.failures ?? [];
  return (
    <View style={{ flexDirection: "column", gap: space.md }}>
      <View style={{ flexDirection: "row", alignItems: "center" }}>
        <Button
          variant="ghost"
          size="sm"
          icon="chevron-left"
          label="团队"
          accessibilityLabel="返回团队"
          onPress={() => store.closeTeamRun()}
          style={touchStyle()}
        />
        <Button
          testID="run-refresh"
          variant="ghost"
          size="sm"
          icon="refresh"
          label="刷新"
          accessibilityLabel="刷新运行"
          onPress={() => void botAccess.loadRun(props.teamId, props.runId)}
          style={{ marginLeft: "auto", ...touchStyle() }}
        />
      </View>
      <View
        style={{
          flexDirection: "column",
          gap: space.xs,
          padding: space.md,
          borderRadius: radius.md,
          borderWidth: 1,
          borderColor: color.border,
          backgroundColor: color.hoverFaint,
        }}
      >
        <Kicker icon="users" text="团队 · 运行" />
        <Text style={{ fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {run()?.teamName ?? "团队"}
        </Text>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
          {statusBadge(run()?.status ?? "unknown")}
          <If when={failures().length > 0}>
            <Badge variant="destructive" label={`${failures().length} 个失败`} />
          </If>
        </View>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 2 }}>{run()?.mission}</Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {shortSpan(run()?.startedAt, run()?.endedAt)} · {formatRunDuration(run()?.durationMs)} · {tokensText(run()?.tokens)}
        </Text>
      </View>
    </View>
  );
}

function RunLoading() {
  return (
    <View testID="run-loading" style={{ flexDirection: "column", gap: space.md, paddingVertical: space.lg }}>
      <Skeleton width={200} height={22} accessibilityLabel="加载中" />
      <Skeleton width="60%" height={12} accessibilityLabel="加载中" />
      <View style={{ flexDirection: "column", gap: space.sm, paddingTop: space.md }}>
        <Each of={[0, 1, 2, 3]} key={(index) => index}>
          {(index) => (
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, height: 44 }}>
              <Skeleton width={110} height={12} accessibilityLabel="加载中" />
              <Skeleton width="42%" height={20} accessibilityLabel="加载中" />
            </View>
          )}
        </Each>
      </View>
    </View>
  );
}

export function RunBody(props: { run: RunProjection; isRunning: boolean }) {
  const run = () => props.run;
  const runModeIs = (mode: "lanes" | "graph"): boolean => botAccess.runMode() === mode;
  const empty = () =>
    run().nodes.length <= 1 &&
    !run().sources.events &&
    !run().sources.tasks &&
    !run().sources.subagents &&
    !run().startedAt;
  const failure = () => run().failures[0];
  return (
    <View style={{ flexDirection: "column", gap: space.md }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: isMobile() ? "nowrap" : "wrap", width: "100%" }}>
        <If when={!isNarrow()}>
          {statusBadge(run().status)}
          <If when={run().failures.length > 0}>
            <Badge variant="destructive" label={`${run().failures.length} 个失败`} />
          </If>
          <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>
            {shortSpan(run().startedAt, run().endedAt)}
          </Text>
        </If>
        <View style={{ marginLeft: isMobile() ? undefined : "auto", flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Segmented
            testID="run-mode"
            accessibilityLabel="运行视图"
            style={{ width: isMobile() ? 180 : 160 }}
            items={[
              { id: "lanes", label: "泳道" },
              { id: "graph", label: "图" },
            ]}
            value={botAccess.runMode()}
            onSelect={(next) => {
              botAccess.runMode.set(next as "lanes" | "graph");
              if (isMobile() && next === "graph") graphSheetOpen.set(true);
            }}
          />
        </View>
      </View>
      <If when={run().failures.length > 0}>
        <FailureBanner node={failure()!} onOpen={() => {
          botAccess.runSelected.set(failure()!.id);
          if (isMobile()) inspectorOpen.set(true);
        }} />
      </If>
      <If when={showTaskPanel()}>
        <TaskPanelEntry
          sessionId={run().runId}
          open={isMobile() ? taskPanelSheetOpen() : taskPanelOpen()}
          onOpen={() => {
            if (isMobile()) taskPanelSheetOpen.set(true);
            else taskPanelOpen.update((open) => !open);
          }}
        />
        <If when={!isMobile() && taskPanelOpen()}>
          <View
            testID="run-task-panel-band"
            style={{
              flexDirection: "column",
              padding: space.md,
              borderRadius: radius.md,
              borderWidth: 1,
              borderColor: color.border,
              backgroundColor: color.hoverFaint,
            }}
          >
            <TaskPanelBody sessionId={run().runId} />
          </View>
        </If>
      </If>
      <If when={empty()}>
        <PageEmpty
          testID="run-empty"
          icon="activity"
          title="这次运行还没有可读的事件"
          description="会话账本、成员任务和子会话都还没有返回内容。刷新一次；如果一直为空，让管理员检查这次会话是否已经启动。"
          primaryLabel="刷新"
          onPrimary={() => void botAccess.loadRun(run().teamId, run().runId)}
        />
      </If>
      <If when={!empty()}>
        <If when={runModeIs("graph")}>
          <If when={isMobile()} else={<RunGraph run={run()} />}>
            <GraphPrompt />
          </If>
        </If>
        <If when={runModeIs("lanes")}>
          <If when={isMobile()} else={<RunLanes run={run()} />}>
            <RunTimeline run={run()} />
          </If>
        </If>
        <RunLegend />
      </If>
      <If when={!isMobile()}>
        <If when={isNarrow()} else={<RunEventList run={run()} />}>
          <View style={{ flexDirection: "row", gap: space.md, alignItems: "stretch" }}>
            <View style={{ flex: 1, minWidth: 0 }}>
              <RunEventList run={run()} />
            </View>
            <View style={{ width: 280, flexShrink: 0 }}>
              <NarrowInspectorEntry />
            </View>
          </View>
        </If>
      </If>
      <CaptainSummary runId={run().runId} />
      <RunNotes run={run()} />
    </View>
  );
}

function GraphPrompt() {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>图在窄屏里以全屏打开，避免把桌面拓扑硬塞进单列。</Text>
      <Button size="sm" variant="outline" label="打开图" accessibilityLabel="打开图" onPress={() => graphSheetOpen.set(true)} style={touchStyle()} />
    </View>
  );
}

/** 停止运行: the dangerous action keeps the outline face (design: a red border,
 * not a red block) and is always followed by the confirmation dialog. */
function StopButton(props: { testID: string; mobile?: boolean; onPress: () => void }) {
  return (
    <Button
      testID={props.testID}
      variant="outline"
      size={props.mobile === true ? "lg" : "sm"}
      accessibilityLabel="停止运行"
      onPress={props.onPress}
      style={[
        { borderColor: alpha(color.danger, 0.55) },
        props.mobile === true ? { flex: 1, height: 44 } : { width: "100%" },
      ]}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}>
        <Icon name="stop" size={12} tint={color.danger} />
        <Text style={{ fontSize: typeScale.sm, color: color.danger }}>停止运行</Text>
      </View>
    </Button>
  );
}

function FailureBanner(props: { node: RunNode; onOpen: () => void }) {
  const refusal = () => refusalOf(props.node);
  const capabilityLine = () => {
    const current = refusal();
    if (!current?.capability) return "";
    return current.text.includes(current.capability) ? "" : ` 缺「${current.capability}」能力。`;
  };
  return (
    <View
      testID="run-failure-banner"
      style={{
        flexDirection: isMobile() ? "column" : "row",
        alignItems: isMobile() ? "stretch" : "center",
        gap: space.md,
        padding: space.md,
        borderRadius: radius.md,
        backgroundColor: color.dangerTint,
        borderWidth: 1,
        borderColor: color.danger,
      }}
    >
      <Icon name="alert" size={14} tint={color.danger} />
      <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: 2 }}>
        <Text style={{ fontSize: typeScale.base, color: color.foreground }}>
          <Text style={{ fontWeight: weight.semibold }}>{props.node.name} 失败</Text>
          {props.node.startedAt ? ` · ${clockOf(props.node.startedAt)}` : ""}
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 2 }}>
          {refusal()?.text ?? "服务端没有返回失败原因。"}
          {capabilityLine()}
          {refusal()?.owner ? ` 由 ${refusal()?.owner}（对象归属者）授予，或换一个成员。` : ""}
        </Text>
      </View>
      <Button variant="outline" size="sm" label="查看原因" accessibilityLabel="查看失败原因" onPress={props.onOpen} style={touchStyle()} />
    </View>
  );
}

function RunLegend() {
  return (
    <View style={{ flexDirection: "row", gap: space.md, flexWrap: "wrap", alignItems: "center" }}>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>实线 调用</Text>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>虚线 创建 / 分叉</Text>
      <Text style={{ fontSize: typeScale.sm, color: color.danger }}>红虚线 拒绝</Text>
      <Text style={{ marginLeft: "auto", fontSize: typeScale.sm, color: color.mutedForeground }}>「图」为拓扑视角</Text>
    </View>
  );
}

// ---- swimlane -------------------------------------------------------------

interface LaneCell {
  node: RunNode;
  row: number;
  left: number;
  width: number;
}

function laneWidth(): number {
  const width = windowSize().width;
  const chrome = isNarrow() ? 240 : 226 + 64 + 300 + 190;
  return Math.max(280, width - chrome);
}

function laneLayout(run: RunProjection): {
  cells: LaneCell[];
  startMs: number;
  endMs: number;
  trackWidth: number;
} {
  const trackWidth = laneWidth();
  const startMs = run.startedAt ? Date.parse(run.startedAt) : Date.now() - 60_000;
  const now = Date.now();
  const endMs = run.endedAt ? Date.parse(run.endedAt) : now;
  const from = Number.isFinite(startMs) ? Math.min(startMs, endMs - 1000) : now - 60_000;
  const to = Number.isFinite(endMs) ? Math.max(endMs, from + 1000) : from + 60_000;
  const cells = run.nodes.map((node, row) => {
    const nodeStart = node.startedAt ? Date.parse(node.startedAt) : from;
    const nodeEnd = node.endedAt ? Date.parse(node.endedAt) : node.status === "running" ? now : nodeStart;
    const safeStart = Number.isFinite(nodeStart) ? Math.min(Math.max(nodeStart, from), to) : from;
    const safeEnd = Number.isFinite(nodeEnd) ? Math.min(Math.max(nodeEnd, safeStart), to) : safeStart;
    const left = ((safeStart - from) / (to - from)) * trackWidth;
    const minWidth = node.status === "failed" ? 32 : MIN_BAR_PX;
    const width = Math.max(minWidth, ((safeEnd - safeStart) / (to - from)) * trackWidth);
    return { node, row, left, width: Math.min(width, trackWidth - left) };
  });
  return { cells, startMs: from, endMs: to, trackWidth };
}

function barFace(status: RunNodeStatus): { backgroundColor?: string; borderColor: string; borderStyle?: "dashed" | "solid" } {
  switch (status) {
    case "failed":
      return { backgroundColor: color.dangerTint, borderColor: color.danger, borderStyle: "dashed" };
    case "done":
      return { backgroundColor: color.successTint, borderColor: color.border, borderStyle: "solid" };
    case "running":
      return { backgroundColor: color.infoTint, borderColor: color.info, borderStyle: "solid" };
    case "pending":
      return { borderColor: color.border, borderStyle: "dashed" };
    case "stopped":
      return { backgroundColor: color.muted, borderColor: color.border, borderStyle: "solid" };
    default:
      return { borderColor: color.border, borderStyle: "dashed" };
  }
}

function RunLanes(props: { run: RunProjection }) {
  const layout = () => laneLayout(props.run);
  const axis = () => {
    const { startMs, endMs, trackWidth } = layout();
    const steps = trackWidth < 420 ? 3 : trackWidth < 520 ? 4 : 5;
    return Array.from({ length: steps + 1 }, (_, step) => {
      const ms = startMs + ((endMs - startMs) * step) / steps;
      return { left: (trackWidth * step) / steps, label: clockOfDate(new Date(ms)) };
    });
  };
  const parentName = (cell: LaneCell): string =>
    layout().cells.find((candidate) => candidate.node.id === cell.node.parentId)?.node.name ?? "";
  const rowHeight = 56;
  return (
    <View testID="run-lanes" style={{ flexDirection: "column", gap: space.xs }}>
      <View
        style={{
          flexDirection: "row",
          alignItems: "center",
          gap: space.md,
          paddingVertical: space.xs,
          borderBottomWidth: 1,
          borderBottomColor: color.border,
        }}
      >
        <Text style={{ width: 170, fontSize: typeScale.sm, color: color.mutedForeground }}>使命</Text>
        <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {props.run.mission || "使命未返回"}
        </Text>
      </View>
      <View style={{ flexDirection: "row" }}>
        <View style={{ width: 170, flexShrink: 0 }} />
        <View style={{ flex: 1, minWidth: 0, height: 22, position: "relative" }}>
          <Each of={axis()} key={(tick) => tick.left}>
            {(tick) => (
              <Text
                style={{
                  position: "absolute",
                  left: Math.max(0, tick().left === layout().trackWidth ? layout().trackWidth - 46 : tick().left - 22),
                  top: 0,
                  fontFamily: font.mono,
                  fontSize: typeScale.key,
                  color: color.mutedForeground,
                  numberOfLines: 1,
                }}
              >
                {tick().label}
              </Text>
            )}
          </Each>
        </View>
      </View>
      <View style={{ position: "relative" }}>
        <Each of={layout().cells} key={(cell) => cell.node.id}>
          {(cell) => <LaneRow cell={cell()} parentName={parentName(cell())} />}
        </Each>
        <View
          style={{
            position: "absolute",
            left: 170,
            top: 0,
            width: layout().trackWidth,
            height: layout().cells.length * rowHeight,
            pointerEvents: "none",
          }}
        >
          <Each of={axis()} key={(tick) => `grid-${tick.left}`}>
            {(tick) => (
              <View style={{ position: "absolute", left: tick().left, top: 0, width: 1, height: "100%", backgroundColor: color.border, opacity: 0.4 }} />
            )}
          </Each>
          <Each of={layout().cells} key={(cell) => `edge-${cell.node.id}`}>
            {(cell) => {
              const parent = () => layout().cells.find((candidate) => candidate.node.id === cell().node.parentId);
              const parentY = () => (parent()?.row ?? 0) * rowHeight + 28;
              const childY = () => cell().row * rowHeight + 28;
              const parentEnd = () => (parent()?.left ?? 0) + (parent()?.width ?? 0);
              const childX = () => cell().left;
              const refused = () => cell().node.status === "failed";
              const dashed = () => refused() || cell().node.edge === "create" || cell().node.edge === "fork";
              const ink = () => (refused() ? color.danger : cell().node.edge === "invoke" ? color.mutedForeground : color.border);
              const top = () => Math.min(parentY(), childY());
              return (
                <If when={cell().node.kind !== "captain"}>
                  <If when={parentEnd() < childX()}>
                    <View
                      style={{
                        position: "absolute",
                        left: parentEnd(),
                        top: parentY(),
                        width: childX() - parentEnd(),
                        height: 1,
                        backgroundColor: dashed() ? undefined : ink(),
                        borderTopWidth: dashed() ? 1 : 0,
                        borderStyle: "dashed",
                        borderColor: ink(),
                      }}
                    />
                  </If>
                  <View
                    style={{
                      position: "absolute",
                      left: childX(),
                      top: top(),
                      width: 1,
                      height: Math.abs(childY() - parentY()),
                      backgroundColor: dashed() ? undefined : ink(),
                      borderLeftWidth: dashed() ? 1 : 0,
                      borderStyle: "dashed",
                      borderColor: ink(),
                    }}
                  />
                  <Text
                    style={{
                      position: "absolute",
                      left: childX() + 4,
                      top: childY() + 13,
                      fontSize: typeScale.key,
                      color: refused() ? color.danger : color.mutedForeground,
                      backgroundColor: color.background,
                      paddingHorizontal: 2,
                    }}
                  >
                    {edgeLabel(cell().node)}
                  </Text>
                </If>
              );
            }}
          </Each>
          <View
            style={{
              position: "absolute",
              left: layout().cells[0]?.left ?? 0,
              top: 0,
              width: 1,
              height: 26,
              backgroundColor: color.border,
            }}
          />
          <Text style={{ position: "absolute", left: 4, top: 2, fontSize: typeScale.key, color: color.mutedForeground }}>下发使命</Text>
        </View>
      </View>
    </View>
  );
}

function LaneRow(props: { cell: LaneCell; parentName: string }) {
  const node = () => props.cell.node;
  const face = () => barFace(node().status);
  const selected = () => botAccess.runSelected() === node().id;
  return (
    <View
      testID={`run-lane-${node().id}`}
      style={{
        flexDirection: "row",
        alignItems: "stretch",
        height: 56,
        borderTopWidth: 1,
        borderTopColor: color.border,
        backgroundColor: node().status === "failed" ? color.dangerTint : undefined,
      }}
    >
      <View style={{ width: 170, flexShrink: 0, flexDirection: "row", alignItems: "center", gap: space.xs, paddingLeft: (node().depth - 1) * 10 }}>
        <BookAvatar name={node().name} />
        <View style={{ minWidth: 0, flexDirection: "column" }}>
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
            {node().name}
          </Text>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
            {props.parentName ? `← ${props.parentName}` : node().kind === "captain" ? "队长" : "运行节点"}
          </Text>
        </View>
      </View>
      <View style={{ flex: 1, minWidth: 0, position: "relative" }}>
        <View
          testID={`run-bar-${node().id}`}
          role="button"
          focusable
          onPress={() => {
            botAccess.runSelected.set(node().id);
            if (isMobile()) inspectorOpen.set(true);
          }}
          accessibilityLabel={`查看节点 ${node().name}，${runStatusLabel(node().status)}`}
          accessibilityState={{ selected: selected() }}
          style={{
            position: "absolute",
            left: props.cell.left,
            top: 16,
            width: props.cell.width,
            height: 24,
            borderRadius: radius.xs,
            borderWidth: 1,
            borderStyle: face().borderStyle,
            borderColor: selected() ? color.foreground : face().borderColor,
            backgroundColor: face().backgroundColor,
            paddingHorizontal: space.xs,
            justifyContent: "center",
          }}
        >
          <Text
            style={{
              fontFamily: font.mono,
              fontSize: typeScale.key,
              color: node().status === "failed" ? color.danger : color.foreground,
              numberOfLines: 1,
              textOverflow: "ellipsis",
            }}
          >
            {node().status === "failed"
              ? "403"
              : props.cell.width < 72
                ? barDuration(node().durationMs)
                : `${barDuration(node().durationMs)}${node().tokens ? ` · ${tokensShort(node().tokens)}` : ""}`}
          </Text>
        </View>
      </View>
    </View>
  );
}

/** ≤767: the swimlane becomes a vertical timeline. It keeps the call order and
 * the edge labels; the failure card keeps its time and the 查看原因 entry. */
function RunTimeline(props: { run: RunProjection }) {
  const rows = () => props.run.nodes;
  return (
    <View testID="run-timeline" style={{ flexDirection: "column", gap: 0, width: windowSize().width - 32, alignSelf: "stretch" }}>
      <Each of={rows()} key={(node) => `timeline-${node.id}`}>
        {(node) => (
          <View style={{ flexDirection: "column", gap: 0, width: "100%", alignSelf: "stretch" }}>
            <If when={node().kind !== "captain"}>
              <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs, paddingVertical: 2, paddingLeft: (node().depth - 1) * 14 }}>
                <Icon
                  name={node().status === "failed" ? "x" : node().edge === "create" || node().edge === "fork" ? "layers" : "arrow-down"}
                  size={11}
                  tint={node().status === "failed" ? color.danger : color.mutedForeground}
                />
                <Text style={{ fontSize: typeScale.sm, color: node().status === "failed" ? color.danger : color.mutedForeground }}>
                  {edgeLabel(node())}
                </Text>
              </View>
            </If>
            <View
              testID={`run-timeline-${node().id}`}
              role="button"
              focusable
              onPress={() => {
                botAccess.runSelected.set(node().id);
                inspectorOpen.set(true);
              }}
              accessibilityLabel={`查看节点 ${node().name}，${runStatusLabel(node().status)}`}
              accessibilityState={{ selected: botAccess.runSelected() === node().id }}
              style={{
                marginLeft: (node().depth - 1) * 14,
                alignSelf: "stretch",
                padding: space.md,
                borderRadius: radius.md,
                borderWidth: 1,
                borderStyle: node().status === "pending" ? "dashed" : "solid",
                borderColor: node().status === "failed" ? color.danger : botAccess.runSelected() === node().id ? color.foreground : color.border,
                backgroundColor: node().status === "failed" ? color.dangerTint : color.hoverFaint,
                minHeight: 56,
                flexDirection: "column",
                gap: 2,
              }}
            >
              <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
                <BookAvatar name={node().name} />
                <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                  {node().name}
                </Text>
                {statusBadge(node().status)}
              </View>
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                {node().status === "failed"
                  ? `${clockOf(node().startedAt)} · ${node().error ?? "服务端没有返回失败原因"}`
                  : `${barDuration(node().durationMs)} · ${tokensText(node().tokens)}`}
              </Text>
              <If when={node().status === "failed"}>
                <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs, paddingTop: 2 }}>
                  <Text style={{ fontSize: typeScale.sm, color: color.danger }}>查看原因</Text>
                  <Icon name="chevron-right" size={11} tint={color.danger} />
                </View>
              </If>
            </View>
          </View>
        )}
      </Each>
    </View>
  );
}

// ---- graph ----------------------------------------------------------------

function RunGraph(props: { run: RunProjection }) {
  const depths = () => {
    const max = props.run.nodes.reduce((top, node) => Math.max(top, node.depth), 1);
    return Array.from({ length: max }, (_, index) => index + 1).map((depth) => ({
      depth,
      nodes: props.run.nodes.filter((node) => node.depth === depth),
    }));
  };
  return (
    <View testID="run-graph" style={{ flexDirection: "column", gap: space.md }}>
      <View
        style={{
          padding: space.md,
          borderRadius: radius.md,
          borderWidth: 1,
          borderColor: color.info,
          backgroundColor: color.infoTint,
          flexDirection: "column",
          gap: 2,
        }}
      >
        <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground }}>
          {props.run.teamName} · {props.run.mission.slice(0, 40)}
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          使命 · 队长 {props.run.captainName ?? "未返回"}
        </Text>
      </View>
      <Each of={depths()} key={(level) => level.depth}>
        {(level) => (
          <View style={{ flexDirection: "column", gap: space.sm }}>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
              <Icon name="arrow-down" size={12} tint={color.mutedForeground} />
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                {level().depth === 1 ? "下发使命" : [...new Set(level().nodes.map((node) => edgeLabel(node)))].join(" · ") || "关联"}
              </Text>
            </View>
            <View style={{ flexDirection: "row", gap: space.sm, flexWrap: "wrap" }}>
              <Each of={level().nodes} key={(node) => node.id}>
                {(node) => <GraphNode node={node()} />}
              </Each>
            </View>
          </View>
        )}
      </Each>
    </View>
  );
}

function GraphNode(props: { node: RunNode }) {
  const node = () => props.node;
  const parent = () => botAccess.run()?.nodes.find((entry) => entry.id === node().parentId);
  const selected = () => botAccess.runSelected() === node().id;
  const dimmed = () => botAccess.runSelected() !== null && !selected();
  return (
    <View
      testID={`run-node-${node().id}`}
      role="button"
      focusable
      onPress={() => {
        botAccess.runSelected.set(node().id);
        if (isMobile()) inspectorOpen.set(true);
      }}
      accessibilityLabel={`查看节点 ${node().name}，${runStatusLabel(node().status)}`}
      accessibilityState={{ selected: selected() }}
      style={{
        minWidth: 180,
        flexGrow: 1,
        maxWidth: 300,
        padding: space.sm,
        borderRadius: radius.md,
        borderWidth: 1,
        borderStyle: node().status === "pending" ? "dashed" : "solid",
        borderColor: node().status === "failed" ? color.danger : selected() ? color.foreground : color.border,
        backgroundColor: node().status === "failed" ? color.dangerTint : color.hoverFaint,
        opacity: dimmed() ? 0.6 : 1,
        flexDirection: "column",
        gap: 2,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}>
        <View
          style={{
            width: 6,
            height: 6,
            borderRadius: radius.pill,
            backgroundColor:
              node().status === "failed"
                ? color.danger
                : node().status === "done"
                  ? color.success
                  : node().status === "running"
                    ? color.info
                    : color.mutedForeground,
          }}
        />
        <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
          {node().name}
        </Text>
        {statusBadge(node().status)}
      </View>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
        {parent() ? `← ${parent()!.name} · ${edgeLabel(node())}` : node().subtitle ?? node().agent ?? "运行节点"}
      </Text>
      <Text style={{ fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
        {node().status === "failed" ? node().error ?? "失败" : `${formatRunDuration(node().durationMs)} · ${tokensText(node().tokens)}`}
      </Text>
    </View>
  );
}

// ---- inspector ------------------------------------------------------------

function RunInspector(props: { node: RunNode | undefined; width: number; onClose: () => void }) {
  const node = () => props.node;
  return (
    <View
      testID="run-inspector"
      style={{
        width: props.width,
        flexShrink: 0,
        minHeight: 0,
        borderLeftWidth: 1,
        borderLeftColor: color.border,
        backgroundColor: color.background,
        flexDirection: "column",
      }}
    >
      <View style={{ padding: space.lg, paddingBottom: space.sm }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
            {node()?.name ?? "检查器"}
          </Text>
          {node() ? statusBadge(node()!.status) : null}
        </View>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {node() ? shortSpan(node()!.startedAt, node()!.endedAt) : "选择一个节点看它做了什么"}
        </Text>
      </View>
      <InspectorTabs />
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
        <View style={{ paddingHorizontal: space.lg, paddingBottom: space.lg }}>
          <InspectorBody node={node()} />
        </View>
      </ScrollArea>
      <InspectorFooter node={node()} />
    </View>
  );
}

function InspectorTabs() {
  return (
    <View style={{ paddingHorizontal: space.lg, paddingBottom: space.sm }}>
      <Segmented
        testID="run-inspector-tab"
        accessibilityLabel="检查器内容"
        items={[
          { id: "summary", label: "摘要" },
          { id: "tools", label: "工具" },
          { id: "permissions", label: "权限" },
        ]}
        value={inspectorTab()}
        onSelect={(next) => inspectorTab.set(next as "summary" | "tools" | "permissions")}
      />
    </View>
  );
}

function InspectorBody(props: { node: RunNode | undefined }) {
  const node = () => props.node;
  const refusal = () => refusalOf(node());
  const tools = () => (node() ? botAccess.nodeTools(node()!) : []);
  const denials = () => (node() ? botAccess.nodeDenials(node()!) : []);
  const loading = () => node()?.sessionId !== undefined && botAccess.nodeEventsLoading() === node()!.sessionId;
  const run = () => botAccess.run();
  const parent = () => (node()?.parentId ? botAccess.run()?.nodes.find((entry) => entry.id === node()!.parentId) : undefined);
  return (
    <View style={{ flexDirection: "column", gap: space.md }}>
      <If when={node()?.status === "failed" || (refusal()?.capability !== undefined)}>
        <StateNote
          tone="danger"
          icon="lock"
          text={refusal()?.text ?? "这一步失败了，但服务端没有返回原因。"}
          testID="run-inspector-refusal"
        >
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            {refusal()?.capability && !(refusal()?.text ?? "").includes(refusal()!.capability!)
              ? `缺「${refusal()?.capability}」能力。`
              : ""}
            {refusal()?.owner ? `由 ${refusal()?.owner}（对象归属者）授予，或换一个成员。` : "让对象的归属者授予后再试。"}
          </Text>
        </StateNote>
      </If>
      <If when={inspectorTab() === "summary"}>
        <View style={{ flexDirection: "column", gap: space.xs }}>
          <FactRow label="对象归属者" value={accountName(node()?.ownerAccountId) || "未返回（需要 Bot 归属读）"} />
          <FactRow label="父级" value={parent()?.name ?? (node()?.kind === "captain" ? "使命" : "未返回")} />
          <FactRow label="实际模型" value={node()?.model ?? run()?.model ?? "未返回"} mono />
          <FactRow label="状态" value={runStatusLabel(node()?.status ?? "unknown")} />
          <FactRow label="开始" value={clockOf(node()?.startedAt)} />
          <FactRow label="结束" value={node()?.endedAt ? clockOf(node()?.endedAt) : "未结束"} />
          <FactRow label="用时" value={formatRunDuration(node()?.durationMs)} />
          <FactRow label="用量" value={tokensText(node()?.tokens)} />
          <If when={node()?.summary !== undefined}>
            <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.foreground }}>
              {node()?.summary}
            </Text>
          </If>
        </View>
      </If>
      <If when={inspectorTab() === "tools"}>
        <If when={node()?.sessionId === undefined}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            这个节点没有会话：它还没有启动，所以没有工具调用。
          </Text>
        </If>
        <If when={node()?.sessionId !== undefined && loading()}>
          <View style={{ flexDirection: "column", gap: space.sm }}>
            <Skeleton width="70%" height={12} accessibilityLabel="加载中" />
            <Skeleton width="50%" height={12} accessibilityLabel="加载中" />
          </View>
        </If>
        <If when={node()?.sessionId !== undefined && !loading()}>
          <If
            when={tools().length > 0}
            else={<Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>这个会话还没有可读的工具调用。</Text>}
          >
            <View style={{ flexDirection: "column", gap: space.xs }}>
              <Each of={tools()} key={(tool) => tool.id}>
                {(tool) => (
                  <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, minHeight: 30, borderTopWidth: 1, borderTopColor: color.border }}>
                    <Text style={{ flex: 1, minWidth: 0, fontFamily: font.mono, fontSize: typeScale.key, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                      {tool().name}
                    </Text>
                    <Badge variant={tool().status === "error" ? "destructive" : tool().status === "ok" ? "success" : "outline"} label={tool().status === "error" ? "失败" : tool().status === "ok" ? "成功" : "进行中"} />
                    <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                      {tool().durationMs !== undefined ? `${tool().durationMs}ms` : "—"}
                    </Text>
                  </View>
                )}
              </Each>
              <Each of={tools().filter((tool) => tool.status === "error" && tool.text)} key={(tool) => `err-${tool.id}`}>
                {(tool) => (
                  <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.danger, numberOfLines: 3 }}>
                    {tool().name}：{tool().text}
                  </Text>
                )}
              </Each>
            </View>
          </If>
        </If>
      </If>
      <If when={inspectorTab() === "permissions"}>
        <If when={node()?.sessionId === undefined}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            这个节点没有会话，没有权限判定可显示。
          </Text>
        </If>
        <If when={node()?.sessionId !== undefined}>
          <If
            when={denials().length > 0}
            else={<Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>这次运行没有返回被拒绝的工具调用。</Text>}
          >
            <View style={{ flexDirection: "column", gap: space.xs }}>
              <Each of={denials()} key={(denial) => denial.key}>
                {(denial) => (
                  <View style={{ flexDirection: "column", gap: 2, paddingVertical: space.xs, borderTopWidth: 1, borderTopColor: color.border }}>
                    <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
                      <Text style={{ flex: 1, minWidth: 0, fontFamily: font.mono, fontSize: typeScale.key, color: color.foreground }}>
                        {denial().key}
                      </Text>
                      <Badge variant="destructive" label="拒绝" />
                    </View>
                    <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 2 }}>{denial().detail}</Text>
                  </View>
                )}
              </Each>
            </View>
          </If>
        </If>
        <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
          权限判定的一等接口还没有接入；这里只显示工具结果里标了错误的调用。需要 run.permission.decisions 才能逐项列出允许/拒绝与范围。
        </Text>
      </If>
      <If when={botAccess.nodeEventsError() !== null && botAccess.nodeEventsError()?.sessionId === node()?.sessionId}>
        <StateNote tone="danger" icon="alert" text={`没能读到这个节点的会话事件：${botAccess.nodeEventsError()?.message ?? ""}`}>
          <Button
            variant="outline"
            size="sm"
            label="重试"
            accessibilityLabel="重试读取节点事件"
            onPress={() => {
              const current = node();
              if (current) botAccess.retryNodeEvents(current);
            }}
            style={touchStyle()}
          />
        </StateNote>
      </If>
    </View>
  );
}

function FactRow(props: { label: string; value: string; mono?: boolean }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "baseline", gap: space.sm }}>
      <Text style={{ width: 72, flexShrink: 0, fontSize: typeScale.sm, color: color.mutedForeground }}>{props.label}</Text>
      <Text
        style={{
          flex: 1,
          minWidth: 0,
          fontFamily: props.mono ? font.mono : undefined,
          fontSize: props.mono ? typeScale.key : typeScale.sm,
          color: color.foreground,
        }}
      >
        {props.value}
      </Text>
    </View>
  );
}

function InspectorFooter(props: { node: RunNode | undefined }) {
  const sessionId = () => props.node?.sessionId;
  return (
    <View style={{ flexDirection: "row", gap: space.sm, padding: space.lg, paddingTop: space.md, borderTopWidth: 1, borderTopColor: color.border }}>
      <Button
        testID="run-inspector-open"
        size={isMobile() ? "lg" : "sm"}
        icon="external"
        label="打开会话"
        accessibilityLabel="打开会话"
        disabled={!sessionId()}
        onPress={() => {
          const id = sessionId();
          if (!id) return;
          store.openChapter("workspace");
          void store.openSession(id);
        }}
        style={{ flex: 1, ...touchStyle() }}
      />
      <Button
        testID="run-inspector-record"
        variant="outline"
        size={isMobile() ? "lg" : "sm"}
        label="查看记录"
        accessibilityLabel="查看记录"
        onPress={() => {
          if (props.node) identityAccess.recordsQuery.set(props.node.name);
          store.openChapter("records");
        }}
        style={touchStyle()}
      />
    </View>
  );
}

// ---- events / summary / notes ---------------------------------------------

/** 768: the inspector is one entry beside the event list; its full form opens
 * as a Sheet (the 390 frame's shape), not as a squeezed third column. */
function NarrowInspectorEntry() {
  const node = () => botAccess.run()?.nodes.find((entry) => entry.id === botAccess.runSelected());
  return (
    <View
      testID="run-inspector-entry"
      style={{
        flexDirection: "column",
        gap: space.sm,
        padding: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
        backgroundColor: color.hoverFaint,
        minHeight: 150,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <Text style={{ flex: 1, fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground }}>检查器</Text>
        {node() ? statusBadge(node()!.status) : null}
      </View>
      <Text style={{ fontSize: typeScale.base, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
        {node()?.name ?? "选择一个节点"}
      </Text>
      <Text style={{ fontSize: typeScale.sm, color: node()?.status === "failed" ? color.danger : color.mutedForeground, numberOfLines: 2 }}>
        {node()?.status === "failed" ? node()?.error ?? "失败" : node() ? `${runStatusLabel(node()!.status)} · ${formatRunDuration(node()!.durationMs)}` : "点节点看它做了什么"}
      </Text>
      <Button
        testID="run-open-inspector"
        variant="outline"
        size="sm"
        label="打开检查器"
        accessibilityLabel="打开检查器"
        onPress={() => inspectorOpen.set(true)}
        style={{ marginTop: "auto", ...touchStyle() }}
      />
    </View>
  );
}

function RunEventList(props: { run: RunProjection }) {
  const rows = () => props.run.nodes.filter((node) => node.kind !== "captain").slice(0, 8);
  return (
    <If when={rows().length > 0}>
      <View testID="run-events" style={{ flexDirection: "column", gap: space.xs, maxWidth: 820 }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
          <Text style={{ fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground }}>运行事件</Text>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{rows().length} 条</Text>
        </View>
        <Each of={rows()} key={(node) => `event-${node.id}`}>
          {(node) => (
            <PressRow
              testID={`run-event-${node().id}`}
              accessibilityLabel={`查看事件 ${node().name}`}
              onPress={() => botAccess.runSelected.set(node().id)}
            >
              <Text style={{ width: 72, flexShrink: 0, fontFamily: font.mono, fontSize: typeScale.key, color: color.mutedForeground }}>
                {clockOf(node().startedAt)}
              </Text>
              <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.sm, color: color.foreground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                {edgeLabel(node())} · {node().name}
                {node().status === "failed" && node().error ? `（${node().error}）` : ""}
              </Text>
              <Badge
                variant={node().status === "failed" ? "destructive" : node().status === "done" ? "success" : node().status === "running" ? "info" : "outline"}
                label={node().status === "failed" ? "拒绝" : node().status === "done" ? "成功" : runStatusLabel(node().status)}
              />
            </PressRow>
          )}
        </Each>
      </View>
    </If>
  );
}

/** The captain's own last answer, when the ledger has one. It is a quote of the
 * session, not a generated summary the server never produced. */
function captainSummaryText(runId: string): string | undefined {
  const events = botAccess.nodeEvents()[runId];
  if (!events) return undefined;
  for (let index = events.length - 1; index >= 0; index -= 1) {
    const event = events[index];
    if (event.kind !== "assistant.message") continue;
    const payload = event.payload as { content?: Array<{ type?: string; text?: string }> } | undefined;
    const text = (payload?.content ?? [])
      .filter((block) => block.type === "text" && typeof block.text === "string")
      .map((block) => block.text as string)
      .join("")
      .trim();
    if (text) return text;
  }
  return undefined;
}

function CaptainSummary(props: { runId: string }) {
  const text = () => captainSummaryText(props.runId);
  return (
    <View style={{ flexDirection: "column", gap: space.xs, maxWidth: 820 }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
        <Text style={{ fontSize: typeScale.section, fontWeight: weight.semibold, color: color.foreground }}>队长小结</Text>
        <Hairline style={{ flex: 1 }} />
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>来自会话最后一条回答</Text>
      </View>
      <If
        when={text() !== undefined}
        else={<Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>这次运行还没有可读的队长小结。</Text>}
      >
        <Text style={{ fontSize: typeScale.base, lineHeight: Math.round(typeScale.base * lineHeight.normal), color: color.foreground }}>
          {text()}
        </Text>
      </If>
    </View>
  );
}

function RunNotes(props: { run: RunProjection }) {
  return (
    <If when={props.run.gaps.length > 0}>
      <View testID="run-notes" style={{ flexDirection: "column", gap: space.xs, maxWidth: 820 }}>
        <StateNote tone="info" icon="info" text="这份运行账只显示已经读到的部分；下面几处还没有接通。">
          <Each of={props.run.gaps} key={(gap) => gap}>
            {(gap) => (
              <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
                · {gap()}
              </Text>
            )}
          </Each>
        </StateNote>
      </View>
    </If>
  );
}

// ---- mobile bar / sheets / dialogs ---------------------------------------

function RunMobileBar(props: { isRunning: boolean; runId: string; onInspector: () => void; onStop: () => void }) {
  return (
    <View
      testID="run-mobile-bar"
      style={{
        flexDirection: "row",
        gap: space.sm,
        padding: space.md,
        borderTopWidth: 1,
        borderTopColor: color.border,
        backgroundColor: color.background,
      }}
    >
      <Button
        testID="run-inspector-open-mobile"
        variant="outline"
        size="lg"
        icon="activity"
        label="检查器"
        accessibilityLabel="打开检查器"
        onPress={props.onInspector}
        style={{ flex: 1, height: 44 }}
      />
      <If
        when={props.isRunning}
        else={
          <Button
            variant="outline"
            size="lg"
            icon="external"
            label="打开会话"
            accessibilityLabel="打开会话"
            onPress={() => {
              store.openChapter("workspace");
              void store.openSession(props.runId);
            }}
            style={{ flex: 1, height: 44 }}
          />
        }
      >
        <StopButton testID="run-stop-mobile" mobile onPress={props.onStop} />
      </If>
    </View>
  );
}

function RunInspectorSheet(props: { onStop: () => void }) {
  const node = () => botAccess.run()?.nodes.find((entry) => entry.id === botAccess.runSelected());
  return (
    <Sheet
      testID="run-inspector-sheet"
      open={inspectorOpen()}
      side="bottom"
      style={{ height: 620, maxHeight: "86%" }}
      title={node()?.name ?? "检查器"}
      description={node() ? shortSpan(node()!.startedAt, node()!.endedAt) : ""}
      onOpenChange={(event) => {
        if (!event.open) inspectorOpen.set(false);
      }}
      footer={
        <>
          <Button variant="outline" label="关闭" accessibilityLabel="关闭检查器" onPress={() => inspectorOpen.set(false)} style={touchStyle()} />
          <Button
            icon="external"
            label="打开会话"
            accessibilityLabel="打开会话"
            disabled={!node()?.sessionId}
            onPress={() => {
              const id = node()?.sessionId;
              if (!id) return;
              inspectorOpen.set(false);
              store.openChapter("workspace");
              void store.openSession(id);
            }}
            style={touchStyle()}
          />
        </>
      }
    >
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
        <View style={{ flexDirection: "column", gap: space.md }}>
          <InspectorTabs />
          <InspectorBody node={node()} />
        </View>
      </ScrollArea>
    </Sheet>
  );
}

function GraphSheet() {
  const run = () => botAccess.run();
  return (
    <Sheet
      testID="run-graph-sheet"
      open={graphSheetOpen()}
      side="bottom"
      style={{ height: 680, maxHeight: "92%" }}
      title="运行图"
      description="同一份账本的拓扑视角；节点按调用深度分层。"
      onOpenChange={(event) => {
        if (!event.open) {
          graphSheetOpen.set(false);
          botAccess.runMode.set("lanes");
        }
      }}
      footer={<Button label="关闭" accessibilityLabel="关闭图" onPress={() => { graphSheetOpen.set(false); botAccess.runMode.set("lanes"); }} style={touchStyle()} />}
    >
      <ScrollArea style={{ flex: 1, minHeight: 0 }}>
        <If when={run() !== null}>
          <RunGraph run={run()!} />
        </If>
      </ScrollArea>
    </Sheet>
  );
}

function StopDialog(props: { runId: string; teamId: string }) {
  return (
    <AlertDialog
      testID="run-stop-dialog"
      open={stopOpen()}
      title="停止这次运行？"
      description="队长和成员会收到停止信号；已完成的会话会保留，正在运行的调用可能被中断。"
      confirmLabel="停止运行"
      cancelLabel="继续运行"
      destructive
      onConfirm={() => {
        stopOpen.set(false);
        void botAccess.stopRun(props.runId).then(async (result) => {
          if (result) await botAccess.loadRun(props.teamId, props.runId);
        });
      }}
      onCancel={() => stopOpen.set(false)}
    />
  );
}

function RerunDialog(props: { teamId: string }) {
  return (
    <AlertDialog
      testID="run-rerun-dialog"
      open={rerunOpen()}
      title="再跑一次？"
      description="会用当前名单与使命启动一次新运行；会产生模型用量，运行页可以随时停止。"
      confirmLabel="再跑一次"
      cancelLabel="取消"
      onConfirm={() => {
        rerunOpen.set(false);
        void botAccess.startTeam(props.teamId).then((result) => {
          if (result?.sessionId) store.openTeamRun(props.teamId, result.sessionId);
        });
      }}
      onCancel={() => rerunOpen.set(false)}
    />
  );
}
