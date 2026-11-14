// Realtime surface · AssistantAudioState — is the assistant's voice playing?
//
// Audio is a first-class part of this surface, so its playback state is shown
// where the reply is, not buried in the status strip. The meter reads the
// host's reported output level; the words carry the state without colour.
import { Text, View } from "nsh";
import { Badge, color, space, typeScale, type BadgeVariant } from "../../../ui/kit";
import { LiveWaveform } from "./live-waveform";

export type AssistantAudioPhase = "idle" | "playing" | "ended" | "stopped";

const AUDIO_META: Record<AssistantAudioPhase, { label: string; variant: BadgeVariant; note: string }> = {
  idle: { label: "无音频", variant: "ghost", note: "" },
  playing: { label: "语音播放中", variant: "info", note: "助手音频边说边播；开口或点打断会立刻停止。" },
  ended: { label: "语音已播完", variant: "ghost", note: "" },
  stopped: { label: "播放已停止", variant: "warning", note: "打断或取消后已清空待播音频。" },
};

export function AssistantAudioState(props: { phase: AssistantAudioPhase; level: number }) {
  const meta = () => AUDIO_META[props.phase];
  return (
    <View
      testID="realtime-assistant-audio"
      style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}
    >
      <Badge variant={meta().variant} icon="play" label={meta().label} />
      <If when={props.phase === "playing"}>
        <LiveWaveform level={props.level} active bars={12} seed={3} label="助手音频电平" />
      </If>
      <If when={!!meta().note}>
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{meta().note}</Text>
      </If>
    </View>
  );
}
