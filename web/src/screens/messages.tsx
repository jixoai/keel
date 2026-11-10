// The conversation's rows: user inputs, assistant work segments (thinking,
// grouped tool cards, Markdown), run summaries, compaction dividers,
// attachments and the approval card. Every row is a pure function of a
// `TurnView` / `DecisionView`; state that survives cell recycling lives in
// module signals keyed by turn/segment id.
//
// Every row is built from the kit (`web/src/ui/kit`): Card for a block, the
// Collapsible trigger for a fold's handle, Badge for a state token, Separator
// for the stream's rules, Button (ghost) for the turn's own actions, Radio for
// the approval options. No numeric font size, radius or z-index is written
// here — `typeScale` / `radius` / `iconSize` / `space` carry them, which is
// what the migration gate (`scripts/kit-imports.mjs` + review) reads for.
//
// Tool families are *derived* from the tool's own name (`./tool-families`):
// `ToolCallView` is a frozen contract with no family field, so the words and
// the counts the summary line says them with live in that pure module.
import { Clipboard, Image, type JSX, keymap, platform, signal, Text, View, windowSize } from "nsh";
import { Radio, RadioGroup as NativeRadioGroup, TextView } from "nsh/base";
import type {
  AttachmentRef,
  DecisionView,
  InputView,
  QueueItemView,
  RunSummary,
  ToolCallView,
  TurnStatus,
  TurnView,
  WorkSegment,
} from "../data/contract";
import { formatDuration, formatTime, formatTokens, preview } from "../data/wire";
import { store } from "../data/store";
import { Icon, type IconName } from "../ui/icons";
import { fitCells, wrapCells } from "../ui/text";
import {
  Avatar,
  Badge,
  Button,
  Card,
  Collapsible,
  CollapsibleTrigger,
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  IconButton,
  Separator,
  Spinner,
  alpha,
  color,
  disabledOpacity,
  font,
  iconSize,
  lineHeight,
  motion,
  radius,
  row,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../ui/kit";
import {
  FAMILY_ICON,
  FAMILY_LABEL,
  familyDetail,
  groupToolCalls,
  summarizeCalls,
  toolTone,
  type ToolFamily,
} from "./tool-families";

const expandedTools = signal<Record<string, boolean>>({});
const expandedThinking = signal<Record<string, boolean>>({});
const expandedToolsDetails = signal<Record<string, boolean>>({});
const expandedFamilies = signal<Record<string, boolean>>({});
const expandedSegments = signal<Record<string, boolean>>({});
/** The terminal approval cursor, keyed by decision id. */
const approvalCursor = signal<Record<string, number>>({});
/** Which copy control just copied (its short "已复制" confirmation). */
const copiedKey = signal<string | null>(null);
/** The turn the pointer is over: its action line becomes fully visible (C3). */
const hoveredTurn = signal<string | null>(null);
let copiedTimer: number | undefined;

const toggle = (mapSignal: typeof expandedTools, key: string, fallback = false) => {
  const map = mapSignal();
  const next = { ...map };
  next[key] = !(map[key] ?? fallback);
  mapSignal.set(next);
};

const isOpen = (map: Record<string, boolean>, key: string, fallback: boolean) =>
  map[key] ?? fallback;

const statusLabel = (status: TurnStatus): string =>
  status === "completed"
    ? "完成"
    : status === "running" || status === "settling"
      ? "运行中"
      : status === "interrupted"
        ? "已停止"
        : "失败";

/** A run state's tint family, as the kit's one status token. */
const statusVariant = (status: TurnStatus): BadgeVariant =>
  status === "completed"
    ? "success"
    : status === "running" || status === "settling"
      ? "info"
      : status === "failed"
        ? "destructive"
        : "secondary";

/** The state's tint as a dot: the kit's Badge carries one tint per variant and
 * an empty badge renders exactly that face (`label` absent). Keeps the run line
 * readable at a glance without a second dot language. */
function ToneDot(props: { variant: BadgeVariant }) {
  return (
    <Badge
      variant={props.variant}
      style={{ width: space.md, height: space.md, paddingHorizontal: space.none }}
    />
  );
}

function AttachmentChip(props: { item: AttachmentRef }) {
  const image = () => (props.item.mime ?? "").startsWith("image/") && !!props.item.url;
  const sizeLabel = () =>
    !props.item.size
      ? ""
      : props.item.size > 1024 * 1024
        ? `${(props.item.size / 1024 / 1024).toFixed(1)} MB`
        : `${Math.max(1, Math.round(props.item.size / 1024))} KB`;
  return (
    <Card size="sm" style={{ alignSelf: "flex-end", maxWidth: "100%", gap: space.sm, padding: space.sm }}>
      <If when={image()}>
        <Image
          src={props.item.url ?? ""}
          style={{
            width: 280,
            maxWidth: "100%",
            height: 158,
            borderRadius: radius.sm,
            objectFit: "cover",
            borderWidth: 1,
            borderColor: color.border,
          }}
        />
      </If>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, alignSelf: "flex-end" }}>
        <Icon name={image() ? "image" : "file"} size={iconSize.sm} tint={color.mutedForeground} />
        <Text style={{ numberOfLines: 1, fontSize: typeScale.sm, color: color.foreground }}>
          {props.item.name}
        </Text>
        <If when={!!props.item.size}>
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{sizeLabel()}</Text>
        </If>
      </View>
    </Card>
  );
}

/** The guide states as the kit's own tokens: a landed guide is done, a queued
 * one waits, an inert one is quiet. */
const guideVariant = (state: "consumed" | "pending" | "inert"): BadgeVariant =>
  state === "consumed" ? "success" : state === "pending" ? "warning" : "ghost";

function UserInputRow(props: { turnId: string; input: InputView; attachments?: AttachmentRef[]; running?: boolean }) {
  const guideState = (): "consumed" | "pending" | "inert" =>
    props.input.consumed ? "consumed" : props.running ? "pending" : "inert";
  const guideLabel = () =>
    guideState() === "consumed" ? "引导已生效" : guideState() === "pending" ? "引导排队中" : "引导未生效";
  return (
    <View style={{ alignItems: "flex-end", gap: space.sm, paddingVertical: space.sm }}>
      {props.attachments?.length ? (
        <View style={{ alignItems: "flex-end", gap: space.sm }}>
          {props.attachments.map((item) => (
            <AttachmentChip item={item} />
          ))}
        </View>
      ) : null}
      {/* The reader's own words: the kit's card face, with the bubble's corner
          cut to the small radius. */}
      <Card
        size="sm"
        style={{
          maxWidth: "86%",
          borderBottomRightRadius: radius.sm,
          paddingHorizontal: space.lg,
          paddingVertical: space.md,
          gap: space.none,
        }}
      >
        <Text
          style={{
            userSelect: "text",
            fontSize: typeScale.base,
            lineHeight: Math.round(typeScale.base * lineHeight.normal),
            color: color.foreground,
          }}
        >
          {props.input.text}
        </Text>
      </Card>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <If when={props.input.delivery === "guide"}>
          <Badge variant={guideVariant(guideState())} icon="external" label={guideLabel()} />
        </If>
        <If when={!!props.input.downgraded}>
          <Badge variant="warning" icon="clock" label="已降级为队列" />
        </If>
      </View>
      {/* A user's own message is copyable text too (C3). */}
      <ActionLine turnId={props.turnId} time={props.input.createdAt}>
        <CopyPress id={`input-${props.input.requestId}`} text={props.input.text} />
      </ActionLine>
    </View>
  );
}

