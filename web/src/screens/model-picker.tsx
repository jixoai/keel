// The session's model picker: a chip in the conversation header showing the
// session's own selection (distinct from the tenant default) and a dialog that
// picks it from /v1/models. The choice is an input fact — it rides every
// subsequent input and never writes the tenant document.
import { Scroll, signal, Text, View, windowSize } from "nsh";
import type { ModelSelection, ModelView } from "../data/contract";
import { store } from "../data/store";
import { modelFacts, reasoningLabel, selectionLabel } from "../data/wire";
import { Icon } from "../ui/icons";
import { Press, Spinner } from "../ui/primitives";
import { alpha, colors, glass, mono, radius, space } from "../ui/theme";

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

/** The header chip: the session's current model, or 默认. */
export function SessionModelChip() {
  const session = () => store.sessionSelection();
  const tenant = () => store.tenantSelection();
  const label = () => {
    const current = session();
    if (current) return selectionLabel(current);
    const fallback = tenant();
    return fallback ? `默认 ${selectionLabel(fallback)}` : "默认";
  };
  const wide = () => windowSize().width >= 660;
  return (
    <Press
      onPress={openModelPicker}
      accessibilityLabel="本会话模型"
      accessibilityState={{ selected: !!session() }}
      hoverStyle={{ backgroundColor: colors.hover }}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: 5,
        height: 26,
        maxWidth: wide() ? 260 : 132,
        paddingHorizontal: 9,
        borderRadius: radius.pill,
        backgroundColor: session() ? colors.brandTint : colors.muted,
        borderWidth: 1,
        borderColor: session() ? alpha(colors.brand, 0.36) : colors.glassEdge,
      }}
    >
      <Icon name="brain" size={11} tint={session() ? colors.brand : colors.mutedForeground} />
      <Text
        style={{
          numberOfLines: 1,
          textOverflow: "ellipsis",
          fontSize: 10.5,
          fontWeight: "600",
          color: session() ? colors.brand : colors.mutedForeground,
        }}
      >
        {label()}
      </Text>
      <Icon name="chevron-down" size={10} tint={session() ? colors.brand : colors.mutedForeground} />
    </Press>
  );
}

