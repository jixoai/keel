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
import { modelFacts, reasoningLabel, selectionLabel } from "../data/wire";
import { Icon } from "../ui/icons";
import {
  Badge,
  Button,
  Dialog,
  RadioGroup,
  ScrollArea,
  Separator,
  Spinner,
  alpha,
  color,
  controlIconSize,
  font,
  iconSize,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
} from "../ui/kit";

export const pickerOpen = signal(false);

export const openModelPicker = () => {
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

const selectModel = (model: ModelView, reasoningLevel?: string) => {
  const current = store.sessionSelection();
  const level = reasoningLevel ?? defaultReasoningFor(model, current);
  const next: ModelSelection = { providerId: model.providerId, modelId: model.modelId };
  if (level) next.reasoningLevel = level;
  store.setSessionSelection(next);
};

/** One model's declared levels as radio options (低 / 高 …). */
const levelOptions = (levels: string[]) => levels.map((level) => ({ value: level, label: reasoningLabel(level) }));

/** The header chip: the session's current model, or 默认. A kit Button — one
 * press opens the picker Dialog directly. */
export function SessionModelChip() {
  const session = () => store.sessionSelection();
  const tenant = () => store.tenantSelection();
  const label = () => {
    const current = session();
    if (current) return selectionLabel(current);
    const fallback = tenant();
    return fallback ? `默认 ${selectionLabel(fallback)}` : "默认";
  };
  return (
    <Button
      variant="outline"
      size="sm"
      icon="brain"
      iconTrailing="chevron-down"
      label={label()}
      selected={!!session()}
      accessibilityLabel="本会话模型"
      onPress={() => openModelPicker()}
    />
  );
}

function PickerModelRow(props: { model: () => ModelView }) {
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
        borderColor: isCurrent() ? alpha(color.brand, 0.42) : color.border,
        backgroundColor: isCurrent() ? color.brandTint : undefined,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
        <Button
          variant="ghost"
          size="sm"
          label={`${props.model().providerId}/${props.model().modelId}`}
          accessibilityLabel={`选择模型 ${props.model().providerId}/${props.model().modelId}`}
          selected={isCurrent()}
          onPress={() => selectModel(props.model())}
        />
        <If when={isCurrent()}>
          <Badge variant="info" label="本会话使用中" />
        </If>
      </View>
      <Text style={{ fontSize: typeScale.xs, lineHeight: lineHeight.normal, color: color.mutedForeground }}>
        {modelFacts(props.model())}
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
            accessibilityLabel={`思考档 ${props.model().providerId}/${props.model().modelId}`}
          />
        </View>
      </If>
    </View>
  );
}

function PickerGroups() {
  const groups = () => {
    const map = new Map<string, ModelView[]>();
    for (const model of store.models()) {
      if (model.selectable === false) continue;
      const list = map.get(model.providerId) ?? [];
      list.push(model);
      map.set(model.providerId, list);
    }
    return [...map.entries()].map(([providerId, models]) => ({
      providerId,
      name: store.providers().find((provider) => provider.providerId === providerId)?.name ?? providerId,
      models,
    }));
  };
  return (
    <View style={{ gap: space.md }}>
      <If when={store.modelsError() || store.providersError()}>
        <Text style={{ fontSize: typeScale.sm, lineHeight: lineHeight.normal, color: color.danger }}>
          {store.modelsError() ?? store.providersError() ?? ""}
        </Text>
      </If>
      <If when={!store.registryLoaded() && !store.modelsError()}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, paddingVertical: space.sm }}>
          <Spinner size={iconSize.sm} tint={color.brand} />
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>正在读取可执行模型…</Text>
        </View>
      </If>
      <If when={store.registryLoaded() && groups().length === 0}>
        <Text style={{ fontSize: typeScale.sm, lineHeight: lineHeight.normal, color: color.mutedForeground }}>
          没有可选的模型：当前进程解析不到任何 provider 的凭据。配置密钥或环境变量后再试。
        </Text>
      </If>
      <Each of={groups()} key={(group) => group.providerId}>
        {(group) => (
          <View style={{ gap: space.sm }}>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
              <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.mutedForeground }}>
                {group().name}
              </Text>
              <Text style={{ fontSize: typeScale.xs, fontFamily: font.mono, color: color.mutedForeground }}>
                {group().providerId}
              </Text>
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
      description="会话选择随之后每条输入发送，不修改租户配置；恢复默认后输入不带选择。"
      accessibilityLabel="本会话模型选择"
      testID="session-model-picker"
      onOpenChange={(event) => {
        if (!event.open) pickerOpen.set(false);
      }}
      onClose={() => pickerOpen.set(false)}
      footer={
        <Button
          variant="ghost"
          size="sm"
          accessibilityLabel="恢复默认"
          // danger-ghost: the kit's Button carries the ghost face and this
          // button's own children carry the danger ink (there is no danger
          // variant on the Button, and a hand-styled button would be a second
          // button language).
          disabled={!session()}
          onPress={() => store.setSessionSelection(null)}
          hoverStyle={{ backgroundColor: color.dangerTint }}
        >
          <Icon name="refresh" size={controlIconSize("sm")} tint={color.danger} />
          <Text style={{ fontSize: typeScale.base, fontWeight: weight.medium, color: color.danger }}>恢复默认</Text>
        </Button>
      }
    >
      <View style={{ gap: space.lg }}>
        <View style={{ gap: space.xs }}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.mutedForeground }}>
              本会话当前
            </Text>
            <If
              when={!!session()}
              else={<Text style={{ fontSize: typeScale.sm, color: color.foreground }}>未选择 · 使用租户默认</Text>}
            >
              <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
                {selectionLabel(session())}
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
                else={<Text style={{ fontSize: typeScale.sm, color: color.warning }}>未设置</Text>}
              >
                <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
                  <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>
                    {selectionLabel(store.tenantSelection())}
                  </Text>
                  <Badge variant="outline" label="只读" />
                </View>
              </If>
            </If>
          </View>
          <Text style={{ fontSize: typeScale.xs, lineHeight: lineHeight.normal, color: color.mutedForeground }}>
            租户默认只在设置页的「模型」分区修改；这里显示它，不在这里写它。
          </Text>
        </View>
        <Separator />
        <ScrollArea maxHeight={380} fade accessibilityLabel="可选模型">
          <PickerGroups />
        </ScrollArea>
      </View>
    </Dialog>
  );
}