/** The last non-empty line of a thinking text — the one-line summary the
 * collapsed block shows while the model is still working (C11). */
function lastLineOf(text: string, max = 48): string {
  const lines = text
    .split("\n")
    .map((line) => line.trim())
    .filter(Boolean);
  const line = lines[lines.length - 1] ?? "";
  return line.length > max ? `${line.slice(0, max - 1)}…` : line;
}

/** The settled thinking span in whole seconds, as the reference's copy says it
 * (`chat.reasoning.durationSeconds`: 持续了 {seconds} 秒): 6.4s reads 6 秒. */
const thinkingSeconds = (durationMs: number | undefined): number | undefined =>
  durationMs === undefined ? undefined : Math.max(1, Math.round(durationMs / 1000));

/** Thinking is folded by default — streaming shows one live summary line,
 * once settled the fold is titled with how long it took (C11). The fold is the
 * kit's Collapsible, so the chevron is the component's own. */
function ThinkingBlock(props: { turnId: string; text: string; running: boolean; durationMs?: number }) {
  const key = `${props.turnId}-thinking`;
  const open = () => isOpen(expandedThinking(), key, false);
  const label = () => {
    // The reference's own words (`chat.reasoning.thinking` / `.thought` /
    // `.durationFewSeconds` / `.durationSeconds`, zh-CN.ts:5374-5377): a
    // Chinese surface never mixes "Thinking" / "Thought · 6.4s" in.
    if (props.running) {
      const line = lastLineOf(props.text);
      return line ? `正在思考 · ${line}` : "正在思考";
    }
    const seconds = thinkingSeconds(props.durationMs);
    return seconds === undefined ? "思考 · 持续了几秒" : `思考 · 持续了 ${seconds} 秒`;
  };
  return (
    <Collapsible
      open={open()}
      accessibilityLabel="思考过程"
      content={
        <View style={{ flexDirection: "row", gap: space.sm, paddingLeft: space.xs }}>
          <View style={{ width: space.xxs, backgroundColor: color.border, borderRadius: radius.pill }} />
          <Text
            style={{
              flex: 1,
              userSelect: "text",
              fontSize: typeScale.sm,
              lineHeight: Math.round(typeScale.sm * lineHeight.relaxed),
              color: color.mutedForeground,
            }}
          >
            {props.text}
          </Text>
        </View>
      }
    >
      <CollapsibleTrigger
        accessibilityLabel="思考过程"
        expanded={open()}
        onPress={() => toggle(expandedThinking, key, false)}
      >
        {props.running ? (
          <Spinner size={iconSize.xs} tint={color.mutedForeground} />
        ) : (
          <Icon name="brain" size={iconSize.sm} tint={color.mutedForeground} />
        )}
        <Text
          style={{
            flex: 1,
            numberOfLines: 1,
            fontSize: typeScale.sm,
            fontWeight: weight.medium,
            color: color.mutedForeground,
          }}
        >
          {label()}
        </Text>
      </CollapsibleTrigger>
    </Collapsible>
  );
}

/** The streaming caret the kit has no piece for (its one gap in this file): a
 * 2px token bar that pulses where the text is arriving. `TextView` takes it as
 * an element, so it stays a decoration, not a component of its own. */
function StreamCaret() {
  return (
    <View
      style={{
        width: space.xxs,
        height: typeScale.lg,
        marginLeft: space.xxs,
        borderRadius: radius.pill,
        backgroundColor: color.brand,
        animationName: "pulse",
        animationDuration: 900,
        animationIterationCount: "infinite",
      }}
    />
  );
}

/** The one assistant-text renderer: a committed segment's text and the live
 * streaming preview are the same component on the same path, so the moment the
 * preview is committed nothing re-flows, re-wraps or re-styles (C7). The only
 * difference is the streaming caret. */
function AssistantText(props: { text: string; streaming: boolean }) {
  const termWidth = Math.max(20, Math.floor(windowSize().width / 8) - 6);
  return (
    <If
      when={platform.idiom === "terminal"}
      else={
        <TextView
          markdown={props.text}
          selectable
          options={{ streaming: props.streaming }}
          streamingCaret={props.streaming ? <StreamCaret /> : undefined}
        />
      }
    >
      <Text
        style={{
          userSelect: "text",
          fontSize: typeScale.base,
          lineHeight: Math.round(typeScale.base * lineHeight.normal),
          numberOfLines: props.streaming ? 3 : undefined,
          color: color.foreground,
        }}
      >
        {props.streaming
          ? fitCells(props.text, termWidth * 2)
          : wrapCells(props.text, termWidth)}
      </Text>
    </If>
  );
}

/** `service=api · timeoutMs=2000` — a call's arguments as one readable line,
 * never raw JSON as the main view (C9). */
function formatArgs(value: unknown, max = 96): string {
  if (value === undefined || value === null) return "";
  if (typeof value === "string") return preview(value, max);
  if (typeof value !== "object") return String(value);
  if (Array.isArray(value)) return preview(value, max);
  const parts: string[] = [];
  for (const [key, item] of Object.entries(value as Record<string, unknown>)) {
    const text =
      item === null || item === undefined ? "—" : typeof item === "object" ? preview(item, 40) : String(item);
    parts.push(`${key}=${text}`);
  }
  const joined = parts.join(" · ");
  return joined.length > max ? `${joined.slice(0, max - 1)}…` : joined;
}

/** The full arguments for the expanded detail (a string keeps its lines). */
function argumentBody(value: unknown): string {
  if (value === undefined || value === null) return "";
  if (typeof value === "string") return value;
  if (typeof value !== "object") return String(value);
  if (Array.isArray(value)) return preview(value, 600);
  const parts: string[] = [];
  for (const [key, item] of Object.entries(value as Record<string, unknown>)) {
    const text =
      item === null || item === undefined
        ? "—"
        : typeof item === "object"
          ? preview(item, 400)
          : String(item);
    parts.push(`${key}=${text}`);
  }
  return parts.join("\n");
}

