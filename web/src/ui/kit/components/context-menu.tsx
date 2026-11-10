// C4 · ContextMenu — shadcn/ui `ContextMenu` on NSH's native `nsh/base`
// ContextMenu: the secondary click / two-finger click / held finger opening, the
// shift+F10 keyboard path and the sheet form on touch screens are the
// component's; the kit carries the surface and reuses the DropdownMenu rows.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | trigger | JSX.Element? | – | what the menu is about (the area right-clicked) |
// | menu | JSX.Element? | – | the rows: `ContextMenuItem`, `ContextMenuSeparator`, submenus |
// | open | boolean? | – | controlled; omit for the menu's own state |
// | onOpenChange | (e: { open, reason }) => void? | – | reports every open/close |
// | disabled | boolean? | false | no menu at all |
// | accessibilityLabel | string? | – | what screen readers call the menu |
// | testID | string? | – | `id:` selector for tests |
//
// Props — ContextMenuItem: the same table as `DropdownMenuItem` (label / icon /
// shortcut / danger / disabled / onSelect).
//
// States: closed · open (at the pointer, or below the trigger on the keyboard
// path) · disabled. TUI: the trigger renders; the terminal lists the menu's rows
// as their titles when it opens.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { View } from "nsh";
import { ContextMenu as NativeContextMenu, type OpenChangeReason } from "nsh/base";
import { radius } from "../tokens";
import { popupSurface } from "./styles";

export { DropdownMenuItem as ContextMenuItem, DropdownMenuSeparator as ContextMenuSeparator } from "./dropdown-menu";

export function ContextMenu(props: {
  trigger?: JSX.Element;
  menu?: JSX.Element;
  open?: boolean | null;
  onOpenChange?: (event: { open: boolean; reason: OpenChangeReason }) => void;
  disabled?: boolean;
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
      <NativeContextMenu
        open={props.open ?? null}
        onOpenChange={(event) => props.onOpenChange?.(event)}
        disabled={props.disabled}
        trigger={props.trigger}
        offset={6}
        margin={8}
        popupStyle={{ ...popupSurface(radius.md), minWidth: 200, flexDirection: "column" }}
      >
        {props.menu}
      </NativeContextMenu>
    </View>
  );
}
