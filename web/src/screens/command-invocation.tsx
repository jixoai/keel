// One `command.invoked` line (change keel-plugin-commands, task 4.2).
//
// A command is a named USER action and its durable invocation record is the
// audit/projection truth: this row is explicitly NOT a user or assistant
// message (no avatar, no Keel header, no Markdown answer). It shows the command
// name, where it came from, danger, status, duration and — when the invocation
// produced one — the bounded structured result, plus the typed failure code and
// message for error/refused/timed-out/cancelled. `/compact` still renders its
// own compaction divider; this row is the invocation record beside it.
import { Clipboard, platform, signal, Text, View } from "nsh";
import type { CommandInvocationView } from "../data/contract";
import {
  cleanDisplayText,
  commandErrorText,
  commandResultDetail,
  commandResultSummary,
  commandSourceLabel,
  commandStatusLabel,
  commandStatusTone,
} from "../data/commands";
import { formatDuration } from "../data/wire";
import { store } from "../data/store";
import {
  Badge,
  Button,
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

const expandedRows = signal<Record<string, boolean>>({});
const copiedRow = signal<string | null>(null);

export function CommandInvocationRow(props: { invocation: () => CommandInvocationView }) {
  const row = () => props.invocation();
  const local = () => store.commandLocalOutcome(row().requestId);
  const status = () => local()?.status ?? row().status;
  const detail = () => commandResultDetail(row().data);
  const errorText = () => local()?.error ?? commandErrorText(row().errorCode, row().error);
  const copyText = () => detail() || errorText();
  const expanded = () => !!expandedRows()[row().requestId];
  const toggleExpanded = () => {
    expandedRows.update((map) => ({ ...map, [row().requestId]: !map[row().requestId] }));
  };
  const copy = () => {
    void Clipboard.writeText(copyText()).then(
      () => {
        copiedRow.set(row().requestId);
        setTimeout(() => {
          if (copiedRow() === row().requestId) copiedRow.set(null);
        }, 1600) as unknown as number;
      },
      (error: unknown) => {
        store.flash({
          tone: "danger",
          text: `复制失败：${error instanceof Error ? error.message : String(error)}`,
        });
      },
    );
  };
  const pending = () => status() === "pending_decision" || status() === "running";
  const failed = () =>
    status() === "error" ||
    status() === "refused" ||
    status() === "timed_out" ||
    status() === "cancelled";
  const summary = () => commandResultSummary(row().data);
  return (
    <Card
      size="sm"
      testID={`command-invocation-${row().requestId}`}
      accessibilityLabel={`命令 /${row().command} ${commandStatusLabel(status())}`}
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
            {`/${cleanDisplayText(row().command)}`}
          </Text>
          <Badge
            variant="outline"
            label={cleanDisplayText(commandSourceLabel(row().source))}
            accessibilityLabel={`来源 ${cleanDisplayText(commandSourceLabel(row().source))}`}
          />
          <Badge
            variant={commandStatusTone(status())}
            label={commandStatusLabel(status())}
            accessibilityLabel={`状态 ${commandStatusLabel(status())}`}
            testID={`command-invocation-status-${row().requestId}`}
          />
          <If when={!!local()}>
            <Badge
              variant="outline"
              label="本地记录"
              accessibilityLabel="本地错误：该请求未产生审计行"
              testID={`command-invocation-local-${row().requestId}`}
            />
          </If>
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
              {errorText()}
            </Text>
            <If when={!!row().errorCode}>
              <Text style={{ fontSize: typeScale["2xs"], color: color.mutedForeground, fontFamily: font.mono }}>
                {row().errorCode ?? ""}
              </Text>
            </If>
          </View>
        </If>
        <If when={!!detail() || !!copyText()}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
            <Button
              size="sm"
              variant="ghost"
              label={expanded() ? "收起详情" : "展开详情"}
              accessibilityLabel={expanded() ? "收起命令详情" : "展开命令详情"}
              testID={`command-invocation-toggle-${row().requestId}`}
              onPress={() => toggleExpanded()}
            />
            <If when={platform.idiom !== "terminal"}>
              <Button
                size="sm"
                variant="ghost"
                label={copiedRow() === row().requestId ? "已复制" : "复制结果"}
                accessibilityLabel={copiedRow() === row().requestId ? "已复制命令结果" : "复制命令结果"}
                testID={`command-invocation-copy-${row().requestId}`}
                onPress={() => copy()}
              />
            </If>
          </View>
        </If>
        <If when={expanded()}>
          <Text
            testID={`command-invocation-detail-${row().requestId}`}
            style={{
              fontFamily: font.mono,
              fontSize: typeScale.xs,
              lineHeight: Math.round(typeScale.xs * lineHeight.normal),
              color: color.mutedForeground,
            }}
          >
            {detail() || errorText()}
          </Text>
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
            {status() === "pending_decision"
              ? "等待审批：允许后自动执行一次；拒绝或超时都不产生副作用。"
              : "命令正在执行。"}
          </Text>
        </If>
      </View>
    </Card>
  );
}
