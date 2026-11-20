// TUI gate for the session header's team face and its roster (change
// keel-agent-team-v1, P4 tail). The header names the team, captain and the
// model the next input runs; the member entry opens a read-only roster with
// its own loading / empty / error / permission faces. 390px is the same
// single-column layout the workspace uses.
import { expect, open, test } from "nsh/test";

const HARNESS = "./session-team.harness.tsx";

const joined = (app: { texts(): string[] }): string => app.texts().join("\n");

const openHeader = (scenario: string, width?: number) =>
  open(HARNESS, { ...(width ? { width } : {}), host: { params: { scenario } } });

test("TUI: 团队 header——团队名、队长与运行模型在标题下，成员入口可打开名单", async () => {
  const app = await openHeader("list", 390);

  await app.waitFor("id:workspace-header-team-facts");
  expect(joined(app)).toContain("品牌发布小组 · 队长 设计师 · DeepSeek V4.1 Flash · 高");
  expect(app.shows("id:session-team-members")).toBe(true);
  expect(app.shows("label:查看团队成员（1 人）")).toBe(true);

  await app.press("id:session-team-members");
  await app.waitFor("id:team-roster-dialog");
  await app.waitFor("text:研究员");
  const roster = joined(app);
  expect(roster).toContain("团队 · 品牌发布小组");
  expect(roster).toContain("队长");
  expect(roster).toContain("设计师");
  expect(roster).toContain("运行模型 DeepSeek V4.1 Flash · 高");
  expect(roster).toContain("成员");
  expect(roster).toContain("1 人");
  expect(roster).toContain("负责资料检索与整理");
  expect(roster).toContain("这里只读");
  await app.press("text:关闭");
  await app.waitFor("id:team-roster-dialog", { state: "gone" });
});

test("TUI: 团队 header——只有队长的团队在名单里说明没有额外成员", async () => {
  const app = await openHeader("empty", 390);

  await app.waitFor("id:workspace-header-team-facts");
  await app.press("id:session-team-members");
  await app.waitFor("id:team-roster-dialog");
  await app.waitFor("text:/只有队长/");
  expect(joined(app)).toContain("这支团队只有队长");
  expect(joined(app)).not.toContain("研究员");
});

test("TUI: 团队名单——加载态说明正在读取，不提前宣布空名单", async () => {
  const app = await openHeader("loading", 390);

  await app.waitFor("id:workspace-header-team-facts");
  await app.press("id:session-team-members");
  await app.waitFor("id:team-roster-loading");
  expect(joined(app)).toContain("正在读取成员资料");
  expect(joined(app)).not.toContain("这支团队只有队长");
  expect(app.shows("id:team-roster-error")).toBe(false);
});

test("TUI: 团队名单——错误态给出服务端原因与重试", async () => {
  const app = await openHeader("error", 390);

  await app.waitFor("id:workspace-header-team-facts");
  await app.press("id:session-team-members");
  await app.waitFor("id:team-roster-error");
  expect(joined(app)).toContain("成员资料读取失败：注册表不可用");
  expect(app.shows("label:重试读取成员资料")).toBe(true);
  expect(joined(app)).not.toContain("这支团队只有队长");
});

test("TUI: 团队名单——403 是权限面，不冒充读取失败", async () => {
  const app = await openHeader("permission", 390);

  await app.waitFor("id:workspace-header-team-facts");
  await app.press("id:session-team-members");
  await app.waitFor("id:team-roster-permission");
  expect(joined(app)).toContain("没有查看成员资料的权限");
  expect(joined(app)).toContain("当前账号没有 agent.read 权限");
  expect(app.shows("id:team-roster-error")).toBe(false);
});
