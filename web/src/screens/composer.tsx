// The composer: queue/guide mode, attachments, send/stop. It only calls the
// store; the delivery decision (a mode applies while a run is active, an idle
// session always sends queue) is made in one place.
import { effect, Picker, platform, signal, Text, type InputRef, untracked, View, windowSize } from "nsh";
import { Input } from "nsh/base";
import type { ConversationView, Delivery } from "../data/contract";
import type { PendingAttachment } from "../data/store";
import { store } from "../data/store";
import { Icon } from "../ui/icons";
import { IconButton, Press, Segmented, Spinner } from "../ui/primitives";
import { alpha, colors, glass, radius, space } from "../ui/theme";
import { QueuePanel, GuideWaitingBanner } from "./messages";

const draft = signal("");
const mode = signal<Delivery>("queue");
const attachments = signal<PendingAttachment[]>([]);
const inputHeight = signal(20);
/** Bumped when text is placed into the draft from outside (the empty-state
 * suggestion chips): the input takes the caret, nothing is sent (C8). */
const prefillTick = signal(0);

export const composerDraft = draft;

/** Fill the composer with text — never send it. */
export function prefillComposer(text: string) {
  draft.set(text);
  prefillTick.update((value) => value + 1);
}

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
  // definition, so the segmented control shows 队列 and 引导 is disabled.
  const effectiveMode = (): Delivery => (busy() ? mode() : "queue");
  // Guides waiting for a boundary: still queued (the server has not injected
  // them yet) plus injected-but-unconsumed ones of the running turn. A guide
  // that was downgraded to queue carries delivery=queue, so it is not counted.
  const guideCount = () => {
    const queued = (view()?.queue ?? []).filter((item) => item.delivery === "guide").length;
    const turns = view()?.turns ?? [];
    const turn = turns[turns.length - 1];
    const injected =
      turn && turn.status === "running"
        ? turn.visibleInputs.filter((input) => input.delivery === "guide" && !input.consumed).length
        : 0;
    return queued + injected;
  };
  const canSend = () => !!draft().trim() || attachments().length > 0;

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
    if (!canSend() || stopping()) return;
    const text = draft();
    const pending = attachments();
    draft.set("");
    attachments.set([]);
    inputHeight.set(20);
    await store.send(text, effectiveMode(), pending);
  };

  const stop = () => {
    if (stopping()) return;
    void store.stop();
  };

  const submitOnEnter = platform.idiom !== "phone";
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
        : "空闲发送直接开始 · Enter 发送 · Shift+Enter 换行";
  const compact = !!props.compact;

  return (
    <View
      style={{
        width: "100%",
        maxWidth: 880,
        alignSelf: "center",
        gap: space.sm,
        paddingHorizontal: compact ? 10 : 16,
      }}
    >
      <QueuePanel queue={view()?.queue ?? []} />
      <GuideWaitingBanner visible={guideCount() > 0} count={guideCount()} stopping={stopping()} />
      {attachments().length ? (
        <View style={{ flexDirection: "row", flexWrap: "wrap", gap: space.sm }}>
          {attachments().map((item, index) => (
            <View
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: 6,
                height: 30,
                paddingLeft: 8,
                paddingRight: 2,
                borderRadius: radius.control,
                ...glass.control,
              }}
            >
              <Icon name="image" size={12} tint={colors.mutedForeground} />
              <Text style={{ numberOfLines: 1, maxWidth: 180, fontSize: 11.5, color: colors.foreground }}>
                {item.name}
              </Text>
              <IconButton
                icon="x"
                label="移除附件"
                size={22}
                iconSize={11}
                hitSlop={11}
                onPress={() => removeAttachment(index)}
              />
            </View>
          ))}
        </View>
      ) : null}
      <View
        style={{
          borderRadius: radius.card,
          ...glass.card,
          padding: 6,
          gap: 4,
        }}
      >
        <Input
          ref={(node) => {
            input = node;
          }}
          testID="composer-input"
          value={draft()}
          onChange={({ value }) => draft.set(value)}
          multiline
          submitBehavior={submitOnEnter ? "submit" : "newline"}
          onSubmit={() => {
            if (!stopping()) void send();
          }}
          placeholder={placeholder()}
          onContentSizeChange={({ size }) => inputHeight.set(Math.min(112, Math.max(20, size.height)))}
          style={{
            height: inputHeight() + 20,
            fontSize: 14,
            lineHeight: 20,
            color: colors.foreground,
            placeholderColor: colors.mutedForeground,
            caretColor: colors.brand,
            selectionColor: alpha(colors.brand, 0.3),
            paddingHorizontal: 12,
            paddingVertical: 10,
          }}
        />
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, paddingHorizontal: 4, paddingBottom: 2 }}>
          <IconButton
            icon="paperclip"
            label="添加附件"
            size={30}
            iconSize={15}
            hitSlop={compact ? 8 : undefined}
            onPress={() => void addPicked()}
          />
          <Segmented
            compact
            options={[
              { value: "queue", label: "队列", icon: "clock" },
              { value: "guide", label: "引导", icon: "external", disabled: !busy() },
            ]}
            value={effectiveMode()}
            onChange={(value) => mode.set(value)}
          />
          <View style={{ flex: 1 }} />
          <If when={windowSize().width >= 560}>
            <Text style={{ numberOfLines: 1, fontSize: 10.5, color: colors.mutedForeground }}>{hint()}</Text>
            <View style={{ flex: 1 }} />
          </If>
          <If when={busy()}>
            <Press
              testID="composer-stop"
              onPress={stopping() ? undefined : stop}
              disabled={stopping()}
              accessibilityLabel={stopping() ? "正在停止" : "停止当前任务"}
              hitSlop={4}
              hoverStyle={{ opacity: 0.9 }}
              activeStyle={{ opacity: 0.7 }}
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: 6,
                height: 36,
                paddingHorizontal: 12,
                borderRadius: 18,
                backgroundColor: stopping() ? colors.muted : colors.dangerTint,
                borderWidth: 1,
                borderColor: stopping() ? colors.border : alpha(colors.danger, 0.4),
              }}
            >
              {stopping() ? (
                <Spinner size={12} tint={colors.mutedForeground} />
              ) : (
                <Icon name="stop" size={12} tint={colors.danger} />
              )}
              <Text
                style={{
                  fontSize: 12,
                  fontWeight: "600",
                  color: stopping() ? colors.mutedForeground : colors.danger,
                }}
              >
                {stopping() ? "停止中" : "停止"}
              </Text>
            </Press>
          </If>
          <Press
            testID="composer-send"
            onPress={canSend() && !stopping() ? () => void send() : undefined}
            disabled={!canSend() || stopping()}
            accessibilityLabel="发送"
            hitSlop={compact ? 4 : undefined}
            hoverStyle={{ opacity: 0.92 }}
            activeStyle={{ opacity: 0.7 }}
            style={{
              width: 40,
              height: 40,
              borderRadius: 20,
              alignItems: "center",
              justifyContent: "center",
              backgroundColor: canSend() ? colors.primary : colors.muted,
              borderWidth: canSend() ? 0 : 1,
              borderColor: colors.glassEdge,
              boxShadow: canSend() ? `inset 0 1px 0 ${alpha(colors.primaryForeground, 0.25)}` : undefined,
            }}
          >
            <Icon
              name="arrow-up"
              size={17}
              tint={canSend() ? colors.primaryForeground : colors.mutedForeground}
            />
          </Press>
        </View>
        <View style={{ flexDirection: "row", alignItems: "center", gap: 5, paddingHorizontal: 8, paddingBottom: 2 }}>
          <Icon
            name={store.mode() === "fixture" ? "spark" : "wifi"}
            size={9}
            tint={colors.mutedForeground}
          />
          <Text style={{ fontSize: 10, color: colors.mutedForeground }}>
            {store.mode() === "fixture" ? "演示数据 · 本地 fixture" : "实时服务 · keel serve"}
          </Text>
        </View>
      </View>
    </View>
  );
}
