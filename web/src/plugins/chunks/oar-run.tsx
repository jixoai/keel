// The OAR external-run card — change keel-oar-renderer.
//
// A builtin chunk contribution to `keel.ui.tool-renderer` (`toolName: "oar"`).
// The host reads the session's projection once and passes it in, so this file
// draws and nothing else: no fetch, no fold, no second record model
// (docs/OAR.md §6). Every colour, size and radius is the kit's; the terminal
// never reaches it (the host degrades `oar.record` to its text row there).
//
// What the card promises:
//   · the runtime and the `simulated` mark are always on the header — a fake
//     runtime is never presented as a live model run;
//   · every row is one folded projection item (a call and its result share a
//     row), with its agent path, kind/state, name, message/error and usage;
//   · long text is two lines here and whole in the row's expansion;
//   · the attribution only names the existing task card (`关联任务`) — it never
//     draws a card of its own.
import { signal, Text, View } from "nsh";
import type { JSX } from "nsh";
import {
  Badge,
  Button,
  Card,
  Separator,
  Spinner,
  color,
  font,
  iconSize,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../../ui/kit";
import { Icon, type IconName } from "../../ui/icons";
import {
  orderItems,
  projectionUsage,
  type ExternalProjectionView,
  type OarProjectionItemView,
  type OarRecordUsageView,
} from "../../data/oar-projection";
import type { PluginChunkContext } from "../types";

/** What the host passes: the session's projection and the one action the card
 * has. Both are the host's, so the chunk holds no transport and no store. */
export interface OarRunCardProps {
  sessionId: string;
  projection: ExternalProjectionView;
  /** Open the existing task card (the projection's `taskId`). */
  onOpenTask?: (taskId: string) => void;
}

/** A row's expansion, keyed by session + seq, so it survives the card's
 * re-render (the host re-mounts the chunk when its state changes). */
const expandedOarRows = signal<Record<string, boolean>>({});

const toggleRow = (key: string) => {
  const map = expandedOarRows();
  expandedOarRows.set({ ...map, [key]: !(map[key] ?? false) });
};

const rowOpen = (key: string): boolean => expandedOarRows()[key] ?? false;

// ---- words ----------------------------------------------------------------

const KIND_LABEL: Record<string, string> = {
  call: "调用",
  result: "结果",
  status: "状态",
  message: "消息",
  usage: "用量",
  error: "错误",
};

const kindLabel = (kind: string): string => KIND_LABEL[kind] ?? kind;

const STATE_LABEL: Record<string, string> = {
  running: "运行中",
  started: "已开始",
  completed: "完成",
  failed: "失败",
  cancelled: "已取消",
  degraded: "降级",
};

const stateLabel = (state: string): string => STATE_LABEL[state] ?? state;

const runtimeLabel = (runtime: string | undefined): string => runtime?.trim() || "未知运行时";

/** A row's tint family: a failure is red before its kind is even read. */
const itemTone = (item: OarProjectionItemView): BadgeVariant => {
  if (item.kind === "error" || item.error) return "destructive";
  switch (item.state) {
    case "completed":
      return "success";
    case "failed":
      return "destructive";
    case "cancelled":
      return "secondary";
    case "degraded":
      return "warning";
    case "running":
    case "started":
      return "info";
    default:
      return "secondary";
  }
};

const itemRunning = (item: OarProjectionItemView): boolean =>
  !item.error && item.state === "running";

const itemIcon = (item: OarProjectionItemView): IconName => {
  if (item.kind === "error" || item.error) return "alert";
  switch (item.state) {
    case "completed":
      return "check";
    case "failed":
      return "alert";
    case "cancelled":
      return "x";
    case "degraded":
      return "alert";
    case "running":
    case "started":
      return "play";
    default:
      break;
  }
  switch (item.kind) {
    case "message":
      return "message";
    case "usage":
      return "activity";
    case "status":
      return "clock";
    default:
      return "tool";
  }
};

const toneFace = (tone: BadgeVariant): string => {
  switch (tone) {
    case "success":
      return color.successTint;
    case "destructive":
      return color.dangerTint;
    case "warning":
      return color.warningTint;
    case "info":
      return color.infoTint;
    default:
      return color.muted;
  }
};

const toneInk = (tone: BadgeVariant): string => {
  switch (tone) {
    case "success":
      return color.success;
    case "destructive":
      return color.danger;
    case "warning":
      return color.warning;
    case "info":
      return color.info;
    default:
      return color.mutedForeground;
  }
};

// ---- values ---------------------------------------------------------------

const oneLine = (value: string, max: number): string => {
  const flat = value.replace(/\s+/g, " ").trim();
  return flat.length > max ? `${flat.slice(0, max - 1)}…` : flat;
};

const jsonText = (value: unknown): string => {
  if (value === undefined) return "";
  if (typeof value === "string") return value;
  try {
    const json = JSON.stringify(value, null, 2);
    return json ?? "";
  } catch {
    return String(value);
  }
};

const jsonOneLine = (value: unknown, max: number): string => {
  if (value === undefined) return "";
  if (typeof value === "string") return oneLine(value, max);
  try {
    const json = JSON.stringify(value);
    return json === undefined ? "" : oneLine(json, max);
  } catch {
    return oneLine(String(value), max);
  }
};

const tokenText = (value: number): string =>
  value >= 1000 ? `${(value / 1000).toFixed(1)}k` : String(value);

const usageLine = (usage: OarRecordUsageView): string => {
  const total = usage.totalTokens ?? (usage.inputTokens ?? 0) + (usage.outputTokens ?? 0);
  const parts: string[] = [];
  if (usage.inputTokens !== undefined) parts.push(`输入 ${tokenText(usage.inputTokens)}`);
  if (usage.outputTokens !== undefined) parts.push(`输出 ${tokenText(usage.outputTokens)}`);
  const body = parts.length ? `（${parts.join(" · ")}）` : "";
  const model = usage.model ? `${usage.model} · ` : "";
  return `${model}${tokenText(total)} tokens${body}`;
};

/** A row's short line and whether it is machine text (JSON, a path, a token
 * count) the mono face should carry. An error first (the sentence is the
 * point), then the record's own text, then its usage, then the result/argument
 * preview. */
interface ItemSummary {
  text: string;
  mono: boolean;
}

const itemSummary = (item: OarProjectionItemView): ItemSummary | null => {
  if (item.error) {
    const code = item.error.code ? `${item.error.code} · ` : "";
    return { text: oneLine(`${code}${item.error.message}`, 260), mono: false };
  }
  if (item.text) return { text: oneLine(item.text, 260), mono: false };
  if (item.usage) return { text: usageLine(item.usage), mono: true };
  if (item.output !== undefined) return { text: jsonOneLine(item.output, 220), mono: true };
  if (item.input !== undefined) return { text: jsonOneLine(item.input, 220), mono: true };
  return null;
};

interface DetailLine {
  label: string;
  value: string;
  mono?: boolean;
  danger?: boolean;
}

/** Everything the row knows, for the expansion: the wire ids first, then the
 * payloads. No field is invented; absent ones are absent. */
const itemDetails = (item: OarProjectionItemView): DetailLine[] => {
  const lines: DetailLine[] = [];
  lines.push({
    label: "seq",
    value: item.resultSeq !== undefined ? `${item.seq} → ${item.resultSeq}` : String(item.seq),
    mono: true,
  });
  if (item.callId) lines.push({ label: "callId", value: item.callId, mono: true });
  if (item.input !== undefined) lines.push({ label: "输入", value: jsonText(item.input), mono: true });
  if (item.output !== undefined) lines.push({ label: "输出", value: jsonText(item.output), mono: true });
  if (item.text) lines.push({ label: "文本", value: item.text });
  if (item.error) {
    const head = [item.error.code, item.error.message].filter(Boolean).join(" · ");
    const data = item.error.data !== undefined ? `\n${jsonText(item.error.data)}` : "";
    lines.push({ label: "错误", value: `${head}${data}`, danger: true });
  }
  if (item.usage) lines.push({ label: "用量", value: usageLine(item.usage) });
  return lines;
};

// ---- the pieces -----------------------------------------------------------

function AgentChip(props: { agent: ExternalProjectionView["agents"][number] }) {
  return (
    <View
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.xs,
        paddingHorizontal: space.xs,
        paddingVertical: space.xxs,
        borderRadius: radius.sm,
        backgroundColor: color.muted,
        borderWidth: 1,
        borderColor: color.border,
      }}
    >
      <Text style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.foreground }}>
        {props.agent.agentPath}
      </Text>
      <Text style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.mutedForeground }}>
        {`${props.agent.calls} 调用`}
      </Text>
      <If when={props.agent.failures > 0}>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.danger }}>
          {`${props.agent.failures} 失败`}
        </Text>
      </If>
    </View>
  );
}

