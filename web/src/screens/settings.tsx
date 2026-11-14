// The settings page (change keel-harness-shell, tasks G1–G5).
//
// The frame is the kit's `SettingsShell`: the icon rail (68px, 268px from the
// wide breakpoint), the h-12 breadcrumb row, the content card and the
// max-w-4xl content column with its 32px section rhythm all belong to that
// block (`web/src/ui/kit/layout/settings-shell.tsx`). This file draws none of
// them.
//
// Eight sections, in the rail's order:
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
//   · 能力       what this deployment can actually do: the media gateway's
//                capabilities and the realtime bridge's preflight, each as
//                available / unconfigured (with the environment variable names
//                to set) / unsupported by the provider. Read-only, and never a
//                URL, a key or a raw runtime error.
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
  DeploymentCapabilitiesApi,
  MediaCapabilitiesView,
  MediaCapabilityStatus,
  MediaCapabilityView,
  PluginStatusView,
  RealtimeStatusView,
  SecretMeta,
  SettingsIssue,
  TriggerStatusView,
} from "../data/contract";
import { KeelApiError, KeelHttp, type FetchLike } from "../data/http";
import { store } from "../data/store";
import {
  displayNameOf,
  formatTime,
  pluginStateLabel,
  providerDisplayName,
  reasoningLabel,
  selectionDisplay,
  selectionLabel,
} from "../data/wire";
import { applyAppearance, appearancePreference, type Appearance } from "../ui/appearance";
import { Icon } from "../ui/icons";
import {
  AlertDialog,
  Badge,
  Button,
  Card,
  CardContent,
  CardFooter,
  CardHeader,
  Collapsible,
  CollapsibleTrigger,
  Dialog,
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
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
import { AgentTeamSettingsSection } from "./settings-agents";

// ---- the section ----------------------------------------------------------

/** The ten sections the rail offers. */
export type SettingsSection =
  | "general"
  | "appearance"
  | "agents"
  | "teams"
  | "models"
  | "capabilities"
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
  { id: "agents", label: "Agents", icon: "terminal" },
  { id: "teams", label: "团队", icon: "message" },
  { id: "capabilities", label: "能力", icon: "spark" },
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
/** Both diagnostics tables start collapsed. */
const modelsDiagnosticsOpen = signal(false);
const pluginDiagnosticsOpen = signal(false);

// ---- the second level: one dialog per job -------------------------------
//
// The list is the page; every write happens in a modal that edits *one* thing.
// The signals below are the modal's draft, separate from the raw document so a
// rejected save never loses what the reader typed.

/** The provider editor: closed, editing an existing provider, or adding one. */
type ProviderEditorMode = "closed" | "edit" | "new";
const providerEditorMode = signal<ProviderEditorMode>("closed");
/** The id the editor opened on (empty for a new provider). */
const editingProviderId = signal("");
const editorId = signal("");
const editorName = signal("");
const editorApiType = signal("openai-chat-completions");
const editorBaseUrl = signal("");
const editorCredential = signal("");
const editorModelIds = signal<string[]>([]);
const editorModelInput = signal("");
const editorIdError = signal<string | null>(null);
const editorBaseUrlError = signal<string | null>(null);
const editorCredentialError = signal<string | null>(null);
const editorDocumentError = signal<string | null>(null);
const confirmDeleteProvider = signal(false);

/** The「更换」dialog: provider + model + reasoning for the default selection. */
const replaceOpen = signal(false);
const replaceProviderId = signal<string | null>(null);
const replaceModelId = signal<string | null>(null);
const replaceReasoning = signal("");
const replaceError = signal<string | null>(null);

/** A save that the server never acknowledged (a transport failure, not a 400:
 * the 400's field issues have their own place). */
const saveFailure = signal<string | null>(null);
/** Feedback belongs to this attempt: a stale rejection from an earlier dialog
 * must not greet the next one. */
const saveAttempted = signal(false);

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
  return parseDocument(text).document ?? store.settings()?.document ?? null;
};

/** A copy of the document a dialog edits: a rejected save leaves the raw draft
 * exactly as it was, so nothing the reader typed is lost. */
const copyDocument = (document: Record<string, unknown>): Record<string, unknown> =>
  JSON.parse(JSON.stringify(document)) as Record<string, unknown>;

/** The candidate document a dialog writes: the typed raw text while it is
 * readable, else the server's document it was opened on. */
const currentDocument = (): { document?: Record<string, unknown>; error?: string } => {
  const text = rawDocument();
  if (text.trim() === "") {
    const server = store.settings()?.document;
    return server ? { document: copyDocument(server) } : { error: "还没有读取到模型配置" };
  }
  return parseDocument(text);
};

/** Submit one candidate document and report whether the server accepted it.
 * The revision is the acknowledgement: a 400 / 409 / transport failure leaves
 * it where it was, and the dialog keeps its draft. */
const saveCandidate = async (document: Record<string, unknown>): Promise<boolean> => {
  const before = store.settings()?.revision ?? -1;
  saveAttempted.set(true);
  saveFailure.set(null);
  await store.saveModelsSettings(document);
  const after = store.settings()?.revision ?? -1;
  const accepted =
    after > before && store.settingsIssues().length === 0 && store.settingsConflict() === null;
  if (accepted) {
    rawDirty.set(false);
    rawParseError.set(null);
    const loaded = store.settings();
    if (loaded) rawDocument.set(JSON.stringify(loaded.document, null, 2));
    return true;
  }
  if (store.settingsIssues().length === 0 && store.settingsConflict() === null) {
    saveFailure.set("保存没有生效：服务端没有返回新版本，请检查连接后重试。");
  }
  return false;
};

/** The credential is a *reference*, never a value. Three spellings reach the
 * same document field: `${NAME}`, `env:NAME` and a bare `NAME` name an
 * environment variable; `secret:NAME` points at the secret box. */
const applyCredential = (access: Record<string, unknown>, raw: string) => {
  const value = raw.trim();
  if (!value) {
    delete access.env;
    delete access.ref;
    return;
  }
  if (value.startsWith("secret:")) {
    access.ref = value;
    delete access.env;
    return;
  }
  if (value.startsWith("env:")) {
    access.env = value.slice("env:".length).trim();
    delete access.ref;
    return;
  }
  const braced = /^\$\{([^}]+)\}$/.exec(value);
  access.env = braced ? braced[1] : value;
  delete access.ref;
};

/** One provider as the draft declares it — the shape the cards and the editor
 * read. */
