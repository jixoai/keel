// C4 · Toast — shadcn/ui `Toast`/`Toaster` on NSH's native `nsh/base` Toast +
// ToastStack: the swipe-to-dismiss gesture, the stack's spring layout and the
// ARIA alert root are the component's; the kit carries the card face and the
// placement. The host may place `ToastViewport` in a root view (the coordinator's
// note: `nsh` Overlay works too, when the app wants it above everything).
//
// Props — Toast
// | prop | type | default | notes |
// |---|---|---|---|
// | title | string? | – | the message ("已保存") |
// | description | string? | – | the second line |
// | tone | "default" \| "success" \| "warning" \| "danger" | "default" | tint family |
// | action | JSX.Element? | – | a Button/Link the user can press |
// | onDismiss | () => void? | – | × pressed, or swiped past the threshold |
// | children | JSX.Element? | – | richer body |
// | accessibilityLabel | string? | title | what screen readers announce |
// | testID | string? | – | `id:` selector for tests |
//
// Props — ToastViewport
// | prop | type | default | notes |
// |---|---|---|---|
// | toasts | ToastItem[] | – | oldest first: `{ id, title, description?, tone? }` |
// | onDismiss | (id: string) => void? | – | a toast was dismissed |
// | placement | Anchor | "bottomRight" | the edge the stack anchors to |
// | width | number? | 360 | the stack's width |
// | testID | string? | – | `id:` selector for tests |
//
// States: default · success/warning/danger · with/without action · dismissed
// (removed from `toasts`). TUI: only the newest toast shows, on the viewport's
// status line.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View } from "nsh";
import { Toast as NativeToast, ToastStack as NativeToastStack, type Anchor, type ToastTransitionStatus } from "nsh/base";
import { color, radius, space, typeScale, weight, zIndex } from "../tokens";
import { IconButton } from "./button";

export type ToastTone = "default" | "success" | "warning" | "danger";

export interface ToastItem {
  id: string;
  title: string;
  description?: string;
  tone?: ToastTone;
}

const toastTint = (tone: ToastTone): { face: string; edge: string; ink: string } => {
  switch (tone) {
    case "success":
      return { face: color.successTint, edge: color.success, ink: color.success };
    case "warning":
      return { face: color.warningTint, edge: color.warning, ink: color.warning };
    case "danger":
      return { face: color.dangerTint, edge: color.danger, ink: color.danger };
    case "default":
    default:
      return { face: color.popover, edge: color.popoverEdge, ink: color.popoverForeground };
  }
};

export function Toast(props: {
  title?: string;
  description?: string;
  tone?: ToastTone;
  action?: JSX.Element;
  onDismiss?: () => void;
  transitionStatus?: ToastTransitionStatus;
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const tint = () => toastTint(props.tone ?? "default");
  return (
    <NativeToast
      testID={props.testID}
      transitionStatus={props.transitionStatus}
      onDismissRequest={props.onDismiss}
      accessibilityLabel={props.accessibilityLabel ?? props.title}
      style={[{ flexDirection: "column" }, props.style ?? null]}
    >
      <View
        style={{
          flexDirection: "row",
          alignItems: "flex-start",
          gap: space.md,
          minWidth: 280,
          padding: space.lg,
          borderRadius: radius.lg,
          borderWidth: 1,
          borderColor: tint().edge,
          backgroundColor: tint().face,
          boxShadow: `0 16px 40px ${color.glassShade}`,
        }}
      >
        <View style={{ flex: 1, flexDirection: "column", gap: space.xxs }}>
          {props.title !== undefined ? (
            <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.foreground }}>
              {props.title}
            </Text>
          ) : null}
          {props.description !== undefined ? (
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{props.description}</Text>
          ) : null}
          {props.children}
        </View>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          {props.action}
          {/* A real button, not a `×` inside a `Text`: NSH's `Text` takes no
              `focusable` / `focusStyle` / `hitSlop`, so the old control could
              not join the tab order and drew no focus (2.4.7), and its glyph
              was under 2.5.8's 24px target. The kit's smallest icon button is a
              24px square with the ring every other control carries; the
              accessible name stays "关闭提示", which is also the existing
              test's selector. */}
          <IconButton icon="x" size="sm" label="关闭提示" onPress={props.onDismiss} />
        </View>
      </View>
    </NativeToast>
  );
}

export function ToastViewport(props: {
  toasts?: ToastItem[];
  onDismiss?: (id: string) => void;
  placement?: Anchor;
  width?: number;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const placement = () => props.placement ?? "bottomRight";
  const atTop = () => placement().startsWith("top");
  const atLeft = () => placement().endsWith("Left");
  return (
    <View
      testID={props.testID}
      style={[
        {
          position: "absolute",
          left: atLeft() ? space.xl : undefined,
          right: atLeft() ? undefined : space.xl,
          top: atTop() ? space.xl : undefined,
          bottom: atTop() ? undefined : space.xl,
          width: props.width ?? 360,
          zIndex: zIndex.toast,
          // The viewport box itself is a pass-through: only its toasts take presses.
          pointerEvents: "box-none",
        },
        props.style ?? null,
      ]}
    >
      <NativeToastStack placement={placement()} style={{ flexDirection: "column", gap: space.sm }}>
        <Each of={props.toasts ?? []} key={(item) => item.id}>
          {(item) => (
            <Toast
              title={item().title}
              description={item().description}
              tone={item().tone}
              onDismiss={() => props.onDismiss?.(item().id)}
              testID={`toast-${item().id}`}
            />
          )}
        </Each>
      </NativeToastStack>
    </View>
  );
}
