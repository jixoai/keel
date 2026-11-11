// The settings page (change keel-harness-shell, tasks G1–G5).
//
// The frame is the kit's `SettingsShell`: the icon rail (68px, 268px from the
// wide breakpoint), the h-12 breadcrumb row, the content card and the
// max-w-4xl content column with its 32px section rhythm all belong to that
// block (`web/src/ui/kit/layout/settings-shell.tsx`). This file draws none of
// them.
//
// Seven sections, in the rail's order:
//   · 通用       the token-level partial switch (moved here from the session
//                list: it is a reading preference, not a session action).
//   · 外观       follow the system / light / dark, written through
//                `applyAppearance` (`src/ui/appearance.ts`), which is also
//                what the app root reads at boot — one source, so the page and
//                the boot agree.
//   · 模型接入   the effective document, its resolved providers/models,
//                a structured form over the document's default selection and
//                each provider's template / endpoint / credential reference,
//                and the raw document behind one collapsed「高级」disclosure.
//   · 密钥       names and times only: values go one way (write-only) and are
//                never read back, echoed or kept.
//   · 插件       the assembled extension surface (generation, plugin states,
//                tools, hooks, triggers) with a diagnostics table that stays
//                collapsed, and the entry to the plugin page (`/plugins`).
//   · 快捷键     the key vocabulary, as keycaps.
//   · 关于       keel.info's version / online / user agent plus the build tag.
//
// The section is *controlled*: this file owns the signal and its storage
// (`SETTINGS_SECTION_STORAGE_KEY`, read on mount and written on every move),
// the shell only renders the rail (`settings-shell.tsx` persists only its own
// uncontrolled state). The page opens on 模型接入 — the section the previous
// settings page was — until a remembered section replaces it.
//
// Three confirmations ask through the kit's `AlertDialog`: the model reset and
// the secret delete live here (the session delete is the session list's).
//
// NSH components run once: every reactive read sits inside JSX, an `<If>` or a
// function the JSX calls; nothing is copied into a local at the top of a
// component.
import type { JSX } from "nsh";
import { cleanup, colorScheme, effect, host, signal, Storage, Text, View } from "nsh";
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
import {
  displayNameOf,
  formatTime,
  modelFacts,
  pluginStateLabel,
  providerDisplayName,
  reasoningLabel,
  selectionDisplay,
} from "../data/wire";
import { applyAppearance, appearancePreference, type Appearance } from "../ui/appearance";
import { Icon } from "../ui/icons";
import {
  AlertDialog,
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  Collapsible,
  CollapsibleTrigger,
  Empty,
  EmptyDescription,
  EmptyTitle,
  Field,
  FieldGroup,
  Input,
  Kbd,
  Label,
  RadioGroup,
  Select,
  Separator,
  SETTINGS_SECTION_STORAGE_KEY,
  SettingsShell,
  Skeleton,
  Spinner,
  Switch,
  Table,
  TableBody,
  TableBodyRow,
  TableHeaderRow,
  Td,
  Textarea,
  Th,
  alpha,
  color,
  font,
  iconSize,
  lineHeight,
  radius,
  space,
  typeScale,
  weight,
  type SelectItem,
  type SettingsSectionEntry,
} from "../ui/kit";
import { defaultReasoningFor } from "./model-picker";

// ---- the section ----------------------------------------------------------

/** The seven sections the rail offers. */
export type SettingsSection =
  | "general"
  | "appearance"
  | "models"
  | "secrets"
  | "plugins"
  | "shortcuts"
  | "about";

/** The open section. Module-level, so leaving the page and coming back lands
 * where the reader was; the storage below survives the process. */
export const settingsSection = signal<SettingsSection>("models");

/** The rail's entries. `group` sits on the first entry of each run, because
 * the shell prints a group label above every entry that carries one. */
const SECTIONS: SettingsSectionEntry[] = [
  { id: "general", label: "通用", icon: "settings", group: "常规" },
  { id: "appearance", label: "外观", icon: "sun" },
  { id: "models", label: "模型接入", icon: "brain", group: "配置" },
  { id: "secrets", label: "密钥", icon: "key" },
  { id: "plugins", label: "插件", icon: "layers" },
  { id: "shortcuts", label: "快捷键", icon: "keyboard", group: "参考" },
  { id: "about", label: "关于", icon: "info" },
];

const isSection = (value: string): value is SettingsSection =>
  SECTIONS.some((entry) => entry.id === value);

const sectionLabel = (): string =>
  SECTIONS.find((entry) => entry.id === settingsSection())?.label ?? "设置";

/** Bumped whenever the section moves by hand: a storage read that resolves
 * afterwards must not pull the reader back to the remembered section. */
let storageReadToken = 0;

const chooseSection = (sectionId: string) => {
  if (!isSection(sectionId)) return;
  storageReadToken += 1;
  settingsSection.set(sectionId);
  void Storage.setItem(SETTINGS_SECTION_STORAGE_KEY, sectionId).catch(() => {
    /* a host without storage just forgets the choice */
  });
};

// ---- the document draft ---------------------------------------------------

/** The raw-document editor's text: the one draft a save submits. Structured
 * controls write through it, so the form and the JSON can never disagree. */
const rawDocument = signal("");
const rawDirty = signal(false);
const rawParseError = signal<string | null>(null);
/** The「高级」disclosure: closed until the reader opens it. */
const rawOpen = signal(false);

/** The model reset's question. */
const confirmReset = signal(false);
/** The secret row whose delete is waiting for its question. */
const confirmingSecret = signal<string | null>(null);
/** The model list's filter: only models the process can run. */
const modelsOnlyExecutable = signal(false);
/** Both diagnostics tables start collapsed. */
const modelsDiagnosticsOpen = signal(false);
const pluginDiagnosticsOpen = signal(false);

const parseDocument = (text: string): { document?: Record<string, unknown>; error?: string } => {
  try {
    const value: unknown = JSON.parse(text);
    if (!value || typeof value !== "object" || Array.isArray(value)) {
      return { error: "配置必须是一个 JSON 对象" };
    }
    return { document: value as Record<string, unknown> };
  } catch (error) {
    return { error: error instanceof Error ? error.message : String(error) };
  }
};

const asRecord = (value: unknown): Record<string, unknown> => {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return {};
  return value as Record<string, unknown>;
};

/** The draft while it is readable, else the server's document: structured
 * controls stay legible even while the raw text is broken. */
const draftDocument = (): Record<string, unknown> | null => {
  const text = rawDocument();
  if (text.trim() === "") return store.settings()?.document ?? null;
  return parseDocument(text).document ?? null;
};

/** Apply one structural change to the draft. A broken raw text blocks the
 * change and opens the disclosure that repairs it — never a silent rewrite. */
const updateDraft = (mutate: (document: Record<string, unknown>) => void) => {
  const text = rawDocument();
  const parsed = text.trim() === "" ? { document: store.settings()?.document } : parseDocument(text);
  if (!parsed.document) {
    rawParseError.set(parsed.error ?? "配置不是合法的 JSON");
    rawOpen.set(true);
    return;
  }
  mutate(parsed.document);
  rawDocument.set(JSON.stringify(parsed.document, null, 2));
  rawDirty.set(true);
  rawParseError.set(null);
};

const selectionOfDocument = (document: Record<string, unknown>): ModelSelection | null => {
  const selection = asRecord(document.selection);
  if (typeof selection.modelId !== "string" || !selection.modelId) return null;
  return {
    providerId: typeof selection.providerId === "string" ? selection.providerId : undefined,
    modelId: selection.modelId,
    reasoningLevel: typeof selection.reasoningLevel === "string" ? selection.reasoningLevel : undefined,
  };
};