interface DraftProvider {
  id: string;
  name: string;
  apiType: string;
  baseUrl: string;
  /** The reference as the form reads it: `env:NAME` or `secret:NAME`. */
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
      typeof access.ref === "string" && access.ref
        ? access.ref
        : typeof access.env === "string" && access.env
          ? `env:${access.env}`
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

const providerNameOf = (provider: DraftProvider): string =>
  provider.name.trim() || displayNameOf(provider.id);

interface ProviderStatus {
  variant: "success" | "warning" | "destructive";
  label: "已配置" | "未配置" | "不可用";
}

/** The badge's state. 已配置 = the process can resolve the credential and the
 * provider declares models; 未配置 = a field the reader still has to fill;
 * 不可用 = the process cannot resolve it. */
const providerStatusOf = (provider: DraftProvider): ProviderStatus => {
  if (!provider.credential.trim() || provider.modelIds.length === 0) {
    return { variant: "warning", label: "未配置" };
  }
  const resolved = store.providers().find((row) => row.providerId === provider.id);
  return resolved?.executable
    ? { variant: "success", label: "已配置" }
    : { variant: "destructive", label: "不可用" };
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
//
// Two levels, and only two:
//
//   · The page — 当前默认模型 (one Card: what is in force, with「更换」「刷新」)
//     and one Card per provider (name, status badge, model chips,「配置」).
//     It answers the three questions a reader actually has: which model am I
//     using, which providers exist, and what do I do to add one.
//   · The dialogs — every write. A provider's protocol / endpoint / credential
//     reference / models are edited in a modal that covers one provider; the
//     default selection has its own small modal. The raw document stays at the
//     bottom behind one closed「高级」disclosure.
//
// The old long page is gone: 部署默认's inline form merged into 当前默认模型 and
// its「更换」dialog; 服务商接入's per-provider forms became the cards and their
// dialogs; 模型列表 and 服务商列表 became the model chips and status badges on
// those cards.

/** The page's first level: what is in force right now. */
function CurrentModelCard() {
  const selection = () => store.tenantSelection();
  const providerName = () => {
    const current = selection();
    if (!current) return "";
    const draft = draftProviders().find((provider) => provider.id === current.providerId);
    if (draft) return providerNameOf(draft);
    const resolved = store.providers().find((provider) => provider.providerId === current.providerId);
    if (resolved) return providerDisplayName(resolved);
    return current.providerId ? displayNameOf(current.providerId) : "未指定服务商";
  };
  return (
    <Card testID="settings-current">
      <CardHeader
        title="当前默认模型"
        description="配置保存后立即生效，无需重启；在飞的任务继续用旧配置跑完。"
        action={
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
            <Button
              testID="settings-change-default"
              variant="outline"
              size="sm"
              label="更换"
              accessibilityLabel="更换默认模型"
              onPress={openReplaceDialog}
            />
            <Button
              testID="settings-refresh-models"
              variant="ghost"
              size="sm"
              icon="refresh"
              label="刷新"
              accessibilityLabel="刷新模型配置"
              onPress={() => void store.loadModelsPanel()}
            />
          </View>
        }
      />
      <CardContent>
        <If when={store.settingsLoading() && !store.settings()}>
          <Skeleton lines={2} accessibilityLabel="正在读取模型配置" testID="settings-models-loading" />
        </If>
        <If when={!!store.settingsError()}>
          <ErrorBlock
            text={store.settingsError() ?? ""}
            onRetry={() => void store.loadModelsPanel()}
          />
        </If>
        <If when={!!store.settings()}>
          <If
            when={!!selection()}
            else={
              <View style={{ gap: space.xs }}>
                <Text
                  style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.warning }}
                >
                  还没有默认模型
                </Text>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  点「更换」选一个服务商与模型；保存后新会话立即使用它。
                </Text>
              </View>
            }
          >
            <View style={{ gap: space.xs }}>
              <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
                <Icon name="brain" size={iconSize.sm} tint={color.brand} />
                <Text
                  testID="settings-default-provider"
                  style={{ fontSize: typeScale.sm, color: color.mutedForeground }}
                >
                  {providerName()}
                </Text>
              </View>
              <Text
                testID="settings-default-model"
                style={{ fontSize: typeScale.lg, fontWeight: weight.semibold, color: color.foreground }}
              >
                {selectionDisplay(selection())}
              </Text>
              <Text
                testID="settings-default-wire"
                style={{ fontFamily: font.mono, fontSize: typeScale.xs, color: color.mutedForeground }}
              >
                {selectionLabel(selection())}
              </Text>
            </View>
          </If>
          <Separator />
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
            <Badge
              variant={store.settings()?.source === "stored" ? "info" : "secondary"}
              label={store.settings()?.source === "stored" ? "已保存覆盖" : "部署默认"}
            />
            <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
              第 {store.settings()?.revision ?? 0} 版
            </Text>
          </View>
        </If>
      </CardContent>
    </Card>
  );
}

/** The one place a rejected save (or a conflict) is explained, so the dialogs
 * and the advanced editor say the same thing. */
function SaveFeedback() {
  return (
    <>
      <If when={saveAttempted() && store.settingsIssues().length > 0}>
        <Notice tone="danger" accessibilityLabel="保存被拒绝" testID="settings-save-issues">
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.danger }}>
            保存被拒绝（{store.settingsIssues().length} 项）
          </Text>
          <IssueList issues={() => store.settingsIssues()} />
        </Notice>
      </If>
      <If when={saveAttempted() && store.settingsConflict() !== null}>
        <Notice tone="warning" accessibilityLabel="文档冲突" testID="settings-save-conflict">
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
      <If when={saveAttempted() && !!saveFailure()}>
        <Notice tone="danger" accessibilityLabel="保存失败" testID="settings-save-failure">
          <Text
            style={{
              fontSize: typeScale.sm,
              lineHeight: Math.round(typeScale.sm * lineHeight.normal),
              color: color.foreground,
            }}
          >
            {saveFailure() ?? ""}
          </Text>
        </Notice>
      </If>
    </>
  );
}

// ---- the dialogs' actions -------------------------------------------------

const closeProviderEditor = () => {
  providerEditorMode.set("closed");
  editingProviderId.set("");
  editorDocumentError.set(null);
  editorIdError.set(null);
  editorBaseUrlError.set(null);
  editorCredentialError.set(null);
  confirmDeleteProvider.set(false);
  saveFailure.set(null);
  saveAttempted.set(false);
};

const seedEditor = (provider: DraftProvider) => {
  providerEditorMode.set("edit");
  editingProviderId.set(provider.id);
  editorId.set(provider.id);
  editorName.set(provider.name);
  editorApiType.set(provider.apiType || "openai-chat-completions");
  editorBaseUrl.set(provider.baseUrl);
  editorCredential.set(provider.credential);
  editorModelIds.set([...provider.modelIds]);
  editorModelInput.set("");
  editorIdError.set(null);
  editorBaseUrlError.set(null);
  editorCredentialError.set(null);
  editorDocumentError.set(null);
  saveFailure.set(null);
  saveAttempted.set(false);
};

