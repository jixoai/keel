// The settings page's「Agents」and「团队」sections (change
// keel-agent-team-v1, task P5).
//
// Two dedicated sections instead of one scrolling pile of modules: a reader
// who wants to see who works here opens Agents; a reader who wants a team
// opens 团队. Each section is one card of cards — an agent card says the
// name, the default model, how many tools it holds and what it is for; a team
// card says the name, **who captains it** (the mandatory seat is printed as a
// line of its own, not left to a dropdown), the roster size and the member
// avatars.
//
// Data comes from the store's agent/team face (`GET /v1/agents`,
// `GET/POST/PUT/DELETE /v1/teams`, member writes under
// `/v1/teams/{id}/members`). The editor dialogs keep their typing in module
// state and the seats in signals, so a refusal leaves every field exactly as
// the reader typed it; the store's `*ActionError` is what the dialog prints.
//
// Five states are drawn in each section: loading (skeleton cards), empty (a
// guide with the one next action), configured, saving (the footer's spinner
// and disabled fields) and failed (the server's reason, verbatim, next to the
// form). Keyboard: the kit Dialog closes on Escape, every control is a kit
// control (Tab reaches it), and Enter in a single-line field submits.
import { cleanup, effect, Scroll, signal, Text, View, windowSize } from "nsh";
import type {
  AgentTeamView,
  AgentTeamWriteRequest,
  AgentToolGrantView,
  AgentView,
  AgentWriteRequest,
  TeamMemberView,
} from "../data/contract";
import { store } from "../data/store";
import { displayNameOf, providerDisplayName } from "../data/wire";
import { Icon } from "../ui/icons";
import {
  AlertDialog,
  Avatar,
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  Combobox,
  Dialog,
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyMedia,
  EmptyTitle,
  Field,
  FieldGroup,
  Input,
  Select,
  Skeleton,
  Tab,
  Tabs,
  Textarea,
  color,
  iconSize,
  lineHeight,
  space,
  typeScale,
  weight,
  type SelectItem,
} from "../ui/kit";

// ---- shared reads ---------------------------------------------------------

/** A seat's display name: the stored agent, the seat's own label, else a
 * readable "not loaded" word. A raw agent id is never a display name — the
 * agents registry can be forbidden while the teams registry is readable. */
const agentNameOf = (agentId: string, label?: string): string => {
  const found = store.agents().find((agent) => agent.id === agentId);
  return found?.name || label || "未载入的 Agent";
};

const providerNameOf = (providerId: string | undefined): string => {
  if (!providerId) return "";
  const provider = store.providers().find((item) => item.providerId === providerId);
  return provider ? providerDisplayName(provider) : displayNameOf(providerId);
};

const modelNameOf = (agent: AgentView): string => {
  const binding = agent.defaultModel;
  if (!binding?.modelId) return "未指定默认模型";
  return displayNameOf(binding.modelId);
};

const modelLabelOf = (agent: AgentView): string => {
  const binding = agent.defaultModel;
  if (!binding?.modelId) return "未指定默认模型";
  const provider = providerNameOf(binding.providerId);
  const model = displayNameOf(binding.modelId);
  return provider ? `${model} · ${provider}` : model;
};

const toolLabelOf = (agent: AgentView): string => {
  const count = agent.tools?.length ?? 0;
  return count > 0 ? `工具 ${count} 个` : "未授予工具";
};

/** One model option's value: `provider/model`. The wire ids are plain tokens,
 * so the first slash is the binding's own separator. */
const modelValueOf = (agent: AgentView | null | undefined): string =>
  agent?.defaultModel?.modelId ? `${agent.defaultModel.providerId ?? ""}/${agent.defaultModel.modelId}` : "";

const splitModelValue = (value: string): { providerId: string; modelId: string } => {
  const at = value.indexOf("/");
  if (at < 0) return { providerId: "", modelId: value };
  return { providerId: value.slice(0, at), modelId: value.slice(at + 1) };
};

const optionModelLabel = (providerId: string, modelId: string): string => {
  const provider = providerNameOf(providerId);
  const model = displayNameOf(modelId);
  return provider ? `${model} · ${provider}` : model;
};

/** Every model this deployment can run, grouped by provider. The provider's
 * display name is the group header, so a long list reads as a few short
 * sections, not one flat run of ids. */
const modelOptions = (): SelectItem[] => {
  const items: SelectItem[] = [];
  const seen = new Set<string>();
  for (const model of store.models()) {
    if (model.selectable === false || model.enabled === false) continue;
    const value = `${model.providerId}/${model.modelId}`;
    if (seen.has(value)) continue;
    seen.add(value);
    items.push({
      value,
      title: optionModelLabel(model.providerId, model.modelId),
      group: providerNameOf(model.providerId) || model.providerId,
    });
  }
  return items;
};

/** The tools on offer: the installed registry (`GET /v1/plugins` tools) plus
 * everything already declared on an agent — a grant that a plugin has since
 * stopped advertising must stay visible/removable, not vanish. */
const toolOptions = (extra: string[]): SelectItem[] => {
  const ids = new Set<string>(store.plugins()?.tools ?? []);
  for (const agent of store.agents()) {
    for (const grant of agent.tools ?? []) ids.add(grant.toolId);
  }
  for (const id of extra) ids.add(id);
  return [...ids].sort().map((id) => ({ value: id, title: id }));
};

// ---- agent draft ----------------------------------------------------------

