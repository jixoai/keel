// C1 · Separator — shadcn/ui `Separator`: a hairline or a vertical rule.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | orientation | "horizontal" \| "vertical" | "horizontal" | vertical rules stretch to their row |
// | label | string? | – | optional centered label (line · label · line, the kit's one divider language) |
// | decorative | boolean? | true | visible to screen readers only when false |
// | accessibilityLabel | string? | – | set with `decorative={false}` |
// | testID | string? | – | `id:` selector for tests |
// | style | StyleProp<ViewStyle>? | – | overrides |
//
// States: plain rule · labeled rule (when `label` is given). TUI draws a rule
// from `─`/`│` cells; the label renders as text in the middle row.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { color, space, typeScale, weight } from "../tokens";

function Rule(props: { vertical: boolean }) {
  return (
    <View
      style={
        props.vertical
          ? { width: 1, alignSelf: "stretch", backgroundColor: color.border }
          : { height: 1, flex: 1, backgroundColor: color.border }
      }
    />
  );
}

export function Separator(props: {
  orientation?: "horizontal" | "vertical";
  label?: string;
  decorative?: boolean;
  accessibilityLabel?: string;
  testID?: string;
  children?: JSX.Element;
  style?: StyleProp<ViewStyle>;
}) {
  const vertical = () => props.orientation === "vertical";
  return (
    <View
      testID={props.testID}
      role={props.decorative === false ? "text" : "none"}
      accessibilityLabel={props.accessibilityLabel}
      accessibilityElementsHidden={props.decorative !== false}
      style={[
        vertical()
          ? { flexDirection: "column", gap: space.xs, alignItems: "center" }
          : { flexDirection: "row", alignItems: "center", gap: space.md },
        !vertical() && props.label === undefined && props.children === undefined
          ? { alignSelf: "stretch" }
          : null,
        props.style ?? null,
      ]}
    >
      {props.label !== undefined || props.children !== undefined ? (
        <>
          <Rule vertical={vertical()} />
          {props.label !== undefined ? (
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.mutedForeground }}>
              {props.label}
            </Text>
          ) : null}
          {props.children}
          <Rule vertical={vertical()} />
        </>
      ) : (
        <Rule vertical={vertical()} />
      )}
    </View>
  );
}
