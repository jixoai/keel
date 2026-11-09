// The conversation: a bottom-aligned List (new content grows from the bottom;
// only a list already at its end follows a streaming reply), the approval
// cards, the run state and the composer. It owns no conversation semantics —
// the projection lives in the data layer.
//
// NSH components run once and update through fine-grained bindings: every
// signal/prop is read inside JSX or an `<If>`, never copied into a local at
// the top of a component (that would freeze the value).
import { cleanup, List, type ListRef, platform, signal, Text, View } from "nsh";
import type { ConversationView, DecisionView, TurnView } from "../data/contract";
import { store } from "../data/store";
import { Icon } from "../ui/icons";
import { IconButton, PillButton, Spinner, StatusPill } from "../ui/primitives";
import { conversationState } from "../ui/run-state";
import { alpha, colors, space } from "../ui/theme";
import { ApprovalCard, EmptyConversation, TurnRow } from "./messages";
import { Composer } from "./composer";
import { ConnectionBadge } from "./session-list";
import { pendingDeleteId } from "./delete-dialog";
import { SessionModelChip } from "./model-picker";

interface Row {
  kind: "turn" | "decision";
  id: string;
  turn?: TurnView;
  decision?: DecisionView;
}

const following = signal(true);
const submitting = signal<Record<string, boolean>>({});
const now = signal(Date.now());

const rowsOf = (view: ConversationView | null): Row[] => {
  if (!view) return [];
  return [
    ...view.turns.map((turn): Row => ({ kind: "turn", id: turn.turnId, turn })),
    ...view.decisions.map((decision): Row => ({ kind: "decision", id: `decision-${decision.decisionId}`, decision })),
  ];
};

function ConversationHeader(props: {
  title: string;
  wide: boolean;
  view: ConversationView | null;
  onBack: () => void;
}) {
  return (
    <View
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.sm,
        height: 48,
        paddingHorizontal: space.md,
        borderBottomWidth: 1,
        borderBottomColor: colors.headerBorder,
        backgroundColor: colors.background,
        zIndex: 4,
      }}
    >
      <If when={!props.wide}>
        <IconButton
          icon="chevron-left"
          label="返回会话列表"
          size={platform.idiom === "terminal" ? 30 : 44}
          iconSize={20}
          onPress={props.onBack}
        />
      </If>
      <View style={{ flex: 1, gap: 1, alignItems: "flex-start" }}>
        <Text style={{ numberOfLines: 1, textOverflow: "ellipsis", fontSize: 14, fontWeight: "700", color: colors.foreground }}>
          {props.title}
        </Text>
        <If when={platform.idiom !== "terminal" && props.view?.connection.status !== "reconnecting"}>
          <ConnectionBadge />
        </If>
      </View>
      <SessionModelChip />
      <StatusPill state={conversationState(props.view)} />
    </View>
  );
}

function RowCell(props: { row: () => Row; wide: boolean }) {
  const isLastRunning = () => {
    const turns = store.conversation()?.turns ?? [];
    const last = turns[turns.length - 1];
    const turn = props.row().turn;
    return !!turn && !!last && turn.turnId === last.turnId && last.status === "running";
  };
  const isActiveDecision = (decisionId: string) => {
    const decisions = store.conversation()?.decisions ?? [];
    for (let index = decisions.length - 1; index >= 0; index -= 1) {
      if (decisions[index].status === "open") return decisions[index].decisionId === decisionId;
    }
    return false;
  };
  return (
    <View
      style={{
        width: "100%",
        maxWidth: 900,
        alignSelf: "center",
        paddingHorizontal: props.wide ? 24 : 12,
      }}
    >
      <If
        when={props.row().kind === "turn"}
        else={
          <View style={{ paddingVertical: space.md }}>
            <ApprovalCard
              decision={() => props.row().decision!}
              active={() => isActiveDecision(props.row().decision!.decisionId)}
              timeoutAt={() => store.decisionTimeout(props.row().decision!.decisionId)}
              submitting={() => !!submitting()[props.row().decision!.decisionId]}
              now={() => now()}
              onAnswer={(decisionId, optionId) => {
                if (submitting()[decisionId]) return;
                submitting.update((map) => ({ ...map, [decisionId]: true }));
                void store.answerDecision(decisionId, optionId).finally(() => {
                  submitting.update((map) => {
                    const next = { ...map };
                    delete next[decisionId];
                    return next;
                  });
                });
              }}
            />
          </View>
        }
      >
        <TurnRow
          turn={() => props.row().turn!}
          attachmentsFor={(requestId) => store.attachmentsFor(requestId)}
          partialText={() => store.partial()}
          isLastRunning={isLastRunning}
          waiting={() => store.waiting()}
        />
      </If>
    </View>
  );
}

