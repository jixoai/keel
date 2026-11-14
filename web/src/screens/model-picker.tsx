// The session's model picker: a chip in the conversation header showing the
// session's own selection (distinct from the tenant default) and the kit's
// Dialog that picks it from /v1/models. The choice is an input fact — it rides
// every subsequent input and never writes the tenant document: the settings
// page stays the document's only writer, and the tenant default is shown here
// read-only.
//
// One press on the chip opens the Dialog itself — there is no popover layer in
// between. Every piece is a kit component (Button / Dialog / Badge / RadioGroup
// / ScrollArea / Separator) and every size a token: no hand-drawn pill, no
// scrim, no z-index and no breakpoint in this file.
import { signal, Text, View } from "nsh";
import type { ModelSelection, ModelView } from "../data/contract";
import { store } from "../data/store";
import {
  checkSurfaceSwitch,
  resolveSelectionSurface,
  resolveSurface,
  surfaceFact,
} from "../surfaces/registry";
import {
  displayNameOf,
  modelFacts,
  providerDisplayName,
  reasoningLabel,
  selectionDisplay,
  selectionLabel,
} from "../data/wire";
import { Icon } from "../ui/icons";
import {
  Badge,
  Button,
  Card,
  CardContent,
  Dialog,
  IconButton,
  RadioGroup,
  ScrollArea,
  Separator,
  Spinner,
  Tooltip,
  alpha,
  color,
  font,
  iconSize,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
} from "../ui/kit";

export const pickerOpen = signal(false);

/** The registry note's raw-detail disclosure: closed by default, and closed
 * again on every open, so the next picker always starts quiet. */
const errorDetailOpen = signal(false);

export const openModelPicker = () => {
  // Every open starts from the quiet state: the registry note's technical
  // detail is a disclosure, never the default face of the dialog.
  errorDetailOpen.set(false);
  pickerOpen.set(true);
  // The options come from the resolved registry; load it if this is the first
  // look (the settings page shares the same load).
  void store.loadRegistry();
  if (!store.settings()) void store.loadModelsSettings();
};

/** The level a model is picked with: the choice already naming this model,
 * else the tenant default's, else 高, else the most capable declared value. */
export function defaultReasoningFor(model: ModelView, current: ModelSelection | null): string | undefined {
  const levels = model.options?.reasoningLevel?.values ?? [];
  if (!levels.length) return undefined;
  const matches = (selection: ModelSelection | null) =>
    !!selection &&
    selection.modelId === model.modelId &&
    (!selection.providerId || selection.providerId === model.providerId) &&
    !!selection.reasoningLevel &&
    levels.includes(selection.reasoningLevel);
  if (matches(current)) return current?.reasoningLevel;
  const tenant = store.tenantSelection();
  if (matches(tenant)) return tenant?.reasoningLevel;
  if (levels.includes("high")) return "high";
  return levels[levels.length - 1];
}

/** A model whose surface differs from the open session's: applying it starts a
 * new session, it never replaces the open one's history. */
const pendingSwitch = signal<{ model: ModelView; level?: string; reason: string } | null>(null);

const providerOf = (model: ModelView) =>
  store.providers().find((provider) => provider.providerId === model.providerId) ?? null;

const selectModel = (model: ModelView, reasoningLevel?: string) => {
  const current = store.sessionSelection() ?? store.tenantSelection();
  const currentKind = resolveSelectionSurface(current, store.models(), store.providers()).kind;
  const nextKind = resolveSurface(model, providerOf(model)).kind;
  const level = reasoningLevel ?? defaultReasoningFor(model, current);
  if (current && currentKind !== nextKind) {
    pendingSwitch.set({
      model,
      ...(level ? { level } : {}),
      reason: checkSurfaceSwitch(currentKind, nextKind).reason ?? "需要新建会话。",
    });
    return;
  }
  pendingSwitch.set(null);
  const next: ModelSelection = { providerId: model.providerId, modelId: model.modelId };
  if (level) next.reasoningLevel = level;
  store.setSessionSelection(next);
};

