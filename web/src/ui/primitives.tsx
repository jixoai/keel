// Small shared UI atoms. They are built from NSH's native primitives only
// (`View`, `Text`, `Scroll`, `Input`) — no second UI framework, no DOM.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Icon, type IconName } from "./icons";
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

export function StatusDot(props: { tone?: "running" | "done" | "failed" | "idle"; size?: number }) {
  const size = props.size ?? 7;
  const tone = props.tone ?? "idle";
  const color =
    tone === "running"
      ? colors.statusRunning
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
        animationName: tone === "running" ? "pulse" : undefined,
        animationDuration: 1200,
        animationIterationCount: "infinite",
      }}
    />
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
  style?: ViewStyle;
}) {
  const size = props.size ?? 32;
  return (
    <Press
      onPress={props.onPress}
      disabled={props.disabled}
      accessibilityLabel={props.label}
      accessibilityState={{ disabled: props.disabled, selected: props.selected }}
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
  options: ReadonlyArray<{ value: T; label: string; icon?: IconName }>;
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
        return (
          <Press
            onPress={() => props.onChange(option.value)}
            accessibilityLabel={option.label}
            accessibilityState={{ selected: selected() }}
            hoverStyle={{ backgroundColor: colors.hover }}
            style={{
              flexDirection: "row",
              alignItems: "center",
              justifyContent: "center",
              gap: 4,
              height: props.compact ? 22 : 26,
              paddingHorizontal: props.compact ? 8 : 10,
              borderRadius: radius.control - 3,
              backgroundColor: selected() ? colors.glassControl : undefined,
              boxShadow: selected() ? `inset 0 1px 0 ${colors.glassControlRim}` : undefined,
            }}
          >
            {option.icon ? (
              <Icon
                name={option.icon}
                size={12}
                tint={selected() ? colors.foreground : colors.mutedForeground}
              />
            ) : null}
            <Text
              style={{
                fontSize: props.compact ? 11 : 12,
                fontWeight: "600",
                color: selected() ? colors.foreground : colors.mutedForeground,
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

export function Divider(props: { label?: string; style?: StyleProp<ViewStyle> }) {
  return (
    <View
      style={[
        { flexDirection: "row", alignItems: "center", gap: space.md, paddingVertical: space.sm },
        props.style ?? null,
      ]}
    >
      <View style={{ flex: 1, height: 1, backgroundColor: colors.border }} />
      {props.label ? (
        <Text style={{ fontSize: 11, fontWeight: "600", color: colors.mutedForeground }}>
          {props.label}
        </Text>
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
  tone?: "primary" | "secondary" | "danger" | "ghost";
  disabled?: boolean;
  compact?: boolean;
  width?: ViewStyle["width"];
}) {
  const tone = props.tone ?? "secondary";
  const bg =
    tone === "primary"
      ? colors.primary
      : tone === "danger"
        ? colors.dangerTint
        : tone === "ghost"
          ? undefined
          : colors.glassControl;
  const fg =
    tone === "primary"
      ? colors.primaryForeground
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
        borderWidth: tone === "primary" ? 0 : 1,
        borderColor: tone === "ghost" ? "transparent" : colors.glassControlRim,
        boxShadow: tone === "primary" ? `inset 0 1px 0 ${alpha(colors.primaryForeground, 0.25)}` : undefined,
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
