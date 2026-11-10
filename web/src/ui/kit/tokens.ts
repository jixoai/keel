// Keel UI kit tokens — the single typed surface every kit component reads.
//
// Two sources, and only two:
//   1. `web/theme.json` — anything that changes with appearance (colors) or is a
//      named radius. Read through `token()`, so appearance switches re-render
//      with no per-component light/dark branch. Numeric keys of the theme's
//      `theme` section are design lengths; the rest are colors.
//   2. This file — the scale constants (space / typeScale / zIndex / motion /
//      lineHeight / weight). They never vary by appearance, so they are code.
//
// Scale values follow the zcode reference shell (base font 14, radius 6/8/12/16,
// 4px spacing ladder). Screens and kit components MUST read these names; a raw
// hex color, a raw px font size or a raw z-index in a component is a gate
// failure (`scripts/kit-imports.mjs`).
import { token } from "nsh";

/** Appearance-dependent colors, one getter per `web/theme.json` key. */
export const color = {
  get accent() {
    return token("accent");
  },
  get accentForeground() {
    return token("accent-foreground");
  },
  get background() {
    return token("background");
  },
  get border() {
    return token("border");
  },
  get brand() {
    return token("brand");
  },
  get brandForeground() {
    return token("brand-foreground");
  },
  get brandMuted() {
    return token("brand-muted");
  },
  get brandTint() {
    return token("brand-tint");
  },
  get card() {
    return token("card");
  },
  get cardForeground() {
    return token("card-foreground");
  },
  get chart1() {
    return token("chart-1");
  },
  get chart2() {
    return token("chart-2");
  },
  get chart3() {
    return token("chart-3");
  },
  get chart4() {
    return token("chart-4");
  },
  get chart5() {
    return token("chart-5");
  },
  get codeInline() {
    return token("code-inline");
  },
  get danger() {
    return token("danger");
  },
  get dangerTint() {
    return token("danger-tint");
  },
  get destructive() {
    return token("destructive");
  },
  get destructiveForeground() {
    return token("destructive-foreground");
  },
  get field() {
    return token("field");
  },
  get fieldEdge() {
    return token("field-edge");
  },
  get fieldRim() {
    return token("field-rim");
  },
  get focusLine() {
    return token("focus-line");
  },
  get foreground() {
    return token("foreground");
  },
  get glassContact() {
    return token("glass-contact");
  },
  get glassControl() {
    return token("glass-control");
  },
  get glassControlRim() {
    return token("glass-control-rim");
  },
  get glassEdge() {
    return token("glass-edge");
  },
  get glassInner() {
    return token("glass-inner");
  },
  get glassLift() {
    return token("glass-lift");
  },
  get glassRim() {
    return token("glass-rim");
  },
  get glassShade() {
    return token("glass-shade");
  },
  get glassState() {
    return token("glass-state");
  },
  get glassStateArmed() {
    return token("glass-state-armed");
  },
  get glassStateEdge() {
    return token("glass-state-edge");
  },
  get glassStateRim() {
    return token("glass-state-rim");
  },
  get headerBorder() {
    return token("header-border");
  },
  get hover() {
    return token("hover");
  },
  get hoverFaint() {
    return token("hover-faint");
  },
  get info() {
    return token("info");
  },
  get infoTint() {
    return token("info-tint");
  },
  get input() {
    return token("input");
  },
  get knob() {
    return token("knob");
  },
  get knobShade() {
    return token("knob-shade");
  },
  get link() {
    return token("link");
  },
  get mark() {
    return token("mark");
  },
  get markForeground() {
    return token("mark-foreground");
  },
  get muted() {
    return token("muted");
  },
  get mutedForeground() {
    return token("muted-foreground");
  },
  get panel() {
    return token("panel");
  },
  get popover() {
    return token("popover");
  },
  get popoverEdge() {
    return token("popover-edge");
  },
  get popoverForeground() {
    return token("popover-foreground");
  },
  get popoverUnder() {
    return token("popover-under");
  },
  get primary() {
    return token("primary");
  },
  get primaryForeground() {
    return token("primary-foreground");
  },
  get ring() {
    return token("ring");
  },
  get secondary() {
    return token("secondary");
  },
  get secondaryForeground() {
    return token("secondary-foreground");
  },
  get sidebar() {
    return token("sidebar");
  },
  get sidebarAccent() {
    return token("sidebar-accent");
  },
  get sidebarAccentForeground() {
    return token("sidebar-accent-foreground");
  },
  get sidebarBorder() {
    return token("sidebar-border");
  },
  get sidebarForeground() {
    return token("sidebar-foreground");
  },
  get sidebarPrimary() {
    return token("sidebar-primary");
  },
  get sidebarPrimaryForeground() {
    return token("sidebar-primary-foreground");
  },
  get sidebarRing() {
    return token("sidebar-ring");
  },
  get statusDone() {
    return token("status-done");
  },
  get statusInterrupted() {
    return token("status-interrupted");
  },
  get statusRunning() {
    return token("status-running");
  },
  get success() {
    return token("success");
  },
  get successTint() {
    return token("success-tint");
  },
  get warning() {
    return token("warning");
  },
  get warningTint() {
    return token("warning-tint");
  },
};

