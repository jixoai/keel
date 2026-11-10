// C2 · Command — shadcn/ui `Command` (the cmdk vocabulary: groups, a search
// field, Up/Down + Return) on NSH's native `nsh/base` Command: the native filter
// (label + keywords), the highlight movement, the virtualization and the
// listbox semantics are the component's; the kit supplies the rows, the search
// bar and the section headers.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | items | { id, label, keywords?, group?, shortcut?, disabled?, checked? }[] | – | the commands |
// | query | string? | – | controlled filter text |
// | defaultQuery | string? | – | first filter text |
// | onQueryChange | (query: string) => void? | – | every keystroke |
// | onSelect | (id: string) => void? | – | Return or a press on a row |
// | onCancel | () => void? | – | Escape with an empty query (close the palette) |
// | placeholder | string? | "搜索命令…" | the search field's placeholder |
// | searchable | boolean? | true | a filter field above the rows |
// | filterable | boolean? | true | off: the app answers the query itself |
// | loading | boolean? | false | a search is under way |
// | maxHeight | number? | 320 | the list's height (it scrolls past it) |
// | header / footer | JSX.Element? | – | slots above the field / below the rows |
// | empty | JSX.Element? | "没有匹配的命令" | shown while nothing matches |
// | accessibilityLabel | string? | – | what screen readers call the palette |
// | testID | string? | – | `id:` selector for tests (also how `type` finds the field) |
//
// States: default (all rows) · filtered (query narrows, sections with no match
// go) · empty (the `empty` slot) · loading · disabled rows (skipped by the
// highlight). TUI: rows are the labels from the row's start; typing into the
// field filters them the same way.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Command as NativeCommand, type CommandEntry, type CommandGroup, type CommandItem } from "nsh/base";
import { Icon } from "../../icons";
import { color, radius, space, typeScale, weight } from "../tokens";
import { menuRow, popupSurface } from "./styles";

export type { CommandEntry, CommandGroup, CommandItem };

export function Command(props: {
  items?: CommandItem[];
  query?: string | null;
  defaultQuery?: string | null;
  onQueryChange?: (query: string) => void;
  onSelect?: (id: string) => void;
  onCancel?: () => void;
  placeholder?: string;
  searchable?: boolean;
  filterable?: boolean;
  loading?: boolean;
  maxHeight?: number;
  header?: JSX.Element;
  footer?: JSX.Element;
  empty?: JSX.Element;
  accessibilityLabel?: string;
  searchAccessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <NativeCommand
      testID={props.testID}
      items={props.items}
      query={props.query ?? null}
      defaultQuery={props.defaultQuery ?? null}
      onQueryChange={(event) => props.onQueryChange?.(event.query)}
      onSelect={(event) => props.onSelect?.(event.id)}
      onCancel={props.onCancel}
      placeholder={props.placeholder ?? "搜索命令…"}
      searchable={props.searchable}
      filterable={props.filterable}
      loading={props.loading}
      accessibilityLabel={props.accessibilityLabel}
      searchAccessibilityLabel={props.searchAccessibilityLabel}
      header={props.header}
      footer={props.footer}
      searchPrefix={<Icon name="search" size={14} tint={color.mutedForeground} />}
      empty={
        props.empty ?? (
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, padding: space.md }}>没有匹配的命令</Text>
        )
      }
      searchBarClass={null}
      searchBarStyle={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.sm,
        borderBottomWidth: 1,
        borderBottomColor: color.border,
        paddingHorizontal: space.sm,
      }}
      searchStyle={{
        flex: 1,
        height: 32,
        fontSize: typeScale.base,
        color: color.foreground,
        placeholderColor: color.mutedForeground,
        backgroundColor: "transparent",
      }}
      listStyle={{ maxHeight: props.maxHeight ?? 320 }}
      itemStyle={[menuRow(), { fontSize: typeScale.base, color: color.popoverForeground }]}
      groupStyle={{
        paddingHorizontal: space.sm,
        paddingTop: space.sm,
        paddingBottom: space.xxs,
        fontSize: typeScale["2xs"],
        fontWeight: weight.semibold,
        color: color.mutedForeground,
      }}
      separatorStyle={{ marginVertical: space.xs, height: 1, backgroundColor: color.border }}
      renderItem={(entry: CommandEntry) => (
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flex: 1 }}>
          <Text
            style={{
              flex: 1,
              fontSize: typeScale.base,
              color: entry.disabled ? color.mutedForeground : color.popoverForeground,
            }}
          >
            {entry.label}
          </Text>
          {entry.checked ? <Icon name="check" size={12} tint={color.primary} /> : null}
          {entry.shortcutLabel ? (
            <Text style={{ fontSize: typeScale["2xs"], color: color.mutedForeground }}>{entry.shortcutLabel}</Text>
          ) : null}
        </View>
      )}
      style={[popupSurface(), { alignSelf: "stretch", padding: 0 }, props.style ?? null]}
    />
  );
}
