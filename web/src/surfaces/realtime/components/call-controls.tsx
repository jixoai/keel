// Realtime surface · CallControls — the call's one primary control and its
// secondary ones (mute, barge-in), the call clock and the live mic meter.
//
// Exactly one primary action exists on this surface, and it is here: start when
// the call is down, stop when it is up. The empty state explains; it does not
// duplicate this button.
//
// When the session cannot start (no realtime model configured) the primary is
// disabled and the actionable next step takes its place beside it:
// 「打开模型设置」. A dead disabled button with no way forward is the dead end this
// panel must not have.
//
// Nothing here starts the microphone on its own — `onStart`/`onStop` are the
// only paths, and they are presses.
import { Text, View } from "nsh";
import { Badge, Button, color, lineHeight, space, typeScale } from "../../../ui/kit";
import { LiveWaveform } from "./live-waveform";
import type { RealtimePhase } from "./status-strip";

export function CallControls(props: {
  stacked: boolean;
  phase: RealtimePhase;
  active: boolean;
  muted: boolean;
  /** A start/stop request is in flight: the primary control stays pressed. */
  busy: boolean;
  interrupting: boolean;
  /** The session cannot start right now: the primary is disabled. */
  blocked: boolean;
  /** The configuration action is the next step (a blocked start, or a start
   * reply that says the voice service is not configured). */
  showConfig: boolean;
  elapsed: string;
  inputLevel: number;
  micLabel: string;
  note: string;
  onStart: () => void;
  onStop: () => void;
  onMute: () => void;
  onInterrupt: () => void;
  onOpenSettings: () => void;
}) {
  const busyLabel = () => (props.active ? "结束中…" : "连接中…");
  return (
    <View style={{ gap: space.sm }}>
      {/* One row that wraps: a button keeps its own width at every viewport —
          the primary never stretches into a full-width slab at 390px. */}
      <View
        style={{
          flexDirection: "row",
          alignItems: "center",
          gap: space.sm,
          flexWrap: "wrap",
        }}
      >
        <Button
          testID="realtime-primary"
          variant={props.active ? "destructive" : props.blocked ? "outline" : "default"}
          size="lg"
          icon={props.active ? "stop" : "message"}
          loading={props.busy}
          disabled={props.busy || props.blocked}
          label={props.busy ? busyLabel() : props.active ? "结束通话" : "开始实时对话"}
          accessibilityLabel={
            props.blocked ? "开始实时对话（当前不可用）" : props.active ? "结束通话" : "开始实时对话"
          }
          onPress={() => (props.active ? props.onStop() : props.onStart())}
          style={{ minWidth: props.stacked ? undefined : 180, minHeight: 36 }}
        />
        {/* The way out of the blocked state: the same settings route the header
            and command palette use. While it is the only way forward it is the
            one filled action; after a failed start it stays secondary and the
            primary (retry) keeps the emphasis. Never a button that silently
            does nothing. */}
        <If when={props.showConfig && !props.active}>
          <Button
            testID="realtime-open-settings"
            variant={props.blocked ? "default" : "secondary"}
            size="lg"
            icon="settings"
            label="打开模型设置"
            accessibilityLabel="打开模型设置"
            onPress={() => props.onOpenSettings()}
            style={{ minHeight: 36 }}
          />
        </If>
        <If when={props.active}>
          <Button
            testID="realtime-mute"
            variant="secondary"
            size="sm"
            icon="message"
            label={props.muted ? "取消静音" : "静音"}
            accessibilityLabel={props.muted ? "取消静音" : "静音"}
            onPress={() => props.onMute()}
          />
          <Button
            testID="realtime-interrupt"
            variant="destructive"
            size="sm"
            icon="stop"
            loading={props.interrupting}
            label="打断"
            accessibilityLabel="打断助手"
            onPress={() => props.onInterrupt()}
          />
          <Badge variant="secondary" icon="clock" label={props.elapsed} testID="realtime-call-timer" />
        </If>
        <View style={{ flex: 1 }} />
        <If when={props.active && !props.muted}>
          <LiveWaveform level={props.inputLevel} active bars={18} seed={1} label="麦克风输入电平" testID="realtime-input-level" />
        </If>
      </View>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <Badge variant={props.muted ? "secondary" : "ghost"} label={props.micLabel} testID="realtime-mic-note" />
        <Text
          testID="realtime-call-note"
          style={{
            flex: 1,
            minWidth: 0,
            fontSize: typeScale.xs,
            lineHeight: Math.round(typeScale.xs * lineHeight.snug),
            color: color.mutedForeground,
          }}
        >
          {props.note || PHASE_NOTE[props.phase]}
        </Text>
      </View>
    </View>
  );
}

/** The quiet line under the controls: the input/next-action fact only. The
 * phase's own sentence lives in the strip — repeating it here would print the
 * same claim twice on one screen. */
const PHASE_NOTE: Record<RealtimePhase, string> = {
  idle: "麦克风只在通话期间开启；拒绝授权时可用文字继续。",
  connecting: "正在申请麦克风权限。",
  ready: "不需要按键录音，直接开口即可。",
  listening: "停顿一下，助手会自动接上。",
  thinking: "想插话，随时开口。",
  speaking: "开口或点「打断」立即停止。",
  interrupted: "继续说下一句即可。",
  reconnecting: "本地转写仍保留。",
  error: "可以重试，或结束通话。",
  closed: "麦克风已释放，不会在后台常听。",
  "permission-denied": "文字输入仍能对话，助手语音仍会播放。",
  unsupported: "仅 web 构建带浏览器音频桥。",
  "no-session": "打开一个会话后即可用。",
  "no-provider": "配置后即可开始通话。",
};