// The text fields are plain module state, not signals, on purpose: a
// modal-hosted input that re-renders per keystroke restarts the TUI host's CJK
// composition after its first committed character (typing 发布助手 left only
// 发). The typed text is written on every change and read at submit; the
// dialog's reactive reads are the coarse facts (open, editing id, model,
// tools, saving, refusal), so typing re-renders nothing. The uncontrolled
// inputs take their initial text from this record, so a later re-render (a
// model pick, a refusal) restores exactly what was typed instead of clearing
// it.
let agentText = { name: "", description: "", prompt: "", promptFile: "", avatarUrl: "" };
const agentOpen = signal(false);
const agentEditingId = signal<string | null>(null);
const agentModel = signal("");
const agentTools = signal<string[]>([]);
const agentNameError = signal<string | null>(null);
/** Which prompt source the editor is editing: the two wire kinds, explicit.
 * The old heuristic (a .md-looking line became a file ref) is gone: a path is
 * a path only when the reader chose the file tab. */
const agentPromptMode = signal<"inline" | "file">("inline");
const agentPromptError = signal<string | null>(null);
const confirmingAgent = signal<AgentView | null>(null);

const editAgent = (agent?: AgentView) => {
  agentNameError.set(null);
  agentPromptError.set(null);
  store.clearAgentActionError();
  // A document written before kinds were finite can carry a ref without the
  // kind; the ref is still the fact, so it opens on the file tab.
  const filePrompt = agent?.systemPrompt?.kind === "file" || (agent?.systemPrompt?.kind !== "inline" && !!agent?.systemPrompt?.ref);
  agentText = {
    name: agent?.name ?? "",
    description: agent?.description ?? "",
    prompt: filePrompt ? "" : (agent?.systemPrompt?.text ?? ""),
    promptFile: filePrompt ? (agent?.systemPrompt?.ref ?? "") : "",
    avatarUrl: agent?.avatar?.url ?? "",
  };
  agentPromptMode.set(filePrompt ? "file" : "inline");
  agentModel.set(modelValueOf(agent));
  agentTools.set((agent?.tools ?? []).map((grant) => grant.toolId));
  agentEditingId.set(agent?.id ?? null);
  agentOpen.set(true);
};

const closeAgentEditor = () => agentOpen.set(false);

const selectPromptMode = (mode: "inline" | "file") => {
  agentPromptMode.set(mode);
  // A validation message belongs to the source that produced it; switching
  // tabs clears it while both typed values stay in `agentText`.
  agentPromptError.set(null);
};

/** The file reference's own validation, mirroring `agentteam.safePromptRef`:
 * non-empty, relative to the workspace root, never escaping it. The check is
 * local because the editor must explain the path before a round trip; the
 * server still owns the final word (its refusal is shown verbatim). */
const promptRefError = (raw: string): string | null => {
  const ref = raw.trim();
  if (!ref) return "文件引用不能为空：例如 AGENTS.md。";
  if (/[\r\n]/.test(ref)) return "文件路径不能包含换行。";
  const normalized = ref.replace(/\\/g, "/");
  if (normalized.startsWith("/") || /^[A-Za-z]:\//.test(normalized) || normalized.startsWith("//")) {
    return "必须使用相对工作区根目录的路径，不能用绝对路径。";
  }
  const parts: string[] = [];
  for (const part of normalized.split("/")) {
    if (!part || part === ".") continue;
    if (part === "..") {
      if (parts.length === 0) return "路径不能越出工作区：不能以 .. 开头或越过工作区根目录。";
      parts.pop();
      continue;
    }
    parts.push(part);
  }
  if (parts.length === 0) return "请填写具体文件，例如 AGENTS.md。";
  return null;
};

/** The system prompt from the explicit source tab. `{}` means no prompt (the
 * run then keeps the deployment prompt); a file tab with an invalid ref
 * returns the field error instead of a request the server must refuse. */
const promptValueOf = (): { value: AgentWriteRequest["systemPrompt"]; error: string | null } => {
  if (agentPromptMode() === "file") {
    const ref = agentText.promptFile.trim();
    const error = promptRefError(ref);
    return { value: error ? {} : { kind: "file", ref }, error };
  }
  const text = agentText.prompt.trim();
  return { value: text ? { kind: "inline", text } : {}, error: null };
};

const toolGrantsOf = (ids: string[], previous: AgentToolGrantView[] | undefined): AgentToolGrantView[] => {
  const kept = new Map((previous ?? []).map((grant) => [grant.toolId, grant]));
  return ids.map((toolId) => kept.get(toolId) ?? { toolId, enabled: true });
};

const submitAgent = () => {
  const name = agentText.name.trim();
  if (!name) {
    agentNameError.set("Agent 名字不能为空");
    return;
  }
  agentNameError.set(null);
  const prompt = promptValueOf();
  if (prompt.error) {
    agentPromptError.set(prompt.error);
    return;
  }
  agentPromptError.set(null);
  const editingId = agentEditingId();
  const editing = editingId ? store.agents().find((agent) => agent.id === editingId) : undefined;
  const model = agentModel();
  const selection = model ? splitModelValue(model) : null;
  const request: AgentWriteRequest = {
    name,
    description: agentText.description.trim(),
    avatar: agentText.avatarUrl.trim() ? { kind: "url", url: agentText.avatarUrl.trim() } : {},
    defaultModel: selection ? { providerId: selection.providerId, modelId: selection.modelId } : {},
    tools: toolGrantsOf(agentTools(), editing?.tools),
    systemPrompt: prompt.value,
  };
  void store.saveAgent(request, editingId ?? undefined).then((ok) => {
    if (ok) closeAgentEditor();
  });
};

// ---- team draft -----------------------------------------------------------

