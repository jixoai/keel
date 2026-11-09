// The settings page (change keel-model-settings): its own route (`/settings`,
// deep-linkable through keel serve's SPA fallback), reachable from the session
// list and dismissible back to the workspace. Three sections:
//
//   模型   source/revision/default selection, the resolved provider and model
//          registry with their issues, the default model + reasoning level,
//          and the raw document with save / reset (per-field server errors).
//   密钥   names and times only; values go one way (write-only) and the
//          section is disabled with the server's reason when the secret box
//          is not configured.
//   插件   the assembled extension surface: generation, plugin states, tools,
//          hooks, triggers and diagnostics, plus manual fires. Plugins load at
//          boot — this page observes and triggers, it does not edit them.
//
// NSH components run once: every value read happens inside JSX or an <If>,
// never copied into a local at the top of a component. Lists are <Each>, rows
// read their item through the accessor `Each` hands them.
import { cleanup, effect, platform, Scroll, signal, Text, View, windowSize } from "nsh";
import { Input } from "nsh/base";
import type {
  ModelSelection,
  ModelView,
  PluginStatusView,
  ProviderView,
  SecretMeta,
  SettingsIssue,
  TriggerStatusView,
} from "../data/contract";
import { store } from "../data/store";
import { formatTime, modelFacts, reasoningLabel, selectionLabel } from "../data/wire";
import { Icon, type IconName } from "../ui/icons";
import { Badge, Press, Spinner } from "../ui/primitives";
import { alpha, colors, glass, mono, radius, space } from "../ui/theme";
import { defaultReasoningFor } from "./model-picker";

export type SettingsSection = "models" | "secrets" | "plugins";

/** The open section (module-level so it survives a route swap). */
export const settingsSection = signal<SettingsSection>("models");

/** The raw-document editor's text; it follows the loaded document until the
 * user types, and is the single source of truth for a save. */
const rawDocument = signal("");
const rawDirty = signal(false);
const rawParseError = signal<string | null>(null);
const confirmingReset = signal(false);

const secretName = signal("");
const secretValue = signal("");
/** The secret row whose delete is waiting for its confirmation. */
const confirmingSecretDelete = signal<string | null>(null);

const isTerminal = () => platform.idiom === "terminal";
const compact = () => isTerminal() || windowSize().width < 720;

const parseDocument = (text: string): { document?: Record<string, unknown>; error?: string } => {
  try {
    const value: unknown = JSON.parse(text);
    if (!value || typeof value !== "object" || Array.isArray(value)) {
      return { error: "文档必须是一个 JSON 对象" };
    }
    return { document: value as Record<string, unknown> };
  } catch (error) {
    return { error: error instanceof Error ? error.message : String(error) };
  }
};

const selectionOfDocument = (document: Record<string, unknown>): ModelSelection | null => {
  const row = document.selection;
  if (!row || typeof row !== "object") return null;
  const selection = row as Record<string, unknown>;
  if (typeof selection.modelId !== "string" || !selection.modelId) return null;
  return {
    providerId: typeof selection.providerId === "string" ? selection.providerId : undefined,
    modelId: selection.modelId,
    reasoningLevel: typeof selection.reasoningLevel === "string" ? selection.reasoningLevel : undefined,
  };
};

/** Rewrite the edited document's default selection (the raw editor is the one
 * source of truth; a parse error blocks this and says so). */
const writeSelection = (next: ModelSelection) => {
  const parsed = parseDocument(rawDocument());
  if (!parsed.document) return;
  const selection: Record<string, unknown> = { providerId: next.providerId, modelId: next.modelId };
  if (next.reasoningLevel) selection.reasoningLevel = next.reasoningLevel;
  rawDocument.set(JSON.stringify({ ...parsed.document, selection }, null, 2));
  rawDirty.set(true);
  rawParseError.set(null);
};

// ---- shared pieces --------------------------------------------------------

function IssueRow(props: { issue: () => SettingsIssue }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "flex-start", gap: 6 }}>
      <Icon name="alert" size={12} tint={colors.danger} />
      <View style={{ flex: 1, gap: 1 }}>
        <If when={!!props.issue().field}>
          <Text style={{ fontSize: 10.5, fontFamily: mono(), color: colors.danger }}>{props.issue().field}</Text>
        </If>
        <Text style={{ fontSize: 11.5, lineHeight: 17, color: colors.foreground }}>{props.issue().message}</Text>
      </View>
    </View>
  );
}

function Issues(props: { issues: () => SettingsIssue[]; label?: string }) {
  return (
    <If when={props.issues().length > 0}>
      <View style={{ gap: 4 }}>
        <If when={!!props.label}>
          <Text style={{ fontSize: 10.5, fontWeight: "600", color: colors.mutedForeground }}>{props.label ?? ""}</Text>
        </If>
        <Each of={props.issues()} key={(issue) => `${issue.field}|${issue.message}`}>
          {(issue) => <IssueRow issue={issue} />}
        </Each>
      </View>
    </If>
  );
}

function FieldLabel(props: { text: string }) {
  return <Text style={{ fontSize: 10.5, fontWeight: "600", color: colors.mutedForeground }}>{props.text}</Text>;
}

function StateChip(props: { label: string; tone: "neutral" | "brand" | "success" | "warning" | "danger" | "info" }) {
  return <Badge label={props.label} tone={props.tone} />;
}

function LoadingRow(props: { text: string }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: 8, paddingVertical: space.md }}>
      <Spinner size={14} tint={colors.brand} />
      <Text style={{ fontSize: 12, color: colors.mutedForeground }}>{props.text}</Text>
    </View>
  );
}

function ErrorRow(props: { text: string; onRetry?: () => void }) {
  return (
    <View
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: 8,
        padding: space.md,
        borderRadius: radius.control,
        backgroundColor: colors.dangerTint,
        borderWidth: 1,
        borderColor: alpha(colors.danger, 0.3),
      }}
    >
      <Icon name="alert" size={14} tint={colors.danger} />
      <Text style={{ flex: 1, fontSize: 11.5, lineHeight: 17, color: colors.foreground }}>{props.text}</Text>
      <If when={!!props.onRetry}>
        <Press
          onPress={props.onRetry}
          accessibilityLabel="重试"
          hoverStyle={{ opacity: 0.9 }}
          style={{
            paddingHorizontal: 10,
            height: 26,
            borderRadius: radius.control,
            alignItems: "center",
            justifyContent: "center",
            ...glass.control,
          }}
        >
          <Text style={{ fontSize: 11, fontWeight: "600", color: colors.foreground }}>重试</Text>
        </Press>
      </If>
    </View>
  );
}

