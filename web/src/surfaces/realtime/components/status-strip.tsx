// Realtime surface · StatusStrip — one strip: what the call is doing, one
// sentence on what happens next, and the connection facts.
//
// Every phase has three channels of its own: a coloured dot that pulses only
// while something is actually live, a word (the badge), and the sentence under
// it. Colour is never the only channel — but it is a channel: 聆听/说话 must not
// read the same as 思考 or 出错 when someone glances at the strip.
//
// The clock is part of the state machine, not decoration: it only appears (and
// only moves) while a real session is up. A surface that never connected shows
// no running timer.
import { View, Text } from "nsh";
import { Badge, color, lineHeight, space, typeScale, weight, type BadgeVariant } from "../../../ui/kit";

/** The surface's states; the host's `ready|listening|thinking|speaking|
 * reconnecting|closed|error` map into these, and the surface adds the ones the
 * host never knows about (no model, no browser audio bridge, denied mic). */
export type RealtimePhase =
  | "idle"
  | "connecting"
  | "ready"
  | "listening"
  | "thinking"
  | "speaking"
  | "interrupted"
  | "reconnecting"
  | "error"
  | "closed"
  | "permission-denied"
  | "unsupported"
  | "no-session"
  | "no-provider";

export const PHASE_META: Record<RealtimePhase, { label: string; variant: BadgeVariant; hint: string }> = {
  idle: {
    label: "未开始",
    variant: "secondary",
    hint: "点「开始实时对话」建立双向语音会话：就绪后开口即可说话。",
  },
  connecting: {
    label: "连接中",
    variant: "warning",
    hint: "正在建立实时会话并申请麦克风；就绪后开口即可说话。",
  },
  ready: {
    label: "就绪",
    variant: "success",
    hint: "会话已就绪：麦克风持续收音，开口说话即可开始一轮。",
  },
  listening: {
    label: "聆听",
    variant: "info",
    hint: "正在接收你的语音并实时转写；停下后助手会自动回应。",
  },
  thinking: {
    label: "思考",
    variant: "info",
    hint: "助手正在组织回复，你可以随时开口打断或点「打断」。",
  },
  speaking: {
    label: "说话",
    variant: "success",
    hint: "助手语音正在边说边播；开口或点「打断」会立刻停止。",
  },
  interrupted: {
    label: "已打断",
    variant: "warning",
    hint: "本轮回复已停止，只保留打断前已经到达的内容；继续说下一句即可。",
  },
  reconnecting: {
    label: "重连中",
    variant: "warning",
    hint: "通道中断，正在重建连接；重连后是一段新的实时会话，断线前的转写不会重放。",
  },
  error: {
    label: "出错",
    variant: "destructive",
    hint: "实时会话出错了；下方显示了语音服务给出的原因，可以重试或结束通话。",
  },
  closed: {
    label: "已结束",
    variant: "secondary",
    hint: "通话已结束，麦克风已释放；可以再次开始一段新会话。",
  },
  "permission-denied": {
    label: "麦克风未授权",
    variant: "destructive",
    hint: "麦克风被拒绝：会话仍在，可以用下面的文字输入继续，助手语音仍会播放。",
  },
  unsupported: {
    label: "宿主不支持",
    variant: "warning",
    hint: "当前宿主没有浏览器音频桥（实时语音只在 web 构建可用），无法建立语音会话。",
  },
  "no-session": {
    label: "未打开会话",
    variant: "secondary",
    hint: "还没有打开会话：先打开一个会话，再开始实时对话。",
  },
  "no-provider": {
    label: "未配置语音模型",
    variant: "warning",
    hint: "没有可用的实时语音模型：在设置里配置一个，或从模型选择器里选择后重试。",
  },
};

/** Which phases are actually moving: only these pulse. A steady dot is a state,
 * a pulsing one is work in progress. */
const LIVE_PHASES: RealtimePhase[] = ["connecting", "listening", "thinking", "speaking", "reconnecting"];

export type MicState = "unknown" | "granted" | "denied" | "missing" | "unsupported";

/** The dot's ink. A separate scale from the badge variants because the dot is
 * the one channel that must separate 聆听 (brand), 思考 (info) and 说话
 * (success) at a glance. */
