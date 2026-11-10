// C2 · Combobox — shadcn/ui `Combobox` (the cmdk vocabulary: type to filter,
// arrows to move, Return to choose) on NSH's native `nsh/base` Combobox: the
// filter field, the list (popup on desktops, sheet on touch), the keys and the
// multiple-value semantics are native; the kit supplies the shell and rows.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | items | { value, title, disabled?, group? }[] | – | the options |
// | value | string[]? | – | chosen values (app-owned) |
// | onChange | (values: string[]) => void? | – | a choice or a clear toggles the set |
// | multiple | boolean? | false | several values; the list stays open |
// | query | string? | – | controlled filter text |
// | onQueryChange | (query: string) => void? | – | every keystroke in the filter field |
// | placeholder | string? | "搜索…" | the field's placeholder |
// | disabled | boolean? | false | dimmed, does not open |
// | invalid | boolean? | false | destructive shell border + ring |
// | cleanable | boolean? | true | a × clears the value |
// | accessibilityLabel | string? | – | what screen readers call the choice |
// | testID | string? | – | `id:` selector for tests |
//
// States: empty (placeholder + hint) · chosen (check at the row's end) · open ·
// disabled · invalid. TUI: the field is one row; rows are titles with `✓`.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Combobox as NativeCombobox, type SelectEntry, type SelectItem } from "nsh/base";
import { Icon } from "../../icons";
import { color, radius, space, typeScale } from "../tokens";
import { controlHeight, controlPadding, controlRadius, focusRing, invalidRing, menuRow, popupSurface } from "./styles";

export function Combobox(props: {
  items?: SelectItem[];
  value?: string[];
  onChange?: (values: string[]) => void;
  multiple?: boolean;
  query?: string;
  onQueryChange?: (query: string) => void;
  placeholder?: string;
  disabled?: boolean;
  invalid?: boolean;
  cleanable?: boolean;
  accessibilityLabel?: string;
  testID?: string;
  empty?: JSX.Element;
  style?: StyleProp<ViewStyle>;
}) {
  const has = (value: string) => (props.value ?? []).includes(value);
  return (
    <NativeCombobox
      testID={props.testID}
      items={props.items}
      multiple={props.multiple}
      value={props.value ?? null}
      query={props.query ?? null}
      onQueryChange={(event) => props.onQueryChange?.(event.query)}
      placeholder={props.placeholder ?? "搜索…"}
      disabled={props.disabled}
      cleanable={props.cleanable ?? true}
      accessibilityLabel={props.accessibilityLabel}
      onChange={(event) => props.onChange?.(event.value)}
      clearButton={<Icon name="x" size={12} tint={color.mutedForeground} />}
      clearButtonAccessibilityLabel="清除选择"
      triggerButton={<Icon name="chevron-down" size={14} tint={color.mutedForeground} />}
      prefix={<Icon name="search" size={14} tint={color.mutedForeground} />}
      popupStyle={popupSurface()}
      listStyle={{ maxHeight: 260 }}
      itemStyle={[menuRow(), { fontSize: typeScale.base, color: color.popoverForeground }]}
      renderGroup={(group) => (
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, padding: space.xs }}>{group.title}</Text>
      )}
      inputStyle={{
        height: controlHeight("default"),
        borderRadius: controlRadius("default"),
        borderWidth: 1,
        borderColor: color.input,
        backgroundColor: color.field,
        paddingHorizontal: space.sm,
        fontSize: typeScale.base,
        color: color.foreground,
        placeholderColor: color.mutedForeground,
      }}
      empty={
        props.empty ?? (
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, padding: space.md }}>没有匹配的项</Text>
        )
      }
      renderItem={(entry: SelectEntry) => (
        <View style={{ flexDirection: "row", alignItems: "center", justifyContent: "space-between", flex: 1 }}>
          <Text
            style={{
              fontSize: typeScale.base,
              color: entry.disabled ? color.mutedForeground : color.popoverForeground,
            }}
          >
            {entry.title}
          </Text>
          {has(entry.value) ? <Icon name="check" size={12} tint={color.primary} /> : null}
        </View>
      )}
      hoverStyle={{ borderColor: color.ring }}
      focusStyle={props.invalid ? invalidRing() : focusRing()}
      disabledStyle={{ opacity: 0.5 }}
      style={[
        {
          flexDirection: "row",
          alignItems: "center",
          gap: space.sm,
          alignSelf: "stretch",
          height: controlHeight("default") + 2,
          paddingHorizontal: controlPadding("default") - 2,
          borderRadius: controlRadius("default"),
          borderWidth: 1,
          borderColor: props.invalid ? color.destructive : color.input,
          backgroundColor: color.field,
        },
        props.style ?? null,
      ]}
    />
  );
}
