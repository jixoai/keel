// Realtime surface · LiveWaveform — the microphone/assistant level meter.
//
// The bars are the measured signal, not an animation: `level` is the RMS the
// host reports (`keel.realtime.level`), so a silent room draws a flat line and
// the panel never pretends to hear something. Each bar keeps its own phase so
// the meter reads as a waveform rather than a progress bar.
//
// Kit only: sizes come from `space`, colors from the theme's brand/muted
// tokens; no hex, no hand-written chrome.
import { View } from "nsh";
import { color, radius, space } from "../../../ui/kit";

export function LiveWaveform(props: {
  /** 0..1, the last reported level. */
  level: number;
  /** Rows that are not moving yet (a stopped call) draw the quiet baseline. */
  active?: boolean;
  bars?: number;
  /** Keeps two meters (mic vs. assistant) from moving in lockstep. */
  seed?: number;
  label: string;
  testID?: string;
}) {
  const count = props.bars ?? 16;
  const size = (index: number) => {
    const value = Math.max(0, Math.min(1, props.level));
    if (!props.active || value <= 0.001) return space.xxs + 1;
    const shape = Math.abs(Math.sin(index * 1.7 + (props.seed ?? 1) * 0.9 + value * 7.3));
    return Math.max(space.xs, Math.round((space.xs + value * space["2xl"]) * (0.45 + shape * 0.55)));
  };
  return (
    <View
      testID={props.testID}
      accessibilityLabel={`${props.label}${props.active ? ` · ${Math.round(props.level * 100)}%` : ""}`}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.xxs,
        height: space["2xl"],
        flexShrink: 0,
      }}
    >
      {Array.from({ length: count }, (_, index) => (
        <View
          style={{
            width: space.xxs + 1,
            height: size(index),
            borderRadius: radius.pill,
            backgroundColor: props.active && props.level > 0.001 ? color.brand : color.mutedForeground,
            opacity: props.active ? (props.level > 0.001 ? 1 : 0.3) : 0.2,
          }}
        />
      ))}
    </View>
  );
}
