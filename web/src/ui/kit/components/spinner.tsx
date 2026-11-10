// C1 · Spinner — shadcn's `Spinner` (a lucide `loader` turning), drawn with an
// NSH-native ring: no image, no second icon language.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | size | number? | 16 (iconSize.md) | box in px |
// | tint | string? | color.foreground | the moving arc's color |
// | accessibilityLabel | string? | "加载中" | what screen readers announce |
// | testID | string? | – | `id:` selector for tests |
//
// States: spinning (default) · static when the platform asks for less motion
// (the ring still shows). TUI: renders as a small glyph-like box; the label is
// what tests and screen readers read.
import { View } from "nsh";
import { alpha, color, iconSize } from "../tokens";

export function Spinner(props: { size?: number; tint?: string; accessibilityLabel?: string; testID?: string }) {
  const size = () => props.size ?? iconSize.md;
  return (
    <View
      testID={props.testID}
      role="progressbar"
      accessibilityLabel={props.accessibilityLabel ?? "加载中"}
      style={{
        width: size(),
        height: size(),
        borderRadius: size() / 2,
        borderWidth: 2,
        borderColor: alpha(color.mutedForeground, 0.3),
        borderTopColor: props.tint ?? color.foreground,
        animationName: "spin",
        animationDuration: 900,
        animationTimingFunction: "linear",
        animationIterationCount: "infinite",
      }}
    />
  );
}