function CardTitle(props: { icon: IconName; title: string; note?: string }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
      <View
        style={{
          width: 26,
          height: 26,
          borderRadius: 8,
          alignItems: "center",
          justifyContent: "center",
          backgroundColor: colors.muted,
          borderWidth: 1,
          borderColor: colors.border,
        }}
      >
        <Icon name={props.icon} size={13} tint={colors.brand} />
      </View>
      <View style={{ flex: 1, gap: 1 }}>
        <Text style={{ fontSize: 13.5, fontWeight: "700", color: colors.foreground }}>{props.title}</Text>
        <If when={!!props.note}>
          <Text style={{ fontSize: 10.5, lineHeight: 15, color: colors.mutedForeground }}>{props.note ?? ""}</Text>
        </If>
      </View>
    </View>
  );
}

function SmallButton(props: { label: string; onPress?: () => void; disabled?: boolean; tone?: "ghost" | "primary" | "danger" }) {
  const tone = props.tone ?? "ghost";
  return (
    <Press
      onPress={props.disabled ? undefined : props.onPress}
      disabled={props.disabled}
      accessibilityLabel={props.label}
      hoverStyle={{ opacity: 0.9 }}
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: 5,
        height: 26,
        paddingHorizontal: 10,
        borderRadius: radius.pill,
        justifyContent: "center",
        backgroundColor:
          tone === "primary" ? colors.primary : tone === "danger" ? colors.dangerTint : colors.glassControl,
        borderWidth: tone === "ghost" ? 1 : 0,
        borderColor: colors.glassControlRim,
      }}
    >
      <Text
        style={{
          fontSize: 10.5,
          fontWeight: "600",
          color:
            tone === "primary"
              ? colors.primaryForeground
              : tone === "danger"
                ? colors.danger
                : colors.foreground,
        }}
      >
        {props.label}
      </Text>
    </Press>
  );
}

// ---- 模型 section ----------------------------------------------------------