// Same rule as the agent editor: typed text is plain state (composition safe),
// the seats are signals (a pick is a deliberate, one-step change), and the
// uncontrolled inputs restore from the record on any re-render.
let teamText = { name: "", description: "" };
const teamOpen = signal(false);
const teamEditingId = signal<string | null>(null);
const teamCaptain = signal("");
const teamMembers = signal<string[]>([]);
const teamNameError = signal<string | null>(null);
const teamCaptainError = signal<string | null>(null);
const confirmingTeam = signal<AgentTeamView | null>(null);

const setTeamCaptain = (agentId: string | null) => {
  const captain = agentId ?? "";
  teamCaptain.set(captain);
  if (teamCaptainError()) teamCaptainError.set(null);
  // The captain is a seat, not a member: picking one removes it from the
  // additional roster, so the same person can never sit twice.
  teamMembers.set(teamMembers().filter((member) => member !== captain));
};

const editTeam = (team?: AgentTeamView) => {
  teamNameError.set(null);
  teamCaptainError.set(null);
  store.clearTeamActionError();
  teamText = { name: team?.name ?? "", description: team?.description ?? "" };
  teamCaptain.set(team?.captain.agentId ?? "");
  teamMembers.set((team?.members ?? []).map((member) => member.agentId));
  teamEditingId.set(team?.id ?? null);
  teamOpen.set(true);
};

const closeTeamEditor = () => teamOpen.set(false);

const submitTeam = () => {
  const name = teamText.name.trim();
  const captain = teamCaptain();
  let invalid = false;
  if (!name) {
    teamNameError.set("团队名字不能为空");
    invalid = true;
  } else {
    teamNameError.set(null);
  }
  if (!captain) {
    teamCaptainError.set("团队必须有队长");
    invalid = true;
  } else {
    teamCaptainError.set(null);
  }
  if (invalid) return;

  const editingId = teamEditingId();
  const editing = editingId ? store.teams().find((team) => team.id === editingId) : undefined;
  const members: TeamMemberView[] = teamMembers()
    .filter((agentId) => agentId !== captain)
    .map((agentId) => ({ agentId }));
  const request: AgentTeamWriteRequest = {
    name,
    description: teamText.description.trim(),
    captain: { agentId: captain },
    members,
  };
  if (editing) {
    // PUT replaces the team document: carry the parts this form does not edit
    // so a rename never wipes defaults, capabilities or feature bindings.
    request.id = editing.id;
    if (editing.defaults) request.defaults = editing.defaults;
    if (editing.capabilities) request.capabilities = editing.capabilities;
    if (editing.featureBindings) request.featureBindings = editing.featureBindings;
  }

  const rosterOnly =
    editing !== undefined &&
    editing.name === name &&
    (editing.description ?? "") === request.description &&
    editing.captain.agentId === captain;
  const finish = (ok: boolean) => {
    if (ok) closeTeamEditor();
  };
  if (rosterOnly && editing) {
    // Only seats changed: use the member routes, one seat at a time.
    const before = new Set((editing.members ?? []).map((member) => member.agentId));
    const after = new Set(teamMembers());
    const calls: Promise<boolean>[] = [];
    for (const agentId of after) if (!before.has(agentId)) calls.push(store.addTeamMember(editing.id, agentId));
    for (const agentId of before) if (!after.has(agentId)) calls.push(store.removeTeamMember(editing.id, agentId));
    void (async () => {
      let ok = true;
      for (const call of calls) ok = (await call) && ok;
      finish(ok);
    })();
    return;
  }
  void store.saveTeam(request, editing?.id).then(finish);
};

// ---- small shared pieces --------------------------------------------------

/** The section-level failure line: the server's reason, verbatim, with a
 * retry. The kit has no banner; this is the same Card retint settings.tsx
 * uses for its notices. */
function SectionError(props: { text: string; onRetry?: () => void; testID?: string }) {
  return (
    <Card
      testID={props.testID}
      accessibilityLabel="加载失败"
      style={{ gap: space.sm, padding: space.md, backgroundColor: color.dangerTint, borderColor: color.destructive }}
    >
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
        <If when={props.onRetry !== undefined}>
          <Button variant="outline" size="sm" label="重试" accessibilityLabel="重试" onPress={props.onRetry} />
        </If>
      </View>
    </Card>
  );
}

/** The permission state: a 403 is not a failure of the section and not a lost
 * network — it is the server saying this reader may not see the registry. It
 * gets its own face (warning, a key, the server's reason verbatim, retry), so
 * "加载失败" never mislabels a permission decision. */
function SectionPermission(props: { scope: string; reason: string; onRetry?: () => void }) {
  return (
    <Card
      testID="settings-permission"
      accessibilityLabel={`没有查看${props.scope}的权限`}
      style={{ gap: space.sm, padding: space.md, backgroundColor: color.warningTint, borderColor: color.warning }}
    >
      <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.md }}>
        <Icon name="key" size={iconSize.sm} tint={color.warning} />
        <View style={{ flex: 1, flexDirection: "column", gap: space.xxs }}>
          <Text style={{ fontSize: typeScale.sm, fontWeight: weight.semibold, color: color.foreground }}>
            {`没有查看${props.scope}的权限`}
          </Text>
          <Text
            style={{
              fontSize: typeScale.sm,
              lineHeight: Math.round(typeScale.sm * lineHeight.normal),
              color: color.foreground,
            }}
          >
            {props.reason || "当前账号没有访问这里所需的权限；请联系管理员分配后再试。"}
          </Text>
        </View>
        <If when={props.onRetry !== undefined}>
          <Button variant="outline" size="sm" label="重试" accessibilityLabel="重试" onPress={props.onRetry} />
        </If>
      </View>
    </Card>
  );
}

