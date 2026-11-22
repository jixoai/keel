// C4 · Pagination — shadcn/ui `Pagination` (+ Content / Item / Link / Previous
// / Next / Ellipsis): the row of page numbers a long list ends with. The page is
// the ink's solid pane; every other link is clear glass under the pointer and
// flat at rest, the vendor's one chosen state.
//
// Props — Pagination / PaginationContent / PaginationItem: `children` (+ the
// usual accessibility label and testID).
// Props — PaginationLink
// | prop | type | default | notes |
// |---|---|---|---|
// | label | string? | – | the page number (omit for children) |
// | active | boolean? | false | this is the open page (solid primary pane) |
// | disabled | boolean? | false | not navigable right now |
// | onPress | () => void? | – | goes to this page |
// Props — PaginationPrevious / PaginationNext: `label`, `onPress`, `disabled`.
// Props — PaginationEllipsis: `label` ("更多页"), decorative.
//
// States: current · other · hover · disabled. TUI: each link is its label; the
// current page carries the selected state.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Button as NativeButton } from "nsh/base";
import { Icon } from "../../icons";
import { color, radius, space, typeScale, weight } from "../tokens";
import { disabledOpacity, focusRing, glassControl, glassPrimary, glassStateFill } from "./styles";

export function Pagination(props: {
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <View
      testID={props.testID}
      role="navigation"
      accessibilityLabel={props.accessibilityLabel ?? "分页"}
      style={[{ flexDirection: "row", alignItems: "center" }, props.style ?? null]}
    >
      {props.children}
    </View>
  );
}

export function PaginationContent(props: { children?: JSX.Element; style?: StyleProp<ViewStyle>; testID?: string }) {
  return (
    <View
      testID={props.testID}
      style={[{ flexDirection: "row", alignItems: "center", gap: space.xxs }, props.style ?? null]}
    >
      {props.children}
    </View>
  );
}

export function PaginationItem(props: { children?: JSX.Element; style?: StyleProp<ViewStyle>; testID?: string }) {
  return (
    <View testID={props.testID} style={[{ flexDirection: "row", alignItems: "center" }, props.style ?? null]}>
      {props.children}
    </View>
  );
}

export function PaginationLink(props: {
  label?: string;
  active?: boolean;
  disabled?: boolean;
  onPress?: () => void;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const active = () => props.active === true;
  return (
    <NativeButton
      testID={props.testID}
      disabled={props.disabled}
      selected={active()}
      onPress={props.onPress}
      accessibilityLabel={props.accessibilityLabel ?? props.label}
      hoverStyle={active() ? undefined : { backgroundColor: color.hover }}
      activeStyle={active() ? glassPrimary() : glassStateFill()}
      selectedStyle={glassPrimary()}
      focusStyle={focusRing()}
      disabledStyle={{ opacity: disabledOpacity }}
      style={[
        {
          minWidth: 28,
          height: 28,
          paddingHorizontal: space.sm,
          flexDirection: "row",
          alignItems: "center",
          justifyContent: "center",
          gap: space.xs,
          borderRadius: radius.sm,
        },
        active()
          ? glassPrimary()
          : {
              ...glassControl(),
              backgroundColor: "transparent",
              borderColor: "transparent",
              boxShadow: undefined,
            },
        props.style ?? null,
      ]}
    >
      {props.label !== undefined ? (
        <Text
          style={{
            fontSize: typeScale.sm,
            fontWeight: active() ? weight.medium : weight.regular,
            color: active() ? color.primaryForeground : color.foreground,
          }}
        >
          {props.label}
        </Text>
      ) : null}
      {props.children}
    </NativeButton>
  );
}

/** The previous-page control: an arrow and a word (shadcn's `Previous`). */
export function PaginationPrevious(props: {
  label?: string;
  onPress?: () => void;
  disabled?: boolean;
  accessibilityLabel?: string;
  testID?: string;
}) {
  return (
    <PaginationLink
      label={props.label ?? "上一页"}
      disabled={props.disabled}
      onPress={props.onPress}
      accessibilityLabel={props.accessibilityLabel ?? props.label ?? "上一页"}
      testID={props.testID}
      style={{ paddingHorizontal: space.sm }}
    >
      <Icon name="chevron-left" size={12} tint={color.mutedForeground} />
    </PaginationLink>
  );
}

/** The next-page control. */
export function PaginationNext(props: {
  label?: string;
  onPress?: () => void;
  disabled?: boolean;
  accessibilityLabel?: string;
  testID?: string;
}) {
  return (
    <PaginationLink
      label={props.label ?? "下一页"}
      disabled={props.disabled}
      onPress={props.onPress}
      accessibilityLabel={props.accessibilityLabel ?? props.label ?? "下一页"}
      testID={props.testID}
      style={{ paddingHorizontal: space.sm }}
    >
      <Icon name="chevron-right" size={12} tint={color.mutedForeground} />
    </PaginationLink>
  );
}

/** A gap in the page numbers. Decorative: it names no page. */
export function PaginationEllipsis(props: { label?: string; testID?: string }) {
  return (
    <View
      testID={props.testID}
      accessibilityLabel={props.label ?? "更多页"}
      accessibilityElementsHidden
      style={{ minWidth: 28, height: 28, alignItems: "center", justifyContent: "center" }}
    >
      <Icon name="dots" size={14} tint={color.mutedForeground} />
    </View>
  );
}
