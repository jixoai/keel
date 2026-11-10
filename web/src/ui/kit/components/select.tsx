// C2 · Select — shadcn/ui `Select` on NSH's native `nsh/base` Select: the list,
// the popup/sheet placement, the keys (arrows, Home/End, typeahead), the
// controlled `value` and the AT trigger value are native; the kit supplies the
// trigger shell and the row look.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | items | { value, title, disabled?, group? }[] | – | the options, in order |
// | value | string \| null? | – | chosen value (app-owned) |
// | defaultValue | string? | – | first value when uncontrolled |
// | onChange | (value: string \| null) => void? | – | a choice (null: cleared) |
// | placeholder | string? | "请选择" | shown while nothing is chosen |
// | size | "sm" \| "default" \| "lg" | "default" | trigger height |
// | searchable | boolean? | false | a filter field above the rows |
// | disabled | boolean? | false | does not open; dimmed |
// | invalid | boolean? | false | destructive trigger border + ring |
// | placement / align | "top"\|"bottom" / "start"\|"center"\|"end"? | "bottom"/"start" | desktop popup side |
// | accessibilityLabel | string? | – | what screen readers call the choice |
// | testID | string? | – | `id:` selector for tests |
//
// States: empty (placeholder) · chosen · open (expanded trigger) · disabled ·
// invalid. TUI: the trigger is one row with the chosen title and `▾`.
import type { StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Select as NativeSelect, type SelectEntry, type SelectItem } from "nsh/base";
import { Icon } from "../../icons";
import { color, radius, space, typeScale } from "../tokens";
import { controlHeight, controlPadding, controlRadius, focusRing, invalidRing, menuRow, popupSurface, type ControlSize } from "./styles";

export type { SelectItem };

export function Select(props: {
  items?: SelectItem[];
  value?: string | null;
  defaultValue?: string | null;
  onChange?: (value: string | null) => void;
  placeholder?: string;
  size?: ControlSize;
  searchable?: boolean;
  searchPlaceholder?: string;
  disabled?: boolean;
  invalid?: boolean;
  placement?: "top" | "bottom" | "left" | "right";
  align?: "start" | "center" | "end";
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const size = () => props.size ?? "default";
  const title = () => props.items?.find((item) => item.value === props.value)?.title;
  return (
    <NativeSelect
      testID={props.testID}
      items={props.items}
      value={props.value}
      defaultValue={props.defaultValue}
      placeholder={props.placeholder ?? "请选择"}
      searchable={props.searchable}
      searchPlaceholder={props.searchPlaceholder}
      disabled={props.disabled}
      placement={props.placement ?? "bottom"}
      align={props.align ?? "start"}
      offset={4}
      accessibilityLabel={props.accessibilityLabel}
      onChange={(event) => props.onChange?.(event.value)}
      popupStyle={popupSurface()}
      listStyle={{ maxHeight: 260 }}
      itemStyle={[
        menuRow(),
        { fontSize: typeScale.base, color: color.popoverForeground },
      ]}
      renderGroup={(group) => (
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, padding: space.xs }}>{group.title}</Text>
      )}
      searchStyle={{
        height: controlHeight("sm"),
        borderRadius: controlRadius("sm"),
        borderWidth: 1,
        borderColor: color.input,
        backgroundColor: color.field,
        paddingHorizontal: space.sm,
        fontSize: typeScale.sm,
        color: color.foreground,
        placeholderColor: color.mutedForeground,
      }}
      empty={
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, padding: space.md }}>没有可选项</Text>
      }
      clearButton={<Icon name="x" size={12} tint={color.mutedForeground} />}
      clearButtonAccessibilityLabel="清除选择"
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
          {entry.value === props.value ? <Icon name="check" size={12} tint={color.primary} /> : null}
        </View>
      )}
      trigger={
        <View
          style={{
            flexDirection: "row",
            alignItems: "center",
            justifyContent: "space-between",
            gap: space.sm,
            height: controlHeight(size()),
            paddingHorizontal: controlPadding(size()),
            borderRadius: controlRadius(size()),
            borderWidth: 1,
            borderColor: props.invalid ? color.destructive : color.input,
            backgroundColor: color.field,
          }}
        >
          <Text
            style={{
              fontSize: typeScale.base,
              color: props.disabled
                ? color.mutedForeground
                : title() === undefined
                  ? color.mutedForeground
                  : color.foreground,
            }}
          >
            {title() ?? props.placeholder ?? "请选择"}
          </Text>
          <Icon name="chevron-down" size={14} tint={color.mutedForeground} />
        </View>
      }
      hoverStyle={{ borderColor: color.ring }}
      focusStyle={props.invalid ? invalidRing() : focusRing()}
      disabledStyle={{ opacity: 0.5 }}
      style={[{ alignSelf: "stretch", borderRadius: controlRadius(size()) }, props.style ?? null]}
    />
  );
}
