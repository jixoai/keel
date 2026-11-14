// Harness · realtime surface components — mounts each reusable piece in the
// states the surface composes them from, so `components/components.test.tsx`
// can assert them without a host bridge. Not a product screen.
//
// Every sample here is a state a live session can actually reach: a speaking
// call, a partial transcript, an interrupted reply carrying a tool request, a
// completed reply, and a reconnect notice. Nothing is fabricated on behalf of
// a provider.
import { mount, signal, View } from "nsh";
import {
  AssistantAudioState,
  AssistantTranscriptRow,
  CallControls,
  LiveWaveform,
  StatusStrip,
  SystemLine,
  UserTranscriptRow,
} from "./components";

const active = signal(true);
const muted = signal(false);

const AT = 1_760_000_000_000;

function Harness() {
  return (
    <View style={{ flex: 1, gap: 8 }}>
      {StatusStrip({
        phase: "speaking",
        detail: "助手语音正在流式播放…",
        active: active(),
        muted: muted(),
        mic: "granted",
        elapsed: "00:12",
        connection: "已连接",
        connectionOk: true,
      })}
      {AssistantAudioState({ phase: "playing", level: 0.6 })}
      {LiveWaveform({ level: 0.4, active: true, bars: 12, label: "麦克风输入电平", testID: "fixture-waveform" })}
      <View style={{ gap: 8 }}>
        {UserTranscriptRow({
          text: "明早十点提醒我",
          at: AT,
          source: "speech",
          maxWidth: "100%",
          testID: "fixture-user",
        })}
        {UserTranscriptRow({
          text: "正在说的这句…",
          at: AT,
          source: "speech",
          partial: true,
          maxWidth: "100%",
          testID: "fixture-partial",
        })}
        {AssistantTranscriptRow({
          text: "我查了一下明天的日程，",
          at: AT,
          status: "interrupted",
          tools: [{ id: "t1", name: "calendar.lookup", args: "day=明天", status: "requested" }],
          audio: "stopped",
          outputLevel: 0,
          maxWidth: "100%",
          testID: "fixture-assistant",
        })}
        {AssistantTranscriptRow({
          text: "好的，已安排。",
          at: AT,
          status: "final",
          tools: [],
          audio: "ended",
          outputLevel: 0,
          maxWidth: "100%",
          testID: "fixture-final-assistant",
        })}
        {SystemLine({ text: "已重连：这是一段新的实时会话", at: AT })}
      </View>
      {CallControls({
        stacked: false,
        phase: active() ? "ready" : "idle",
        active: active(),
        muted: muted(),
        busy: false,
        interrupting: false,
        blocked: false,
        showConfig: false,
        elapsed: "00:12",
        inputLevel: 0.3,
        micLabel: muted() ? "麦克风已静音" : "麦克风已授权",
        note: "",
        onStart: () => active.set(true),
        onStop: () => active.set(false),
        onMute: () => muted.update((value) => !value),
        onInterrupt: () => {},
        onOpenSettings: () => {},
      })}
    </View>
  );
}

mount(Harness);
