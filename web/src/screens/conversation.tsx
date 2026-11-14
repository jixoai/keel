// The conversation panel — the column's geometry, the row list, and the
// composer's anchor. Nothing else belongs to this file.
//
// One geometry owner, two consumers. Every number of the column lives in the
// kit's layout blocks (change keel-ui-kit): `ConversationColumn` carries the
// inset (48 / 192 per side), the cap (896 / 1152) and the 168px rail shift, and
// `ComposerDock` carries the same geometry plus the dock's own insets. This
// panel writes no width at all. The two blocks are SIBLINGS inside the panel's
// `position: relative` box: nesting them would apply the insets and the shift
// twice (each block brings its own padding + maxWidth + translate), so the
// composer would sit narrower than the rows it belongs to.
//
// The geometry is keyed on the PANEL's width, not the window's: the sidebar and
// the status rail take their share out of the conversation's budget, so at a
// 1280px window with the 264px sidebar the column is in the 864–1280 band
// (896 cap, 48px insets) rather than the wide one. The panel measures itself
// (`onLayout`) and hands that number to both blocks.
//
// What is deliberately NOT here (change keel-harness-shell):
//   · the header — title, model chip, run state and the back affordance are the
//     shell's (`WorkspaceShell`'s h-12 header slot). This panel draws no second
//     header, and the gradient mask that softened content scrolling under it is
//     gone with it;
//   · the delete confirmation — `DeleteDialog` is a root-level `AlertDialog`
//     covering the app; the panel paints no second overlay of its own;
//   · connection state — a kit `Badge` on a status strip, never a hand-mixed
//     alpha bar.
//
// NSH components run once and update through fine-grained bindings: every
// signal/prop is read inside JSX or an `<If>`, never copied into a local at the
// top of a component (that would freeze the value).
import { cleanup, effect, List, type ListRef, platform, signal, Text, untracked, View, windowSize } from "nsh";
import type { ConversationView, DecisionView, QueueItemView, ScrollState, TurnView } from "../data/contract";
import { store } from "../data/store";
import { Badge, color, ComposerDock, ConversationColumn, row, Skeleton, space, typeScale } from "../ui/kit";
import { ApprovalCard, EmptyConversation, GuideWaitingRow, OfflineBanner, TurnErrorBanner, TurnRow } from "./messages";
import { Composer, prefillComposer } from "./composer";
import { ImageSurface } from "../surfaces/image";
import { RealtimeSurface } from "../surfaces/realtime";
import { SpeechSurface } from "../surfaces/speech";
import { UnsupportedSurface } from "../surfaces/unsupported";
import { resolveSelectionSurface, type SurfaceDescriptor } from "../surfaces/registry";

interface Row {
  kind: "turn" | "decision" | "guide";
  id: string;
  turn?: TurnView;
  decision?: DecisionView;
  guide?: QueueItemView;
}

const submitting = signal<Record<string, boolean>>({});
const now = signal(Date.now());

// The reader's scroll intent for the OPEN session (C5). The value is replaced
// from the store's per-session memory whenever a session opens, and every
// scroll writes it back — so the intent and the reading position can never
// leak across sessions.
const scroll = signal<ScrollState>({ intent: "following", offset: 0 });
/** Whether the content is taller than the viewport: the "回到底部" gate. */
const overflowing = signal(false);
/** The session whose saved position has already been handed to the list. */
const scrollRestored = signal<string | null>(null);
/** A detached session's restore in flight: while the list lays its rows out it
 * may report offsets of its own; they are not the reader's intent, and the
 * saved offset is applied again until it sticks. */
let pendingRestore: { sessionId: string; offset: number; tries: number } | null = null;

const rowsOf = (view: ConversationView | null): Row[] => {
  if (!view) return [];
  return [
    ...view.turns.map((turn): Row => ({ kind: "turn", id: turn.turnId, turn })),
    ...view.decisions.map((decision): Row => ({ kind: "decision", id: `decision-${decision.decisionId}`, decision })),
    // Guides still waiting for a boundary show at the stream's tail (C12):
    // the place they will be injected.
    ...view.queue
      .filter((item) => item.delivery === "guide")
      .map((item): Row => ({ kind: "guide", id: `guide-${item.requestId}`, guide: item })),
  ];
};

