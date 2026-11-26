// 02 任务 · 超级助理工单看板 — change keel-collab-superagent, 4.2/4.3.
//
// A product face over the orchestrator's read projections:
//   `GET /v1/superagent/tickets`   the board (ticket + mandate + active worker)
//   `GET /v1/superagent/attention` the open human-decision inbox
// The operator's writes are answer / resume / stop. Reading needs `task.read`;
// every write is offered only under `task.write` and is still decided by the
// server's independent operator authorization.
//
// The page is the same TSX on the web and in the terminal: the wide layout is
// a ticket list beside its detail with the attention cards above; below 1024
// the list and the detail are two screens (the route's objectId names the
// open ticket), and below 768 the count tiles wrap 2×2. Nothing here invents a
// ticket, a question or a status: an empty read is empty, a failed read keeps
// its request id, and a denied read names the missing key.
//
// NSH components run once: every reactive read sits in a vnode getter, an
// `<If>` or a function the JSX calls — never in a body-level local.
import { effect, openUrl, signal, Text, View } from "nsh";
import type {
  SuperagentAttentionView,
  SuperagentTicketView,
} from "../../data/contract";
import {
  attentionKindLabel,
  attentionRouteLabel,
  assignmentStatusLabel,
  mandateStrengthLabel,
  openAttentionsOf,
  reportStateLabel,
  superagentAccess,
  superagentCounts,
  superagentDeadline,
  superagentRequesterText,
  superagentSourceText,
  superagentStatusLabel,
  superagentStatusTone,
} from "../../data/superagent";
import { store } from "../../data/store";
import { can, permissionHint } from "../../data/permissions";
import { capabilityOf } from "../../data/permission-catalog";
import { botAccess } from "../../data/bots";
import { Icon } from "../../ui/icons";
import {
  AlertDialog,
  Badge,
  Button,
  Card,
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  Input,
  ScrollArea,
  alpha,
  color,
  font,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
  type BadgeVariant,
} from "../../ui/kit";
import {
  FactLine,
  Hairline,
  PageEmpty,
  PageError,
  PageLoading,
  PagePermission,
  PaperBody,
  PressRow,
  SectionLabel,
  SheetHead,
  StateNote,
  fieldHeightProp,
  formatWhen,
  isMobile,
  isNarrow,
  touchStyle,
} from "./common";
import { BackToIndex } from "./shell";

// ---- layout ---------------------------------------------------------------

/** The split view's breakpoint (the books shell's tablet floor). Below it the
 * list and the detail are separate screens; never a squeezed third column. */
const splitView = (): boolean => !isNarrow();

/** The detail column's width on a wide screen. The list keeps the rest. */
const DETAIL_WIDTH_PX = 420;

/** The stop confirmation's target (title is read once: the dialog's sentence
 * must not change under the reader when the board refreshes). */
const stopTarget = signal<{ id: string; title: string } | null>(null);

/** The answer drafts, keyed by attention id. Module-level so a refresh does not
 * throw away text the reader already typed; reset with the page's state. */
const drafts = signal<Record<string, string>>({});
/** The option whose answer is in flight (the button's own loading face). */
const answeringOption = signal<string | null>(null);
/** The last "open source" refusal, keyed by the card that asked (`attention:<id>`
 * / `ticket:<id>`): a host without a browser says so in place. */
const openLinkError = signal<{ key: string; message: string } | null>(null);

const openSourceLink = (key: string, url: string): void => {
  if (!url) return;
  void openUrl(url)
    .then(() => {
      if (openLinkError()?.key === key) openLinkError.set(null);
    })
    .catch((error: unknown) => {
      openLinkError.set({
        key,
        message: error instanceof Error && error.message ? error.message : String(error),
      });
    });
};

const openLinkFailure = (key: string): string | null => {
  const failure = openLinkError();
  return failure?.key === key ? failure.message : null;
};

const draftOf = (id: string): string => drafts()[id] ?? "";
const setDraft = (id: string, text: string): void => {
  drafts.update((current) => ({ ...current, [id]: text }));
};
const clearDraft = (id: string): void => {
  drafts.update((current) => {
    if (!(id in current)) return current;
    const next = { ...current };
    delete next[id];
    return next;
  });
};

// ---- shared reads ---------------------------------------------------------

const tickets = (): SuperagentTicketView[] => superagentAccess.board()?.tickets ?? [];

/** The ticket named by the route. On a wide screen no explicit selection
 * means the first row (the list and the detail are visible together); on a
 * narrow screen it means the list is the page. A stale id resolves to
 * `undefined` and the detail says the ticket is gone. */
const activeTicket = (): SuperagentTicketView | undefined => {
  const rows = tickets();
  const id = store.routeObjectId();
  if (id) return rows.find((ticket) => ticket.id === id);
  return splitView() ? rows[0] : undefined;
};

const ticketKnown = (): boolean => {
  const id = store.routeObjectId();
  return id === null || tickets().some((ticket) => ticket.id === id);
};

/** A row/child kept one frame after its source disappeared (an Each removal
 * or a refresh): the getter needs a value, never a throw. */
const emptyTicket = (id: string): SuperagentTicketView => ({
  id,
  title: "未命名工单",
  status: "open",
});

const emptyAttention = (id: string): SuperagentAttentionView => ({
  id,
  workItemId: "",
  prompt: "",
});

const workerName = (ticket: SuperagentTicketView): string => {
  const botId = ticket.activeAssignment?.botId || ticket.workerBot;
  if (!botId) return "未记录员工";
  return botAccess.botNameOf(botId) || botId;
};

