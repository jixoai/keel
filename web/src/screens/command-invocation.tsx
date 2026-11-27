// One `command.invoked` line (change keel-plugin-commands, task 4.2).
//
// A command is a named USER action and its durable invocation record is the
// audit/projection truth: this row is explicitly NOT a user or assistant
// message (no avatar, no Keel header, no Markdown answer). It shows the command
// name, where it came from, danger, status, duration and — when the invocation
// produced one — the bounded structured result, plus the typed failure code and
// message for error/refused/timed-out/cancelled. `/compact` still renders its
// own compaction divider; this row is the invocation record beside it.
import { Text, View } from "nsh";
import type { CommandInvocationView } from "../data/contract";
import {
  commandResultSummary,
  commandSourceLabel,
  commandStatusLabel,
  commandStatusTone,
} from "../data/commands";
import { formatDuration } from "../data/wire";
import {
  Badge,
  Card,
  color,
  font,
  iconSize,
  lineHeight,
  space,
  typeScale,
  weight,
} from "../ui/kit";
import { Icon } from "../ui/icons";

export function CommandInvocationRow(props: { invocation: () => CommandInvocationView }) {
  const row = () => props.invocation();
  const pending = () => row().status === "pending_decision" || row().status === "running";
  const failed = () =>
    row().status === "error" ||
    row().status === "refused" ||
    row().status === "timed_out" ||
    row().status === "cancelled";
  const summary = () => commandResultSummary(row().data);
  return (
    <Card
      size="sm"
      testID={`command-invocation-${row().requestId}`}
      accessibilityLabel={`命令 /${row().command} ${commandStatusLabel(row().status)}`}
    >
      <View style={{ gap: space.sm }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
          <Icon name="terminal" size={iconSize.xs} tint={color.mutedForeground} />
          <Text
            style={{
              fontSize: typeScale.base,
              fontWeight: weight.semibold,
              color: color.foreground,
              fontFamily: font.mono,
            }}
          >
            {`/${row().command}`}
          </Text>
          <Badge
            variant="outline"
            label={commandSourceLabel(row().source)}
            accessibilityLabel={`来源 ${commandSourceLabel(row().source)}`}
          />
          <Badge
            variant={commandStatusTone(row().status)}
            label={commandStatusLabel(row().status)}
            accessibilityLabel={`状态 ${commandStatusLabel(row().status)}`}
            testID={`command-invocation-status-${row().requestId}`}
          />
          <If when={!!formatDuration(row().durationMs)}>
            <Text
              style={{
                fontSize: typeScale.xs,
                color: color.mutedForeground,
                fontVariant: ["tabular-nums"],
              }}
            >
              {formatDuration(row().durationMs)}
            </Text>
          </If>
          <If when={!!row().truncated}>
            <Badge variant="warning" label="已截断" accessibilityLabel="结果已截断" />
          </If>
        </View>
        <If when={!!summary()}>
          <Text
            testID={`command-invocation-result-${row().requestId}`}
            style={{
              fontFamily: font.mono,
              fontSize: typeScale.xs,
              lineHeight: Math.round(typeScale.xs * lineHeight.normal),
              color: color.mutedForeground,
              numberOfLines: 4,
            }}
          >
            {`结果：${summary()}`}
          </Text>
        </If>
        <If when={failed()}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}>
            <Icon name="alert" size={iconSize.xs} tint={color.destructive} />
            <Text
              testID={`command-invocation-error-${row().requestId}`}
              style={{
                flex: 1,
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                color: color.destructive,
              }}
            >
              {row().error || `命令${commandStatusLabel(row().status)}`}
            </Text>
            <If when={!!row().errorCode}>
              <Text style={{ fontSize: typeScale["2xs"], color: color.mutedForeground, fontFamily: font.mono }}>
                {row().errorCode ?? ""}
              </Text>
            </If>
          </View>
        </If>
        <If when={pending()}>
          <Text
            testID={`command-invocation-pending-${row().requestId}`}
            style={{
              fontSize: typeScale.xs,
              lineHeight: Math.round(typeScale.xs * lineHeight.normal),
              color: color.info,
            }}
          >
            {row().status === "pending_decision"
              ? "等待审批：允许后自动执行一次；拒绝或超时都不产生副作用。"
              : "命令正在执行。"}
          </Text>
        </If>
      </View>
    </Card>
  );
}
