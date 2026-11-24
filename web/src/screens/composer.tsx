// The composer: queue/guide mode, attachments, send/stop. It only calls the
// store; the delivery decision (a mode applies while a run is active, an idle
// session always sends queue) is made in one place.
//
// Chrome belongs to the kit, not to this file. The shell is `ComposerSurface`
// (`rounded-2xl` 16px radius, 1px border, `p-3`, focus-within brand ring —
// kit/layout/composer-dock.tsx:165-213), the dock draws its own 16px insets and
// the conversation column's caps, so nothing here writes a column width. The
// toolbar's faces are kit Buttons / Tabs / Badges and every value below is a
// token read (`web/theme.json` through kit/tokens.ts).
//
// The web target edits with `nsh/base` RichEditor (Markdown shortcuts, HTML
// paste, formatted text); the terminal and native/phone hosts keep the plain
// Input. Both paths are chrome-less (the shell's p-3 is the box) and auto-grow,
// and everything either draws with is a token.
import { cleanup, effect, host, Picker, platform, signal, Text, type InputRef, untracked, View } from "nsh";
import { createRichEditor, Input, markdown, RichEditor, type RichEditorSession, starterKit } from "nsh/base";
import type { ConversationView, Delivery, QueueItemView } from "../data/contract";
import type { PendingAttachment } from "../data/store";
import { store } from "../data/store";
import {
  alpha,
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  color,
  ComposerSurface,
  font,
  IconButton,
  Kbd,
  lineHeight,
  space,
  Tab,
  Tabs,
  Tooltip,
  typeScale,
  weight,
} from "../ui/kit";

import {
  buildLearnInstruction,
  completionTrigger,
  CompletionMenu,
  insertCompletion,
  parseLearnCommand,
} from "./composer-completion";

const draft = signal("");
const mode = signal<Delivery>("queue");
const attachments = signal<PendingAttachment[]>([]);
/** The field's content height in px, as the field itself reports it (0 before
 * the first measure; the floor below covers that first frame). */
const inputHeight = signal(0);
/** True while a submit is in flight: attachment uploads are not instant. */
const sending = signal(false);
/** Bumped when text is placed into the draft from outside (the empty-state
 * suggestion chips): the input takes the caret, nothing is sent (C8). */
const prefillTick = signal(0);
/** The composer's own measured width. The toolbar's hint is a convenience; on
 * a narrow column (the side pane open at 1440 leaves the column ~290px) it
 * pushed the send button outside the composer shell. Measured on the root so
 * the hint drops before the primary control does. */
const composerWidth = signal(0);
/** The editor revision at which a picked completion should stay closed. The
 * editor's Markdown normalisation drops the trailing space the plain input
 * used to end a token, so the revision (not the text) is the close signal. */
const completionDismissedAt = signal(0);
/** True while a draft change is being written into the editor: the editor's
 * change callback must not echo it back. */
let applyingRichDraft = false;

/** The rich editor is made once, lazily, and only for drawn hosts. */
let richSession: RichEditorSession | null = null;
/** A fresh empty document (setContent("") can leave a zero-width block). */
const EMPTY_DOC = { type: "doc", content: [{ type: "paragraph" }] };
function richEditor(): RichEditorSession | null {
  // This slice ships the web target; the terminal and native/phone hosts keep
  // the plain Input (its Enter/submit behavior is already correct there).
  if (platform.os !== "web" || platform.idiom === "terminal") return null;
  if (!richSession) richSession = createRichEditor({ plugins: [starterKit(), markdown()] });
  return richSession;
}

export const composerDraft = draft;

/** Fill the composer with text — never send it. */
export function prefillComposer(text: string) {
  draft.set(text);
  prefillTick.update((value) => value + 1);
}

/** One relaxed line of the field's own type, plus a hair of cushion so a
 * descender never clips: the field's floor. */
const oneLine = () => Math.round(typeScale.base * lineHeight.relaxed) + space.xs;
/** Five lines: past that the field scrolls instead of eating the column. */
const fieldCeiling = () => oneLine() * 5;

