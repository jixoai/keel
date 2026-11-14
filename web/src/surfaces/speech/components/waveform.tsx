// The STT workbench's level display: a row of bars, either the stable shape of
// a recorded take or a live animation driven by the recorder's clock.
//
// `beat` is read inside JSX (`liveBar(props.beat())`), so a caller passes the
// tick accessor and only this row's bars re-run while recording. Everything is
// drawn with kit tokens: no component owns a colour or a type size.
import type { JSX } from "nsh";
import { View } from "nsh";
import { color, radius, space } from "../../../ui/kit";

/** A row's waveform is stable: the same seed always draws the same bars. */
const stillBar = (seed: number, index: number): number =>
  5 + Math.round(Math.abs(Math.sin(index * 1.7 + seed * 0.13)) * 11);

/** Read inside JSX: each beat moves the live bars. */
const liveBar = (beat: number, index: number): number => {
  const value =
    Math.abs(Math.sin(index * 1.31 + beat * 0.37) * 0.7) +
    Math.abs(Math.sin(index * 0.77 + beat * 0.11) * 0.3);
  return 4 + Math.round(value * 18);
};

export function Waveform(props: {
  bars?: number;
  /** The recorder is listening: the bars move with `beat`. */
  active?: boolean;
  /** Keeps an inactive waveform's shape stable per row. */
  seed?: number;
  /** The recorder's clock, read inside the bars while `active`. */
  beat?: () => number;
  testID?: string;
  accessibilityLabel?: string;
}): JSX.Element {
  const count = props.bars ?? 16;
  const height = (index: number): number => {
    if (!props.active) return stillBar(props.seed ?? 1, index);
    return props.beat ? liveBar(props.beat(), index) : stillBar(props.seed ?? 1, index);
  };
  return (
    <View
      testID={props.testID ?? "speech-waveform"}
      accessible
      role="image"
      accessibilityLabel={
        props.accessibilityLabel ?? (props.active ? "录音波形（动效）" : "音频波形")
      }
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.xxs,
        // An explicit box keeps the waveform visible in hosts whose layout
        // does not give a row of fixed-height bars an intrinsic width.
        width: count * (space.xxs + 1) + Math.max(0, count - 1) * space.xxs,
        height: space["2xl"],
      }}
    >
      {Array.from({ length: count }, (_, index) => (
        <View
          style={{
            width: space.xxs + 1,
            height: height(index),
            borderRadius: radius.pill,
            backgroundColor: props.active ? color.brand : color.mutedForeground,
            opacity: props.active ? 1 : 0.55,
          }}
        />
      ))}
    </View>
  );
}
