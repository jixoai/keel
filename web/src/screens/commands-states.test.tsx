// TUI gate for the command menu's five states (change keel-plugin-commands
// 4.4): loading / empty / error / permission / success, through the real store
// and the real menu (see commands-states.harness.tsx).
import { expect, open, test } from "nsh/test";

const HARNESS = "./commands-states.harness.tsx";
const openState = (scenario: string, width?: number) =>
  open(HARNESS, { ...(width ? { width } : {}), host: { params: { scenario } } });

test("TUI: 命令菜单五态——加载", async () => {
  const app = await openState("loading");
  await app.waitFor("id:composer-commands");
  await app.waitFor("id:composer-commands-loading");
  expect(app.texts()).toContain("正在读取命令目录…");
});

test("TUI: 命令菜单五态——空目录", async () => {
  const app = await openState("empty");
  await app.waitFor("id:composer-commands-empty");
  expect(app.texts()).toContain("当前会话没有可用命令。");
});

test("TUI: 命令菜单五态——错误与重试", async () => {
  const app = await openState("error");
  await app.waitFor("id:composer-commands-error");
  expect(app.texts().join("\n")).toContain("命令目录读取失败");
  expect(app.shows("id:composer-commands-retry")).toBe(true);
  await app.press("label:重试读取命令目录");
});

test("TUI: 命令菜单五态——无权限（缺 key 写明）", async () => {
  const app = await openState("permission");
  await app.waitFor("id:composer-commands-group-builtin");
  const text = app.texts().join("\n");
  expect(text).toContain("/compact");
  expect(text).toContain("缺少权限：model.invoke");
  expect(app.shows("label:命令 /compact 不可用：缺少权限：model.invoke")).toBe(true);
});

test("TUI: 命令菜单五态——成功（内置 + 插件同序）", async () => {
  const app = await openState("success");
  await app.waitFor("id:composer-commands-group-builtin");
  await app.waitFor("id:composer-commands-group-plugin");
  const text = app.texts().join("\n");
  expect(text.indexOf("/compact")).toBeLessThan(text.indexOf("/demo.workflow:plan"));
  expect(app.shows("label:执行命令 /compact")).toBe(true);
});

test("TUI: 390 命令菜单与表单——卡片宽度不超出视口", async () => {
  const menu = await openState("success", 390);
  await menu.waitFor("id:composer-commands");
  expect(menu.layout("id:composer-commands")?.width ?? 0).toBeLessThanOrEqual(390);
  expect(menu.texts().join("\n")).toContain("/demo.workflow:plan");

  const form = await open("./commands-form.harness.tsx", { width: 390 });
  await form.waitFor("id:composer-command-form");
  expect(form.layout("id:composer-command-form")?.width ?? 0).toBeLessThanOrEqual(390);
  const text = form.texts().join("\n");
  expect(text).toContain("goal");
  expect(text).toContain("depth");
  expect(text).toContain("dryRun");
});
