// Solid Glass tokens for the NSH client.
//
// The values live in `web/theme.json` (the theme file NSH compiles into the
// app); this module only names them for TSX. Every color read goes through
// `token()` (or a theme class), so switching appearance re-renders with no
// local light/dark branches. Motion constants are code tokens because NSH's
// theme file carries design tokens (colors, lengths, fonts), not curves.
import { token } from "nsh";

export const colors = {
  get background() {
    return token("background");
  },
  get foreground() {
    return token("foreground");
  },
  get panel() {
    return token("panel");
  },
  get card() {
    return token("card");
  },
  get cardForeground() {
    return token("card-foreground");
  },
  get popover() {
    return token("popover");
  },
  get primary() {
    return token("primary");
  },
  get primaryForeground() {
    return token("primary-foreground");
  },
  get secondary() {
    return token("secondary");
  },
  get muted() {
    return token("muted");
  },
  get mutedForeground() {
    return token("muted-foreground");
  },
  get accent() {
    return token("accent");
  },
  get accentForeground() {
    return token("accent-foreground");
  },
  get destructive() {
    return token("destructive");
  },
  get destructiveForeground() {
    return token("destructive-foreground");
  },
  get headerBorder() {
    return token("header-border");
  },
  get mark() {
    return token("mark");
  },
  get markForeground() {
    return token("mark-foreground");
  },
  get border() {
    return token("border");
  },
  get input() {
    return token("input");
  },
  get ring() {
    return token("ring");
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
  get link() {
    return token("link");
  },
  get success() {
    return token("success");
  },
  get warning() {
    return token("warning");
  },
  get danger() {
    return token("danger");
  },
  get info() {
    return token("info");
  },
  get statusRunning() {
    return token("status-running");
  },
  get statusDone() {
    return token("status-done");
  },
  get statusInterrupted() {
    return token("status-interrupted");
  },
  get sidebar() {
    return token("sidebar");
  },
  get sidebarAccent() {
    return token("sidebar-accent");
  },
  get sidebarBorder() {
    return token("sidebar-border");
  },
  get glassRim() {
    return token("glass-rim");
  },
  get glassInner() {
    return token("glass-inner");
  },
  get glassEdge() {
    return token("glass-edge");
  },
  get glassShade() {
    return token("glass-shade");
  },
  get glassContact() {
    return token("glass-contact");
  },
  get glassLift() {
    return token("glass-lift");
  },
  get glassControl() {
    return token("glass-control");
  },
  get glassControlRim() {
    return token("glass-control-rim");
  },
  get glassState() {
    return token("glass-state");
  },
  get glassStateRim() {
    return token("glass-state-rim");
  },
  get glassStateArmed() {
    return token("glass-state-armed");
  },
  get hover() {
    return token("hover");
  },
  get field() {
    return token("field");
  },
  get fieldRim() {
    return token("field-rim");
  },
  get fieldEdge() {
    return token("field-edge");
  },
  get focusLine() {
    return token("focus-line");
  },
  get codeInline() {
    return token("code-inline");
  },
  get brandTint() {
    return token("brand-tint");
  },
  get successTint() {
    return token("success-tint");
  },
  get warningTint() {
    return token("warning-tint");
  },
  get dangerTint() {
    return token("danger-tint");
  },
  get infoTint() {
    return token("info-tint");
  },
} as const;

export const radius = {
  get card() {
    return token("radius-card");
  },
  get inset() {
    return token("radius");
  },
  get control() {
    return token("radius-control");
  },
  get pill() {
    return token("radius-pill");
  },
};

/** `color` at opacity `a`, without writing a raw color. */
export const alpha = (color: string, a: number) =>
  `color-mix(in srgb, ${color} ${Math.round(a * 100)}%, transparent)`;

export const mono = () => token("font-mono");

/** The four Solid Glass finishes: rim + hairline + shade, never a face
 * gradient. Thickness order: panel < card < popover. */
export const glass = {
  get panel() {
    return {
      backgroundColor: colors.panel,
      borderWidth: 1,
      borderColor: colors.glassEdge,
      boxShadow: `inset 0 1px 0 ${colors.glassRim}, 0 10px 30px ${colors.glassContact}`,
    };
  },
  get card() {
    return {
      backgroundColor: colors.card,
      borderWidth: 1,
      borderColor: colors.glassEdge,
      boxShadow: `inset 0 1px 0 ${colors.glassRim}, inset 0 0 0 0.5px ${colors.glassInner}, 0 12px 34px ${colors.glassContact}`,
    };
  },
  get popover() {
    return {
      backgroundColor: colors.popover,
      borderWidth: 1,
      borderColor: colors.glassEdge,
      boxShadow: `inset 0 1px 0 ${colors.glassRim}, 0 18px 48px ${colors.glassShade}`,
    };
  },
  get control() {
    return {
      backgroundColor: colors.glassControl,
      borderWidth: 1,
      borderColor: colors.glassControlRim,
    };
  },
  get state() {
    return {
      backgroundColor: colors.glassState,
      borderWidth: 1,
      borderColor: colors.glassStateRim,
    };
  },
  get field() {
    return {
      backgroundColor: colors.field,
      borderWidth: 1,
      borderColor: colors.fieldEdge,
      boxShadow: `inset 0 1px 0 ${colors.fieldRim}`,
    };
  },
};

export const motion = {
  press: { stiffness: 560, damping: 38 },
  settle: { stiffness: 320, damping: 30 },
  fade: 180,
  stream: 90,
} as const;

export const space = { xxs: 2, xs: 4, sm: 8, md: 12, lg: 16, xl: 24, xxl: 32 } as const;
