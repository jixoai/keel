// BoardThread + BoardTaskCard + the action dialogs — keel-board-v1 tasks
// 15.2/15.3/15.4.
//
// The task page is a working log, not a dashboard: the root task card sits
// above/next to its thread, the actions come from the server's `moves` only,
// and every dangerous action asks once with its consequence spelled out. The
// claim token lives in the data layer's memory; a reload shows one truth —
// 「认领凭据不在本标签页」— and waits for expiry or an owner action.
import { effect, keymap, Text, View, windowSize } from "nsh";
import {
  AlertDialog,
  Badge,
  Button,
  Card,
  Checkbox,
  Dialog,
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  Input,
  Progress,
  RadioGroup,
  ScrollArea,
  Select,
  Sheet,
  Textarea,
  color,
  font,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
} from "../../ui/kit";
import {
  BOARD_MOVE_LABEL,
  BOARD_KEY_MOVES,
  boardActorName,
  boardAgeText,
  boardClock,
  boardDeadlineText,
  boardFailureCopy,
  boardLeaseFacts,
  boardNeedsFact,
  boardPlatformLabel,
  boardRenderableMoves,
  boardStateIcon,
  boardStateLabel,
  boardStateTone,
  boardStore,
  boardThreadRefLabel,
  boardTimeShort,
  boardVisibleLabels,
  effectiveTaskState,
  hasMove,
  type BoardMove,
  type BoardQueueRow,
  type BoardTone,
} from "../../data/board";
import type { BoardMessageView, BoardTaskView } from "../../data/board-wire";
import { can } from "../../data/permissions";
import { store } from "../../data/store";
import {
  BookAvatar,
  FactLine,
  Hairline,
  PageError,
  PageLoading,
  SectionLabel,
  StateNote,
  copyText,
  formatWhen,
  isMobile,
  touchStyle,
} from "../books/common";
import {
  assignTarget,
  answerText,
  blockKind,
  blockReason,
  boardDialog,
  cancelReason,
  closeDialog,
  completeResult,
  failReason,
  needsPicked,
  needsQuery,
  openDialog,
  replyDraft,
  unblockNote,
} from "./state";

const toneVariant = (tone: BoardTone): "destructive" | "warning" | "success" | "info" | "outline" | "secondary" =>
  tone === "destructive" ? "destructive" : tone === "warning" ? "warning" : tone === "success" ? "success" : tone === "info" ? "info" : tone === "outline" ? "outline" : "secondary";

const twoPane = (): boolean => windowSize().width >= 1180;

const rowOf = (id: string): BoardQueueRow | undefined => {
  const message = boardStore.cache().messages[id];
  const task = boardStore.cache().tasks[id];
  if (!message || !task) return undefined;
  return {
    id,
    channel: message.channel,
    message,
    task,
    state: task.state,
    labels: message.labels ?? {},
    replies: boardStore.cache().threads[id]?.replyCount ?? 0,
  };
};

function StateBadge(props: { state: string; testID?: string }) {
  return (
    <Badge
      testID={props.testID}
      variant={toneVariant(boardStateTone(props.state))}
      icon={boardStateIcon(props.state)}
      label={boardStateLabel(props.state)}
    />
  );
}

// ---- task card ------------------------------------------------------------

function LeaseBlock(props: { taskId: string; task: BoardTaskView }) {
  const now = () => boardStore.clock();
  const facts = () => boardLeaseFacts(props.task, now(), {
    hasToken: boardStore.heldByThisTab(props.taskId, now()),
    mine: boardStore.isMyActor(props.task.claim?.actor),
  });
  const holderText = () => {
    const holder = facts().holder;
    if (holder === "this-tab") return "本标签页持有";
    if (holder === "another-tab") return "由你（另一个标签页）持有";
    if (holder === "someone") return `${boardActorName(boardStore.cache(), props.task.claim?.actor)}持有`;
    return "";
  };
  return (
    <View testID="board-task-lease" style={{ flexDirection: "column", gap: space.xs }}>
      <View style={{ flexDirection: "row", alignItems: "baseline", gap: space.sm }}>
        <Text
          testID="board-task-lease-text"
          style={{
            fontFamily: font.mono,
            fontSize: typeScale.section,
            fontWeight: weight.medium,
            color: facts().tone === "danger" ? color.destructive : facts().tone === "warning" ? color.warning : color.foreground,
          }}
        >
          {facts().expired ? "租约已到期" : facts().text}
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{holderText()}</Text>
      </View>
      <Progress
        testID="board-task-lease-progress"
        value={facts().fraction}
        tone={facts().tone === "danger" ? "danger" : "default"}
        label={facts().expired ? "租约已到期" : `租约${facts().text}`}
      />
    </View>
  );
}

function NeedsList(props: { taskId: string; task: BoardTaskView; onOpenTask: (id: string) => void }) {
  const facts = () => boardNeedsFact(boardStore.cache(), props.task);
  return (
    <View style={{ flexDirection: "column", gap: space.xs }}>
      <SectionLabel
        text="前置依赖"
        note={facts().length ? `${facts().filter((fact) => fact.done).length}/${facts().length} 完成` : "无前置"}
      />
      <If when={facts().length > 0}>
        <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.xs }}>
          <Each of={facts()} key={(fact) => fact.id}>
            {(fact) => (
              <Badge
                testID={`board-need-${fact().id}`}
                variant={fact().done ? "success" : fact().state === "failed" || fact().state === "canceled" ? "destructive" : "outline"}
                icon={fact().done ? "check" : fact().state === "failed" ? "alert" : "clock"}
                label={`#${fact().id} ${boardStateLabel(fact().state)}`}
                onPress={() => props.onOpenTask(fact().id)}
              />
            )}
          </Each>
        </View>
      </If>
    </View>
  );
}

