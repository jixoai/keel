// TUI gate for the captain timeline's team face (change
// keel-session-surfaces-v1, S4). The member cards are a separate projection:
// the five states are checked here, and the transcript test pins that a card is
// a sibling region — it does not become a captain/user/assistant/tool row.
import { expect, open, test } from "nsh/test";

const HARNESS = "./team-activity.harness.tsx";

const joined = (app: { texts(): string[] }): string => app.texts().join("\n");

const openWith = (scenario: string, width = 390) =>
  open(HARNESS, { width, host: { params: { scenario } } });

test("TUI: 团队动态——成功态按任务卡显示标题、状态与更新时间", async () => {
  const app = await openWith("success");
  await app.waitFor("id:team-activity");
  await app.waitFor("id:team-task-task_research");

  const text = joined(app);
  expect(text).toContain("团队动态");
  expect(text).toContain("检索竞品资料");
  expect(text).toContain("整理发布清单");
  expect(text).toContain("进行中");
  expect(text).toContain("已完成");
  // The card's own status is the result: no fabricated result sentence.
  expect(text).toContain("成员任务状态：进行中");
  expect(text).toContain("成员任务状态：已完成");
  expect(text).toContain("更新于");
  expect(text).not.toContain("结果摘要");
  expect(app.shows("id:team-activity-list")).toBe(true);
});

test("TUI: 团队动态——空态说明还没有成员任务", async () => {
  const app = await openWith("empty");
  await app.waitFor("id:team-activity-empty");

  expect(joined(app)).toContain("还没有成员任务");
  expect(joined(app)).toContain("队长委派成员后");
  expect(app.shows("id:team-activity-error")).toBe(false);
  expect(app.shows("id:team-activity-list")).toBe(false);
});

test("TUI: 团队动态——加载态不提前宣布空名单", async () => {
  const app = await openWith("loading");
  await app.waitFor("id:team-activity-loading");

  expect(joined(app)).toContain("正在读取成员任务");
  expect(joined(app)).not.toContain("还没有成员任务");
  expect(app.shows("id:team-activity-error")).toBe(false);
});

test("TUI: 团队动态——错误态给出服务端原因与重试", async () => {
  const app = await openWith("error");
  await app.waitFor("id:team-activity-error");

  expect(joined(app)).toContain("成员任务读取失败：任务投影不可用");
  expect(app.shows("label:重试读取成员任务")).toBe(true);
  expect(joined(app)).not.toContain("还没有成员任务");
});

test("TUI: 团队动态——403 是权限面，不冒充读取失败", async () => {
  const app = await openWith("permission");
  await app.waitFor("id:team-activity-permission");

  expect(joined(app)).toContain("没有查看成员任务的权限");
  expect(joined(app)).toContain("当前账号没有 task.read 权限");
  expect(app.shows("id:team-activity-error")).toBe(false);
});

test("TUI: 团队动态——卡片区域与 captain transcript 分离，不混入对话行", async () => {
  const app = await openWith("transcript");
  await app.waitFor("id:team-activity");
  await app.waitFor("text:/CAPTAIN-REPLY-ONLY/");

  const text = joined(app);
  expect(text).toContain("CAPTAIN-REPLY-ONLY");
  expect(text).toContain("检索竞品资料");
  expect(app.shows("id:conversation-list")).toBe(true);
  expect(app.shows("id:team-task-task_research")).toBe(true);
  // Exactly one occurrence: if the card had been folded into the captain's
  // transcript or a tool row, the same title would appear again there.
  expect(text.split("检索竞品资料").length - 1).toBe(1);
  // The team row's status sentence is its own; the transcript keeps its reply.
  expect(text).toContain("成员任务状态：进行中");
});
