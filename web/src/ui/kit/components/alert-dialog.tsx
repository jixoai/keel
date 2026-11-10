// C4 · AlertDialog — shadcn/ui `AlertDialog`: a modal that interrupts and asks,
// built on the native `nsh/base` Dialog layer (Escape/backdrop policy included)
// plus the kit's Button pair, so confirm/cancel look the same everywhere.
//
// NSH note: `nsh/base` also exports a native `AlertDialog` *module* (a system
// alert shown with `AlertDialog.show(...)`, promise-based, not part of the view
// tree). This component is the tree-native equivalent (testable in TUI, styleable
// with kit tokens); the native module stays available for platform-native
// alerts.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | open | boolean? | true | shown while true |
// | title / description | string? | – | the question and its explanation |
// | confirmLabel | string? | "确认" | the primary action |
// | cancelLabel | string? | "取消" | the secondary action; omit to hide it |
// | destructive | boolean? | false | the confirm runs in the destructive face |
// | onConfirm | () => void? | – | confirm pressed (the app closes the dialog) |
// | onCancel | () => void? | – | cancel / Escape / backdrop |
// | closeOnEscape / closeOnBackdropPress | boolean? | true | native close policy |
// | accessibilityLabel | string? | title | what screen readers call the alert |
// | testID | string? | – | `id:` selector for tests |
//
// States: open · destructive open · cancel-less (single action) · closed. TUI:
// the title, description and both actions are text rows; `press` on them fires
// `onConfirm` / `onCancel`.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text, View, windowSize } from "nsh";
import { Dialog as NativeDialog } from "nsh/base";
import { Icon } from "../../icons";
import { alpha, color, radius, space, typeScale, weight } from "../tokens";
import { Button } from "./button";

/** Where the footer stops being one row and stacks, shadcn's `sm` (`flex-col-reverse
 * gap-2 sm:flex-row sm:justify-end`): below it the two buttons would push each
 * other off a long-labelled dialog (`取消删除密钥 <name>` and its confirm). */
const ALERT_DIALOG_FOOTER_STACK_BELOW_PX = 640;

const footerStacked = () => windowSize().width < ALERT_DIALOG_FOOTER_STACK_BELOW_PX;

export function AlertDialog(props: {
  open?: boolean;
  title?: string;
  description?: string;
  children?: JSX.Element;
  confirmLabel?: string;
  cancelLabel?: string;
  destructive?: boolean;
  onConfirm?: () => void;
  onCancel?: () => void;
  closeOnEscape?: boolean;
  closeOnBackdropPress?: boolean;
  accessibilityLabel?: string;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  return (
    <NativeDialog
      open={props.open ?? true}
      testID={props.testID}
      closeOnEscape={props.closeOnEscape ?? true}
      closeOnBackdropPress={props.closeOnBackdropPress ?? true}
      onOpenChange={() => props.onCancel?.()}
      // One scrim for every modal: the theme's shade at full strength (60%
      // black in the dark appearance) and the reference's `backdrop-blur-xs`.
      backdropStyle={{ backgroundColor: color.glassShade, backdropFilter: "blur(4px)" }}
      style={[
        {
          width: 400,
          maxWidth: "92%",
          flexDirection: "column",
          gap: space.lg,
          padding: space.xl,
          borderRadius: radius.xl,
          borderWidth: 1,
          borderColor: color.popoverEdge,
          backgroundColor: color.popover,
          boxShadow: `0 24px 64px ${alpha(color.glassShade, 1)}`,
          // shadcn's placement (`fixed top-1/2 left-1/2 -translate-x-1/2
          // -translate-y-1/2`): the dialog layer is a full-window column, so the
          // cross axis centers with `alignSelf` and the main axis with auto
          // margins. Without these the popup sits at the window's top-left
          // corner — the runtime gives the layer no placement of its own.
          alignSelf: "center",
          marginTop: "auto",
          marginBottom: "auto",
        },
        props.style ?? null,
      ]}
    >
      <View
        role="alert"
        accessibilityLabel={props.accessibilityLabel ?? props.title}
        style={{ flexDirection: "column", gap: space.lg }}
      >
      <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.md }}>
        <View
          style={{
            width: 28,
            height: 28,
            borderRadius: radius.pill,
            alignItems: "center",
            justifyContent: "center",
            backgroundColor: props.destructive ? color.dangerTint : color.infoTint,
          }}
        >
          <Icon
            name={props.destructive ? "alert" : "message"}
            size={15}
            tint={props.destructive ? color.danger : color.info}
          />
        </View>
        <View style={{ flex: 1, flexDirection: "column", gap: space.xs }}>
          {props.title !== undefined ? (
            <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.popoverForeground }}>
              {props.title}
            </Text>
          ) : null}
          {props.description !== undefined ? (
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{props.description}</Text>
          ) : null}
        </View>
      </View>
      {props.children}
      <View
        style={{
          flexDirection: footerStacked() ? "column-reverse" : "row",
          // A row keeps its line only while both labels fit: a long question
          // (`确认删除密钥 <name>`) wraps the second button onto its own line
          // instead of pushing the first one out of the dialog.
          flexWrap: footerStacked() ? undefined : "wrap",
          alignItems: footerStacked() ? "stretch" : undefined,
          justifyContent: footerStacked() ? undefined : "flex-end",
          gap: footerStacked() ? space.sm : space.md,
        }}
      >
        {props.cancelLabel !== undefined ? (
          <Button
            variant="outline"
            label={props.cancelLabel}
            onPress={() => {
              props.onCancel?.();
            }}
            testID={props.testID !== undefined ? `${props.testID}-cancel` : undefined}
          />
        ) : null}
        <Button
          variant={props.destructive ? "destructive" : "default"}
          label={props.confirmLabel ?? "确认"}
          onPress={props.onConfirm}
          testID={props.testID !== undefined ? `${props.testID}-confirm` : undefined}
        />
      </View>
      </View>
    </NativeDialog>
  );
}