function NoSession(props: { wide: boolean; onOpenList?: () => void }) {
  return (
    <View style={{ flex: 1, alignItems: "center", justifyContent: "center", padding: 24, gap: 14 }}>
      <View
        style={{
          width: 64,
          height: 64,
          borderRadius: 20,
          alignItems: "center",
          justifyContent: "center",
          backgroundColor: colors.muted,
          borderWidth: 1,
          borderColor: colors.border,
        }}
      >
        <Icon name="message" size={28} tint={colors.brand} />
      </View>
      <Text style={{ fontSize: 18, fontWeight: "700", color: colors.foreground }}>选择一个会话</Text>
      <Text style={{ maxWidth: 420, textAlign: "center", fontSize: 12.5, lineHeight: 19, color: colors.mutedForeground }}>
        从左侧选择一个会话，或新建一个开始。
      </Text>
      <Text style={{ maxWidth: 420, textAlign: "center", fontSize: 12.5, lineHeight: 19, color: colors.mutedForeground }}>
        审批、工具和流式回复会实时出现在这里。
      </Text>
      <If
        when={!props.wide}
        else={<PillButton label="新建会话" tone="primary" onPress={() => void store.createSession()} />}
      >
        <PillButton label="打开会话列表" tone="primary" onPress={() => props.onOpenList?.()} />
      </If>
    </View>
  );
}

export function Conversation(props: { wide: boolean; onOpenList?: () => void; onBack?: () => void }) {
  let list: ListRef | undefined;
  const title = () => {
    const id = store.activeId();
    const row = store.sessions().find((item) => item.sessionId === id);
    return row?.title || (id ? id.slice(0, 12) : "会话");
  };
  const jumpToEnd = () => {
    following.set(true);
    list?.scrollToEnd({ animated: true });
  };
  const timer = setInterval(() => {
    const decisions = store.conversation()?.decisions ?? [];
    if (decisions.some((decision) => decision.status === "open")) now.set(Date.now());
  }, 1000);
  cleanup(() => clearInterval(timer));

  return (
    <View style={{ flex: 1, backgroundColor: colors.background }}>
      <If when={store.activeId()} else={<NoSession wide={props.wide} onOpenList={props.onOpenList} />}>
        <View style={{ flex: 1 }}>
          <ConversationHeader
            title={title()}
            wide={props.wide}
            view={store.conversation()}
            onBack={props.onBack ?? (() => store.closeSession())}
          />
          <If when={store.connection().status === "reconnecting"}>
            <View
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: space.sm,
                paddingHorizontal: space.md,
                height: 30,
                backgroundColor: colors.warningTint,
                borderBottomWidth: 1,
                borderBottomColor: alpha(colors.warning, 0.26),
              }}
            >
              <Spinner size={11} tint={colors.warning} />
              <Text style={{ fontSize: 11, color: colors.warning }}>
                连接中断，正在续接缺失的事件；缺口会补发且不重复。
              </Text>
            </View>
          </If>
          <View style={{ flex: 1, minHeight: 0 }}>
            <If
              when={store.conversation()}
              else={
                <View style={{ flex: 1, alignItems: "center", justifyContent: "center", gap: 10 }}>
                  <Spinner size={18} tint={colors.brand} />
                  <Text style={{ fontSize: 12, color: colors.mutedForeground }}>正在载入事件快照…</Text>
                </View>
              }
            >
              <If
                when={rowsOf(store.conversation()).length > 0}
                else={<EmptyConversation onSuggestion={(text) => void store.send(text, "queue", [])} />}
              >
                <List
                  ref={list}
                  each={rowsOf(store.conversation())}
                  keyExtractor={(row) => row.id}
                  estimatedItemSize={160}
                  alignment="bottom"
                  scrollbar={platform.idiom === "terminal" ? false : undefined}
                  style={{ flex: 1 }}
                  contentStyle={{ paddingTop: 16, paddingBottom: 12 }}
                  onScroll={(event) => {
                    following.set(event.offset.y + event.size.height >= event.contentSize.height - 56);
                  }}
                >
                  {(row) => <RowCell row={row} wide={props.wide} />}
                </List>
              </If>
            </If>
            {/* Content scrolling under the header softens instead of hard-cutting. */}
            <View
              style={{
                position: "absolute",
                top: 0,
                left: 0,
                right: 0,
                height: 22,
                backgroundImage: `linear-gradient(to bottom, ${colors.background}, ${alpha(colors.background, 0)})`,
                pointerEvents: "none",
                zIndex: 3,
              }}
            />
            <If when={!following()}>
              <View style={{ position: "absolute", right: 18, bottom: 12 }}>
                <PillButton label="回到底部" icon="arrow-down" compact onPress={jumpToEnd} />
              </View>
            </If>
          </View>
          <View style={{ paddingTop: 12, paddingBottom: 10 }}>
            <Composer view={store.conversation()} compact={!props.wide} />
          </View>
          {/* The right pane never keeps showing a session that is about to be
              deleted: the confirmation covers it with a neutral placeholder. */}
          <If when={pendingDeleteId() === store.activeId()}>
            <View
              style={{
                position: "absolute",
                top: 48,
                left: 0,
                right: 0,
                bottom: 0,
                zIndex: 3,
                backgroundColor: colors.background,
                alignItems: "center",
                justifyContent: "center",
                gap: 8,
                padding: 24,
              }}
            >
              <Icon name="trash" size={22} tint={colors.mutedForeground} />
              <Text style={{ fontSize: 13, fontWeight: "600", color: colors.mutedForeground }}>
                此会话等待删除确认
              </Text>
              <Text style={{ fontSize: 11.5, color: colors.mutedForeground }}>取消后可以继续查看。</Text>
            </View>
          </If>
        </View>
      </If>
    </View>
  );
}
