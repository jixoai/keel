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
// | onHighlightChange | (id: string \| null) => void? | – | the highlighted row (keys or pointer) |
// | onCancel | () => void? | – | Escape with an empty query (close the palette) |
// | placeholder | string? | "搜索命令…" | the search field's placeholder |
// | searchable | boolean? | true | a filter field above the rows |
// | filterable | boolean? | true | off: the app answers the query itself |
// | loading | boolean? | false | a search is under way |
// | autoFocus | boolean? | false | the search field takes the keyboard focus once shown |
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
//
// The highlighted row is painted here, from the native `onHighlightChange`
// id: the native command projects the highlight as the row's `focus:` state,
// and the web host does not carry projected states (nsh-platform-web has no
// projected-state handling), so a `focus:` class would leave the web's
// keyboard cursor invisible. One wash for the keys and the pointer both — the
// native command moves the same highlight for either.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { signal, Text, View } from "nsh";
import { Command as NativeCommand, type CommandEntry, type CommandGroup, type CommandItem } from "nsh/base";
import { Icon } from "../../icons";
import { color, radius, space, typeScale, weight } from "../tokens";
import { menuRow, popupSurface } from "./styles";

export type { CommandEntry, CommandGroup, CommandItem };

/** One row's content, with or without the highlight wash. Its own component:
 * the wash has to be part of what the `If` mounts (see `renderItem`). */
function CommandRow(props: { entry: CommandEntry; highlighted: boolean }) {
  return (
    <View
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.md,
        width: "100%",
        height: 28,
        borderRadius: radius.sm,
        // The reference's `data-selected:bg-menu-hover`: the kit's selected
        // surface token (a step above `hover`, so the keyboard cursor reads at
        // a glance), never a colour of its own.
        backgroundColor: props.highlighted ? color.selected : undefined,
      }}
    >
      <Text
        style={{
          flex: 1,
          fontSize: typeScale.base,
          color: props.entry.disabled ? color.mutedForeground : color.popoverForeground,
        }}
      >
        {props.entry.label}
      </Text>
      {props.entry.checked ? <Icon name="check" size={12} tint={color.primary} /> : null}
      {props.entry.shortcutLabel ? (
        <Text style={{ fontSize: typeScale["2xs"], color: color.mutedForeground }}>{props.entry.shortcutLabel}</Text>
      ) : null}
    </View>
  );
}

export function Command(props: {
  items?: CommandItem[];
  query?: string | null;
  defaultQuery?: string | null;
  onQueryChange?: (query: string) => void;
  onSelect?: (id: string) => void;
  onHighlightChange?: (id: string | null) => void;
  onCancel?: () => void;
  placeholder?: string;
  searchable?: boolean;
  filterable?: boolean;
  loading?: boolean;
  /** The search field takes the keyboard focus once shown (native
   * `Command`'s `autoFocus`): a palette that opens to be typed in. */
  autoFocus?: boolean;
  maxHeight?: number;
  header?: JSX.Element;
  footer?: JSX.Element;
  empty?: JSX.Element;
  accessibilityLabel?: string;
  searchAccessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  /** The row the native command has highlighted (the keys or the pointer):
   * the wash below follows it. */
  const highlighted = signal<string | null>(null);
  return (
    <NativeCommand
      testID={props.testID}
      items={props.items}
      query={props.query ?? null}
      defaultQuery={props.defaultQuery ?? null}
      onQueryChange={(event) => props.onQueryChange?.(event.query)}
      onSelect={(event) => props.onSelect?.(event.id)}
      onHighlightChange={(event) => {
        highlighted.set(event.highlighted);
        props.onHighlightChange?.(event.highlighted);
      }}
      onCancel={props.onCancel}
      placeholder={props.placeholder ?? "搜索命令…"}
      searchable={props.searchable}
      filterable={props.filterable}
      loading={props.loading}
      autoFocus={props.autoFocus}
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
        // The wash is a mount, not a style value: a host view's styles are
        // made once, so only an `If` that swaps the row's subtree shows the
        // state. The native highlight lands on the row's projected `focus:`
        // class, and the web host carries no projected state
        // (`nsh-platform-web` has no projected-state handling), which would
        // leave the web's keyboard cursor invisible. The pointer moves the
        // same highlight, so hover and the keys share one face.
        <If
          when={entry.id === highlighted()}
          else={<CommandRow entry={entry} highlighted={false} />}
        >
          <CommandRow entry={entry} highlighted />
        </If>
      )}
      style={[popupSurface(), { alignSelf: "stretch", padding: 0 }, props.style ?? null]}
    />
  );
}