function SectionLoading(props: { text: string; testID?: string }) {
  return (
    <View testID={props.testID} style={{ gap: space.md }}>
      <Text style={{ fontSize: typeScale.sm, color: color.mutedForeground }}>{props.text}</Text>
      <Skeleton height={64} />
      <Skeleton height={64} />
      <Skeleton height={64} />
    </View>
  );
}

const mutedProse = {
  fontSize: typeScale.sm,
  lineHeight: Math.round(typeScale.sm * lineHeight.normal),
  color: color.mutedForeground,
} as const;

const factText = {
  fontSize: typeScale.xs,
  color: color.mutedForeground,
} as const;

/** The card grid's single-column breakpoint (the design's ≤900px): the model
 * badge keeps only the model name there (no "默认模型" wording or provider),
 * so the value itself is not the part that gets ellipsized in a 184px content
 * column. The full label stays in the badge's accessibility label. */
const narrowCards = () => windowSize().width <= 900;

/** The editor body's scroll height: the dialog has a header and footer, so the
 * form gets what the window leaves after them — more than the old fixed 430 on
 * a 390×844 screen (where the last field otherwise ended at the viewport
 * edge), less on a short window. */
const editorScrollHeight = () => Math.min(520, Math.max(240, windowSize().height - 280));

// ---- Agents ---------------------------------------------------------------

function AgentCard(props: { agent: () => AgentView }) {
  return (
    <Card
      size="sm"
      testID={`agent-card-${props.agent().id}`}
      accessibilityLabel={`Agent ${props.agent().name}`}
    >
      <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.md }}>
        <Avatar src={props.agent().avatar?.url ?? null} name={props.agent().name} size="lg" />
        <View style={{ flex: 1, flexDirection: "column", gap: space.xs }}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
            <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>
              {props.agent().name}
            </Text>
            <If when={props.agent().archived === true}>
              <Badge variant="secondary" label="已归档" />
            </If>
            <View style={{ flex: 1 }} />
            <Button
              variant="ghost"
              size="sm"
              label="编辑"
              accessibilityLabel={`编辑 Agent ${props.agent().name}`}
              onPress={() => editAgent(props.agent())}
            />
            <Button
              variant="ghost"
              size="sm"
              icon="trash"
              label="删除"
              accessibilityLabel={`删除 Agent ${props.agent().name}`}
              onPress={() => {
                store.clearAgentActionError();
                confirmingAgent.set(props.agent());
              }}
            />
          </View>
          <Text style={mutedProse}>{props.agent().description || "还没有描述——编辑里补一句它能做什么。"}</Text>
          <View
            style={{
              // One fact per line at the single column: in a row the model
              // badge would shrink to leave room for its neighbours instead of
              // wrapping, ellipsizing the model name; a column gives every
              // fact the card's full width.
              flexDirection: narrowCards() ? "column" : "row",
              alignItems: narrowCards() ? "flex-start" : "center",
              gap: space.sm,
              flexWrap: "wrap",
            }}
          >
            {/* The model is the longest fact. At the single column the badge
                keeps the brain glyph and spends its text on the model name;
                the wide form adds the "默认模型" wording and provider. */}
            <Badge
              variant="outline"
              // The single column has ~184px for the card's content: the glyph
              // plus the "默认模型" wording would push the value into an
              // ellipsis, so the narrow card keeps the brain glyph and spends
              // the text on the model itself; the full wording stays in the
              // accessibility label.
              icon="brain"
              truncate
              style={{ maxWidth: "100%" }}
              label={
                narrowCards()
                  ? modelNameOf(props.agent())
                  : `默认模型 ${modelLabelOf(props.agent())}`
              }
              accessibilityLabel={`默认模型 ${modelLabelOf(props.agent())}`}
            />
            <Badge variant="outline" icon="tool" label={toolLabelOf(props.agent())} />
            <Text
              style={{
                ...factText,
                maxWidth: "100%",
                numberOfLines: 1,
                textOverflow: "ellipsis",
              }}
              testID={`agent-prompt-fact-${props.agent().id}`}
            >
              {props.agent().systemPrompt?.kind === "file" || props.agent().systemPrompt?.ref
                ? `文件引用 · ${props.agent().systemPrompt?.ref ?? ""}`
                : props.agent().systemPrompt?.text
                  ? "内联系统提示词"
                  : "未设置系统提示词"}
            </Text>
          </View>
        </View>
      </View>
    </Card>
  );
}