const confirmSurfaceSwitch = async () => {
  const pending = pendingSwitch();
  if (!pending) return;
  pendingSwitch.set(null);
  const id = await store.createSession();
  if (!id) return;
  const next: ModelSelection = { providerId: pending.model.providerId, modelId: pending.model.modelId };
  if (pending.level) next.reasoningLevel = pending.level;
  store.setSessionSelection(next);
  pickerOpen.set(false);
};

/** One model's declared levels as radio options (低 / 高 …). */
const levelOptions = (levels: string[]) => levels.map((level) => ({ value: level, label: reasoningLabel(level) }));

/** The header chip: the model this session's next input runs on — the
 * session's own choice, else the tenant default. The visible label is the
 * model's readable name (`selectionDisplay`); the wire identity
 * (`selectionLabel`) lives in the tooltip. A kit Button — one press opens the
 * picker Dialog directly; `iconOnly` is the narrow column's form (the header
 * pays title room with it, and the accessibility label carries the name). */
export function SessionModelChip(props: { iconOnly?: boolean } = {}) {
  const session = () => store.sessionSelection();
  const tenant = () => store.tenantSelection();
  const effective = () => session() ?? tenant();
  const label = () => selectionDisplay(effective()) || "选择模型";
  const detail = () => selectionLabel(effective());
  return (
    <If
      when={!props.iconOnly}
      else={
        <IconButton
          testID="session-model-chip"
          variant="outline"
          icon="brain"
          label={`本会话模型：${label()}`}
          onPress={() => openModelPicker()}
        />
      }
    >
      <Button
        testID="session-model-chip"
        variant="outline"
        size="sm"
        icon="brain"
        iconTrailing="chevron-down"
        label={label()}
        selected={!!session()}
        accessibilityLabel={`本会话模型：${label()}`}
        tooltip={detail() ? <Tooltip label={detail()} /> : undefined}
        onPress={() => openModelPicker()}
      />
    </If>
  );
}