/** The draft's default selection (what the structured form edits). */
const documentSelection = (): ModelSelection | null => {
  const document = draftDocument();
  return document ? selectionOfDocument(document) : null;
};

const writeSelection = (next: ModelSelection) => {
  updateDraft((document) => {
    const selection: Record<string, unknown> = { providerId: next.providerId, modelId: next.modelId };
    if (next.reasoningLevel) selection.reasoningLevel = next.reasoningLevel;
    document.selection = selection;
  });
};

/** One provider as the draft declares it — the shape the editor form reads. */
interface DraftProvider {
  id: string;
  name: string;
  apiType: string;
  baseUrl: string;
  credential: string;
  modelIds: string[];
}

const draftProviders = (): DraftProvider[] => {
  const document = draftDocument();
  const rows = document && Array.isArray(document.providers) ? document.providers : [];
  const providers: DraftProvider[] = [];
  for (const raw of rows) {
    const provider = asRecord(raw);
    const id = typeof provider.id === "string" ? provider.id : "";
    if (!id) continue;
    const api = asRecord(provider.api);
    const access = asRecord(provider.access);
    const credential =
      typeof access.env === "string" && access.env
        ? access.env
        : typeof access.ref === "string"
          ? access.ref
          : "";
    providers.push({
      id,
      name: typeof provider.name === "string" ? provider.name : "",
      apiType: typeof api.type === "string" ? api.type : "",
      baseUrl: typeof api.baseUrl === "string" ? api.baseUrl : "",
      credential,
      modelIds: Array.isArray(provider.modelIds) ? provider.modelIds.map(String) : [],
    });
  }
  return providers;
};

const patchProvider = (providerId: string, mutate: (provider: Record<string, unknown>) => void) => {
  updateDraft((document) => {
    const rows = Array.isArray(document.providers) ? document.providers : [];
    for (const raw of rows) {
      const provider = asRecord(raw);
      if (provider.id !== providerId) continue;
      mutate(provider);
      return;
    }
  });
};

const setProviderApiType = (providerId: string, apiType: string) => {
  patchProvider(providerId, (provider) => {
    const api: Record<string, unknown> = { ...asRecord(provider.api) };
    api.type = apiType;
    if (typeof api.baseUrl !== "string") api.baseUrl = "";
    provider.api = api;
  });
};

const setProviderBaseUrl = (providerId: string, baseUrl: string) => {
  patchProvider(providerId, (provider) => {
    provider.api = { ...asRecord(provider.api), baseUrl };
  });
};

/** The credential is a *reference*: an env variable name, or `secret:NAME`
 * for the secret box. The value itself never reaches this form. */
const setProviderCredential = (providerId: string, credential: string) => {
  patchProvider(providerId, (provider) => {
    const access = { ...asRecord(provider.access) };
    access.type = typeof access.type === "string" ? access.type : "api-key";
    if (credential.startsWith("secret:")) {
      access.ref = credential;
      delete access.env;
    } else {
      access.env = credential;
      delete access.ref;
    }
    provider.access = access;
  });
};

/** The protocol templates the document validator knows, each under the name a
 * reader picks by. The value stays the document's own token; a document that
 * carries a token outside this list keeps its raw text (the「高级」editor is
 * where it is changed). */
const API_PROTOCOLS: SelectItem[] = [
  { value: "openai-chat-completions", title: "OpenAI 兼容" },
  { value: "openai-responses", title: "OpenAI Responses" },
  { value: "anthropic-messages", title: "Anthropic Messages" },
];

/** A protocol token in the reader's words; an unknown token is shown as it is
 * (never mapped onto a made-up name). */
const protocolLabel = (value: string | undefined): string =>
  API_PROTOCOLS.find((item) => item.value === value)?.title ?? (value ?? "");

// ---- shared pieces --------------------------------------------------------

function IssueLine(props: { issue: () => SettingsIssue }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm }}>
      <Icon name="alert" size={iconSize.xs} tint={color.danger} />
      <View style={{ flex: 1, gap: space.xxs }}>
        <If when={!!props.issue().field}>
          <Text style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.danger }}>
            {props.issue().field}
          </Text>
        </If>
        <Text
          style={{
            fontSize: typeScale.sm,
            lineHeight: Math.round(typeScale.sm * lineHeight.normal),
            color: color.foreground,
          }}
        >
          {props.issue().message}
        </Text>
      </View>
    </View>
  );
}

function IssueList(props: { issues: () => SettingsIssue[]; label?: string }) {
  return (
    <If when={props.issues().length > 0}>
      <View style={{ gap: space.sm }}>
        <If when={props.label !== undefined}>
          <Text style={{ fontSize: typeScale.xs, fontWeight: weight.semibold, color: color.mutedForeground }}>
            {props.label ?? ""}
          </Text>
        </If>
        <Each of={props.issues()} key={(issue) => `${issue.field}|${issue.message}`}>
          {(issue) => <IssueLine issue={issue} />}
        </Each>
      </View>
    </If>
  );
}

function LoadingRow(props: { text: string }) {
  return (
    <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, paddingVertical: space.sm }}>
      <Spinner size={iconSize.sm} tint={color.brand} />
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{props.text}</Text>
    </View>
  );
}

/** A tonal notice: the Card's face, retinted. The kit has no banner of its own
 * and this is the same composition the conversation's failure banner uses. */
function Notice(props: {
  tone: "danger" | "warning";
  children?: JSX.Element;
  accessibilityLabel?: string;
  testID?: string;
}) {
  return (
    <Card
      testID={props.testID}
      accessibilityLabel={props.accessibilityLabel}
      style={{
        gap: space.sm,
        padding: space.md,
        backgroundColor: props.tone === "danger" ? color.dangerTint : color.warningTint,
        borderColor: alpha(props.tone === "danger" ? color.danger : color.warning, 0.3),
      }}
    >
      {props.children}
    </Card>
  );
}

function ErrorBlock(props: { text: string; onRetry?: () => void }) {
  return (
    <Notice tone="danger" accessibilityLabel="加载失败">
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
        <Icon name="alert" size={iconSize.sm} tint={color.danger} />
        <Text
          style={{
            flex: 1,
            fontSize: typeScale.sm,
            lineHeight: Math.round(typeScale.sm * lineHeight.normal),
            color: color.foreground,
          }}
        >
          {props.text}
        </Text>
        <If when={!!props.onRetry}>
          <Button variant="outline" size="sm" label="重试" onPress={props.onRetry} />
        </If>
      </View>
    </Notice>
  );
}

/** One field of the「关于」readout: a name over its value. */
function AboutRow(props: { name: string; value?: string; children?: JSX.Element }) {
  return (
    <View style={{ gap: space.xxs }}>
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>{props.name}</Text>
      {props.value !== undefined ? (
        <Text style={{ fontSize: typeScale.sm, color: color.foreground, userSelect: "text" }}>
          {props.value}
        </Text>
      ) : (
        props.children
      )}
    </View>
  );
}

// ---- 通用 -----------------------------------------------------------------

function GeneralSection() {
  return (
    <Card testID="settings-general">
      <CardHeader title="通用" description="会话的阅读偏好；只影响这台设备的界面，不进模型配置。" />
      <CardContent>
        <Field
          orientation="horizontal"
          hint="回答生成时逐字显示；关闭后整段回复完成才出现。运行中的任务不受影响，下一次生成按新设置进行。"
        >
          <Switch
            checked={store.showPartials()}
            onChange={(checked) => void store.setPartials(checked)}
            label={`逐字预览 ${store.showPartials() ? "开" : "关"}`}
            accessibilityLabel="切换逐字预览"
            testID="settings-partials"
          />
        </Field>
      </CardContent>
    </Card>
  );
}

