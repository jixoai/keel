// Kit-internal style helpers. Not a component: the shared fragments the kit's
// components compose from, so a focus ring / hairline / control shell is written
// once and stays token-only.
//
// Every value here is read *inside* a function: kit style fragments never
// snapshot a token at module load, so an appearance switch re-renders them.
import type { StyleProp, ViewStyle } from "nsh";
import { alpha, color, radius, space } from "../tokens";

/** The dense control ladder the kit uses for sm/default/lg sizes. */
export type ControlSize = "sm" | "default" | "lg";

/** Height of a control at each size (shadcn radix-mira: 24 / 28 / 32). */
export const controlHeight = (size: ControlSize): number => (size === "sm" ? 24 : size === "lg" ? 32 : 28);

/** Icon box of a control at each size. */
export const controlIconSize = (size: ControlSize): number => (size === "sm" ? 12 : size === "lg" ? 16 : 14);

/** Radius of a control at each size. */
export const controlRadius = (size: ControlSize): number =>
  size === "sm" ? radius.sm : size === "lg" ? radius.lg : radius.md;

/** Horizontal padding of a control at each size. */
export const controlPadding = (size: ControlSize): number => (size === "sm" ? space.md : size === "lg" ? space.lg : space.md);

/** The keyboard focus ring (shadcn's `ring-2 ring-ring/35`, at a contrast the
 * WCAG non-text floor actually sees).
 *
 * `color.ring` is a translucent wash meant for a resting border: in the dark
 * appearance it is `rgba(255,255,255,0.15)`, and multiplying it by `0.35` again
 * left an effective alpha of 0.0525 — measured 1.14:1 against the background,
 * where 1.4.11 asks for 3:1. The ring therefore derives from the foreground
 * token at `0.65`: measured 5.78:1 on the dark background (5.44 on the panel,
 * 4.96 on a popover) and 3.73:1 on the light background (3.66 panel, 3.80
 * popover), so one value clears the floor on every surface a control sits on.
 * The resting border keeps `color.ring` — only the ring's contrast changes. */
export const focusRing = (): StyleProp<ViewStyle> => ({
  borderColor: color.ring,
  // An outline, not a box-shadow: the focus ring must survive the web target's
  // style pipeline (a box-shadow set through `focusStyle` reached the element as
  // `none` in the browser, so keyboard users saw no ring at all). The outline is
  // a first-class style property on every platform and paints outside the border
  // box, which is exactly the 2px halo the spec asks for.
  outlineStyle: "solid",
  outlineWidth: 2,
  outlineColor: alpha(color.foreground, 0.65),
  outlineOffset: 0,
});

/** The invalid/error ring (shadcn's `aria-invalid:ring-destructive/25`). */
export const invalidRing = (): StyleProp<ViewStyle> => ({
  borderColor: color.destructive,
  outlineStyle: "solid",
  outlineWidth: 2,
  outlineColor: alpha(color.destructive, 0.45),
  outlineOffset: 0,
});

/** The shell of an input-like field (Input, Select trigger, Combobox). */
export const fieldShell = (size: ControlSize = "default"): StyleProp<ViewStyle> => ({
  height: controlHeight(size),
  borderRadius: controlRadius(size),
  borderWidth: 1,
  borderColor: color.input,
  backgroundColor: color.field,
  paddingHorizontal: controlPadding(size),
});

/** A 1px hairline in the border token. */
export const hairline = (): StyleProp<ViewStyle> => ({ height: 1, backgroundColor: color.border });

/** A row that centers its children. */
export const row = (): StyleProp<ViewStyle> => ({ flexDirection: "row", alignItems: "center" });

/** The opacity a disabled control takes (shadcn's `disabled:opacity-50`). */
export const disabledOpacity = 0.5;

/** A surfaced popup: popover face + border + radius (menus, popovers, dialogs). */
export const popupSurface = (r: number = radius.lg): StyleProp<ViewStyle> => ({
  backgroundColor: color.popover,
  borderWidth: 1,
  borderColor: color.popoverEdge,
  borderRadius: r,
  padding: space.xs,
  boxShadow: `0 12px 32px ${alpha(color.glassShade, 1)}`,
});

/** The row styling every menu-like list shares (Select, Menu, Command). */
export const menuRow = (): StyleProp<ViewStyle> => ({
  flexDirection: "row",
  alignItems: "center",
  gap: space.md,
  height: controlHeight("default"),
  paddingHorizontal: space.sm,
  borderRadius: radius.sm,
});
