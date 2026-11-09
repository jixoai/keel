// Small shared UI atoms. They are built from NSH's native primitives only
// (`View`, `Text`, `Scroll`, `Input`) — no second UI framework, no DOM.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Icon, type IconName } from "./icons";
import { runStateLabel, runStateTone, type RunState } from "./run-state";
import { alpha, colors, glass, motion, radius, space } from "./theme";

export function Press(props: {
  onPress?: () => void;
  onLongPress?: () => void;
  disabled?: boolean;
  testID?: string;
  style?: StyleProp<ViewStyle>;
  hoverStyle?: StyleProp<ViewStyle>;
  activeStyle?: StyleProp<ViewStyle>;
  accessibilityLabel?: string;
  accessibilityState?: { selected?: boolean; checked?: boolean; disabled?: boolean };
  /** A touch target that stays visually small but is easier to hit. */
  hitSlop?: number | { top?: number; right?: number; bottom?: number; left?: number };
  onMouseEnter?: () => void;
  onMouseLeave?: () => void;
  onFocus?: () => void;
  onBlur?: () => void;
  children: JSX.Element;
}) {
  return (
    <View
      testID={props.testID}
      onPress={props.disabled ? undefined : props.onPress}
      onLongPress={props.disabled ? undefined : props.onLongPress}
      disabled={props.disabled}
      accessibilityLabel={props.accessibilityLabel}
      accessibilityState={props.accessibilityState}
      hitSlop={props.hitSlop}
      onMouseEnter={props.onMouseEnter}
      onMouseLeave={props.onMouseLeave}
      onFocus={props.onFocus}
      onBlur={props.onBlur}
      hoverStyle={props.disabled ? undefined : (props.hoverStyle ?? { opacity: 0.9 })}
      activeStyle={props.disabled ? undefined : (props.activeStyle ?? { opacity: 0.7 })}
      style={[props.style ?? null, props.disabled ? { opacity: 0.38 } : null]}
    >
      {props.children}
    </View>
  );
}

export function Spinner(props: { size?: number; tint?: string }) {
  const size = props.size ?? 14;
  return (
    <View
      style={{
        width: size,
        height: size,
        borderRadius: size / 2,
        borderWidth: 2,
        borderColor: alpha(colors.mutedForeground, 0.25),
        borderTopColor: props.tint ?? colors.brand,
        animationName: "spin",
        animationDuration: 900,
        animationTimingFunction: "linear",
        animationIterationCount: "infinite",
      }}
    />
  );
}

export function StatusDot(props: {
  tone?: "running" | "done" | "failed" | "idle" | "brand" | "warning";
  size?: number;
  /** A parked state (waiting on a human) keeps its color but not the motion:
   * waiting is not activity. Defaults to the tone's own rule. */
  animated?: boolean;
}) {
  const size = props.size ?? 7;
  const tone = props.tone ?? "idle";
  const color =
    tone === "running"
      ? colors.statusRunning
      : tone === "brand"
        ? colors.brand
        : tone === "warning"
          ? colors.warning
          : tone === "done"
            ? colors.statusDone
            : tone === "failed"
              ? colors.danger
              : colors.mutedForeground;
  return (
    <View
      style={{
        width: size,
        height: size,
        borderRadius: size / 2,
        backgroundColor: color,
        opacity: tone === "idle" ? 0.5 : 1,
        animationName:
          props.animated === false
            ? undefined
            : tone === "running" || tone === "brand" || tone === "warning"
              ? "pulse"
              : undefined,
        animationDuration: 1200,
        animationIterationCount: "infinite",
      }}
    />
  );
}

/** The one authoritative run-state display. The header shows this pill; every
 * other surface is a quieter projection of the same vocabulary. */
export function StatusPill(props: { state: RunState }) {
  const tone = () => runStateTone[props.state];
  const fill = () =>
    tone() === "brand"
      ? colors.brandTint
      : tone() === "warning"
        ? colors.warningTint
        : tone() === "danger"
          ? colors.dangerTint
          : colors.muted;
  const ink = () =>
    tone() === "brand"
      ? colors.brand
      : tone() === "warning"
        ? colors.warning
        : tone() === "danger"
          ? colors.danger
          : colors.mutedForeground;
  const dot = (): "brand" | "warning" | "failed" | "done" | "idle" =>
    tone() === "brand" ? "brand" : tone() === "warning" ? "warning" : tone() === "danger" ? "failed" : "done";
  return (
    <View
      accessibilityLabel={`任务状态：${runStateLabel[props.state]}`}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: 5,
        paddingHorizontal: 8,
        height: 24,
        borderRadius: radius.pill,
        backgroundColor: fill(),
      }}
    >
      <StatusDot tone={dot()} size={6} />
      <Text style={{ fontSize: 10.5, fontWeight: "600", color: ink() }}>{runStateLabel[props.state]}</Text>
    </View>
  );
}