// ---- 外观 -----------------------------------------------------------------

/** The one write for the appearance: `applyAppearance` calls nsh's
 * `setColorScheme` (the color scheme, the components' theme and the platform's
 * controls follow at once) and keeps the choice under `keel:appearance` — the
 * same value the app root restores at boot (`src/ui/appearance.ts`), so the
 * page and the boot can never disagree. */
function AppearanceSection() {
  const currentScheme = () =>
    colorScheme() === "dark" ? "深色" : "浅色";
  return (
    <Card testID="settings-appearance">
      <CardHeader title="外观" description="主题跟随系统，或固定为浅色 / 深色；选择会被记住，下次启动照此显示。" />
      <CardContent>
        <Field
          label="颜色偏好"
          hint={`当前生效：${currentScheme()}${
            appearancePreference() === "system" ? " · 跟随系统" : ""
          }`}
        >
          <RadioGroup
            value={appearancePreference()}
            options={[
              { value: "system", label: "跟随系统" },
              { value: "light", label: "浅色" },
              { value: "dark", label: "深色" },
            ]}
            onChange={(value) => applyAppearance(value as Appearance)}
            accessibilityLabel="颜色偏好"
            testID="settings-appearance-preference"
          />
        </Field>
      </CardContent>
    </Card>
  );
}

// ---- 模型接入 ------------------------------------------------------------

function CurrentCard() {
  return (
    <Card testID="settings-current">
      <CardHeader
        title="当前生效"
        description="配置保存后立即生效，无需重启；在飞的任务继续用旧配置跑完。"
        action={
          <Button
            variant="ghost"
            size="sm"
            icon="refresh"
            label="刷新"
            accessibilityLabel="刷新模型配置"
            onPress={() => void store.loadModelsPanel()}
          />
        }
      />
      <CardContent>
        <If when={store.settingsLoading() && !store.settings()}>
          <LoadingRow text="正在读取模型配置…" />
        </If>
        <If when={!!store.settingsError()}>
          <ErrorBlock
            text={store.settingsError() ?? ""}
            onRetry={() => void store.loadModelsPanel()}
          />
        </If>
        <If when={!!store.settings()}>
          <View style={{ gap: space.md }}>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
              <Badge
                variant={store.settings()?.source === "stored" ? "info" : "secondary"}
                label={store.settings()?.source === "stored" ? "已保存覆盖" : "部署默认"}
              />
              {/* The revision stays visible but in the reader's words: it is the
               * only proof a save took effect, and "第 4 版" is a version a
               * customer understands. */}
              <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                {store.settings()?.source === "stored"
                  ? `最近更新：已保存覆盖部署默认 · 第 ${store.settings()?.revision ?? 0} 版`
                  : `最近更新：沿用部署默认 · 第 ${store.settings()?.revision ?? 0} 版`}
              </Text>
            </View>
            <Field label="默认模型">
              <If
                when={!!store.tenantSelection()}
                else={
                  <Text style={{ fontSize: typeScale.sm, color: color.warning }}>
                    未设置（还没有默认选择）
                  </Text>
                }
              >
                <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>
                  {selectionDisplay(store.tenantSelection())}
                </Text>
              </If>
            </Field>
          </View>
        </If>
      </CardContent>
    </Card>
  );
}

/** The provider choices of the structured form: readable names, the id as the
 * value (what the document stores). */
const providerItems = (): SelectItem[] =>
  draftProviders().map((provider) => ({
    value: provider.id,
    title: provider.name.trim().length > 0 ? provider.name : displayNameOf(provider.id),
  }));

/** The model choices for the selected provider (or all declared models):
 * readable names, the wire identity as the value. */
const modelItems = (selection: ModelSelection | null): SelectItem[] => {
  const providers = draftProviders();
  const chosen = providers.find((provider) => provider.id === selection?.providerId);
  const rows = (chosen ? [chosen] : providers).flatMap((provider) =>
    provider.modelIds.map((modelId) => {
      const resolved = store
        .models()
        .find((model) => model.providerId === provider.id && model.modelId === modelId);
      const unavailable = resolved?.selectable === false;
      const name = displayNameOf(modelId);
      const title =
        chosen || providers.length === 1
          ? `${name}${unavailable ? "（不可执行）" : ""}`
          : `${name} · ${provider.name.trim().length > 0 ? provider.name : displayNameOf(provider.id)}${
              unavailable ? "（不可执行）" : ""
            }`;
      return {
        value: chosen ? modelId : `${provider.id}/${modelId}`,
        title,
        disabled: unavailable,
      };
    }),
  );
  return rows;
};

const reasoningItems = (selection: ModelSelection | null): SelectItem[] => {
  const model = store
    .models()
    .find(
      (row) =>
        row.modelId === selection?.modelId &&
        (!selection?.providerId || row.providerId === selection.providerId),
    );
  const levels = model?.options?.reasoningLevel?.values ?? [];
  return [{ value: "", title: "不指定" }, ...levels.map((level) => ({ value: level, title: reasoningLabel(level) }))];
};