function PickerModelRow(props: { model: () => ModelView }) {
  const isCurrent = () => {
    const current = store.sessionSelection();
    return !!current && current.modelId === props.model().modelId && current.providerId === props.model().providerId;
  };
  const levels = () => props.model().options?.reasoningLevel?.values ?? [];
  return (
    <View
      style={{
        gap: 5,
        padding: space.sm,
        borderRadius: radius.control,
        borderWidth: 1,
        borderColor: isCurrent() ? alpha(colors.brand, 0.42) : colors.glassEdge,
        backgroundColor: isCurrent() ? colors.brandTint : undefined,
      }}
    >
      <Press
        onPress={() => selectModel(props.model())}
        accessibilityLabel={`选择模型 ${props.model().providerId}/${props.model().modelId}`}
        hoverStyle={{ opacity: 0.92 }}
        style={{ gap: 3 }}
      >
        <View style={{ flexDirection: "row", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
          <Text style={{ fontSize: 12.5, fontWeight: "600", color: colors.foreground }}>
            {props.model().modelId}
          </Text>
          <Text style={{ fontSize: 10, fontFamily: mono(), color: colors.mutedForeground }}>
            {props.model().providerId}
          </Text>
          <If when={isCurrent()}>
            <Text style={{ fontSize: 10.5, fontWeight: "600", color: colors.brand }}>本会话使用中</Text>
          </If>
        </View>
        <Text style={{ fontSize: 10.5, lineHeight: 15, color: colors.mutedForeground }}>
          {modelFacts(props.model())}
        </Text>
      </Press>
      <If when={levels().length > 0}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: 5, flexWrap: "wrap" }}>
          <Text style={{ fontSize: 10, color: colors.mutedForeground }}>思考档</Text>
          <Each of={levels()} key="index">
            {(level) => (
              <Press
                onPress={() => selectModel(props.model(), level())}
                accessibilityLabel={`本会话思考档 ${reasoningLabel(level())}`}
                accessibilityState={{
                  selected: isCurrent() && store.sessionSelection()?.reasoningLevel === level(),
                }}
                hoverStyle={{ opacity: 0.9 }}
                style={{
                  paddingHorizontal: 8,
                  height: 22,
                  borderRadius: radius.pill,
                  alignItems: "center",
                  justifyContent: "center",
                  backgroundColor:
                    isCurrent() && store.sessionSelection()?.reasoningLevel === level()
                      ? colors.glassControl
                      : colors.muted,
                  borderWidth: 1,
                  borderColor:
                    isCurrent() && store.sessionSelection()?.reasoningLevel === level()
                      ? colors.glassControlRim
                      : colors.glassEdge,
                }}
              >
                <Text
                  style={{
                    fontSize: 10,
                    fontWeight: "600",
                    color:
                      isCurrent() && store.sessionSelection()?.reasoningLevel === level()
                        ? colors.foreground
                        : colors.mutedForeground,
                  }}
                >
                  {reasoningLabel(level())}
                </Text>
              </Press>
            )}
          </Each>
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
    <View style={{ gap: space.sm }}>
      <If when={store.modelsError() || store.providersError()}>
        <Text style={{ fontSize: 11.5, lineHeight: 17, color: colors.danger }}>
          {store.modelsError() ?? store.providersError() ?? ""}
        </Text>
      </If>
      <If when={!store.registryLoaded() && !store.modelsError()}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: 8, paddingVertical: space.sm }}>
          <Spinner size={13} tint={colors.brand} />
          <Text style={{ fontSize: 12, color: colors.mutedForeground }}>正在读取可执行模型…</Text>
        </View>
      </If>
      <If when={store.registryLoaded() && groups().length === 0}>
        <Text style={{ fontSize: 12, lineHeight: 18, color: colors.mutedForeground }}>
          没有可选的模型：当前进程解析不到任何 provider 的凭据。配置密钥或环境变量后再试。
        </Text>
      </If>
      <Each of={groups()} key={(group) => group.providerId}>
        {(group) => (
          <View style={{ gap: 5 }}>
            <View style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
              <Text style={{ fontSize: 10.5, fontWeight: "600", color: colors.mutedForeground }}>
                {group().name}
              </Text>
              <Text style={{ fontSize: 10, fontFamily: mono(), color: colors.mutedForeground }}>
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

/** The picker dialog: session current vs tenant default, then the selectable
 * models with their reasoning levels. */
export function SessionModelPicker() {
  const close = () => pickerOpen.set(false);
  const session = () => store.sessionSelection();
  const compact = () => windowSize().width < 720 || windowSize().height < 560;
  return (
    <If when={pickerOpen() && store.route() === "workspace" && !!store.activeId()}>
      <View
        style={{
          position: "absolute",
          top: 0,
          left: 0,
          right: 0,
          bottom: 0,
          zIndex: 25,
          alignItems: "center",
          justifyContent: "center",
          padding: compact() ? 10 : 24,
        }}
      >
        <Press
          onPress={close}
          accessibilityLabel="关闭本会话模型选择"
          activeStyle={{ opacity: 1 }}
          hoverStyle={{ opacity: 1 }}
          style={{
            position: "absolute",
            top: 0,
            left: 0,
            right: 0,
            bottom: 0,
            backgroundColor: alpha(colors.background, 0.68),
          }}
        >
          <View style={{ flex: 1 }} />
        </Press>
        <View
          accessibilityLabel="本会话模型选择"
          style={{
            width: "100%",
            maxWidth: 520,
            maxHeight: "86%",
            borderRadius: radius.card,
            ...glass.popover,
            backgroundColor: colors.popover,
            padding: space.md,
            gap: space.sm,
          }}
        >
          <View style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
            <Icon name="brain" size={15} tint={colors.brand} />
            <Text style={{ flex: 1, fontSize: 14, fontWeight: "700", color: colors.foreground }}>
              本会话模型
            </Text>
            <Press
              onPress={close}
              accessibilityLabel="关闭"
              hoverStyle={{ backgroundColor: colors.hover }}
              style={{
                width: 26,
                height: 26,
                borderRadius: radius.control,
                alignItems: "center",
                justifyContent: "center",
              }}
            >
              <Icon name="x" size={13} tint={colors.mutedForeground} />
            </Press>
          </View>
          <View style={{ gap: 4 }}>
            <View style={{ flexDirection: "row", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
              <Text style={{ fontSize: 10.5, fontWeight: "600", color: colors.mutedForeground }}>本会话当前</Text>
              <If
                when={!!session()}
                else={<Text style={{ fontSize: 11.5, color: colors.foreground }}>未选择 · 使用租户默认</Text>}
              >
                <Text style={{ fontSize: 11.5, fontWeight: "600", color: colors.foreground }}>
                  {selectionLabel(session())}
                </Text>
              </If>
            </View>
            <View style={{ flexDirection: "row", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
              <Text style={{ fontSize: 10.5, fontWeight: "600", color: colors.mutedForeground }}>租户默认</Text>
              <If
                when={!!store.settings()}
                else={<Text style={{ fontSize: 11.5, color: colors.mutedForeground }}>读取中…</Text>}
              >
                <If
                  when={!!store.tenantSelection()}
                  else={<Text style={{ fontSize: 11.5, color: colors.warning }}>未设置</Text>}
                >
                  <Text style={{ fontSize: 11.5, color: colors.foreground }}>
                    {selectionLabel(store.tenantSelection())}
                  </Text>
                </If>
              </If>
            </View>
          </View>
          <View style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
            <Press
              onPress={session() ? () => store.setSessionSelection(null) : undefined}
              disabled={!session()}
              accessibilityLabel="恢复默认"
              hoverStyle={{ opacity: 0.9 }}
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: 5,
                height: 28,
                paddingHorizontal: 11,
                borderRadius: radius.control,
                backgroundColor: colors.dangerTint,
                borderWidth: 1,
                borderColor: alpha(colors.danger, 0.3),
              }}
            >
              <Icon name="refresh" size={12} tint={colors.danger} />
              <Text style={{ fontSize: 11.5, fontWeight: "600", color: colors.danger }}>恢复默认</Text>
            </Press>
            <Text style={{ flex: 1, fontSize: 10, lineHeight: 14, color: colors.mutedForeground }}>
              会话选择只随之后每条输入发送，不修改租户配置；恢复默认后输入不带选择。
            </Text>
          </View>
          <View style={{ height: 1, backgroundColor: colors.border }} />
          <Scroll style={{ maxHeight: compact() ? 300 : 380 }} contentStyle={{ paddingRight: 2 }}>
            <PickerGroups />
          </Scroll>
        </View>
      </View>
    </If>
  );
}
