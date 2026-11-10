// C1 · Kbd — shadcn/ui `Kbd`: a keycap for a shortcut hint (⌘ K, Esc).
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | keys | string[]? | – | one cap per key; `Kbd` renders `+` between caps when >1 |
// | label | string? | – | a single cap's text (alternative to `keys`) |
// | children | JSX.Element? | – | custom content (an icon) |
// | accessibilityLabel | string? | joined keys | what screen readers say ("Command K") |
// | testID | string? | – | `id:` selector for tests |
// | style | StyleProp<ViewStyle>? | – | overrides |
//
// States: single cap · combination (`⌘ + K`) · empty (renders nothing visible
// but keeps its box, so a hint row does not jump). TUI: caps render as bracketed
// text cells; the keys are what tests read.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { color, radius, space, typeScale, weight } from "../tokens";

/** One keycap. */
export function KbdCap(props: { label: string; style?: StyleProp<ViewStyle> }) {
  return (
    <View
      style={[
        {
          minWidth: 20,
          height: 20,
          paddingHorizontal: space.xs,
          alignItems: "center",
          justifyContent: "center",
          borderRadius: radius.xs,
          borderWidth: 1,
          borderColor: color.border,
          backgroundColor: color.muted,
        },
        props.style ?? null,
      ]}
    >
      <Text
        style={{
          fontSize: typeScale.xs,
          fontWeight: weight.medium,
          color: color.mutedForeground,
          fontFamily: undefined,
        }}
      >
        {props.label}
      </Text>
    </View>
  );
}

export function Kbd(props: {
  keys?: string[];
  label?: string;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const keys = () => props.keys ?? (props.label !== undefined ? [props.label] : []);
  return (
    <View
      testID={props.testID}
      role="keyboardkey"
      accessibilityLabel={props.accessibilityLabel ?? keys().join("+")}
      style={[
        { flexDirection: "row", alignItems: "center", gap: space.xxs, pointerEvents: "none" },
        props.style ?? null,
      ]}
    >
      {keys().map((key, index) => (
        <>
          {index > 0 ? (
            <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground, fontWeight: weight.regular }}>+</Text>
          ) : null}
          <KbdCap label={key} />
        </>
      ))}
      {props.children}
    </View>
  );
}