/** One call inside a group: the kit's Collapsible row, its detail below. */
function ToolRow(props: { call: ToolCallView; waiting?: boolean }) {
  const key = `tool-${props.call.toolCallId}`;
  const open = () => isOpen(expandedToolsDetails(), key, false);
  const tone = toolTone(props.call);
  /** A call with no result while the conversation is parked on an open
   * question: the question is the activity, so no motion (C2). */
  const parked = () => tone === "running" && props.waiting === true;
  const duration = () =>
    props.call.finishedAt && props.call.startedAt
      ? formatDuration(props.call.finishedAt - props.call.startedAt)
      : "";
  const resultText = () =>
    props.call.error ?? (props.call.result === undefined ? "" : preview(props.call.result, 1400));
  const toneFace = () =>
    tone === "failed" ? color.dangerTint : tone === "done" ? color.successTint : color.muted;
  return (
    <Collapsible
      open={open()}
      accessibilityLabel={`工具 ${props.call.name}`}
      content={
        <View
          style={{
            gap: space.sm,
            padding: space.md,
            marginBottom: space.md,
            borderRadius: radius.md,
            backgroundColor: color.muted,
            borderWidth: 1,
            borderColor: color.border,
          }}
        >
          {/* Result first: what a call did matters more than what it was asked. */}
          <If
            when={props.call.result !== undefined || !!props.call.error}
            else={<Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>等待结果…</Text>}
          >
            <Text style={{ fontSize: typeScale.xs, fontWeight: weight.bold, color: color.mutedForeground }}>
              结果
            </Text>
            <Text
              style={{
                userSelect: "text",
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.snug),
                color: props.call.error ? color.danger : color.foreground,
              }}
            >
              {resultText()}
            </Text>
          </If>
          <Text
            style={{
              fontSize: typeScale.xs,
              fontWeight: weight.bold,
              color: color.mutedForeground,
              marginTop: space.xs,
            }}
          >
            参数
          </Text>
          <Text
            style={{
              userSelect: "text",
              fontSize: typeScale.xs,
              lineHeight: Math.round(typeScale.xs * lineHeight.snug),
              color: color.mutedForeground,
              fontFamily: font.mono,
            }}
          >
            {argumentBody(props.call.args) || "（无）"}
          </Text>
        </View>
      }
    >
      <CollapsibleTrigger
        accessibilityLabel={`工具 ${props.call.name}`}
        expanded={open()}
        onPress={() => toggle(expandedToolsDetails, key, false)}
      >
        <View
          style={{
            width: 22,
            height: 22,
            borderRadius: radius.sm,
            alignItems: "center",
            justifyContent: "center",
            backgroundColor: toneFace(),
          }}
        >
          {tone === "running" ? (
            parked() ? (
              <Icon name="clock" size={iconSize.xs} tint={color.warning} />
            ) : (
              <Spinner size={iconSize.xs} tint={color.brand} />
            )
          ) : (
            <Icon
              name={tone === "failed" ? "alert" : "check"}
              size={iconSize.xs}
              tint={tone === "failed" ? color.danger : color.success}
            />
          )}
        </View>
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
          {props.call.name}
        </Text>
        <Text style={{ flex: 1, numberOfLines: 1, fontSize: typeScale.sm, color: color.mutedForeground }}>
          {formatArgs(props.call.args, 72)}
        </Text>
        <If when={!!duration()}>
          <Text
            style={{ fontSize: typeScale.xs, color: color.mutedForeground, fontVariant: ["tabular-nums"] }}
          >
            {duration()}
          </Text>
        </If>
      </CollapsibleTrigger>
    </Collapsible>
  );
}

/** One family of a segment's calls, collapsed by default: the segment's own
 * summary line already says how many and how many failed (C9). The count sits
 * in the kit's badge, so the row reads as one summary + one state token. */
function ToolFamilyGroup(props: {
  groupId: string;
  family: ToolFamily;
  calls: ToolCallView[];
  waiting?: boolean;
}) {
  const key = `${props.groupId}-${props.family}`;
  const open = () => isOpen(expandedFamilies(), key, false);
  const failed = () => props.calls.filter((call) => toolTone(call) === "failed").length;
  return (
    <Collapsible
      open={open()}
      accessibilityLabel={`工具家族 ${FAMILY_LABEL[props.family]}`}
      content={
        <View style={{ paddingLeft: space.xxs, paddingBottom: space.xxs }}>
          {props.calls.map((call) => (
            <ToolRow call={call} waiting={props.waiting} />
          ))}
        </View>
      }
    >
      <CollapsibleTrigger
        icon={FAMILY_ICON[props.family]}
        label={FAMILY_LABEL[props.family]}
        expanded={open()}
        onPress={() => toggle(expandedFamilies, key, false)}
        accessibilityLabel={`工具家族 ${FAMILY_LABEL[props.family]}`}
      >
        <Badge
          variant={failed() > 0 ? "destructive" : "ghost"}
          label={`${props.calls.length} 个 · ${familyDetail(props.calls)}`}
        />
      </CollapsibleTrigger>
    </Collapsible>
  );
}

/** A segment's tool work as one card: the kit's Card face, the summary row as
 * the fold's handle, each family (or lone call) as a row inside. */
function ToolGroup(props: { segment: WorkSegment; running: boolean; waiting?: boolean }) {
  const key = props.segment.segmentId;
  const calls = () => props.segment.toolCalls;
  const open = () => isOpen(expandedTools(), key, true);
  const failed = () => calls().filter((call) => toolTone(call) === "failed").length;
  return (
    <Card size="sm" style={{ padding: space.sm, gap: space.none }}>
      <Collapsible
        open={open()}
        accessibilityLabel="工具调用组"
        content={
          <View style={{ paddingBottom: space.xxs }}>
            {groupToolCalls(calls()).map((item) =>
              item.kind === "family" ? (
                <ToolFamilyGroup
                  groupId={key}
                  family={item.family}
                  calls={item.calls}
                  waiting={props.waiting}
                />
              ) : (
                <ToolRow call={item.call} waiting={props.waiting} />
              ),
            )}
            {calls().length === 0 ? (
              <Text
                style={{
                  fontSize: typeScale.sm,
                  color: color.mutedForeground,
                  paddingVertical: space.md,
                }}
              >
                正在准备工具…
              </Text>
            ) : null}
          </View>
        }
      >
        <CollapsibleTrigger
          icon="tool"
          label={summarizeCalls(calls(), props.running)}
          expanded={open()}
          onPress={() => toggle(expandedTools, key, true)}
          accessibilityLabel="工具调用组"
        >
          <If when={props.running}>
            <Spinner size={iconSize.xs} tint={color.mutedForeground} />
          </If>
          <If when={!props.running && failed() > 0}>
            <Badge variant="destructive" icon="alert" label="需要留意" />
          </If>
        </CollapsibleTrigger>
      </Collapsible>
    </Card>
  );
}

/** The ledger's own line about a run: state, what it cost, and who answered —
 * the identity fact (provider/model) lines up with the model in settings. */