/** The rows' cells: content only. Their width, insets and shift are the
 * column's (`ConversationColumn`), never a `maxWidth` of their own. */
function RowCell(props: { row: () => Row }) {
  const isLastRunning = () => {
    const turns = store.conversation()?.turns ?? [];
    const last = turns[turns.length - 1];
    const turn = props.row().turn;
    return !!turn && !!last && turn.turnId === last.turnId && last.status === "running";
  };
  const isLastTurn = () => {
    const turns = store.conversation()?.turns ?? [];
    const last = turns[turns.length - 1];
    const turn = props.row().turn;
    return !!turn && !!last && turn.turnId === last.turnId;
  };
  const isActiveDecision = (decisionId: string) => {
    const decisions = store.conversation()?.decisions ?? [];
    for (let index = decisions.length - 1; index >= 0; index -= 1) {
      if (decisions[index].status === "open") return decisions[index].decisionId === decisionId;
    }
    return false;
  };
  return (
    <View style={{ width: "100%" }}>
      <If when={props.row().kind === "turn"}>
        <TurnRow
          turn={() => props.row().turn!}
          attachmentsFor={(requestId) => store.attachmentsFor(requestId)}
          partialText={() => store.partial()}
          isLastRunning={isLastRunning}
          isLast={isLastTurn}
          waiting={() => store.waiting()}
          now={() => now()}
        />
      </If>
      <If when={props.row().kind === "decision"}>
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
      </If>
      <If when={props.row().kind === "guide"}>
        <View style={{ paddingVertical: space.sm }}>
          <GuideWaitingRow item={props.row().guide!} stopping={!!store.conversation()?.stopping} />
        </View>
      </If>
    </View>
  );
}

/** The snapshot has not arrived yet: the shape of the answer, not a claim about
 * it (the kit's `Skeleton`, which announces "加载中" to screen readers and
 * draws muted cells in the terminal). */
function LoadingRows() {
  return (
    <View style={{ flexDirection: "column", gap: space["2xl"], paddingVertical: space["3xl"] }}>
      <Skeleton lines={3} />
      <Skeleton lines={2} />
    </View>
  );
}