/** The row's worker line: the current Assignment's own title and status when
 * there is one, the dispatch count either way. */
const workerLine = (ticket: SuperagentTicketView): string => {
  const assignment = ticket.activeAssignment;
  const dispatches = `${ticket.assignmentCount ?? 0} 次派工`;
  if (!assignment) return `执行员工 ${workerName(ticket)} · 没有执行中的 Assignment · ${dispatches}`;
  return `执行员工 ${workerName(ticket)} · ${assignment.title || "当前任务"} · ${assignmentStatusLabel(assignment.status)} · ${dispatches}`;
};

const ticketActionFailure = (
  ticketId: string,
  kinds: Array<"resume" | "stop">,
): ReturnType<typeof superagentAccess.actionError> => {
  const failure = superagentAccess.actionError();
  if (!failure || failure.id !== ticketId || !kinds.includes(failure.kind as "resume" | "stop")) return null;
  return failure;
};

const answerFailure = (attentionId: string) => {
  const failure = superagentAccess.actionError();
  return failure && failure.id === attentionId && failure.kind === "answer" ? failure : null;
};

/** The current Assignment's report, in the operator's words. The board only
 * carries the current/last Assignment, so the section is named after that
 * Assignment, not a report history. `assignment.error` is NOT a report: it has
 * its own failure note, so it is never repeated as a summary here. */
function reportOf(ticket: SuperagentTicketView): { summary: string; raw?: string } | null {
  const assignment = ticket.activeAssignment;
  if (!assignment) return null;
  const result = assignment.result;
  if (result !== undefined && result !== null) {
    let raw: string;
    if (typeof result === "string") raw = result;
    else {
      try {
        raw = JSON.stringify(result, null, 2);
      } catch {
        raw = String(result);
      }
    }
    let summary = "";
    if (result && typeof result === "object" && !Array.isArray(result)) {
      const record = result as Record<string, unknown>;
      if (typeof record.summary === "string") summary = record.summary;
      if (typeof record.state === "string") {
        summary = summary ? `${reportStateLabel(record.state)} · ${summary}` : `状态 ${reportStateLabel(record.state)}`;
      }
    } else if (typeof result === "string") {
      summary = result;
    }
    if (summary) return { summary };
    const capped = raw.length > 1200 ? `${raw.slice(0, 1200)}\n…` : raw;
    return { summary: "Worker 提交了结构化结果，下面原样显示。", raw: capped };
  }
  return null;
}

const shortId = (value: string | undefined): string => {
  if (!value) return "未记录";
  return value.length > 12 ? `…${value.slice(-8)}` : value;
};

const detailAttentionText = (item: SuperagentAttentionView): string => {
  const deadline = superagentDeadline(item.deadline);
  return `${attentionKindLabel(item.kind)} · ${deadline.text}`;
};

// ---- the page -------------------------------------------------------------

export function SuperagentPage() {
  const allowed = () => can("task.read");
  effect(() => {
    if (allowed()) superagentAccess.ensure();
  });
  // The page's ephemeral UI belongs to this mount: a new account's session
  // must not inherit another reader's drafts or an open confirmation.
  let uiReset = false;
  effect(() => {
    if (uiReset) return;
    uiReset = true;
    drafts.set({});
    answeringOption.set(null);
    stopTarget.set(null);
    openLinkError.set(null);
  });
  // A route change drops the stop confirmation: it must not follow the reader
  // to another ticket.
  let lastRouteId: string | null = null;
  effect(() => {
    const id = store.routeObjectId();
    if (id === lastRouteId) return;
    lastRouteId = id;
    stopTarget.set(null);
  });
  return (
    <View testID="superagent-page" style={{ flex: 1, minHeight: 0 }}>
      <If
        when={allowed()}
        else={
          <View style={{ flex: 1, minHeight: 0, alignItems: "center", justifyContent: "center", padding: space["2xl"] }}>
            <PagePermission capability="查看任务" capabilityKey="task.read" testID="superagent-permission" />
          </View>
        }
      >
        <SuperagentBody />
      </If>
    </View>
  );
}

function SuperagentBody() {
  const board = () => superagentAccess.board();
  const loading = () => superagentAccess.loading() && board() === null;
  const error = () => superagentAccess.error();
  return (
    <ScrollArea testID="superagent-scroll" accessibilityLabel="超级助理工单看板" style={{ flex: 1, minHeight: 0 }}>
      <PaperBody testID="superagent-paper">
        <SuperagentHead />
        <If when={superagentAccess.notice() !== null}>
          <StateNote
            testID="superagent-notice"
            tone="success"
            icon="check"
            text={superagentAccess.notice() ?? ""}
          />
        </If>
        <If when={loading()}>
          <PageLoading rows={6} label="读取工单看板" testID="superagent-loading" />
        </If>
        <If when={!loading() && error() !== null}>
          <If
            when={error()?.status === 403}
            else={
              <PageError
                testID="superagent-error"
                title={error()?.status === 503 ? "这个部署还没有接通工单看板" : "没能读到工单看板"}
                message={error()?.message ?? ""}
                onRetry={() => void superagentAccess.refresh()}
                retrying={superagentAccess.loading()}
                requestId={error()?.requestId}
              />
            }
          >
            <PagePermission
              capability={capabilityOf(error()?.requiredKey ?? "task.read").displayName}
              capabilityKey={error()?.requiredKey ?? "task.read"}
              testID="superagent-permission"
            />
          </If>
        </If>
        <If when={!loading() && error() === null && board() !== null}>
          <SuperagentContent />
        </If>
      </PaperBody>
    </ScrollArea>
  );
}