function SelectionCard() {
  const selection = () => documentSelection();
  const chooseProvider = (providerId: string) => {
    const provider = draftProviders().find((row) => row.id === providerId);
    const modelId = provider?.modelIds[0] ?? "";
    const resolved = store
      .models()
      .find((model) => model.providerId === providerId && model.modelId === modelId);
    writeSelection({
      providerId,
      modelId,
      reasoningLevel: resolved ? defaultReasoningFor(resolved, selection()) : undefined,
    });
  };
  const chooseModel = (value: string) => {
    const current = selection();
    const slash = value.indexOf("/");
    const providerId = current?.providerId ?? (slash > 0 ? value.slice(0, slash) : undefined);
    const modelId = slash > 0 && !current?.providerId ? value.slice(slash + 1) : value;
    const resolved = store
      .models()
      .find((model) => model.modelId === modelId && (!providerId || model.providerId === providerId));
    writeSelection({
      providerId,
      modelId,
      reasoningLevel: resolved ? defaultReasoningFor(resolved, current) : undefined,
    });
  };
  const chooseReasoning = (value: string) => {
    const current = selection();
    if (!current) return;
    writeSelection({ ...current, reasoningLevel: value === "" ? undefined : value });
  };
  const save = () => {
    const parsed = parseDocument(rawDocument());
    if (!parsed.document) {
      rawParseError.set(parsed.error ?? "配置不是合法的 JSON");
      rawOpen.set(true);
      return;
    }
    void store.saveModelsSettings(parsed.document).then(() => {
      // The server is authoritative after a write: on success the editor
      // follows the reloaded document; on failure the text stays as typed.
      if (store.settingsIssues().length === 0 && store.settingsConflict() === null) {
        rawDirty.set(false);
      }
    });
  };
  return (
    <Card testID="settings-selection">
      <CardHeader
        title="部署默认"
        description="结构化编辑默认选择；保存时整份配置一起提交，服务端按打开时读到的配置核对，并逐字段校验。"
      />
      <CardContent>
        <If when={store.settingsIssues().length > 0}>
          <Notice tone="danger" accessibilityLabel="保存被拒绝">
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.danger }}>
              保存被拒绝（{store.settingsIssues().length} 项）
            </Text>
            <IssueList issues={() => store.settingsIssues()} />
          </Notice>
        </If>
        <If when={store.settingsConflict() !== null}>
          <Notice tone="warning" accessibilityLabel="文档冲突">
            <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.warning }}>
              配置已被其它写入更新
            </Text>
            <Text
              style={{
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                color: color.foreground,
              }}
            >
              刷新以载入最新文档，再重新保存你的改动；本次修改没有被写入。
            </Text>
            <View style={{ flexDirection: "row" }}>
              <Button
                variant="outline"
                size="sm"
                label="刷新最新文档"
                onPress={() => {
                  rawDirty.set(false);
                  void store.loadModelsPanel();
                }}
              />
            </View>
          </Notice>
        </If>
        <FieldGroup>
          <Field label="服务商" hint="来自当前配置的服务商；保存后服务端按凭据解析可执行性。">
            <Select
              testID="settings-selection-provider"
              accessibilityLabel="默认服务商"
              items={providerItems()}
              value={selection()?.providerId ?? null}
              placeholder="选择服务商"
              onChange={(value) => {
                if (value) chooseProvider(value);
              }}
            />
          </Field>
          <Field
            label="模型"
            hint={
              modelItems(selection()).length === 0
                ? "该服务商还没有声明模型。"
                : "可选的模型才能保存为默认；不可执行的会标明原因。"
            }
          >
            <Select
              testID="settings-selection-model"
              accessibilityLabel="默认模型"
              items={modelItems(selection())}
              value={
                selection()?.providerId
                  ? (selection()?.modelId ?? null)
                  : selection()
                    ? `${selection()?.providerId ?? ""}/${selection()?.modelId ?? ""}`
                    : null
              }
              disabled={modelItems(selection()).length === 0}
              placeholder="选择模型"
              onChange={(value) => {
                if (value) chooseModel(value);
              }}
            />
          </Field>
          <Field label="思考档" hint="只列出该模型声明的档位；不指定则用模型默认。">
            <Select
              testID="settings-selection-reasoning"
              accessibilityLabel="默认思考档"
              items={reasoningItems(selection())}
              value={selection()?.reasoningLevel ?? ""}
              disabled={reasoningItems(selection()).length <= 1}
              onChange={(value) => chooseReasoning(value ?? "")}
            />
          </Field>
        </FieldGroup>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
          <Button
            icon="check"
            label="保存并生效"
            accessibilityLabel="保存模型配置"
            loading={store.settingsSaving()}
            disabled={store.settingsSaving() || !!rawParseError() || !rawDocument().trim()}
            onPress={save}
          />
          <Button
            variant="outline"
            icon="refresh"
            label="重置"
            accessibilityLabel="重置为部署默认"
            disabled={store.settingsSaving()}
            onPress={() => confirmReset.set(true)}
          />
          <View style={{ flex: 1 }} />
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
            {rawDirty() ? "有未保存的修改" : "编辑区与当前生效配置一致"}
          </Text>
        </View>
      </CardContent>
      <AlertDialog
        testID="settings-reset"
        open={confirmReset()}
        title="重置为部署默认"
        description="重置会删除已保存的覆盖，回到部署默认；确定吗？"
        confirmLabel="确认重置"
        cancelLabel="取消"
        destructive
        onConfirm={() => {
          confirmReset.set(false);
          rawDirty.set(false);
          void store.resetModelsSettings();
        }}
        onCancel={() => confirmReset.set(false)}
      />
    </Card>
  );
}