export function Badge(props: {
  label: string;
  tone?: "neutral" | "brand" | "success" | "warning" | "danger" | "info";
  icon?: IconName;
}) {
  const tone = props.tone ?? "neutral";
  const fill =
    tone === "brand"
      ? colors.brandTint
      : tone === "success"
        ? colors.successTint
        : tone === "warning"
          ? colors.warningTint
          : tone === "danger"
            ? colors.dangerTint
            : tone === "info"
              ? colors.infoTint
              : colors.muted;
  const ink =
    tone === "brand"
      ? colors.brand
      : tone === "success"
        ? colors.success
        : tone === "warning"
          ? colors.warning
          : tone === "danger"
            ? colors.danger
            : tone === "info"
              ? colors.info
              : colors.mutedForeground;
  return (
    <View
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: 4,
        paddingHorizontal: 7,
        height: 20,
        borderRadius: radius.pill,
        backgroundColor: fill,
      }}
    >
      {props.icon ? <Icon name={props.icon} size={11} tint={ink} /> : null}
      <Text style={{ fontSize: 11, fontWeight: "600", color: ink }}>{props.label}</Text>
    </View>
  );
}

export function IconButton(props: {
  icon: IconName;
  label: string;
  onPress?: () => void;
  size?: number;
  iconSize?: number;
  tint?: string;
  disabled?: boolean;
  selected?: boolean;
  hitSlop?: number;
  onFocus?: () => void;
  onBlur?: () => void;
  style?: ViewStyle;
}) {
  const size = props.size ?? 32;
  return (
    <Press
      onPress={props.onPress}
      disabled={props.disabled}
      accessibilityLabel={props.label}
      accessibilityState={{ disabled: props.disabled, selected: props.selected }}
      hitSlop={props.hitSlop}
      onFocus={props.onFocus}
      onBlur={props.onBlur}
      hoverStyle={{ backgroundColor: colors.hover }}
      activeStyle={{ backgroundColor: colors.glassStateArmed }}
      style={[
        {
          width: size,
          height: size,
          borderRadius: radius.control,
          alignItems: "center",
          justifyContent: "center",
          backgroundColor: props.selected ? colors.glassState : undefined,
          borderWidth: props.selected ? 1 : 0,
          borderColor: colors.glassStateRim,
        },
        props.style ?? null,
      ]}
    >
      <Icon name={props.icon} size={props.iconSize ?? 17} tint={props.tint ?? colors.mutedForeground} />
    </Press>
  );
}

export function Segmented<T extends string>(props: {
  options: ReadonlyArray<{ value: T; label: string; icon?: IconName; disabled?: boolean }>;
  value: T;
  onChange: (value: T) => void;
  compact?: boolean;
}) {
  return (
    <View
      style={{
        flexDirection: "row",
        padding: 3,
        gap: 2,
        borderRadius: radius.control,
        backgroundColor: colors.muted,
        borderWidth: 1,
        borderColor: colors.glassEdge,
      }}
    >
      {props.options.map((option) => {
        const selected = () => props.value === option.value;
        const disabled = () => !!option.disabled;
        return (
          <Press
            onPress={() => (disabled() ? undefined : props.onChange(option.value))}
            disabled={disabled()}
            accessibilityLabel={option.label}
            accessibilityState={{ selected: selected(), disabled: disabled() }}
            hitSlop={props.compact ? { top: 11, bottom: 11, left: 3, right: 3 } : undefined}
            hoverStyle={{ backgroundColor: colors.hover }}
            style={{
              flexDirection: "row",
              alignItems: "center",
              justifyContent: "center",
              gap: 4,
              height: props.compact ? 22 : 26,
              paddingHorizontal: props.compact ? 8 : 10,
              borderRadius: radius.control - 3,
              backgroundColor: selected() && !disabled() ? colors.glassControl : undefined,
              boxShadow: selected() && !disabled() ? `inset 0 1px 0 ${colors.glassControlRim}` : undefined,
            }}
          >
            {option.icon ? (
              <Icon
                name={option.icon}
                size={12}
                tint={disabled() ? colors.mutedForeground : selected() ? colors.foreground : colors.mutedForeground}
              />
            ) : null}
            <Text
              style={{
                fontSize: props.compact ? 11 : 12,
                fontWeight: "600",
                color: disabled() ? colors.mutedForeground : selected() ? colors.foreground : colors.mutedForeground,
              }}
            >
              {option.label}
            </Text>
          </Press>
        );
      })}
    </View>
  );
}

