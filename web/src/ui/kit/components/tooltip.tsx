// C4 · Tooltip — shadcn/ui `Tooltip` on NSH's native `nsh/base` Tooltip: the
// hover delay, the keyboard-focus showing, Escape and the placement are the
// component's. The tooltip is given to the element it explains, through that
// element's `tooltip` prop (`<View tooltip={<Tooltip label="…" />}>`).
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | label | string? | – | the hint's text |
// | children | JSX.Element? | – | richer content in place of `label` |
// | placement | "top" \| "bottom" \| "left" \| "right" | "top" | preferred side |
// | align | "start" \| "center" \| "end" | "center" | along that side |
// | delay | number? | 500 | ms the pointer rests before it shows (Infinity: never by itself) |
// | open | boolean? | – | controlled showing (touch screens) |
// | disabled | boolean? | false | no tooltip at all |
// | accessibilityLabel | string? | label | what screen readers get from the bubble |
//
// Props — TooltipText: a one-line muted bubble body, for the common case.
//
// States: hidden · shown (pointer delay / focus / controlled) · disabled.
// TUI: the terminal shows it on the viewport's bottom row instead of a bubble.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Tooltip as NativeTooltip } from "nsh/base";
import { color, radius, space, typeScale } from "../tokens";

export function Tooltip(props: {
  label?: string;
  children?: JSX.Element;
  placement?: "top" | "bottom" | "left" | "right";
  align?: "start" | "center" | "end";
  delay?: number;
  offset?: number;
  open?: boolean | null;
  defaultOpen?: boolean;
  disabled?: boolean;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <NativeTooltip
      open={props.open ?? null}
      defaultOpen={props.defaultOpen}
      placement={props.placement ?? "top"}
      align={props.align ?? "center"}
      offset={props.offset ?? 6}
      delay={props.delay ?? 500}
      disabled={props.disabled}
      style={[
        {
          maxWidth: 320,
          paddingHorizontal: space.md,
          paddingVertical: space.sm,
          borderRadius: radius.md,
          borderWidth: 1,
          borderColor: color.popoverEdge,
          backgroundColor: color.popover,
        },
        props.style ?? null,
      ]}
    >
      <View accessibilityLabel={props.accessibilityLabel ?? props.label} testID={props.testID}>
        {props.label !== undefined ? (
          <Text style={{ fontSize: typeScale.sm, color: color.popoverForeground }}>{props.label}</Text>
        ) : null}
        {props.children}
      </View>
    </NativeTooltip>
  );
}

/** The bubble body the kit uses for a one-line hint. */
export function TooltipText(props: { label: string }) {
  return <Tooltip label={props.label} />;
}