function TaskFacts(props: { task: BoardTaskView; message: BoardMessageView }) {
  const cache = () => boardStore.cache();
  const owner = () => boardActorName(cache(), props.task.owner);
  const by = () => boardActorName(cache(), props.task.by);
  const source = () => {
    const origin = props.task.origin;
    const copy = props.task.copies?.[0];
    if (origin?.platform === "superagent") return "超级助理工单";
    if (origin?.platform) return `${boardPlatformLabel(origin.platform)} 线程`;
    if (copy?.platform) return boardPlatformLabel(copy.platform);
    return "任务面";
  };
  if (isMobile()) {
    return (
      <View style={{ flexDirection: "row", flexWrap: "wrap", columnGap: space.md, rowGap: 2 }}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          创建者 <Text style={{ color: color.foreground }}>{owner()}</Text>
        </Text>
        <If when={by() !== owner()}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            发起 <Text style={{ color: color.foreground }}>{by()}</Text>
          </Text>
        </If>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          指派 <Text style={{ color: color.foreground }}>{boardActorName(cache(), props.task.for)}</Text>
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          来源 <Text style={{ color: color.foreground }}>{source()}</Text>
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          频道 <Text style={{ fontFamily: font.mono, color: color.foreground }}>{props.message.channel}</Text>
        </Text>
      </View>
    );
  }
  return (
    <View style={{ flexDirection: "column", gap: space.xs }}>
      <FactLine label="创建者" value={owner()} />
      <If when={by() !== owner()}>
        <FactLine label="发起" value={by()} />
      </If>
      <FactLine label="指派给" value={boardActorName(cache(), props.task.for)} />
      <FactLine label="来源" value={source()} />
      <FactLine label="频道" value={props.message.channel} mono />
    </View>
  );
}

function CopiesList(props: { task: BoardTaskView }) {
  const copies = props.task.copies ?? [];
  return (
    <If when={copies.length > 0}>
      <View style={{ flexDirection: "column", gap: space.xs }}>
        <SectionLabel text="外部副本" />
        <Each of={copies} key={(copy) => `${copy.target}-${copy.status}`}>
          {(copy) => (
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, minWidth: 0 }}>
              <Text style={{ flex: 1, minWidth: 0, fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1, textOverflow: "ellipsis" }}>
                {copy().target ? boardThreadRefLabel(copy().target) : boardPlatformLabel(copy().platform)}
              </Text>
              <Badge
                variant={copy().status === "synced" || copy().status === "已同步" ? "success" : copy().status === "failed" ? "destructive" : "warning"}
                label={copy().status ?? "未知"}
              />
            </View>
          )}
        </Each>
      </View>
    </If>
  );
}

function TaskActions(props: { taskId: string; task: BoardTaskView; onOpenTask: (id: string) => void }) {
  const now = () => boardStore.clock();
  const stale = () => boardStore.threadStale();
  const claimLost = () =>
    effectiveTaskState(props.task, now()) === "claimed" &&
    boardStore.isMyActor(props.task.claim?.actor) &&
    !boardStore.heldByThisTab(props.taskId, now()) &&
    !boardLeaseFacts(props.task, now(), {
      hasToken: false,
      mine: true,
    }).expired;
  const moves = () =>
    boardRenderableMoves(props.task, {
      hasToken: boardStore.heldByThisTab(props.taskId, now()),
      mine: boardStore.isMyActor(props.task.claim?.actor),
      now: now(),
    });
  const actionMoves = (): BoardMove[] => moves().filter((move) => move !== "reply");
  const primary = () =>
    (["complete", "claim", "unblock", "reopen", "answer"] as BoardMove[]).find((move) => actionMoves().includes(move));
  const secondary = () =>
    (["renew", "release", "block", "assign", "needs"] as BoardMove[]).filter((move) => actionMoves().includes(move));
  const dangerous = () => (["fail", "cancel"] as BoardMove[]).filter((move) => actionMoves().includes(move));
  const overflow = () =>
    actionMoves().filter(
      (move) =>
        move !== primary() &&
        !secondary().includes(move) &&
        !dangerous().includes(move),
    );
  const run = (move: BoardMove) => {
    switch (move) {
      case "claim":
        void boardStore.claim(props.taskId);
        break;
      case "renew":
        void boardStore.renew(props.taskId);
        break;
      case "release":
      case "complete":
      case "fail":
      case "block":
      case "unblock":
      case "reopen":
      case "assign":
      case "needs":
      case "cancel":
      case "answer":
        openDialog(move, props.taskId);
        break;
      default:
        break;
    }
  };
  const buttonFor = (move: BoardMove, tier: "primary" | "secondary" | "danger" | "overflow") => (
    <Button
      testID={`board-action-${move}`}
      variant={tier === "primary" ? "default" : tier === "danger" ? "destructive" : "outline"}
      size="sm"
      label={BOARD_MOVE_LABEL[move]}
      accessibilityLabel={`${BOARD_MOVE_LABEL[move]} #${props.taskId}`}
      onPress={() => run(move)}
      style={touchStyle()}
    />
  );
  return (
    <View testID="board-task-actions" style={{ flexDirection: "column", gap: space.sm }}>
      <If when={props.task.archived === true}>
        <StateNote tone="info" icon="lock" text="只读归档：保留旧工单的记录，不提供状态变更。" />
      </If>
      <If when={props.task.archived !== true && props.task.mirror?.readOnly === true}>
        <StateNote tone="info" icon="lock" text="镜像任务只读：由超级助理管理；命令桥未开启时不能在这里改状态。" />
      </If>
      <If when={props.task.moves === undefined}>
        <StateNote
          testID="board-moves-missing"
          tone="warning"
          icon="alert"
          text="服务端没有返回可执行动作；任务面按只读处理，动作开放后会自动出现。"
        />
      </If>
      <If
        when={
          props.task.moves !== undefined &&
          props.task.archived !== true &&
          props.task.mirror?.readOnly !== true &&
          actionMoves().length === 0 &&
          !claimLost() &&
          effectiveTaskState(props.task, now()) !== "completed" &&
          effectiveTaskState(props.task, now()) !== "canceled"
        }
      >
        <StateNote tone="info" icon="lock" text="当前没有可执行的动作；需要由创建者或管理员推进。" />
      </If>
      <If when={stale()}>
        <StateNote
          testID="board-stale-write-lock"
          tone="warning"
          icon="refresh"
          text="实时更新中断：写操作暂不可用，先刷新任务再继续。"
        />
      </If>
      <If when={!stale() && props.task.archived !== true && props.task.mirror?.readOnly !== true && actionMoves().length > 0}>
        <View style={{ flexDirection: "column", gap: space.sm }}>
          <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.sm }}>
            <If when={primary() !== undefined}>{buttonFor(primary()!, "primary")}</If>
            <Each of={secondary()} key={(move) => move}>
              {(move) => buttonFor(move(), "secondary")}
            </Each>
            <Each of={overflow()} key={(move) => move}>
              {(move) => buttonFor(move(), "overflow")}
            </Each>
          </View>
          <If when={dangerous().length > 0}>
            <Hairline />
            <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.sm }}>
              <Each of={dangerous()} key={(move) => move}>
                {(move) => buttonFor(move(), "danger")}
              </Each>
            </View>
          </If>
        </View>
      </If>
    </View>
  );
}

