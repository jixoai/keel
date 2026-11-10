// C4 · DropdownMenu — shadcn/ui `DropdownMenu` (+ Item / Separator / Label /
// CheckboxItem) on NSH's native `nsh/base` Menu + MenuItem + MenuSeparator: the
// trigger's keyboard behavior (Down/Up open, Return chooses), the popup
// placement, press-outside / Escape closing and the sheet form on touch screens
// are the component's; the kit carries the surface and the rows.
//
// Props — DropdownMenu
// | prop | type | default | notes |
// |---|---|---|---|
// | trigger | JSX.Element? | – | what opens the menu (an IconButton) |
// | children | JSX.Element? | – | Items / Separators / Labels / Submenus |
// | open | boolean? | – | controlled; omit for the menu's own state |
// | onOpenChange | (e: { open, reason }) => void? | – | reports every open/close (required when `open` is controlled) |
// | placement / align | "bottom" / "start"? | "bottom"/"start" | popup anchor |
// | accessibilityLabel | string? | – | what screen readers call the menu |
// | testID | string? | – | `id:` selector for tests |
//
// Props — DropdownMenuItem
// | prop | type | default | notes |
// |---|---|---|---|
// | label | string? | – | the row's text |
// | icon | IconName? | – | leading glyph |
// | shortcut | string? | – | trailing key hint (Kbd) |
// | checked | boolean? | – | checkable row (`menuitemcheckbox` / radio) |
// | danger | boolean? | false | destructive ink (Delete) |
// | disabled | boolean? | false | not choosable; dimmed |
// | onSelect | () => void? | – | chosen (closes the menu unless `closeOnSelect` is false) |
//
// States: closed · open · item hovered/highlighted (`focus:`) · disabled item ·
// checked item. TUI: the menu's rows render as their titles; the terminal marks
// the highlighted row.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Menu as NativeMenu, MenuItem as NativeMenuItem, MenuSeparator, type OpenChangeReason } from "nsh/base";
import { Icon, type IconName } from "../../icons";
import { color, radius, space, typeScale, weight } from "../tokens";
import { disabledOpacity, menuRow, popupSurface } from "./styles";

export { MenuSeparator as DropdownMenuSeparator };

export function DropdownMenu(props: {
  trigger?: JSX.Element;
  children?: JSX.Element;
  open?: boolean | null;
  onOpenChange?: (event: { open: boolean; reason: OpenChangeReason }) => void;
  placement?: "top" | "bottom" | "left" | "right";
  align?: "start" | "center" | "end";
  offset?: number;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <View
      accessibilityLabel={props.accessibilityLabel}
      testID={props.testID}
      style={[{ flexDirection: "column" }, props.style ?? null]}
    >
      <NativeMenu
        open={props.open ?? null}
        onOpenChange={(event) => props.onOpenChange?.(event)}
        trigger={props.trigger}
        placement={props.placement ?? "bottom"}
        align={props.align ?? "start"}
        offset={props.offset ?? 6}
        margin={8}
        popupStyle={{ ...popupSurface(radius.md), minWidth: 200, flexDirection: "column" }}
      >
        {props.children}
      </NativeMenu>
    </View>
  );
}

export function DropdownMenuItem(props: {
  label?: string;
  icon?: IconName;
  shortcut?: string;
  checked?: boolean | null;
  role?: "menuitem" | "menuitemcheckbox" | "menuitemradio";
  danger?: boolean;
  disabled?: boolean;
  closeOnSelect?: boolean;
  onSelect?: () => void;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const ink = () => (props.danger ? color.danger : props.disabled ? color.mutedForeground : color.popoverForeground);
  return (
    <NativeMenuItem
      testID={props.testID}
      disabled={props.disabled}
      checked={props.checked}
      role={props.role}
      closeOnSelect={props.closeOnSelect}
      onSelect={props.onSelect}
      accessibilityLabel={props.accessibilityLabel ?? props.label}
      hoverStyle={{ backgroundColor: color.hover }}
      activeStyle={{ backgroundColor: color.accent }}
      disabledStyle={{ opacity: disabledOpacity }}
      checkedStyle={{ backgroundColor: color.accent }}
      style={[menuRow(), props.style ?? null]}
      indicator={
        props.checked ? <Icon name="check" size={12} tint={color.popoverForeground} /> : <View style={{ width: 12 }} />
      }
    >
      {props.icon ? <Icon name={props.icon} size={14} tint={ink()} /> : null}
      {props.label !== undefined ? (
        <Text style={{ flex: 1, fontSize: typeScale.base, color: ink() }}>{props.label}</Text>
      ) : null}
      {props.children}
      {props.shortcut !== undefined ? (
        <Text style={{ fontSize: typeScale["2xs"], color: color.mutedForeground }}>{props.shortcut}</Text>
      ) : null}
    </NativeMenuItem>
  );
}

/** A section label inside a menu (shadcn's `DropdownMenuLabel`). */
export function DropdownMenuLabel(props: { label: string; testID?: string }) {
  return (
    <View style={{ height: 24, justifyContent: "center", paddingHorizontal: space.sm }} testID={props.testID}>
      <Text style={{ fontSize: typeScale["2xs"], fontWeight: weight.semibold, color: color.mutedForeground }}>
        {props.label}
      </Text>
    </View>
  );
}