/** A line · label · line rule. Every in-conversation divider (compaction,
 * a new work segment) uses this one language. */
export function Divider(props: {
  label?: string;
  icon?: IconName;
  tone?: "muted" | "brand" | "warning";
  style?: StyleProp<ViewStyle>;
}) {
  const tone = props.tone ?? "muted";
  const ink =
    tone === "brand" ? colors.brand : tone === "warning" ? colors.warning : colors.mutedForeground;
  return (
    <View
      style={[
        { flexDirection: "row", alignItems: "center", gap: space.md, paddingVertical: space.sm },
        props.style ?? null,
      ]}
    >
      <View style={{ flex: 1, height: 1, backgroundColor: colors.border }} />
      {props.label ? (
        <View style={{ flexDirection: "row", alignItems: "center", gap: 4 }}>
          {props.icon ? <Icon name={props.icon} size={11} tint={ink} /> : null}
          <Text style={{ fontSize: 11, fontWeight: "600", color: ink }}>{props.label}</Text>
        </View>
      ) : null}
      <View style={{ flex: 1, height: 1, backgroundColor: colors.border }} />
    </View>
  );
}

export function Card(props: {
  children: JSX.Element | JSX.Element[];
  style?: ViewStyle;
  tone?: "card" | "panel" | "popover" | "none";
}) {
  const tone = props.tone ?? "card";
  const face = tone === "none" ? {} : glass[tone];
  return (
    <View style={[{ borderRadius: radius.card, ...face }, props.style ?? null]}>{props.children}</View>
  );
}

export function PillButton(props: {
  label: string;
  icon?: IconName;
  onPress?: () => void;
  tone?: "primary" | "secondary" | "danger" | "destructive" | "ghost";
  disabled?: boolean;
  compact?: boolean;
  width?: ViewStyle["width"];
}) {
  const tone = props.tone ?? "secondary";
  const bg =
    tone === "primary"
      ? colors.primary
      : tone === "destructive"
        ? colors.destructive
        : tone === "danger"
          ? colors.dangerTint
          : tone === "ghost"
            ? undefined
            : colors.glassControl;
  const fg =
    tone === "primary"
      ? colors.primaryForeground
      : tone === "destructive"
        ? colors.destructiveForeground
        : tone === "danger"
          ? colors.danger
          : colors.foreground;
  return (
    <Press
      onPress={props.onPress}
      disabled={props.disabled}
      accessibilityLabel={props.label}
      hoverStyle={{ opacity: 0.92 }}
      activeStyle={{ opacity: 0.7 }}
      style={{
        flexDirection: "row",
        alignItems: "center",
        justifyContent: "center",
        gap: 6,
        height: props.compact ? 28 : 34,
        paddingHorizontal: props.compact ? 10 : 14,
        width: props.width,
        borderRadius: radius.control,
        backgroundColor: bg,
        borderWidth: tone === "primary" || tone === "destructive" ? 0 : 1,
        borderColor: tone === "ghost" ? "transparent" : colors.glassControlRim,
        boxShadow:
          tone === "primary"
            ? `inset 0 1px 0 ${alpha(colors.primaryForeground, 0.25)}`
            : tone === "destructive"
              ? `inset 0 1px 0 ${alpha(colors.destructiveForeground, 0.25)}`
              : undefined,
      }}
    >
      {props.icon ? <Icon name={props.icon} size={14} tint={fg} /> : null}
      <Text style={{ fontSize: 12, fontWeight: "600", color: fg }}>{props.label}</Text>
    </Press>
  );
}

/** A quiet animated caret for streaming text. */
export function Caret(props: { tint?: string }) {
  return (
    <View
      style={{
        width: 2,
        height: 16,
        marginLeft: 2,
        borderRadius: 1,
        backgroundColor: props.tint ?? colors.brand,
        animationName: "pulse",
        animationDuration: 900,
        animationIterationCount: "infinite",
      }}
    />
  );
}

/** The thin rule used between tool rows and message metadata. */
export const hairline = () => ({ height: 1, backgroundColor: colors.border });

export const pressTransition = {
  transitionProperty: "opacity, background-color",
  transitionDuration: motion.fade,
  transitionTimingFunction: "ease-out",
} as const;