export function BoardTaskCard(props: { taskId: string; task: BoardTaskView; message: BoardMessageView; onOpenTask: (id: string) => void }) {
  const now = () => boardStore.clock();
  const state = () => effectiveTaskState(props.task, now());
  const lease = () => boardLeaseFacts(props.task, now(), {
    hasToken: boardStore.heldByThisTab(props.taskId, now()),
    mine: boardStore.isMyActor(props.task.claim?.actor),
  });
  const claimLost = () =>
    state() === "claimed" && boardStore.isMyActor(props.task.claim?.actor) && !boardStore.heldByThisTab(props.taskId, now()) && !lease().expired;
  return (
    <Card
      testID="board-task-card"
      style={{
        padding: isMobile() ? space.md : space.lg,
        gap: isMobile() ? space.sm : space.md,
        flexDirection: "column",
        backgroundColor: color.card,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: space.sm }}>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.base, color: color.mutedForeground }}>#{props.taskId}</Text>
        <StateBadge state={state()} testID="board-task-state" />
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {claimLost() ? "认领凭据不在本标签页" : boardDeadlineText(props.task, props.message, now())}
        </Text>
      </View>
      <If when={!boardStore.meLoaded() && boardStore.meFailure() !== null}>
        <StateNote
          testID="board-identity-unknown"
          tone="warning"
          icon="alert"
          text="身份未知：无法确认这个认领是否属于你。续租或结算前需要重新认领，或由创建者/管理员指派/取消。"
        />
      </If>
      <If when={state() === "claimed" && !claimLost()}>
        <LeaseBlock taskId={props.taskId} task={props.task} />
      </If>
      <If when={props.task.state === "claimed" && state() === "ready"}>
        <StateNote
          testID="board-lease-ran-out"
          tone="warning"
          icon="clock"
          text="租约已到期：任务按「可认领」呈现；点认领会由服务端再次校验。"
        />
      </If>
      <If when={claimLost()}>
        <StateNote
          testID="board-claim-lost"
          tone="warning"
          icon="alert"
          text="认领凭据不在本标签页，无法续租或结算。等待租约到期后可重新认领，或由创建者/管理员指派/取消。"
        />
      </If>
      <TaskFacts task={props.task} message={props.message} />
      <NeedsList taskId={props.taskId} task={props.task} onOpenTask={props.onOpenTask} />
      <If when={props.task.blocked !== null && props.task.blocked !== undefined}>
        <StateNote
          testID="board-task-blocked"
          tone="warning"
          icon="alert"
          text={`${props.task.blocked?.kind === "awaiting_approval" ? "等待审批" : "等待回复"}：${props.task.blocked?.reason ?? ""}`}
        />
      </If>
      <If when={(props.task.result ?? "") !== ""}>
        <View style={{ flexDirection: "column", gap: space.xs }}>
          <SectionLabel text="结果" />
          <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.foreground }}>
            {props.task.result}
          </Text>
        </View>
      </If>
      <If when={(props.task.reason ?? "") !== "" && props.task.state === "failed"}>
        <StateNote testID="board-task-failure" tone="danger" icon="alert" text={`失败原因：${props.task.reason}`} />
      </If>
      <CopiesList task={props.task} />
      <Hairline />
      <TaskActions taskId={props.taskId} task={props.task} onOpenTask={props.onOpenTask} />
      <If when={!isMobile()}>
        <View style={{ flexDirection: "row", gap: space.sm }}>
          <Button
            testID="board-task-copy-link"
            variant="ghost"
            size="sm"
            icon="copy"
            label="复制链接"
            accessibilityLabel={`复制任务 #${props.taskId} 的链接`}
            onPress={() => void copyText(`#${props.taskId} ${props.message.channel}`)}
            style={touchStyle()}
          />
        </View>
      </If>
    </Card>
  );
}

// ---- thread ---------------------------------------------------------------

