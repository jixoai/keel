// C1 · Progress — shadcn/ui `Progress`: one bar that says how far a known job
// has come. The track is a groove cut into the surface; the filled part is the
// ink's solid pane, so it belongs to the same material as a checked box and the
// primary button (vendor `[data-slot="progress-track"]` /
// `[data-slot="progress-indicator"]`).
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | value | number? | 0 | 0–1 (clamped); the app owns it |
// | tone | "default" \| "danger" | "default" | failure uses the destructive ink |
// | label | string? | "进度" | what screen readers call the bar |
// | height | number? | 6 | track height in px |
// | testID | string? | – | `id:` selector for tests |
//
// States: empty (0) · partial · complete · danger. TUI: the bar's own value is
// reported to assistive tech; a test reads the label.
import { View } from "nsh";
import { alpha, color } from "../tokens";
import { glassGroove, glassPrimary } from "./styles";

export function Progress(props: {
  value?: number;
  tone?: "default" | "danger";
  label?: string;
  height?: number;
  testID?: string;
}) {
  const fraction = () => {
    const raw = Number.isFinite(props.value) ? (props.value as number) : 0;
    return Math.min(1, Math.max(0, raw));
  };
  const danger = () => props.tone === "danger";
  const height = () => props.height ?? 6;
  return (
    <View
      testID={props.testID}
      role="progressbar"
      accessibilityLabel={props.label ?? "进度"}
      accessibilityValue={{ min: 0, max: 100, now: Math.round(fraction() * 100) }}
      style={{
        height: height(),
        alignSelf: "stretch",
        borderRadius: 999,
        overflow: "hidden",
        ...glassGroove(),
      }}
    >
      <View
        style={{
          width: `${Math.round(fraction() * 100)}%`,
          height: "100%",
          borderRadius: 999,
          ...glassPrimary(),
          ...(danger()
            ? {
                backgroundColor: color.destructive,
                borderColor: alpha(color.destructive, 0.55),
              }
            : {}),
        }}
      />
    </View>
  );
}
