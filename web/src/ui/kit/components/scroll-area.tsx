// C3 · ScrollArea — shadcn/ui `ScrollArea` on NSH's native scroll view: the
// scrolling, the indicator (or a themed one) and the keyboard/wheel handling
// are the platform's.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | horizontal | boolean? | false | scrolls sideways |
// | scrollbar | boolean? | true | the platform's indicator |
// | maxHeight | number? | – | caps the viewport (a scrolling list inside a card) |
// | contentStyle | StyleProp<ViewStyle>? | – | styles the content box |
// | fade | boolean? | false | reserves room for a scrollbar (padding right) |
// | accessibilityLabel | string? | – | what screen readers call the region |
// | testID | string? | – | `id:` selector for tests (`scroll` targets it) |
//
// States: resting · scrolled (indicator shows) · empty (renders its children
// even when there are none) · content-fits (no indicator). TUI: scrolls with
// `app.scroll("id:…", n)`.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Scroll } from "nsh";
import { space } from "../tokens";

export function ScrollArea(props: {
  children?: JSX.Element;
  horizontal?: boolean;
  scrollbar?: boolean;
  maxHeight?: number;
  fade?: boolean;
  contentStyle?: StyleProp<ViewStyle>;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <Scroll
      testID={props.testID}
      horizontal={props.horizontal}
      scrollbar={props.scrollbar ?? true}
      accessibilityLabel={props.accessibilityLabel}
      contentStyle={[{ flexDirection: props.horizontal ? "row" : "column" }, props.contentStyle ?? null]}
      style={[
        { maxHeight: props.maxHeight, paddingRight: props.fade ? space.sm : undefined },
        props.style ?? null,
      ]}
    >
      {props.children}
    </Scroll>
  );
}
