// TUI gate for the settings「Agents」section (change keel-agent-team-v1).
//
// The harness answers the registry through the store's transport seam, so the
// real `KeelHttp` parsing runs: the card facts, the create dialog's fields and
// validation, the save path (list reload) and the refusal path (the server's
// reason, with the reader's input still on screen).
import { expect, open, test } from "nsh/test";

const HARNESS = "./settings-agents.harness.tsx";

const joined = (app: { texts(): string[] }): string => app.texts().join("\n");

test("TUI: Agents——列表卡片给出名字、默认模型、工具数与描述", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "list" } } });

  await app.waitFor("id:agent-card-agent_design");
  const texts = app.texts();
  expect(texts).toContain("设计师");
  expect(texts).toContain("负责界面与品牌视觉");
  expect(joined(app)).toContain("默认模型 DeepSeek V4.1 Flash · S2API");
  expect(joined(app)).toContain("工具 2 个");
  expect(joined(app)).toContain("工具 1 个");
  expect(joined(app)).toContain("2 位 Agent");
  // The cards are separate blocks, each with its own edit/delete actions.
  expect(app.shows("label:编辑 Agent 设计师")).toBe(true);
  expect(app.shows("label:删除 Agent 研究员")).toBe(true);
});

test("TUI: Agents——空态引导下一步（创建一个）", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "empty" } } });

  await app.waitFor("text:/还没有 Agent，创建一个/");
  expect(joined(app)).toContain("团队至少要有一位队长");
  expect(app.shows("label:空态新建 Agent")).toBe(true);
});

test("TUI: Agents——新建 Dialog 字段齐全，名字为空时给出可读校验", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "list" } } });
  await app.waitFor("id:agent-card-agent_design");

  await app.press("id:agent-create");
  await app.waitFor("id:agent-dialog");
  const texts = app.texts();
  for (const label of ["描述", "默认模型", "工具", "系统提示词", "头像"]) {
    expect(texts).toContain(label);
  }
  expect(joined(app)).toContain("名字");
  expect(joined(app)).toContain("AGENTS.md");
  expect(texts).toContain("选择默认模型");

  // Empty name: submit refuses locally, the dialog stays and names the field.
  await app.press("id:agent-save");
  await app.waitFor("text:/Agent 名字不能为空/");
  expect(app.shows("id:agent-dialog")).toBe(true);
});

test("TUI: Agents——保存成功后 Dialog 关闭，列表出现新 Agent", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "list" } } });
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

test("TUI: Agents——保存失败只报原因，输入不丢", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "refuse" } } });
  await app.waitFor("id:agent-card-agent_design");

  await app.press("id:agent-create");
  await app.waitFor("id:agent-dialog");
  await app.type("id:agent-name", "发布助手");
  await app.press("id:agent-save");

  await app.waitFor("id:agent-save-error");
  expect(joined(app)).toContain("工具 image.generate 未安装");
  expect(app.shows("id:agent-dialog")).toBe(true);
  expect(app.value("id:agent-name")).toBe("发布助手");
});

test("TUI: Agents——被团队引用时删除失败，原因可读", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "referenced" } } });
  await app.waitFor("id:agent-card-agent_design");

  await app.press("label:删除 Agent 设计师");
  await app.waitFor("text:/删除 Agent 设计师？/");
  await app.press("label:确认删除 Agent 设计师");
  await app.waitFor("id:agent-delete-error");
  expect(joined(app)).toContain("品牌发布小组");
  // The card is still there: nothing was removed behind the refusal.
  expect(app.shows("id:agent-card-agent_design")).toBe(true);
});
