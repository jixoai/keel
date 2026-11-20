// TUI gate for the settings「Agents」section (change keel-agent-team-v1).
//
// The harness answers the registry through the store's transport seam, so the
// real `KeelHttp` parsing runs: the card facts, the five states (loading,
// empty, configured, saving, failed — with 403 as its own permission face),
// the create/edit dialogs' fields and validation, the save path (list reload)
// and the refusal path (the server's reason, with the reader's input still on
// screen). The system prompt's two sources are explicit tabs: an inline text
// and a workspace-relative file reference (AGENTS.md), validated before the
// round trip and persisted through the same `systemPrompt` field.
import { expect, open, test } from "nsh/test";

const HARNESS = "./settings-agents.harness.tsx";

const joined = (app: { texts(): string[] }): string => app.texts().join("\n");

const openSection = (scenario: string) => open(HARNESS, { host: { params: { scenario } } });

test("TUI: Agents——列表卡片给出名字、默认模型、工具数、提示词来源与描述", async () => {
  const app = await openSection("list");

  await app.waitFor("id:agent-card-agent_design");
  const texts = app.texts();
  expect(texts).toContain("设计师");
  expect(texts).toContain("负责界面与品牌视觉");
  expect(joined(app)).toContain("DeepSeek V4.1 Flash");
  expect(joined(app)).toContain("工具 2 个");
  expect(joined(app)).toContain("工具 1 个");
  expect(joined(app)).toContain("2 位 Agent");
  // The prompt source is a card fact, not a dialog-only field.
  expect(joined(app)).toContain("内联系统提示词");
  expect(joined(app)).toContain("文件引用 · AGENTS.md");
  // The cards are separate blocks, each with its own edit/delete actions.
  expect(app.shows("label:编辑 Agent 设计师")).toBe(true);
  expect(app.shows("label:删除 Agent 研究员")).toBe(true);
});

test("TUI: Agents——空态引导下一步（创建一个）", async () => {
  const app = await openSection("empty");

  await app.waitFor("text:/还没有 Agent，创建一个/");
  expect(joined(app)).toContain("团队至少要有一位队长");
  expect(app.shows("label:空态新建 Agent")).toBe(true);
});

test("TUI: Agents——新建 Dialog 字段齐全，名字为空与文件引用空值都给出可读校验", async () => {
  const app = await openSection("list");
  await app.waitFor("id:agent-card-agent_design");

  await app.press("id:agent-create");
  await app.waitFor("id:agent-dialog");
  const texts = app.texts();
  for (const label of ["描述", "默认模型", "工具", "系统提示词", "头像"]) {
    expect(texts).toContain(label);
  }
  expect(joined(app)).toContain("名字");
  expect(texts).toContain("选择默认模型");

  // The two prompt sources are explicit tabs, not a path-looking heuristic.
  expect(app.shows("id:agent-prompt-mode-inline")).toBe(true);
  expect(app.shows("id:agent-prompt-mode-file")).toBe(true);
  expect(app.shows("id:agent-prompt")).toBe(true);
  await app.press("id:agent-prompt-mode-file");
  await app.waitFor("id:agent-prompt-file");
  expect(joined(app)).toContain("相对工作区根目录的路径");
  expect(joined(app)).toContain("AGENTS.md");
  // Switching back restores the inline editor; neither value is lost.
  await app.press("id:agent-prompt-mode-inline");
  await app.waitFor("id:agent-prompt");

  // Empty name: submit refuses locally, the dialog stays and names the field.
  await app.press("id:agent-save");
  await app.waitFor("text:/Agent 名字不能为空/");
  expect(app.shows("id:agent-dialog")).toBe(true);
});

test("TUI: Agents——保存成功后 Dialog 关闭，列表出现新 Agent", async () => {
  const app = await openSection("list");
  await app.waitFor("id:agent-card-agent_design");

  await app.press("id:agent-create");
  await app.waitFor("id:agent-dialog");
  await app.type("id:agent-name", "发布助手");
  await app.type("id:agent-description", "负责发布清单与渠道沟通");

  // The model picker groups by provider; picking one writes provider/model.
  await app.press("text:选择默认模型");
  await app.waitFor("text:/DeepSeek V4.1 Flash/");
  await app.press("text:DeepSeek V4.1 Flash · S2API");
  await app.press("id:agent-save");

  await app.waitFor("id:agent-dialog", { state: "gone" });
  await app.waitFor("id:agent-card-agent_new_1");
  expect(joined(app)).toContain("发布助手");
  expect(joined(app)).toContain("负责发布清单与渠道沟通");
  expect(joined(app)).toContain("3 位 Agent");
});

test("TUI: Agents——AGENTS.md 文件引用：编辑回填、改路径、保存后卡片同步", async () => {
  const app = await openSection("list");
  await app.waitFor("id:agent-card-agent_research");

  await app.press("label:编辑 Agent 研究员");
  await app.waitFor("id:agent-dialog");
  // The stored document's kind opens the file tab and fills its own field.
  await app.waitFor("id:agent-prompt-file");
  expect(app.value("id:agent-prompt-file")).toBe("AGENTS.md");
  expect(joined(app)).toContain("文件引用在 run 时读取");

  await app.clear("id:agent-prompt-file");
  await app.type("id:agent-prompt-file", "prompts/reviewer.md");
  await app.press("id:agent-save");

  await app.waitFor("id:agent-dialog", { state: "gone" });
  await app.waitFor("text:/文件引用 · prompts\\/reviewer\\.md/");
  // Re-open: the server's stored document, not the dialog's memory, is shown.
  await app.press("label:编辑 Agent 研究员");
  await app.waitFor("id:agent-dialog");
  expect(app.value("id:agent-prompt-file")).toBe("prompts/reviewer.md");
});