function SuperagentHead() {
  const counts = () => superagentCounts(tickets());
  return (
    <SheetHead
      testID="superagent-head"
      kicker={{ icon: "inbox", text: "02 / 任务 · 工单" }}
      title="工单看板"
      badges={
        <>
          <If when={superagentAccess.board() !== null}>
            <Badge variant="secondary" label={`${tickets().length} 张工单`} />
          </If>
          <If when={superagentAccess.board() !== null && counts().waiting > 0}>
            <Badge variant="warning" icon="alert" label={`${counts().waiting} 张待人工`} />
          </If>
          <If when={!can("task.write")}>
            <Badge variant="outline" icon="lock" label="只读" />
          </If>
        </>
      }
      description="来源事件被接单后，工单在这里派给员工执行；员工提问时，先回答收件箱里的问题，必要时恢复或停止工单。"
      actions={
        <Button
          testID="superagent-refresh"
          variant="ghost"
          size="sm"
          icon="refresh"
          label={superagentAccess.loading() ? "刷新中…" : "刷新"}
          loading={superagentAccess.loading()}
          accessibilityLabel="刷新工单看板"
          onPress={() => void superagentAccess.refresh()}
          style={touchStyle()}
        />
      }
    />
  );
}

function SuperagentContent() {
  const empty = () =>
    tickets().length === 0 &&
    superagentAccess.inbox().length === 0 &&
    superagentAccess.inboxError() === null;
  return (
    <View style={{ flexDirection: "column", gap: space.lg }}>
      <If when={empty()}>
        <PageEmpty
          testID="superagent-empty"
          icon="inbox"
          title="还没有工单"
          description="来源（例如 GitHub Issue）被 orchestrator 接单后会出现在这里；员工提问时，收件箱会在看板上方出现。"
          primaryLabel="刷新"
          onPrimary={() => void superagentAccess.refresh()}
          primaryTestID="superagent-empty-refresh"
          secondaryLabel="去工作区"
          onSecondary={() => store.openChapter("workspace")}
        />
      </If>
      <If when={!empty()}>
        <If when={tickets().length > 0}>
          <CountTiles />
        </If>
        <AttentionSection />
        <If when={tickets().length > 0}>
          <BoardSection />
        </If>
        <If when={tickets().length === 0}>
          <Text
            testID="superagent-board-empty"
            style={{ fontSize: typeScale.sm, color: color.mutedForeground }}
          >
            看板里还没有工单；收件箱的读取结果以上方为准。
          </Text>
        </If>
      </If>
      <StopDialog />
    </View>
  );
}

// ---- counters -------------------------------------------------------------

const COUNT_TILES: Array<{
  key: "waiting" | "running" | "done" | "failed";
  label: string;
  hint: string;
  icon: "alert" | "activity" | "check" | "x";
  face: () => string;
  ink: () => string;
}> = [
  {
    key: "waiting",
    label: "待人工",
    hint: "有开放问题或被阻塞",
    icon: "alert",
    face: () => color.warningTint,
    ink: () => color.warning,
  },
  {
    key: "running",
    label: "执行中",
    hint: "已派工，员工正在做",
    icon: "activity",
    face: () => color.infoTint,
    ink: () => color.info,
  },
  {
    key: "done",
    label: "完成",
    hint: "已交付并结束",
    icon: "check",
    face: () => color.successTint,
    ink: () => color.success,
  },
  {
    key: "failed",
    label: "失败",
    hint: "执行失败或已停止",
    icon: "x",
    face: () => color.dangerTint,
    ink: () => color.danger,
  },
];

function CountTiles() {
  const counts = () => superagentCounts(tickets());
  // The two transitional states are explicit: a tile's label never claims an
  // `open`/`cancelling` ticket is "执行中" when no worker holds it.
  const extra = () =>
    [
      counts().pending > 0 ? `待派工 ${counts().pending} 张` : "",
      counts().stopping > 0 ? `停止中 ${counts().stopping} 张` : "",
      counts().other > 0 ? `其他状态 ${counts().other} 张` : "",
    ]
      .filter((part) => part !== "")
      .join(" · ");
  return (
    <View testID="superagent-counts" style={{ flexDirection: "column", gap: space.sm }}>
      <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.sm }}>
        <Each of={COUNT_TILES} key={(tile) => tile.key}>
          {(tile) => {
            const value = () => counts()[tile().key];
            return (
              <View
                testID={`superagent-count-${tile().key}`}
                accessibilityLabel={`${tile().label} ${value()} 张`}
                style={{
                  flexGrow: 1,
                  flexBasis: 130,
                  minWidth: 130,
                  flexDirection: "column",
                  gap: space.xxs,
                  padding: space.md,
                  borderRadius: radius.md,
                  backgroundColor: tile().face(),
                  borderWidth: 1,
                  borderColor: alpha(tile().ink(), 0.24),
                }}
              >
                <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}>
                  <Icon name={tile().icon} size={13} tint={tile().ink()} />
                  <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>
                    {tile().label}
                  </Text>
                </View>
                <Text style={{ fontFamily: font.mono, fontSize: typeScale["2xl"], fontWeight: weight.semibold, color: color.foreground }}>
                  {value()}
                </Text>
                <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}>
                  {tile().hint}
                </Text>
              </View>
            );
          }}
        </Each>
      </View>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
        全部 {counts().total} 张工单
        {extra() ? ` · ${extra()}` : ""}
      </Text>
    </View>
  );
}