function AgentEditor() {
  return (
    <Dialog
      testID="agent-dialog"
      open={agentOpen()}
      size="lg"
      title={agentEditingId() === null ? "新建 Agent" : "编辑 Agent"}
      description="Agent 是团队里的一位成员：名字、默认模型和工具决定它能做什么。只填名字也可以创建。"
      onOpenChange={(event) => {
        if (!event.open) closeAgentEditor();
      }}
      footer={
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
          <Button variant="ghost" label="取消" onPress={closeAgentEditor} />
          <Button
            testID="agent-save"
            label={store.agentsSaving() ? "保存中…" : agentEditingId() === null ? "创建" : "保存"}
            loading={store.agentsSaving()}
            disabled={store.agentsSaving()}
            onPress={submitAgent}
          />
        </View>
      }
    >
      <Scroll testID="agent-form" style={{ maxHeight: editorScrollHeight() }}>
        <FieldGroup>
          <Field label="名字" required error={agentNameError() ?? ""} hint="团队卡片、会话里都用这个名字。">
            <Input
              testID="agent-name"
              accessibilityLabel="Agent 名字"
              defaultValue={agentText.name}
              placeholder="例如 设计师"
              maxLength={64}
              disabled={store.agentsSaving()}
              onChange={(value) => {
                agentText.name = value;
                // A field's own complaint belongs to the value that produced
                // it; typing a new name clears it before the next submit.
                if (agentNameError()) agentNameError.set(null);
              }}
              onSubmit={submitAgent}
            />
          </Field>
          <Field label="描述" hint="一句话说明它负责什么；会显示在卡片上。">
            <Input
              testID="agent-description"
              accessibilityLabel="Agent 描述"
              defaultValue={agentText.description}
              placeholder="例如 负责界面与品牌视觉"
              maxLength={240}
              disabled={store.agentsSaving()}
              onChange={(value) => {
                agentText.description = value;
              }}
              onSubmit={submitAgent}
            />
          </Field>
          <Field
            label="默认模型"
            hint={
              modelOptions().length > 0
                ? "只是默认值：会话或团队可以覆盖它。"
                : "还没有可选模型；先只填名字创建，稍后到「模型接入」配置。"
            }
          >
            <Select
              testID="agent-model"
              accessibilityLabel="选择默认模型"
              items={modelOptions()}
              value={agentModel()}
              placeholder="选择默认模型"
              searchable
              disabled={store.agentsSaving() || modelOptions().length === 0}
              onChange={(value) => agentModel.set(value ?? "")}
            />
          </Field>
          <Field label="工具" hint="来自已安装的工具注册源；不选表示不授予工具。">
            <Combobox
              testID="agent-tools"
              accessibilityLabel="选择 Agent 工具"
              items={toolOptions(agentTools())}
              multiple
              value={agentTools()}
              placeholder="搜索并选择工具"
              disabled={store.agentsSaving()}
              onChange={(values) => agentTools.set(values)}
            />
          </Field>
          <Field
            label="系统提示词"
            required={agentPromptMode() === "file"}
            hint={
              agentPromptMode() === "file"
                ? "相对工作区根目录的路径（例如 AGENTS.md 或 prompts/reviewer.md）。不在页面里预览文件内容：每次 run 由服务端读取；读取失败会降级为无该段并留诊断。"
                : "直接写提示词；留空表示不设置。文件引用与内联文本二选一，切换来源不会丢另一边的输入。"
            }
          >
            <View style={{ flexDirection: "column", gap: space.md }}>
              <Tabs
                testID="agent-prompt-source"
                accessibilityLabel="系统提示词来源"
                variant="default"
              >
                <Tab
                  testID="agent-prompt-mode-inline"
                  label="内联文本"
                  selected={agentPromptMode() === "inline"}
                  disabled={store.agentsSaving()}
                  onPress={() => selectPromptMode("inline")}
                />
                <Tab
                  testID="agent-prompt-mode-file"
                  label="文件引用"
                  selected={agentPromptMode() === "file"}
                  disabled={store.agentsSaving()}
                  onPress={() => selectPromptMode("file")}
                />
              </Tabs>
              <If
                when={agentPromptMode() === "inline"}
                else={
                  <View style={{ flexDirection: "column", gap: space.xs }}>
                    {/* The reason sits above the input, where the reader is
                        after pressing save; below it the field's bottom edge
                        is the scroll viewport's edge on a 390px screen. */}
                    <If when={!!agentPromptError()}>
                      <View
                        testID="agent-prompt-error"
                        role="alert"
                        style={{ flexDirection: "row", alignItems: "center", gap: space.xs }}
                      >
                        <Icon name="alert" size={12} tint={color.danger} />
                        <Text
                          style={{
                            flex: 1,
                            fontSize: typeScale.sm,
                            lineHeight: Math.round(typeScale.sm * lineHeight.normal),
                            color: color.danger,
                          }}
                        >
                          {agentPromptError() ?? ""}
                        </Text>
                      </View>
                    </If>
                    <Input
                      testID="agent-prompt-file"
                      accessibilityLabel="提示词文件路径"
                      defaultValue={agentText.promptFile}
                      placeholder="AGENTS.md 或 prompts/reviewer.md"
                      maxLength={512}
                      invalid={!!agentPromptError()}
                      disabled={store.agentsSaving()}
                      onChange={(value) => {
                        agentText.promptFile = value;
                        // The path complaint is re-checked on save; a new path
                        // clears the old one's message while it is typed.
                        if (agentPromptError()) agentPromptError.set(null);
                      }}
                      onSubmit={submitAgent}
                    />
                    <Text
                      style={{
                        fontSize: typeScale.xs,
                        lineHeight: Math.round(typeScale.xs * lineHeight.normal),
                        color: color.mutedForeground,
                      }}
                    >
                      文件引用在 run 时读取；路径越出工作区会被服务端拒绝，编辑器也会先在本地校验。
                    </Text>
                  </View>
                }
              >
                <Textarea
                  testID="agent-prompt"
                  accessibilityLabel="内联系统提示词"
                  defaultValue={agentText.prompt}
                  placeholder="例如 你是资深视觉设计师，先给方案再动手。"
                  rows={3}
                  maxLength={4000}
                  disabled={store.agentsSaving()}
                  onChange={(value) => {
                    agentText.prompt = value;
                    if (agentPromptError()) agentPromptError.set(null);
                  }}
                />
              </If>
            </View>
          </Field>
          <Field label="头像" hint="留空使用按 Agent id 生成的 identicon；也可以填一个图片 URL。">
            <Input
              testID="agent-avatar"
              accessibilityLabel="头像地址"
              defaultValue={agentText.avatarUrl}
              placeholder="https://…（可留空）"
              maxLength={512}
              disabled={store.agentsSaving()}
              onChange={(value) => {
                agentText.avatarUrl = value;
              }}
              onSubmit={submitAgent}
            />
          </Field>
        </FieldGroup>
      </Scroll>
      {/* The refusal is outside the form's scroll: a 390px dialog ends its
          viewport at the last field, so a reason printed below it would be
          invisible exactly when the reader needs it. */}
      <If when={!!store.agentsActionError()}>
        <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm }}>
          <Icon name="alert" size={iconSize.sm} tint={color.danger} />
          <Text
            testID="agent-save-error"
            style={{
              flex: 1,
              fontSize: typeScale.sm,
              lineHeight: Math.round(typeScale.sm * lineHeight.normal),
              color: color.danger,
            }}
          >
            保存失败：{store.agentsActionError() ?? ""}
          </Text>
        </View>
      </If>
    </Dialog>
  );
}

