// The conversation's rows: user inputs, assistant work segments (thinking,
// grouped tool cards, Markdown), run summaries, compaction dividers,
// attachments and the approval card. Every row is a pure function of a
// `TurnView` / `DecisionView`; state that survives cell recycling lives in
// module signals keyed by turn/segment id.
import { Image, keymap, platform, signal, Text, View, windowSize } from "nsh";
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
import { formatDuration, formatTokens, preview } from "../data/wire";
import { Caret, Divider, IconButton, Press, Spinner, StatusDot } from "../ui/primitives";
import { CONTINUATION, fitCells, wrapCells } from "../ui/text";
import { Icon } from "../ui/icons";
import { alpha, colors, glass, radius, space } from "../ui/theme";

const expandedTools = signal<Record<string, boolean>>({});
const expandedThinking = signal<Record<string, boolean>>({});
const expandedToolsDetails = signal<Record<string, boolean>>({});
/** The terminal approval cursor, keyed by decision id. */
const approvalCursor = signal<Record<string, number>>({});

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

function UserInputRow(props: { input: InputView; attachments?: AttachmentRef[]; running?: boolean }) {
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
    </View>
  );
}

function ThinkingBlock(props: { turnId: string; text: string; running: boolean }) {
  const key = `${props.turnId}-thinking`;
  const open = () => isOpen(expandedThinking(), key, props.running);
  return (
    <View style={{ gap: space.xs }}>
      <Press
        onPress={() => toggle(expandedThinking, key, props.running)}
        accessibilityLabel="思考过程"
        style={{ flexDirection: "row", alignItems: "center", gap: 6, alignSelf: "flex-start", height: 28 }}
      >
        {props.running ? (
          <Spinner size={12} tint={colors.mutedForeground} />
        ) : (
          <Icon name="brain" size={13} tint={colors.mutedForeground} />
        )}
        <Text style={{ fontSize: 12, fontWeight: "500", color: colors.mutedForeground }}>
          {props.running ? "思考中…" : "思考过程"}
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

function toolTone(call: ToolCallView): "running" | "done" | "failed" {
  if (call.error) return "failed";
  return call.result !== undefined ? "done" : "running";
}

function ToolRow(props: { call: ToolCallView }) {
  const key = `tool-${props.call.toolCallId}`;
  const open = () => isOpen(expandedToolsDetails(), key, false);
  const tone = toolTone(props.call);
  const duration = props.call.finishedAt && props.call.startedAt
    ? formatDuration(props.call.finishedAt - props.call.startedAt)
    : "";
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
          {preview(props.call.args, 72)}
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
          <Text style={{ fontSize: 10.5, fontWeight: "700", color: colors.mutedForeground }}>参数</Text>
          <Text style={{ userSelect: "text", fontSize: 11.5, lineHeight: 17, color: colors.foreground }}>
            {preview(props.call.args, 1000) || "（无）"}
          </Text>
          {props.call.result !== undefined || props.call.error ? (
            <>
              <Text style={{ fontSize: 10.5, fontWeight: "700", color: colors.mutedForeground, marginTop: 4 }}>
                结果
              </Text>
              <Text
                style={{
                  userSelect: "text",
                  fontSize: 11.5,
                  lineHeight: 17,
                  color: props.call.error ? colors.danger : colors.foreground,
                }}
              >
                {props.call.error ?? preview(props.call.result, 1400)}
              </Text>
            </>
          ) : (
            <Text style={{ fontSize: 11.5, color: colors.mutedForeground }}>等待结果…</Text>
          )}
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
  const detail = () =>
    failed() > 0
      ? `${failed()} 个失败`
      : calls().length > 0 && done() === calls().length
        ? "全部完成"
        : "";
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
        <Text style={{ fontSize: 12, fontWeight: "600", color: colors.mutedForeground }}>
          {calls().length} 个工具
        </Text>
        {props.running ? (
          <Spinner size={11} tint={colors.mutedForeground} />
        ) : detail() ? (
          <Text style={{ fontSize: 11, color: failed() > 0 ? colors.danger : colors.mutedForeground }}>
            · {detail()}
          </Text>
        ) : null}
        <View style={{ flex: 1 }} />
        <View style={{ transform: open() ? "rotate(0deg)" : "rotate(-90deg)", transitionProperty: "transform", transitionDuration: 180 }}>
          <Icon name="chevron-down" size={12} tint={colors.mutedForeground} />
        </View>
      </Press>
      {open() ? (
        <View style={{ paddingBottom: 4 }}>
          {calls().map((call) => (
            <ToolRow call={call} />
          ))}
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
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: 6, paddingVertical: 4 }}>
      <StatusDot tone={tone} size={6} />
      <Text style={{ fontSize: 11.5, color: colors.mutedForeground, fontVariant: ["tabular-nums"] }}>
        {text()}
      </Text>
      {summary &&
      summary.stopReason &&
      summary.stopReason !== "done" &&
      summary.stopReason !== "end_turn" &&
      summary.stopReason !== "cancelled" &&
      summary.stopReason !== "canceled" ? (
        <Text style={{ fontSize: 10.5, color: colors.warning }}>{summary.stopReason}</Text>
      ) : null}

    </View>
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

export function TurnRow(props: {
  turn: () => TurnView;
  attachmentsFor: (requestId: string) => AttachmentRef[] | undefined;
  partialText: () => string | undefined;
  isLastRunning: () => boolean;
}) {
  const synthetic = () => {
    const turn = props.turn();
    return turn.origin === "manual_compact" && turn.visibleInputs.length === 0 && turn.workSegments.length === 0;
  };
  const streaming = () => {
    const turn = props.turn();
    return props.isLastRunning() && turn.status === "running" && !!props.partialText();
  };
  const waitingForWords = () => {
    const turn = props.turn();
    return (
      turn.status === "running" &&
      !turn.workSegments.some((segment) => segment.assistantText || segment.toolCalls.length || segment.assistantThinking) &&
      !streaming()
    );
  };
  const terminalPartial = () => {
    const text = props.partialText() ?? "";
    const budget = Math.max(40, Math.floor(windowSize().width / 8) - 8) * 2;
    return fitCells(text, budget);
  };
  return (
    <View style={{ paddingVertical: space.sm }}>
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
              input={input}
              attachments={props.attachmentsFor(input.requestId)}
              running={props.turn().status === "running"}
            />
          ))}
          <AssistantHeader ordinal={props.turn().ordinal} />
          <View style={{ gap: space.md, paddingLeft: 2 }}>
            {props.turn().workSegments.map((segment, index) => (
              <View style={{ gap: 10 }}>
                <If when={index > 0 && !!segment.triggerInput}>
                  <Divider label="引导生效 · 新工作段" tone="brand" />
                </If>
                <If when={!!segment.assistantThinking}>
                  <ThinkingBlock
                    turnId={props.turn().turnId}
                    text={segment.assistantThinking ?? ""}
                    running={
                      props.turn().status === "running" &&
                      index === props.turn().workSegments.length - 1 &&
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
                <If when={!!segment.assistantText}>
                  <If
                    when={platform.idiom === "terminal"}
                    else={<TextView markdown={segment.assistantText ?? ""} selectable options={{ streaming: false }} />}
                  >
                    <Text
                      style={{
                        userSelect: "text",
                        fontSize: 13.5,
                        lineHeight: 20,
                        color: colors.foreground,
                      }}
                    >
                      {wrapCells(segment.assistantText ?? "", Math.max(20, Math.floor(windowSize().width / 8) - 6))}
                    </Text>
                  </If>
                </If>
              </View>
            ))}
            <If when={streaming()}>
              <If
                when={platform.idiom === "terminal"}
                else={
                  <View style={{ gap: 6 }}>
                    <TextView
                      markdown={props.partialText() ?? ""}
                      selectable
                      options={{ streaming: true }}
                      streamingCaret={<Caret />}
                    />
                    <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>partial · 尚未提交</Text>
                  </View>
                }
              >
                <View style={{ gap: 4 }}>
                  <Text
                    style={{
                      userSelect: "text",
                      fontSize: 13,
                      lineHeight: 19,
                      numberOfLines: 3,
                      color: colors.mutedForeground,
                    }}
                  >
                    {terminalPartial()}
                  </Text>
                  <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>
                    partial · 尚未提交 · {CONTINUATION}
                  </Text>
                </View>
              </If>
            </If>
            <If when={waitingForWords()}>
              <View style={{ flexDirection: "row", alignItems: "center", gap: 8, height: 24 }}>
                <Spinner size={12} tint={colors.brand} />
                <Text style={{ fontSize: 12.5, color: colors.mutedForeground }}>正在思考…</Text>
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

export function QueuePanel(props: { queue: QueueItemView[]; onRemove?: (requestId: string) => void }) {
  return (
    <If when={props.queue.length > 0}>
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
            队列 · {props.queue.length}
          </Text>
          <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>当前任务结束后按顺序发送</Text>
        </View>
        {props.queue.map((item, index) => (
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, minHeight: 26 }}>
            <Text style={{ width: 16, fontSize: 10.5, color: colors.mutedForeground, fontVariant: ["tabular-nums"] }}>
              {index + 1}
            </Text>
            <If when={item.delivery === "guide"}>
              <View style={{ flexDirection: "row", alignItems: "center", gap: 3 }}>
                <Icon name="external" size={10} tint={colors.warning} />
                <Text style={{ fontSize: 10, fontWeight: "600", color: colors.warning }}>引导</Text>
              </View>
            </If>
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

export function GuideWaitingBanner(props: { visible: boolean; count: number; stopping?: boolean }) {
  const ink = () => (props.stopping ? colors.warning : colors.brand);
  return (
    <If when={props.visible}>
      <View
        style={{
          flexDirection: "row",
          alignItems: "center",
          gap: space.sm,
          paddingHorizontal: 12,
          height: 34,
          marginBottom: 6,
          borderRadius: radius.control,
          backgroundColor: props.stopping ? colors.warningTint : colors.brandMuted,
          borderWidth: 1,
          borderColor: alpha(ink(), 0.3),
        }}
      >
        {props.stopping ? (
          <Icon name="clock" size={12} tint={ink()} />
        ) : (
          <Spinner size={12} tint={ink()} />
        )}
        <Text style={{ fontSize: 12, fontWeight: "600", color: ink() }}>
          {props.stopping ? "停止中 · 引导转为队列" : "等待引导当前任务…"}
        </Text>
        <Text style={{ fontSize: 11, color: colors.mutedForeground }}>
          {props.stopping
            ? `${props.count} 条引导将在任务结束后入队`
            : props.count > 1
              ? `${props.count} 条引导将在工具轮边界注入`
              : "将在工具轮边界注入当前任务"}
        </Text>
      </View>
    </If>
  );
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
        <Text style={{ fontSize: 20, fontWeight: "700", color: colors.foreground }}>开始一段对话</Text>
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
            accessibilityLabel={text}
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