// The panel keeps the two callbacks the old in-panel header used (`onBack`,
// `onOpenList`): the signature is shared with the shell, which owns those
// affordances now, and a shell that still hands them down must keep compiling.
function LlmConversation(props: { wide: boolean; onOpenList?: () => void; onBack?: () => void }) {
  let list: ListRef | undefined;
  // The conversation's own width: what the kit's blocks are keyed on. 0 until
  // the first layout lands — the window is the stand-in for that one frame.
  const paneWidthPx = signal(0);
  const paneWidth = () => (paneWidthPx() > 0 ? paneWidthPx() : windowSize().width);
  const rows = () => rowsOf(store.conversation());
  // The centered (draft) layout, defined once and nowhere else: nothing to read
  // yet — no open session, or a session whose snapshot is in and has no rows.
  // The reference's `centeredEmptyLayout = centerEmptyStateWithDock &&
  // renderUnits.length === 0` (ConversationTimeline.tsx:526): a brand-new
  // session gets the 672px column, not the wide one.
  const draft = () => !store.activeId() || (!!store.conversation() && rows().length === 0);
  // A draft has no snapshot to wait for: the greeting is the honest state, not
  // a loading that cannot finish.
  const loading = () => !draft() && !store.conversation();
  const ready = () => !loading();
  const empty = () => draft() || rows().length === 0;
  const jumpToEnd = () => {
    const id = store.activeId();
    scroll.set({ intent: "following", offset: 0 });
    if (id) store.saveScrollState(id, { intent: "following", offset: 0 });
    list?.scrollToEnd({ animated: true });
  };
  // Take the session's remembered scroll intent as soon as its snapshot is on
  // screen; a detached reader keeps their offset, and the list holds it while
  // content grows (maintainVisibleContentPosition). Nothing here is written
  // back to the ledger — this is the reader's place in the panel, not a fact.
  effect(() => {
    const id = store.activeId();
    const view = store.conversation();
    if (!id) {
      if (scrollRestored() !== null) {
        scrollRestored.set(null);
        pendingRestore = null;
        scroll.set({ intent: "following", offset: 0 });
        overflowing.set(false);
      }
      return;
    }
    // Wait for the snapshot to belong to this session: restoring into the
    // previous session's rows would land on the wrong content.
    if (!view || view.sessionId !== id) return;
    if (untracked(scrollRestored) === id) return;
    scrollRestored.set(id);
    const saved = store.scrollState(id) ?? { intent: "following" as const, offset: 0 };
    scroll.set(saved);
    overflowing.set(false);
    pendingRestore = null;
    // Put a detached reader back exactly where they were (offset 0 included).
    // The offset is applied here and again by every scroll report until it
    // sticks — a list still measuring its new rows must not be mistaken for a
    // reader who scrolled, nor may it drag them back to the bottom.
    if (saved.intent !== "detached") return;
    pendingRestore = { sessionId: id, offset: saved.offset, tries: 0 };
    const timers = [32, 240].map((delay) =>
      setTimeout(() => list?.scrollToOffset({ offset: saved.offset, animated: false }), delay),
    );
    return () => {
      for (const timer of timers) clearTimeout(timer);
    };
  });
  const timer = setInterval(() => {
    // The panel's clock only ticks while something on screen is moving: a run
    // (work-segment timers) or an open question (the answer countdown).
    if (store.busy() || store.waiting()) now.set(Date.now());
  }, 1000);
  cleanup(() => clearInterval(timer));

  return (
    <View
      testID="conversation-panel"
      onLayout={(event) => paneWidthPx.set(event.bounds.size.width)}
      style={{
        flex: 1,
        minWidth: 0,
        // The containing block for the absolute layers a kit block may draw
        // (the status rail, the dock's back-to-bottom button), and the sibling
        // of both columns' geometry.
        position: "relative",
        backgroundColor: color.background,
      }}
    >
      {/* Connection state is a kit Badge, not a hand-mixed alpha bar: one short
          status token, and the sentence that says what happens next. */}
      <If when={store.connection().status === "reconnecting"}>
        <View style={[row(), { gap: space.md, paddingHorizontal: space.xl, paddingVertical: space.sm }]}>
          <Badge variant="warning" icon="alert" label="重连中" />
          <Text style={{ flex: 1, numberOfLines: 1, fontSize: typeScale.sm, color: color.mutedForeground }}>
            连接中断，正在续接缺失的事件；缺口会补发且不重复。
          </Text>
        </View>
      </If>
      <ConversationColumn
        draft={draft()}
        // No inline status rail in this pane: the shell's status surface is its
        // own side pane / a chip, so the column must stay centred instead of
        // shifting 168px for a rail that is not there (the reference keys the
        // offset off the panel's layout, conversationLayout.ts:43-50).
        rail={false}
        windowWidthPx={paneWidth()}
        testID="conversation-column"
        // `flexDirection: "row"`: the height hand-off. The kit's block owns the
        // width, but its inner column is content-sized, so the outer edge is
        // laid out as a row and the inner column stretches (flexbox's default
        // `alignItems: stretch`) to the panel's height — which is what lets the
        // list scroll inside its own viewport instead of growing past it. No
        // width, height or inset is written here.
        style={{ flex: 1, minHeight: 0, flexDirection: "row" }}
      >
        {/* Three states, one surface: the answer is loading, there is nothing
            to show yet (the greeting), or there are rows. The list keeps its
            own `<List>` element (the rows are recycled through it), so it is a
            plain sibling `<If>` rather than a branch of an `else` prop. */}
        <If when={loading()}>
          <LoadingRows />
        </If>
        <If when={ready() && empty()}>
          <EmptyConversation onSuggestion={(text) => prefillComposer(text)} />
        </If>
        <If when={ready() && !empty()}>
          <List
            ref={list}
            testID="conversation-list"
            each={rows()}
            keyExtractor={(row) => row.id}
            estimatedItemSize={160}
            alignment="bottom"
            maintainVisibleContentPosition={{ autoscrollToBottomThreshold: 48 }}
            scrollbar={platform.idiom === "terminal" ? false : undefined}
            style={{ flex: 1 }}
            contentStyle={{ paddingTop: space.xl, paddingBottom: space.lg }}
            onScroll={(event) => {
              const id = store.activeId();
              // A restore in flight is not a reader's intent: re-apply the
              // remembered offset (bounded) instead of recording.
              if (id && pendingRestore && pendingRestore.sessionId === id) {
                if (Math.abs(event.offset.y - pendingRestore.offset) > 4 && pendingRestore.tries < 6) {
                  pendingRestore.tries += 1;
                  list?.scrollToOffset({ offset: pendingRestore.offset, animated: false });
                  return;
                }
                pendingRestore = null;
              }
              const atEnd = event.offset.y + event.size.height >= event.contentSize.height - 48;
              const next: ScrollState = { intent: atEnd ? "following" : "detached", offset: event.offset.y };
              scroll.set(next);
              overflowing.set(event.contentSize.height > event.size.height + 8);
              if (id) store.saveScrollState(id, next);
            }}
          >
            {(row) => <RowCell row={row} />}
          </List>
        </If>
      </ConversationColumn>
      <ComposerDock
        draft={draft()}
        rail={false}
        windowWidthPx={paneWidth()}
        backToBottomVisible={scroll().intent === "detached" && overflowing()}
        onBackToBottom={jumpToEnd}
        backToBottomLabel="回到底部"
        testID="composer-dock"
      >
        {/* The failure's reason sits directly above the input it belongs to
            (C5): same column, same layer, closable and retryable. The offline
            face rides the same slot — the reader learns the service is gone
            here, before a send can vanish. */}
        <OfflineBanner />
        <TurnErrorBanner compact={!props.wide} />
        <Composer view={store.conversation()} compact={!props.wide} />
      </ComposerDock>
    </View>
  );
}