/**
 * The 4px spacing ladder, plus 2px and 6px steps the dense chrome needs.
 * Use `space.md` for the default gap between stacked controls, `space.lg` for
 * panel padding, `space.xl`/`space["2xl"]` for section rhythm.
 */
export const space = {
  none: 0,
  /** 2px — hairline insets, badge vertical padding. */
  xxs: 2,
  /** 4px — the unit step (toolbar gaps, chip padding). */
  xs: 4,
  /** 6px — dense list rows. */
  sm: 6,
  /** 8px — the default gap between stacked controls. */
  md: 8,
  /** 12px — control padding, inline groups. */
  lg: 12,
  /** 16px — panel padding, card padding. */
  xl: 16,
  /** 24px — section rhythm inside a column. */
  "2xl": 24,
  /** 32px — page-level rhythm. */
  "3xl": 32,
  /** 40px — hero spacing on empty states. */
  "4xl": 40,
} as const;

/**
 * Radius has two layers: the plain ladder for kit components, and the three
 * semantic roles that `web/theme.json` owns (so the design can retune the
 * finishes without touching components).
 */
export const radius = {
  none: 0,
  xs: 4,
  /** 6px — chips, badges, menu rows. */
  sm: 6,
  /** 8px — buttons, inputs, small controls. */
  md: 8,
  /** 12px — cards, panes, popovers. */
  lg: 12,
  /** 16px — dialogs, the composer shell. */
  xl: 16,
  /** 20px — large surfaces. */
  "2xl": 20,
  pill: 9999,
  /** Semantic roles from the theme file. */
  get inset() {
    return token("radius");
  },
  get card() {
    return token("radius-card");
  },
  get control() {
    return token("radius-control");
  },
} as const;

/**
 * Type scale in px, following the zcode reference (`--ui-font-size` = 14).
 * `typeScale.base` is the body default; `caption` is the muted second line.
 */
export const typeScale = {
  "2xs": 9,
  xs: 10,
  sm: 12,
  caption: 13,
  base: 14,
  lg: 16,
  xl: 18,
  "2xl": 24,
  "3xl": 30,
} as const;

/** Unitless line heights, paired with `typeScale`. */
export const lineHeight = {
  tight: 1.2,
  snug: 1.35,
  normal: 1.5,
  relaxed: 1.65,
} as const;

export const weight = {
  regular: 400,
  medium: 500,
  semibold: 600,
  bold: 700,
} as const;

/** Icon box sizes, in px. */
export const iconSize = {
  xs: 12,
  sm: 14,
  md: 16,
  lg: 18,
  xl: 20,
  "2xl": 24,
} as const;

/**
 * Layering. Components never write a number: the shell owns the layer order, so
 * a tooltip always beats a popover and a dialog always beats the dock.
 */
export const zIndex = {
  base: 0,
  content: 10,
  header: 20,
  sticky: 30,
  backdrop: 40,
  sheet: 50,
  dialog: 60,
  popover: 70,
  dropdown: 80,
  tooltip: 90,
  toast: 100,
} as const;

/**
 * Motion. Durations are ms; `press`/`settle` are the two NSH spring presets the
 * shell has always used (gesture feedback vs. layout settling). `fast` is the
 * default for hover/press feedback, `base` for panel transitions.
 */
export const motion = {
  instant: 0,
  fast: 120,
  base: 180,
  slow: 260,
  stream: 90,
  ease: {
    standard: "cubic-bezier(0.2, 0, 0, 1)",
    emphasis: "cubic-bezier(0.3, 0, 0, 1.1)",
    exit: "cubic-bezier(0.4, 0, 1, 1)",
  },
  press: { stiffness: 560, damping: 38 },
  settle: { stiffness: 320, damping: 30 },
} as const;

/**
 * The monospace stack. There is deliberately no `sans` here: `web/theme.json`'s
 * `font-sans` is applied by NSH as the app-wide default face and is not exposed
 * through `token()`, so re-declaring it here would be a second source of truth.
 */
export const font = {
  get mono() {
    return token("font-mono");
  },
} as const;

/** `color` at opacity `a`: the only sanctioned way to derive a color. */
export const alpha = (c: string, a: number) =>
  `color-mix(in srgb, ${c} ${Math.round(a * 100)}%, transparent)`;
