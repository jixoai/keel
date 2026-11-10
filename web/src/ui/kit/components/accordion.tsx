// C3 · Accordion — shadcn/ui `Accordion` (+ Item / Trigger / Panel) on NSH's
// native `nsh/base` Accordion: the open-values state, the header/trigger/panel
// semantics and the optional native reveal are the component's; the kit carries
// the row, the chevron and the panel's padding.
//
// Props — Accordion
// | prop | type | default | notes |
// |---|---|---|---|
// | value | string[]? | – | the open items' values (app-owned) |
// | multiple | boolean? | false | several items open at once |
// | disabled | boolean? | false | every trigger disabled |
// | onChange | (value: string[]) => void? | – | a trigger's press asks for the next set |
// | children | JSX.Element? | – | the AccordionItems |
// | accessibilityLabel | string? | – | what screen readers call the stack (wrapper) |
// | testID | string? | – | `id:` selector for tests |
//
// Props — AccordionItem
// | prop | type | default | notes |
// |---|---|---|---|
// | value | string | – | the item's identity in `value` |
// | title | string? | – | the trigger row's text |
// | disabled | boolean? | false | no presses; stays as `value` says |
// | reveal | boolean? | true | native clipped-height spring on open/close |
// | children | JSX.Element? | – | the panel's content |
//
// States: closed (`▸`, panel hidden) · open (`▾`, chevron rotated, panel
// visible) · disabled (dimmed). TUI: the native trigger starts with its own
// `▸` / `▾` disclosure and the panel's text follows it.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import {
  Accordion as NativeAccordion,
  AccordionHeader,
  AccordionItem as NativeAccordionItem,
  AccordionPanel,
  AccordionTrigger,
} from "nsh/base";
import { context } from "nsh";
import { Icon } from "../../icons";
import { color, radius, space, typeScale, weight } from "../tokens";
import { disabledOpacity, focusRing } from "./styles";

/** The accordion's open values, so an item can draw its own chevron. */
const openValues = context<() => readonly string[]>(() => []);

export function Accordion(props: {
  value?: string[];
  multiple?: boolean;
  disabled?: boolean;
  onChange?: (value: string[]) => void;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <View
      testID={props.testID}
      role="group"
      accessibilityLabel={props.accessibilityLabel}
      style={[{ flexDirection: "column" }, props.style ?? null]}
    >
      <openValues.Provider value={() => props.value ?? []}>
        <NativeAccordion
          value={props.value}
          multiple={props.multiple}
          disabled={props.disabled}
          onChange={(event) => props.onChange?.(event.value)}
          style={{ flexDirection: "column" }}
        >
          {props.children}
        </NativeAccordion>
      </openValues.Provider>
    </View>
  );
}

export function AccordionItem(props: {
  value: string;
  title?: string;
  disabled?: boolean;
  reveal?: boolean;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const expanded = () => openValues()().includes(props.value);
  return (
    <NativeAccordionItem
      value={props.value}
      disabled={props.disabled}
      testID={props.testID}
      style={[{ flexDirection: "column", borderBottomWidth: 1, borderBottomColor: color.border }, props.style ?? null]}
    >
      <AccordionHeader>
        <AccordionTrigger
          accessibilityLabel={props.accessibilityLabel ?? props.title}
          hoverStyle={{ backgroundColor: color.hover }}
          activeStyle={{ backgroundColor: color.accent }}
          focusStyle={focusRing()}
          disabledStyle={{ opacity: disabledOpacity }}
          style={{
            flexDirection: "row",
            alignItems: "center",
            gap: space.md,
            height: 36,
            paddingHorizontal: space.sm,
            borderRadius: radius.sm,
          }}
        >
          <Text style={{ flex: 1, fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground }}>
            {props.title ?? ""}
          </Text>
          <View
            style={{
              transform: expanded() ? "rotate(180deg)" : "rotate(0deg)",
              transitionProperty: "transform",
              transitionDuration: 160,
            }}
          >
            <Icon name="chevron-down" size={14} tint={color.mutedForeground} />
          </View>
        </AccordionTrigger>
      </AccordionHeader>
      <AccordionPanel reveal={props.reveal ?? true} accessibilityLabel={props.title}>
        <View style={{ padding: space.sm, paddingTop: space.none, flexDirection: "column", gap: space.xs }}>
          {props.children}
        </View>
      </AccordionPanel>
    </NativeAccordionItem>
  );
}

/** The raw native parts, re-exported so an app can build a custom item. */
export { AccordionTrigger, AccordionHeader, AccordionPanel };
