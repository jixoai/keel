// Legacy theme projection for the pre-kit screens.
//
// The single source of design values is now the kit: `web/theme.json` (colors,
// named radii, font stacks) read through `./kit/tokens`. This module only keeps
// the old names alive so `web/src/screens/**` keeps rendering while it migrates
// to `web/src/ui/kit` — it holds no values of its own. New code imports the kit
// (`./kit/tokens`, `./kit/layout`), never this file.
//
// The values themselves moved to the zcode `theme-zai-dark` scale (14/12/10/9
// type, 6/12/16 radii, #161616/#202020/#2b2b2b surfaces, 10% white border,
// brand #fff, accent #001d3d, 5/10% surface/hover/selected washes) when
// `theme.json` was switched; every name below is a projection of that file.
import { token } from "nsh";
import { alpha, color, font, motion as kitMotion, radius as kitRadius, space as kitSpace } from "./kit/tokens";

export { alpha };

/** Appearance colors, one getter per theme key. Kept for the old screens. */
export const colors = {
  get background() {
    return color.background;
  },
  get foreground() {
    return color.foreground;
  },
  get panel() {
    return color.panel;
  },
  get card() {
    return color.card;
  },
  get cardForeground() {
    return color.cardForeground;
  },
  get popover() {
    return color.popover;
  },
  get primary() {
    return color.primary;
  },
  get primaryForeground() {
    return color.primaryForeground;
  },
  get secondary() {
    return color.secondary;
  },
  get muted() {
    return color.muted;
  },
  get mutedForeground() {
    return color.mutedForeground;
  },
  get accent() {
    return color.accent;
  },
  get accentForeground() {
    return color.accentForeground;
  },
  get destructive() {
    return color.destructive;
  },
  get destructiveForeground() {
    return color.destructiveForeground;
  },
  get headerBorder() {
    return color.headerBorder;
  },
  get mark() {
    return color.mark;
  },
  get markForeground() {
    return color.markForeground;
  },
  get border() {
    return color.border;
  },
  get input() {
    return color.input;
  },
  get ring() {
    return color.ring;
  },
  get brand() {
    return color.brand;
  },
  get brandForeground() {
    return color.brandForeground;
  },
  get brandMuted() {
    return color.brandMuted;
  },
  get link() {
    return color.link;
  },
  get success() {
    return color.success;
  },
  get warning() {
    return color.warning;
  },
  get danger() {
    return color.danger;
  },
  get info() {
    return color.info;
  },
  get statusRunning() {
    return color.statusRunning;
  },
  get statusDone() {
    return color.statusDone;
  },
  get statusInterrupted() {
    return color.statusInterrupted;
  },
  get sidebar() {
    return color.sidebar;
  },
  get sidebarAccent() {
    return color.sidebarAccent;
  },
  get sidebarBorder() {
    return color.sidebarBorder;
  },
  get glassRim() {
    return color.glassRim;
  },
  get glassInner() {
    return color.glassInner;
  },
  get glassEdge() {
    return color.glassEdge;
  },
  get glassShade() {
    return color.glassShade;
  },
  get glassContact() {
    return color.glassContact;
  },
  get glassLift() {
    return color.glassLift;
  },
  get glassControl() {
    return color.glassControl;
  },
  get glassControlRim() {
    return color.glassControlRim;
  },
  get glassState() {
    return color.glassState;
  },
  get glassStateRim() {
    return color.glassStateRim;
  },
  get glassStateArmed() {
    return color.glassStateArmed;
  },
  get hover() {
    return color.hover;
  },
  get field() {
    return color.field;
  },
  get fieldRim() {
    return color.fieldRim;
  },
  get fieldEdge() {
    return color.fieldEdge;
  },
  get focusLine() {
    return color.focusLine;
  },
  get codeInline() {
    return color.codeInline;
  },
  get brandTint() {
    return color.brandTint;
  },
  get successTint() {
    return color.successTint;
  },
  get warningTint() {
    return color.warningTint;
  },
  get dangerTint() {
    return color.dangerTint;
  },
  get infoTint() {
    return color.infoTint;
  },
} as const;

/** The named radii of the theme file, plus the pill sentinel. */
export const radius = {
  get card() {
    return kitRadius.card;
  },
  get inset() {
    return kitRadius.inset;
  },
  get control() {
    return kitRadius.control;
  },
  get pill() {
    return token("radius-pill");
  },
};

export const mono = () => font.mono;

/** The four Solid Glass finishes. They now sit on the zai-dark washes
 * (5% surface / 10% border) rather than a light material. */
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

/** Motion, from the kit tokens (`fade` is the kit's `base` duration). */
export const motion = {
  press: kitMotion.press,
  settle: kitMotion.settle,
  fade: kitMotion.base,
  stream: kitMotion.stream,
} as const;

/** The old dense spacing ladder, projected on the kit's 4px ladder. */
export const space = {
  xxs: kitSpace.xxs,
  xs: kitSpace.xs,
  sm: kitSpace.sm,
  md: kitSpace.md,
  lg: kitSpace.lg,
  xl: kitSpace.xl,
  xxl: kitSpace["2xl"],
} as const;
