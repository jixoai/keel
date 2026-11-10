// C3 · kit container components — TUI gate: Card's three parts, Collapsible and
// Accordion open/close through the app's signals, Tabs switching panels, and the
// Table's header/body cells.
import { expect, open, test } from "nsh/test";

test("TUI: C3 Card 三件套与 Collapsible 开合", async () => {
  const app = await open("./harness-container.tsx");
  expect(app.texts()).toContain("发布计划");
  expect(app.texts()).toContain("周五上线");
  expect(app.texts()).toContain("内容：灰度 10% → 50% → 100%");
  expect(app.texts()).toContain("确认计划");

  expect(app.shows("id:collapsible-body")).toBe(false);
  await app.press("id:collapsible-trigger");
  await app.waitFor("id:collapsible-body");
  await app.press("id:collapsible-trigger");
  await app.waitFor("id:collapsible-body", { state: "gone" });
});

test("TUI: C3 Accordion 展开与 Tabs 切换面板", async () => {
  const app = await open("./harness-container.tsx");

  await app.press("label:什么是 Keel？");
  await app.waitFor("text:Keel 是任务外壳。");

  expect(app.shows("id:tab-panel-overview")).toBe(true);
  expect(app.shows("id:tab-panel-log")).toBe(false);
  await app.press("id:tab-log");
  await app.waitFor("id:tab-panel-log");
  await app.waitFor("id:tab-panel-overview", { state: "gone" });
});

test("TUI: C3 Table 表头与表体渲染与 ScrollArea 滚动", async () => {
  const app = await open("./harness-container.tsx");
  expect(app.shows("id:table-scroll")).toBe(true);
  await app.scroll("id:table-scroll", 16);
  expect(app.texts()).toContain("会话");
  expect(app.texts()).toContain("状态");
  expect(app.texts()).toContain("发布周报");
  expect(app.texts()).toContain("完成");
});