/** One provider's structured row: template, endpoint, credential reference. */
function ProviderEditorRow(props: { provider: () => DraftProvider }) {
  const name = () => props.provider().name.trim() || displayNameOf(props.provider().id);
  return (
    <View
      style={{
        gap: space.md,
        padding: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
        <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>
          {name()}
        </Text>
        {/* The document's own key, as a detail: the name above is what the
            reader edits by. */}
        <Text style={{ fontFamily: font.mono, fontSize: typeScale["2xs"], color: color.mutedForeground }}>
          {props.provider().id}
        </Text>
        <Badge variant="secondary" label={`${props.provider().modelIds.length} 个模型`} />
      </View>
      <FieldGroup>
        <Field label="接口协议" hint="只列常用的三种；其它取值可在「高级」的原始配置里改。">
          <Select
            testID={`settings-provider-template-${props.provider().id}`}
            accessibilityLabel={`${name()} 接口协议`}
            items={API_PROTOCOLS}
            value={props.provider().apiType}
            placeholder="选择协议"
            onChange={(value) => {
              if (value) setProviderApiType(props.provider().id, value);
            }}
          />
        </Field>
        <Field label="服务地址" hint="服务商的接口根地址，例如 https://api.example.com/v1。">
          <Input
            testID={`settings-provider-endpoint-${props.provider().id}`}
            accessibilityLabel={`${name()} 服务地址`}
            value={props.provider().baseUrl}
            placeholder="https://…"
            onChange={(value) => setProviderBaseUrl(props.provider().id, value)}
          />
        </Field>
        <Field
          label="凭据引用"
          hint="环境变量的名字，或「密钥」分区里保存的名字；这里永远不存凭据的值。"
        >
          <Input
            testID={`settings-provider-credential-${props.provider().id}`}
            accessibilityLabel={`${name()} 凭据引用`}
            value={props.provider().credential}
            placeholder="S2API_KEY 或 secret:name"
            onChange={(value) => setProviderCredential(props.provider().id, value)}
          />
        </Field>
      </FieldGroup>
    </View>
  );
}

function ProviderEditorCard() {
  const providers = () => draftProviders();
  return (
    <Card testID="settings-provider-editor">
      <CardHeader
        title="服务商接入"
        description="按服务商编辑接口协议、服务地址与凭据引用；改的是待保存的配置，按上方「保存并生效」生效。"
      />
      <CardContent>
        <If when={providers().length === 0}>
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            当前配置里还没有服务商；在「高级」里补上服务商列表，或重置为部署默认。
          </Text>
        </If>
        <Each of={providers()} key={(provider) => provider.id}>
          {(provider) => <ProviderEditorRow provider={provider} />}
        </Each>
      </CardContent>
    </Card>
  );
}

/** The raw document, behind one closed disclosure: the escape hatch, never the
 * primary surface. The parse diagnostics live here too — the main cards show
 * what the configuration is, not what the validator said about it. */
function AdvancedCard() {
  const diagnostics = () => [...(store.settings()?.issues ?? []), ...store.providersIssues()];
  return (
    <Card testID="settings-advanced-card">
      <Collapsible
        open={rawOpen()}
        accessibilityLabel="高级"
        content={
          <View style={{ gap: space.md }}>
            <Text
              style={{
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                color: color.mutedForeground,
              }}
            >
              完整的模型配置：服务商列表、模型规则与默认选择。保存时整份提交；
              服务端校验失败会按字段列出原因。
            </Text>
            <Textarea
              testID="settings-raw-document"
              accessibilityLabel="原始 models 文档"
              value={rawDocument()}
              onChange={(value) => {
                rawDocument.set(value);
                rawDirty.set(true);
                rawParseError.set(parseDocument(value).error ?? null);
              }}
              rows={10}
              invalid={rawParseError() !== null}
              placeholder={'{\n  "schemaVersion": 1,\n  "providers": [],\n  "selection": {}\n}'}
            />
            <If when={!!rawParseError()}>
              <Text
                style={{
                  fontSize: typeScale.xs,
                  lineHeight: Math.round(typeScale.xs * lineHeight.normal),
                  fontFamily: font.mono,
                  color: color.danger,
                }}
              >
                原始文档不是合法 JSON：{rawParseError() ?? ""}
              </Text>
            </If>
            <If when={diagnostics().length > 0}>
              <View style={{ gap: space.sm }}>
                <Label label="配置诊断" />
                <IssueList issues={diagnostics} />
              </View>
            </If>
          </View>
        }
      >
        <CollapsibleTrigger
          testID="settings-advanced"
          label="高级"
          icon="file"
          expanded={rawOpen()}
          onPress={() => rawOpen.set(!rawOpen())}
        />
      </Collapsible>
    </Card>
  );
}

function ModelRow(props: { model: () => ModelView }) {
  const tenant = () => store.tenantSelection();
  const isDefault = () =>
    tenant()?.modelId === props.model().modelId &&
    tenant()?.providerId === props.model().providerId;
  const choose = () => {
    const model = props.model();
    writeSelection({
      providerId: model.providerId,
      modelId: model.modelId,
      reasoningLevel: defaultReasoningFor(model, documentSelection()),
    });
  };
  return (
    <View
      style={{
        flexDirection: "row",
        alignItems: "flex-start",
        gap: space.md,
        padding: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: isDefault() ? alpha(color.brand, 0.4) : color.border,
        backgroundColor: isDefault() ? color.brandTint : undefined,
        flexWrap: "wrap",
      }}
    >
      <View style={{ flex: 1, minWidth: 0, gap: space.xxs }}>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
          <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>
            {displayNameOf(props.model().modelId)}
          </Text>
          <Text style={{ fontFamily: font.mono, fontSize: typeScale["2xs"], color: color.mutedForeground }}>
            {props.model().providerId}/{props.model().modelId}
          </Text>
          <If when={isDefault()}>
            <Badge variant="info" label="当前默认" />
          </If>
          <If when={props.model().selectable === false}>
            <Badge variant="warning" label="不可选" />
          </If>
          <If when={props.model().enabled === false}>
            <Badge variant="secondary" label="已禁用" />
          </If>
        </View>
        <Text style={{ fontSize: typeScale.xs, lineHeight: Math.round(typeScale.xs * lineHeight.normal), color: color.mutedForeground }}>
          {modelFacts(props.model())}
        </Text>
        <IssueList issues={() => props.model().issues ?? []} />
      </View>
      <If when={!isDefault()}>
        <Button
          variant="outline"
          size="sm"
          label="设为默认"
          accessibilityLabel={`设为默认 ${displayNameOf(props.model().modelId)}`}
          disabled={props.model().selectable === false}
          onPress={choose}
        />
      </If>
    </View>
  );
}

/** The capability gate's diagnostics: one table, closed until asked for. */
function ModelsDiagnostics() {
  const enforcement = () => store.enforcement();
  return (
    <If when={!!enforcement()}>
      <Collapsible
        open={modelsDiagnosticsOpen()}
        accessibilityLabel="模型诊断"
        content={
          <View style={{ gap: space.md }}>
            <Label label="能力执行门" />
            <Table bare accessibilityLabel="能力执行门">
              <TableHeaderRow>
                <Th>
                  <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>能力</Text>
                </Th>
                <Th>
                  <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>执行</Text>
                </Th>
              </TableHeaderRow>
              <TableBody>
                <TableBodyRow>
                  <Td>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>工具调用</Text>
                  </Td>
                  <Td>
                    <Badge
                      variant={enforcement()?.toolCall ? "success" : "warning"}
                      label={enforcement()?.toolCall ? "强制" : "未强制"}
                    />
                  </Td>
                </TableBodyRow>
                <TableBodyRow>
                  <Td>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>媒体</Text>
                  </Td>
                  <Td>
                    <Badge
                      variant={enforcement()?.media ? "success" : "warning"}
                      label={enforcement()?.media ? "强制" : "记录但未强制"}
                    />
                  </Td>
                </TableBodyRow>
              </TableBody>
            </Table>
          </View>
        }
      >
        <CollapsibleTrigger
          testID="settings-models-diagnostics"
          label="诊断"
          icon="tool"
          expanded={modelsDiagnosticsOpen()}
          onPress={() => modelsDiagnosticsOpen.set(!modelsDiagnosticsOpen())}
        />
      </Collapsible>
    </If>
  );
}

function ModelsListCard() {
  const models = () =>
    store.models().filter((model) => !modelsOnlyExecutable() || model.selectable !== false);
  return (
    <Card testID="settings-models">
      <CardHeader
        title="模型列表"
        description="来自服务端清单：只含凭据可解析的可见服务商；可选的模型可以点选为默认。"
      />
      <CardContent>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
          <Switch
            checked={modelsOnlyExecutable()}
            onChange={(checked) => modelsOnlyExecutable.set(checked)}
            label="只看可执行"
            accessibilityLabel="只看可执行模型"
          />
          <View style={{ flex: 1 }} />
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
            {models().length} / {store.models().length} 个模型
          </Text>
        </View>
        <If when={!!store.modelsError()}>
          <ErrorBlock text={store.modelsError() ?? ""} onRetry={() => void store.loadRegistry()} />
        </If>
        <If when={!store.modelsError() && !store.registryLoaded()}>
          <LoadingRow text="正在读取模型列表…" />
        </If>
        <If when={!store.modelsError() && store.registryLoaded() && store.models().length === 0}>
          <Empty accessibilityLabel="没有可用的模型">
            <EmptyTitle label="没有可用的模型" />
            <EmptyDescription label="当前进程解析不到任何服务商的凭据，或配置为空。" />
          </Empty>
        </If>
        <If
          when={
            !store.modelsError() &&
            store.registryLoaded() &&
            store.models().length > 0 &&
            models().length === 0
          }
        >
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
            没有可执行的模型；关掉「只看可执行」查看全部。
          </Text>
        </If>
        <Each of={models()} key={(model) => `${model.providerId}/${model.modelId}`}>
          {(model) => <ModelRow model={model} />}
        </Each>
        <IssueList issues={() => store.modelIssues()} label="列表诊断" />
        <ModelsDiagnostics />
      </CardContent>
    </Card>
  );
}

function ProviderRow(props: { provider: () => ProviderView }) {
  const accessLabel = () => {
    const parts: string[] = [];
    if (props.provider().access?.env) parts.push(`env ${props.provider().access?.env}`);
    if (props.provider().access?.ref) parts.push(`ref ${props.provider().access?.ref}`);
    return parts.join(" · ") || "无凭据引用";
  };
  const modelIssues = () =>
    (props.provider().models ?? []).filter((model) => (model.issues ?? []).length > 0);
  return (
    <View
      style={{
        gap: space.sm,
        padding: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
        <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>
          {providerDisplayName(props.provider())}
        </Text>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale["2xs"], color: color.mutedForeground }}>
          {props.provider().providerId}
        </Text>
        <Badge
          variant={props.provider().executable ? "success" : "destructive"}
          label={props.provider().executable ? "可执行" : "不可执行"}
        />
        <Badge variant="secondary" label={`${(props.provider().models ?? []).length} 个模型`} />
      </View>
      <Text style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.mutedForeground }}>
        {accessLabel()} · {protocolLabel(props.provider().api?.type)} {props.provider().api?.baseUrl ?? ""}
      </Text>
      <IssueList issues={() => props.provider().issues ?? []} />
      <Each
        of={modelIssues()}
        key={(model) => `${model.providerId}/${model.modelId}`}
      >
        {(model) => (
          <View style={{ gap: space.xxs }}>
            <Text style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.mutedForeground }}>
              {model().providerId}/{model().modelId}
            </Text>
            <IssueList issues={() => model().issues ?? []} />
          </View>
        )}
      </Each>
    </View>
  );
}

function ProvidersListCard() {
  return (
    <Card testID="settings-providers">
      <CardHeader
        title="服务商列表"
        description="来自服务端清单（只含可见服务商）：凭据状态与可执行性。"
      />
      <CardContent>
        <If when={!!store.providersError()}>
          <ErrorBlock text={store.providersError() ?? ""} onRetry={() => void store.loadRegistry()} />
        </If>
        <If when={!store.providersError() && !store.registryLoaded()}>
          <LoadingRow text="正在读取服务商…" />
        </If>
        <If
          when={!store.providersError() && store.registryLoaded() && store.providers().length === 0}
        >
          <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>没有可见的服务商。</Text>
        </If>
        <Each of={store.providers()} key={(provider) => provider.providerId}>
          {(provider) => <ProviderRow provider={provider} />}
        </Each>
      </CardContent>
    </Card>
  );
}

