// The composer: queue/guide mode, attachments, send/stop. It only calls the
// store; the delivery decision (a mode applies while a run is active, an idle
// session always sends queue) is made in one place.
import { Picker, platform, signal, Text, View, windowSize } from "nsh";
import { Input } from "nsh/base";
import type { ConversationView, Delivery } from "../data/contract";
import type { PendingAttachment } from "../data/store";
import { store } from "../data/store";
import { Icon } from "../ui/icons";
import { IconButton, PillButton, Press, Segmented, Spinner } from "../ui/primitives";
import { alpha, colors, glass, radius, space } from "../ui/theme";
import { QueuePanel, GuideWaitingBanner } from "./messages";

const draft = signal("");
const mode = signal<Delivery>("queue");
const attachments = signal<PendingAttachment[]>([]);
const inputHeight = signal(20);

export const composerDraft = draft;

export function Composer(props: { view: ConversationView | null; compact?: boolean }) {
  const view = () => props.view;
  const busy = () => !!view()?.busy;
  const stopping = () => !!view()?.stopping;
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
    await store.send(text, mode(), pending);
  };

  const stop = () => {
    if (stopping()) return;
    void store.stop();
  };

  const submitOnEnter = platform.idiom !== "phone";
  const placeholder = () =>
    busy()
      ? mode() === "guide"
        ? "输入引导，注入当前任务…"
        : "输入消息，排队等待当前任务结束…"
      : "给 Keel 发消息…";

  return (
    <View style={{ width: "100%", maxWidth: 880, alignSelf: "center", gap: space.sm, paddingHorizontal: props.compact ? 10 : 16 }}>
      <QueuePanel queue={view()?.queue ?? []} />
      <GuideWaitingBanner visible={guideCount() > 0} count={guideCount()} />
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
              <IconButton icon="x" label="移除附件" size={22} iconSize={11} onPress={() => removeAttachment(index)} />
            </View>
          ))}
        </View>
      ) : null}
      <View
        style={{
          borderRadius: radius.pill,
          ...glass.card,
          padding: 6,
          gap: 4,
        }}
      >
        <Input
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
            onPress={() => void addPicked()}
          />
          <Segmented
            compact
            options={[
              { value: "queue", label: "队列", icon: "clock" },
              { value: "guide", label: "引导", icon: "external" },
            ]}
            value={mode()}
            onChange={(value) => mode.set(value)}
          />
          <View style={{ flex: 1 }} />
          <If when={windowSize().width >= 560}>
            <Text style={{ numberOfLines: 1, fontSize: 10.5, color: colors.mutedForeground }}>
              {busy()
                ? mode() === "guide"
                  ? "引导会在工具轮边界注入；无法注入时回到队列"
                  : "当前任务结束后按顺序发送"
                : platform.idiom === "phone"
                  ? "Return 换行"
                  : "Enter 发送 · Shift+Enter 换行"}
            </Text>
            <View style={{ flex: 1 }} />
          </If>
          <If when={busy()}>
            <Press
              testID="composer-stop"
              onPress={stopping() ? undefined : stop}
              disabled={stopping()}
              accessibilityLabel={stopping() ? "正在停止" : "停止当前任务"}
              hoverStyle={{ opacity: 0.9 }}
              activeStyle={{ opacity: 0.7 }}
              style={{
                width: 34,
                height: 34,
                borderRadius: 17,
                alignItems: "center",
                justifyContent: "center",
                backgroundColor: colors.dangerTint,
                borderWidth: 1,
                borderColor: alpha(colors.danger, 0.34),
              }}
            >
              {stopping() ? (
                <Spinner size={14} tint={colors.danger} />
              ) : (
                <Icon name="stop" size={14} tint={colors.danger} />
              )}
            </Press>
          </If>
          <Press
            testID="composer-send"
            onPress={canSend() && !stopping() ? () => void send() : undefined}
            disabled={!canSend() || stopping()}
            accessibilityLabel="发送"
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
      </View>
      <View style={{ flexDirection: "row", alignItems: "center", justifyContent: "center", gap: 6, paddingBottom: 2 }}>
        <Text style={{ fontSize: 10, color: colors.mutedForeground }}>
          {store.mode() === "fixture" ? "演示数据 · 本地 fixture" : "实时服务 · keel serve"}
        </Text>
        <Text style={{ fontSize: 10, color: colors.mutedForeground }}>·</Text>
        <Text style={{ fontSize: 10, color: colors.mutedForeground }}>
          {stopping() ? "停止屏障已触发，等待权威确认" : busy() ? "运行中" : "就绪"}
        </Text>
      </View>
    </View>
  );
}

/** Send a suggestion from the empty state through the same composer path. */
export function sendSuggestion(text: string) {
  draft.set(text);
  void store.send(text, busyMode(), []);
  draft.set("");
}

const busyMode = (): Delivery => (store.busy() ? "queue" : "queue");
