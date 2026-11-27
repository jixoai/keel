// TUI verification for W12's slots: the same App.tsx on the terminal host.
//
// The fixture transport reports one enabled plugin (`keel.w12.fixture`) whose
// chunk contribution draws in the top band, the sidebar and the settings
// sections. The terminal has no WebView but a chunk is a host-tree component,
// so all three render there — the spec's "chunk works in TUI" rule.
import { expect, open, test } from "nsh/test";

const openApp = () => open("../../App.tsx", { host: { params: { fixture: true, plugins: true } } });

test("TUI: 系统栏的插件槽渲染 chunk 贡献，侧栏与设置同时保留", async () => {
  const app = await openApp();
  await app.waitFor("text:W12 插件条");
  await app.waitFor("text:W12 插件侧栏");
  expect(app.shows("id:keel-system-bar")).toBe(true);
  expect(app.shows("id:plugin-top-panels")).toBe(true);
  expect(app.shows("id:plugin-sidebar-slot")).toBe(true);
  // The plugin slot kept its own contract inside the new system bar: no
  // chapter navigation moved into it.
  expect(app.shows("id:books-top")).toBe(true);
  expect(app.shows("id:books-chapter-identity")).toBe(false);
});

test("TUI: 设置页渲染 action 与 settings-section 贡献", async () => {
  const app = await openApp();
  await app.press("label:打开设置");
  await app.press("id:settings-section-plugins");
  await app.waitFor("text:W12 插件设置区");
  await app.waitFor("text:W12 插件动作");
  expect(app.shows("id:plugin-settings-sections")).toBe(true);
  expect(app.shows("id:plugin-action-buttons")).toBe(true);
});

test("TUI: 附件预览按插件贡献降级——chunk 渲染，iframe 说明需要图形界面，默认行保留", async () => {
  const app = await openApp();
  await app.press("label:打开会话 发布审批");
  // The image attachment hits the chunk claim; the PDF hits the iframe claim,
  // which the terminal cannot run and must say so explicitly.
  await app.waitFor("text:/W12 附件预览 · demo-shot.png/", { timeout: 10_000 });
  await app.waitFor("text:/终端不可用（需要图形界面）/");
  // The host's default facts are never hidden by a preview or its absence.
  expect(app.texts()).toContain("发布说明.pdf");
  expect(app.texts()).toContain("application/pdf");
});