function ReplyBlock(props: { message: BoardMessageView; pending?: boolean }) {
  const author = () => props.message.by?.name ?? props.message.author?.name ?? props.message.by?.handle ?? "未知作者";
  return (
    <View
      testID={`board-reply-${props.message.id}`}
      style={{ flexDirection: "column", gap: space.xs, paddingVertical: space.md }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <BookAvatar name={author()} size="sm" />
        <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>{author()}</Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          {props.pending ? "发送中…" : formatWhen(props.message.createdAt)}
        </Text>
      </View>
      <Text
        style={{
          fontSize: typeScale.base,
          lineHeight: Math.round(typeScale.base * lineHeight.relaxed),
          color: props.pending ? color.mutedForeground : color.foreground,
        }}
      >
        {props.message.text}
      </Text>
    </View>
  );
}

const replyAllowed = (task: BoardTaskView): boolean =>
  task.archived !== true && task.mirror?.readOnly !== true && hasMove(task, "reply");

function Composer(props: { taskId: string; task: BoardTaskView }) {
  const canReply = () => replyAllowed(props.task) && !boardStore.threadStale();
  const sending = () => boardStore.replySending();
  const error = () => boardStore.replyError();
  const send = () => {
    if (!canReply() || sending()) return;
    const text = replyDraft().trim();
    if (!text) return;
    void boardStore.reply(text).then((ok) => {
      if (ok) replyDraft.set("");
    });
  };
  keymap.bind("mod+enter", () => send());
  return (
    <View
      testID="board-composer"
      style={{
        flexDirection: "column",
        gap: space.sm,
        paddingTop: space.md,
        borderTopWidth: 1,
        borderTopColor: color.border,
        backgroundColor: color.background,
      }}
    >
      <If when={boardStore.threadStale()}>
        <StateNote tone="warning" icon="refresh" text="实时更新中断：回复暂不可用。" />
      </If>
      <If when={canReply()}>
        <Textarea
          testID="board-reply-input"
          value={replyDraft()}
          placeholder="写下第一条工作日志，说明你看到了什么、准备怎么做。"
          rows={isMobile() ? 2 : 3}
          maxLength={10000}
          disabled={sending() || boardStore.threadStale()}
          accessibilityLabel="回复任务"
          onChange={(value) => replyDraft.set(value)}
        />
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>Cmd/Ctrl+Enter 发送</Text>
          <View style={{ flex: 1 }} />
          <Button
            testID="board-reply-send"
            label={sending() ? "发送中…" : "回复"}
            loading={sending()}
            disabled={sending() || !replyDraft().trim() || boardStore.threadStale()}
            accessibilityLabel="发送回复"
            onPress={send}
            style={touchStyle()}
          />
        </View>
        <If when={error() !== null}>
          <StateNote
            testID="board-reply-error"
            tone="danger"
            icon="alert"
            text={`${error()?.message ?? "回复失败"} 草稿还在，可以重试。`}
          />
        </If>
      </If>
    </View>
  );
}

export function BoardTaskPage(props: { taskId: string; onBack: () => void; onOpenTask: (id: string) => void }) {
  const cache = () => boardStore.cache();
  const row = () => rowOf(props.taskId);
  const thread = () => cache().threads[props.taskId];
  const phase = () => boardStore.threadPhase();
  const error = () => boardStore.threadError();
  const stale = () => boardStore.threadStale();
  const taskKnown = () => row() !== undefined;
  const replies = () => thread()?.replies ?? [];
  const root = () => row()?.message;
  // Loading only before the first read: a cached root renders immediately and
  // the refresh paints over it.
  const loading = () => phase() === "loading" && !root();
  return (
    <View testID="board-task-page" style={{ flex: 1, minHeight: 0, flexDirection: "column", gap: space.md }}>
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
        <Button
          testID="board-task-back"
          variant="ghost"
          size="sm"
          icon="chevron-left"
          label="队列"
          accessibilityLabel="返回任务队列"
          onPress={props.onBack}
          style={touchStyle()}
        />
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.mutedForeground }}>任务 #{props.taskId}</Text>
      </View>
      <If when={loading()}>
        <PageLoading rows={4} label="加载任务" testID="board-task-loading" />
      </If>
      <If when={!loading() && phase() === "error" && !root()}>
        <PageError
          testID="board-task-error"
          title="没能读到这个任务"
          message={`${(error()?.message ?? "读取失败").replace(/。$/, "")}。任务可能不存在，或频道权限已变化。`}
          requestId={error()?.requestId}
          onRetry={() => void boardStore.refreshThread()}
        />
      </If>
      <If when={!loading() && root() !== undefined}>
        <If when={stale()}>
          <StateNote testID="board-task-stale" tone="warning" icon="refresh" text="显示的是上次读取的内容；正在重试实时更新。" />
        </If>
        <If
          when={isMobile()}
          else={
            <If
              when={twoPane()}
              else={
                <ScrollArea testID="board-task-scroll" style={{ flex: 1, minHeight: 0 }}>
                  <View style={{ flexDirection: "column", gap: space.lg, paddingBottom: space.xl }}>
                    <BoardTaskCard taskId={props.taskId} task={row()!.task} message={root()!} onOpenTask={props.onOpenTask} />
                    <ThreadBody taskId={props.taskId} root={root()!} replies={replies()} onOpenTask={props.onOpenTask} />
                    <If when={replyAllowed(row()!.task)}>
                      <Composer taskId={props.taskId} task={row()!.task} />
                    </If>
                  </View>
                </ScrollArea>
              }
            >
              <View style={{ flex: 1, minHeight: 0, flexDirection: "row", gap: space.xl }}>
                <View style={{ flex: 1, minWidth: 0, minHeight: 0, flexDirection: "column" }}>
                  <ScrollArea testID="board-task-scroll" style={{ flex: 1, minHeight: 0 }}>
                    <ThreadBody taskId={props.taskId} root={root()!} replies={replies()} onOpenTask={props.onOpenTask} />
                  </ScrollArea>
                  <If when={replyAllowed(row()!.task)}>
                      <Composer taskId={props.taskId} task={row()!.task} />
                    </If>
                </View>
                <View style={{ width: 340, flexShrink: 0, minHeight: 0 }}>
                  <ScrollArea testID="board-task-card-scroll" style={{ flex: 1, minHeight: 0 }}>
                    <BoardTaskCard taskId={props.taskId} task={row()!.task} message={root()!} onOpenTask={props.onOpenTask} />
                  </ScrollArea>
                </View>
              </View>
            </If>
          }
        >
          {/* A phone pins the composer above the bottom tabs: the card and the
              replies scroll under it, so the last action/reply is never hidden
              behind a fixed overlay. */}
          <View style={{ flex: 1, minHeight: 0, flexDirection: "column", gap: space.sm }}>
            <ScrollArea testID="board-task-scroll" style={{ flex: 1, minHeight: 0 }}>
              <View style={{ flexDirection: "column", gap: isMobile() ? space.sm : space.lg, paddingBottom: space.md }}>
                <BoardTaskCard taskId={props.taskId} task={row()!.task} message={root()!} onOpenTask={props.onOpenTask} />
                <ThreadBody taskId={props.taskId} root={root()!} replies={replies()} onOpenTask={props.onOpenTask} />
              </View>
            </ScrollArea>
            <If when={replyAllowed(row()!.task)}>
                      <Composer taskId={props.taskId} task={row()!.task} />
                    </If>
          </View>
        </If>
      </If>
    </View>
  );
}