const openProviderEditor = (providerId: string) => {
  const provider = draftProviders().find((row) => row.id === providerId);
  if (provider) seedEditor(provider);
};

const openNewProvider = () => {
  providerEditorMode.set("new");
  editingProviderId.set("");
  editorId.set("");
  editorName.set("");
  editorApiType.set("openai-chat-completions");
  editorBaseUrl.set("");
  editorCredential.set("");
  editorModelIds.set([]);
  editorModelInput.set("");
  editorIdError.set(null);
  editorBaseUrlError.set(null);
  editorCredentialError.set(null);
  editorDocumentError.set(null);
  saveFailure.set(null);
  saveAttempted.set(false);
};

const addEditorModel = () => {
  const value = editorModelInput().trim();
  if (!value) return;
  if (!editorModelIds().includes(value)) editorModelIds.set([...editorModelIds(), value]);
  editorModelInput.set("");
};

const removeEditorModel = (modelId: string) => {
  editorModelIds.set(editorModelIds().filter((row) => row !== modelId));
};

const saveProvider = async () => {
  editorIdError.set(null);
  editorBaseUrlError.set(null);
  editorCredentialError.set(null);
  editorDocumentError.set(null);
  saveFailure.set(null);
  const isNew = providerEditorMode() === "new";
  const originalId = editingProviderId();
  const id = editorId().trim();
  const baseUrl = editorBaseUrl().trim();
  const credential = editorCredential().trim();
  let invalid = false;
  if (!id) {
    editorIdError.set("给这个 Provider 一个 id，例如 s2api。");
    invalid = true;
  } else if (!/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(id)) {
    editorIdError.set("id 只能包含字母、数字、点、下划线和连字符。");
    invalid = true;
  } else if (isNew && draftProviders().some((provider) => provider.id === id)) {
    editorIdError.set("这个 id 已经被另一个 Provider 使用。");
    invalid = true;
  }
  if (!baseUrl) {
    editorBaseUrlError.set("服务地址不能为空，例如 https://api.example.com/v1。");
    invalid = true;
  }
  if (!credential) {
    editorCredentialError.set("凭据引用不能为空：env:NAME 或 secret:NAME。");
    invalid = true;
  }
  if (invalid) return;

  const candidate = currentDocument();
  if (!candidate.document) {
    editorDocumentError.set(candidate.error ?? "配置不是合法的 JSON，先到「高级」修复原始文档。");
    return;
  }
  const document = candidate.document;
  const rows = Array.isArray(document.providers) ? [...document.providers] : [];
  const index = rows.findIndex((raw) => asRecord(raw).id === originalId);
  const provider: Record<string, unknown> = index >= 0 ? { ...asRecord(rows[index]) } : { id };
  provider.id = id;
  if (editorName().trim()) provider.name = editorName().trim();
  else delete provider.name;
  provider.api = { ...asRecord(provider.api), type: editorApiType(), baseUrl };
  const access = { ...asRecord(provider.access) };
  access.type = typeof access.type === "string" && access.type ? access.type : "api-key";
  applyCredential(access, credential);
  provider.access = access;
  provider.modelIds = editorModelIds().map((modelId) => modelId.trim()).filter(Boolean);
  if (index >= 0) rows[index] = provider;
  else rows.push(provider);
  document.providers = rows;
  if (originalId && originalId !== id) {
    const selection = asRecord(document.selection);
    if (selection.providerId === originalId) document.selection = { ...selection, providerId: id };
  }
  // An empty deployment has no default yet; the first provider that declares a
  // model becomes it, so "add a provider" alone leads to a working state. A
  // valid existing selection is never touched.
  const currentSelection = asRecord(document.selection);
  const selectionValid =
    typeof currentSelection.modelId === "string" &&
    currentSelection.modelId.length > 0 &&
    rows.some((raw) => {
      const row = asRecord(raw);
      return (
        row.id === currentSelection.providerId &&
        Array.isArray(row.modelIds) &&
        row.modelIds.map(String).includes(String(currentSelection.modelId))
      );
    });
  if (!selectionValid) {
    const modelIds = Array.isArray(provider.modelIds) ? provider.modelIds.map(String) : [];
    if (modelIds.length > 0) document.selection = { providerId: id, modelId: modelIds[0] };
  }
  const ok = await saveCandidate(document);
  if (ok) closeProviderEditor();
};

const deleteProvider = async () => {
  confirmDeleteProvider.set(false);
  const id = editingProviderId();
  const candidate = currentDocument();
  if (!candidate.document) {
    editorDocumentError.set(candidate.error ?? "配置不是合法的 JSON，先到「高级」修复原始文档。");
    return;
  }
  const document = candidate.document;
  const rows = (Array.isArray(document.providers) ? document.providers : []).filter(
    (raw) => asRecord(raw).id !== id,
  );
  document.providers = rows;
  const selection = asRecord(document.selection);
  if (selection.providerId === id) {
    const next = rows
      .map(asRecord)
      .find((row) => Array.isArray(row.modelIds) && row.modelIds.length > 0);
    if (next) {
      document.selection = { providerId: next.id, modelId: String((next.modelIds as unknown[])[0]) };
    } else {
      delete document.selection;
    }
  }
  const ok = await saveCandidate(document);
  if (ok) closeProviderEditor();
};

const openReplaceDialog = () => {
  const current = store.tenantSelection();
  const providers = draftProviders();
  const chosen = providers.find((provider) => provider.id === current?.providerId) ?? providers[0];
  const modelId =
    chosen && current && chosen.modelIds.includes(current.modelId)
      ? current.modelId
      : chosen?.modelIds[0];
  replaceProviderId.set(chosen?.id ?? null);
  replaceModelId.set(modelId ?? null);
  replaceReasoning.set(current?.reasoningLevel ?? "");
  replaceError.set(null);
  saveFailure.set(null);
  saveAttempted.set(false);
  replaceOpen.set(true);
};

const chooseReplaceProvider = (providerId: string) => {
  replaceProviderId.set(providerId);
  const provider = draftProviders().find((row) => row.id === providerId);
  const modelId = provider?.modelIds[0] ?? null;
  replaceModelId.set(modelId);
  const resolved = store
    .models()
    .find((model) => model.providerId === providerId && model.modelId === modelId);
  replaceReasoning.set(
    resolved ? defaultReasoningFor(resolved, store.tenantSelection()) ?? "" : "",
  );
  replaceError.set(null);
};