const dotInk = (phase: RealtimePhase): string => {
  switch (phase) {
    case "connecting":
    case "reconnecting":
    case "unsupported":
    case "no-provider":
      return color.warning;
    case "ready":
      return color.success;
    case "listening":
      return color.brand;
    case "thinking":
      return color.info;
    case "speaking":
      return color.success;
    case "interrupted":
      return color.warning;
    case "error":
    case "permission-denied":
      return color.danger;
    case "idle":
    case "closed":
    case "no-session":
    default:
      return color.mutedForeground;
  }
};

function PhaseDot(props: { phase: RealtimePhase }) {
  const live = () => LIVE_PHASES.includes(props.phase);
  return (
    <View
      testID="realtime-phase-dot"
      accessibilityLabel={`阶段指示：${PHASE_META[props.phase].label}`}
      style={{
        width: 10,
        height: 10,
        borderRadius: 5,
        flexShrink: 0,
        backgroundColor: dotInk(props.phase),
        opacity: props.phase === "idle" || props.phase === "closed" ? 0.45 : 1,
        animationName: live() ? "pulse" : "none",
        animationDuration: 1100,
        animationIterationCount: live() ? "infinite" : 1,
      }}
    />
  );
}

export function StatusStrip(props: {
  phase: RealtimePhase;
  /** The reason a state was entered (a denial, a service error, a reconnect
   * note). Replaces the generic next-step sentence, which would hide the why. */
  detail: string;
  active: boolean;
  muted: boolean;
  /** The microphone's permission/device state, not the call's phase: a denied
   * mic keeps the call up as a text-in/voice-out session. */
  mic: MicState;
  elapsed: string;
  connection: string;
  connectionOk: boolean;
}) {
  const meta = () => PHASE_META[props.phase];
  const hint = () => props.detail || meta().hint;
  const mic = () => {
    if (!props.active) return { label: "麦克风未开启", variant: "ghost" as BadgeVariant };
    if (props.muted) return { label: "麦克风已静音", variant: "secondary" as BadgeVariant };
    if (props.mic === "denied") return { label: "麦克风未授权", variant: "destructive" as BadgeVariant };
    if (props.mic === "missing") return { label: "无可用麦克风", variant: "destructive" as BadgeVariant };
    if (props.mic === "unsupported") return { label: "麦克风不可用", variant: "warning" as BadgeVariant };
    return { label: "麦克风开启 · 持续收音", variant: "ghost" as BadgeVariant };
  };
  return (
    <View
      testID="realtime-status-strip"
      style={{
        gap: space.sm,
        paddingHorizontal: space.xl,
        paddingVertical: space.md,
        borderBottomWidth: 1,
        borderBottomColor: color.border,
        flexShrink: 0,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
        {PhaseDot({ phase: props.phase })}
        <Text style={{ fontSize: typeScale.caption, fontWeight: weight.semibold, color: color.foreground }}>
          实时语音
        </Text>
        <Badge variant={meta().variant} label={meta().label} testID="realtime-phase" />
        <Badge variant={mic().variant} icon="message" label={mic().label} testID="realtime-mic-state" />
        {/* The right group carries its own `marginLeft: auto`: when the row
            wraps on a narrow pane, the connection fact stays right-aligned
            instead of landing as an orphan at the left of a second line. */}
        <View
          style={{
            marginLeft: "auto",
            flexDirection: "row",
            alignItems: "center",
            gap: space.md,
            flexWrap: "wrap",
          }}
        >
          {/* The clock exists only while a real session is up: an idle or
              never-connected surface must not show a running timer. */}
          <If when={props.active}>
            <Badge variant="secondary" icon="clock" label={props.elapsed} testID="realtime-elapsed" />
          </If>
          <Badge
            variant={props.connectionOk ? "success" : "ghost"}
            icon={props.connectionOk ? "wifi" : "wifi-off"}
            label={props.connection}
            testID="realtime-connection"
          />
        </View>
      </View>
      <Text
        testID="realtime-phase-hint"
        style={{
          numberOfLines: 3,
          fontSize: typeScale.sm,
          lineHeight: Math.round(typeScale.sm * lineHeight.snug),
          color: color.mutedForeground,
        }}
      >
        {hint()}
      </Text>
    </View>
  );
}
