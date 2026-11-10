// C4 · kit overlay & navigation components — TUI gate: Dialog / AlertDialog /
// Sheet open and close through app-owned signals, Popover / DropdownMenu /
// ContextMenu act, Toast dismisses, and Empty / Breadcrumb render.
import { expect, open, test } from "nsh/test";

test("TUI: C4 Dialog / AlertDialog / Sheet 打开与关闭", async () => {
  const app = await open("./harness-overlay.tsx");

  expect(app.shows("text:对话框内容")).toBe(false);
  await app.press("id:open-dialog");
  await app.waitFor("text:对话框内容");
  await app.press("id:dialog-cancel");
  await app.waitFor("text:对话框内容", { state: "gone" });

  await app.press("id:open-alert");
  await app.waitFor("text:删除这个会话？");
  await app.press("id:alert-delete-confirm");
  await app.waitFor("text:/已确认删除/");

  await app.press("id:open-sheet");
  await app.waitFor("text:抽屉内容");
  await app.press("label:关闭");
  await app.waitFor("text:抽屉内容", { state: "gone" });
});

test("TUI: C4 Popover / DropdownMenu / ContextMenu 交互", async () => {
  const app = await open("./harness-overlay.tsx");

  expect(app.shows("text:气泡内容")).toBe(false);
  await app.press("id:popover-trigger");
  await app.waitFor("text:气泡内容");
  await app.press("id:popover-close");
  await app.waitFor("text:气泡内容", { state: "gone" });

  await app.press("id:menu-trigger");
  await app.waitFor("label:重命名");
  await app.press("label:重命名");
  await app.waitFor("text:/已选 重命名/");

  // The context menu's trigger area renders (its rows open on the secondary
  // button, which the TUI has no mouse for).
  expect(app.shows("id:context-area")).toBe(true);
});

test("TUI: C4 Empty / Breadcrumb / Toast", async () => {
  const app = await open("./harness-overlay.tsx");

  expect(app.texts()).toContain("还没有消息");
  expect(app.texts()).toContain("发送一条消息，Keel 会在这里开始工作。");
  expect(app.texts()).toContain("新建会话");

  expect(app.texts()).toContain("首页");
  await app.press("id:crumb-home");
  await app.waitFor("text:/已选 首页/");

  expect(app.texts()).toContain("已保存");
  expect(app.texts()).toContain("模型配置");
  await app.press("label:关闭提示");
  await app.waitFor("text:/提示数 0/");

  // Tooltip: both triggers render; the app-opened one shows its hint on the
  // terminal's bottom row (a hover-only tooltip has no pointer to trigger it in
  // the TUI, which is the platform difference the spec allows).
  expect(app.shows("id:tooltip-target")).toBe(true);
  await app.waitFor("text:常显提示");
});