const chooseReplaceModel = (modelId: string) => {
  replaceModelId.set(modelId);
  const providerId = replaceProviderId();
  const resolved = store
    .models()
    .find((model) => model.providerId === providerId && model.modelId === modelId);
  replaceReasoning.set(
    resolved ? defaultReasoningFor(resolved, store.tenantSelection()) ?? "" : "",
  );
  replaceError.set(null);
};

const saveReplacement = async () => {
  replaceError.set(null);
  const providerId = replaceProviderId();
  const modelId = replaceModelId();
  if (!providerId || !modelId) {
    replaceError.set("先选择服务商与模型。");
    return;
  }
  const candidate = currentDocument();
  if (!candidate.document) {
    replaceError.set(candidate.error ?? "配置不是合法的 JSON");
    return;
  }
  const selection: Record<string, unknown> = { providerId, modelId };
  if (replaceReasoning()) selection.reasoningLevel = replaceReasoning();
  candidate.document.selection = selection;
  const ok = await saveCandidate(candidate.document);
  if (ok) replaceOpen.set(false);
};

// ---- the first level: provider cards --------------------------------------

function ProviderCard(props: { provider: () => DraftProvider }) {
  const status = () => providerStatusOf(props.provider());
  const resolved = () =>
    store.providers().find((provider) => provider.providerId === props.provider().id);
  const wire = () => {
    const provider = props.provider();
    const protocol = protocolLabel(provider.apiType) || "协议未设置";
    return provider.baseUrl
      ? `${protocol} · ${provider.baseUrl}`
      : `${protocol} · 服务地址未设置`;
  };
  return (
    <Card
      testID={`provider-card-${props.provider().id}`}
      accessibilityLabel={`Provider ${providerNameOf(props.provider())}`}
    >
      <CardHeader
        title={providerNameOf(props.provider())}
        description={wire()}
        action={
          <Badge
            testID={`provider-status-${props.provider().id}`}
            variant={status().variant}
            label={status().label}
          />
        }
      />
      <CardContent>
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
          <If when={props.provider().modelIds.length === 0}>
            <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>还没有模型</Text>
          </If>
          <Each of={props.provider().modelIds} key={(modelId) => modelId}>
            {(modelId) => (
              <Badge
                testID={`provider-model-${props.provider().id}-${modelId()}`}
                variant="secondary"
                label={displayNameOf(modelId())}
              />
            )}
          </Each>
        </View>
        <IssueList issues={() => resolved()?.issues ?? []} />
      </CardContent>
      <CardFooter>
        <Text
          style={{
            fontFamily: font.mono,
            fontSize: typeScale["2xs"],
            color: color.mutedForeground,
          }}
        >
          {props.provider().credential || "凭据未设置"}
        </Text>
        <View style={{ flex: 1 }} />
        <Button
          testID={`provider-configure-${props.provider().id}`}
          variant="link"
          size="sm"
          label="配置"
          accessibilityLabel={`配置 ${providerNameOf(props.provider())}`}
          onPress={() => openProviderEditor(props.provider().id)}
        />
      </CardFooter>
    </Card>
  );
}

function ProvidersSection() {
  const providers = () => draftProviders();
  const loading = () =>
    (store.settingsLoading() && !store.settings()) ||
    (!store.registryLoaded() && !store.providersError() && !store.modelsError());
  return (
    <View style={{ gap: space.lg }}>
      <View style={{ flexDirection: "row", alignItems: "flex-end", gap: space.md, flexWrap: "wrap" }}>
        <View style={{ flex: 1, minWidth: 200, gap: space.xxs }}>
          <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>
            Provider
          </Text>
          <Text
            style={{
              fontSize: typeScale.sm,
              lineHeight: Math.round(typeScale.sm * lineHeight.normal),
              color: color.mutedForeground,
            }}
          >
            一个服务商一张卡片；点「配置」只编辑这一个，保存后立即生效。
          </Text>
        </View>
        <Button
          testID="provider-add"
          variant="outline"
          size="sm"
          icon="plus"
          label="添加 Provider"
          accessibilityLabel="添加 Provider"
          onPress={openNewProvider}
        />
      </View>
      <If when={loading()}>
        <Skeleton lines={3} accessibilityLabel="正在读取服务商" testID="providers-loading" />
      </If>
      <If when={!loading() && providers().length === 0 && !!store.settings()}>
        <Card testID="providers-empty">
          <Empty accessibilityLabel="还没有 Provider">
            <EmptyMedia icon="plus" tone="brand" />
            <EmptyTitle label="还没有 Provider" />
            <EmptyDescription label="添加一个 Provider 需要两项：接口地址（例如 https://api.example.com/v1）与凭据引用（env:NAME 或 secret:NAME）。" />
            <EmptyContent>
              <Button
                icon="plus"
                label="添加 Provider"
                accessibilityLabel="添加 Provider"
                onPress={openNewProvider}
              />
            </EmptyContent>
          </Empty>
        </Card>
      </If>
      <Each of={providers()} key={(provider) => provider.id}>
        {(provider) => <ProviderCard provider={provider} />}
      </Each>
    </View>
  );
}

// ---- the second level: one provider ---------------------------------------