// ---- the attention inbox --------------------------------------------------

function AttentionSection() {
  const items = () => superagentAccess.inbox();
  const error = () => superagentAccess.inboxError();
  return (
    <View testID="superagent-inbox" style={{ flexDirection: "column", gap: space.md }}>
      <SectionLabel text="待人工收件箱" note={`${items().length} 个开放问题`} testID="superagent-inbox-head" />
      <If when={error() !== null}>
        <StateNote
          testID="superagent-inbox-error"
          tone="danger"
          icon="alert"
          text={`没能读到收件箱：${error()?.message ?? ""}`}
        >
          <Button
            variant="outline"
            size="sm"
            label="重试收件箱"
            accessibilityLabel="重试收件箱"
            onPress={() => void superagentAccess.refresh()}
            style={touchStyle()}
          />
        </StateNote>
      </If>
      <If when={error() === null && items().length === 0}>
        <Text
          testID="superagent-inbox-empty"
          style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.mutedForeground }}
        >
          没有待人工的问题；员工提问后会出现在这里。
        </Text>
      </If>
      <Each of={items()} key={(item) => item.id}>
        {(item) => <AttentionCard attentionId={item().id} />}
      </Each>
    </View>
  );
}

function AttentionCard(props: { attentionId: string }) {
  const attention = () =>
    superagentAccess.inbox().find((row) => row.id === props.attentionId) ?? emptyAttention(props.attentionId);
  const deadline = () => superagentDeadline(attention().deadline);
  const pending = () => {
    const action = superagentAccess.action();
    return action?.id === attention().id && action.kind === "answer";
  };
  const failure = () => answerFailure(attention().id);
  const writable = () => can("task.write");
  const answerable = () => writable() && !deadline().expired && !pending();
  const hasOptions = () => (attention().options ?? []).length > 0;
  const canSubmitText = () => attention().allowFreeText === true && draftOf(attention().id).trim().length > 0;

  const answerOption = (optionId: string) => {
    if (!answerable()) return;
    answeringOption.set(optionId);
    const text = draftOf(attention().id).trim();
    void superagentAccess
      .answer(attention().id, { optionId, text: text || undefined })
      .then((ok) => {
        if (ok) clearDraft(attention().id);
      })
      .finally(() => answeringOption.set(null));
  };

  const answerText = () => {
    const text = draftOf(attention().id).trim();
    if (!answerable() || !text) return;
    void superagentAccess.answer(attention().id, { text }).then((ok) => {
      if (ok) clearDraft(attention().id);
    });
  };

  return (
    <Card
      size="sm"
      testID={`superagent-attention-${attention().id}`}
      accessibilityLabel={`来自工单 ${attention().ticketTitle ?? "未命名"} 的${attentionKindLabel(attention().kind)}问题`}
      style={{
        borderWidth: 1,
        borderColor: deadline().expired ? alpha(color.danger, 0.35) : alpha(color.warning, 0.3),
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: space.sm }}>
        <Badge variant="info" label={attentionKindLabel(attention().kind)} />
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1 }}>
          来自「{attention().ticketTitle ?? "未命名工单"}」
        </Text>
        <View style={{ marginLeft: "auto", flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Badge
            variant={deadline().expired ? "destructive" : "warning"}
            icon="clock"
            label={deadline().text}
            testID={`superagent-attention-${attention().id}-deadline`}
          />
        </View>
      </View>
      <Text
        testID={`superagent-attention-${attention().id}-prompt`}
        style={{
          fontSize: typeScale.base,
          lineHeight: Math.round(typeScale.base * lineHeight.relaxed),
          fontWeight: weight.medium,
          color: color.foreground,
        }}
      >
        {attention().prompt || "（这个问题没有正文）"}
      </Text>
      <View style={{ flexDirection: "column", gap: 2 }}>
        <View style={{ flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: space.sm }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 2 }}>
            来源 {superagentSourceText(attention().thread)}
            {attention().route ? ` · 路由 ${attentionRouteLabel(attention().route)}` : ""}
          </Text>
          <If when={attention().thread?.url}>
            <Button
              variant="link"
              size="sm"
              label="打开来源"
              accessibilityLabel={`打开来源 ${attention().ticketTitle ?? attention().id}`}
              onPress={() => openSourceLink(`attention:${attention().id}`, attention().thread?.url ?? "")}
            />
          </If>
        </View>
        <If when={openLinkFailure(`attention:${attention().id}`) !== null}>
          <Text style={{ fontSize: typeScale.sm, color: color.danger }}>
            打不开来源：{openLinkFailure(`attention:${attention().id}`)}
          </Text>
        </If>
      </View>

      <If when={!writable()}>
        <StateNote
          testID={`superagent-attention-${attention().id}-readonly`}
          tone="warning"
          icon="lock"
          text={`你的账号缺少「写任务」（task.write）能力，答复控件不会显示。${permissionHint("task.write")}`}
        />
      </If>
      <If when={writable() && deadline().expired}>
        <StateNote
          testID={`superagent-attention-${attention().id}-expired`}
          tone="danger"
          icon="clock"
          text={`${deadline().text}。服务端会拒绝过期问题的答复；等系统归档，或由有权限的人先恢复工单。`}
        />
      </If>
      <If when={writable() && !deadline().expired && !hasOptions() && attention().allowFreeText !== true}>
        <StateNote
          tone="warning"
          icon="info"
          text="这个问题没有选项，也没有允许自由文本；服务端不会接受答复，请让管理员检查员工的提问。"
        />
      </If>

      <If when={writable() && !deadline().expired && hasOptions()}>
        <View style={{ flexDirection: "column", gap: space.sm }}>
          <Each of={attention().options ?? []} key={(option) => option.id}>
            {(option) => (
              <View style={{ flexDirection: "column", gap: 2 }}>
                <Button
                  testID={`superagent-attention-${attention().id}-option-${option().id}`}
                  variant="outline"
                  size="field"
                  label={
                    pending() && answeringOption() === option().id ? "提交中…" : option().label || option().id
                  }
                  accessibilityLabel={`答复：${option().label || option().id}`}
                  disabled={!answerable()}
                  loading={pending() && answeringOption() === option().id}
                  onPress={() => answerOption(option().id)}
                  style={touchStyle()}
                />
                {option().description ? (
                  <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{option().description}</Text>
                ) : null}
              </View>
            )}
          </Each>
        </View>
      </If>

      <If when={writable() && !deadline().expired && attention().allowFreeText === true}>
        <View style={{ flexDirection: isMobile() ? "column" : "row", alignItems: "stretch", gap: space.sm }}>
          <Input
            testID={`superagent-attention-${attention().id}-text`}
            value={draftOf(attention().id)}
            onChange={(value) => setDraft(attention().id, value)}
            placeholder={hasOptions() ? "补充说明（可随所选选项一起提交）" : "用一段话回答"}
            accessibilityLabel="自由文本答复"
            size="field"
            height={fieldHeightProp()}
            disabled={pending()}
            onSubmit={() => answerText()}
            style={{ flex: 1, minWidth: 0 }}
          />
          <Button
            testID={`superagent-attention-${attention().id}-answer`}
            size="field"
            label={pending() && answeringOption() === null ? "提交中…" : "提交答复"}
            accessibilityLabel="提交答复"
            disabled={!canSubmitText() || pending()}
            loading={pending() && answeringOption() === null}
            onPress={answerText}
            style={touchStyle()}
          />
        </View>
      </If>

      <If when={failure() !== null}>
        <StateNote
          testID={`superagent-attention-${attention().id}-error`}
          tone="danger"
          icon="alert"
          text={`答复失败：${failure()?.failure.message ?? ""}`}
        >
          <If when={failure()?.failure.status === 403}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              即使账号已有 task.write，服务端还要求 operator 授权（OperatorAuthorizer）。
            </Text>
          </If>
        </StateNote>
      </If>
    </Card>
  );
}