/** One folded item. The row itself is the disclosure: a multi-line pressable
 * row (the shape the task panel's tree uses — the kit's `CollapsibleTrigger` is
 * a 32px single-line control and cannot hold three lines). The expansion holds
 * the item's whole record: the projection's fields, nothing re-folded. */
function OarItemRow(props: { item: OarProjectionItemView; sessionId: string }) {
  const key = () => `${props.sessionId}:${props.item.seq}`;
  const open = () => rowOpen(key());
  const details = () => itemDetails(props.item);
  const tone = () => itemTone(props.item);
  const summary = () => itemSummary(props.item);
  const expandable = () => details().length > 2 || (!!summary() && (summary()?.text.length ?? 0) > 120);
  return (
    <View
      testID={`oar-run-row-${props.item.seq}`}
      role={expandable() ? "button" : "none"}
      focusable={expandable()}
      onPress={() => expandable() && toggleRow(key())}
      accessibilityLabel={`外部运行记录 ${props.item.seq} ${props.item.name ?? kindLabel(props.item.kind)}`}
      accessibilityState={{ expanded: open() }}
      hoverStyle={expandable() ? { backgroundColor: color.hover } : undefined}
      style={{
        flexDirection: "column",
        gap: space.xs,
        minWidth: 0,
        paddingVertical: space.sm,
        // No horizontal pad: the row's icon aligns with the header's, and the
        // hover wash spans the card's content width exactly.
        paddingHorizontal: 0,
        borderRadius: radius.md,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm, minWidth: 0 }}>
        <View
          style={{
            width: 22,
            height: 22,
            borderRadius: radius.sm,
            alignItems: "center",
            justifyContent: "center",
            backgroundColor: toneFace(tone()),
          }}
        >
          {itemRunning(props.item) ? (
            <Spinner size={iconSize.xs} tint={color.brand} />
          ) : (
            <Icon name={itemIcon(props.item)} size={iconSize.xs} tint={toneInk(tone())} />
          )}
        </View>
        <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: 1 }}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs, minWidth: 0 }}>
            <Text
              style={{
                flexShrink: 1,
                minWidth: 0,
                fontSize: typeScale.sm,
                fontWeight: weight.semibold,
                color: color.foreground,
                numberOfLines: 1,
              }}
            >
              {props.item.name ?? kindLabel(props.item.kind)}
            </Text>
            <If when={!!props.item.state}>
              <Badge variant={tone()} label={stateLabel(props.item.state ?? "")} />
            </If>
          </View>
          <Text
            style={{
              flexShrink: 1,
              minWidth: 0,
              fontFamily: font.mono,
              fontSize: typeScale.xs,
              color: color.mutedForeground,
              numberOfLines: 1,
            }}
          >
            {`${kindLabel(props.item.kind)} · ${props.item.agentPath} · seq ${props.item.seq}`}
          </Text>
          <If when={!!summary()}>
            <Text
              style={{
                flexShrink: 1,
                minWidth: 0,
                fontFamily: summary()?.mono ? font.mono : undefined,
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.snug),
                color: props.item.error ? color.danger : color.mutedForeground,
                numberOfLines: 2,
              }}
            >
              {summary()?.text}
            </Text>
          </If>
        </View>
        <If when={expandable()}>
          <Icon
            name={open() ? "chevron-down" : "chevron-right"}
            size={iconSize.sm}
            tint={color.mutedForeground}
          />
        </If>
      </View>
      <If when={open()}>
        <View
          style={{
            gap: space.xs,
            padding: space.sm,
            borderRadius: radius.md,
            backgroundColor: color.muted,
            borderWidth: 1,
            borderColor: color.border,
          }}
        >
          {details().map((line) => (
            <View style={{ flexDirection: "row", gap: space.sm, alignItems: "flex-start", minWidth: 0 }}>
              <Text
                style={{
                  width: 48,
                  fontSize: typeScale.xs,
                  fontWeight: weight.bold,
                  color: color.mutedForeground,
                }}
              >
                {line.label}
              </Text>
              <Text
                style={{
                  flex: 1,
                  minWidth: 0,
                  userSelect: "text",
                  fontFamily: line.mono ? font.mono : undefined,
                  fontSize: typeScale.xs,
                  lineHeight: Math.round(typeScale.xs * lineHeight.snug),
                  color: line.danger ? color.danger : color.foreground,
                }}
              >
                {line.value}
              </Text>
            </View>
          ))}
        </View>
      </If>
    </View>
  );
}

