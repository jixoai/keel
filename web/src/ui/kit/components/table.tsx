// C3 · Table — shadcn/ui `Table` (+ Header / Body / Footer / Row / Head /
// Cell / Caption) on NSH's native table parts (the table/row/columnheader/cell
// roles screen readers hear are native).
//
// Props — Table
// | prop | type | default | notes |
// |---|---|---|---|
// | children | JSX.Element? | – | Header / Body / Footer / Caption |
// | accessibilityLabel | string? | – | what screen readers call the table |
// | bare | boolean? | false | no card shell (a table inside a card) |
// | testID | string? | – | `id:` selector for tests |
//
// Props — rows and cells: `children` (+ `style`); `TableHead` takes `width`.
// `TableRow` hovers and stripes via `striped`.
//
// States: header/body/footer groups · hovered row · empty body (pass an
// `Empty`/text row from the app) · caption present. TUI: cells render as their
// own rows of text, in order.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { View } from "nsh";
import {
  Table as NativeTable,
  TableBody,
  TableCaption,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from "nsh/base";
import { color, radius, space, typeScale, weight } from "../tokens";

export { TableBody, TableCaption, TableCell, TableFooter, TableHead, TableHeader, TableRow };

export function Table(props: {
  children?: JSX.Element;
  accessibilityLabel?: string;
  bare?: boolean;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <View
      testID={props.testID}
      style={[
        props.bare
          ? {}
          : {
              borderRadius: radius.lg,
              borderWidth: 1,
              borderColor: color.border,
              backgroundColor: color.card,
              overflow: "hidden",
            },
        props.style ?? null,
      ]}
    >
      <NativeTable accessibilityLabel={props.accessibilityLabel} style={{ flexDirection: "column" }}>
        {props.children}
      </NativeTable>
    </View>
  );
}

/** A styled header cell: quiet, small, medium weight. */
export function Th(props: { children?: JSX.Element; width?: number | `${number}%`; style?: StyleProp<ViewStyle> }) {
  return (
    <TableHead style={[{ flex: props.width === undefined ? 1 : undefined, width: props.width }, props.style ?? null]}>
      <View style={{ height: 34, justifyContent: "center", paddingHorizontal: space.lg }}>
        {props.children}
      </View>
    </TableHead>
  );
}

/** A styled body cell. */
export function Td(props: { children?: JSX.Element; width?: number | `${number}%`; style?: StyleProp<ViewStyle> }) {
  return (
    <TableCell style={[{ flex: props.width === undefined ? 1 : undefined, width: props.width }, props.style ?? null]}>
      <View style={{ minHeight: 36, justifyContent: "center", paddingHorizontal: space.lg }}>{props.children}</View>
    </TableCell>
  );
}

/** The standard table header row: a muted band with a bottom hairline. */
export function TableHeaderRow(props: { children?: JSX.Element; testID?: string }) {
  return (
    <TableHeader>
      <TableRow
        testID={props.testID}
        style={{
          flexDirection: "row",
          alignItems: "stretch",
          backgroundColor: color.muted,
          borderBottomWidth: 1,
          borderBottomColor: color.border,
        }}
      >
        {props.children}
      </TableRow>
    </TableHeader>
  );
}

/** A body row with the kit's hover and the row separator. Row-level presses are
 * not native on a table row: put the control in a cell (`Button`, `Link`,
 * `Checkbox`), which keeps the row's own role intact. */
export function TableBodyRow(props: { children?: JSX.Element; striped?: boolean; testID?: string }) {
  return (
    <TableRow
      testID={props.testID}
      hoverStyle={{ backgroundColor: color.hover }}
      style={{
        flexDirection: "row",
        alignItems: "stretch",
        borderBottomWidth: 1,
        borderBottomColor: color.border,
        backgroundColor: props.striped ? color.hoverFaint : undefined,
      }}
    >
      {props.children}
    </TableRow>
  );
}