function RunSummaryRow(props: { status: TurnStatus; summary?: RunSummary }) {
  const summary = props.summary;
  const text = () => {
    if (!summary) return statusLabel(props.status);
    const duration = summary.durationMs ? ` · ${formatDuration(summary.durationMs)}` : "";
    const tokens =
      summary.tokensIn || summary.tokensOut
        ? ` · ↑${formatTokens(summary.tokensIn)} ↓${formatTokens(summary.tokensOut)}`
        : "";
    return `${statusLabel(props.status)}${duration}${tokens}`;
  };
  const identity = () => {
    if (!summary || (!summary.provider && !summary.model)) return "";
    return `${summary.provider ? `${summary.provider}/` : ""}${summary.model ?? ""}`;
  };
  /** An unusual end to a run that nevertheless completed (a truncated answer,
   * a tool round). A failure or a stop already says so in the status word. */
  const noteReason = () => {
    if (!summary || props.status !== "completed") return "";
    const reason = summary.stopReason;
    if (!reason || reason === "done" || reason === "end_turn" || reason === "cancelled" || reason === "canceled") return "";
    if (reason === "length" || reason === "max_tokens") return "达到输出上限";
    if (reason === "tool_use") return "工具轮继续";
    return reason;
  };
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, paddingVertical: space.xs }}>
      <ToneDot variant={statusVariant(props.status)} />
      <Text
        style={{ fontSize: typeScale.sm, color: color.mutedForeground, fontVariant: ["tabular-nums"] }}
      >
        {text()}
      </Text>
      <If when={!!noteReason()}>
        <Badge variant="warning" label={noteReason()} />
      </If>
      <If when={!!identity()}>
        <Badge variant="ghost" accessibilityLabel={identity()}>
          <Text
            style={{ fontSize: typeScale.sm, color: color.mutedForeground, fontFamily: font.mono }}
          >
            {identity()}
          </Text>
        </Badge>
      </If>
    </View>
  );
}

/** The one retry control: re-asks the turn's own input as a new commit
 * (`POST /v1/sessions/{id}/retry`), spinning while the server is working. */
function RetryPress(props: { turnId: string; compact?: boolean }) {
  const pending = () => store.retryingTurn() === props.turnId;
  return (
    <Button
      variant="ghost"
      size={props.compact ? "sm" : "default"}
      icon="refresh"
      loading={pending()}
      disabled={pending()}
      label={pending() ? "重试中" : "重试"}
      accessibilityLabel="重试本回合"
      onPress={() => void store.retryTurn(props.turnId)}
    />
  );
}

/** The ledger's own error line inside a turn (the actionable one is the banner
 * above the composer). */
function InlineError(props: { text: string }) {
  return (
    <Card
      style={{
        flexDirection: "row",
        alignItems: "flex-start",
        gap: space.sm,
        padding: space.md,
        backgroundColor: color.dangerTint,
        borderColor: alpha(color.danger, 0.3),
      }}
    >
      <Icon name="alert" size={iconSize.sm} tint={color.danger} />
      <Text
        style={{
          flex: 1,
          userSelect: "text",
          fontSize: typeScale.sm,
          lineHeight: Math.round(typeScale.sm * lineHeight.normal),
          color: color.danger,
        }}
      >
        {props.text}
      </Text>
    </Card>
  );
}

/** The turn-level failure banner (C6). It sits above the composer, carries the
 * authority's reason, can be closed, and can be retried when the turn has a
 * committed input to re-ask. The store keys dismissals by turn, so closing it
 * only silences this failure — reopening the session shows it again. */
export function TurnErrorBanner(props: { compact?: boolean }) {
  const error = () => {
    const row = store.turnError();
    return row && row.sessionId === store.activeId() ? row : null;
  };
  return (
    <If when={!!error()}>
      {/* The dock already pads its children by the column's `px-4`
          (`COMPOSER_DOCK_INSET_PX`, composer-dock.tsx:134): a second inset here
          would push the banner off the input shell it belongs to. */}
      <View
        style={{
          width: "100%",
          maxWidth: 880,
          alignSelf: "center",
          marginBottom: space.sm,
        }}
      >
        <Card
          accessibilityLabel="回合失败"
          style={{
            flexDirection: "row",
            alignItems: "center",
            gap: space.md,
            paddingVertical: space.sm,
            paddingLeft: space.lg,
            paddingRight: space.xs,
            backgroundColor: color.dangerTint,
            borderColor: alpha(color.danger, 0.32),
          }}
        >
          <Icon name="alert" size={iconSize.sm} tint={color.danger} />
          <View style={{ flex: 1, gap: space.xxs }}>
            <Text style={{ fontSize: typeScale.xs, fontWeight: weight.bold, color: color.danger }}>
              这次运行失败了
            </Text>
            <Text
              style={{
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.snug),
                color: color.foreground,
                userSelect: "text",
                numberOfLines: 3,
              }}
            >
              {error()?.text ?? ""}
            </Text>
          </View>
          <If when={!!error()?.turnId && !!error()?.retryable}>
            <RetryPress turnId={error()?.turnId ?? ""} compact={props.compact} />
          </If>
          <IconButton icon="x" label="关闭错误提示" onPress={() => store.dismissTurnError()} />
        </Card>
      </View>
    </If>
  );
}

/** A compaction, as the stream's one rule (the kit's Separator). */
function CompactionRow(props: { note?: string }) {
  return (
    <View style={{ paddingVertical: space.sm, gap: space.sm }}>
      <Separator label="上下文已压缩" />
      <If when={!!props.note}>
        <Text
          style={{
            alignSelf: "flex-start",
            maxWidth: 680,
            numberOfLines: 3,
            textOverflow: "ellipsis",
            fontSize: typeScale.sm,
            lineHeight: Math.round(typeScale.sm * lineHeight.normal),
            color: color.foreground,
          }}
        >
          {props.note ?? ""}
        </Text>
      </If>
    </View>
  );
}

function AssistantHeader(props: { ordinal: number }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, height: space["2xl"] }}>
      <Avatar
        size={20}
        accessibilityLabel=""
        fallback={<Icon name="spark" size={iconSize.xs} tint={color.brand} />}
        style={{ borderWidth: 1, borderColor: color.border }}
      />
      <Text style={{ fontSize: typeScale.sm, fontWeight: weight.bold, color: color.foreground }}>Keel</Text>
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>#{props.ordinal}</Text>
    </View>
  );
}

/** The ledger's own thinking window for a segment: from the segment's first
 * event to the first tool it triggered; a segment that never called a tool
 * falls back to its own span, then to the run's duration (C11). */
function thinkingDurationMs(turn: TurnView, segment: WorkSegment): number | undefined {
  const start = segment.startedAt;
  if (start !== undefined) {
    const toolStarts = segment.toolCalls
      .map((call) => call.startedAt)
      .filter((value): value is number => value !== undefined);
    if (toolStarts.length) {
      const first = Math.min(...toolStarts);
      return first > start ? first - start : undefined;
    }
    if (segment.endedAt !== undefined && segment.endedAt > start) return segment.endedAt - start;
  }
  if (!segment.toolCalls.length && (turn.runSummary?.durationMs ?? 0) > 0) {
    return turn.runSummary?.durationMs;
  }
  return undefined;
}

/** `12 秒` / `1 分 20 秒` — a work segment's own words (C10). */
function formatElapsed(ms: number): string {
  const seconds = Math.max(0, Math.round(ms / 1000));
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  return rest ? `${minutes} 分 ${rest} 秒` : `${minutes} 分`;
}

/** A segment's settled span from the ledger, with the run's duration as the
 * fallback for the single-run turn (the segment was the run). */
function segmentDurationMs(turn: TurnView, segment: WorkSegment, isLast: boolean): number | undefined {
  if (segment.startedAt !== undefined && segment.endedAt !== undefined && segment.endedAt > segment.startedAt) {
    return segment.endedAt - segment.startedAt;
  }
  if (isLast && (turn.runSummary?.durationMs ?? 0) > 0) return turn.runSummary?.durationMs;
  return undefined;
}

