// The workspace's right pane: the open session's running facts (change
// keel-harness-shell, C6 — the pane that `web/App.tsx` used to stand in for
// with a bare status capsule).
//
// What it shows, and where each fact comes from:
//
//   · the run state       `conversationState(store.conversation())` — the same
//                         vocabulary the header's badge uses, one word;
//   · the model           the session's own selection, else the tenant default
//                         (`selectionDisplay`: the display name, never the raw
//                         wire identity);
//   · the reasoning level that selection's level, in the pickers' words;
//   · the usage           the open view's run summaries added up;
//   · the tool calls      every work segment's tool calls, counted;
//   · the plugins         the assembled extension surface, once it is loaded
//                         (the pane asks for it when it opens).
//
// A row without a fact is not drawn: the pane never prints a placeholder for
// something it cannot read. With no session open it is an honest empty state,
// never an empty panel.
//
// NSH components run once: every reactive read sits in a getter the JSX calls
// (or in an `<If>`), never in a body-level local.
import { cleanup, effect, Text, View } from "nsh";
import { store } from "../data/store";
import { formatTokens, reasoningLabel, selectionDisplay } from "../data/wire";
import {
  Badge,
  ScrollArea,
  color,
  lineHeight,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../ui/kit";
import { conversationState, runStateLabel, runStateTone, type RunTone } from "../ui/run-state";

const stateBadge: Record<RunTone, BadgeVariant> = {
  neutral: "secondary",
  brand: "info",
  warning: "warning",
  danger: "destructive",
};

/** The connection in the reader's words. */
const connectionLabel = (status: string): string => {
  switch (status) {
    case "open":
      return "服务已连接";
    case "connecting":
      return "正在连接";
    case "reconnecting":
      return "连接中断，正在续接";
    default:
      return "未连接";
  }
};

/** One fact: its name above its value. */
function StatusFact(props: { label: string; value: string }) {
  return (
    <View style={{ gap: space.xxs }}>
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{props.label}</Text>
      <Text
        style={{
          fontSize: typeScale.sm,
          lineHeight: Math.round(typeScale.sm * lineHeight.normal),
          color: color.foreground,
          userSelect: "text",
        }}
      >
        {props.value}
      </Text>
    </View>
  );
}

/** The open session's model: the session's own input fact wins over the tenant
 * default, the way the header's chip reads it. */
const effectiveSelection = () => store.sessionSelection() ?? store.tenantSelection();

/** Tokens in/out, added across the open view's runs; `null` while no run has
 * reported usage. */
const usageTotals = () => {
  const turns = store.conversation()?.turns ?? [];
  let tokensIn = 0;
  let tokensOut = 0;
  let runs = 0;
  for (const turn of turns) {
    const summary = turn.runSummary;
    if (!summary) continue;
    tokensIn += summary.tokensIn;
    tokensOut += summary.tokensOut;
    runs += 1;
  }
  return runs > 0 ? { tokensIn, tokensOut } : null;
};

/** Tool calls across every work segment of the open view. */
const toolCallCount = () => {
  const turns = store.conversation()?.turns ?? [];
  let count = 0;
  for (const turn of turns) {
    for (const segment of turn.workSegments) count += segment.toolCalls.length;
  }
  return count;
};

export function SessionStatusPane() {
  // The plugin surface is part of this pane's facts; ask for it when the pane
  // opens (the store's own guard keeps this to one request while it is in
  // flight, and the settings page shares the same load).
  effect(() => {
    if (store.mode() !== "http") return;
    const timer = setTimeout(() => void store.loadPlugins(), 0);
    cleanup(() => clearTimeout(timer));
  });

  return (
    <View
      testID="session-status-pane"
      style={{ flex: 1, minHeight: 0, flexDirection: "column" }}
    >
      <View
        style={{
          flexShrink: 0,
          gap: space.xs,
          paddingHorizontal: space.xl,
          paddingVertical: space.lg,
          borderBottomWidth: 1,
          borderBottomColor: color.border,
        }}
      >
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
          <Text
            style={{
              flex: 1,
              fontSize: typeScale.base,
              fontWeight: weight.semibold,
              color: color.foreground,
            }}
          >
            会话状态
          </Text>
          <If when={!!store.activeId()}>
            <Badge
              testID="session-status-state"
              variant={stateBadge[runStateTone[conversationState(store.conversation())]]}
              label={runStateLabel[conversationState(store.conversation())]}
            />
          </If>
        </View>
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
          这些事实来自当前会话的运行记录。
        </Text>
      </View>

      <ScrollArea
        style={{ flex: 1, minHeight: 0 }}
        contentStyle={{ flexDirection: "column", padding: space.xl, gap: space.xl }}
        accessibilityLabel="会话状态详情"
      >
        <If
          when={!!store.activeId()}
          else={
            <View style={{ gap: space.xs }}>
              <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
                还没有打开会话
              </Text>
              <Text
                style={{
                  fontSize: typeScale.sm,
                  lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                  color: color.mutedForeground,
                }}
              >
                打开一个会话后，这里显示它的模型、用量与工具调用。
              </Text>
            </View>
          }
        >
          <If when={!!effectiveSelection()}>
            <StatusFact label="模型" value={selectionDisplay(effectiveSelection())} />
          </If>
          <If when={!!effectiveSelection()?.reasoningLevel}>
            <StatusFact
              label="思考档"
              value={reasoningLabel(effectiveSelection()?.reasoningLevel)}
            />
          </If>
          <StatusFact
            label="连接"
            value={connectionLabel(store.connection().status)}
          />
          <If when={usageTotals() !== null}>
            <StatusFact
              label="用量"
              value={`输入 ${formatTokens(usageTotals()?.tokensIn ?? 0)} · 输出 ${formatTokens(
                usageTotals()?.tokensOut ?? 0,
              )}`}
            />
          </If>
          <If when={toolCallCount() > 0}>
            <StatusFact label="工具调用" value={`${toolCallCount()} 次`} />
          </If>
          <If when={!!store.plugins()}>
            <StatusFact
              label="插件"
              value={`${(store.plugins()?.plugins ?? []).length} 个插件 · ${
                (store.plugins()?.tools ?? []).length
              } 个工具`}
            />
          </If>
        </If>
      </ScrollArea>
    </View>
  );
}