function PickerModelRow(props: { model: () => ModelView }) {
  /** Whether this row is the model the open session runs on. */
  const isCurrent = () => {
    const current = store.sessionSelection();
    return !!current && current.modelId === props.model().modelId && current.providerId === props.model().providerId;
  };
  const levels = () => props.model().options?.reasoningLevel?.values ?? [];
  const level = () => (isCurrent() ? store.sessionSelection()?.reasoningLevel : undefined);
  return (
    <View
      style={{
        gap: space.xs,
        padding: space.sm,
        borderRadius: radius.lg,
        borderWidth: 1,
        // The current row reads at a glance: a full-strength accent edge over a
        // tinted face, plus the state badge below (brand is the theme's accent,
        // so this stays one visual language in both appearances).
        borderColor: isCurrent() ? alpha(color.brand, 0.7) : color.border,
        backgroundColor: isCurrent() ? color.brandTint : undefined,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <Button
          variant="ghost"
          size="sm"
          label={displayNameOf(props.model().modelId)}
          accessibilityLabel={`选择模型 ${displayNameOf(props.model().modelId)}`}
          tooltip={<Tooltip label={`${props.model().providerId}/${props.model().modelId}`} />}
          selected={isCurrent()}
          onPress={() => selectModel(props.model())}
        />
        <If when={isCurrent()}>
          <Badge variant="default" label="本会话使用中" />
        </If>
      </View>
      <Text style={{ fontSize: typeScale.xs, lineHeight: lineHeight.normal, color: color.mutedForeground }}>
        {`${modelFacts(props.model())} · ${surfaceFact(props.model(), providerOf(props.model()))}`}
      </Text>
      <If when={levels().length > 0}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
          <Text style={{ fontSize: typeScale.xs, fontWeight: weight.medium, color: color.mutedForeground }}>
            思考档
          </Text>
          <RadioGroup
            orientation="horizontal"
            options={levelOptions(levels())}
            value={level()}
            onChange={(value) => selectModel(props.model(), value)}
            accessibilityLabel={`思考档 ${displayNameOf(props.model().modelId)}`}
          />
        </View>
      </If>
    </View>
  );
}

/** The transport's reason, made fit to show: a loopback host:port is an
 * implementation detail of the server, never a fact the reader needs (and a
 * private address is not something this surface may print). */
function redactEndpoint(reason: string): string {
  return reason
    .replace(/\b(?:localhost|127(?:\.\d{1,3}){3}|0\.0\.0\.0|\[::1\])(?::\d+)?\b/gi, "本机服务")
    .replace(/\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b/g, "服务地址")
    .replace(/\bdial tcp\b/gi, "连接")
    .replace(/connect:\s*connection refused/gi, "连接被拒绝");
}

function PickerGroups() {
  const registryError = () => store.modelsError() ?? store.providersError() ?? "";
  const notWired = () => registryError().includes("not wired");
  const selectableCount = () => store.models().filter((model) => model.selectable !== false).length;
  /** One state wins: when usable rows are listed, the note is gone — a banner
   * saying "not connected" over selectable models would contradict the screen.
   * The note only explains an empty list. */
  const registryNote = () => !!registryError() && selectableCount() === 0;
  const groups = () => {
    const map = new Map<string, ModelView[]>();
    for (const model of store.models()) {
      if (model.selectable === false) continue;
      const list = map.get(model.providerId) ?? [];
      list.push(model);
      map.set(model.providerId, list);
    }
    return [...map.entries()].map(([providerId, models]) => {
      const provider = store.providers().find((row) => row.providerId === providerId) ?? { providerId };
      const name = providerDisplayName(provider);
      return {
        providerId,
        name,
        models,
        // "S2API s2api" says the same thing twice: the wire id is a detail
        // line, shown only when the readable name actually differs from it.
        showId: name.replace(/\s+/g, "").toLowerCase() !== providerId.replace(/[-_]/g, "").toLowerCase(),
      };
    });
  };
  return (
    <View style={{ gap: space.md }}>
      {/* A registry note is a block, not a stray line: its own face, an icon,
          and a body that wraps inside the width it is given — the same shape
          the conversation's offline banner uses. The transport's own words sit
          behind the disclosure, with private endpoints redacted. */}
      <If when={registryNote()}>
        <Card
          testID="model-registry-note"
          accessibilityLabel={notWired() ? "真实模型暂未接入" : "真实模型暂不可用"}
          size="sm"
          style={{
            flexDirection: "row",
            alignItems: "flex-start",
            gap: space.md,
            backgroundColor: color.warningTint,
            borderColor: alpha(color.warning, 0.32),
          }}
        >
          <Icon name="alert" size={iconSize.sm} tint={color.warning} />
          <View style={{ flex: 1, gap: space.xxs }}>
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.warning }}>
              {notWired() ? "真实模型暂未接入" : "真实模型暂不可用"}
            </Text>
            <Text
              style={{
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                color: color.foreground,
                userSelect: "text",
              }}
            >
              {notWired()
                ? "配置 Provider 的密钥后，真实模型会出现在这里。"
                : "模型服务未启动或暂时不可达，真实模型列表读不到。确认服务运行后重试。"}
            </Text>
            <Button
              variant="ghost"
              size="sm"
              label={errorDetailOpen() ? "收起技术细节" : "技术细节"}
              accessibilityLabel={errorDetailOpen() ? "收起技术细节" : "查看技术细节"}
              style={{ alignSelf: "flex-start" }}
              onPress={() => errorDetailOpen.set(!errorDetailOpen())}
            />
            <If when={errorDetailOpen()}>
              <Text
                style={{
                  fontFamily: font.mono,
                  fontSize: typeScale.xs,
                  lineHeight: lineHeight.normal,
                  color: color.mutedForeground,
                  userSelect: "text",
                }}
              >
                {redactEndpoint(registryError())}
              </Text>
            </If>
          </View>
        </Card>
      </If>
      <If when={!store.registryLoaded() && !registryError()}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, paddingVertical: space.sm }}>
          <Spinner size={iconSize.sm} tint={color.brand} />
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>正在读取可执行模型…</Text>
        </View>
      </If>
      {/* The note above already says why the list is empty; a second paragraph
          would only repeat it. */}
      <If when={!registryError() && store.registryLoaded() && selectableCount() === 0}>
        <Text
          style={{
            fontSize: typeScale.sm,
            lineHeight: Math.round(typeScale.sm * lineHeight.normal),
            color: color.mutedForeground,
          }}
        >
          没有可执行的 Provider。配置密钥或环境变量后再试。
        </Text>
      </If>
      <Each of={groups()} key={(group) => group.providerId}>
        {(group) => (
          <View style={{ gap: space.sm }}>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
              <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.mutedForeground }}>
                {group().name}
              </Text>
              {/* The wire identity is a detail line: shown only when the
                  readable name differs from it. */}
              <If when={group().showId}>
                <Text style={{ fontSize: typeScale.xs, fontFamily: font.mono, color: color.mutedForeground }}>
                  {group().providerId}
                </Text>
              </If>
            </View>
            <Each of={group().models} key={(model) => `${model.providerId}/${model.modelId}`}>
              {(model) => <PickerModelRow model={model} />}
            </Each>
          </View>
        )}
      </Each>
    </View>
  );
}

