// TUI gate for the settings「技能」section (change keel-harness-v2, W7 skills).
//
// The fixture answers the real `KeelHttp` parsing path: the section reads the
// library, renders the trust/pin/update/conflict facts, opens one skill's body
// and resources, creates a skill through the same write path the agent's
// skills.manage uses, and surfaces the server's typed refusals (`pinned`,
// `source_in_use`) instead of a generic error. The five states are covered
// across the tests: loading, empty, configured, busy/refused, error.
import { expect, open, test } from "nsh/test";

const HARNESS = "./settings-skills.harness.tsx";

const joined = (app: { texts(): string[] }): string => app.texts().join("\n");

const openSection = (scenario: string) => open(HARNESS, { host: { params: { scenario } } });

test("TUI: 技能——卡片给出信任/当前版本/固定/更新与冲突，订阅源可读", async () => {
  const app = await openSection("list");
  await app.waitFor("id:skill-card-deploy-flow");

  const text = joined(app);
  expect(text).toContain("deploy-flow");
  expect(text).toContain("发布检查清单与回滚步骤");
  expect(text).toContain("本地");
  expect(text).toContain("team-review");
  expect(text).toContain("受信任");
  // The conflict is a sentence, not a raw code.
  expect(text).toContain("已固定：源里有新版本，但不会自动更新");
  expect(text).toContain("2 个版本");
  expect(app.shows("id:skill-update-team-review")).toBe(true);
  expect(app.shows("id:skill-unpin-team-review")).toBe(true);
  expect(app.shows("id:skill-source-team")).toBe(true);
  expect(text).toContain("/srv/skills");
});

test("TUI: 技能——查看打开 SKILL.md 正文与资源清单", async () => {
  const app = await openSection("list");
  await app.waitFor("id:skill-card-deploy-flow");

  await app.press("id:skill-view-deploy-flow");
  await app.waitFor("id:skill-detail-dialog");
  expect(app.shows("id:skill-detail-body")).toBe(true);
  const text = joined(app);
  expect(text).toContain("1. 运行检查");
  expect(text).toContain("SKILL.md");
  expect(text).toContain("版本（不可变）");
});

test("TUI: 技能——空态给出下一步与新建表单", async () => {
  const app = await openSection("empty");
  await app.waitFor("text:/还没有技能/");
  expect(joined(app)).toContain("订阅一个本地技能目录");
  for (const id of ["skills-new-name", "skills-new-description", "skills-new-body", "skills-new-save"]) {
    expect(app.shows(`id:${id}`)).toBe(true);
  }
});

test("TUI: 技能——加载态与失败态（500 / 403）都有人话与重试", async () => {
  const loading = await openSection("loading");
  await loading.waitFor("id:settings-skills-loading");

  const broken = await openSection("error");
  await broken.waitFor("id:settings-skills-error");
  expect(joined(broken)).toContain("技能库读取失败");
  expect(joined(broken)).toContain("技能库暂时不可用");

  const denied = await openSection("permission");
  await denied.waitFor("id:settings-skills-error");
  expect(joined(denied)).toContain("没有 skills.read 权限");
});

test("TUI: 技能——新建走 skills.manage create 形状，保存后卡片出现且表单清空", async () => {
  const app = await openSection("empty");
  await app.waitFor("id:settings-skills");
  await app.type("id:skills-new-name", "release-check");
  await app.type("id:skills-new-description", "发布前检查");
  await app.type("id:skills-new-body", "1. 检查版本号");
  await app.press("id:skills-new-save");

  await app.waitFor("id:skill-card-release-check");
  const text = joined(app);
  expect(text).toContain("发布前检查");
  expect(app.value("id:skills-new-name")).toBe("");
});

test("TUI: 技能——固定/解除固定是真实状态迁移", async () => {
  const app = await openSection("list");
  await app.waitFor("id:skill-card-deploy-flow");
  expect(app.shows("id:skill-pin-deploy-flow")).toBe(true);

  await app.press("id:skill-pin-deploy-flow");
  await app.waitFor("id:skill-unpin-deploy-flow");
  expect(joined(app)).toContain("固定");
  await app.press("id:skill-unpin-deploy-flow");
  await app.waitFor("id:skill-pin-deploy-flow");
});

test("TUI: 技能——更新被固定拦住时显示 server 的 pinned code，不装成功", async () => {
  const app = await openSection("refuse");
  await app.waitFor("id:skill-card-team-review");

  expect(app.shows("label:更新技能 team-review")).toBe(true);
  await app.press("id:skill-update-team-review");
  await app.waitFor("id:settings-skills-action-error");
  const text = joined(app);
  expect(text).toContain("is pinned");
  expect(text).toContain("code: pinned");
});

test("TUI: 技能——订阅源仍在用时取消订阅被拒，错误可见", async () => {
  const app = await openSection("source-in-use");
  await app.waitFor("id:skill-source-team");

  await app.press("id:skill-source-remove-team");
  await app.waitFor("id:skill-source-remove-confirm");
  await app.press("label:确认取消订阅 team");
  await app.waitFor("id:settings-skills-action-error");
  expect(joined(app)).toContain("source_in_use");
});

test("TUI: 技能——订阅一个本地目录后源列表出现它", async () => {
  const app = await openSection("empty");
  await app.waitFor("id:settings-skills");
  await app.type("id:skill-source-id", "ops");
  await app.type("id:skill-source-url", "/srv/ops-skills");
  await app.press("id:skill-source-subscribe");

  await app.waitFor("id:skill-source-ops");
  expect(joined(app)).toContain("/srv/ops-skills");
});

test("TUI: 技能——移除要二次确认，第二张卡同样可操作", async () => {
  const app = await openSection("list");
  await app.waitFor("id:skill-card-team-review");
  await app.scroll("id:skills-scroll", 20);
  await app.press("id:skill-remove-team-review");
  await app.waitFor("id:skill-remove-confirm");
  expect(joined(app)).toContain("移除技能 team-review");
});