function ModelsSection() {
  // The editor follows the loaded document until the reader types in it; every
  // successful write and reload makes the server's document authoritative.
  effect(() => {
    const loaded = store.settings();
    if (!loaded) return;
    if (!rawDirty()) rawDocument.set(JSON.stringify(loaded.document, null, 2));
  });
  return (
    <View style={{ gap: space["3xl"] }}>
      <CurrentCard />
      <SelectionCard />
      <AdvancedCard />
      <ProviderEditorCard />
      <ModelsListCard />
      <ProvidersListCard />
    </View>
  );
}

/** The secret form's draft: the value lives only in this signal, and is
 * cleared the moment the server accepts it. */
const secretName = signal("");
const secretValue = signal("");

// ---- 密钥 -----------------------------------------------------------------

function SecretRow(props: { secret: () => SecretMeta }) {
  return (
    <View
      style={{
        flexDirection: "row",
        alignItems: "center",
        gap: space.md,
        padding: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
        flexWrap: "wrap",
      }}
    >
      <Icon name="key" size={iconSize.sm} tint={color.mutedForeground} />
      <View style={{ flexDirection: "row", alignItems: "baseline", gap: space.xs, flexWrap: "wrap" }}>
        <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>
          {props.secret().name}
        </Text>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.mutedForeground }}>
          secret:{props.secret().name}
        </Text>
      </View>
      <View style={{ flex: 1 }} />
      <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
        创建 {formatTime(props.secret().createdAt)} · 更新 {formatTime(props.secret().updatedAt)}
      </Text>
      <Button
        variant="ghost"
        size="sm"
        icon="trash"
        label="删除"
        accessibilityLabel={`删除密钥 ${props.secret().name}`}
        disabled={store.secretsBusy()}
        onPress={() => confirmingSecret.set(props.secret().name)}
      />
    </View>
  );
}

function SecretsSection() {
  const available = () => store.secretsAvailable();
  /** Why the box is off, in the reader's words: the server names its own
   * configuration key, and that name is not the reader's business. */
  const reasonText = () => {
    const raw = store.secretsReason();
    if (!raw || /KEEL_SECRET_KEY/.test(raw)) return "服务端尚未启用密钥存储。";
    return raw;
  };
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
      confirmingSecret.set(null);
    });
  };
  return (
    <View style={{ gap: space["3xl"] }}>
      <Card testID="settings-secrets">
        <CardHeader
          title="密钥"
          description="按名字写入/覆盖/删除；值只写不回读（响应、日志、列表都不含它）。模型配置里按名字引用这里保存的密钥。"
        />
        <CardContent>
          <If when={!available()}>
            <Notice tone="warning" accessibilityLabel="密钥存储未启用">
              <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.warning }}>
                密钥存储未启用
              </Text>
              <Text
                style={{
                  fontSize: typeScale.sm,
                  lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                  color: color.foreground,
                }}
              >
                {reasonText()}
              </Text>
              <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
                不会回退为明文存储；配置密钥盒后这里即可写入。
              </Text>
            </Notice>
          </If>
          <If when={store.secretsLoading()}>
            <LoadingRow text="正在读取密钥…" />
          </If>
          <If when={!!store.secretsError()}>
            <ErrorBlock text={store.secretsError() ?? ""} onRetry={() => void store.loadSecrets()} />
          </If>
          <If when={!store.secretsError() && !store.secretsLoading() && store.secrets().length === 0}>
            <Empty accessibilityLabel="还没有密钥">
              <EmptyTitle label="还没有密钥" />
              <EmptyDescription label="写入一个名字和值；之后这里只显示名字与时间。" />
            </Empty>
          </If>
          <Each of={store.secrets()} key={(secret) => secret.name}>
            {(secret) => <SecretRow secret={secret} />}
          </Each>
          <Separator />
          <View style={{ gap: space.md }}>
            <Label label={available() ? "写入 / 覆盖" : "写入（不可用）"} />
            <FieldGroup>
              <Field label="名字" hint="写入同名名字即覆盖；模型配置里用 secret:名字 引用它。">
                <Input
                  testID="secret-name"
                  accessibilityLabel="密钥名字"
                  value={secretName()}
                  onChange={(value) => secretName.set(value)}
                  placeholder="名字，例如 s2api"
                  maxLength={128}
                  disabled={!available()}
                />
              </Field>
              <Field label="值" hint="保存后输入框清空；之后的界面只显示名字与时间。">
                <Input
                  testID="secret-value"
                  accessibilityLabel="密钥值"
                  value={secretValue()}
                  onChange={(value) => secretValue.set(value)}
                  placeholder="值（保存后不再显示）"
                  masked
                  maxLength={4096}
                  disabled={!available()}
                />
              </Field>
            </FieldGroup>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
              <Button
                icon="check"
                label={isOverwrite() ? "覆盖保存" : "保存密钥"}
                accessibilityLabel="保存密钥"
                loading={store.secretsBusy()}
                disabled={!available() || store.secretsBusy() || !secretName().trim() || !secretValue()}
                onPress={submit}
              />
              <If when={!!store.secretsActionError()}>
                <Text style={{ fontSize: typeScale.sm, color: color.danger }}>
                  {store.secretsActionError() ?? ""}
                </Text>
              </If>
            </View>
          </View>
        </CardContent>
      </Card>
      <AlertDialog
        testID="settings-secret-delete"
        open={!!confirmingSecret()}
        title={`删除密钥 ${confirmingSecret() ?? ""}？`}
        description="删除后引用它的服务商会解析不到凭据，相关模型不可执行；此操作不可撤销。"
        confirmLabel={`确认删除密钥 ${confirmingSecret() ?? ""}`}
        cancelLabel={`取消删除密钥 ${confirmingSecret() ?? ""}`}
        destructive
        onConfirm={() => {
          const name = confirmingSecret();
          confirmingSecret.set(null);
          if (name) void store.deleteSecret(name);
        }}
        onCancel={() => confirmingSecret.set(null)}
      />
    </View>
  );
}

// ---- 插件 -----------------------------------------------------------------

const pluginStateVariant = (state: string | undefined) => {
  switch (state) {
    case "ready":
      return "success" as const;
    case "failed":
      return "destructive" as const;
    case "stopped":
      return "secondary" as const;
    default:
      return "warning" as const;
  }
};

/** The plugin page's route (`App.tsx` renders it). SEAM: the store's `Route`
 * union grows `plugins` with the parallel plugin entry; this is the same narrow
 * cast the app root makes at its own boundary, so the two agree the moment the
 * union grows. */
const openPluginPage = () => {
  (store.route as unknown as { set: (route: "plugins") => void }).set("plugins");
};

