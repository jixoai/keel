// The conversation's rows: user inputs, assistant work segments (thinking,
// grouped tool cards, Markdown), run summaries, compaction dividers,
// attachments and the approval card. Every row is a pure function of a
// `TurnView` / `DecisionView`; state that survives cell recycling lives in
// module signals keyed by turn/segment id.
import { Clipboard, Image, type JSX, keymap, platform, signal, Text, View, windowSize } from "nsh";
import { TextView } from "nsh/base";
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
import { Caret, Divider, IconButton, PillButton, Press, Spinner, StatusDot } from "../ui/primitives";
import { fitCells, wrapCells } from "../ui/text";
import { Icon, type IconName } from "../ui/icons";
import { alpha, colors, glass, mono, radius, space } from "../ui/theme";

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

const statusTone = (status: TurnStatus): "done" | "running" | "failed" | "idle" =>
  status === "completed" ? "done" : status === "running" ? "running" : status === "failed" ? "failed" : "idle";

function AttachmentChip(props: { item: AttachmentRef }) {
  const image = () => (props.item.mime ?? "").startsWith("image/") && !!props.item.url;
  return (
    <View style={{ gap: space.sm, alignItems: "flex-end" }}>
      {image() ? (
        <Image
          src={props.item.url!}
          style={{
            width: 280,
            maxWidth: "100%",
            height: 158,
            borderRadius: radius.inset,
            objectFit: "cover",
            borderWidth: 1,
            borderColor: colors.glassEdge,
          }}
        />
      ) : null}
      <View
        style={{
          flexDirection: "row",
          alignItems: "center",
          gap: space.sm,
          paddingHorizontal: 10,
          height: 34,
          alignSelf: "flex-end",
          maxWidth: "100%",
          borderRadius: radius.control,
          ...glass.control,
        }}
      >
        <Icon name={image() ? "image" : "file"} size={13} tint={colors.mutedForeground} />
        <Text style={{ numberOfLines: 1, fontSize: 12, color: colors.foreground }}>{props.item.name}</Text>
        {props.item.size ? (
          <Text style={{ fontSize: 11, color: colors.mutedForeground }}>
            {props.item.size > 1024 * 1024
              ? `${(props.item.size / 1024 / 1024).toFixed(1)} MB`
              : `${Math.max(1, Math.round(props.item.size / 1024))} KB`}
          </Text>
        ) : null}
      </View>
    </View>
  );
}

