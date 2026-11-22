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
  paddingHorizontal: controlPadding(size),
  ...glassField(),
});

/** A 1px hairline in the border token. */
export const hairline = (): StyleProp<ViewStyle> => ({ height: 1, backgroundColor: color.border });

/** A row that centers its children. */
export const row = (): StyleProp<ViewStyle> => ({ flexDirection: "row", alignItems: "center" });

/** The opacity a disabled control takes (shadcn's `disabled:opacity-50`). */
export const disabledOpacity = 0.5;

// ---- Solid Glass materials -------------------------------------------------
//
// The vendor stylesheet (`solid-glass.css`) paints the finish at the edge:
// a line of light along the top, a hairline round the outside, a shadow
// gathered underneath. Keel reads those values from `theme.json` (no colour
// is written here) and paints the same four legs through NSH's style props.
// The order is always rim → inner edge → contact → shade, so every pane in
// the app belongs to one material.

/** The two blur radii the vendor sheet uses (`--glass-blur` / control). */
export const GLASS_BLUR = "blur(26px) saturate(180%)";
export const GLASS_BLUR_CONTROL = "blur(14px)";

/** The thinnest pane: the panels a page is laid out in (`--panel`). */
export const glassPanel = (): StyleProp<ViewStyle> => ({
  backgroundColor: color.panel,
  borderWidth: 1,
  borderColor: color.glassEdge,
  backdropFilter: GLASS_BLUR,
  boxShadow: `inset 0 1px 0 ${color.glassRim}, inset 0 0 0 1px ${color.glassInner}`,
});

/** A card pane (`--card`): thicker than a panel, same rim and hairline. */
export const glassCard = (): StyleProp<ViewStyle> => ({
  backgroundColor: color.card,
  borderWidth: 1,
  borderColor: color.glassEdge,
  backdropFilter: GLASS_BLUR,
  boxShadow: `inset 0 1px 0 ${color.glassRim}, inset 0 0 0 1px ${color.glassInner}, 0 1px 2px ${color.glassContact}`,
});

/** The thickest pane: menus, popovers, dialogs (`--popover`). */
export const glassPopover = (): StyleProp<ViewStyle> => ({
  backgroundColor: color.popover,
  borderWidth: 1,
  borderColor: color.popoverEdge,
  backdropFilter: GLASS_BLUR,
  boxShadow: `inset 0 1px 0 ${color.glassRim}, inset 0 -1px 0 ${color.popoverUnder}, 0 24px 60px -18px ${color.glassShade}, 0 6px 16px -6px ${color.glassLift}`,
});

/** Small things made of clear glass: a glass button, a quiet chip. */
export const glassControl = (): StyleProp<ViewStyle> => ({
  backgroundColor: color.glassControl,
  borderWidth: 1,
  borderColor: color.glassControlRim,
  backdropFilter: GLASS_BLUR_CONTROL,
  boxShadow: `inset 0 1px 0 ${color.glassRim}, 0 1px 2px ${color.glassContact}`,
});

/** The field a value is typed into: a pane of clear glass to type on. */
export const glassField = (): StyleProp<ViewStyle> => ({
  backgroundColor: color.field,
  borderWidth: 1,
  borderColor: color.fieldEdge,
  boxShadow: `inset 0 1px 0 ${color.fieldRim}`,
});

/** The one chosen / held-open / keyboard-cursor state. */
export const glassState = (): StyleProp<ViewStyle> => ({
  backgroundColor: color.glassState,
  borderWidth: 1,
  borderColor: color.glassStateEdge,
  boxShadow: `inset 0 1px 0 ${color.glassStateRim}`,
});

/** The one state without a border of its own: a menu row, a command row. */
export const glassStateFill = (): StyleProp<ViewStyle> => ({
  backgroundColor: color.glassState,
  boxShadow: `inset 0 1px 0 ${color.glassStateRim}, 0 0 0 1px ${color.glassStateEdge}`,
});

/** A solid primary pane: the ink button, a checked box, a chosen day. */
export const glassPrimary = (): StyleProp<ViewStyle> => ({
  backgroundColor: color.primary,
  borderWidth: 1,
  borderColor: color.primaryEdge,
  backdropFilter: GLASS_BLUR_CONTROL,
  boxShadow: `inset 0 1px 0 ${color.primaryRim}, inset 0 -1px 0 ${color.primaryUnder}, inset 0 0 0 1px ${color.primaryInner}, 0 1px 2px ${color.glassContact}`,
});

/** A groove cut into the surface (an empty switch track, an unchecked box). */
export const glassGroove = (): StyleProp<ViewStyle> => ({
  backgroundColor: color.muted,
  borderWidth: 1,
  borderColor: color.glassEdge,
  boxShadow: `inset 0 1px 1.5px ${color.wellShade}, inset 0 -1px 0 ${color.wellRim}`,
});

/** A knob (a switch thumb, a slider thumb): the brightest small pane. */
export const glassKnob = (): StyleProp<ViewStyle> => ({
  backgroundColor: color.knob,
  boxShadow: `inset 0 1px 0 ${alpha(color.knob, 0.9)}, inset 0 -1px 0 ${alpha(color.foreground, 0.14)}, 0 1px 2px ${color.knobShade}`,
});

/** The tray a row of tabs sits in. */
export const glassTabsTray = (): StyleProp<ViewStyle> => ({
  backgroundColor: color.muted,
  boxShadow: `inset 0 1px 2px ${color.wellShade}, inset 0 -1px 0 ${color.wellRim}`,
});

/** The tab lying open in that tray. */
export const glassTabsActive = (): StyleProp<ViewStyle> => ({
  backgroundColor: color.glassControl,
  borderWidth: 1,
  borderColor: color.glassEdge,
  boxShadow: `inset 0 1px 0 ${color.glassControlRim}, 0 1px 2px ${color.glassContact}`,
});

/** A surfaced popup: popover face + border + radius (menus, popovers, dialogs). */
export const popupSurface = (r: number = radius.lg): StyleProp<ViewStyle> => ({
  ...glassPopover(),
  borderRadius: r,
  padding: space.xs,
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
