// One declared CLI capability, shared by the plugin page and the settings
// 「团队」section's Team CLI panel (change keel-agent-team-v1, tasks P5).
//
// The row is the manifest's own declaration, verbatim: command path, danger
// class, idempotency, the permissions the caller must hold and the surfaces
// allowed to call it. Nothing here invents a command the plugin does not
// declare, and nothing the plugin declares is hidden. The copy action reports
// the platform's own answer next to the command instead of swallowing it.
import { Clipboard, signal, Text, View } from "nsh";
import type { CliCapabilityView } from "../data/contract";
import {
  Badge,
  Button,
  color,
  font,
  lineHeight,
  space,
  typeScale,
  type BadgeVariant,
} from "../ui/kit";

/** The last clipboard write, per capability command: what `Clipboard.writeText`
 * answered. A platform without a writable clipboard reports it here instead of
 * failing silently; the button stays reachable either way. */
const copyOutcome = signal<{ key: string; ok: boolean; text: string } | null>(null);

const messageOf = (error: unknown): string =>
  error instanceof Error ? error.message : typeof error === "string" ? error : "未知错误";

/** Put one declared command on the system clipboard. The platform's own answer
 * is reported next to the command, never swallowed. A platform whose clipboard
 * is read-only (or absent) rejects the call — the command itself stays on
 * screen and selectable, the button stays reachable, and the row says why the
 * copy did not happen. */
const copyCommand = async (key: string, command: string) => {
  try {
    await Clipboard.writeText(command);
    copyOutcome.set({ key, ok: true, text: "已复制到剪贴板" });
  } catch (error) {
    copyOutcome.set({
      key,
      ok: false,
      text: `复制失败：${messageOf(error)}。命令仍在屏幕上，可手动选择复制。`,
    });
  }
};

/** The danger badge of one capability: the closed set read|write|destructive,
 * rendered by what a caller must expect (a retry hint, a human confirmation),
 * never by colour alone. */
const dangerVariant = (danger: string | undefined): BadgeVariant =>
  danger === "destructive" ? "destructive" : danger === "write" ? "warning" : "outline";
const dangerLabel = (danger: string | undefined): string =>
  danger === "destructive" ? "破坏性" : danger === "write" ? "写入" : "只读";

/** One `callable_by` value in the reader's words. */
const callerLabel = (caller: string): string =>
  caller === "team_captain" ? "队长" : caller === "agent" ? "Agent" : "人工";

/** The command as a reader types it below `keel`. */
export const cliCommandOf = (capability: CliCapabilityView): string =>
  `keel ${capability.path.join(" ")}`;

/** One declared CLI capability (`provides.cli`): the command path, the risk
 * attributes the manifest declares, the permissions the caller must hold, the
 * one-line summary, and the copy action. */
export function CliCapabilityRow(props: { capability: CliCapabilityView }) {
  const command = cliCommandOf(props.capability);
  const callers = (props.capability.callable_by ?? []).map(callerLabel).join(" · ");
  // The manifest's permission names are the declaration's own vocabulary (the
  // same names the capability catalog serves); the row renders them verbatim
  // rather than translating an open vocabulary into a closed one.
  const permissions = () => props.capability.permissions ?? [];
  const copy = () => {
    const outcome = copyOutcome();
    return outcome && outcome.key === props.capability.path.join(" ") ? outcome : null;
  };
  return (
    <View
      testID={`cli-capability-${props.capability.path.join("-")}`}
      style={{ gap: space.xxs, paddingVertical: space.xs }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <Text
          style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.foreground, userSelect: "text" }}
        >
          {command}
        </Text>
        <Badge variant={dangerVariant(props.capability.danger)} label={dangerLabel(props.capability.danger)} />
        <If when={props.capability.idempotent === true}>
          <Badge variant="outline" label="可重试" />
        </If>
        <Button
          variant="ghost"
          size="sm"
          icon="copy"
          label="复制命令"
          testID={`capability-copy-${props.capability.path.join("-")}`}
          accessibilityLabel={`复制命令 ${command}`}
          onPress={() => void copyCommand(props.capability.path.join(" "), command)}
        />
      </View>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <If when={permissions().length > 0}>
          <Text style={{ fontSize: typeScale["2xs"], color: color.mutedForeground }}>
            {`权限：${permissions().join(" · ")}`}
          </Text>
        </If>
        <If when={callers.length > 0}>
          <Text style={{ fontSize: typeScale["2xs"], color: color.mutedForeground }}>{`可调用：${callers}`}</Text>
        </If>
      </View>
      <Text
        style={{
          fontSize: typeScale.xs,
          lineHeight: Math.round(typeScale.xs * lineHeight.normal),
          color: color.mutedForeground,
        }}
      >
        {props.capability.summary}
      </Text>
      <If when={!!copy()}>
        <Text
          style={{
            fontSize: typeScale["2xs"],
            color: copy()?.ok ? color.mutedForeground : color.danger,
          }}
        >
          {copy()?.text ?? ""}
        </Text>
      </If>
    </View>
  );
}