function UserInputRow(props: { turnId: string; input: InputView; attachments?: AttachmentRef[]; running?: boolean }) {
  const guideState = (): "consumed" | "pending" | "inert" =>
    props.input.consumed ? "consumed" : props.running ? "pending" : "inert";
  const guideLabel = () =>
    guideState() === "consumed" ? "引导已生效" : guideState() === "pending" ? "引导排队中" : "引导未生效";
  const guideFill = () =>
    guideState() === "consumed"
      ? colors.successTint
      : guideState() === "pending"
        ? colors.warningTint
        : colors.muted;
  const guideInk = () =>
    guideState() === "consumed"
      ? colors.success
      : guideState() === "pending"
        ? colors.warning
        : colors.mutedForeground;
  return (
    <View style={{ alignItems: "flex-end", gap: space.sm, paddingVertical: 6 }}>
      {props.attachments?.length ? (
        <View style={{ alignItems: "flex-end", gap: space.sm }}>
          {props.attachments.map((item) => (
            <AttachmentChip item={item} />
          ))}
        </View>
      ) : null}
      <View
        style={{
          maxWidth: "86%",
          paddingHorizontal: 14,
          paddingVertical: 10,
          borderRadius: radius.card,
          borderBottomRightRadius: 5,
          borderWidth: 1,
          borderColor: colors.glassStateRim,
          backgroundColor: colors.glassState,
          boxShadow: `inset 0 1px 0 ${colors.glassStateRim}`,
        }}
      >
        <Text style={{ userSelect: "text", fontSize: 14, lineHeight: 21, color: colors.foreground }}>
          {props.input.text}
        </Text>
      </View>
      <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
        {props.input.delivery === "guide" ? (
          <View
            style={{
              flexDirection: "row",
              alignItems: "center",
              gap: 4,
              paddingHorizontal: 6,
              height: 18,
              borderRadius: radius.pill,
              backgroundColor: guideFill(),
            }}
          >
            <Icon name="external" size={10} tint={guideInk()} />
            <Text style={{ fontSize: 10, fontWeight: "600", color: guideInk() }}>{guideLabel()}</Text>
          </View>
        ) : null}
        {props.input.downgraded ? (
          <View style={{ flexDirection: "row", alignItems: "center", gap: 4 }}>
            <Icon name="clock" size={10} tint={colors.warning} />
            <Text style={{ fontSize: 10, color: colors.warning }}>已降级为队列</Text>
          </View>
        ) : null}
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

/** Thinking is folded by default — streaming shows one live summary line,
 * once settled the fold is titled with how long it took (C11). */
function ThinkingBlock(props: { turnId: string; text: string; running: boolean; durationMs?: number }) {
  const key = `${props.turnId}-thinking`;
  const open = () => isOpen(expandedThinking(), key, false);
  const label = () => {
    if (props.running) {
      const line = lastLineOf(props.text);
      return line ? `Thinking · ${line}` : "Thinking";
    }
    const duration = formatDuration(props.durationMs);
    return duration ? `Thought · ${duration}` : "Thought";
  };
  return (
    <View style={{ gap: space.xs }}>
      <Press
        onPress={() => toggle(expandedThinking, key, false)}
        accessibilityLabel="思考过程"
        style={{ flexDirection: "row", alignItems: "center", gap: 6, alignSelf: "flex-start", height: 28 }}
      >
        {props.running ? (
          <Spinner size={12} tint={colors.mutedForeground} />
        ) : (
          <Icon name="brain" size={13} tint={colors.mutedForeground} />
        )}
        <Text
          style={{
            numberOfLines: 1,
            maxWidth: 480,
            fontSize: 12,
            fontWeight: "500",
            color: colors.mutedForeground,
          }}
        >
          {label()}
        </Text>
        <View
          style={{
            transform: open() ? "rotate(0deg)" : "rotate(-90deg)",
            transitionProperty: "transform",
            transitionDuration: 180,
          }}
        >
          <Icon name="chevron-down" size={12} tint={colors.mutedForeground} />
        </View>
      </Press>
      {open() ? (
        <View style={{ flexDirection: "row", gap: space.sm, paddingLeft: 5 }}>
          <View style={{ width: 2, borderRadius: 1, backgroundColor: alpha(colors.border, 1) }} />
          <Text
            style={{
              flex: 1,
              userSelect: "text",
              fontSize: 12.5,
              lineHeight: 19,
              color: colors.mutedForeground,
            }}
          >
            {props.text}
          </Text>
        </View>
      ) : null}
    </View>
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
          streamingCaret={props.streaming ? <Caret /> : undefined}
        />
      }
    >
      <Text
        style={{
          userSelect: "text",
          fontSize: 13.5,
          lineHeight: 20,
          numberOfLines: props.streaming ? 3 : undefined,
          color: colors.foreground,
        }}
      >
        {props.streaming
          ? fitCells(props.text, termWidth * 2)
          : wrapCells(props.text, termWidth)}
      </Text>
    </If>
  );
}

function toolTone(call: ToolCallView): "running" | "done" | "failed" {
  if (call.error) return "failed";
  return call.result !== undefined ? "done" : "running";
}

// ---- tool families (C9) ---------------------------------------------------

type ToolFamily = "explore" | "terminal" | "changes" | "other";

/** The words a tool name carries its family in (matched on `a.b_c-d` tokens). */
const FAMILY_WORDS: Record<Exclude<ToolFamily, "other">, string[]> = {
  terminal: ["shell", "bash", "sh", "zsh", "exec", "run", "terminal", "command", "cmd", "process", "spawn"],
  changes: [
    "edit", "write", "patch", "apply", "replace", "create", "delete", "remove", "mkdir",
    "move", "rename", "git", "commit", "diff", "build", "test", "install", "deploy", "publish",
  ],
  explore: [
    "read", "list", "search", "grep", "glob", "find", "open", "load", "fetch", "view", "show",
    "cat", "ls", "tree", "stat", "head", "tail", "inspect", "check", "health", "status",
    "context", "workspace", "calendar", "transcript", "web", "http",
  ],
};

function familyOf(name: string): ToolFamily {
  const tokens = name.toLowerCase().split(/[^a-z0-9]+/).filter(Boolean);
  const has = (words: string[]) => tokens.some((token) => words.includes(token));
  if (has(FAMILY_WORDS.terminal)) return "terminal";
  if (has(FAMILY_WORDS.changes)) return "changes";
  if (has(FAMILY_WORDS.explore)) return "explore";
  return "other";
}

const FAMILY_LABEL: Record<ToolFamily, string> = {
  explore: "探索",
  terminal: "终端",
  changes: "变更",
  other: "工具",
};

const FAMILY_ICON: Record<ToolFamily, IconName> = {
  explore: "search",
  terminal: "terminal",
  changes: "file",
  other: "tool",
};

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

function ToolRow(props: { call: ToolCallView }) {
  const key = `tool-${props.call.toolCallId}`;
  const open = () => isOpen(expandedToolsDetails(), key, false);
  const tone = toolTone(props.call);
  const duration = props.call.finishedAt && props.call.startedAt
    ? formatDuration(props.call.finishedAt - props.call.startedAt)
    : "";
  const argsLine = formatArgs(props.call.args, 72);
  const resultText = () =>
    props.call.error ?? (props.call.result === undefined ? "" : preview(props.call.result, 1400));
  return (
    <View style={{ borderTopWidth: 1, borderTopColor: colors.border }}>
      <Press
        onPress={() => toggle(expandedToolsDetails, key, false)}
        accessibilityLabel={`工具 ${props.call.name}`}
        style={{ flexDirection: "row", alignItems: "center", gap: space.sm, paddingVertical: 8, minHeight: 38 }}
        hoverStyle={{ backgroundColor: colors.hover }}
      >
        <View
          style={{
            width: 22,
            height: 22,
            borderRadius: 6,
            alignItems: "center",
            justifyContent: "center",
            backgroundColor:
              tone === "failed" ? colors.dangerTint : tone === "done" ? colors.successTint : colors.muted,
          }}
        >
          {tone === "running" ? (
            <Spinner size={11} tint={colors.brand} />
          ) : (
            <Icon
              name={tone === "failed" ? "alert" : "check"}
              size={12}
              tint={tone === "failed" ? colors.danger : colors.success}
            />
          )}
        </View>
        <Text style={{ fontSize: 12.5, fontWeight: "600", color: colors.foreground }}>{props.call.name}</Text>
        <Text style={{ flex: 1, numberOfLines: 1, fontSize: 11.5, color: colors.mutedForeground }}>
          {argsLine}
        </Text>
        {duration ? (
          <Text style={{ fontSize: 11, color: colors.mutedForeground, fontVariant: ["tabular-nums"] }}>
            {duration}
          </Text>
        ) : null}
        <Icon name="chevron-down" size={12} tint={colors.mutedForeground} />
      </Press>
      {open() ? (
        <View
          style={{
            gap: 6,
            padding: 10,
            marginBottom: 8,
            borderRadius: radius.control,
            backgroundColor: colors.muted,
            borderWidth: 1,
            borderColor: colors.glassEdge,
          }}
        >
          {/* Result first: what a call did matters more than what it was asked. */}
          {props.call.result !== undefined || props.call.error ? (
            <>
              <Text style={{ fontSize: 10.5, fontWeight: "700", color: colors.mutedForeground }}>结果</Text>
              <Text
                style={{
                  userSelect: "text",
                  fontSize: 11.5,
                  lineHeight: 17,
                  color: props.call.error ? colors.danger : colors.foreground,
                }}
              >
                {resultText()}
              </Text>
            </>
          ) : (
            <Text style={{ fontSize: 11.5, color: colors.mutedForeground }}>等待结果…</Text>
          )}
          <Text style={{ fontSize: 10.5, fontWeight: "700", color: colors.mutedForeground, marginTop: 4 }}>
            参数
          </Text>
          <Text
            style={{
              userSelect: "text",
              fontSize: 11,
              lineHeight: 16,
              color: colors.mutedForeground,
              fontFamily: mono(),
            }}
          >
            {argumentBody(props.call.args) || "（无）"}
          </Text>
        </View>
      ) : null}
    </View>
  );
}

/** One family of a segment's calls, collapsed by default: the segment's own
 * summary line already says how many and how many failed (C9). */
function ToolFamilyGroup(props: { groupId: string; family: ToolFamily; calls: ToolCallView[] }) {
  const key = `${props.groupId}-${props.family}`;
  const open = () => isOpen(expandedFamilies(), key, false);
  const failed = () => props.calls.filter((call) => toolTone(call) === "failed").length;
  const done = () => props.calls.filter((call) => toolTone(call) === "done").length;
  const pending = () => props.calls.length - failed() - done();
  const detail = () =>
    failed() > 0 ? `${failed()} 个失败` : pending() > 0 ? `${pending()} 个进行中` : "全部完成";
  return (
    <View style={{ borderTopWidth: 1, borderTopColor: colors.border }}>
      <Press
        onPress={() => toggle(expandedFamilies, key, false)}
        accessibilityLabel={`工具家族 ${FAMILY_LABEL[props.family]}`}
        style={{ flexDirection: "row", alignItems: "center", gap: space.sm, height: 34 }}
        hoverStyle={{ backgroundColor: colors.hover }}
      >
        <Icon name={FAMILY_ICON[props.family]} size={12} tint={colors.mutedForeground} />
        <Text style={{ fontSize: 12, fontWeight: "600", color: colors.foreground }}>
          {FAMILY_LABEL[props.family]}
        </Text>
        <Text style={{ fontSize: 11, color: failed() > 0 ? colors.danger : colors.mutedForeground }}>
          {props.calls.length} 个 · {detail()}
        </Text>
        <View style={{ flex: 1 }} />
        <View
          style={{
            transform: open() ? "rotate(0deg)" : "rotate(-90deg)",
            transitionProperty: "transform",
            transitionDuration: 180,
          }}
        >
          <Icon name="chevron-down" size={12} tint={colors.mutedForeground} />
        </View>
      </Press>
      {open() ? (
        <View style={{ paddingLeft: 2, paddingBottom: 2 }}>
          {props.calls.map((call) => (
            <ToolRow call={call} />
          ))}
        </View>
      ) : null}
    </View>
  );
}

function ToolGroup(props: { segment: WorkSegment; running: boolean }) {
  const key = props.segment.segmentId;
  const calls = () => props.segment.toolCalls;
  const open = () => isOpen(expandedTools(), key, true);
  const failed = () => calls().filter((call) => toolTone(call) === "failed").length;
  const done = () => calls().filter((call) => toolTone(call) === "done").length;
  const unfinished = () => calls().length - failed() - done();
  /** The line every tool block is read by: how many, how many failed. */
  const summary = () => {
    const total = calls().length;
    const families = new Set(calls().map((call) => familyOf(call.name)));
    const noun = families.size === 1 && families.has("terminal") ? "命令" : "工具";
    const failures = failed();
    if (failures > 0) return `${total} 个${noun}，${failures} 个失败`;
    if (!props.running && unfinished() > 0) return `${total} 个${noun}，${unfinished()} 个未完成`;
    if (total > 0 && done() === total) return `${total} 个${noun}，全部完成`;
    return `${total} 个${noun}`;
  };
  /** Families in first-appearance order, only those with two or more members;
   * everything else stays a standalone row (C9). */
  const items = () => {
    const groups = new Map<ToolFamily, ToolCallView[]>();
    for (const call of calls()) {
      const family = familyOf(call.name);
      const bucket = groups.get(family);
      if (bucket) bucket.push(call);
      else groups.set(family, [call]);
    }
    const emitted = new Set<ToolFamily>();
    const list: Array<{ kind: "family"; family: ToolFamily; calls: ToolCallView[] } | { kind: "call"; call: ToolCallView }> = [];
    for (const call of calls()) {
      const family = familyOf(call.name);
      const bucket = groups.get(family) ?? [];
      if (bucket.length < 2) {
        list.push({ kind: "call", call });
        continue;
      }
      if (emitted.has(family)) continue;
      emitted.add(family);
      list.push({ kind: "family", family, calls: bucket });
    }
    return list;
  };
  return (
    <View
      style={{
        borderRadius: radius.card,
        ...glass.card,
        overflow: "hidden",
        paddingHorizontal: 12,
        paddingTop: 2,
      }}
    >
      <Press
        onPress={() => toggle(expandedTools, key, true)}
        accessibilityLabel="工具调用组"
        style={{ flexDirection: "row", alignItems: "center", gap: space.sm, height: 38 }}
        hoverStyle={{ opacity: 0.86 }}
      >
        <View
          style={{
            width: 24,
            height: 24,
            borderRadius: 7,
            alignItems: "center",
            justifyContent: "center",
            backgroundColor: colors.muted,
          }}
        >
          <Icon name="tool" size={13} tint={colors.mutedForeground} />
        </View>
        <Text style={{ fontSize: 12, fontWeight: "600", color: colors.mutedForeground }}>{summary()}</Text>
        {props.running ? (
          <Spinner size={11} tint={colors.mutedForeground} />
        ) : failed() > 0 ? (
          <Text style={{ fontSize: 11, color: colors.danger }}>· 需要留意</Text>
        ) : null}
        <View style={{ flex: 1 }} />
        <View style={{ transform: open() ? "rotate(0deg)" : "rotate(-90deg)", transitionProperty: "transform", transitionDuration: 180 }}>
          <Icon name="chevron-down" size={12} tint={colors.mutedForeground} />
        </View>
      </Press>
      {open() ? (
        <View style={{ paddingBottom: 4 }}>
          {items().map((item) =>
            item.kind === "family" ? (
              <ToolFamilyGroup groupId={key} family={item.family} calls={item.calls} />
            ) : (
              <ToolRow call={item.call} />
            ),
          )}
          {calls().length === 0 ? (
            <Text style={{ fontSize: 12, color: colors.mutedForeground, paddingVertical: 10 }}>
              正在准备工具…
            </Text>
          ) : null}
        </View>
      ) : null}
    </View>
  );
}

function RunSummaryRow(props: { status: TurnStatus; summary?: RunSummary }) {
  const summary = props.summary;
  const tone = statusTone(props.status);
  const text = () => {
    if (!summary) return statusLabel(props.status);
    const duration = summary.durationMs ? ` · ${formatDuration(summary.durationMs)}` : "";
    const tokens =
      summary.tokensIn || summary.tokensOut
        ? ` · ↑${formatTokens(summary.tokensIn)} ↓${formatTokens(summary.tokensOut)}`
        : "";
    return `${statusLabel(props.status)}${duration}${tokens}`;
  };
  /** Who answered this run — the ledger's own identity fact, shown beside the
   * cost so it lines up with the model configured in settings. */
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
    <View style={{ flexDirection: "row", alignItems: "center", gap: 6, paddingVertical: 4 }}>
      <StatusDot tone={tone} size={6} />
      <Text style={{ fontSize: 11.5, color: colors.mutedForeground, fontVariant: ["tabular-nums"] }}>
        {text()}
      </Text>
      {noteReason() ? <Text style={{ fontSize: 10.5, color: colors.warning }}>{noteReason()}</Text> : null}
      {identity() ? (
        <Text style={{ fontSize: 10.5, color: colors.mutedForeground, fontFamily: mono() }}>{identity()}</Text>
      ) : null}
    </View>
  );
}