test("TUI: Agents——文件引用本地校验：空、绝对路径、越出工作区都不提交", async () => {
  const app = await openSection("list");
  await app.waitFor("id:agent-card-agent_design");

  await app.press("id:agent-create");
  await app.waitFor("id:agent-dialog");
  await app.type("id:agent-name", "评审助手");
  await app.press("id:agent-prompt-mode-file");
  await app.waitFor("id:agent-prompt-file");

  await app.press("id:agent-save");
  await app.waitFor("text:/文件引用不能为空/");

  await app.type("id:agent-prompt-file", "/etc/passwd");
  await app.press("id:agent-save");
  await app.waitFor("text:/必须使用相对工作区根目录的路径/");

  await app.clear("id:agent-prompt-file");
  await app.type("id:agent-prompt-file", "../../secret.md");
  await app.press("id:agent-save");
  await app.waitFor("text:/路径不能越出工作区/");
  // No invalid path was sent: the dialog and both fields are still here.
  expect(app.shows("id:agent-dialog")).toBe(true);
  expect(app.value("id:agent-name")).toBe("评审助手");

  await app.clear("id:agent-prompt-file");
  await app.type("id:agent-prompt-file", "AGENTS.md");
  await app.press("id:agent-save");
  await app.waitFor("id:agent-dialog", { state: "gone" });
  await app.waitFor("id:agent-card-agent_new_1");
  expect(joined(app)).toContain("文件引用 · AGENTS.md");
});

test("TUI: Agents——保存失败只报原因，文件引用与输入不丢", async () => {
  const app = await openSection("refuse");
  await app.waitFor("id:agent-card-agent_design");

  await app.press("id:agent-create");
  await app.waitFor("id:agent-dialog");
  await app.type("id:agent-name", "发布助手");
  await app.press("id:agent-prompt-mode-file");
  await app.waitFor("id:agent-prompt-file");
  await app.type("id:agent-prompt-file", "prompts/reviewer.md");
  await app.press("id:agent-save");

  await app.waitFor("id:agent-save-error");
  expect(joined(app)).toContain("工具 image.generate 未安装");
  expect(app.shows("id:agent-dialog")).toBe(true);
  expect(app.value("id:agent-name")).toBe("发布助手");
  expect(app.value("id:agent-prompt-file")).toBe("prompts/reviewer.md");
});

test("TUI: Agents——加载态：骨架与说明在场，主操作禁用", async () => {
  const app = await openSection("loading");

  await app.waitFor("id:agents-loading");
  expect(joined(app)).toContain("正在读取 Agent 清单");
  expect(app.shows("label:新建 Agent")).toBe(true);
  expect(app.enabled("id:agent-create")).toBe(false);
  expect(app.shows("id:agent-card-agent_design")).toBe(false);
});

test("TUI: Agents——错误态：服务端原因逐字呈现，可重试", async () => {
  const app = await openSection("error");

  await app.waitFor("id:agents-error");
  expect(joined(app)).toContain("Agent 注册表暂时不可用");
  expect(app.shows("label:加载失败")).toBe(true);
  expect(app.shows("label:重试")).toBe(true);
  // A failure is not an empty registry: the guide does not offer creation.
  expect(app.shows("text:/还没有 Agent，创建一个/")).toBe(false);
});

test("TUI: Agents——权限不足（403）有自己的面，不冒充加载失败", async () => {
  const app = await openSection("permission");

  await app.waitFor("id:settings-permission");
  expect(joined(app)).toContain("没有查看 Agent 注册表的权限");
  expect(joined(app)).toContain("当前账号没有 agent.read 权限");
  expect(app.shows("label:没有查看 Agent 注册表的权限")).toBe(true);
  expect(app.shows("id:agents-error")).toBe(false);
  expect(app.shows("label:加载失败")).toBe(false);
  expect(app.shows("label:重试")).toBe(true);
});

test("TUI: Agents——保存中：字段与按钮锁定，保存按钮带进行中文案", async () => {
  const app = await openSection("slow");
  await app.waitFor("id:agent-card-agent_design");

  await app.press("id:agent-create");
  await app.waitFor("id:agent-dialog");
  await app.type("id:agent-name", "慢速助手");
  await app.press("id:agent-save");

  await app.waitFor("text:/保存中…/");
  expect(app.enabled("id:agent-save")).toBe(false);
  expect(app.enabled("id:agent-name")).toBe(false);
  expect(app.shows("id:agent-dialog")).toBe(true);
  await app.waitFor("id:agent-dialog", { state: "gone" });
  await app.waitFor("id:agent-card-agent_new_1");
});

test("TUI: Agents——被团队引用时删除失败，原因可读", async () => {
  const app = await openSection("referenced");
  await app.waitFor("id:agent-card-agent_design");

  await app.press("label:删除 Agent 设计师");
  await app.waitFor("text:/删除 Agent 设计师？/");
  await app.press("label:确认删除 Agent 设计师");
  await app.waitFor("id:agent-delete-error");
  expect(joined(app)).toContain("品牌发布小组");
  // The card is still there: nothing was removed behind the refusal.
  expect(app.shows("id:agent-card-agent_design")).toBe(true);
});