// ---- the board ------------------------------------------------------------

function BoardSection() {
  const selected = () => store.routeObjectId();
  return (
    <View testID="superagent-board" style={{ flexDirection: "column", gap: space.md }}>
      <SectionLabel text="工单" note={`${tickets().length} 张 · 按最近更新`} testID="superagent-board-head" />
      <If when={splitView()}>
        <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.lg }}>
          <View style={{ flex: 1, minWidth: 0 }}>
            <TicketList />
          </View>
          <View
            testID="superagent-detail-column"
            style={{
              width: DETAIL_WIDTH_PX,
              flexShrink: 0,
              borderLeftWidth: 1,
              borderLeftColor: color.border,
              paddingLeft: space.lg,
            }}
          >
            <TicketDetail />
          </View>
        </View>
      </If>
      <If when={!splitView()}>
        <If when={selected() !== null} else={<TicketList />}>
          <BackToIndex label="工单列表" onPress={() => store.closeSuperagentTicket()} />
          <TicketDetail />
        </If>
      </If>
    </View>
  );
}

function TicketList() {
  return (
    <View
      testID="superagent-ticket-list"
      accessibilityLabel="工单列表"
      style={{
        flexDirection: "column",
        borderWidth: 1,
        borderColor: color.border,
        borderRadius: radius.md,
        backgroundColor: color.panel,
        overflow: "hidden",
      }}
    >
      <Each of={tickets()} key={(ticket) => ticket.id}>
        {(ticket) => <TicketRow ticketId={ticket().id} />}
      </Each>
    </View>
  );
}

function TicketRow(props: { ticketId: string }) {
  const ticket = () => tickets().find((row) => row.id === props.ticketId) ?? emptyTicket(props.ticketId);
  const selected = () => store.routeObjectId() === ticket().id;
  const open = () => ticket().openAttentionCount ?? 0;
  return (
    <PressRow
      testID={`superagent-ticket-${ticket().id}`}
      accessibilityLabel={`打开工单 ${ticket().title || "未命名工单"}`}
      selected={selected()}
      onPress={() => store.openSuperagentTicket(ticket().id)}
      style={{ alignItems: "flex-start", paddingVertical: space.sm }}
    >
      <View style={{ flex: 1, minWidth: 0, flexDirection: "column", gap: 3 }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <Badge
            variant={superagentStatusTone(ticket().status) as BadgeVariant}
            label={superagentStatusLabel(ticket().status)}
            testID={`superagent-ticket-status-${ticket().id}`}
          />
          <Text
            style={{
              flex: 1,
              minWidth: 0,
              fontSize: typeScale.base,
              fontWeight: weight.medium,
              color: color.foreground,
              numberOfLines: 1,
              textOverflow: "ellipsis",
            }}
          >
            {ticket().title || "未命名工单"}
          </Text>
        </View>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1 }}>
          {superagentRequesterText(ticket())} · {superagentSourceText(ticket().thread)} · 更新{" "}
          {formatWhen(ticket().updatedAt ?? ticket().createdAt)}
        </Text>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground, numberOfLines: 1 }}>
          {workerLine(ticket())}
        </Text>
      </View>
      <If when={open() > 0}>
        <View style={{ alignItems: "flex-end", gap: space.xxs, flexShrink: 0 }}>
          <Badge
            variant="warning"
            icon="alert"
            label={`待答 ${open()}`}
            testID={`superagent-ticket-open-${ticket().id}`}
          />
        </View>
      </If>
    </PressRow>
  );
}