/** The one retry control: re-asks the turn's own input as a new commit
 * (`POST /v1/sessions/{id}/retry`), spinning while the server is working. */
function RetryPress(props: { turnId: string; compact?: boolean }) {  const pending = () => store.retryingTurn() === props.turnId;
  return (
    <Press
      onPress={pending() ? undefined : () => void store.retryTurn(props.turnId)}
      disabled={pending()}
      accessibilityLabel="重试本回合"
      hitSlop={props.compact ? 4 : undefined}
      hoverStyle={{ backgroundColor: colors.hover }}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: 5,
        height: 26,
        paddingHorizontal: 8,
        borderRadius: radius.control,
      }}
    >
      {pending() ? (
        <Spinner size={11} tint={colors.mutedForeground} />
      ) : (
        <Icon name="refresh" size={12} tint={colors.mutedForeground} />
      )}
      <Text style={{ fontSize: 11.5, fontWeight: "600", color: colors.mutedForeground }}>
        {pending() ? "重试中" : "重试"}
      </Text>
    </Press>
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
      <View
        style={{
          width: "100%",
          maxWidth: 880,
          alignSelf: "center",
          paddingHorizontal: props.compact ? 10 : 16,
          marginBottom: space.sm,
        }}
      >
        <View
          accessibilityLabel="回合失败"
          style={{
            flexDirection: "row",
            alignItems: "center",
            gap: space.sm,
            paddingLeft: 12,
            paddingRight: 4,
            paddingVertical: 8,
            borderRadius: radius.card,
            backgroundColor: colors.dangerTint,
            borderWidth: 1,
            borderColor: alpha(colors.danger, 0.32),
          }}
        >
          <Icon name="alert" size={14} tint={colors.danger} />
          <View style={{ flex: 1, gap: 2 }}>
            <Text style={{ fontSize: 11, fontWeight: "700", color: colors.danger }}>
              这次运行失败了
            </Text>
            <Text
              style={{
                fontSize: 11.5,
                lineHeight: 17,
                color: colors.foreground,
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
          <IconButton
            icon="x"
            label="关闭错误提示"
            size={28}
            iconSize={13}
            onPress={() => store.dismissTurnError()}
          />
        </View>
      </View>
    </If>
  );
}