export function AgentsSection() {
  // The section loads itself (one read on mount, like 能力): the store keeps
  // the rows, the section never holds a second copy. Tools and models come
  // from the same settings transport, loaded only when absent so a section
  // switch does not re-read what is already on screen.
  effect(() => {
    const timer = setTimeout(() => {
      void store.loadAgents();
      if (store.plugins() === null) void store.loadPlugins();
      if (!store.registryLoaded()) void store.loadRegistry();
    }, 0);
    cleanup(() => clearTimeout(timer));
  });
  const agents = () => store.agents().filter((agent) => agent.archived !== true);
  return (
    <View style={{ gap: space["3xl"] }}>
      <Card testID="settings-agents">
        <CardHeader
          title="Agents"
          description="每个 Agent 是一位可被团队引用的成员：名字、默认模型、工具与系统提示词。团队从这里取值，不需要再配一遍。"
          action={
            <Button
              testID="agent-create"
              size="sm"
              icon="plus"
              label="新建 Agent"
              accessibilityLabel="新建 Agent"
              // The first read has nothing to edit: a dialog opened over the
              // skeleton would show empty pickers and then remount under the
              // reader. The empty and configured states keep the action.
              disabled={store.agentsLoading() && store.agents().length === 0}
              onPress={() => editAgent()}
            />
          }
        />
        <CardContent>
          <If when={store.agentsLoading() && store.agents().length === 0}>
            <SectionLoading testID="agents-loading" text="正在读取 Agent 清单…" />
          </If>
          <If when={store.agentsErrorStatus() === 403}>
            <SectionPermission
              scope=" Agent 注册表"
              reason={store.agentsError() ?? ""}
              onRetry={() => void store.loadAgents()}
            />
          </If>
          <If when={!!store.agentsError() && store.agentsErrorStatus() !== 403}>
            <SectionError
              testID="agents-error"
              text={store.agentsError() ?? ""}
              onRetry={() => void store.loadAgents()}
            />
          </If>
          <If when={!store.agentsLoading() && !store.agentsError() && agents().length === 0}>
            <Empty accessibilityLabel="还没有 Agent">
              <EmptyMedia icon="brain" tone="brand" />
              <EmptyTitle label="还没有 Agent，创建一个" />
              <EmptyDescription label="只填名字就能创建；默认模型与工具可以之后再加。团队至少要有一位队长，所以先建 Agent。" />
              <EmptyContent>
                <Button
                  icon="plus"
                  label="新建 Agent"
                  accessibilityLabel="空态新建 Agent"
                  onPress={() => editAgent()}
                />
              </EmptyContent>
            </Empty>
          </If>
          <If when={!store.agentsError() && agents().length > 0}>
            <Text style={factText}>
              {`${agents().length} 位 Agent${store.models().length > 0 ? ` · 本机可选模型 ${store.models().length} 个` : ""}`}
            </Text>
          </If>
          <Each of={agents()} key={(agent) => agent.id}>
            {(agent) => <AgentCard agent={agent} />}
          </Each>
        </CardContent>
      </Card>

      <AgentEditor />

      <AlertDialog
        testID="agent-delete-dialog"
        open={confirmingAgent() !== null}
        destructive
        title={`删除 Agent ${confirmingAgent()?.name ?? ""}？`}
        description="被团队引用的 Agent 不能删除：服务端会拒绝并说明是哪支团队。删除后不可撤销。"
        confirmLabel={`确认删除 Agent ${confirmingAgent()?.name ?? ""}`}
        cancelLabel="取消删除"
        onConfirm={() => {
          const agent = confirmingAgent();
          if (!agent) return;
          void store.deleteAgent(agent.id).then((ok) => {
            if (ok) confirmingAgent.set(null);
          });
        }}
        onCancel={() => {
          store.clearAgentActionError();
          confirmingAgent.set(null);
        }}
      >
        <If when={!!store.agentsActionError()}>
          <Text
            testID="agent-delete-error"
            style={{
              fontSize: typeScale.sm,
              lineHeight: Math.round(typeScale.sm * lineHeight.normal),
              color: color.danger,
            }}
          >
            删除失败：{store.agentsActionError() ?? ""}
          </Text>
        </If>
      </AlertDialog>
    </View>
  );
}

// ---- 团队 -----------------------------------------------------------------