function PluginRow(props: { plugin: () => PluginStatusView }) {
  return (
    <View
      style={{
        gap: space.sm,
        padding: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
        <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>
          {props.plugin().id}
        </Text>
        <If when={!!props.plugin().version}>
          <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>v{props.plugin().version}</Text>
        </If>
        <Badge
          variant={pluginStateVariant(props.plugin().state)}
          label={pluginStateLabel(props.plugin().state)}
        />
        <View style={{ flex: 1 }} />
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
          {props.plugin().startedAt ? `启动 ${formatTime(props.plugin().startedAt)}` : ""}
          {props.plugin().stoppedAt ? ` · 停止 ${formatTime(props.plugin().stoppedAt)}` : ""}
        </Text>
      </View>
      <If when={(props.plugin().provides ?? []).length > 0 || (props.plugin().requires ?? []).length > 0}>
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
          提供 {(props.plugin().provides ?? []).join("/") || "—"} · 依赖{" "}
          {(props.plugin().requires ?? []).join("/") || "无"}
        </Text>
      </If>
      <If when={(props.plugin().tools ?? []).length > 0}>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.mutedForeground }}>
          工具：{(props.plugin().tools ?? []).join(" · ")}
        </Text>
      </If>
      <If when={(props.plugin().hooks ?? []).length > 0}>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.mutedForeground }}>
          hook：{(props.plugin().hooks ?? []).join(" · ")}
        </Text>
      </If>
      <If when={(props.plugin().triggers ?? []).length > 0}>
        <Text style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.mutedForeground }}>
          触发器：{(props.plugin().triggers ?? []).map((trigger) => trigger.id).join(" · ")}
        </Text>
      </If>
      <Each of={props.plugin().diagnostics ?? []} key="index">
        {(line) => (
          <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm }}>
            <Icon name="alert" size={iconSize.xs} tint={color.warning} />
            <Text
              style={{
                flex: 1,
                fontFamily: font.mono,
                fontSize: typeScale.xs,
                lineHeight: Math.round(typeScale.xs * lineHeight.normal),
                color: color.warning,
              }}
            >
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
  const outcome = () =>
    store.fireOutcome()?.triggerId === props.trigger().id ? store.fireOutcome() : null;
  const canFire = () => props.trigger().enabled !== false;
  return (
    <View
      style={{
        gap: space.sm,
        padding: space.md,
        borderRadius: radius.md,
        borderWidth: 1,
        borderColor: color.border,
      }}
    >
      <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
        <Icon name="clock" size={iconSize.xs} tint={color.mutedForeground} />
        <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>
          {props.trigger().id}
        </Text>
        <Badge variant="secondary" label={props.trigger().kind ?? "trigger"} />
        <If when={props.trigger().enabled === false}>
          <Badge variant="warning" label="已禁用" />
        </If>
        <View style={{ flex: 1 }} />
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground, fontVariant: ["tabular-nums"] }}>
          {props.trigger().fires ?? 0} 次触发 · {props.trigger().failures ?? 0} 失败
        </Text>
        <Button
          testID={`trigger-fire-${props.trigger().id}`}
          variant="outline"
          size="sm"
          icon="play"
          label={firing() ? "触发中…" : "触发"}
          accessibilityLabel={`手动触发 ${props.trigger().id}`}
          loading={firing()}
          disabled={!canFire() || firing()}
          onPress={() => void store.fireTrigger(props.trigger().id)}
        />
      </View>
      <Text
        style={{
          fontSize: typeScale.xs,
          lineHeight: Math.round(typeScale.xs * lineHeight.normal),
          color: color.mutedForeground,
        }}
      >
        {props.trigger().schedule ? `计划 ${props.trigger().schedule} · ` : ""}
        {props.trigger().prompt ? `提示「${props.trigger().prompt}」 · ` : ""}
        上次 {formatTime(props.trigger().lastFireAt) || "未触发"}
        {props.trigger().lastStatus ? `（${props.trigger().lastStatus}）` : ""}
        {props.trigger().nextFireAt ? ` · 下次 ${formatTime(props.trigger().nextFireAt)}` : ""}
      </Text>
      <If when={!!outcome()}>
        <Text
          style={{
            fontFamily: font.mono,
            fontSize: typeScale.xs,
            lineHeight: Math.round(typeScale.xs * lineHeight.normal),
            color: outcome()?.ok ? color.success : color.danger,
          }}
        >
          {outcome()?.ok ? "触发结果：" : "触发失败："}
          {outcome()?.text ?? ""}
        </Text>
      </If>
    </View>
  );
}

/** The hook pipeline's counters: a table, collapsed, with the health as one
 * Badge — a number nobody can read at a glance is not a status. */
function PluginDiagnostics() {
  const hooks = () => store.plugins()?.hookStats ?? store.plugins()?.hooks;
  const degraded = () => (hooks()?.dropped ?? 0) > 0 || (hooks()?.failed ?? 0) > 0;
  return (
    <If when={!!hooks()}>
      <Collapsible
        open={pluginDiagnosticsOpen()}
        accessibilityLabel="插件诊断"
        content={
          <View style={{ gap: space.md }}>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
              <Label label="hook 管道" />
              <Badge
                variant={degraded() ? "destructive" : "success"}
                label={degraded() ? "有丢弃或失败" : "正常"}
              />
            </View>
            <Table bare accessibilityLabel="hook 诊断">
              <TableHeaderRow>
                <Th>
                  <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>指标</Text>
                </Th>
                <Th>
                  <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>数值</Text>
                </Th>
              </TableHeaderRow>
              <TableBody>
                <TableBodyRow>
                  <Td>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>订阅</Text>
                  </Td>
                  <Td>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground, fontVariant: ["tabular-nums"] }}>
                      {hooks()?.subscriptions ?? 0}
                    </Text>
                  </Td>
                </TableBodyRow>
                <TableBodyRow>
                  <Td>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>发出</Text>
                  </Td>
                  <Td>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground, fontVariant: ["tabular-nums"] }}>
                      {hooks()?.emitted ?? 0}
                    </Text>
                  </Td>
                </TableBodyRow>
                <TableBodyRow>
                  <Td>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>投递</Text>
                  </Td>
                  <Td>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground, fontVariant: ["tabular-nums"] }}>
                      {hooks()?.delivered ?? 0}
                    </Text>
                  </Td>
                </TableBodyRow>
                <TableBodyRow>
                  <Td>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>丢弃</Text>
                  </Td>
                  <Td>
                    <Badge
                      variant={(hooks()?.dropped ?? 0) > 0 ? "destructive" : "secondary"}
                      label={String(hooks()?.dropped ?? 0)}
                    />
                  </Td>
                </TableBodyRow>
                <TableBodyRow>
                  <Td>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>失败</Text>
                  </Td>
                  <Td>
                    <Badge
                      variant={(hooks()?.failed ?? 0) > 0 ? "destructive" : "secondary"}
                      label={String(hooks()?.failed ?? 0)}
                    />
                  </Td>
                </TableBodyRow>
                <TableBodyRow>
                  <Td>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>队列</Text>
                  </Td>
                  <Td>
                    <Text style={{ fontSize: typeScale.sm, color: color.foreground, fontVariant: ["tabular-nums"] }}>
                      {hooks()?.queued ?? 0}/{hooks()?.capacity ?? 0}
                    </Text>
                  </Td>
                </TableBodyRow>
              </TableBody>
            </Table>
          </View>
        }
      >
        <CollapsibleTrigger
          testID="settings-plugin-diagnostics"
          label="诊断"
          icon="tool"
          expanded={pluginDiagnosticsOpen()}
          onPress={() => pluginDiagnosticsOpen.set(!pluginDiagnosticsOpen())}
        />
      </Collapsible>
    </If>
  );
}