function CompactionRow(props: { note?: string }) {
  return (
    <View style={{ paddingVertical: space.sm, gap: 6 }}>
      <Divider label="上下文已压缩" icon="layers" tone="warning" style={{ paddingVertical: space.xs }} />
      {props.note ? (
        <Text
          style={{
            alignSelf: "flex-start",
            maxWidth: 680,
            numberOfLines: 3,
            textOverflow: "ellipsis",
            fontSize: 12,
            lineHeight: 18,
            color: colors.foreground,
          }}
        >
          {props.note}
        </Text>
      ) : null}
    </View>
  );
}

function AssistantHeader(props: { ordinal: number }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, height: 24 }}>
      <View
        style={{
          width: 20,
          height: 20,
          borderRadius: 10,
          alignItems: "center",
          justifyContent: "center",
          backgroundColor: colors.muted,
          borderWidth: 1,
          borderColor: colors.border,
        }}
      >
        <Icon name="spark" size={11} tint={colors.brand} />
      </View>
      <Text style={{ fontSize: 12, fontWeight: "700", color: colors.foreground }}>Keel</Text>
      <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>#{props.ordinal}</Text>
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
 * the fold's handle; the latest answer always renders outside the fold. */
function SegmentHeader(props: {
  label: () => string;
  preview: () => string;
  running: () => boolean;
  foldable: () => boolean;
  open: () => boolean;
  onToggle: () => void;
}) {
  return (
    <Press
      onPress={props.foldable() ? props.onToggle : undefined}
      accessibilityLabel={props.label()}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: 6,
        height: 30,
        paddingHorizontal: 6,
        borderRadius: radius.control,
      }}
      hoverStyle={props.foldable() ? { backgroundColor: colors.hover } : undefined}
    >
      {props.running() ? (
        <Spinner size={11} tint={colors.mutedForeground} />
      ) : (
        <Icon name="clock" size={12} tint={colors.mutedForeground} />
      )}
      <Text
        style={{
          fontSize: 11.5,
          fontWeight: "600",
          color: colors.mutedForeground,
          fontVariant: ["tabular-nums"],
        }}
      >
        {props.label()}
      </Text>
      <Text style={{ flex: 1, numberOfLines: 1, fontSize: 11, color: colors.mutedForeground }}>
        {props.preview()}
      </Text>
      <If when={props.foldable()}>
        <View
          style={{
            transform: props.open() ? "rotate(0deg)" : "rotate(-90deg)",
            transitionProperty: "transform",
            transitionDuration: 180,
          }}
        >
          <Icon name="chevron-down" size={12} tint={colors.mutedForeground} />
        </View>
      </If>
    </Press>
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
    <Press
      onPress={copy}
      accessibilityLabel={props.label ?? "复制"}
      hoverStyle={{ backgroundColor: colors.hover }}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: 5,
        height: 26,
        paddingHorizontal: 8,
        borderRadius: radius.control,
      }}
    >
      <Icon name={copied() ? "check" : "file"} size={12} tint={copied() ? colors.success : colors.mutedForeground} />
      <Text
        style={{
          fontSize: 11.5,
          fontWeight: "600",
          color: copied() ? colors.success : colors.mutedForeground,
        }}
      >
        {copied() ? "已复制" : props.label ?? "复制"}
      </Text>
    </Press>
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
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: 2,
        height: 26,
        opacity: revealed() ? 1 : 0,
        pointerEvents: revealed() ? "auto" : "none",
        transitionProperty: "opacity",
        transitionDuration: 180,
      }}
    >
      {props.children}
      <View style={{ flex: 1 }} />
      {props.time ? (
        <Text style={{ fontSize: 10.5, color: colors.mutedForeground, fontVariant: ["tabular-nums"] }}>
          {formatTime(props.time)}
        </Text>
      ) : null}
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
    return turn.origin === "manual_compact" && turn.visibleInputs.length === 0 && turn.workSegments.length === 0;
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
   * happy with the answer) or a failed one (C3). */
  const retryable = () =>
    props.turn().status !== "running" &&
    props.turn().status !== "settling" &&
    (props.isLast() || props.turn().status === "failed");
  return (
    <View
      onMouseEnter={() => hoveredTurn.set(props.turn().turnId)}
      onMouseLeave={() => {
        if (hoveredTurn() === props.turn().turnId) hoveredTurn.set(null);
      }}
      style={{ paddingVertical: space.sm }}
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
          <View style={{ gap: space.md, paddingLeft: 2 }}>
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
              const headerVisible = () => props.turn().workSegments.length > 1 || (isLast() && props.turn().status === "running");
              return (
                <View style={{ gap: 10 }}>
                  <If when={index > 0 && !!segment.triggerInput}>
                    <Divider label="引导生效 · 新工作段" tone="brand" />
                  </If>
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
                  <If when={foldOpen()}>
                    <View style={{ gap: 10 }}>
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
                          running={
                            props.turn().status === "running" &&
                            segment.toolCalls.some((call) => call.result === undefined && !call.error)
                          }
                        />
                      </If>
                      <If when={!!segment.assistantText && !isLast()}>
                        <AssistantText text={segment.assistantText ?? ""} streaming={false} />
                      </If>
                    </View>
                  </If>
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
              <View
                style={{
                  flexDirection: "row",
                  alignItems: "flex-start",
                  gap: space.sm,
                  padding: 10,
                  borderRadius: radius.control,
                  backgroundColor: colors.dangerTint,
                  borderWidth: 1,
                  borderColor: alpha(colors.danger, 0.3),
                }}
              >
                <Icon name="alert" size={14} tint={colors.danger} />
                <Text style={{ flex: 1, userSelect: "text", fontSize: 12, lineHeight: 18, color: colors.danger }}>
                  {props.turn().error ?? ""}
                </Text>
              </View>
            </If>
            <If when={waitingForWords()}>
              {/* Loading is motion only (C7): no "thinking…" words. */}
              <View style={{ flexDirection: "row", alignItems: "center", height: 24 }}>
                <Spinner size={12} tint={colors.brand} />
              </View>
            </If>
            {props.turn().postTurnEvents.map((event) => {
              if (event.kind === "compaction") return <CompactionRow note={event.note} />;
              if (event.kind === "assistant.error") {
                return (
                  <View
                    style={{
                      flexDirection: "row",
                      alignItems: "flex-start",
                      gap: space.sm,
                      padding: 10,
                      borderRadius: radius.control,
                      backgroundColor: colors.dangerTint,
                      borderWidth: 1,
                      borderColor: alpha(colors.danger, 0.3),
                    }}
                  >
                    <Icon name="alert" size={14} tint={colors.danger} />
                    <Text style={{ flex: 1, userSelect: "text", fontSize: 12, lineHeight: 18, color: colors.danger }}>
                      {event.note ?? "模型返回错误"}
                    </Text>
                  </View>
                );
              }
              if (event.kind === "external.event") {
                return (
                  <Text style={{ fontSize: 11.5, color: colors.mutedForeground }}>
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
  const cursorIndex = () => {
    const decision = props.decision();
    const stored = approvalCursor()[decision.decisionId];
    if (stored !== undefined && stored < decision.options.length) return stored;
    const recommended = decision.options.findIndex((option) => option.optionId === decision.recommendation);
    return recommended >= 0 ? recommended : 0;
  };
  const moveCursor = (delta: number) => {
    const count = props.decision().options.length;
    if (!count) return;
    const next = (cursorIndex() + delta + count) % count;
    approvalCursor.update((map) => ({ ...map, [props.decision().decisionId]: next }));
  };
  const answerCursor = () => {
    const option = props.decision().options[cursorIndex()];
    if (option) props.onAnswer(props.decision().decisionId, option.optionId);
  };
  const keyboardReady = () => open() && !expired() && !props.submitting() && props.active();
  if (platform.idiom === "terminal") {
    keymap.bind("up", () => keyboardReady() && moveCursor(-1), { when: "notEditing" });
    keymap.bind("left", () => keyboardReady() && moveCursor(-1), { when: "notEditing" });
    keymap.bind("down", () => keyboardReady() && moveCursor(1), { when: "notEditing" });
    keymap.bind("right", () => keyboardReady() && moveCursor(1), { when: "notEditing" });
    keymap.bind("enter", () => keyboardReady() && answerCursor(), { when: "notEditing" });
  }

  const title = () => (open() ? "需要你的确认" : answered() ? "已确认" : timedOut() ? "已超时" : "已取消");
  const tone = () =>
    open() && !expired() ? "warning" : timedOut() || cancelled() ? "warning" : "neutral";
  const icon = () => (open() && !expired() ? "alert" : answered() ? "check" : "clock");
  const iconInk = () =>
    open() && !expired()
      ? colors.warning
      : timedOut() || cancelled()
        ? colors.warning
        : colors.mutedForeground;
  const iconFill = () =>
    open() && !expired()
      ? colors.warningTint
      : timedOut() || cancelled()
        ? colors.warningTint
        : colors.muted;

  return (
    <View
      style={{
        borderRadius: radius.card,
        ...(answered()
          ? { backgroundColor: colors.muted, borderWidth: 1, borderColor: colors.glassEdge }
          : glass.popover),
        padding: 16,
        gap: 12,
        borderColor: tone() === "warning" ? alpha(colors.warning, 0.36) : colors.glassEdge,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <View
          style={{
            width: 26,
            height: 26,
            borderRadius: 8,
            alignItems: "center",
            justifyContent: "center",
            backgroundColor: iconFill(),
          }}
        >
          <Icon name={icon()} size={14} tint={iconInk()} />
        </View>
        <Text style={{ fontSize: 13, fontWeight: "700", color: colors.foreground }}>{title()}</Text>
        <View style={{ flex: 1 }} />
        <If when={open() && props.timeoutAt() !== undefined}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: 4 }}>
            <Icon name="clock" size={11} tint={expired() ? colors.danger : colors.mutedForeground} />
            <Text
              style={{
                fontSize: 11,
                color: expired() ? colors.danger : colors.mutedForeground,
                fontVariant: ["tabular-nums"],
              }}
            >
              {expired() ? "已超时" : `剩余 ${remaining()}`}
            </Text>
          </View>
        </If>
      </View>
      <Text
        style={{
          userSelect: "text",
          fontSize: answered() ? 13 : 14.5,
          lineHeight: 21,
          fontWeight: "600",
          color: answered() ? colors.mutedForeground : colors.foreground,
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
                    {props.decision().options.map((option, index) => {
                      const recommended = () => props.decision().recommendation === option.optionId;
                      const onCursor = () => cursorIndex() === index;
                      const highlighted = () => recommended() || onCursor();
                      return (
                        <Press
                          onPress={() =>
                            open() && !expired() && !props.submitting()
                              ? props.onAnswer(props.decision().decisionId, option.optionId)
                              : undefined
                          }
                          disabled={!open() || expired() || props.submitting()}
                          accessibilityLabel={option.label}
                          accessibilityState={{ selected: onCursor() }}
                          hoverStyle={{ backgroundColor: colors.hover }}
                          activeStyle={{ backgroundColor: colors.glassStateArmed }}
                          style={{
                            width: "100%",
                            minHeight: rowMinHeight(),
                            flexDirection: "row",
                            alignItems: "center",
                            gap: 10,
                            paddingHorizontal: 12,
                            paddingVertical: 8,
                            borderRadius: radius.control,
                            borderWidth: 1,
                            borderColor: highlighted() ? alpha(colors.brand, 0.45) : colors.glassEdge,
                            backgroundColor: highlighted() ? colors.brandMuted : colors.glassControl,
                          }}
                        >
                          <View
                            style={{
                              width: 18,
                              height: 18,
                              borderRadius: 9,
                              borderWidth: 1.5,
                              borderColor: highlighted() ? colors.brand : colors.input,
                              alignItems: "center",
                              justifyContent: "center",
                            }}
                          >
                            <If when={highlighted()}>
                              <View style={{ width: 8, height: 8, borderRadius: 4, backgroundColor: colors.brand }} />
                            </If>
                          </View>
                          <View style={{ flex: 1, gap: 2, justifyContent: "center" }}>
                            <Text style={{ fontSize: 13, lineHeight: 18, fontWeight: "600", color: colors.foreground }}>
                              {option.label}
                            </Text>
                            <If when={withSubtitles()}>
                              <Text style={{ fontSize: 11.5, lineHeight: 16, color: colors.mutedForeground }}>
                                {option.description ?? ""}
                              </Text>
                            </If>
                          </View>
                          <If when={recommended()}>
                            <View
                              style={{
                                paddingHorizontal: 6,
                                height: 18,
                                borderRadius: radius.pill,
                                backgroundColor: colors.brandTint,
                                alignItems: "center",
                                justifyContent: "center",
                              }}
                            >
                              <Text style={{ fontSize: 10, fontWeight: "700", color: colors.brand }}>推荐</Text>
                            </View>
                          </If>
                        </Press>
                      );
                    })}
                    <If when={props.submitting()}>
                      <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
                        <Spinner size={11} tint={colors.brand} />
                        <Text style={{ fontSize: 11, color: colors.mutedForeground }}>提交中…</Text>
                      </View>
                    </If>
                    <If when={platform.idiom === "terminal"}>
                      <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>
                        ↑↓ 选择 · Enter 确认
                      </Text>
                    </If>
                  </View>
                }
              >
                <Text style={{ fontSize: 12.5, color: colors.mutedForeground }}>审批已取消。</Text>
              </If>
            }
          >
            <Text style={{ fontSize: 12.5, color: colors.warning }}>已超时，选项不再可用。</Text>
          </If>
        }
      >
        <View
          style={{
            flexDirection: "row",
            alignItems: "center",
            gap: 8,
            paddingHorizontal: 10,
            minHeight: 32,
            borderRadius: radius.control,
            backgroundColor: colors.glassControl,
            alignSelf: "flex-start",
          }}
        >
          <Icon name="check" size={13} tint={colors.mutedForeground} />
          <Text style={{ fontSize: 12.5, fontWeight: "600", color: colors.foreground }}>
            {answerLabel() || "已选择"} · 已记录你的选择
          </Text>
        </View>
      </If>
    </View>
  );
}

/** A guide that has not landed yet (C12). It is shown at the tail of the
 * stream — the place it will be injected — instead of a banner above the
 * composer, so a reader sees where the guidance is going to arrive. */
export function GuideWaitingRow(props: { item: QueueItemView; stopping: boolean }) {
  const ink = () => (props.stopping ? colors.warning : colors.brand);
  return (
    <View
      accessibilityLabel="等待引导当前任务"
      style={{
        alignSelf: "flex-start",
        maxWidth: "86%",
        gap: 5,
        paddingHorizontal: 12,
        paddingVertical: 8,
        borderRadius: radius.card,
        backgroundColor: props.stopping ? colors.warningTint : colors.brandMuted,
        borderWidth: 1,
        borderColor: alpha(ink(), 0.3),
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
        {props.stopping ? (
          <Icon name="clock" size={12} tint={ink()} />
        ) : (
          <Spinner size={12} tint={ink()} />
        )}
        <Text style={{ fontSize: 12, fontWeight: "600", color: ink() }}>
          {props.stopping ? "停止中 · 引导将转为队列" : "等待引导当前任务…"}
        </Text>
      </View>
      <Text
        style={{
          userSelect: "text",
          fontSize: 13,
          lineHeight: 19,
          numberOfLines: 3,
          color: colors.foreground,
        }}
      >
        {props.item.text}
      </Text>
      <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>
        {props.stopping ? "任务结束后按顺序入队" : "将在工具轮边界注入当前任务"}
      </Text>
    </View>
  );
}

export function QueuePanel(props: { queue: QueueItemView[]; onRemove?: (requestId: string) => void }) {
  // Guides render at the stream's tail (C12); the panel keeps the queue.
  const items = () => props.queue.filter((item) => item.delivery !== "guide");
  return (
    <If when={items().length > 0}>
      <View
        style={{
          borderRadius: radius.card,
          ...glass.card,
          paddingHorizontal: 12,
          paddingVertical: 8,
          gap: 6,
        }}
      >
        <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
          <Icon name="clock" size={12} tint={colors.mutedForeground} />
          <Text style={{ fontSize: 11, fontWeight: "700", color: colors.mutedForeground }}>
            队列 · {items().length}
          </Text>
          <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>当前任务结束后按顺序发送</Text>
        </View>
        {items().map((item, index) => (
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, minHeight: 26 }}>
            <Text style={{ width: 16, fontSize: 10.5, color: colors.mutedForeground, fontVariant: ["tabular-nums"] }}>
              {index + 1}
            </Text>
            <Text style={{ flex: 1, numberOfLines: 1, fontSize: 12, color: colors.foreground }}>
              {item.text}
            </Text>
            <If when={!!props.onRemove}>
              <IconButton
                icon="x"
                label="移出队列"
                size={22}
                iconSize={12}
                onPress={() => props.onRemove?.(item.requestId)}
              />
            </If>
          </View>
        ))}
      </View>
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
    <View style={{ flex: 1, alignItems: "center", justifyContent: "center", padding: 24, gap: 18 }}>
      <View
        style={{
          width: 56,
          height: 56,
          borderRadius: 18,
          alignItems: "center",
          justifyContent: "center",
          backgroundColor: colors.muted,
          borderWidth: 1,
          borderColor: colors.border,
        }}
      >
        <Icon name="spark" size={26} tint={colors.brand} />
      </View>
      <View style={{ alignItems: "center", gap: 6 }}>
        <Text style={{ fontSize: 20, fontWeight: "700", color: colors.foreground }}>
          {greetingFor(new Date())}，我是 Keel
        </Text>
        <Text style={{ maxWidth: 420, fontSize: 13, lineHeight: 19, color: colors.mutedForeground, textAlign: "center" }}>
          发送一条消息，Keel 会在这里开始工作。
        </Text>
        <Text style={{ maxWidth: 420, fontSize: 13, lineHeight: 19, color: colors.mutedForeground, textAlign: "center" }}>
          运行中可以补充引导，需要时再处理审批。
        </Text>
      </View>
      <View style={{ flexDirection: "row", flexWrap: "wrap", justifyContent: "center", gap: space.sm, maxWidth: 560 }}>
        {suggestions.map((text) => (
          <Press
            onPress={() => props.onSuggestion(text)}
            accessibilityLabel={`使用建议 ${text}`}
            hitSlop={6}
            hoverStyle={{ backgroundColor: colors.hover }}
            style={{
              paddingHorizontal: 12,
              height: 32,
              borderRadius: radius.pill,
              ...glass.control,
            }}
          >
            <Text style={{ fontSize: 12, color: colors.foreground }}>{text}</Text>
          </Press>
        ))}
      </View>
    </View>
  );
}
