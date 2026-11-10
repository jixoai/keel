// C1 · Skeleton — shadcn/ui `Skeleton`: a pulsing placeholder box that holds a
// layout while content loads.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | width | number \| string? | "100%" | token-sized number or a percentage |
// | height | number? | 12 | token-sized height |
// | radius | number? | radius.sm | corner radius |
// | lines | number? | 1 | stacked bars (a paragraph placeholder) |
// | accessibilityLabel | string? | "加载中" | kept for AT: skeletons announce the wait |
// | testID | string? | – | `id:` selector for tests |
//
// States: pulsing (default) · static (reduced motion: the box stays, the pulse
// is dropped by the platform). TUI: renders as a muted block of cells.
import { View, type StyleProp, type ViewStyle } from "nsh";
import { color, radius as radiusToken, space } from "../tokens";

export function Skeleton(props: {
  width?: number | `${number}%`;
  height?: number;
  radius?: number;
  lines?: number;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const lines = () => Math.max(1, props.lines ?? 1);
  const bar = (index: number) => (
    <View
      style={{
        width: index === lines() - 1 && lines() > 1 ? "80%" : (props.width ?? "100%"),
        height: props.height ?? 12,
        borderRadius: props.radius ?? radiusToken.sm,
        backgroundColor: color.muted,
        animationName: "pulse",
        animationDuration: 1400,
        animationIterationCount: "infinite",
        opacity: 0.9,
      }}
    />
  );
  return (
    <View
      testID={props.testID}
      accessibilityLabel={props.accessibilityLabel ?? "加载中"}
      accessibilityElementsHidden={false}
      style={[{ flexDirection: "column", gap: space.sm, alignSelf: "stretch" }, props.style ?? null]}
    >
      {Array.from({ length: lines() }, (_, index) => bar(index))}
    </View>
  );
}