/** The session's surface: the provider kind decides the composer/timeline pair
 * (`keel-session-surfaces-v1`). LLM is the regression baseline; image,
 * realtime and speech mount their own panels. A real selection resolves
 * through the registry, so the standard-mode model chip follows the same rule
 * once the registry projects a provider type.
 *
 * The dispatch is exhaustive on purpose. `video` and `generic` have no shipped
 * panel, and they mount the honest placeholder — falling through to the chat
 * shell (the old behaviour) presented a video or unknown-provider session as a
 * text conversation with an empty history. */
export function Conversation(props: { wide: boolean; onOpenList?: () => void; onBack?: () => void }) {
  const resolved = () =>
    resolveSelectionSurface(
      store.sessionSelection() ?? store.tenantSelection(),
      store.models(),
      store.providers(),
    );
  const kind = () => resolved().kind;
  const descriptor = () => (kind() === "llm" ? null : resolved().descriptor) as SurfaceDescriptor | null;
  const openSettings = () => {
    store.openSettings();
    if (store.mode() === "http" && !store.settings() && !store.settingsLoading()) void store.loadModelsSettings();
  };
  return (
    <If
      when={kind() === "image"}
      else={
        <If
          when={kind() === "realtime"}
          else={
            <If
              when={kind() === "speech"}
              else={
                <If
                  when={kind() === "video" || kind() === "generic"}
                  else={<LlmConversation wide={props.wide} onOpenList={props.onOpenList} onBack={props.onBack} />}
                >
                  <UnsupportedSurface
                    wide={props.wide}
                    kind={kind()}
                    descriptor={descriptor()!}
                    onOpenSettings={openSettings}
                  />
                </If>
              }
            >
              <SpeechSurface wide={props.wide} mode={descriptor()?.speechMode ?? "stt"} />
            </If>
          }
        >
          <RealtimeSurface wide={props.wide} />
        </If>
      }
    >
      <ImageSurface wide={props.wide} />
    </If>
  );
}