/** The first non-empty line of a segment — the preview a folded segment keeps. */
function firstLineOf(text: string | undefined, max = 64): string {
  const line = (text ?? "")
    .split("\n")
    .map((item) => item.trim())
    .find(Boolean) ?? "";
  return line.length > max ? `${line.slice(0, max - 1)}…` : line;
}

/** The one work-segment header: 工作中 N 秒 / 已工作 N 秒 / 已停止 (C10). It is
 * the fold's handle; the latest answer always renders outside the fold. A
 * segment with nothing to fold keeps the same row, without a dead chevron. */
function SegmentHeader(props: {
  label: () => string;
  preview: () => string;
  running: () => boolean;
  foldable: () => boolean;
  open: () => boolean;
  onToggle: () => void;
}) {
  const content = () => (
    <>
      {props.running() ? (
        <Spinner size={iconSize.xs} tint={color.mutedForeground} />
      ) : (
        <Icon name="clock" size={iconSize.sm} tint={color.mutedForeground} />
      )}
      <Text
        style={{
          fontSize: typeScale.sm,
          fontWeight: weight.semibold,
          color: color.mutedForeground,
          fontVariant: ["tabular-nums"],
        }}
      >
        {props.label()}
      </Text>
      <Text style={{ flex: 1, numberOfLines: 1, fontSize: typeScale.xs, color: color.mutedForeground }}>
        {props.preview()}
      </Text>
    </>
  );
  return props.foldable() ? (
    <CollapsibleTrigger
      accessibilityLabel={props.label()}
      expanded={props.open()}
      onPress={props.onToggle}
    >
      {content()}
    </CollapsibleTrigger>
  ) : (
    <View
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.sm,
        height: space["3xl"],
        paddingHorizontal: space.sm,
      }}
    >
      {content()}
    </View>
  );
}

/** Copy with a short in-place confirmation; a failure is never swallowed. */
function CopyPress(props: { id: string; text: string; label?: string }) {
  const copied = () => copiedKey() === props.id;
  const copy = () => {
    void Clipboard.writeText(props.text).then(
      () => {
        copiedKey.set(props.id);
        if (copiedTimer !== undefined) clearTimeout(copiedTimer);
        copiedTimer = setTimeout(() => {
          copiedKey.set(null);
          copiedTimer = undefined;
        }, 1600) as unknown as number;
      },
      (error: unknown) => {
        store.flash({
          tone: "danger",
          text: `复制失败：${error instanceof Error ? error.message : String(error)}`,
        });
      },
    );
  };
  return (
    <Button
      variant="ghost"
      size="sm"
      icon={copied() ? "check" : "file"}
      label={copied() ? "已复制" : props.label ?? "复制"}
      accessibilityLabel={props.label ?? "复制"}
      onPress={copy}
    />
  );
}

/** Touch screens and terminals have no hover: the action line just shows. */
const actionsAlwaysVisible = () =>
  platform.idiom === "terminal" || platform.idiom === "phone" || platform.idiom === "tv";

/** The quiet action line under a message: only a pointer over the turn (or a
 * platform without hover) reveals it — it reserves its place either way, so
 * nothing on screen jumps when it appears (C3). */
function ActionLine(props: { turnId: string; time?: number; children: JSX.Element | JSX.Element[] }) {
  const revealed = () => actionsAlwaysVisible() || hoveredTurn() === props.turnId;
  return (
    <View
      style={[
        row(),
        {
          gap: space.xxs,
          height: space["2xl"],
          opacity: revealed() ? 1 : 0,
          pointerEvents: revealed() ? "auto" : "none",
          transitionProperty: "opacity",
          transitionDuration: motion.base,
        },
      ]}
    >
      {props.children}
      <View style={{ flex: 1 }} />
      <If when={!!props.time}>
        <Text
          style={{ fontSize: typeScale.xs, color: color.mutedForeground, fontVariant: ["tabular-nums"] }}
        >
          {formatTime(props.time ?? 0)}
        </Text>
      </If>
    </View>
  );
}

