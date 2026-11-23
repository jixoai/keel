// C3 · Collapsible — shadcn/ui `Collapsible` on NSH's native `nsh/base`
// Collapsible (open state, the optional native reveal animation).
//
// Props — Collapsible
// | prop | type | default | notes |
// |---|---|---|---|
// | open | boolean? | false | the content is in the tree while open |
// | reveal | boolean? | true | native clipped-height spring while opening |
// | content | JSX.Element? | – | what expands (made once, keeps its state) |
// | children | JSX.Element? | – | the always-visible part (usually a trigger) |
// | accessibilityLabel | string? | – | what screen readers call the region |
// | testID | string? | – | `id:` selector for tests |
//
// Props — CollapsibleTrigger
// | prop | type | default | notes |
// |---|---|---|---|
// | label | string? | – | the row's text |
// | expanded | boolean? | false | switches the chevron and is said to AT |
// | disabled | boolean? | false | no presses, dimmed |
// | onPress | () => void? | – | toggles the collapsible's `open` |
//
// States: closed (trigger shows `chevron-down`) · open (chevron points up,
// content visible) · disabled (dimmed, rejects presses). TUI: the panel's text
// appears right under the trigger row.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Collapsible as NativeCollapsible, Button as NativeButton } from "nsh/base";
import { Icon, type IconName } from "../../icons";
import { color, radius, space, typeScale, weight } from "../tokens";
import { glassStateFill } from "./styles";
import { disabledOpacity, focusRing } from "./styles";

export function Collapsible(props: {
  open?: boolean;
  reveal?: boolean;
  content?: JSX.Element;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <View
      accessibilityLabel={props.accessibilityLabel}
      role={props.accessibilityLabel !== undefined ? "group" : "none"}
      style={[{ flexDirection: "column" }, props.style ?? null]}
    >
      <NativeCollapsible
        open={props.open}
        reveal={props.reveal ?? true}
        content={props.content}
        testID={props.testID}
        style={{ flexDirection: "column", gap: space.sm }}
      >
        {props.children}
      </NativeCollapsible>
    </View>
  );
}

export function CollapsibleTrigger(props: {
  label?: string;
  icon?: IconName;
  expanded?: boolean;
  disabled?: boolean;
  onPress?: () => void;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    // The native Button has no expanded state of its own: the disclosure's
    // state is exposed on this wrapper (shadcn's `aria-expanded`), while the
    // press itself stays on the native button below.
    <View
      accessibilityState={{ expanded: props.expanded }}
      accessibilityLabel={props.accessibilityLabel ?? props.label}
      style={[{ flexDirection: "column" }, props.style ?? null]}
    >
      <NativeButton
        testID={props.testID}
        disabled={props.disabled}
        onPress={props.onPress}
        accessibilityLabel={props.accessibilityLabel ?? props.label}
        hoverStyle={{ backgroundColor: color.hover }}
        activeStyle={glassStateFill()}
        focusStyle={focusRing()}
        disabledStyle={{ opacity: disabledOpacity }}
        style={{
          flexDirection: "row",
          alignItems: "center",
          justifyContent: "space-between",
          gap: space.md,
          height: 32,
          paddingHorizontal: space.sm,
          borderRadius: radius.md,
        }}
      >
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flex: 1 }}>
          {props.icon ? <Icon name={props.icon} size={14} tint={color.mutedForeground} /> : null}
          {props.label !== undefined ? (
            <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground }}>
              {props.label}
            </Text>
          ) : null}
          {props.children}
        </View>
        <Icon
          name={props.expanded ? "chevron-up" : "chevron-down"}
          size={14}
          tint={color.mutedForeground}
        />
      </NativeButton>
    </View>
  );
}
