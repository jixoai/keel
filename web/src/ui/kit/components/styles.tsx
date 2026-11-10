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

/** The keyboard focus ring (shadcn's `ring-2 ring-ring/35`). */
export const focusRing = (): StyleProp<ViewStyle> => ({
  borderColor: color.ring,
  boxShadow: `0 0 0 2px ${alpha(color.ring, 0.35)}`,
});

/** The invalid/error ring (shadcn's `aria-invalid:ring-destructive/25`). */
export const invalidRing = (): StyleProp<ViewStyle> => ({
  borderColor: color.destructive,
  boxShadow: `0 0 0 2px ${alpha(color.destructive, 0.25)}`,
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
  height: 28,
  paddingHorizontal: space.sm,
  borderRadius: radius.sm,
});