function ProviderConfigDialog() {
  const mode = () => providerEditorMode();
  const isNew = () => mode() === "new";
  const title = () =>
    isNew()
      ? "添加 Provider"
      : `配置 ${editorName().trim() || displayNameOf(editingProviderId())}`;
  return (
    <>
      <Dialog
        testID="provider-config-dialog"
        open={mode() !== "closed"}
        // `margin: auto` is how nsh's dialog layer centers a card (the kit Dialog
        // carries no placement of its own); without it the popup sits where the
        // layer's default puts it and a tall form overflows over the sidebar.
        // The cap keeps a long form on screen; the body below scrolls inside it.
        style={{ margin: "auto" }}
        maxHeight={720}
        title={title()}
        description={
          isNew()
            ? "填接口地址与凭据引用，再声明这个服务商可用的模型；保存后立即生效。"
            : "只编辑这一个 Provider；保存后回到列表，状态与徽标随之更新。"
        }
        onOpenChange={(event) => {
          if (!event.open) closeProviderEditor();
        }}
        footer={
          <>
            <If when={!isNew()}>
              <Button
                variant="destructive"
                size="sm"
                icon="trash"
                label="删除该 Provider"
                accessibilityLabel="删除该 Provider"
                disabled={store.settingsSaving()}
                onPress={() => confirmDeleteProvider.set(true)}
              />
            </If>
            <View style={{ flex: 1 }} />
            <Button
              variant="outline"
              label="取消"
              accessibilityLabel="取消"
              disabled={store.settingsSaving()}
              onPress={closeProviderEditor}
            />
            <Button
              icon="check"
              label="保存并生效"
              accessibilityLabel="保存并生效"
              loading={store.settingsSaving()}
              disabled={store.settingsSaving()}
              onPress={() => void saveProvider()}
            />
          </>
        }
      >
        <SaveFeedback />
        <If when={!!editorDocumentError()}>
          <Notice tone="danger" accessibilityLabel="配置不可编辑">
            <Text
              style={{
                fontSize: typeScale.sm,
                lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                color: color.foreground,
              }}
            >
              {editorDocumentError() ?? ""}
            </Text>
          </Notice>
        </If>
        <FieldGroup>
          <Field
            label="Provider ID"
            required
            hint="配置里唯一的标识，例如 s2api；改名会同步更新指向它的默认选择。"
            error={editorIdError() ?? undefined}
          >
            <Input
              testID="provider-config-id"
              accessibilityLabel="Provider ID"
              value={editorId()}
              placeholder="s2api"
              onChange={(value) => {
                editorId.set(value);
                editorIdError.set(null);
              }}
            />
          </Field>
          <Field label="显示名称" hint="给读者看的名字；留空则用 id。">
            <Input
              testID="provider-config-name"
              accessibilityLabel="显示名称"
              value={editorName()}
              placeholder="例如 S2API"
              onChange={(value) => editorName.set(value)}
            />
          </Field>
          <Field label="接口协议" hint="只列常用的三种；其它取值可在「高级」的原始配置里改。">
            <Select
              testID="provider-config-protocol"
              accessibilityLabel="接口协议"
              items={API_PROTOCOLS}
              value={editorApiType()}
              placeholder="选择协议"
              onChange={(value) => {
                if (value) editorApiType.set(value);
              }}
            />
          </Field>
          <Field
            label="服务地址"
            required
            hint="服务商的接口根地址，例如 https://api.example.com/v1。"
            error={editorBaseUrlError() ?? undefined}
          >
            <Input
              testID="provider-config-endpoint"
              accessibilityLabel="服务地址"
              value={editorBaseUrl()}
              placeholder="https://…"
              onChange={(value) => {
                editorBaseUrl.set(value);
                editorBaseUrlError.set(null);
              }}
            />
          </Field>
          <Field
            label="凭据引用"
            required
            hint="三种写法：${NAME} 或 env:NAME 指环境变量，secret:NAME 指「密钥」分区里保存的密钥；这里永远不存凭据的值。"
            error={editorCredentialError() ?? undefined}
          >
            <Input
              testID="provider-config-credential"
              accessibilityLabel="凭据引用"
              value={editorCredential()}
              placeholder="env:S2API_KEY 或 secret:name"
              onChange={(value) => {
                editorCredential.set(value);
                editorCredentialError.set(null);
              }}
            />
          </Field>
          <View style={{ gap: space.sm }}>
            <Label label="模型列表" />
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
              <If when={editorModelIds().length === 0}>
                <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>
                  还没有模型；在下面添加模型 ID。
                </Text>
              </If>
              <Each of={editorModelIds()} key={(modelId) => modelId}>
                {(modelId) => (
                  <Badge
                    testID={`provider-config-model-${modelId()}`}
                    variant="secondary"
                    icon="x"
                    label={displayNameOf(modelId())}
                    accessibilityLabel={`移除模型 ${displayNameOf(modelId())}`}
                    onPress={() => removeEditorModel(modelId())}
                  />
                )}
              </Each>
            </View>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm }}>
              <View style={{ flex: 1 }}>
                <Input
                  testID="provider-config-model-input"
                  accessibilityLabel="要添加的模型 ID"
                  value={editorModelInput()}
                  placeholder="模型 ID，例如 deepseek-v4.1-flash"
                  submitBehavior="submit"
                  onChange={(value) => editorModelInput.set(value)}
                  onSubmit={() => addEditorModel()}
                />
              </View>
              <Button
                variant="outline"
                size="sm"
                icon="plus"
                label="添加模型"
                accessibilityLabel="添加模型"
                onPress={addEditorModel}
              />
            </View>
            <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
              模型 ID 必须与服务商的清单一致；保存后按凭据解析可执行性。
            </Text>
          </View>
        </FieldGroup>
      </Dialog>
      <AlertDialog
        testID="settings-provider-delete"
        open={confirmDeleteProvider()}
        title={`删除 Provider ${editorName().trim() || displayNameOf(editingProviderId())}？`}
        description="删除后指向它的默认选择会改到下一个可用 Provider；此操作不可撤销。"
        confirmLabel="确认删除"
        cancelLabel="取消删除"
        destructive
        onConfirm={() => void deleteProvider()}
        onCancel={() => confirmDeleteProvider.set(false)}
      />
    </>
  );
}

// ---- the second level: the default selection ------------------------------

function ChangeDefaultDialog() {
  const providers = () => draftProviders();
  const modelItems = (): SelectItem[] => {
    const provider = providers().find((row) => row.id === replaceProviderId());
    return (provider?.modelIds ?? []).map((modelId) => {
      const resolved = store
        .models()
        .find((model) => model.providerId === provider?.id && model.modelId === modelId);
      const unavailable = resolved?.selectable === false;
      return {
        value: modelId,
        title: `${displayNameOf(modelId)}${unavailable ? "（不可执行）" : ""}`,
        disabled: unavailable,
      };
    });
  };
  const reasoningItems = (): SelectItem[] => {
    const model = store
      .models()
      .find(
        (row) =>
          row.providerId === replaceProviderId() && row.modelId === replaceModelId(),
      );
    const levels = model?.options?.reasoningLevel?.values ?? [];
    return [
      { value: "", title: "不指定" },
      ...levels.map((level) => ({ value: level, title: reasoningLabel(level) })),
    ];
  };
  return (
    <Dialog
      testID="default-model-dialog"
      open={replaceOpen()}
      style={{ margin: "auto" }}
      maxHeight={640}
      title="更换默认模型"
      description="保存后新的默认选择立即生效；在飞的任务继续用旧配置跑完。"
      onOpenChange={(event) => {
        if (!event.open) replaceOpen.set(false);
      }}
      footer={
        <>
          <Button
            variant="outline"
            label="取消"
            accessibilityLabel="取消更换默认模型"
            disabled={store.settingsSaving()}
            onPress={() => replaceOpen.set(false)}
          />
          <Button
            icon="check"
            label="保存并生效"
            accessibilityLabel="保存默认模型"
            loading={store.settingsSaving()}
            disabled={store.settingsSaving() || providers().length === 0}
            onPress={() => void saveReplacement()}
          />
        </>
      }
    >
      <SaveFeedback />
      <If when={providers().length === 0}>
        <Notice tone="warning" accessibilityLabel="还没有 Provider">
          <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>
            还没有 Provider；先在列表里添加一个，再选默认模型。
          </Text>
        </Notice>
      </If>
      <If when={!!replaceError()}>
        <Notice tone="danger" accessibilityLabel="默认模型未保存">
          <Text style={{ fontSize: typeScale.sm, color: color.foreground }}>{replaceError() ?? ""}</Text>
        </Notice>
      </If>
      <FieldGroup>
        <Field label="服务商" hint="来自当前配置的服务商。">
          <Select
            testID="default-provider"
            accessibilityLabel="默认服务商"
            items={providers().map((provider) => ({
              value: provider.id,
              title: providerNameOf(provider),
            }))}
            value={replaceProviderId()}
            placeholder="选择服务商"
            onChange={(value) => {
              if (value) chooseReplaceProvider(value);
            }}
          />
        </Field>
        <Field
          label="模型"
          hint={
            modelItems().length === 0
              ? "该服务商还没有声明模型。"
              : "不可执行的模型会标明原因并禁用。"
          }
        >
          <Select
            testID="default-model"
            accessibilityLabel="默认模型"
            items={modelItems()}
            value={replaceModelId()}
            disabled={modelItems().length === 0}
            placeholder="选择模型"
            onChange={(value) => {
              if (value) chooseReplaceModel(value);
            }}
          />
        </Field>
        <Field label="思考档" hint="只列出该模型声明的档位；不指定则用模型默认。">
          <Select
            testID="default-reasoning"
            accessibilityLabel="默认思考档"
            items={reasoningItems()}
            value={replaceReasoning()}
            disabled={reasoningItems().length <= 1}
            onChange={(value) => replaceReasoning.set(value ?? "")}
          />
        </Field>
      </FieldGroup>
    </Dialog>
  );
}

