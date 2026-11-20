// The captain timeline's team face (change keel-session-surfaces-v1, S4):
// member task cards as their own region, never as transcript rows.
//
// The cards are the tasklog projection of the child sessions (`taskId`,
// `title`, `status`, `updatedAt`). The wire has no result/summary field, so the
// panel says "成员任务状态" and prints the card's own status — it never invents
// a member's result text. Rendering sits beside the conversation list (a
// sibling region), not inside `rowsOf`, so a card cannot become a user,
// assistant or tool row.
import { Text, View } from "nsh";
import type { TeamTaskView } from "../data/contract";
import { formatTime } from "../data/wire";
import { store } from "../data/store";
import { Icon } from "../ui/icons";
import {
  Badge,
  Button,
  Card,
  CardHeader,
  IconButton,
  ScrollArea,
  Skeleton,
  color,
  iconSize,
  lineHeight,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../ui/kit";

const STATUS_LABEL: Record<string, string> = {
  open: "待开始",
  in_progress: "进行中",
  blocked: "阻塞",
  done: "已完成",
  closed: "已关闭",
};

const statusLabel = (status: string): string => STATUS_LABEL[status] ?? (status || "未知状态");

const statusVariant = (status: string): BadgeVariant =>
  status === "done"
    ? "success"
    : status === "in_progress"
      ? "info"
      : status === "blocked"
        ? "warning"
        : status === "closed"
          ? "secondary"
          : status === "open"
            ? "outline"
            : "secondary";

const cardTitle = (task: TeamTaskView): string => (task.title ?? "").trim() || "未命名成员任务";

/** One card row: title, status and update time. The second line is the honest
 * projection sentence, not a result summary. */
function TeamTaskRow(props: { task: TeamTaskView }) {
  const label = () => statusLabel(props.task.status);
  const updated = () => formatTime(props.task.updatedAt);
  return (
    <View
      testID={`team-task-${props.task.taskId}`}
      style={{
        flexDirection: "column",
        gap: space.xxs,
        paddingVertical: space.sm,
        borderBottomWidth: 1,
        borderColor: color.border,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <Text
          style={{
            flex: 1,
            numberOfLines: 1,
            textOverflow: "ellipsis",
            fontSize: typeScale.sm,
            fontWeight: weight.medium,
            color: color.foreground,
          }}
        >
          {cardTitle(props.task)}
        </Text>
        <Badge
          testID={`team-task-status-${props.task.taskId}`}
          variant={statusVariant(props.task.status)}
          label={label()}
        />
      </View>
      <Text
        style={{
          numberOfLines: 2,
          fontSize: typeScale.xs,
          lineHeight: Math.round(typeScale.xs * lineHeight.normal),
          color: color.mutedForeground,
        }}
      >
        {props.task.origin === "subagent" ? "成员任务" : "任务卡"} · 成员任务状态：{label()}
        {updated() ? ` · 更新于 ${updated()}` : ""}
      </Text>
    </View>
  );
}

/** The team activity region. It only exists for a team session; a standard
 * session's timeline has no members to report. */
export function TeamActivityPanel() {
  const tasks = () => store.teamTasks();
  const loading = () => store.teamTasksLoading() && tasks().length === 0;
  const denied = () => store.teamTasksErrorStatus() === 403;
  const failed = () => !!store.teamTasksError();
  const empty = () => !loading() && !failed() && tasks().length === 0;
  return (
    <If when={!!store.sessionTeam()}>
      <Card
        testID="team-activity"
        accessibilityLabel="团队动态：成员任务卡"
        size="sm"
        style={{ marginBottom: space.md }}
      >
        <CardHeader
          title="团队动态"
          description="成员任务卡汇总在这里；它们不写入队长与你的对话记录。"
          action={
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}>
              <If when={tasks().length > 0}>
                <Badge
                  variant="secondary"
                  label={`${tasks().length}${store.teamTasksHasMore() ? "+" : ""} 项`}
                />
              </If>
              <IconButton
                testID="team-activity-refresh"
                icon="refresh"
                label="刷新团队动态"
                disabled={store.teamTasksLoading()}
                onPress={() => void store.loadTeamTasks()}
              />
            </View>
          }
        />
        <If when={loading()}>
          <View testID="team-activity-loading" style={{ flexDirection: "column", gap: space.sm }}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>正在读取成员任务…</Text>
            <Skeleton lines={2} />
          </View>
        </If>
        <If when={denied()}>
          <View
            testID="team-activity-permission"
            style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm }}
          >
            <Icon name="key" size={iconSize.sm} tint={color.warning} />
            <Text
              style={{
                flex: 1,
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                color: color.foreground,
              }}
            >
              没有查看成员任务的权限：{store.teamTasksError() ?? ""}
            </Text>
          </View>
        </If>
        <If when={failed() && !denied()}>
          <View testID="team-activity-error" style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm }}>
            <Icon name="alert" size={iconSize.sm} tint={color.danger} />
            <Text
              style={{
                flex: 1,
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                color: color.foreground,
              }}
            >
              成员任务读取失败：{store.teamTasksError() ?? ""}
            </Text>
            <Button
              variant="outline"
              size="sm"
              label="重试"
              accessibilityLabel="重试读取成员任务"
              onPress={() => void store.loadTeamTasks()}
            />
          </View>
        </If>
        <If when={empty()}>
          <Text
            testID="team-activity-empty"
            style={{
              fontSize: typeScale.sm,
              lineHeight: Math.round(typeScale.sm * lineHeight.normal),
              color: color.mutedForeground,
            }}
          >
            还没有成员任务。队长委派成员后，成员的任务卡会出现在这里。
          </Text>
        </If>
        <If when={!loading() && !failed() && tasks().length > 0}>
          <ScrollArea testID="team-activity-list" maxHeight={220}>
            <View style={{ flexDirection: "column" }}>
              <Each of={tasks()} key={(task) => task.taskId}>
                {(task) => <TeamTaskRow task={task()} />}
              </Each>
            </View>
          </ScrollArea>
        </If>
      </Card>
    </If>
  );
}
