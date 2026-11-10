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
// The editing field is the native `nsh/base` Input: the kit's `Input` /
// `Textarea` carry their own field chrome and expose no content-size callback,
// while this field must be chrome-less (the shell's p-3 is the box) and
// auto-grow. Everything it draws with is still a token.
import { effect, Picker, platform, signal, Text, type InputRef, untracked, View } from "nsh";
import { Input } from "nsh/base";
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
  IconButton,
  Kbd,
  lineHeight,
  space,
  Tab,
  Tabs,
  Tooltip,
  typeScale,
} from "../ui/kit";

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
  effect(() => {
    if (prefillTick() === 0) return;
    untracked(() => input?.focus());
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
  // shortcut hint stays reachable through the send button's tooltip.
  const showHint = () => !props.compact;
  const fieldHeight = () => Math.min(Math.max(inputHeight(), oneLine()), fieldCeiling());

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
    const text = draft();
    const pending = attachments();
    draft.set("");
    attachments.set([]);
    inputHeight.set(0);
    sending.set(true);
    const ok = await store.send(text, effectiveMode(), pending);
    sending.set(false);
    if (!ok) {
      // Nothing was committed: give the writer their words back (attachment
      // previews included). The reason is in the turn-error banner.
      draft.set(text);
      attachments.set(pending);
    }
  };

  const stop = () => {
    if (stopping()) return;
    void store.stop();
  };

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
    <View style={{ gap: space.sm }}>
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
      <ComposerSurface testID="composer-surface">
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
            // The shell owns the box: no border, no fill, no padding of its own
            // (the shell's p-3 is the only padding around the text).
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