// ---- 高级 -----------------------------------------------------------------

const saveRawDocument = () => {
  const parsed = parseDocument(rawDocument());
  if (!parsed.document) {
    rawParseError.set(parsed.error ?? "配置不是合法的 JSON");
    return;
  }
  void saveCandidate(parsed.document);
};

/** The raw document, behind one closed disclosure at the bottom: the escape
 * hatch, never the primary surface. */
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
              直接编辑整份配置，保存后立即生效。服务商列表、模型规则与默认选择都在这里；服务端按字段列出拒绝原因。
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
            <SaveFeedback />
            <If when={diagnostics().length > 0}>
              <View style={{ gap: space.sm }}>
                <Label label="配置诊断" />
                <IssueList issues={diagnostics} />
              </View>
            </If>
            <ModelsDiagnostics />
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.md, flexWrap: "wrap" }}>
              <Button
                icon="check"
                label="保存配置"
                accessibilityLabel="保存配置"
                loading={store.settingsSaving()}
                disabled={store.settingsSaving() || !!rawParseError() || !rawDocument().trim()}
                onPress={saveRawDocument}
              />
              <Button
                variant="outline"
                icon="refresh"
                label="重置为部署默认"
                accessibilityLabel="重置为部署默认"
                disabled={store.settingsSaving()}
                onPress={() => confirmReset.set(true)}
              />
              <View style={{ flex: 1 }} />
              <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
                {rawDirty() ? "有未保存的修改" : "编辑区与当前生效配置一致"}
              </Text>
            </View>
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