/** A chip is one line: a longer name is ellipsised, and the full name stays
 * reachable through the chip's and the remove button's accessibility labels. */
const CHIP_NAME_MAX = 28;
const chipName = (name: string) =>
  name.length > CHIP_NAME_MAX ? `${name.slice(0, CHIP_NAME_MAX - 1)}…` : name;

/** The pending sends, above the shell. Guide deliveries are not here: they show
 * at the stream's tail (C12), where they will be injected. */
function QueueCard(props: { queue: QueueItemView[] }) {
  const items = () => props.queue.filter((item) => item.delivery !== "guide");
  return (
    <If when={items().length > 0}>
      <Card size="sm" testID="composer-queue" accessibilityLabel="发送队列">
        <CardHeader
          title="队列"
          description="当前任务结束后按顺序发送"
          action={
            <Badge
              variant="secondary"
              label={`${items().length} 条`}
              accessibilityLabel={`队列 ${items().length} 条`}
              testID="composer-queue-count"
            />
          }
        />
        <CardContent>
          {items().map((item, index) => (
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
              <Text
                style={{
                  fontSize: typeScale.xs,
                  color: color.mutedForeground,
                  fontVariant: ["tabular-nums"],
                }}
              >
                {index + 1}
              </Text>
              <Text style={{ flex: 1, numberOfLines: 1, fontSize: typeScale.sm, color: color.foreground }}>
                {item.text}
              </Text>
              <If when={!!item.immediate}>
                <Badge variant="info" label="立即" accessibilityLabel="立即发送" />
              </If>
            </View>
          ))}
        </CardContent>
      </Card>
    </If>
  );
}

/** The submit control's hint. The reference (`ChatPromptEditor.tsx`) attaches
 * the shortcut to the send button instead of writing it into the toolbar: the
 * keycaps show on hover/focus, and on a phone (where Return is a newline) there
 * is nothing to show. */
const sendHint = () => (
  <Tooltip accessibilityLabel="Enter 发送，Shift+Enter 换行">
    <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
      <Kbd keys={["Enter"]} />
      <Text style={{ fontSize: typeScale.sm, color: color.popoverForeground }}>发送</Text>
      <Kbd keys={["Shift", "Enter"]} />
      <Text style={{ fontSize: typeScale.sm, color: color.popoverForeground }}>换行</Text>
    </View>
  </Tooltip>
);