// ---- the detail -----------------------------------------------------------

function TicketDetail() {
  const ticket = () => activeTicket();
  const known = () => ticketKnown();
  return (
    <View testID="superagent-detail" style={{ flexDirection: "column", gap: space.md }}>
      <If when={!known()}>
        <Empty testID="superagent-detail-missing" accessibilityLabel="工单不在了">
          <EmptyMedia icon="alert" tone="danger" />
          <EmptyTitle label="这张工单不在了" />
          <EmptyDescription label="地址里的工单 id 不在当前看板里；它可能已被删除，或属于另一个账号。" />
          <EmptyContent>
            <Button
              label="返回工单列表"
              accessibilityLabel="返回工单列表"
              onPress={() => store.closeSuperagentTicket()}
              style={touchStyle()}
            />
          </EmptyContent>
        </Empty>
      </If>
      <If when={known() && ticket() === undefined}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          看板里还没有可打开的工单。
        </Text>
      </If>
      <If when={known() && ticket() !== undefined}>
        <TicketDetailBody />
      </If>
    </View>
  );
}

function TicketDetailBody() {
  const ticket = () => activeTicket() ?? emptyTicket("");
  const report = () => reportOf(ticket());
  const assignment = () => ticket().activeAssignment;
  const open = () => openAttentionsOf(superagentAccess.inbox(), ticket().id);
  const inboxFailed = () => superagentAccess.inboxError() !== null;
  const revoked = () => !!ticket().mandate?.revokedAt;
  const strength = () => mandateStrengthLabel(ticket().mandate?.strength);
  const source = () => superagentSourceText(ticket().thread, ticket().source?.kind);
  return (
    <View style={{ flexDirection: "column", gap: space.md }}>
      <View style={{ flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: space.sm }}>
        <Badge
          variant={superagentStatusTone(ticket().status) as BadgeVariant}
          label={superagentStatusLabel(ticket().status)}
          testID="superagent-detail-status"
        />
        <Badge variant="outline" label={ticket().modeId ?? "superagent"} />
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          更新 {formatWhen(ticket().updatedAt ?? ticket().createdAt)}
        </Text>
      </View>
      <Text
        testID="superagent-detail-title"
        style={{
          fontSize: typeScale["2xl"],
          lineHeight: Math.round(typeScale["2xl"] * lineHeight.tight),
          fontWeight: weight.semibold,
          letterSpacing: -0.3,
          color: color.foreground,
        }}
      >
        {ticket().title || "未命名工单"}
      </Text>
      <View style={{ flexDirection: "column", gap: 2 }}>
        <View style={{ flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: space.sm }}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            {source()} · 创建 {formatWhen(ticket().createdAt)}
          </Text>
          <If when={ticket().thread?.url}>
            <Button
              variant="link"
              size="sm"
              label="打开来源"
              accessibilityLabel={`打开来源 ${ticket().title || ticket().id}`}
              onPress={() => openSourceLink(`ticket:${ticket().id}`, ticket().thread?.url ?? "")}
            />
          </If>
        </View>
        <If when={openLinkFailure(`ticket:${ticket().id}`) !== null}>
          <Text style={{ fontSize: typeScale.sm, color: color.danger }}>
            打不开来源：{openLinkFailure(`ticket:${ticket().id}`)}
          </Text>
        </If>
      </View>

      <SectionLabel text="说明" />
      <View
        style={{
          padding: space.md,
          borderRadius: radius.sm,
          backgroundColor: color.muted,
          borderLeftWidth: 2,
          borderLeftColor: color.warning,
        }}
      >
        <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.relaxed), color: color.foreground }}>
          {ticket().request || "来源事件没有附带正文。"}
        </Text>
      </View>

      <SectionLabel text="Mandate" note={revoked() ? "已撤销" : "有效"} />
      <View style={{ flexDirection: "column", gap: space.xs }}>
        <FactLine label="发起人" value={superagentRequesterText(ticket())} />
        <FactLine label="来源" value={source()} />
        <FactLine label="强度" value={strength()} />
        <FactLine label="接受" value={`${formatWhen(ticket().mandate?.acceptedAt)}${ticket().mandate?.acceptedBy ? ` · ${ticket().mandate?.acceptedBy}` : ""}`} />
        <FactLine label="策略" value={ticket().mandate?.policyVersion || "未记录"} mono />
      </View>
      <If when={revoked()}>
        <StateNote
          testID="superagent-detail-revoked"
          tone="danger"
          icon="lock"
          text={`Mandate 已撤销：${formatWhen(ticket().mandate?.revokedAt)}${ticket().mandate?.revokedBy ? ` · ${ticket().mandate?.revokedBy}` : ""}${ticket().mandate?.revocationCause ? ` · ${ticket().mandate?.revocationCause}` : ""}。这张工单不再接受自动输入。`}
        />
      </If>

      <SectionLabel text="Worker" note={`${ticket().assignmentCount ?? 0} 次派工`} />
      <If
        when={assignment() !== undefined}
        else={
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            这张工单当前没有执行中的 Assignment
            {(ticket().assignmentCount ?? 0) > 0 ? `（历史 ${ticket().assignmentCount} 次，看板只投影当前一次）` : ""}。
          </Text>
        }
      >
        <View style={{ flexDirection: "column", gap: space.xs }}>
          <FactLine label="员工" value={workerName(ticket())} />
          <FactLine label="状态" value={assignmentStatusLabel(assignment()?.status)} />
          <FactLine label="会话" value={shortId(assignment()?.sessionId)} mono />
          <FactLine label="派工" value={formatWhen(assignment()?.createdAt)} />
          <FactLine label="更新" value={formatWhen(assignment()?.updatedAt)} />
        </View>
      </If>
      <If when={report() !== null}>
        <View testID="superagent-detail-report" style={{ flexDirection: "column", gap: space.xs }}>
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.foreground }}>当前 Assignment 报告</Text>
          <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.foreground }}>
            {report()?.summary}
          </Text>
          <If when={report()?.raw !== undefined && report()?.raw !== report()?.summary}>
            <View
              style={{
                backgroundColor: color.muted,
                borderRadius: radius.sm,
                padding: space.sm,
              }}
            >
              <Text
                style={{
                  fontFamily: font.mono,
                  fontSize: typeScale.sm,
                  lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                  color: color.mutedForeground,
                }}
              >
                {report()?.raw}
              </Text>
            </View>
          </If>
        </View>
      </If>
      <If when={assignment()?.error}>
        <StateNote tone="danger" icon="alert" text={`Assignment 错误：${assignment()?.error ?? ""}`} />
      </If>

      <SectionLabel text="Attention" note={`${open().length} 个开放`} />
      <If when={inboxFailed()}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          收件箱读取失败，问题列表不可用；重试收件箱后再看。
        </Text>
      </If>
      <If when={!inboxFailed() && open().length === 0}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>没有开放的问题。</Text>
      </If>
      <Each of={open()} key={(item) => item.id}>
        {(item) => (
          <View
            testID={`superagent-detail-attention-${item().id}`}
            style={{ flexDirection: "column", gap: 2, paddingVertical: space.xs, borderBottomWidth: 1, borderBottomColor: color.border }}
          >
            <Text style={{ fontSize: typeScale.sm, color: color.foreground, numberOfLines: 2 }}>{item().prompt}</Text>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              {detailAttentionText(item())} · 到上方收件箱答复
            </Text>
          </View>
        )}
      </Each>

      <SectionLabel text="待投递输入" note={`${ticket().failedPendingCount ?? 0} 个失败 / ${ticket().pendingCount ?? (ticket().pending?.length ?? 0)} 个待投递`} />
      <PendingInputs />

      <Hairline />
      <TicketActions />
    </View>
  );
}