// ---- the card -------------------------------------------------------------

function OarRunCard(props: OarRunCardProps) {
  const projection = () => props.projection;
  const items = () => orderItems(projection().items);
  const usage = () => projectionUsage(projection());
  // A fake runtime is never shown as live: the mark is on the header even if a
  // malformed projection forgot `simulated` but named the fake adapter.
  const simulated = () => projection().simulated === true || projection().runtime === "fake";
  return (
    <Card
      testID="oar-run-card"
      accessibilityLabel={`外部运行（${runtimeLabel(projection().runtime)}${simulated() ? " · 模拟运行时" : ""}）`}
      style={{ gap: space.md }}
    >
      {/* The header: what ran, whether it was a live model, how much it used. */}
      <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm, minWidth: 0 }}>
        <View
          style={{
            width: 24,
            height: 24,
            borderRadius: radius.sm,
            alignItems: "center",
            justifyContent: "center",
            backgroundColor: color.brandTint,
          }}
        >
          <Icon name="terminal" size={iconSize.md} tint={color.brand} />
        </View>
        <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: 1 }}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs, flexWrap: "wrap" }}>
            <Text
              style={{
                fontFamily: font.mono,
                fontSize: typeScale.key,
                letterSpacing: 0.6,
                color: color.mutedForeground,
              }}
            >
              OAR
            </Text>
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
              外部运行
            </Text>
            <Badge testID="oar-run-runtime" variant="outline" label={runtimeLabel(projection().runtime)} />
            <If when={simulated()}>
              <Badge testID="oar-run-simulated" variant="warning" icon="alert" label="模拟运行时" />
            </If>
          </View>
          <Text
            style={{
              fontFamily: font.mono,
              fontSize: typeScale.xs,
              color: color.mutedForeground,
              numberOfLines: 1,
            }}
          >
            {`session ${projection().backendSessionId || "—"} · ${items().length} 条记录 · ${projection().agents.length} 个 agent`}
          </Text>
        </View>
        <If when={!!usage()}>
          <View style={{ flexDirection: "column", alignItems: "flex-end", gap: 1 }}>
            <Text
              style={{
                fontFamily: font.mono,
                fontSize: typeScale.sm,
                color: color.foreground,
                fontVariant: ["tabular-nums"],
              }}
            >
              {`${tokenText(usage()?.totalTokens ?? 0)} tokens`}
            </Text>
            <Text style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.mutedForeground }}>
              {`输入 ${tokenText(usage()?.inputTokens ?? 0)} · 输出 ${tokenText(usage()?.outputTokens ?? 0)}`}
            </Text>
          </View>
        </If>
      </View>

      <If when={projection().agents.length > 0}>
        <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.xs }}>
          {projection().agents.map((agent) => (
            <AgentChip agent={agent} />
          ))}
        </View>
      </If>

      <Separator />

      <If
        when={items().length > 0}
        else={
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            还没有外部运行记录：这次运行的 journal 是空的。
          </Text>
        }
      >
        <View testID="oar-run-items" style={{ flexDirection: "column", gap: space.xs }}>
          {items().map((item) => (
            <OarItemRow item={item} sessionId={props.sessionId} />
          ))}
        </View>
      </If>

      {/* Attribution is the existing card's, never a second one. */}
      <If when={!!projection().taskId}>
        <Separator />
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>关联任务</Text>
          <Text
            testID="oar-run-task"
            style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.foreground }}
          >
            {projection().taskId}
          </Text>
          <View style={{ flex: 1 }} />
          <If when={!!props.onOpenTask}>
            <Button
              testID="oar-run-open-task"
              variant="ghost"
              size="sm"
              icon="layers"
              label="打开任务卡"
              accessibilityLabel="打开关联的任务卡"
              onPress={() => {
                const taskId = projection().taskId;
                if (taskId) props.onOpenTask?.(taskId);
              }}
            />
          </If>
        </View>
      </If>
    </Card>
  );
}

/** The chunk's public entry: the host calls this inside a guarded getter, and
 * only for a ready projection. */
export function render(context: PluginChunkContext): JSX.Element {
  const props = context.props as OarRunCardProps | undefined;
  if (!props || !props.projection) {
    return (
      <Card testID="oar-run-card" accessibilityLabel="外部运行">
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          尚未读到外部运行投影。
        </Text>
      </Card>
    );
  }
  return <OarRunCard {...props} />;
}