function ThreadBody(props: {
  taskId: string;
  root: BoardMessageView;
  replies: BoardMessageView[];
  onOpenTask: (id: string) => void;
}) {
  const pending = () => boardStore.pendingReplies();
  const empty = () => props.replies.length === 0 && pending().length === 0;
  const older = () => boardStore.cache().threads[props.taskId]?.older ?? null;
  const labels = () => boardVisibleLabels(props.root.labels);
  const heading = () => props.root.text.split("\n")[0] ?? "";
  const body = () => props.root.text.split("\n").slice(1).join("\n");
  return (
    <View testID="board-thread" style={{ flexDirection: "column", gap: space.sm }}>
      <View
        testID="board-thread-root"
        style={{
          flexDirection: "column",
          gap: space.sm,
          padding: isMobile() ? space.md : space.lg,
          borderRadius: radius.lg,
          backgroundColor: color.muted,
          borderWidth: 1,
          borderColor: color.border,
        }}
      >
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
          <BookAvatar name={props.root.by?.name ?? "?"} size="sm" />
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>
            {props.root.by?.name ?? props.root.author?.name ?? "未知作者"}
          </Text>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{formatWhen(props.root.createdAt)}</Text>
          <Text style={{ fontFamily: font.mono, fontSize: typeScale.sm, color: color.mutedForeground }}>{props.root.channel}</Text>
        </View>
        <View style={{ flexDirection: "column", gap: 2 }}>
          <Text
            style={{
              fontSize: typeScale.lg,
              lineHeight: Math.round(typeScale.lg * lineHeight.normal),
              fontWeight: weight.medium,
              color: color.foreground,
            }}
          >
            {heading()}
          </Text>
          <If when={body() !== ""}>
            <Text
              style={{
                fontSize: typeScale.base,
                lineHeight: Math.round(typeScale.base * lineHeight.normal),
                color: color.foreground,
              }}
            >
              {body()}
            </Text>
          </If>
        </View>
        <If when={labels().length > 0}>
          <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.xs }}>
            <Each of={labels()} key={(label) => `${label.key}:${label.value}`}>
              {(label) => <Badge variant={toneVariant(label().tone)} label={label().text} />}
            </Each>
          </View>
        </If>
      </View>
      <If when={empty()}>
        <Empty testID="board-thread-empty" accessibilityLabel="还没有回复">
          <EmptyMedia icon="message" />
          <EmptyTitle label="还没有回复" />
          <EmptyDescription label="写下第一条工作日志，说明你看到了什么、准备怎么做。" />
        </Empty>
      </If>
      <Each of={props.replies} key={(reply) => reply.id}>
        {(reply) => <ReplyBlock message={reply()} />}
      </Each>
      <Each of={pending()} key={(item) => item.id}>
        {(item) => (
          <ReplyBlock
            message={{
              id: item().id,
              channel: props.root.channel,
              text: item().text,
              createdAt: item().createdAt,
              labels: {},
            }}
            pending
          />
        )}
      </Each>
      <If when={older() !== null}>
        <Text testID="board-thread-older" style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          更早的回复暂未加载；当前只显示最新 50 条。
        </Text>
      </If>
    </View>
  );
}

// ---- dialogs --------------------------------------------------------------

const taskOf = (id: string | undefined): BoardTaskView | undefined => (id ? boardStore.cache().tasks[id] : undefined);

function requiredNote(value: string, label: string) {
  return value.trim() ? "" : `请填写${label}。`;
}