/** A pending input's id is a server fact ("fixture:e1", "attention:<uuid>");
 * the DOM/test id keeps it readable without its separators. */
const pendingDomId = (id: string): string => id.replace(/[^a-zA-Z0-9_-]+/g, "-");

/** The durable input queue of one ticket: an answer or follow-up captured for a
 * worker but not yet delivered. A failed entry is the operator's decision point
 * (retry after fixing the seam, or dismiss); it is never silently dropped. */
function PendingInputs() {
  const ticket = () => activeTicket() ?? emptyTicket("");
  const entries = () => ticket().pending ?? [];
  const writable = () => can("task.write");
  const actionOf = (id: string) => {
    const action = superagentAccess.action();
    return action?.id === id ? action.kind : null;
  };
  const failureOf = (id: string) => {
    const failure = superagentAccess.actionError();
    return failure && failure.id === id ? failure : null;
  };
  return (
    <View testID="superagent-detail-pending" style={{ flexDirection: "column", gap: space.sm }}>
      <If when={entries().length === 0}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>没有等待投递的输入。</Text>
      </If>
      <Each of={entries()} key={(entry) => entry.id}>
        {(entry) => {
          const domId = `superagent-pending-${pendingDomId(entry().id)}`;
          return (
            <View
              testID={domId}
              style={{
                flexDirection: "column",
                gap: space.xs,
                padding: space.sm,
                borderRadius: radius.sm,
                borderWidth: 1,
                borderColor: entry().failed ? color.danger : color.border,
                backgroundColor: color.muted,
              }}
            >
              <View style={{ flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: space.sm }}>
                <Badge
                  variant={entry().failed ? "destructive" : "warning"}
                  label={entry().failed ? "投递失败" : "等待投递"}
                  testID={`${domId}-status`}
                />
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  {entry().kind === "attention_answer" ? "人工答复" : entry().allowDispatch ? "人类跟进" : "不可信上下文"}
                  {" · "}尝试 {entry().attempts ?? 0} 次
                </Text>
              </View>
              <Text style={{ fontSize: typeScale.sm, lineHeight: Math.round(typeScale.sm * lineHeight.normal), color: color.foreground }}>
                {entry().text || "（没有正文）"}
              </Text>
              <If when={entry().lastError}>
                <Text testID={`${domId}-error`} style={{ fontSize: typeScale.sm, color: color.danger }}>
                  错误：{entry().lastError}
                </Text>
              </If>
              <If when={writable()}>
                <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
                  <Button
                    testID={`${domId}-retry`}
                    size="field"
                    icon="play"
                    label={actionOf(entry().id) === "retry" ? "重试中…" : "重试投递"}
                    accessibilityLabel="重试投递"
                    disabled={actionOf(entry().id) !== null}
                    loading={actionOf(entry().id) === "retry"}
                    onPress={() => {
                      const current = ticket();
                      if (current) void superagentAccess.retryPending(current.id, entry().id);
                    }}
                    style={touchStyle()}
                  />
                  <Button
                    testID={`${domId}-dismiss`}
                    variant="destructive"
                    size="field"
                    icon="trash"
                    label={actionOf(entry().id) === "dismiss" ? "清除中…" : "清除"}
                    accessibilityLabel="清除这条待投递输入"
                    disabled={actionOf(entry().id) !== null}
                    loading={actionOf(entry().id) === "dismiss"}
                    onPress={() => {
                      const current = ticket();
                      if (current) void superagentAccess.dismissPending(current.id, entry().id);
                    }}
                    style={touchStyle()}
                  />
                </View>
              </If>
              <If when={failureOf(entry().id) !== null}>
                <StateNote
                  testID={`${domId}-action-error`}
                  tone="danger"
                  icon="alert"
                  text={failureOf(entry().id)?.failure.message ?? ""}
                />
              </If>
            </View>
          );
        }}
      </Each>
      <If when={!writable() && entries().length > 0}>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          你的账号缺少「写任务」（task.write）能力，不能重试或清除待投递输入。{permissionHint("task.write")}
        </Text>
      </If>
    </View>
  );
}