export function TurnRow(props: {
  turn: () => TurnView;
  attachmentsFor: (requestId: string) => AttachmentRef[] | undefined;
  partialText: () => string | undefined;
  isLastRunning: () => boolean;
  /** The conversation is parked on an open question (C2): the approval card
   * is the current activity, so the turn shows no loading motion. */
  waiting: () => boolean;
  /** The newest turn: its answer is the one a reader may want again (C3). */
  isLast: () => boolean;
  /** The conversation panel's clock, read only while something is running. */
  now: () => number;
}) {
  const synthetic = () => {
    const turn = props.turn();
    if (turn.visibleInputs.length > 0) return false;
    // The projection always opens one segment; a maintenance turn is the one
    // whose segment holds no answer and no tool work.
    return turn.workSegments.every(
      (segment) => !segment.assistantText && !segment.assistantThinking && segment.toolCalls.length === 0,
    );
  };
  const streaming = () => {
    const turn = props.turn();
    return props.isLastRunning() && turn.status === "running" && !!props.partialText();
  };
  /** The streaming preview lands inside the last segment — the place the
   * committed text will occupy — and only floats free before any segment
   * exists (C7). */
  const streamInSegment = () => streaming() && props.turn().workSegments.length > 0;
  const waitingForWords = () => {
    const turn = props.turn();
    return (
      turn.status === "running" &&
      !props.waiting() &&
      !turn.workSegments.some((segment) => segment.assistantText || segment.toolCalls.length || segment.assistantThinking) &&
      !streaming()
    );
  };
  /** The turn's merged assistant text: what 复制 puts on the clipboard, tool
   * JSON excluded (C3). */
  const copyText = () => props.turn().assistantOutput ?? "";
  const settledTime = () => {
    const turn = props.turn();
    if (turn.status === "running" || turn.status === "settling") return undefined;
    const segments = turn.workSegments;
    for (let index = segments.length - 1; index >= 0; index -= 1) {
      const segment = segments[index]!;
      if (segment.endedAt !== undefined) return segment.endedAt;
      if (segment.startedAt !== undefined) return segment.startedAt;
    }
    const inputs = turn.visibleInputs;
    return inputs.length ? inputs[inputs.length - 1]!.createdAt : undefined;
  };
  /** A settled turn may be asked again: the newest one (the reader is not
   * happy with the answer) or a failed one (C3) — and only when it has a
   * committed input to re-ask. A maintenance turn (compaction) has none: the
   * server would refuse the retry, so the button must not be offered. */
  const hasCommittedInput = () => props.turn().visibleInputs.some((input) => input.seq !== undefined);
  const retryable = () =>
    props.turn().status !== "running" &&
    props.turn().status !== "settling" &&
    hasCommittedInput() &&
    (props.isLast() || props.turn().status === "failed");
  return (
    <View
      onMouseEnter={() => hoveredTurn.set(props.turn().turnId)}
      onMouseLeave={() => {
        if (hoveredTurn() === props.turn().turnId) hoveredTurn.set(null);
      }}
      // The row's own `px-4`: the reference's timeline content container pads
      // its rows 16px inside the column (`ConversationTimeline.tsx:2005`), and
      // the kit's message column carries the column inset/cap but no inner
      // pad, while `ComposerDock` does (`COMPOSER_DOCK_INSET_PX` 16,
      // composer-dock.tsx:52,134). Without it the rows and the composer below
      // them disagree by 16px.
      style={{ paddingHorizontal: space.xl, paddingVertical: space.sm }}
    >
      <If when={synthetic()}>
        <View style={{ paddingVertical: space.xs }}>
          {props.turn().postTurnEvents.map((event) =>
            event.kind === "compaction" ? <CompactionRow note={event.note} /> : null,
          )}
        </View>
      </If>
      <If when={!synthetic()}>
        <View style={{ gap: space.sm }}>
          {props.turn().visibleInputs.map((input) => (
            <UserInputRow
              turnId={props.turn().turnId}
              input={input}
              attachments={props.attachmentsFor(input.requestId)}
              running={props.turn().status === "running"}
            />
          ))}
          <AssistantHeader ordinal={props.turn().ordinal} />
          <View style={{ gap: space.md }}>
            {props.turn().workSegments.map((segment, index) => {
              const isLast = () => index === props.turn().workSegments.length - 1;
              const segmentKey = `${props.turn().turnId}:seg:${segment.segmentId}`;
              // Earlier segments fold once the turn moved past them; the last
              // one stays open, and its answer renders outside the fold anyway.
              const foldOpen = () => isOpen(expandedSegments(), segmentKey, isLast());
              const foldable = () => !!segment.assistantThinking || segment.toolCalls.length > 0;
              const segmentLabel = () => {
                const turn = props.turn();
                if (isLast() && turn.status === "running") {
                  // A parked question is not work in progress (C2): the clock
                  // is not running while the run waits on the human.
                  if (props.waiting()) return "等待确认";
                  const elapsed = segment.startedAt !== undefined ? formatElapsed(props.now() - segment.startedAt) : "";
                  return elapsed ? `工作中 ${elapsed}` : "工作中";
                }
                if (isLast() && turn.status === "interrupted") return "已停止";
                const duration = segmentDurationMs(turn, segment, isLast());
                return duration !== undefined ? `已工作 ${formatElapsed(duration)}` : "已工作";
              };
              const segmentPreview = () =>
                foldOpen()
                  ? ""
                  : firstLineOf(segment.assistantText) ||
                    firstLineOf(segment.assistantThinking) ||
                    (segment.toolCalls.length ? `${segment.toolCalls.length} 个工具` : "");
              const headerVisible = () =>
                props.turn().workSegments.length > 1 || (isLast() && props.turn().status === "running");
              return (
                <View style={{ gap: space.md }}>
                  <If when={index > 0 && !!segment.triggerInput}>
                    <Separator label="引导生效 · 新工作段" />
                  </If>
                  <Collapsible
                    open={foldOpen()}
                    content={
                      <View style={{ gap: space.md }}>
                        <If when={!!segment.assistantThinking}>
                          <ThinkingBlock
                            turnId={props.turn().turnId}
                            text={segment.assistantThinking ?? ""}
                            durationMs={thinkingDurationMs(props.turn(), segment)}
                            running={
                              props.turn().status === "running" &&
                              !props.waiting() &&
                              isLast() &&
                              !segment.assistantText &&
                              segment.toolCalls.length === 0
                            }
                          />
                        </If>
                        <If when={segment.toolCalls.length > 0}>
                          <ToolGroup
                            segment={segment}
                            waiting={props.waiting()}
                            running={
                              props.turn().status === "running" &&
                              !props.waiting() &&
                              segment.toolCalls.some((call) => call.result === undefined && !call.error)
                            }
                          />
                        </If>
                        <If when={!!segment.assistantText && !isLast()}>
                          <AssistantText text={segment.assistantText ?? ""} streaming={false} />
                        </If>
                      </View>
                    }
                  >
                    <If when={headerVisible()}>
                      <SegmentHeader
                        label={segmentLabel}
                        preview={segmentPreview}
                        running={() => isLast() && props.turn().status === "running" && !props.waiting()}
                        foldable={foldable}
                        open={foldOpen}
                        onToggle={() => toggle(expandedSegments, segmentKey, isLast())}
                      />
                    </If>
                  </Collapsible>
                  {/* The latest answer is never inside the fold. */}
                  <If when={!!segment.assistantText && isLast()}>
                    <AssistantText text={segment.assistantText ?? ""} streaming={false} />
                  </If>
                  <If when={streamInSegment() && isLast()}>
                    <AssistantText text={props.partialText() ?? ""} streaming />
                  </If>
                </View>
              );
            })}
            <If when={streaming() && props.turn().workSegments.length === 0}>
              <AssistantText text={props.partialText() ?? ""} streaming />
            </If>
            {/* A failed turn keeps its reason in history: the banner above the
                composer is the actionable one, this is the ledger's own. */}
            <If when={props.turn().status === "failed" && !!props.turn().error}>
              <InlineError text={props.turn().error ?? ""} />
            </If>
            <If when={waitingForWords()}>
              {/* Loading is motion only (C7): no "thinking…" words. */}
              <View style={{ flexDirection: "row", alignItems: "center", height: space["2xl"] }}>
                <Spinner size={iconSize.xs} tint={color.brand} />
              </View>
            </If>
            {props.turn().postTurnEvents.map((event) => {
              if (event.kind === "compaction") return <CompactionRow note={event.note} />;
              if (event.kind === "assistant.error") {
                return <InlineError text={event.note ?? "模型返回错误"} />;
              }
              if (event.kind === "external.event") {
                return (
                  <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                    外部事件 · {event.note ?? event.kind}
                  </Text>
                );
              }
              return null;
            })}
            <If when={!!copyText() || retryable()}>
              {/* 复制/重试/时间 for the turn's answer (C3). */}
              <ActionLine turnId={props.turn().turnId} time={settledTime()}>
                <If when={!!copyText()}>
                  <CopyPress id={`turn-${props.turn().turnId}`} text={copyText()} label="复制回答" />
                </If>
                <If when={retryable()}>
                  <RetryPress turnId={props.turn().turnId} />
                </If>
              </ActionLine>
            </If>
            <If
              when={
                props.turn().status === "completed" ||
                props.turn().status === "interrupted" ||
                props.turn().status === "failed"
              }
            >
              <RunSummaryRow status={props.turn().status} summary={props.turn().runSummary} />
            </If>
          </View>
        </View>
      </If>
    </View>
  );
}