function TeamCard(props: { team: () => AgentTeamView }) {
  const members = () => props.team().members ?? [];
  const captainName = () => agentNameOf(props.team().captain.agentId, props.team().captain.label);
  return (
    <Card size="sm" testID={`team-card-${props.team().id}`} accessibilityLabel={`团队 ${props.team().name}`}>
      <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.md }}>
        <Avatar name={props.team().name} size="lg" />
        <View style={{ flex: 1, flexDirection: "column", gap: space.xs }}>
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
            <Text style={{ fontSize: typeScale.base, fontWeight: weight.semibold, color: color.foreground }}>
              {props.team().name}
            </Text>
            <If when={props.team().status === "valid"}>
              <Badge variant="success" icon="check" label="可用" />
            </If>
            <If when={props.team().status === "invalid"}>
              <Badge variant="destructive" icon="alert" label="不可用" />
            </If>
            <View style={{ flex: 1 }} />
            <Button
              variant="ghost"
              size="sm"
              label="编辑"
              accessibilityLabel={`编辑团队 ${props.team().name}`}
              onPress={() => editTeam(props.team())}
            />
            <Button
              variant="ghost"
              size="sm"
              icon="trash"
              label="删除"
              accessibilityLabel={`删除团队 ${props.team().name}`}
              onPress={() => {
                store.clearTeamActionError();
                confirmingTeam.set(props.team());
              }}
            />
          </View>
          <Text style={mutedProse}>{props.team().description || "还没有描述——编辑里补一句这支团队负责什么。"}</Text>
          {/* The captain is the mandatory seat: own line, never implied. */}
          <View style={{ flexDirection: "row", alignItems: "center", gap: space.sm, flexWrap: "wrap" }}>
            <Badge
              variant="default"
              truncate
              style={{ maxWidth: "100%" }}
              label={`队长：${captainName()}`}
            />
            <Text style={factText}>{`成员 ${members().length} 人`}</Text>
            <If when={members().length === 0}>
              <Text style={factText}>只有队长</Text>
            </If>
          </View>
          <If when={members().length > 0}>
            <View style={{ flexDirection: "row", alignItems: "center", gap: space.xs, flexWrap: "wrap" }}>
              <Text style={factText}>成员</Text>
              <Each of={members().slice(0, 8)} key={(member) => member.agentId}>
                {(member) => (
                  <Avatar
                    name={agentNameOf(member().agentId, member().label)}
                    size="sm"
                    accessibilityLabel={agentNameOf(member().agentId, member().label)}
                  />
                )}
              </Each>
              <If when={members().length > 8}>
                <Text style={factText}>{`+${members().length - 8}`}</Text>
              </If>
            </View>
          </If>
        </View>
      </View>
    </Card>
  );
}

function TeamEditor() {
  const agents = () => store.agents().filter((agent) => agent.archived !== true);
  const captainItems = (): SelectItem[] =>
    agents().map((agent) => ({ value: agent.id, title: agent.name || agent.id }));
  // The captain stays in the list, disabled, so a reader who looks for that
  // person here is told why the seat cannot be removed instead of silently
  // finding no such row.
  const memberItems = (): SelectItem[] =>
    agents().map((agent) => ({
      value: agent.id,
      title: agent.id === teamCaptain() ? `${agent.name || agent.id}（队长，不可移除）` : agent.name || agent.id,
      disabled: agent.id === teamCaptain(),
    }));
  const noAgents = () => agents().length === 0;
  const saving = () => store.teamsSaving();
  // A team cannot be saved without a captain; the name is validated on submit
  // (so the field can explain itself) and does not gate the button.
  const canSave = () => !saving() && teamCaptain() !== "";
  return (
    <Dialog
      testID="team-dialog"
      open={teamOpen()}
      size="lg"
      title={teamEditingId() === null ? "新建团队" : "编辑团队"}
      description="团队必须有一位队长；成员是除队长之外的席位，可以稍后再加。"
      onOpenChange={(event) => {
        if (!event.open) closeTeamEditor();
      }}
      footer={
        <View style={{ flexDirection: "row", alignItems: "center", gap: space.md }}>
          <Button variant="ghost" label="取消" onPress={closeTeamEditor} />
          <Button
            testID="team-save"
            label={saving() ? "保存中…" : teamEditingId() === null ? "创建" : "保存"}
            loading={saving()}
            disabled={!canSave()}
            onPress={submitTeam}
          />
        </View>
      }
    >
      <Scroll testID="team-form" style={{ maxHeight: editorScrollHeight() }}>
        <FieldGroup>
          <Field label="名字" required error={teamNameError() ?? ""} hint="团队名会出现在会话的启动模式里。">
            <Input
              testID="team-name"
              accessibilityLabel="团队名字"
              defaultValue={teamText.name}
              placeholder="例如 品牌发布小组"
              maxLength={64}
              disabled={saving()}
              onChange={(value) => {
                teamText.name = value;
                if (teamNameError()) teamNameError.set(null);
              }}
              onSubmit={submitTeam}
            />
          </Field>
          <Field label="描述" hint="一句话说明这支团队负责什么。">
            <Input
              testID="team-description"
              accessibilityLabel="团队描述"
              defaultValue={teamText.description}
              placeholder="例如 负责新品发布的全链路交付"
              maxLength={240}
              disabled={saving()}
              onChange={(value) => {
                teamText.description = value;
              }}
              onSubmit={submitTeam}
            />
          </Field>
          <Field
            label="队长"
            required
            error={teamCaptainError() ?? ""}
            hint={noAgents() ? "还没有 Agent：先到「Agents」创建一位，再回来组队。" : "团队必须有队长；队长始终在队内，不能当作普通成员移除。"}
          >
            <Select
              testID="team-captain"
              accessibilityLabel="选择队长"
              items={captainItems()}
              value={teamCaptain()}
              placeholder="选择队长（必选）"
              searchable
              invalid={!!teamCaptainError()}
              disabled={saving() || noAgents()}
              onChange={(value) => setTeamCaptain(value)}
            />
          </Field>
          <Field
            label="成员"
            hint={
              noAgents()
                ? "先在「Agents」里创建成员。"
                : "可多选；队长不会出现在可添加列表里（列表中标为「不可移除」）。"
            }
          >
            <Combobox
              testID="team-members"
              accessibilityLabel="选择团队成员"
              items={memberItems()}
              multiple
              value={teamMembers()}
              placeholder="搜索并添加成员"
              disabled={saving() || noAgents()}
              onChange={(values) => teamMembers.set(values)}
            />
          </Field>
        </FieldGroup>
      </Scroll>
      {/* As the Agent editor: the refusal stays outside the form's scroll, so
          it is visible on a 390px screen without scrolling. */}
      <If when={!!store.teamsActionError()}>
        <View style={{ flexDirection: "row", alignItems: "flex-start", gap: space.sm }}>
          <Icon name="alert" size={iconSize.sm} tint={color.danger} />
          <Text
            testID="team-save-error"
            style={{
              flex: 1,
              fontSize: typeScale.sm,
              lineHeight: Math.round(typeScale.sm * lineHeight.normal),
              color: color.danger,
            }}
          >
            保存失败：{store.teamsActionError() ?? ""}
          </Text>
        </View>
      </If>
    </Dialog>
  );
}

