// The conversation: a bottom-aligned List (new content grows from the bottom;
// only a list already at its end follows a streaming reply), the approval
// cards, the run state and the composer. It owns no conversation semantics —
// the projection lives in the data layer.
//
// NSH components run once and update through fine-grained bindings: every
// signal/prop is read inside JSX or an `<If>`, never copied into a local at
// the top of a component (that would freeze the value).
import { cleanup, List, type ListRef, signal, Text, View } from "nsh";
import type { ConversationView, DecisionView, TurnView } from "../data/contract";
import { store } from "../data/store";
import { Icon } from "../ui/icons";
import { IconButton, PillButton, Spinner, StatusDot } from "../ui/primitives";
import { alpha, colors, radius, space } from "../ui/theme";
import { ApprovalCard, EmptyConversation, TurnRow } from "./messages";
import { Composer } from "./composer";
import { ConnectionBadge } from "./session-list";

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

const runLabel = (view: ConversationView | null): string => {
  if (!view) return "未连接";
  if (view.stopping) return "停止中";
  if (view.busy) return "运行中";
  const last = view.turns[view.turns.length - 1];
  if (last?.status === "failed") return "上次失败";
  if (last?.status === "interrupted") return "已停止";
  return "就绪";
};

const runTone = (view: ConversationView | null): "running" | "done" | "failed" | "idle" => {
  if (!view) return "idle";
  if (view.busy || view.stopping) return "running";
  const last = view.turns[view.turns.length - 1];
  return last?.status === "failed" ? "failed" : last?.status === "interrupted" ? "idle" : "done";
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
        borderBottomColor: colors.border,
        backgroundColor: alpha(colors.background, 0.86),
      }}
    >
      <If when={!props.wide}>
        <IconButton icon="chevron-left" label="返回会话列表" size={30} onPress={props.onBack} />
      </If>
      <View style={{ flex: 1, gap: 1 }}>
        <Text style={{ numberOfLines: 1, fontSize: 14, fontWeight: "700", color: colors.foreground }}>
          {props.title}
        </Text>
        <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
          <StatusDot tone={runTone(props.view)} size={6} />
          <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>{runLabel(props.view)}</Text>
          <Text style={{ fontSize: 10, color: colors.mutedForeground }}>·</Text>
          <ConnectionBadge />
        </View>
      </View>
      <If when={!!props.view?.busy}>
        <View
          style={{
            flexDirection: "row",
            alignItems: "center",
            gap: 5,
            paddingHorizontal: 8,
            height: 24,
            borderRadius: radius.pill,
            backgroundColor: props.view?.stopping ? colors.dangerTint : colors.brandTint,
          }}
        >
          <Spinner size={10} tint={props.view?.stopping ? colors.danger : colors.brand} />
          <Text
            style={{
              fontSize: 10.5,
              fontWeight: "600",
              color: props.view?.stopping ? colors.danger : colors.brand,
            }}
          >
            {props.view?.stopping ? "停止中" : "运行中"}
          </Text>
        </View>
      </If>
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
          backgroundColor: colors.brandMuted,
          borderWidth: 1,
          borderColor: alpha(colors.brand, 0.28),
        }}
      >
        <Icon name="message" size={28} tint={colors.brand} />
      </View>
      <Text style={{ fontSize: 18, fontWeight: "700", color: colors.foreground }}>选择一个会话</Text>
      <Text style={{ maxWidth: 420, textAlign: "center", fontSize: 12.5, lineHeight: 19, color: colors.mutedForeground }}>
        从左侧选择会话，或新建一个。演示数据里包含工具卡、引导等待、审批、停止与压缩等状态。
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
                连接中断，正在用 after={store.connection().lastSeq} 续传；缺口会补发且不重复。
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
                  style={{ flex: 1 }}
                  contentStyle={{ paddingTop: 10, paddingBottom: 12 }}
                  onScroll={(event) => {
                    following.set(event.offset.y + event.size.height >= event.contentSize.height - 56);
                  }}
                >
                  {(row) => <RowCell row={row} wide={props.wide} />}
                </List>
              </If>
            </If>
            <If when={!following()}>
              <View style={{ position: "absolute", right: 18, bottom: 12 }}>
                <PillButton label="回到底部" icon="arrow-down" compact onPress={jumpToEnd} />
              </View>
            </If>
          </View>
          <View style={{ paddingTop: 8, paddingBottom: 10 }}>
            <Composer view={store.conversation()} compact={!props.wide} />
          </View>
        </View>
      </If>
    </View>
  );
}