export function Composer(props: { view: ConversationView | null; compact?: boolean }) {
  let input: InputRef | undefined;
  const rich = richEditor();
  // The session listener and the view's onChange both land here. The web host
  // turns Enter into `keel.send` (host.js owns that keydown: the native rich
  // editor consumes Enter itself); the draft is the Markdown text.
  const syncRichDraft = () => {
    if (!rich || applyingRichDraft) return;
    draft.set(rich.getMarkdown().replace(/\u200b/g, "").replace(/\n$/, ""));
  };
  effect(() => {
    if (prefillTick() === 0) return;
    untracked(() => {
      if (rich) rich.focus();
      else input?.focus();
    });
  });
  // The session's own listener is the source of truth for editor-origin
  // changes; the view's onChange is not the only path a programmatic change
  // can take (and a test host may deliver it a turn later).
  effect(() => {
    if (!rich) return;
    const off = rich.on("update", syncRichDraft);
    cleanup(off);
  });
  // External draft writes (empty-state prefills, a completion, a failed send's
  // restore, the clear after a send) reach the rich editor here; editor-origin
  // changes already match and do nothing.
  effect(() => {
    const text = draft();
    if (!rich) return;
    untracked(() => {
      if (applyingRichDraft) return;
      const current = rich.getMarkdown().replace(/\n+$/, "");
      if (current === text.replace(/\n+$/, "")) return;
      applyingRichDraft = true;
      rich.setContent(text, "markdown");
      applyingRichDraft = false;
    });
  });
  const view = () => props.view;
  const busy = () => !!view()?.busy;
  const stopping = () => !!view()?.stopping;
  // An idle session has no run to steer: queue is the effective mode by
  // definition, so the mode control shows 队列 and 引导 is disabled.
  const effectiveMode = (): Delivery => (busy() ? mode() : "queue");
  const canSend = () => !!draft().trim() || attachments().length > 0;
  const submitOnEnter = () => platform.idiom !== "phone";
  // The shell reports its single-column layout (≤767px) as `compact`: with a
  // toolbar this dense, the state hint is the first thing it drops. The
  // shortcut hint stays reachable through the send button's tooltip. The
  // column can also be narrow while the window is not (the right status pane
  // is open), so the measured width drops the hint too.
  const showHint = () => !props.compact && (composerWidth() === 0 || composerWidth() >= 460);
  const fieldHeight = () => Math.min(Math.max(inputHeight(), oneLine()), fieldCeiling());

  // `$` opens the grouped menu once per session: the first activation loads
  // skills/files/tasks, later keystrokes reuse the answer. The load leaves the
  // effect's turn (a signal write inside the first render is not allowed).
  effect(() => {
    if (!completionTrigger(draft())) return;
    const sessionId = store.activeId();
    if (!sessionId) return;
    if (store.completionSession() === sessionId) return;
    const timer = setTimeout(() => void store.loadCompletion(sessionId), 0);
    cleanup(() => clearTimeout(timer));
  });

  /** Replace the active `$token` with the chosen row; never send it. */
  const pickCompletion = (insert: string) => {
    const active = completionTrigger(draft());
    if (!active) return;
    const next = insertCompletion(draft(), active.start, insert);
    draft.set(next);
    untracked(() => {
      if (rich) {
        applyingRichDraft = true;
        rich.setContent(next, "markdown");
        applyingRichDraft = false;
        completionDismissedAt.set(rich.revision + 1);
        rich.focus();
      } else {
        input?.focus();
      }
    });
  };

  const addPicked = async () => {
    try {
      const picked = await Picker.pickImages({ maxDimension: 2048, quality: 0.85 });
      if (!picked.length) return;
      const next = [...attachments()];
      for (const item of picked) {
        const file = item.file;
        next.push({
          name: file.name || "image.png",
          mime: file.type || "image/png",
          size: file.size,
          preview: URL.createObjectURL(file),
          file,
        });
      }
      attachments.set(next);
    } catch (error) {
      store.flash({
        tone: "warn",
        text: `无法打开附件选择器：${error instanceof Error ? error.message : String(error)}`,
      });
    }
  };

  const removeAttachment = (index: number) => {
    const next = [...attachments()];
    const [removed] = next.splice(index, 1);
    if (removed?.preview) {
      try {
        URL.revokeObjectURL(removed.preview);
      } catch {
        /* already gone */
      }
    }
    attachments.set(next);
  };

  const send = async () => {
    if (!canSend() || stopping() || sending()) return;
    const raw = draft();
    // `/learn` is one message that asks the agent for a skills.manage create;
    // the tool result is the only success signal, so the composer says what it
    // sent, never "learned".
    const learn = parseLearnCommand(raw);
    const text = learn ? buildLearnInstruction(learn.prompt) : raw;
    const pending = attachments();
    draft.set("");
    if (rich) {
      applyingRichDraft = true;
      rich.setContent(EMPTY_DOC, "json");
      applyingRichDraft = false;
      rich.focus();
    }
    attachments.set([]);
    inputHeight.set(0);
    sending.set(true);
    if (learn) {
      store.flash({ tone: "info", text: "已发送沉淀请求；以 Agent 的 skills 工具结果为准。" });
    }
    const ok = await store.send(text, effectiveMode(), pending);
    sending.set(false);
    if (!ok) {
      // Nothing was committed: give the writer their words back (attachment
      // previews included). The reason is in the turn-error banner.
      draft.set(raw);
      if (rich) {
        applyingRichDraft = true;
        rich.setContent(raw, "markdown");
        applyingRichDraft = false;
      }
      attachments.set(pending);
    }
  };

  const stop = () => {
    if (stopping()) return;
    void store.stop();
  };

  // The web host turns Enter in this composer into `keel.send` (host.js): the
  // rich editor consumes Enter internally, so a DOM keydown is the reliable
  // interception point. Other hosts use the send button.
  effect(() => {
    if (!rich || !host.available()) return;
    const off = host.on("keel.send", () => {
      if (!stopping()) void send();
    });
    cleanup(off);
  });

  const placeholder = () =>
    stopping()
      ? "停止中，等待任务结束…"
      : busy()
        ? mode() === "guide"
          ? "输入引导，注入当前任务…"
          : "输入消息，当前任务后发送…"
        : "给 Keel 发消息，空闲时直接开始…";
  const hint = () =>
    stopping()
      ? "停止中，引导将转为队列"
      : busy()
        ? mode() === "guide"
          ? "引导在工具轮边界注入，无法注入时转队列"
          : "当前任务结束后按顺序发送"
        : "空闲发送直接开始";

  return (
    <View
      style={{ gap: space.sm }}
      onLayout={(event) => {
        const next = event.bounds.size.width;
        if (Math.abs(next - composerWidth()) < 0.5) return;
        composerWidth.set(next);
      }}
    >
      <QueueCard queue={view()?.queue ?? []} />
      <If when={attachments().length > 0}>
        <View
          style={{ flexDirection: "row", flexWrap: "wrap", alignItems: "center", gap: space.sm }}
          accessibilityLabel="已添加附件"
        >
          {attachments().map((item, index) => (
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.xxs }}>
              <Badge
                variant="secondary"
                icon="image"
                label={chipName(item.name)}
                accessibilityLabel={`附件 ${item.name}`}
              />
              <IconButton
                icon="x"
                label={`移除附件 ${item.name}`}
                onPress={() => removeAttachment(index)}
              />
            </View>
          ))}
        </View>
      </If>
      <If
        when={
          !!completionTrigger(draft()) &&
          (!rich || rich.revision > completionDismissedAt())
        }
      >
        <CompletionMenu
          query={completionTrigger(draft())?.query ?? ""}
          sessionId={store.activeId()}
          onPick={pickCompletion}
        />
      </If>
      <ComposerSurface testID="composer-surface">
        <If
          when={!!rich}
          else={
            <Input
              ref={(node) => {
                input = node;
              }}
              testID="composer-input"
              accessibilityLabel="输入消息"
              value={draft()}
              onChange={({ value }) => draft.set(value)}
              multiline
              submitBehavior={submitOnEnter() ? "submit" : "newline"}
              onSubmit={() => {
                if (!stopping()) void send();
              }}
              placeholder={placeholder()}
              onContentSizeChange={({ size }) => inputHeight.set(size.height)}
              style={{
                // The shell owns the box: no border, no fill, no padding of its
                // own (the shell's p-3 is the only padding around the text).
                height: fieldHeight(),
                borderWidth: 0,
                backgroundColor: "transparent",
                paddingHorizontal: space.none,
                paddingVertical: space.none,
                fontSize: typeScale.base,
                lineHeight: Math.round(typeScale.base * lineHeight.relaxed),
                color: color.foreground,
                placeholderColor: color.mutedForeground,
                caretColor: color.brand,
                selectionColor: alpha(color.brand, 0.3),
              }}
            />
          }
        >
          <View style={{ position: "relative" }}>
            <RichEditor
              editor={rich!}
              testID="composer-input"
              accessibilityLabel="输入消息"
              placeholder={null}
            onChange={syncRichDraft}
            onContentSizeChange={({ contentSize }) => inputHeight.set(contentSize.height)}
            styles={{
              root: {
                height: fieldHeight(),
                backgroundColor: "transparent",
              },
              content: {
                paddingHorizontal: space.none,
                paddingVertical: space.none,
                fontSize: typeScale.base,
                lineHeight: Math.round(typeScale.base * lineHeight.relaxed),
                color: color.foreground,
              },
              placeholder: { color: color.mutedForeground },
              caret: { backgroundColor: color.brand },
              selection: { backgroundColor: alpha(color.brand, 0.3) },
              nodes: {
                paragraph: {
                  fontSize: typeScale.base,
                  lineHeight: Math.round(typeScale.base * lineHeight.relaxed),
                },
                heading: { fontWeight: weight.semibold },
                "heading[level=1]": {
                  fontSize: typeScale["2xl"],
                  lineHeight: Math.round(typeScale["2xl"] * lineHeight.tight),
                  fontWeight: weight.semibold,
                },
                "heading[level=2]": {
                  fontSize: typeScale.section,
                  lineHeight: Math.round(typeScale.section * lineHeight.tight),
                  fontWeight: weight.semibold,
                },
                "heading[level=3]": {
                  fontSize: typeScale.xl,
                  lineHeight: Math.round(typeScale.xl * lineHeight.snug),
                  fontWeight: weight.semibold,
                },
                bulletList: { paddingLeft: space.lg },
                orderedList: { paddingLeft: space.lg },
                listItem: { paddingLeft: space.none },
                blockquote: {
                  borderLeftWidth: 2,
                  borderLeftColor: color.border,
                  paddingLeft: space.sm,
                  color: color.mutedForeground,
                },
                codeBlock: {
                  fontFamily: font.mono,
                  fontSize: typeScale.caption,
                  lineHeight: Math.round(typeScale.caption * lineHeight.relaxed),
                  backgroundColor: alpha(color.muted, 0.35),
                },
              },
              listMarker: { color: color.foreground, fontSize: typeScale.base },
              marks: {
                bold: { fontWeight: weight.bold },
                italic: { fontStyle: "italic" },
                strike: { textDecorationLine: "line-through" },
                code: {
                  fontFamily: font.mono,
                  fontSize: typeScale.caption,
                  backgroundColor: alpha(color.muted, 0.35),
                },
                link: { color: color.brand, textDecorationLine: "underline" },
              },
            }}
          />
            {/* The editor's own placeholder disappears after a programmatic
                clear in some hosts; this overlay follows the same empty state. */}
            <If when={!draft().trim()}>
              <Text
                testID="composer-placeholder"
                style={{
                  position: "absolute",
                  left: 0,
                  top: 0,
                  right: 0,
                  fontSize: typeScale.base,
                  lineHeight: Math.round(typeScale.base * lineHeight.relaxed),
                  color: color.mutedForeground,
                  pointerEvents: "none",
                }}
              >
                {placeholder()}
              </Text>
            </If>
          </View>
        </If>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
          <IconButton
            testID="composer-attach"
            icon="paperclip"
            label="添加附件"
            onPress={() => void addPicked()}
          />
          <Tabs accessibilityLabel="投递方式" testID="composer-mode">
            <Tab
              testID="composer-mode-queue"
              label="队列"
              icon="clock"
              selected={effectiveMode() === "queue"}
              onPress={() => mode.set("queue")}
            />
            <Tab
              testID="composer-mode-guide"
              label="引导"
              icon="external"
              selected={effectiveMode() === "guide"}
              disabled={!busy()}
              onPress={() => mode.set("guide")}
            />
          </Tabs>
          <View style={{ flex: 1 }} />
          <If when={showHint()}>
            <Text
              testID="composer-hint"
              style={{ numberOfLines: 1, fontSize: typeScale.sm, color: color.mutedForeground }}
            >
              {hint()}
            </Text>
          </If>
          <If when={busy()}>
            <Button
              testID="composer-stop"
              variant="destructive"
              size="sm"
              icon="stop"
              label={stopping() ? "停止中" : "停止"}
              loading={stopping()}
              disabled={stopping()}
              accessibilityLabel={stopping() ? "正在停止" : "停止当前任务"}
              onPress={() => stop()}
            />
          </If>
          <Button
            testID="composer-send"
            variant="default"
            size="icon"
            icon="arrow-up"
            loading={sending()}
            disabled={!canSend() || stopping() || sending()}
            accessibilityLabel="发送"
            onPress={() => void send()}
            tooltip={submitOnEnter() ? sendHint() : undefined}
          />
        </View>
      </ComposerSurface>
    </View>
  );
}