/** The picker dialog: the session's own selection next to the tenant default
 * (read-only here — the settings page is the tenant document's only writer),
 * then the selectable models with their reasoning levels. */
export function SessionModelPicker() {
  const session = () => store.sessionSelection();
  const tenant = () => store.tenantSelection();
  return (
    <Dialog
      open={pickerOpen() && store.route() === "workspace" && !!store.activeId()}
      size="default"
      // The popup never grows past its natural height: the model list below
      // carries its own viewport cap and scrolls inside it.
      maxHeight={600}
      // Center the card in the runtime's dialog layer: nsh's dialog popup sits
      // where its own style puts it, and `margin: auto` is the documented way
      // to center it there (nsh `elements/dialog.md`, "a card centered over
      // it"; the kit Dialog carries no placement of its own).
      style={{ margin: "auto" }}
      title="本会话模型"
      description="会话选择会随之后每条输入发送，不修改租户配置；恢复默认后输入不再携带会话选择。"
      accessibilityLabel="本会话模型选择"
      testID="session-model-picker"
      onOpenChange={(event) => {
        if (!event.open) {
          pickerOpen.set(false);
          errorDetailOpen.set(false);
        }
      }}
      onClose={() => {
        pickerOpen.set(false);
        errorDetailOpen.set(false);
      }}
      footer={
        // The footer is its own band, not the tail of the model list: an
        // opaque face and a hairline above it (the list's bottom fade sits on
        // the other side of that line), so a sliced row can never read as part
        // of this row — at 390 the two used to share one band.
        <View
          style={{
            width: "100%",
            flexDirection: "row",
            alignItems: "center",
            justifyContent: "flex-end",
            paddingTop: space.md,
            borderTopWidth: 1,
            borderTopColor: color.border,
            backgroundColor: color.popover,
          }}
        >
          {/* Clearing a selection is a reset, not a deletion: the neutral ghost
              face is the honest treatment (danger is reserved for destructive
              actions). */}
          <Button
            variant="ghost"
            size="sm"
            icon="refresh"
            label="恢复默认"
            accessibilityLabel="恢复默认"
            tooltip={<Tooltip label="清除本会话的模型选择" />}
            disabled={!session()}
            onPress={() => {
              store.setSessionSelection(null);
            }}
          />
        </View>
      }
    >
      <View style={{ gap: space.lg }}>
        <View style={{ gap: space.xs }}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.mutedForeground }}>
              本会话当前
            </Text>
            {/* The row names the session's own selection; without one the
                tenant default is what runs. */}
            <If
              when={!!session()}
              else={<Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>未选择 · 使用租户默认</Text>}
            >
              <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
                {selectionDisplay(session())}
              </Text>
            </If>
          </View>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.mutedForeground }}>
              租户默认
            </Text>
            <If
              when={!!store.settings()}
              else={<Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>读取中…</Text>}
            >
              <If
                when={!!store.tenantSelection()}
                else={
                  // The warning state as an outline pill (warning ink, no
                  // fill): distinct from the filled marks, still legible.
                  <Badge
                    variant="outline"
                    accessibilityLabel="未设置"
                    style={{ borderColor: alpha(color.warning, 0.35), backgroundColor: "transparent" }}
                  >
                    <Text style={{ fontSize: typeScale.sm, fontWeight: weight.medium, color: color.warning }}>
                      未设置
                    </Text>
                  </Badge>
                }
              >
                <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
                  <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>
                    {selectionDisplay(store.tenantSelection())}
                  </Text>
                  <Badge variant="outline" label="只读" />
                </View>
              </If>
            </If>
          </View>
          <Text style={{ fontSize: typeScale.xs, lineHeight: lineHeight.normal, color: color.mutedForeground }}>
            租户默认在设置页的「模型」分区修改；此处只读显示，不会改动配置。
          </Text>
          {/* The wire identity, as the detail line: the readable name above is
              what the reader picks by, the id is what the server stores. */}
          <If when={!!(session() ?? tenant())}>
            <Text
              style={{
                fontFamily: font.mono,
                fontSize: typeScale["2xs"],
                color: color.mutedForeground,
                userSelect: "text",
              }}
            >
              {selectionLabel(session() ?? tenant())}
            </Text>
          </If>
        </View>
        <If when={!!pendingSwitch()}>
          <Card size="sm" testID="surface-switch-confirm" accessibilityLabel="跨会话面板切换确认">
            <CardContent>
              <View style={{ gap: space.sm }}>
                {/* The badge gets its own row: inline with a long wrapping
                    sentence the row's height is computed from one line and the
                    text spills over the next sibling. */}
                <View style={{ flexDirection: "row" }}>
                  <Badge variant="warning" label="需要新建会话" />
                </View>
                <Text
                  style={{
                    fontSize: typeScale.sm,
                    lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                    color: color.foreground,
                  }}
                >
                  {pendingSwitch()?.reason ?? ""}
                </Text>
                <Text
                  style={{
                    fontSize: typeScale.xs,
                    lineHeight: Math.round(typeScale.xs * lineHeight.normal),
                    color: color.mutedForeground,
                  }}
                >
                  目标：{pendingSwitch() ? surfaceFact(pendingSwitch()!.model) : ""}。
                  确认后创建新会话并应用该模型；当前会话的历史保持不动。
                </Text>
                <View
                  style={{
                    flexDirection: "row",
                    alignItems: "center",
                    gap: space.sm,
                    justifyContent: "flex-end",
                    flexWrap: "wrap",
                  }}
                >
                  <Button variant="ghost" size="sm" label="取消" onPress={() => pendingSwitch.set(null)} />
                  <Button
                    variant="default"
                    size="sm"
                    label="新建会话并应用"
                    onPress={() => void confirmSurfaceSwitch()}
                  />
                </View>
              </View>
            </CardContent>
          </Card>
        </If>
        <Separator />
        {/* The list's own viewport, with the scroll cue the sliced row needs:
            the content keeps a bottom inset so the last row can clear the
            gradient, and the gradient (transparent → the popover's own face)
            fades whatever is below the fold instead of cutting it. The strip
            is invisible when nothing scrolls under it, so a short list shows
            no false "more below" hint. */}
        <View style={{ position: "relative" }}>
          <ScrollArea
            maxHeight={380}
            fade
            accessibilityLabel="可选模型"
            contentStyle={{ paddingBottom: space["2xl"] }}
          >
            <PickerGroups />
          </ScrollArea>
          <View
            style={{
              position: "absolute",
              left: 0,
              right: 0,
              bottom: 0,
              height: space["2xl"],
              pointerEvents: "none",
              backgroundImage: `linear-gradient(to top, ${alpha(color.popover, 1)}, ${alpha(color.popover, 0)})`,
            }}
          />
        </View>
      </View>
    </Dialog>
  );
}
