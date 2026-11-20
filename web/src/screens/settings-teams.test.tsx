// TUI gate for the settings「团队」section (change keel-agent-team-v1).
//
// The mandatory captain is the fact this gate protects: the card prints
// 「队长：<name>」 as its own line, the create dialog cannot save without one,
// and the captain never appears as a removable member. The harness answers the
// registry through the store's transport seam, so the real `KeelHttp` parsing
// runs.
import { expect, open, test } from "nsh/test";

const HARNESS = "./settings-teams.harness.tsx";

const joined = (app: { texts(): string[] }): string => app.texts().join("\n");

const openSection = (scenario: string) => open(HARNESS, { host: { params: { scenario } } });

test("TUI: 团队——卡片明确队长、成员数与成员头像", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "list" } } });

  await app.waitFor("id:team-card-team_brand");
  const texts = app.texts();
  expect(texts).toContain("品牌发布小组");
  expect(texts).toContain("队长：设计师");
  expect(joined(app)).toContain("成员 2 人");
  expect(joined(app)).toContain("成员 0 人");
  expect(joined(app)).toContain("只有队长");
  expect(joined(app)).toContain("2 支团队 · 队长必选、唯一");
  // The second team's captain is a different agent: the line is per card.
  expect(texts).toContain("队长：研究员");
  expect(app.shows("label:编辑团队 品牌发布小组")).toBe(true);
  expect(app.shows("label:删除团队 资料组")).toBe(true);
});

test("TUI: 团队——空态说明「先有 Agent 才能组队」", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "empty" } } });

  await app.waitFor("text:/还没有团队，创建一个/");
  expect(joined(app)).toContain("先在「Agents」里创建 Agent");
  expect(app.shows("label:空态新建团队")).toBe(true);
});

test("TUI: 团队——无队长时保存禁用并说明原因", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "list" } } });
  await app.waitFor("id:team-card-team_brand");

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  expect(joined(app)).toContain("团队必须有队长");
  expect(app.enabled("id:team-save")).toBe(false);

  await app.type("id:team-name", "新小组");
  // Still disabled: a name is not a captain.
  expect(app.enabled("id:team-save")).toBe(false);

  await app.press("text:选择队长（必选）");
  await app.waitFor("text:设计师");
  await app.press("text:设计师");
  expect(app.enabled("id:team-save")).toBe(true);
});

test("TUI: 团队——保存成功后卡片出现且队长行正确", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "list" } } });
  await app.waitFor("id:team-card-team_brand");

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  await app.type("id:team-name", "新小组");
  await app.type("id:team-description", "负责新渠道试投");
  await app.press("text:选择队长（必选）");
  await app.waitFor("text:设计师");
  await app.press("text:设计师");
  await app.press("id:team-save");

  await app.waitFor("id:team-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_new_1");
  expect(joined(app)).toContain("新小组");
  expect(joined(app)).toContain("负责新渠道试投");
  expect(joined(app)).toContain("队长：设计师");
  expect(joined(app)).toContain("3 支团队 · 队长必选、唯一");
});

test("TUI: 团队——编辑时队长在成员列表里标为不可移除", async () => {
  const app = await open(HARNESS, { host: { params: { scenario: "list" } } });
  await app.waitFor("id:team-card-team_brand");

  await app.press("label:编辑团队 品牌发布小组");
  await app.waitFor("id:team-dialog");
  await app.type("id:team-members", "设计师");
  await app.waitFor("text:/设计师（队长，不可移除）/");
  expect(joined(app)).toContain("设计师（队长，不可移除）");
  expect(joined(app)).toContain("队长始终在队内，不能当作普通成员移除");
});

test("TUI: 团队——加载态：骨架与说明在场，主操作禁用", async () => {
  const app = await openSection("loading");

  await app.waitFor("id:teams-loading");
  expect(joined(app)).toContain("正在读取团队清单");
  expect(app.shows("label:新建团队")).toBe(true);
  expect(app.enabled("id:team-create")).toBe(false);
  expect(app.shows("id:team-card-team_brand")).toBe(false);
});

test("TUI: 团队——错误态：服务端原因逐字呈现，可重试", async () => {
  const app = await openSection("error");

  await app.waitFor("id:teams-error");
  expect(joined(app)).toContain("团队注册表暂时不可用");
  expect(app.shows("label:加载失败")).toBe(true);
  expect(app.shows("label:重试")).toBe(true);
  expect(app.shows("text:/还没有团队，创建一个/")).toBe(false);
});

test("TUI: 团队——权限不足（403）有自己的面，不冒充加载失败", async () => {
  const app = await openSection("permission");

  await app.waitFor("id:settings-permission");
  expect(joined(app)).toContain("没有查看团队注册表的权限");
  expect(joined(app)).toContain("当前账号没有 team.read 权限");
  expect(app.shows("id:teams-error")).toBe(false);
  expect(app.shows("label:加载失败")).toBe(false);
  expect(app.shows("label:重试")).toBe(true);
});

test("TUI: 团队——保存中：字段与按钮锁定，保存按钮带进行中文案", async () => {
  const app = await openSection("slow");
  await app.waitFor("id:team-card-team_brand");

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  await app.type("id:team-name", "慢速小组");
  await app.press("text:选择队长（必选）");
  await app.waitFor("text:设计师");
  await app.press("text:设计师");
  await app.press("id:team-save");

  await app.waitFor("text:/保存中…/");
  expect(app.enabled("id:team-save")).toBe(false);
  expect(app.enabled("id:team-name")).toBe(false);
  expect(app.shows("id:team-dialog")).toBe(true);
  await app.waitFor("id:team-dialog", { state: "gone" });
  await app.waitFor("id:team-card-team_new_1");
});

test("TUI: 团队——Agent 注册表不可读时队长显示可读占位，不泄漏裸 id", async () => {
  const app = await openSection("agents-permission");

  await app.waitFor("id:team-card-team_brand");
  expect(joined(app)).toContain("队长：未载入的 Agent");
  expect(joined(app)).not.toContain("agent_design");
});

test("TUI: 团队——保存失败：原因可读，名字与队长保留", async () => {
  const app = await openSection("refuse");
  await app.waitFor("id:team-card-team_brand");

  await app.press("id:team-create");
  await app.waitFor("id:team-dialog");
  await app.type("id:team-name", "新小组");
  await app.press("text:选择队长（必选）");
  await app.waitFor("text:设计师");
  await app.press("text:设计师");
  await app.press("id:team-save");

  await app.waitFor("id:team-save-error");
  expect(joined(app)).toContain("队长 agent_design 当前不可用");
  expect(app.shows("id:team-dialog")).toBe(true);
  expect(app.value("id:team-name")).toBe("新小组");
});