function CurrentCard() {
  const settings = () => store.settings();
  return (
    <View style={{ borderRadius: radius.card, ...glass.card, padding: space.md, gap: space.sm }}>
      <CardTitle icon="spark" title="当前生效" note="配置保存后立即生效，无需重启；在飞的任务继续用旧配置跑完。" />
      <If when={store.settingsLoading() && !settings()}>
        <LoadingRow text="正在读取模型配置…" />
      </If>
      <If when={!!store.settingsError()}>
        <ErrorRow text={store.settingsError() ?? ""} onRetry={() => void store.loadModelsSettings()} />
      </If>
      <If when={!!settings()}>
        <View style={{ gap: 6 }}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
            <StateChip
              label={settings()?.source === "stored" ? "已保存覆盖" : "部署默认"}
              tone={settings()?.source === "stored" ? "brand" : "neutral"}
            />
            <Text style={{ fontSize: 11.5, color: colors.mutedForeground, fontVariant: ["tabular-nums"] }}>
              revision {settings()?.revision ?? 0}
            </Text>
            <SmallButton label="刷新" onPress={() => void store.loadModelsPanel()} />
          </View>
          <View style={{ flexDirection: "row", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
            <FieldLabel text="默认模型" />
            <If
              when={!!store.tenantSelection()}
              else={
                <Text style={{ fontSize: 11.5, color: colors.warning }}>未设置（文档里没有 selection）</Text>
              }
            >
              <Text style={{ fontSize: 12, fontWeight: "600", color: colors.foreground }}>
                {selectionLabel(store.tenantSelection())}
              </Text>
            </If>
          </View>
          <If when={!!store.settingsSavedSummary()}>
            <Text style={{ fontSize: 10.5, color: colors.success }}>
              最近一次写入：{store.settingsSavedSummary() ?? ""}
            </Text>
          </If>
          <Issues issues={() => settings()?.issues ?? []} label="解析诊断" />
          <If when={(settings()?.issues ?? []).length === 0}>
            <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>解析诊断：无</Text>
          </If>
        </View>
      </If>
    </View>
  );
}

function ReasoningChips() {
  const levels = (): string[] => {
    const selection = store.tenantSelection();
    if (!selection) return [];
    const model = store
      .models()
      .find((item) => item.modelId === selection.modelId && item.providerId === selection.providerId);
    return model?.options?.reasoningLevel?.values ?? [];
  };
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
      <FieldLabel text="思考档" />
      <If
        when={levels().length > 0}
        else={<Text style={{ fontSize: 11, color: colors.mutedForeground }}>选择模型后可选思考档</Text>}
      >
        <Each of={levels()} key="index">
          {(level) => (
            <Press
              onPress={() => {
                const selection = store.tenantSelection();
                if (!selection) return;
                writeSelection({ ...selection, reasoningLevel: level() });
              }}
              accessibilityLabel={`思考档 ${reasoningLabel(level())}`}
              accessibilityState={{ selected: store.tenantSelection()?.reasoningLevel === level() }}
              hoverStyle={{ opacity: 0.9 }}
              style={{
                paddingHorizontal: 9,
                height: 24,
                borderRadius: radius.pill,
                alignItems: "center",
                justifyContent: "center",
                backgroundColor:
                  store.tenantSelection()?.reasoningLevel === level() ? colors.brandTint : colors.muted,
                borderWidth: 1,
                borderColor:
                  store.tenantSelection()?.reasoningLevel === level() ? alpha(colors.brand, 0.4) : colors.glassEdge,
              }}
            >
              <Text
                style={{
                  fontSize: 11,
                  fontWeight: "600",
                  color:
                    store.tenantSelection()?.reasoningLevel === level() ? colors.brand : colors.mutedForeground,
                }}
              >
                {reasoningLabel(level())}
              </Text>
            </Press>
          )}
        </Each>
      </If>
    </View>
  );
}

function EditCard() {
  const defaultSelection = () => store.tenantSelection();
  const save = () => {
    const parsed = parseDocument(rawDocument());
    if (!parsed.document) {
      rawParseError.set(parsed.error ?? "文档不是合法的 JSON");
      return;
    }
    void store.saveModelsSettings(parsed.document).then(() => {
      // The server is authoritative after a write: on success the editor
      // follows the reloaded document; on failure the text stays as typed.
      if (store.settingsIssues().length === 0 && store.settingsConflict() === null) {
        rawDirty.set(false);
        confirmingReset.set(false);
      }
    });
  };
  return (
    <View style={{ borderRadius: radius.card, ...glass.card, padding: space.md, gap: space.sm }}>
      <CardTitle
        icon="layers"
        title="编辑"
        note="设置默认模型与思考档，或直接编辑原始文档。保存即校验：字段级错误原样呈现。"
      />
      <View style={{ flexDirection: "row", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
        <FieldLabel text="默认模型" />
        <If
          when={!!defaultSelection()}
          else={<Text style={{ fontSize: 11.5, color: colors.warning }}>未设置：在下方模型列表点选一个模型</Text>}
        >
          <Text style={{ fontSize: 12, fontWeight: "600", color: colors.foreground }}>
            {selectionLabel(defaultSelection())}
          </Text>
        </If>
      </View>
      <ReasoningChips />
      <If when={!!rawParseError()}>
        <View
          style={{
            gap: 3,
            padding: space.sm,
            borderRadius: radius.control,
            backgroundColor: colors.dangerTint,
            borderWidth: 1,
            borderColor: alpha(colors.danger, 0.3),
          }}
        >
          <Text style={{ fontSize: 11, fontWeight: "600", color: colors.danger }}>原始文档不是合法 JSON</Text>
          <Text style={{ fontSize: 11, lineHeight: 16, fontFamily: mono(), color: colors.foreground }}>
            {rawParseError() ?? ""}
          </Text>
        </View>
      </If>
      <If when={store.settingsIssues().length > 0}>
        <View
          style={{
            gap: 6,
            padding: space.sm,
            borderRadius: radius.control,
            backgroundColor: colors.dangerTint,
            borderWidth: 1,
            borderColor: alpha(colors.danger, 0.3),
          }}
        >
          <Text style={{ fontSize: 11.5, fontWeight: "600", color: colors.danger }}>
            保存被拒绝（{store.settingsIssues().length} 项）
          </Text>
          <Issues issues={() => store.settingsIssues()} />
        </View>
      </If>
      <If when={store.settingsConflict() !== null}>
        <View
          style={{
            gap: 6,
            padding: space.sm,
            borderRadius: radius.control,
            backgroundColor: colors.warningTint,
            borderWidth: 1,
            borderColor: alpha(colors.warning, 0.32),
          }}
        >
          <Text style={{ fontSize: 11.5, fontWeight: "600", color: colors.warning }}>
            文档已被其它写入更新（当前 revision {store.settingsConflict() ?? "?"}）
          </Text>
          <Text style={{ fontSize: 11, lineHeight: 16, color: colors.foreground }}>
            刷新以载入最新文档，再重新保存你的改动；本次修改没有被写入。
          </Text>
          <View style={{ flexDirection: "row" }}>
            <SmallButton
              label="刷新最新文档"
              onPress={() => {
                confirmingReset.set(false);
                void store.loadModelsPanel();
              }}
            />
          </View>
        </View>
      </If>
      <View style={{ flexDirection: "row", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        <Press
          onPress={store.settingsSaving() || !!rawParseError() || !rawDocument().trim() ? undefined : save}
          disabled={store.settingsSaving() || !!rawParseError() || !rawDocument().trim()}
          accessibilityLabel="保存模型配置"
          hoverStyle={{ opacity: 0.92 }}
          style={{
            flexDirection: "row",
            alignItems: "center",
            gap: 6,
            height: 32,
            paddingHorizontal: 14,
            borderRadius: radius.control,
            backgroundColor: colors.primary,
          }}
        >
          <If when={store.settingsSaving()} else={<Icon name="check" size={13} tint={colors.primaryForeground} />}>
            <Spinner size={12} tint={colors.primaryForeground} />
          </If>
          <Text style={{ fontSize: 12, fontWeight: "600", color: colors.primaryForeground }}>
            {store.settingsSaving() ? "保存中…" : "保存并生效"}
          </Text>
        </Press>
        <If
          when={confirmingReset()}
          else={
            <Press
              onPress={store.settingsSaving() ? undefined : () => confirmingReset.set(true)}
              disabled={store.settingsSaving()}
              accessibilityLabel="重置为部署默认"
              hoverStyle={{ opacity: 0.9 }}
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: 6,
                height: 32,
                paddingHorizontal: 12,
                borderRadius: radius.control,
                backgroundColor: colors.dangerTint,
                borderWidth: 1,
                borderColor: alpha(colors.danger, 0.32),
              }}
            >
              <Icon name="refresh" size={13} tint={colors.danger} />
              <Text style={{ fontSize: 12, fontWeight: "600", color: colors.danger }}>重置</Text>
            </Press>
          }
        >
          <View style={{ flexDirection: "row", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
            <Text style={{ fontSize: 11.5, color: colors.danger }}>重置会删除已保存的覆盖，回到部署默认；确定吗？</Text>
            <Press
              onPress={() => {
                confirmingReset.set(false);
                void store.resetModelsSettings().then(() => {
                  if (store.settingsConflict() === null) rawDirty.set(false);
                });
              }}
              accessibilityLabel="确认重置"
              hoverStyle={{ opacity: 0.9 }}
              style={{
                height: 28,
                paddingHorizontal: 12,
                borderRadius: radius.control,
                alignItems: "center",
                justifyContent: "center",
                backgroundColor: colors.destructive,
              }}
            >
              <Text style={{ fontSize: 11.5, fontWeight: "600", color: colors.destructiveForeground }}>
                确认重置
              </Text>
            </Press>
            <Press
              onPress={() => confirmingReset.set(false)}
              accessibilityLabel="取消重置"
              hoverStyle={{ opacity: 0.9 }}
              style={{
                height: 28,
                paddingHorizontal: 12,
                borderRadius: radius.control,
                alignItems: "center",
                justifyContent: "center",
                ...glass.control,
              }}
            >
              <Text style={{ fontSize: 11.5, fontWeight: "600", color: colors.foreground }}>取消</Text>
            </Press>
          </View>
        </If>
        <View style={{ flex: 1 }} />
        <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>
          {rawDirty() ? "有未保存的修改" : "编辑区与当前生效配置一致"}
        </Text>
      </View>
      <View style={{ gap: 4 }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
          <FieldLabel text="原始文档" />
          <Text style={{ fontSize: 10, color: colors.mutedForeground }}>
            完整 models 文档（schemaVersion / providers / 规则数组 / selection）
          </Text>
        </View>
        <Input
          testID="settings-raw-document"
          value={rawDocument()}
          onChange={({ value }) => {
            rawDocument.set(value);
            rawDirty.set(true);
            rawParseError.set(parseDocument(value).error ?? null);
          }}
          multiline
          submitBehavior="newline"
          placeholder={'{\n  "schemaVersion": 1,\n  "providers": [],\n  "selection": {}\n}'}
          style={{
            minHeight: compact() ? 180 : 240,
            fontSize: 11.5,
            lineHeight: 16,
            fontFamily: mono(),
            color: colors.foreground,
            placeholderColor: colors.mutedForeground,
            caretColor: colors.brand,
            selectionColor: alpha(colors.brand, 0.3),
            paddingHorizontal: 10,
            paddingVertical: 8,
            borderRadius: radius.control,
            borderWidth: 1,
            borderColor: rawParseError() ? colors.danger : colors.border,
            backgroundColor: colors.background,
          }}
        />
        <Text style={{ fontSize: 10, color: colors.mutedForeground }}>
          保存时整份文档提交（带 expectedRevision）；服务端校验失败会按字段路径列出原因。
        </Text>
      </View>
    </View>
  );
}

function ModelRow(props: { model: () => ModelView }) {
  const tenant = () => store.tenantSelection();
  const isDefault = () =>
    tenant()?.modelId === props.model().modelId && tenant()?.providerId === props.model().providerId;
  const choose = () => {
    const model = props.model();
    const current = selectionOfDocument(store.settings()?.document ?? {});
    writeSelection({
      providerId: model.providerId,
      modelId: model.modelId,
      reasoningLevel: defaultReasoningFor(model, current),
    });
  };
  return (
    <Press
      onPress={props.model().selectable === false ? undefined : choose}
      disabled={props.model().selectable === false}
      accessibilityLabel={`设为默认 ${props.model().providerId}/${props.model().modelId}`}
      hoverStyle={{ backgroundColor: colors.hover }}
      style={{
        gap: 4,
        padding: space.sm,
        borderRadius: radius.control,
        borderWidth: 1,
        borderColor: isDefault() ? alpha(colors.brand, 0.4) : colors.glassEdge,
        backgroundColor: isDefault() ? colors.brandTint : undefined,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        <Text style={{ fontSize: 12.5, fontWeight: "600", color: colors.foreground }}>
          {props.model().providerId}/{props.model().modelId}
        </Text>
        <If when={isDefault()}>
          <StateChip label="当前默认" tone="brand" />
        </If>
        <If when={props.model().selectable === false}>
          <StateChip label="不可选" tone="warning" />
        </If>
        <If when={props.model().enabled === false}>
          <StateChip label="已禁用" tone="neutral" />
        </If>
        <View style={{ flex: 1 }} />
        <If when={props.model().selectable !== false}>
          <Text
            style={{
              fontSize: 10.5,
              fontWeight: "600",
              color: isDefault() ? colors.brand : colors.mutedForeground,
            }}
          >
            {isDefault() ? "已选为默认" : "设为默认"}
          </Text>
        </If>
      </View>
      <Text style={{ fontSize: 10.5, lineHeight: 15, color: colors.mutedForeground }}>
        {modelFacts(props.model())}
      </Text>
      <Issues issues={() => props.model().issues ?? []} />
    </Press>
  );
}

function ModelsList() {
  const models = () => store.models();
  return (
    <View style={{ borderRadius: radius.card, ...glass.card, padding: space.md, gap: space.sm }}>
      <CardTitle
        icon="brain"
        title="模型列表"
        note="来自 /v1/models：只含凭据可解析的可见 provider；可选的模型可以点选为默认。"
      />
      <If when={!!store.modelsError()}>
        <ErrorRow text={store.modelsError() ?? ""} onRetry={() => void store.loadRegistry()} />
      </If>
      <If when={!store.modelsError() && !store.registryLoaded()}>
        <LoadingRow text="正在读取模型列表…" />
      </If>
      <If when={!store.modelsError() && store.registryLoaded() && models().length === 0}>
        <Text style={{ fontSize: 12, color: colors.mutedForeground }}>
          没有可用的模型：当前进程解析不到任何 provider 的凭据，或配置为空。
        </Text>
      </If>
      <Each of={models()} key={(model) => `${model.providerId}/${model.modelId}`}>
        {(model) => <ModelRow model={model} />}
      </Each>
      <Issues issues={() => store.modelIssues()} label="列表诊断" />
      <If when={!!store.enforcement()}>
        <Text style={{ fontSize: 10, color: colors.mutedForeground }}>
          能力执行门：工具调用 {store.enforcement()?.toolCall ? "强制" : "未强制"} · 媒体{" "}
          {store.enforcement()?.media ? "强制" : "记录但未强制"}
        </Text>
      </If>
    </View>
  );
}

function ProviderRow(props: { provider: () => ProviderView }) {
  const accessLabel = () => {
    const parts: string[] = [];
    if (props.provider().access?.env) parts.push(`env ${props.provider().access?.env}`);
    if (props.provider().access?.ref) parts.push(`ref ${props.provider().access?.ref}`);
    return parts.join(" · ") || "无凭据引用";
  };
  const modelIssues = () => (props.provider().models ?? []).filter((model) => (model.issues ?? []).length > 0);
  return (
    <View
      style={{
        gap: 4,
        padding: space.sm,
        borderRadius: radius.control,
        borderWidth: 1,
        borderColor: colors.glassEdge,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        <Text style={{ fontSize: 12.5, fontWeight: "600", color: colors.foreground }}>
          {props.provider().name
            ? `${props.provider().name} · ${props.provider().providerId}`
            : props.provider().providerId}
        </Text>
        <StateChip
          label={props.provider().executable ? "可执行" : "不可执行"}
          tone={props.provider().executable ? "success" : "danger"}
        />
        <StateChip label={`${(props.provider().models ?? []).length} 个模型`} tone="neutral" />
      </View>
      <Text style={{ fontSize: 10.5, fontFamily: mono(), color: colors.mutedForeground }}>
        {accessLabel()} · {props.provider().api?.type ?? ""} {props.provider().api?.baseUrl ?? ""}
      </Text>
      <Issues issues={() => props.provider().issues ?? []} />
      <Each of={modelIssues()} key={(model) => `${model.providerId}/${model.modelId}`}>
        {(model) => (
          <View style={{ gap: 3 }}>
            <Text style={{ fontSize: 10.5, fontFamily: mono(), color: colors.mutedForeground }}>
              {model().providerId}/{model().modelId}
            </Text>
            <Issues issues={() => model().issues ?? []} />
          </View>
        )}
      </Each>
    </View>
  );
}

function ProvidersList() {
  return (
    <View style={{ borderRadius: radius.card, ...glass.card, padding: space.md, gap: space.sm }}>
      <CardTitle
        icon="layers"
        title="Provider 列表"
        note="来自 /v1/providers（只含可见 provider）：凭据状态、可执行性与逐条诊断。"
      />
      <If when={!!store.providersError()}>
        <ErrorRow text={store.providersError() ?? ""} onRetry={() => void store.loadRegistry()} />
      </If>
      <If when={!store.providersError() && !store.registryLoaded()}>
        <LoadingRow text="正在读取 provider…" />
      </If>
      <If when={!store.providersError() && store.registryLoaded() && store.providers().length === 0}>
        <Text style={{ fontSize: 12, color: colors.mutedForeground }}>没有可见的 provider。</Text>
      </If>
      <Each of={store.providers()} key={(provider) => provider.providerId}>
        {(provider) => <ProviderRow provider={provider} />}
      </Each>
      <Issues issues={() => store.providersIssues()} label="解析诊断" />
    </View>
  );
}

function ModelsSection() {
  // The editor follows the loaded document until the user types in it; every
  // successful write and reload makes the server's document authoritative.
  effect(() => {
    const loaded = store.settings();
    if (!loaded) return;
    if (!rawDirty()) rawDocument.set(JSON.stringify(loaded.document, null, 2));
  });
  return (
    <View style={{ gap: space.md }}>
      <CurrentCard />
      <EditCard />
      <ModelsList />
      <ProvidersList />
    </View>
  );
}

// ---- 密钥 section -----------------------------------------------------------

function SecretRow(props: { secret: () => SecretMeta }) {
  const confirming = () => confirmingSecretDelete() === props.secret().name;
  return (
    <View
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: 8,
        padding: space.sm,
        borderRadius: radius.control,
        borderWidth: 1,
        borderColor: confirming() ? alpha(colors.danger, 0.36) : colors.glassEdge,
        flexWrap: "wrap",
      }}
    >
      <Icon name="key" size={13} tint={colors.mutedForeground} />
      <View style={{ flexDirection: "row", alignItems: "baseline", gap: 4 }}>
        <Text style={{ fontSize: 12.5, fontWeight: "600", color: colors.foreground }}>{props.secret().name}</Text>
        <Text style={{ fontSize: 10, color: colors.mutedForeground }}>secret:{props.secret().name}</Text>
      </View>
      <View style={{ flex: 1 }} />
      <Text style={{ fontSize: 10, color: colors.mutedForeground }}>
        创建 {formatTime(props.secret().createdAt)} · 更新 {formatTime(props.secret().updatedAt)}
      </Text>
      <If
        when={confirming()}
        else={
          <SmallButton
            label="删除"
            tone="danger"
            disabled={store.secretsBusy()}
            onPress={() => confirmingSecretDelete.set(props.secret().name)}
          />
        }
      >
        <View style={{ flexDirection: "row", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
          <Text style={{ fontSize: 10.5, color: colors.danger }}>删除 {props.secret().name}？</Text>
          <Press
            onPress={() => {
              confirmingSecretDelete.set(null);
              void store.deleteSecret(props.secret().name);
            }}
            accessibilityLabel={`确认删除密钥 ${props.secret().name}`}
            hoverStyle={{ opacity: 0.9 }}
            style={{
              height: 24,
              paddingHorizontal: 9,
              borderRadius: radius.control,
              alignItems: "center",
              justifyContent: "center",
              backgroundColor: colors.destructive,
            }}
          >
            <Text style={{ fontSize: 10.5, fontWeight: "600", color: colors.destructiveForeground }}>
              确认删除
            </Text>
          </Press>
          <Press
            onPress={() => confirmingSecretDelete.set(null)}
            accessibilityLabel={`取消删除密钥 ${props.secret().name}`}
            hoverStyle={{ opacity: 0.9 }}
            style={{
              height: 24,
              paddingHorizontal: 9,
              borderRadius: radius.control,
              alignItems: "center",
              justifyContent: "center",
              ...glass.control,
            }}
          >
            <Text style={{ fontSize: 10.5, fontWeight: "600", color: colors.foreground }}>取消</Text>
          </Press>
        </View>
      </If>
    </View>
  );
}

function SecretsSection() {
  const available = () => store.secretsAvailable();
  const isOverwrite = () =>
    store.secrets().some((item) => item.name === secretName().trim() && !!secretName().trim());
  const submit = () => {
    const name = secretName().trim();
    const value = secretValue();
    if (!name || !value) {
      store.flash({ tone: "warn", text: "密钥的名字和值都不能为空" });
      return;
    }
    void store.saveSecret(name, value).then((ok) => {
      if (!ok) return;
      // The value is done the moment it is accepted: clear it, keep nothing.
      secretValue.set("");
      confirmingSecretDelete.set(null);
    });
  };
  return (
    <View style={{ gap: space.md }}>
      <View style={{ borderRadius: radius.card, ...glass.card, padding: space.md, gap: space.sm }}>
        <CardTitle
          icon="key"
          title="密钥"
          note={'按名字写入/覆盖/删除；值只写不回读（响应、日志、列表都不含它）。模型配置用 access.ref = "secret:NAME" 引用。'}
        />
        <If when={!available()}>
          <View
            style={{
              gap: 4,
              padding: space.sm,
              borderRadius: radius.control,
              backgroundColor: colors.warningTint,
              borderWidth: 1,
              borderColor: alpha(colors.warning, 0.32),
            }}
          >
            <Text style={{ fontSize: 11.5, fontWeight: "600", color: colors.warning }}>密钥存储未启用</Text>
            <Text style={{ fontSize: 11, lineHeight: 16, color: colors.foreground }}>
              {store.secretsReason() || "服务端没有配置 KEEL_SECRET_KEY（密钥盒）。"}
            </Text>
            <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>
              不会回退为明文存储；配置密钥盒后这里即可写入。
            </Text>
          </View>
        </If>
        <If when={store.secretsLoading()}>
          <LoadingRow text="正在读取密钥…" />
        </If>
        <If when={!!store.secretsError()}>
          <ErrorRow text={store.secretsError() ?? ""} onRetry={() => void store.loadSecrets()} />
        </If>
        <If when={!store.secretsError() && !store.secretsLoading() && store.secrets().length === 0}>
          <Text style={{ fontSize: 12, color: colors.mutedForeground }}>还没有密钥。</Text>
        </If>
        <Each of={store.secrets()} key={(secret) => secret.name}>
          {(secret) => <SecretRow secret={secret} />}
        </Each>
        <View style={{ gap: 6, paddingTop: 4 }}>
          <FieldLabel text={available() ? "写入 / 覆盖" : "写入（不可用）"} />
          <View style={{ flexDirection: "row", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
            <Input
              testID="secret-name"
              value={secretName()}
              onChange={({ value }) => secretName.set(value)}
              placeholder="名字，例如 s2api"
              autoCapitalize="none"
              autoCorrect={false}
              maxLength={128}
              style={{
                width: compact() ? "100%" : 200,
                height: 34,
                fontSize: 12.5,
                color: colors.foreground,
                placeholderColor: colors.mutedForeground,
                caretColor: colors.brand,
                paddingHorizontal: 10,
                borderRadius: radius.control,
                borderWidth: 1,
                borderColor: colors.border,
                backgroundColor: colors.background,
                opacity: available() ? 1 : 0.38,
              }}
            />
            <Input
              testID="secret-value"
              value={secretValue()}
              onChange={({ value }) => secretValue.set(value)}
              placeholder="值（保存后不再显示）"
              masked
              autoCapitalize="none"
              autoCorrect={false}
              maxLength={4096}
              style={{
                width: compact() ? "100%" : 260,
                height: 34,
                fontSize: 12.5,
                color: colors.foreground,
                placeholderColor: colors.mutedForeground,
                caretColor: colors.brand,
                paddingHorizontal: 10,
                borderRadius: radius.control,
                borderWidth: 1,
                borderColor: colors.border,
                backgroundColor: colors.background,
                opacity: available() ? 1 : 0.38,
              }}
            />
            <Press
              onPress={available() && !store.secretsBusy() ? submit : undefined}
              disabled={!available() || store.secretsBusy() || !secretName().trim() || !secretValue()}
              accessibilityLabel="保存密钥"
              hoverStyle={{ opacity: 0.92 }}
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: 6,
                height: 34,
                paddingHorizontal: 14,
                borderRadius: radius.control,
                backgroundColor: colors.primary,
              }}
            >
              <If
                when={store.secretsBusy()}
                else={<Icon name="check" size={13} tint={colors.primaryForeground} />}
              >
                <Spinner size={12} tint={colors.primaryForeground} />
              </If>
              <Text style={{ fontSize: 12, fontWeight: "600", color: colors.primaryForeground }}>
                {store.secretsBusy() ? "写入中…" : isOverwrite() ? "覆盖保存" : "保存密钥"}
              </Text>
            </Press>
          </View>
          <If when={!!store.secretsActionError()}>
            <Text style={{ fontSize: 11.5, color: colors.danger }}>{store.secretsActionError() ?? ""}</Text>
          </If>
          <Text style={{ fontSize: 10, color: colors.mutedForeground }}>
            保存后输入框清空；之后的界面只显示名字与时间。
          </Text>
        </View>
      </View>
    </View>
  );
}

// ---- 插件 section -----------------------------------------------------------

const pluginStateTone = (state: string | undefined): "success" | "danger" | "neutral" | "warning" =>
  state === "ready" ? "success" : state === "failed" ? "danger" : state === "stopped" ? "neutral" : "warning";

function PluginRow(props: { plugin: () => PluginStatusView }) {
  return (
    <View
      style={{
        gap: 5,
        padding: space.sm,
        borderRadius: radius.control,
        borderWidth: 1,
        borderColor: colors.glassEdge,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        <Text style={{ fontSize: 12.5, fontWeight: "600", color: colors.foreground }}>{props.plugin().id}</Text>
        <If when={!!props.plugin().version}>
          <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>v{props.plugin().version}</Text>
        </If>
        <StateChip label={props.plugin().state ?? "unknown"} tone={pluginStateTone(props.plugin().state)} />
        <View style={{ flex: 1 }} />
        <Text style={{ fontSize: 10, color: colors.mutedForeground }}>
          {props.plugin().startedAt ? `启动 ${formatTime(props.plugin().startedAt)}` : ""}
          {props.plugin().stoppedAt ? ` · 停止 ${formatTime(props.plugin().stoppedAt)}` : ""}
        </Text>
      </View>
      <If when={(props.plugin().provides ?? []).length > 0 || (props.plugin().requires ?? []).length > 0}>
        <Text style={{ fontSize: 10.5, color: colors.mutedForeground }}>
          提供 {(props.plugin().provides ?? []).join("/") || "—"} · 依赖{" "}
          {(props.plugin().requires ?? []).join("/") || "无"}
        </Text>
      </If>
      <If when={(props.plugin().tools ?? []).length > 0}>
        <Text style={{ fontSize: 10.5, fontFamily: mono(), color: colors.mutedForeground }}>
          工具：{(props.plugin().tools ?? []).join(" · ")}
        </Text>
      </If>
      <If when={(props.plugin().hooks ?? []).length > 0}>
        <Text style={{ fontSize: 10.5, fontFamily: mono(), color: colors.mutedForeground }}>
          hook：{(props.plugin().hooks ?? []).join(" · ")}
        </Text>
      </If>
      <If when={(props.plugin().triggers ?? []).length > 0}>
        <Text style={{ fontSize: 10.5, fontFamily: mono(), color: colors.mutedForeground }}>
          触发器：{(props.plugin().triggers ?? []).map((trigger) => trigger.id).join(" · ")}
        </Text>
      </If>
      <Each of={props.plugin().diagnostics ?? []} key="index">
        {(line) => (
          <View style={{ flexDirection: "row", alignItems: "flex-start", gap: 6 }}>
            <Icon name="alert" size={11} tint={colors.warning} />
            <Text style={{ flex: 1, fontSize: 10.5, lineHeight: 15, fontFamily: mono(), color: colors.warning }}>
              {line()}
            </Text>
          </View>
        )}
      </Each>
    </View>
  );
}

function TriggerRow(props: { trigger: () => TriggerStatusView }) {
  const firing = () => store.firingTrigger() === props.trigger().id;
  const outcome = () => (store.fireOutcome()?.triggerId === props.trigger().id ? store.fireOutcome() : null);
  const canFire = () => props.trigger().enabled !== false;
  return (
    <View
      style={{
        gap: 5,
        padding: space.sm,
        borderRadius: radius.control,
        borderWidth: 1,
        borderColor: colors.glassEdge,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        <Icon name="clock" size={12} tint={colors.mutedForeground} />
        <Text style={{ fontSize: 12, fontWeight: "600", color: colors.foreground }}>{props.trigger().id}</Text>
        <StateChip label={props.trigger().kind ?? "trigger"} tone="neutral" />
        <If when={props.trigger().enabled === false}>
          <StateChip label="已禁用" tone="warning" />
        </If>
        <View style={{ flex: 1 }} />
        <Text style={{ fontSize: 10, color: colors.mutedForeground, fontVariant: ["tabular-nums"] }}>
          {props.trigger().fires ?? 0} 次触发 · {props.trigger().failures ?? 0} 失败
        </Text>
        <Press
          testID={`trigger-fire-${props.trigger().id}`}
          onPress={canFire() && !firing() ? () => void store.fireTrigger(props.trigger().id) : undefined}
          disabled={!canFire() || firing()}
          accessibilityLabel={`手动触发 ${props.trigger().id}`}
          hoverStyle={{ opacity: 0.92 }}
          style={{
            flexDirection: "row",
            alignItems: "center",
            gap: 5,
            height: 26,
            paddingHorizontal: 10,
            borderRadius: radius.pill,
            backgroundColor: canFire() ? colors.brandTint : colors.muted,
          }}
        >
          <If
            when={firing()}
            else={<Icon name="play" size={11} tint={canFire() ? colors.brand : colors.mutedForeground} />}
          >
            <Spinner size={11} tint={colors.brand} />
          </If>
          <Text
            style={{
              fontSize: 10.5,
              fontWeight: "600",
              color: canFire() ? colors.brand : colors.mutedForeground,
            }}
          >
            {firing() ? "触发中…" : "触发"}
          </Text>
        </Press>
      </View>
      <Text style={{ fontSize: 10.5, lineHeight: 15, color: colors.mutedForeground }}>
        {props.trigger().schedule ? `计划 ${props.trigger().schedule} · ` : ""}
        {props.trigger().prompt ? `提示「${props.trigger().prompt}」 · ` : ""}
        上次 {formatTime(props.trigger().lastFireAt) || "未触发"}
        {props.trigger().lastStatus ? `（${props.trigger().lastStatus}）` : ""}
        {props.trigger().nextFireAt ? ` · 下次 ${formatTime(props.trigger().nextFireAt)}` : ""}
      </Text>
      <If when={!!outcome()}>
        <Text
          style={{
            fontSize: 11,
            lineHeight: 16,
            fontFamily: mono(),
            color: outcome()?.ok ? colors.success : colors.danger,
          }}
        >
          {outcome()?.ok ? "触发结果：" : "触发失败："}
          {outcome()?.text ?? ""}
        </Text>
      </If>
    </View>
  );
}

function PluginsSection() {
  const plugins = () => store.plugins()?.plugins ?? [];
  const hooks = () => store.plugins()?.hookStats ?? store.plugins()?.hooks;
  return (
    <View style={{ gap: space.md }}>
      <View style={{ borderRadius: radius.card, ...glass.card, padding: space.md, gap: space.sm }}>
        <CardTitle
          icon="layers"
          title="插件清单"
          note="插件在进程启动时装载并与依赖一起装配；这里看状态与控制触发，不做代码级配置。"
        />
        <If when={store.pluginsLoading()}>
          <LoadingRow text="正在读取插件清单…" />
        </If>
        <If when={!!store.pluginsError()}>
          <ErrorRow text={store.pluginsError() ?? ""} onRetry={() => void store.loadPlugins()} />
        </If>
        <If when={!store.pluginsError() && !store.pluginsLoading()}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
            <StateChip label={`generation ${store.plugins()?.generation ?? "-"}`} tone="brand" />
            <Text style={{ fontSize: 11, color: colors.mutedForeground }}>
              {(store.plugins()?.tools ?? []).length} 个工具 · {plugins().length} 个插件
            </Text>
            <SmallButton label="刷新" onPress={() => void store.loadPlugins()} />
          </View>
          <If when={!!hooks()}>
            <Text style={{ fontSize: 10.5, color: colors.mutedForeground, fontVariant: ["tabular-nums"] }}>
              hook 订阅 {hooks()?.subscriptions ?? 0} · 发出 {hooks()?.emitted ?? 0} · 投递{" "}
              {hooks()?.delivered ?? 0} · 丢弃 {hooks()?.dropped ?? 0} · 失败 {hooks()?.failed ?? 0} · 队列{" "}
              {hooks()?.queued ?? 0}/{hooks()?.capacity ?? 0}
            </Text>
          </If>
          <If when={(store.plugins()?.tools ?? []).length > 0}>
            <Text style={{ fontSize: 10.5, lineHeight: 15, fontFamily: mono(), color: colors.mutedForeground }}>
              工具：{(store.plugins()?.tools ?? []).join(" · ")}
            </Text>
          </If>
          <If when={plugins().length === 0}>
            <Text style={{ fontSize: 12, color: colors.mutedForeground }}>没有已装载的插件。</Text>
          </If>
        </If>
        <Each of={plugins()} key={(plugin) => plugin.id}>
          {(plugin) => <PluginRow plugin={plugin} />}
        </Each>
      </View>

      <View style={{ borderRadius: radius.card, ...glass.card, padding: space.md, gap: space.sm }}>
        <CardTitle
          icon="clock"
          title="触发器"
          note="来自 /v1/triggers：手动触发以 manual 来源记录审计；被禁用的触发器无法触发。"
        />
        <If when={!store.pluginsError() && !store.pluginsLoading() && store.triggers().length === 0}>
          <Text style={{ fontSize: 12, color: colors.mutedForeground }}>没有注册的触发器。</Text>
        </If>
        <Each of={store.triggers()} key={(trigger) => trigger.id}>
          {(trigger) => <TriggerRow trigger={trigger} />}
        </Each>
      </View>
    </View>
  );
}

// ---- the page ---------------------------------------------------------------

function SectionTabs() {
  const options: Array<{ value: SettingsSection; label: string; icon: IconName }> = [
    { value: "models", label: "模型", icon: "brain" },
    { value: "secrets", label: "密钥", icon: "key" },
    { value: "plugins", label: "插件", icon: "layers" },
  ];
  return (
    <View
      style={{
        flexDirection: "row",
        padding: 3,
        gap: 2,
        borderRadius: radius.control,
        backgroundColor: colors.muted,
        borderWidth: 1,
        borderColor: colors.glassEdge,
      }}
    >
      <Each of={options} key={(option) => option.value}>
        {(option) => (
          <Press
            onPress={() => settingsSection.set(option().value)}
            accessibilityLabel={`设置分区 ${option().label}`}
            accessibilityState={{ selected: settingsSection() === option().value }}
            hoverStyle={{ backgroundColor: colors.hover }}
            style={{
              flexDirection: "row",
              alignItems: "center",
              gap: 5,
              height: 28,
              paddingHorizontal: 12,
              borderRadius: radius.control - 3,
              backgroundColor: settingsSection() === option().value ? colors.glassControl : undefined,
              boxShadow:
                settingsSection() === option().value ? `inset 0 1px 0 ${colors.glassControlRim}` : undefined,
            }}
          >
            <Icon
              name={option().icon}
              size={12}
              tint={settingsSection() === option().value ? colors.foreground : colors.mutedForeground}
            />
            <Text
              style={{
                fontSize: 12,
                fontWeight: "600",
                color: settingsSection() === option().value ? colors.foreground : colors.mutedForeground,
              }}
            >
              {option().label}
            </Text>
          </Press>
        )}
      </Each>
    </View>
  );
}

/** The standalone settings page. Rendered by the app shell when the route is
 * `settings`; the workspace (and its open session) waits underneath. */
/**
 * The settings page edits server state, so it must say when there is no server
 * behind it: fixture mode is a demo, never the product's answer. The strip sits
 * above every section and offers the honest way back — probe again.
 */
function DataSourceStrip() {
  return (
    <If when={store.mode() !== "http"}>
      <View
        style={{
          flexDirection: "row",
          alignItems: "center",
          gap: space.sm,
          paddingHorizontal: space.md,
          paddingVertical: 7,
          backgroundColor: store.mode() === "checking" ? colors.infoTint : colors.warningTint,
          borderBottomWidth: 1,
          borderBottomColor:
            store.mode() === "checking" ? alpha(colors.info, 0.32) : alpha(colors.warning, 0.32),
        }}
      >
        <If
          when={store.mode() === "checking"}
          else={<Icon name="alert" size={13} tint={colors.warning} />}
        >
          <Spinner size={12} tint={colors.info} />
        </If>
        <Text style={{ flex: 1, fontSize: 11.5, color: colors.foreground }}>
          {store.mode() === "checking"
            ? "正在探测 keel serve…连接后本页显示服务端的真实配置。"
            : "演示数据 · 未连接服务：以下内容来自本地 fixture，不是服务端的真实状态。"}
        </Text>
        <If when={store.mode() === "fixture"}>
          <Press
            onPress={() => void store.reconnect()}
            accessibilityLabel="重试连接服务"
            hoverStyle={{ opacity: 0.88 }}
            style={{
              paddingHorizontal: 10,
              height: 22,
              borderRadius: radius.pill,
              backgroundColor: colors.warning,
              alignItems: "center",
              justifyContent: "center",
            }}
          >
            <Text style={{ fontSize: 10, fontWeight: "600", color: colors.primaryForeground }}>
              重试连接
            </Text>
          </Press>
        </If>
      </View>
    </If>
  );
}

export function SettingsScreen() {
  const load = () => {
    const section = settingsSection();
    if (section === "models") void store.loadModelsPanel();
    else if (section === "secrets") void store.loadSecrets();
    else void store.loadPlugins();
  };
  // Each section loads what it shows; switching sections (or re-entering the
  // page) refreshes that section. A booting probe is waited for, not raced:
  // the effect re-runs when `mode` settles, which is when the transport the
  // load should use actually exists. The load goes out of the effect's own
  // turn: a mount-time load sets its loading signals synchronously, and
  // writing them while the tree is still mounting must not happen inside the
  // effect.
  effect(() => {
    if (store.route() !== "settings") return;
    if (store.mode() === "checking") return;
    settingsSection();
    const timer = setTimeout(load, 0);
    cleanup(() => clearTimeout(timer));
  });
  return (
    <View style={{ flex: 1, backgroundColor: colors.background }}>
      <View
        style={{
          flexDirection: "row",
          alignItems: "center",
          gap: space.sm,
          minHeight: 48,
          paddingHorizontal: space.md,
          paddingVertical: compact() ? 6 : 0,
          borderBottomWidth: 1,
          borderBottomColor: colors.headerBorder,
          backgroundColor: colors.background,
          flexWrap: "wrap",
          zIndex: 4,
        }}
      >
        <Press
          onPress={() => store.closeSettings()}
          accessibilityLabel="返回工作区"
          hoverStyle={{ backgroundColor: colors.hover }}
          style={{
            flexDirection: "row",
            alignItems: "center",
            gap: 4,
            height: 30,
            paddingHorizontal: 8,
            borderRadius: radius.control,
          }}
        >
          <Icon name="chevron-left" size={16} tint={colors.mutedForeground} />
          <Text style={{ fontSize: 12, fontWeight: "600", color: colors.mutedForeground }}>工作区</Text>
        </Press>
        <View style={{ gap: 1 }}>
          <Text style={{ fontSize: 15, fontWeight: "700", color: colors.foreground }}>设置</Text>
          <Text style={{ fontSize: 10, color: colors.mutedForeground }}>
            模型 / 密钥 / 插件 · 独立设置页{isTerminal() ? "" : " · /settings"}
          </Text>
        </View>
        <View style={{ flex: 1 }} />
        <SectionTabs />
        <Press
          onPress={load}
          accessibilityLabel="刷新设置分区"
          hoverStyle={{ backgroundColor: colors.hover }}
          style={{
            width: 30,
            height: 30,
            borderRadius: radius.control,
            alignItems: "center",
            justifyContent: "center",
          }}
        >
          <Icon name="refresh" size={15} tint={colors.mutedForeground} />
        </Press>
      </View>
      <DataSourceStrip />
      <Scroll style={{ flex: 1 }} contentStyle={{ alignItems: "center" }}>
        <View
          style={{
            width: "100%",
            maxWidth: 880,
            padding: compact() ? space.md : space.lg,
            paddingBottom: space.xxl,
            gap: space.md,
          }}
        >
          <If
            when={settingsSection() === "models"}
            else={
              <If when={settingsSection() === "secrets"} else={<PluginsSection />}>
                <SecretsSection />
              </If>
            }
          >
            <ModelsSection />
          </If>
        </View>
      </Scroll>
    </View>
  );
}
