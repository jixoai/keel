// TUI verification for W12's slots: the same App.tsx on the terminal host.
//
// The fixture transport reports one enabled plugin (`keel.w12.fixture`) whose
// chunk contribution draws in the top band, the sidebar and the settings
// sections. The terminal has no WebView but a chunk is a host-tree component,
// so all three render there — the spec's "chunk works in TUI" rule.
import { expect, open, test } from "nsh/test";

const openApp = () => open("../../App.tsx", { host: { params: { fixture: true, plugins: true } } });

test("TUI: 顶栏与侧栏渲染插件的 chunk 贡献", async () => {
  const app = await openApp();
  await app.waitFor("text:W12 插件条");
  await app.waitFor("text:W12 插件侧栏");
  expect(app.shows("id:plugin-top-panels")).toBe(true);
  expect(app.shows("id:plugin-sidebar-slot")).toBe(true);
  // The band kept its own contract: no chapter navigation moved into it.
  expect(app.shows("id:books-top")).toBe(true);
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