export function ModelsSection() {
  // The editor follows the loaded document until the reader types in it; every
  // successful write and reload makes the server's document authoritative.
  effect(() => {
    const loaded = store.settings();
    if (!loaded) return;
    if (!rawDirty()) rawDocument.set(JSON.stringify(loaded.document, null, 2));
  });
  effect(() => {
    cleanup(() => {
      providerEditorMode.set("closed");
      replaceOpen.set(false);
      confirmDeleteProvider.set(false);
    });
  });
  return (
    <View style={{ gap: space["3xl"] }}>
      <CurrentModelCard />
      <ProvidersSection />
      <AdvancedCard />
      <ProviderConfigDialog />
      <ChangeDefaultDialog />
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
    case "enabled":
      return "success" as const;
    case "installed":
    case "disabled":
    case "uninstalled":
      return "secondary" as const;
    case "failed":
    case "invalid":
      return "destructive" as const;
    default:
      return "outline" as const;
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
      <If
        when={
          (props.plugin().provides?.tools ?? []).length > 0 ||
          (props.plugin().provides?.cli ?? []).length > 0 ||
          (props.plugin().requires ?? []).length > 0
        }
      >
        <Text style={{ fontSize: typeScale.xs, color: color.mutedForeground }}>
          提供 {(props.plugin().provides?.tools ?? []).join("/") || "—"} · 依赖{" "}
          {(props.plugin().requires ?? []).join("/") || "无"}
          {(props.plugin().provides?.cli ?? []).length > 0
            ? ` · CLI ${(props.plugin().provides?.cli ?? []).length} 条`
            : ""}
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

// ---- 能力 -----------------------------------------------------------------
//
// What this deployment can actually do, instead of a generic model list. Two
// read-only probes answer it: `GET /v1/media/capabilities` (the media
// gateway's own rows) and `GET /v1/realtime/status` (the voice bridge's
// preflight). The server owns every fact; this page renders availability, the
// environment variable NAMES an operator sets when a row is unconfigured, and
// the provider's reason when it declines a capability. The transport drops
// URLs, keys and runtime errors before they can reach the screen, so the page
// never has raw infrastructure text to print.

type CapabilityRowStatus = MediaCapabilityStatus | "auth" | "unknown";

interface CapabilityRowView {
  id: string;
  label: string;
  status: CapabilityRowStatus;
  /** One short line in the reader's words; no infrastructure text. */
  detail?: string;
  /** Environment variable NAMES (never values). */
  env: string[];
}

interface CapabilitiesState {
  loading: boolean;
  rows: CapabilityRowView[];
  /** A probe that could not answer, in the order the banner prioritizes. */
  notice: "auth" | "unsupported" | "unreachable" | null;
}

type CapabilityProbeFailure = "auth" | "unsupported" | "unreachable";

interface CapabilityRowSpec {
  id: string;
  label: string;
  /** Wire ids that mean this row (canonicalized before comparison). */
  aliases: string[];
  /** The per-capability model override an operator sets. */
  env: string;
}

/** The rows, in reading order. A media capability row is named after the
 * operation, not the provider family (image generation and image editing are
 * separate answers). */
const MEDIA_CAPABILITY_ROWS: CapabilityRowSpec[] = [
  {
    id: "chat",
    label: "文本对话",
    aliases: ["chat", "text", "llm", "chat-completions", "text-chat"],
    env: "KEEL_MEDIA_MODEL_CHAT",
  },
  {
    id: "image",
    label: "图像生成",
    aliases: ["image", "images", "image-generation", "image-generate", "text-to-image"],
    env: "KEEL_MEDIA_MODEL_IMAGE",
  },
  {
    id: "image-edit",
    label: "图像编辑",
    aliases: ["image-edit", "image-editing", "edit-image", "image-editor"],
    env: "KEEL_MEDIA_MODEL_IMAGE_EDIT",
  },
  {
    id: "decision",
    label: "决策",
    aliases: ["decision", "decisions", "decide", "system-one", "systemone", "noul"],
    env: "KEEL_MEDIA_MODEL_DECISION",
  },
  {
    id: "video",
    label: "视频",
    aliases: ["video", "videos", "video-generation", "video-generate", "text-to-video"],
    env: "KEEL_MEDIA_MODEL_VIDEO",
  },
  {
    id: "tts",
    label: "语音合成",
    aliases: ["tts", "speech", "speech-synthesis", "text-to-speech", "audio-tts"],
    env: "KEEL_MEDIA_MODEL_TTS",
  },
  {
    id: "stt",
    label: "语音识别",
    aliases: ["stt", "transcription", "speech-recognition", "speech-to-text", "audio-stt", "transcribe"],
    env: "KEEL_MEDIA_MODEL_STT",
  },
];

const REALTIME_CAPABILITY_ROW: CapabilityRowSpec = {
  id: "realtime",
  label: "实时语音",
  aliases: ["realtime", "real-time", "voice", "live-voice"],
  env: "KEEL_REALTIME_MODEL",
};

const CAPABILITY_ROWS: CapabilityRowSpec[] = [...MEDIA_CAPABILITY_ROWS, REALTIME_CAPABILITY_ROW];

/** The gateway-level variables, shown when the media gateway itself is absent. */
const MEDIA_GATEWAY_ENV = ["KEEL_MEDIA_BASE_URL", "KEEL_MEDIA_API_KEY"];
/** The voice bridge's variables (the built-in provider reads exactly these). */
const REALTIME_ENV = ["KEEL_REALTIME_WS_URL", "KEEL_REALTIME_API_KEY", "KEEL_REALTIME_MODEL"];

const canonicalCapabilityId = (value: string): string =>
  value
    .trim()
    .replace(/([a-z0-9])([A-Z])/g, "$1-$2")
    .toLowerCase()
    .replace(/[_.\s]+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "");

const capabilityRowIdOf = (value: string): string => {
  const id = canonicalCapabilityId(value);
  const spec = CAPABILITY_ROWS.find((row) => row.id === id || row.aliases.includes(id));
  return spec?.id ?? id;
};

const capabilitiesState = signal<CapabilitiesState>({
  loading: true,
  rows: CAPABILITY_ROWS.map((row) => ({ id: row.id, label: row.label, status: "unknown", env: [] })),
  notice: null,
});

/** The probes' transport. Lazy, so a harness that never opens the section
 * never constructs one. */
let capabilityClient: KeelHttp | null = null;
let capabilityFetch: FetchLike | null = null;

/** Test/host seam: answers the two probes without a network. The product
 * never calls this — a real run leaves it null and the transport uses the
 * platform's `globalThis.fetch`, same origin as the app. The TUI harness needs
 * it because an in-memory terminal has no origin for a relative URL to resolve
 * against. */
export const setCapabilityFetch = (fetchImpl: FetchLike | null): void => {
  capabilityFetch = fetchImpl;
  capabilityClient = null;
};

const capabilityTransport = (): DeploymentCapabilitiesApi =>
  (capabilityClient ??= new KeelHttp("", capabilityFetch ?? undefined));

/** Bumped per load: a section switch unmounts and remounts, and the older
 * answer must not overwrite the newer one. */
let capabilityLoadToken = 0;

const capabilityFailureOf = (error: unknown): CapabilityProbeFailure => {
  if (error instanceof KeelApiError) {
    if (error.status === 401 || error.status === 403) return "auth";
    if (error.status === 404) return "unsupported";
  }
  return "unreachable";
};

const capabilityFailureRow = (spec: CapabilityRowSpec, failure: CapabilityProbeFailure): CapabilityRowView => {
  if (failure === "auth") {
    return { id: spec.id, label: spec.label, status: "auth", detail: "需要登录后才能读取。", env: [] };
  }
  if (failure === "unsupported") {
    return { id: spec.id, label: spec.label, status: "unsupported", detail: "当前服务端不支持能力探测。", env: [] };
  }
  return { id: spec.id, label: spec.label, status: "unknown", detail: "暂时无法读取状态。", env: [] };
};

const mediaCapabilityRowViews = (
  view: MediaCapabilitiesView | null,
  failure: CapabilityProbeFailure | null,
): CapabilityRowView[] => {
  if (failure) return MEDIA_CAPABILITY_ROWS.map((spec) => capabilityFailureRow(spec, failure));
  const byId = new Map<string, MediaCapabilityView>();
  for (const capability of view?.capabilities ?? []) {
    const id = capabilityRowIdOf(capability.id);
    if (!byId.has(id)) byId.set(id, capability);
  }
  return MEDIA_CAPABILITY_ROWS.map((spec) => {
    const capability = byId.get(spec.id);
    if (!capability) {
      // A complete gateway document that omits the operation is a statement
      // that the deployment does not provide it; a gateway-level "not
      // configured" is the ordinary warning state instead.
      if (view && !view.configured) {
        return {
          id: spec.id,
          label: spec.label,
          status: "unconfigured",
          detail: "未配置；设置以下环境变量后可用。",
          env: [...MEDIA_GATEWAY_ENV, spec.env],
        };
      }
      return view
        ? { id: spec.id, label: spec.label, status: "unsupported", detail: "该部署未提供此能力。", env: [] }
        : capabilityFailureRow(spec, "unreachable");
    }
    if (capability.status === "available") {
      return {
        id: spec.id,
        label: spec.label,
        status: "available",
        detail: capability.model ? `模型：${capability.model}` : "已配置",
        env: [],
      };
    }
    if (capability.status === "unsupported") {
      return {
        id: spec.id,
        label: spec.label,
        status: "unsupported",
        detail: capability.reason ?? "Provider 不支持此能力。",
        env: [],
      };
    }
    const env = capability.env?.length
      ? capability.env
      : view && !view.configured
        ? [...MEDIA_GATEWAY_ENV, spec.env]
        : [spec.env];
    return { id: spec.id, label: spec.label, status: "unconfigured", detail: "未配置；设置以下环境变量后可用。", env };
  });
};

const realtimeCapabilityRowView = (
  view: RealtimeStatusView | null,
  failure: CapabilityProbeFailure | null,
): CapabilityRowView => {
  const spec = REALTIME_CAPABILITY_ROW;
  if (failure) return capabilityFailureRow(spec, failure);
  if (!view) return capabilityFailureRow(spec, "unreachable");
  if (!view.configured) {
    return {
      id: spec.id,
      label: spec.label,
      status: "unconfigured",
      detail: "未配置；设置以下环境变量后可用。",
      env: [...REALTIME_ENV],
    };
  }
  const models = view.models.slice(0, 3).join(" / ");
  return {
    id: spec.id,
    label: spec.label,
    status: "available",
    detail: models ? `模型：${models}` : "已配置",
    env: [],
  };
};

const loadDeploymentCapabilities = async () => {
  const token = ++capabilityLoadToken;
  capabilitiesState.set({ ...capabilitiesState(), loading: true, notice: null });
  const [media, realtime] = await Promise.allSettled([
    capabilityTransport().mediaCapabilities(),
    capabilityTransport().realtimeStatus(),
  ]);
  if (token !== capabilityLoadToken) return;
  const mediaFailure = media.status === "rejected" ? capabilityFailureOf(media.reason) : null;
  const realtimeFailure = realtime.status === "rejected" ? capabilityFailureOf(realtime.reason) : null;
  const rows = [
    ...mediaCapabilityRowViews(media.status === "fulfilled" ? media.value : null, mediaFailure),
    realtimeCapabilityRowView(realtime.status === "fulfilled" ? realtime.value : null, realtimeFailure),
  ];
  const failures = [mediaFailure, realtimeFailure].filter(
    (failure): failure is CapabilityProbeFailure => failure !== null,
  );
  const notice = failures.includes("unreachable")
    ? "unreachable"
    : failures.includes("auth")
      ? "auth"
      : failures.includes("unsupported")
        ? "unsupported"
        : null;
  capabilitiesState.set({ loading: false, rows, notice });
};

const capabilityBadge = (
  status: CapabilityRowStatus,
): { variant: "success" | "warning" | "secondary" | "outline"; label: string } => {
  switch (status) {
    case "available":
      return { variant: "success", label: "可用" };
    case "unconfigured":
      return { variant: "warning", label: "未配置" };
    case "unsupported":
      return { variant: "secondary", label: "不支持" };
    case "auth":
      return { variant: "outline", label: "需要登录" };
    default:
      return { variant: "outline", label: "状态未知" };
  }
};

function CapabilityRow(props: { row: () => CapabilityRowView }) {
  const badge = () => capabilityBadge(props.row().status);
  return (
    <View
      testID={`capability-row-${props.row().id}`}
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
          {props.row().label}
        </Text>
        <Badge testID={`capability-status-${props.row().id}`} variant={badge().variant} label={badge().label} />
        <If when={!!props.row().detail}>
          <Text
            style={{
              flex: 1,
              minWidth: 160,
              fontSize: typeScale.sm,
              lineHeight: Math.round(typeScale.sm * lineHeight.normal),
              color: color.mutedForeground,
            }}
          >
            {props.row().detail ?? ""}
          </Text>
        </If>
      </View>
      <If when={props.row().env.length > 0}>
        <Text
          testID={`capability-env-${props.row().id}`}
          style={{
            fontFamily: font.mono,
            fontSize: typeScale.xs,
            lineHeight: Math.round(typeScale.xs * lineHeight.normal),
            color: color.mutedForeground,
            userSelect: "text",
          }}
        >
          {`需要设置：${props.row().env.join(" · ")}`}
        </Text>
      </If>
    </View>
  );
}

/** The read-only 能力 page. It loads its own two probes on mount: they are
 * facts of the live process, not of the settings document or the sessions
 * store, and the section owns nothing writable. */
export function CapabilitiesSection() {
  effect(() => {
    const timer = setTimeout(() => void loadDeploymentCapabilities(), 0);
    cleanup(() => clearTimeout(timer));
  });
  const count = (status: CapabilityRowStatus): number =>
    capabilitiesState().rows.filter((row) => row.status === status).length;
  const unresolved = (): number => count("auth") + count("unknown");
  return (
    <Card testID="settings-capabilities">
      <CardHeader
        title="能力"
        description="当前部署实际可用的模型能力（只读）。配置来自服务端进程的环境变量，这里只显示变量名，不显示端点或密钥。"
        action={
          <Button
            variant="outline"
            size="sm"
            label="刷新"
            accessibilityLabel="刷新能力状态"
            onPress={() => void loadDeploymentCapabilities()}
          />
        }
      />
      <CardContent>
        <If when={capabilitiesState().loading}>
          <LoadingRow text="正在读取部署能力…" />
        </If>
        <If when={!capabilitiesState().loading}>
          <View style={{ gap: space["3xl"] }}>
            <Text
              testID="capabilities-summary"
              style={{ fontSize: typeScale.sm, color: color.mutedForeground }}
            >
              {`${count("available")} 项可用 · ${count("unconfigured")} 项未配置 · ${count("unsupported")} 项不支持${
                unresolved() > 0 ? ` · ${unresolved()} 项状态未知` : ""
              }`}
            </Text>
            <If when={capabilitiesState().notice === "auth"}>
              <Notice tone="warning" testID="capabilities-notice-auth" accessibilityLabel="需要登录">
                <Text
                  style={{
                    fontSize: typeScale.sm,
                    lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                    color: color.foreground,
                  }}
                >
                  需要登录后才能读取部署能力。
                </Text>
              </Notice>
            </If>
            <If when={capabilitiesState().notice === "unreachable"}>
              <ErrorBlock
                text="暂时无法读取部署能力，请稍后重试。"
                onRetry={() => void loadDeploymentCapabilities()}
              />
            </If>
            <If when={capabilitiesState().notice === "unsupported"}>
              <Text
                testID="capabilities-notice-unsupported"
                style={{ fontSize: typeScale.sm, color: color.mutedForeground }}
              >
                当前服务端不提供部分能力探测；标注「不支持」的行即该情况。
              </Text>
            </If>
            <Each of={capabilitiesState().rows} key={(row) => row.id}>
              {(row) => <CapabilityRow row={row} />}
            </Each>
          </View>
        </If>
      </CardContent>
    </Card>
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
    <If when={settingsSection() === "agents" || settingsSection() === "teams"} else={<SectionBodyGeneral />}>
      <AgentTeamSettingsSection section={() => settingsSection()} />
    </If>
  );
}

function SectionBodyGeneral() {
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
    <If when={settingsSection() === "shortcuts"} else={<SectionBodyCapabilities />}>
      <ShortcutsSection />
    </If>
  );
}

function SectionBodyCapabilities() {
  return (
    <If when={settingsSection() === "capabilities"} else={<AboutSection />}>
      <CapabilitiesSection />
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