function TicketActions() {
  const ticket = () => activeTicket();
  const writable = () => can("task.write");
  const pending = () => {
    const action = superagentAccess.action();
    return !!ticket() && action?.id === ticket()?.id;
  };
  const failure = () => {
    const current = ticket();
    return current ? ticketActionFailure(current.id, ["resume", "stop"]) : null;
  };
  const revoked = () => !!ticket()?.mandate?.revokedAt;
  const status = () => ticket()?.status ?? "";
  const canResume = () => !revoked() && (status() === "blocked" || status() === "done" || status() === "failed");
  const canStop = () => !revoked() && status() !== "cancelled" && status() !== "cancelling";
  const opened = () => {
    const current = ticket();
    if (current) stopTarget.set({ id: current.id, title: current.title || "未命名工单" });
  };
  return (
    <View testID="superagent-detail-actions" style={{ flexDirection: "column", gap: space.sm }}>
      <SectionLabel text="操作" note="恢复 / 停止" />
      <If when={!writable()}>
        <StateNote
          testID="superagent-write-denied"
          tone="warning"
          icon="lock"
          text={`你的账号缺少「写任务」（task.write）能力，恢复与停止按钮不会显示。${permissionHint("task.write")}`}
        />
      </If>
      <If when={writable()}>
        <View style={{ flexDirection: "row", flexWrap: "wrap", alignItems: "center", gap: space.sm }}>
          <If when={canResume()}>
            <Button
              testID="superagent-ticket-resume"
              size="field"
              icon="play"
              label={pending() && superagentAccess.action()?.kind === "resume" ? "恢复中…" : "恢复工单"}
              accessibilityLabel="恢复工单"
              disabled={pending()}
              loading={pending() && superagentAccess.action()?.kind === "resume"}
              onPress={() => {
                const current = ticket();
                if (current) void superagentAccess.resume(current.id);
              }}
              style={touchStyle()}
            />
          </If>
          <If when={canStop()}>
            <Button
              testID="superagent-ticket-stop"
              variant="destructive"
              size="field"
              icon="stop"
              label={pending() && superagentAccess.action()?.kind === "stop" ? "停止中…" : "停止工单"}
              accessibilityLabel="停止工单"
              disabled={pending()}
              loading={pending() && superagentAccess.action()?.kind === "stop"}
              onPress={opened}
              style={touchStyle()}
            />
          </If>
          <If when={!canResume() && !canStop()}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              {revoked() ? "Mandate 已撤销，这张工单不能再恢复或停止。" : "这张工单正在停止中，等待服务端完成取消。"}
            </Text>
          </If>
        </View>
        <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
          恢复会重开待人工、完成或失败（failed）的工单并派一次人工跟进；停止会撤销 Mandate 并取消绑定的会话。两个操作还会经过服务端的 operator 授权检查。
        </Text>
      </If>
      <If when={failure() !== null}>
        <StateNote
          testID="superagent-detail-action-error"
          tone="danger"
          icon="alert"
          text={`${failure()?.kind === "resume" ? "恢复" : "停止"}失败：${failure()?.failure.message ?? ""}`}
        >
          <If when={failure()?.failure.status === 403}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
              即使账号已有 task.write，服务端还要求 operator 授权（OperatorAuthorizer）。
            </Text>
          </If>
        </StateNote>
      </If>
    </View>
  );
}

// ---- the stop confirmation ------------------------------------------------

function StopDialog() {
  const target = () => stopTarget();
  return (
    <AlertDialog
      testID="superagent-stop-dialog"
      open={target() !== null}
      title="停止这张工单？"
      description={
        target()
          ? `「${target()!.title}」将被停止：Mandate 被撤销，绑定的员工会话取消；记录保留可审计。`
          : ""
      }
      confirmLabel="停止工单"
      cancelLabel="取消"
      destructive
      onConfirm={() => {
        const current = target();
        stopTarget.set(null);
        if (current) void superagentAccess.stop(current.id);
      }}
      onCancel={() => stopTarget.set(null)}
    />
  );
}