export function ApprovalCard(props: {
  decision: () => DecisionView;
  /** The last open decision takes the terminal key bindings. */
  active: () => boolean;
  timeoutAt: () => number | undefined;
  submitting: () => boolean;
  onAnswer: (decisionId: string, optionId: string) => void;
  now: () => number;
}) {
  const open = () => props.decision().status === "open";
  const answered = () => props.decision().status === "answered";
  const timedOut = () => props.decision().status === "timeout";
  const cancelled = () => props.decision().status === "cancelled";
  const expired = () => {
    const deadline = props.timeoutAt();
    return open() && deadline !== undefined && props.now() >= deadline;
  };
  const remaining = () => {
    const deadline = props.timeoutAt();
    if (deadline === undefined) return "";
    const left = Math.max(0, Math.round((deadline - props.now()) / 1000));
    return `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`;
  };
  const answerLabel = () => {
    const answer = props.decision().answer;
    if (!answer) return "";
    return props.decision().options.find((option) => option.optionId === answer)?.label ?? answer;
  };
  const withSubtitles = () => props.decision().options.every((option) => !!option.description);
  const rowMinHeight = () => (withSubtitles() ? 52 : 44);
  /** The keyboard cursor's option — undefined until the terminal's ↑↓ moved
   * it. It is what the radios call checked: the question has no answer until
   * the human gives one. */
  const cursor = (): number | undefined => {
    const decision = props.decision();
    const stored = approvalCursor()[decision.decisionId];
    return stored !== undefined && stored < decision.options.length ? stored : undefined;
  };
  /** What Enter answers: the cursor's option, else the recommendation (the
   * 推荐位), else the first. */
  const answerIndex = (): number => {
    const decision = props.decision();
    const at = cursor();
    if (at !== undefined) return at;
    const recommended = decision.options.findIndex((option) => option.optionId === decision.recommendation);
    return recommended >= 0 ? recommended : 0;
  };
  const moveCursor = (delta: number) => {
    const count = props.decision().options.length;
    if (!count) return;
    const next = (answerIndex() + delta + count) % count;
    approvalCursor.update((map) => ({ ...map, [props.decision().decisionId]: next }));
  };
  const answerCursor = () => {
    const option = props.decision().options[answerIndex()];
    if (option) props.onAnswer(props.decision().decisionId, option.optionId);
  };
  /** The rows are locked while the question is not answerable any more. */
  const locked = () => !open() || expired() || props.submitting();
  const keyboardReady = () => open() && !expired() && !props.submitting() && props.active();
  if (platform.idiom === "terminal") {
    keymap.bind("up", () => keyboardReady() && moveCursor(-1), { when: "notEditing" });
    keymap.bind("left", () => keyboardReady() && moveCursor(-1), { when: "notEditing" });
    keymap.bind("down", () => keyboardReady() && moveCursor(1), { when: "notEditing" });
    keymap.bind("right", () => keyboardReady() && moveCursor(1), { when: "notEditing" });
    keymap.bind("enter", () => keyboardReady() && answerCursor(), { when: "notEditing" });
  }

  const title = () => (open() ? "需要你的确认" : answered() ? "已确认" : timedOut() ? "已超时" : "已取消");
  /** Waiting — the card is parked on the human — is the warning family: this
   * theme has no `interaction-confirmation-*` pair (zcode's green waiting
   * treatment), so the warning tokens carry it. Registered deviation. */
  const waiting = () => open() && !expired();
  const icon = (): IconName => (waiting() ? "alert" : answered() ? "check" : "clock");
  const ink = () =>
    waiting() || timedOut() || cancelled() ? color.warning : color.mutedForeground;
  const fill = () =>
    waiting() || timedOut() || cancelled() ? color.warningTint : color.muted;

  return (
    // The row's `px-4`, as the turns have it: the message column carries the
    // column inset/cap but no inner pad, while the composer's dock carries 16
    // (`COMPOSER_DOCK_INSET_PX`), so the card keeps the same left/right edge.
    <Card
      accessibilityLabel={`审批 ${props.decision().question}`}
      style={[
        { gap: space.lg, marginHorizontal: space.xl },
        answered() ? { backgroundColor: color.muted } : null,
        waiting() ? { borderColor: alpha(color.warning, 0.36) } : null,
      ]}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <View
          style={{
            width: 26,
            height: 26,
            borderRadius: radius.md,
            alignItems: "center",
            justifyContent: "center",
            backgroundColor: fill(),
          }}
        >
          <Icon name={icon()} size={iconSize.sm} tint={ink()} />
        </View>
        <Text style={{ fontSize: typeScale.caption, fontWeight: weight.bold, color: color.foreground }}>
          {title()}
        </Text>
        <View style={{ flex: 1 }} />
        <If when={open() && props.timeoutAt() !== undefined}>
          {/* The deadline is the ledger's own (`store.decisionTimeout`); the
              waiting time is a token, not a progress bar. */}
          <Badge
            variant="warning"
            icon="clock"
            label={expired() ? "已超时" : `剩余 ${remaining()}`}
            accessibilityLabel={expired() ? "剩余时间已超时" : `剩余 ${remaining()}`}
          />
        </If>
      </View>
      <Text
        style={{
          userSelect: "text",
          fontSize: typeScale.base,
          lineHeight: Math.round(typeScale.base * lineHeight.normal),
          fontWeight: weight.semibold,
          color: answered() ? color.mutedForeground : color.foreground,
        }}
      >
        {props.decision().question}
      </Text>
      <If
        when={answered()}
        else={
          <If
            when={timedOut()}
            else={
              <If
                when={cancelled()}
                else={
                  <View style={{ gap: space.sm }}>
                    {/* The options are one set of radios — the base's own
                        group (role/checked for AT) with the app's children:
                        indicator, label, description, the recommendation. */}
                    <View
                      role="radiogroup"
                      accessibilityLabel={props.decision().question}
                      style={{ flexDirection: "column" }}
                    >
                      <NativeRadioGroup style={{ flexDirection: "column", gap: space.sm }}>
                        {props.decision().options.map((option, index) => {
                          const recommended = () => props.decision().recommendation === option.optionId;
                          /** The keyboard cursor is this option — the one radio
                           * of the set that is checked. `checked` follows the
                           * cursor and never a pre-answered option, because
                           * the base reports a press on a checked radio as
                           * nothing: a press here always answers (C4). */
                          const onCursor = () => cursor() === index;
                          return (
                            <Radio
                              checked={onCursor()}
                              disabled={locked()}
                              accessibilityLabel={option.label}
                              onChange={() => {
                                if (locked()) return;
                                approvalCursor.update((map) => ({
                                  ...map,
                                  [props.decision().decisionId]: index,
                                }));
                                props.onAnswer(props.decision().decisionId, option.optionId);
                              }}
                              hoverStyle={{ backgroundColor: color.hover }}
                              disabledStyle={{ opacity: disabledOpacity }}
                              style={[
                                {
                                  flexDirection: "row",
                                  alignItems: "center",
                                  gap: space.md,
                                  width: "100%",
                                  minHeight: rowMinHeight(),
                                  paddingHorizontal: space.lg,
                                  paddingVertical: space.md,
                                  borderRadius: radius.md,
                                  borderWidth: 1,
                                  backgroundColor: color.card,
                                },
                                onCursor()
                                  ? { borderColor: color.primary, backgroundColor: color.secondary }
                                  : { borderColor: color.border },
                              ]}
                            >
                              {/* A terminal draws the base's own radio; the
                                  other hosts get this one indicator. */}
                              <If when={platform.idiom !== "terminal"}>
                                <View
                                  style={{
                                    width: iconSize.lg,
                                    height: iconSize.lg,
                                    borderRadius: radius.pill,
                                    borderWidth: 1,
                                    borderColor: onCursor() ? color.primary : color.input,
                                    alignItems: "center",
                                    justifyContent: "center",
                                  }}
                                >
                                  <If when={onCursor()}>
                                    <View
                                      style={{
                                        width: space.md,
                                        height: space.md,
                                        borderRadius: radius.pill,
                                        backgroundColor: color.primary,
                                      }}
                                    />
                                  </If>
                                </View>
                              </If>
                              <View style={{ flex: 1, gap: space.xxs, justifyContent: "center" }}>
                                <Text
                                  style={{
                                    fontSize: typeScale.caption,
                                    lineHeight: Math.round(typeScale.caption * lineHeight.snug),
                                    fontWeight: weight.semibold,
                                    color: color.foreground,
                                  }}
                                >
                                  {option.label}
                                </Text>
                                <If when={withSubtitles()}>
                                  <Text
                                    style={{
                                      fontSize: typeScale.sm,
                                      lineHeight: Math.round(typeScale.sm * lineHeight.snug),
                                      color: color.mutedForeground,
                                    }}
                                  >
                                    {option.description ?? ""}
                                  </Text>
                                </If>
                              </View>
                              <If when={recommended()}>
                                <Badge variant="secondary" label="推荐" />
                              </If>
                            </Radio>
                          );
                        })}
                      </NativeRadioGroup>
                    </View>
                    <If when={props.submitting()}>
                      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
                        <Spinner size={iconSize.sm} tint={color.brand} />
                        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>提交中…</Text>
                      </View>
                    </If>
                    <If when={platform.idiom === "terminal"}>
                      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
                        ↑↓ 选择 · Enter 确认
                      </Text>
                    </If>
                  </View>
                }
              >
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>审批已取消。</Text>
              </If>
            }
          >
            <Text style={{ fontSize: typeScale.sm, color: color.warning }}>已超时，选项不再可用。</Text>
          </If>
        }
      >
        <View
          style={{
            flexDirection: "row",
            alignItems: "center",
            gap: space.md,
            paddingHorizontal: space.md,
            minHeight: space["3xl"],
            borderRadius: radius.md,
            backgroundColor: color.muted,
            alignSelf: "flex-start",
          }}
        >
          <Icon name="check" size={iconSize.sm} tint={color.mutedForeground} />
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
            {answerLabel() || "已选择"} · 已记录你的选择
          </Text>
        </View>
      </If>
    </Card>
  );
}