export function TeamsSection() {
  effect(() => {
    const timer = setTimeout(() => {
      void store.loadTeams();
      // The captain/member pickers need names: agents are read here too, so a
      // reader who opens 团队 first still sees who can be chosen.
      if (store.agents().length === 0 && !store.agentsLoading()) void store.loadAgents();
    }, 0);
    cleanup(() => clearTimeout(timer));
  });
  const teams = () => store.teams().filter((team) => team.archived !== true);
  return (
    <View style={{ gap: space["3xl"] }}>
      <Card testID="settings-teams">
        <CardHeader
          title="团队"
          description="每支团队有一位必选的队长，成员是队长的协作者。会话可以选择「团队」模式启动，由队长调度成员。"
          action={
            <Button
              testID="team-create"
              size="sm"
              icon="plus"
              label="新建团队"
              accessibilityLabel="新建团队"
              disabled={store.teamsLoading() && store.teams().length === 0}
              onPress={() => editTeam()}
            />
          }
        />
        <CardContent>
          <If when={store.teamsLoading() && store.teams().length === 0}>
            <SectionLoading testID="teams-loading" text="正在读取团队清单…" />
          </If>
          <If when={store.teamsErrorStatus() === 403}>
            <SectionPermission
              scope="团队注册表"
              reason={store.teamsError() ?? ""}
              onRetry={() => void store.loadTeams()}
            />
          </If>
          <If when={!!store.teamsError() && store.teamsErrorStatus() !== 403}>
            <SectionError
              testID="teams-error"
              text={store.teamsError() ?? ""}
              onRetry={() => void store.loadTeams()}
            />
          </If>
          <If when={!store.teamsLoading() && !store.teamsError() && teams().length === 0}>
            <Empty accessibilityLabel="还没有团队">
              <EmptyMedia icon="message" tone="brand" />
              <EmptyTitle label="还没有团队，创建一个" />
              <EmptyDescription label="建团队要先有一位队长：先在「Agents」里创建 Agent，再回来选它当队长。" />
              <EmptyContent>
                <Button
                  icon="plus"
                  label="新建团队"
                  accessibilityLabel="空态新建团队"
                  onPress={() => editTeam()}
                />
              </EmptyContent>
            </Empty>
          </If>
          <If when={!store.teamsError() && teams().length > 0}>
            <Text style={factText}>{`${teams().length} 支团队 · 队长必选、唯一`}</Text>
          </If>
          <Each of={teams()} key={(team) => team.id}>
            {(team) => <TeamCard team={team} />}
          </Each>
        </CardContent>
      </Card>

      <TeamEditor />

      <AlertDialog
        testID="team-delete-dialog"
        open={confirmingTeam() !== null}
        destructive
        title={`删除团队 ${confirmingTeam()?.name ?? ""}？`}
        description="运行中的会话不受影响；之后的启动不再提供这支团队。此操作不可撤销。"
        confirmLabel={`确认删除团队 ${confirmingTeam()?.name ?? ""}`}
        cancelLabel="取消删除"
        onConfirm={() => {
          const team = confirmingTeam();
          if (!team) return;
          void store.deleteTeam(team.id).then((ok) => {
            if (ok) confirmingTeam.set(null);
          });
        }}
        onCancel={() => {
          store.clearTeamActionError();
          confirmingTeam.set(null);
        }}
      >
        <If when={!!store.teamsActionError()}>
          <Text
            testID="team-delete-error"
            style={{
              fontSize: typeScale.sm,
              lineHeight: Math.round(typeScale.sm * lineHeight.normal),
              color: color.danger,
            }}
          >
            删除失败：{store.teamsActionError() ?? ""}
          </Text>
        </If>
      </AlertDialog>
    </View>
  );
}

/** The one component `settings.tsx` renders for the two new rail entries. The
 * open section arrives as a getter so this module never imports the signal
 * (settings.tsx imports this module; a second import would be a cycle). */
export function AgentTeamSettingsSection(props: { section: () => string }) {
  return (
    <If when={props.section() === "agents"} else={<TeamsSection />}>
      <AgentsSection />
    </If>
  );
}