export function TaskDialogs(props: { taskId: string }) {
  const dialog = () => boardDialog();
  const taskId = () => dialog()?.taskId ?? props.taskId;
  const task = () => taskOf(taskId());
  const message = () => boardStore.cache().messages[taskId()];
  let lastKey = "";
  effect(() => {
    const current = dialog();
    const key = current ? `${current.kind}:${current.taskId ?? ""}` : "";
    if (key === lastKey) return;
    lastKey = key;
    if (!current) return;
    completeResult.set("");
    failReason.set("");
    blockReason.set("");
    blockKind.set("needs_input");
    unblockNote.set("");
    answerText.set("");
    cancelReason.set("");
    assignTarget.set(task()?.for ?? null);
    needsQuery.set("");
    needsPicked.set([...(task()?.needs ?? [])]);
    if (current.kind === "needs") {
      // Preload the candidate rows from the queue's cache; the search filters
      // them locally.
    }
  });
  const close = () => closeDialog();
  const requireMove = (move: BoardMove, run: () => void) => {
    if (!task() || !hasMove(task(), move)) {
      boardStore.flash("当前动作不可用。", "warning");
      close();
      return;
    }
    run();
  };
  const number = () => taskId();
  return (
    <View>
      <Dialog
        testID="board-dialog-complete"
        open={dialog()?.kind === "complete"}
        title={`完成 #${number()}？`}
        description="完成后不可撤销；依赖它的任务会变为可认领。"
        onOpenChange={(event) => !event.open && close()}
        footer={
          <>
            <Button variant="ghost" label="取消" accessibilityLabel="取消完成" onPress={close} style={touchStyle()} />
            <Button
              testID="board-dialog-complete-submit"
              label="完成"
              accessibilityLabel="确认完成任务"
              disabled={boardStore.actionInFlight() !== null}
              onPress={() =>
                requireMove("complete", () => {
                  void boardStore.complete(number(), completeResult().trim() || undefined).then((ok) => ok && close());
                })
              }
              style={touchStyle()}
            />
          </>
        }
      >
        <View style={{ flexDirection: "column", gap: space.sm }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>结果（可选）</Text>
          <Textarea
            testID="board-dialog-complete-input"
            value={completeResult()}
            placeholder="交付了什么、验证了什么、链接或版本号。"
            maxLength={1000}
            rows={4}
            accessibilityLabel="完成结果"
            onChange={(value) => completeResult.set(value)}
          />
        </View>
      </Dialog>

      <Dialog
        testID="board-dialog-fail"
        open={dialog()?.kind === "fail"}
        title={`标记 #${number()} 失败？`}
        description="失败会结束认领；依赖它的任务不会自动就绪，需要重新打开并完成。"
        onOpenChange={(event) => !event.open && close()}
        footer={
          <>
            <Button variant="ghost" label="取消" accessibilityLabel="取消标记失败" onPress={close} style={touchStyle()} />
            <Button
              testID="board-dialog-fail-submit"
              variant="destructive"
              label="标记失败"
              accessibilityLabel="确认标记失败"
              disabled={!!requiredNote(failReason(), "失败原因") || boardStore.actionInFlight() !== null}
              onPress={() =>
                requireMove("fail", () => {
                  void boardStore.fail(number(), failReason().trim()).then((ok) => ok && close());
                })
              }
              style={touchStyle()}
            />
          </>
        }
      >
        <View style={{ flexDirection: "column", gap: space.sm }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>失败原因</Text>
          <Textarea
            testID="board-dialog-fail-input"
            value={failReason()}
            placeholder="为什么没有完成？保留可核对的证据。"
            maxLength={1000}
            rows={3}
            invalid={!!requiredNote(failReason(), "失败原因") && failReason() !== ""}
            accessibilityLabel="失败原因"
            onChange={(value) => failReason.set(value)}
          />
        </View>
      </Dialog>

      <Dialog
        testID="board-dialog-block"
        open={dialog()?.kind === "block"}
        title={`阻塞 #${number()}？`}
        description="阻塞会结束当前认领；解除阻塞只有创建者、频道管理员或当前持有者可以操作。"
        onOpenChange={(event) => !event.open && close()}
        footer={
          <>
            <Button variant="ghost" label="取消" accessibilityLabel="取消阻塞" onPress={close} style={touchStyle()} />
            <Button
              testID="board-dialog-block-submit"
              variant="destructive"
              label="阻塞"
              accessibilityLabel="确认阻塞任务"
              disabled={!!requiredNote(blockReason(), "说明") || boardStore.actionInFlight() !== null}
              onPress={() =>
                requireMove("block", () => {
                  void boardStore.block(number(), blockReason().trim(), blockKind()).then((ok) => ok && close());
                })
              }
              style={touchStyle()}
            />
          </>
        }
      >
        <View style={{ flexDirection: "column", gap: space.md }}>
          <RadioGroup
            testID="board-dialog-block-kind"
            value={blockKind()}
            options={[
              { value: "needs_input", label: "等待回复" },
              { value: "awaiting_approval", label: "等待审批" },
            ]}
            onChange={(value) => blockKind.set(value)}
            accessibilityLabel="阻塞类型"
          />
          <Textarea
            testID="board-dialog-block-input"
            value={blockReason()}
            placeholder="卡在哪里？谁需要做什么？"
            maxLength={1000}
            rows={3}
            accessibilityLabel="阻塞说明"
            onChange={(value) => blockReason.set(value)}
          />
        </View>
      </Dialog>

      <Dialog
        testID="board-dialog-unblock"
        open={dialog()?.kind === "unblock"}
        title={`解除阻塞 #${number()}？`}
        description="任务会按前置依赖回到「可认领」或「等待依赖」。"
        onOpenChange={(event) => !event.open && close()}
        footer={
          <>
            <Button variant="ghost" label="取消" accessibilityLabel="取消解除阻塞" onPress={close} style={touchStyle()} />
            <Button
              testID="board-dialog-unblock-submit"
              label="解除阻塞"
              accessibilityLabel="确认解除阻塞"
              disabled={boardStore.actionInFlight() !== null}
              onPress={() =>
                requireMove("unblock", () => {
                  void boardStore.unblock(number(), unblockNote().trim() || undefined).then((ok) => ok && close());
                })
              }
              style={touchStyle()}
            />
          </>
        }
      >
        <View style={{ flexDirection: "column", gap: space.sm }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>补充说明（可选，会作为回复发布）</Text>
          <Textarea
            testID="board-dialog-unblock-input"
            value={unblockNote()}
            placeholder="例如：文案由产品定稿，已确认。"
            maxLength={10000}
            rows={3}
            accessibilityLabel="解除阻塞补充说明"
            onChange={(value) => unblockNote.set(value)}
          />
        </View>
      </Dialog>

      <Dialog
        testID="board-dialog-assign"
        open={dialog()?.kind === "assign"}
        title={`指派 #${number()}`}
        description="只能指派给创建者自己的账号或 Bot；跨账号指派不在本版范围。"
        onOpenChange={(event) => !event.open && close()}
        footer={
          <>
            <Button variant="ghost" label="取消" accessibilityLabel="取消指派" onPress={close} style={touchStyle()} />
            <Button
              testID="board-dialog-assign-submit"
              label="保存指派"
              accessibilityLabel="保存指派"
              disabled={boardStore.actionInFlight() !== null}
              onPress={() =>
                requireMove("assign", () => {
                  void boardStore.assign(number(), assignTarget()).then((ok) => ok && close());
                })
              }
              style={touchStyle()}
            />
          </>
        }
      >
        <Select
          testID="board-dialog-assign-select"
          items={[
            { value: "", title: "池中（任何有编辑权限的人）" },
            ...boardStore.actors().map((actor) => ({ value: actor.id, title: actor.name || actor.handle || actor.id })),
          ]}
          value={assignTarget() ?? ""}
          placeholder="池中"
          accessibilityLabel="指派给"
          onChange={(value) => assignTarget.set(value ? value : null)}
        />
      </Dialog>

      <NeedsDialog taskId={number()} task={task()} />

      <Dialog
        testID="board-dialog-cancel"
        open={dialog()?.kind === "cancel"}
        title={`取消 #${number()}？`}
        description="取消后不可恢复；依赖它的任务会一直等待，需要改依赖或取消。"
        onOpenChange={(event) => !event.open && close()}
        footer={
          <>
            <Button variant="ghost" label="保留任务" accessibilityLabel="保留任务" onPress={close} style={touchStyle()} />
            <Button
              testID="board-dialog-cancel-submit"
              variant="destructive"
              label="取消任务"
              accessibilityLabel="确认取消任务"
              disabled={!!requiredNote(cancelReason(), "取消原因") || boardStore.actionInFlight() !== null}
              onPress={() =>
                requireMove("cancel", () => {
                  void boardStore.cancel(number(), cancelReason().trim()).then((ok) => ok && close());
                })
              }
              style={touchStyle()}
            />
          </>
        }
      >
        <View style={{ flexDirection: "column", gap: space.sm }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>取消原因</Text>
          <Textarea
            testID="board-dialog-cancel-input"
            value={cancelReason()}
            placeholder="为什么取消？"
            maxLength={1000}
            rows={3}
            accessibilityLabel="取消原因"
            onChange={(value) => cancelReason.set(value)}
          />
        </View>
      </Dialog>

      <Dialog
        testID="board-dialog-answer"
        open={dialog()?.kind === "answer"}
        title="回答并解除阻塞"
        description={`提问：${task()?.blocked?.reason ?? ""}`}
        onOpenChange={(event) => !event.open && close()}
        footer={
          <>
            <Button variant="ghost" label="取消" accessibilityLabel="取消回答" onPress={close} style={touchStyle()} />
            <Button
              testID="board-dialog-answer-submit"
              label="回答"
              accessibilityLabel="提交回答"
              disabled={!answerText().trim() || boardStore.actionInFlight() !== null}
              onPress={() =>
                requireMove("answer", () => {
                  void boardStore.answer(number(), answerText().trim()).then((ok) => ok && close());
                })
              }
              style={touchStyle()}
            />
          </>
        }
      >
        <Textarea
          testID="board-dialog-answer-input"
          value={answerText()}
          placeholder="回答会作为回复写入线程，并把问题交回超级助理。"
          maxLength={10000}
          rows={4}
          accessibilityLabel="回答内容"
          onChange={(value) => answerText.set(value)}
        />
      </Dialog>

      <AlertDialog
        testID="board-dialog-release"
        open={dialog()?.kind === "release"}
        title={`释放 #${number()}？`}
        description="任务会回到池中，其他人可以认领。已发布的回复不会撤回。"
        confirmLabel="释放"
        cancelLabel="继续持有"
        destructive
        onCancel={close}
        onConfirm={() =>
          requireMove("release", () => {
            void boardStore.release(number()).then((ok) => ok && close());
          })
        }
      />

      <AlertDialog
        testID="board-dialog-reopen"
        open={dialog()?.kind === "reopen"}
        title={`重新打开 #${number()}？`}
        description="任务回到可认领；失败原因保留在记录里；依赖者仍会等到它完成。"
        confirmLabel="重新打开"
        cancelLabel="保持失败"
        onCancel={close}
        onConfirm={() =>
          requireMove("reopen", () => {
            void boardStore.reopen(number()).then((ok) => ok && close());
          })
        }
      />

      <ShortcutHelp />
    </View>
  );
}

function NeedsDialog(props: { taskId: string; task: BoardTaskView | undefined }) {
  const dialog = () => boardDialog();
  const candidates = () => {
    const query = needsQuery().trim().toLowerCase();
    return boardStore
      .queueIds()
      .filter((id) => id !== props.taskId)
      .map((id) => ({ id, task: boardStore.cache().tasks[id], message: boardStore.cache().messages[id] }))
      .filter((entry) => entry.task && entry.message)
      .filter((entry) => {
        if (!query) return true;
        return (
          entry.id.includes(query) ||
          (entry.message?.text ?? "").toLowerCase().includes(query)
        );
      })
      .slice(0, 60);
  };
  const toggle = (id: string) => {
    const current = needsPicked();
    if (current.includes(id)) {
      needsPicked.set(current.filter((value) => value !== id));
      return;
    }
    if (current.length >= 20) {
      boardStore.flash("最多 20 个前置。", "warning");
      return;
    }
    needsPicked.set([...current, id]);
  };
  return (
    <Dialog
      testID="board-dialog-needs"
      open={dialog()?.kind === "needs"}
      title={`前置依赖 #${props.taskId}`}
      description="最多 20 个前置；保存空数组表示无前置。"
      onOpenChange={(event) => !event.open && closeDialog()}
      footer={
        <>
          <Button variant="ghost" label="取消" accessibilityLabel="取消修改前置" onPress={closeDialog} style={touchStyle()} />
          <Button
            testID="board-dialog-needs-submit"
            label={`保存${needsPicked().length ? ` ${needsPicked().length}/20` : ""}`}
            accessibilityLabel="保存前置依赖"
            disabled={boardStore.actionInFlight() !== null}
            onPress={() => {
              if (!props.task || !hasMove(props.task, "needs")) {
                boardStore.flash("当前动作不可用。", "warning");
                closeDialog();
                return;
              }
              void boardStore.setNeeds(props.taskId, needsPicked()).then((ok) => ok && closeDialog());
            }}
            style={touchStyle()}
          />
        </>
      }
    >
      <View style={{ flexDirection: "column", gap: space.sm }}>
        <Input
          testID="board-dialog-needs-search"
          value={needsQuery()}
          placeholder="按 #id 或标题过滤"
          accessibilityLabel="搜索前置任务"
          onChange={(value) => needsQuery.set(value)}
        />
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>已选 {needsPicked().length}/20</Text>
        <ScrollArea maxHeight={240} style={{ borderWidth: 1, borderColor: color.border, borderRadius: radius.md }}>
          <View style={{ flexDirection: "column", padding: space.sm, gap: 2 }}>
            <Each of={candidates()} key={(entry) => entry.id}>
              {(entry) => (
                <Checkbox
                  testID={`board-needs-option-${entry().id}`}
                  checked={needsPicked().includes(entry().id)}
                  disabled={!needsPicked().includes(entry().id) && needsPicked().length >= 20}
                  label={`#${entry().id} ${(entry().message?.text.split("\n")[0] ?? "").slice(0, 48)}`}
                  accessibilityLabel={`前置任务 #${entry().id}`}
                  onChange={() => toggle(entry().id)}
                  style={{ minHeight: 36 }}
                />
              )}
            </Each>
          </View>
        </ScrollArea>
      </View>
    </Dialog>
  );
}

function ShortcutHelp() {
  const open = () => boardDialog()?.kind === "shortcuts";
  const rows = [
    ["j / ↓", "下一行（队列）"],
    ["k / ↑", "上一行（队列）"],
    ["Home / End", "首行 / 末行（队列）"],
    ["Enter", "打开任务"],
    ["c", "认领"],
    ["r", "续租"],
    ["l", "释放"],
    ["e", "完成"],
    ["f", "标记失败"],
    ["b", "阻塞"],
    ["u", "解除阻塞"],
    ["o", "重新打开"],
    ["a", "指派"],
    ["n", "前置依赖"],
    ["x", "取消任务"],
    ["[ / ]", "上一 / 下一状态页"],
    ["/", "筛选（队列）"],
    ["Cmd/Ctrl+Enter", "发送回复"],
    ["Esc", "关闭对话框 / 返回队列"],
    ["?", "本帮助"],
  ];
  return (
    <Dialog
      testID="board-dialog-shortcuts"
      open={open()}
      title="快捷键"
      description="单字母动作只在服务端允许该动作时生效；输入框聚焦时单字母失效。"
      onOpenChange={(event) => !event.open && closeDialog()}
      footer={<Button label="关闭" accessibilityLabel="关闭快捷键帮助" onPress={closeDialog} style={touchStyle()} />}
    >
      <View style={{ flexDirection: "column", gap: 4 }}>
        <Each of={rows} key={([key]) => key}>
          {(row) => (
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, minHeight: 28 }}>
              <Text style={{ width: 120, fontFamily: font.mono, fontSize: typeScale.sm, color: color.foreground }}>{row()[0]}</Text>
              <Text style={{ flex: 1, fontSize: typeScale.sm, color: color.mutedForeground }}>{row()[1]}</Text>
            </View>
          )}
        </Each>
      </View>
    </Dialog>
  );
}

/** The task page's keyboard bindings: only moves the server allows, read from
 * the current route each time so one binding set survives queue ↔ task. */
export function bindTaskKeys(
  taskId: () => string | null,
  onBack: () => void,
  onOpenTask: (id: string) => void,
): void {
  const task = () => {
    const id = taskId();
    return id ? boardStore.cache().tasks[id] : undefined;
  };
  const guard = () => !boardDialog() && task() !== undefined;
  const allowed = (move: BoardMove) => {
    const id = taskId();
    if (!id || !task()) return false;
    if (boardStore.threadStale()) return false;
    return boardRenderableMoves(task(), {
      hasToken: boardStore.heldByThisTab(id, boardStore.clock()),
      mine: boardStore.isMyActor(task()?.claim?.actor),
      now: boardStore.clock(),
    }).includes(move);
  };
  for (const [key, move] of Object.entries(BOARD_KEY_MOVES)) {
    keymap.bind(key, () => {
      const id = taskId();
      if (!id || !guard() || !allowed(move)) return;
      if (move === "claim") {
        void boardStore.claim(id);
        return;
      }
      if (move === "renew") {
        void boardStore.renew(id);
        return;
      }
      if (move === "reply" || move === "release.force" || move === "takeover") return;
      openDialog(move, id);
    }, { when: "notEditing" });
  }
  keymap.bind("escape", () => {
    if (!guard()) return;
    onBack();
  }, { when: "notEditing" });
  keymap.bind("shift+/", () => {
    if (!guard()) return;
    openDialog("shortcuts");
  }, { when: "notEditing" });
}

export function leaseStateForTest(task: BoardTaskView, now: number, hasToken: boolean) {
  return boardLeaseFacts(task, now, { hasToken });
}

export function ageForTest(iso: string | undefined, now: number): string {
  return boardAgeText(iso, now);
}

export function clockForTest(iso: string | undefined): string {
  return boardClock(iso);
}