/** A guide that has not landed yet (C12). It is shown at the tail of the
 * stream — the place it will be injected — instead of a banner above the
 * composer, so a reader sees where the guidance is going to arrive. */
export function GuideWaitingRow(props: { item: QueueItemView; stopping: boolean }) {
  const ink = () => (props.stopping ? color.warning : color.brand);
  return (
    // The row's `px-4`, like the turns and the approval card beside it.
    <Card
      accessibilityLabel="等待引导当前任务"
      style={{
        alignSelf: "flex-start",
        maxWidth: "86%",
        marginHorizontal: space.xl,
        gap: space.sm,
        paddingHorizontal: space.lg,
        paddingVertical: space.md,
        backgroundColor: props.stopping ? color.warningTint : color.brandMuted,
        borderColor: alpha(ink(), 0.3),
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        {props.stopping ? (
          <Icon name="clock" size={iconSize.sm} tint={ink()} />
        ) : (
          <Spinner size={iconSize.sm} tint={ink()} />
        )}
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: ink() }}>
          {props.stopping ? "停止中 · 引导将转为队列" : "等待引导当前任务…"}
        </Text>
      </View>
      <Text
        style={{
          userSelect: "text",
          fontSize: typeScale.sm,
          lineHeight: Math.round(typeScale.sm * lineHeight.normal),
          numberOfLines: 3,
          color: color.foreground,
        }}
      >
        {props.item.text}
      </Text>
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
        {props.stopping ? "任务结束后按顺序入队" : "将在工具轮边界注入当前任务"}
      </Text>
    </Card>
  );
}

export function QueuePanel(props: { queue: QueueItemView[]; onRemove?: (requestId: string) => void }) {
  // Guides render at the stream's tail (C12); the panel keeps the queue.
  const items = () => props.queue.filter((item) => item.delivery !== "guide");
  return (
    <If when={items().length > 0}>
      <Card size="sm" accessibilityLabel="队列" style={{ gap: space.sm }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Icon name="clock" size={iconSize.sm} tint={color.mutedForeground} />
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.bold, color: color.mutedForeground }}>
            {`队列 · ${items().length}`}
          </Text>
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
            当前任务结束后按顺序发送
          </Text>
        </View>
        {items().map((item, index) => (
          <View
            style={{
              flexDirection: "row",
              alignItems: "center",
              gap: space.sm,
              minHeight: space["2xl"],
            }}
          >
            <Text
              style={{
                width: space.xl,
                fontSize: typeScale.xs,
                color: color.mutedForeground,
                fontVariant: ["tabular-nums"],
              }}
            >
              {index + 1}
            </Text>
            <Text style={{ flex: 1, numberOfLines: 1, fontSize: typeScale.sm, color: color.foreground }}>
              {item.text}
            </Text>
            <If when={!!props.onRemove}>
              <IconButton
                icon="x"
                label="移出队列"
                size="sm"
                onPress={() => props.onRemove?.(item.requestId)}
              />
            </If>
          </View>
        ))}
      </Card>
    </If>
  );
}

/** A greeting for the hour the panel was opened on (C14). */
function greetingFor(date: Date): string {
  const hour = date.getHours();
  if (hour >= 5 && hour < 11) return "早上好";
  if (hour >= 11 && hour < 13) return "中午好";
  if (hour >= 13 && hour < 18) return "下午好";
  if (hour >= 18 && hour < 23) return "晚上好";
  return "夜深了";
}

export function EmptyConversation(props: { onSuggestion: (text: string) => void }) {
  const suggestions = [
    "帮我总结一下这周的发布准备情况",
    "检查三个服务的健康状态",
    "把行动项按负责人分组",
  ];
  return (
    <Empty accessibilityLabel="空会话" style={{ flex: 1, justifyContent: "center" }}>
      <EmptyMedia icon="spark" tone="brand" />
      <View style={{ alignItems: "center", gap: space.sm }}>
        <EmptyTitle label={`${greetingFor(new Date())}，我是 Keel`} />
        <EmptyDescription label="发送一条消息，Keel 会在这里开始工作。" />
        <EmptyDescription label="运行中可以补充引导，需要时再处理审批。" />
      </View>
      <EmptyContent style={{ flexWrap: "wrap", justifyContent: "center", maxWidth: 560 }}>
        {suggestions.map((text) => (
          <Button
            variant="secondary"
            label={text}
            accessibilityLabel={`使用建议 ${text}`}
            onPress={() => props.onSuggestion(text)}
          />
        ))}
      </EmptyContent>
    </Empty>
  );
}