function PluginsSection() {
  const plugins = () => store.plugins()?.plugins ?? [];
  return (
    <View style={{ gap: space["3xl"] }}>
      <Card testID="settings-plugins">
        <CardHeader
          title="插件清单"
          description="插件在进程启动时装载并与依赖一起装配；这里看状态与控制触发，不做代码级配置。"
          action={
            <Button
              variant="ghost"
              size="sm"
              icon="refresh"
              label="刷新"
              accessibilityLabel="刷新插件清单"
              onPress={() => void store.loadPlugins()}
            />
          }
        />
        <CardContent>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
            <View style={{ flex: 1, gap: space.xxs }}>
              <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
                插件页面
              </Text>
              <Text style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.mutedForeground }}>
                /plugins
              </Text>
            </View>
            <Button
              variant="outline"
              size="sm"
              icon="external"
              label="打开插件页"
              accessibilityLabel="打开插件页"
              onPress={openPluginPage}
            />
          </View>
          <If when={store.pluginsLoading()}>
            <LoadingRow text="正在读取插件清单…" />
          </If>
          <If when={!!store.pluginsError()}>
            <ErrorBlock text={store.pluginsError() ?? ""} onRetry={() => void store.loadPlugins()} />
          </If>
          <If when={!store.pluginsError() && !store.pluginsLoading()}>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
              <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
                {(store.plugins()?.tools ?? []).length} 个工具 · {plugins().length} 个插件
              </Text>
            </View>
            <If when={(store.plugins()?.tools ?? []).length > 0}>
              <Text
                style={{
                  fontFamily: font.mono,
                  fontSize: typeScale.xs,
                  lineHeight: Math.round(typeScale.xs * lineHeight.normal),
                  color: color.mutedForeground,
                }}
              >
                工具：{(store.plugins()?.tools ?? []).join(" · ")}
              </Text>
            </If>
            <If when={plugins().length === 0}>
              <Empty accessibilityLabel="没有已装载的插件">
                <EmptyTitle label="没有已装载的插件" />
                <EmptyDescription label="插件随进程启动装载；装入后刷新本页。" />
              </Empty>
            </If>
          </If>
          <Each of={plugins()} key={(plugin) => plugin.id}>
            {(plugin) => <PluginRow plugin={plugin} />}
          </Each>
          <PluginDiagnostics />
        </CardContent>
      </Card>

      <Card testID="settings-triggers">
        <CardHeader
          title="触发器"
          description="来自服务端清单：手动触发会记入审计；被停用的触发器无法触发。"
        />
        <CardContent>
          <If
            when={!store.pluginsError() && !store.pluginsLoading() && store.triggers().length === 0}
          >
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>没有注册的触发器。</Text>
          </If>
          <Each of={store.triggers()} key={(trigger) => trigger.id}>
            {(trigger) => <TriggerRow trigger={trigger} />}
          </Each>
        </CardContent>
      </Card>
    </View>
  );
}

// ---- 快捷键 ---------------------------------------------------------------

const SHORTCUTS: Array<{ keys: string[]; label: string }> = [
  { keys: ["mod", "K"], label: "聚焦会话搜索" },
  { keys: ["Enter"], label: "发送消息" },
  { keys: ["Shift", "Enter"], label: "输入换行" },
  { keys: ["Esc"], label: "关闭弹层、取消当前选择" },
];

function ShortcutsSection() {
  return (
    <Card testID="settings-shortcuts">
      <CardHeader title="快捷键" description="当前版本的键位；这里只读展示，不做改键。" />
      <CardContent>
        <Each of={SHORTCUTS} key={(row) => row.label}>
          {(row) => (
            <View
              style={{
                flexDirection: "row",
                alignItems: "center",
                gap: space.lg,
                paddingVertical: space.sm,
              }}
            >
              <Kbd keys={row().keys} accessibilityLabel={row().keys.join("+")} />
              <Text style={{ flex: 1, fontSize: typeScale.sm, color: color.foreground }}>{row().label}</Text>
            </View>
          )}
        </Each>
      </CardContent>
    </Card>
  );
}

// ---- 关于 -----------------------------------------------------------------

interface HostInfo {
  version?: string;
  online?: boolean;
  userAgent?: string;
  origin?: string;
}

const hostInfo = signal<HostInfo | null>(null);
const hostInfoError = signal<string | null>(null);

function AboutSection() {
  effect(() => {
    void (async () => {
      if (!host.available()) {
        hostInfo.set({});
        return;
      }
      try {
        const info = await host.call<HostInfo>("keel.info", {}, { timeout: 1200 });
        hostInfo.set(info ?? {});
      } catch (error) {
        hostInfoError.set(error instanceof Error ? error.message : String(error));
      }
    })();
  });
  return (
    <Card testID="settings-about">
      <CardHeader title="关于" description="版本、运行环境与构建标签。" />
      <CardContent>
        <If when={hostInfo() === null && !hostInfoError()}>
          <Skeleton lines={3} accessibilityLabel="正在读取运行信息" />
        </If>
        <If when={!!hostInfoError()}>
          <ErrorBlock text={hostInfoError() ?? ""} />
        </If>
        <If when={hostInfo() !== null}>
          <View style={{ gap: space.lg }}>
            <AboutRow name="版本" value={hostInfo()?.version ?? "—"} />
            <AboutRow
              name="在线"
              value={hostInfo()?.online === undefined ? "—" : hostInfo()?.online ? "在线" : "离线"}
            />
            <AboutRow name="构建">
              <Badge variant="secondary" label="NSH nightly" />
            </AboutRow>
            <AboutRow name="运行环境">
              <Text
                style={{
                  fontSize: typeScale.sm,
                  lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                  color: color.foreground,
                  userSelect: "text",
                }}
              >
                {hostInfo()?.userAgent ?? "宿主未提供运行信息。"}
              </Text>
            </AboutRow>
            <If when={!!hostInfo()?.origin}>
              <AboutRow name="来源" value={hostInfo()?.origin ?? ""} />
            </If>
          </View>
        </If>
      </CardContent>
    </Card>
  );
}

// ---- the page -------------------------------------------------------------

function SectionBody() {
  return (
    <If when={settingsSection() === "general"} else={<SectionBodyNext />}>
      <GeneralSection />
    </If>
  );
}

function SectionBodyNext() {
  return (
    <If when={settingsSection() === "appearance"} else={<SectionBodyNextNext />}>
      <AppearanceSection />
    </If>
  );
}

function SectionBodyNextNext() {
  return (
    <If when={settingsSection() === "models"} else={<SectionBodyRest />}>
      <ModelsSection />
    </If>
  );
}

function SectionBodyRest() {
  return (
    <If when={settingsSection() === "secrets"} else={<SectionBodyTail />}>
      <SecretsSection />
    </If>
  );
}

function SectionBodyTail() {
  return (
    <If when={settingsSection() === "plugins"} else={<SectionBodyEnd />}>
      <PluginsSection />
    </If>
  );
}

function SectionBodyEnd() {
  return (
    <If when={settingsSection() === "shortcuts"} else={<AboutSection />}>
      <ShortcutsSection />
    </If>
  );
}

/** The standalone settings page. The workspace (and its open session) waits
 * underneath; the shell remembers nothing itself because the section is
 * controlled from here. */
export function SettingsScreen() {
  // The remembered section, read once per mount. A choice made before the read
  // resolves wins: its token bump makes the late answer stale.
  effect(() => {
    const token = ++storageReadToken;
    void (async () => {
      let stored: string | null = null;
      try {
        stored = await Storage.getItem(SETTINGS_SECTION_STORAGE_KEY);
      } catch {
        /* a host without storage: no memory to restore */
      }
      if (token !== storageReadToken) return;
      if (stored !== null && isSection(stored)) settingsSection.set(stored);
    })();
  });

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
    const section = settingsSection();
    const timer = setTimeout(() => {
      if (section === "models") void store.loadModelsPanel();
      else if (section === "secrets") void store.loadSecrets();
      else if (section === "plugins") void store.loadPlugins();
    }, 0);
    cleanup(() => clearTimeout(timer));
  });

  return (
    <SettingsShell
      testID="settings-page"
      sections={SECTIONS}
      section={settingsSection()}
      onSectionChange={chooseSection}
      breadcrumb={["设置", sectionLabel()]}
      onBack={() => store.closeSettings()}
    >
      <View testID="settings-section-body" style={{ flexDirection: "column" }}>
        <SectionBody />
      </View>
    </SettingsShell>
  );
}
