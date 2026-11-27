// TUI acceptance for the attachment preview slot's failure isolation (change
// keel-plugin-attachment, tasks 3.3-3.5).
//
// The harness draws the host's default line and the preview slot separately, so
// every case proves the same invariant: a preview (or its failure) never
// removes the attachment's facts, and one broken renderer affects only itself.
import { expect, open, test } from "nsh/test";

const openHarness = (scenario: string, manualClock = false) =>
  open("./attachment-row.harness.tsx", {
    ...(manualClock ? { clock: {} } : {}),
    host: { params: { scenario } },
  });

test("TUI: 附件预览 chunk 命中——渲染预览且默认行与下载入口仍在", async () => {
  const app = await openHarness("ready");
  await app.waitFor("text:/W12 附件预览 · shot.png/");
  expect(app.shows("id:default-row")).toBe(true);
  expect(app.shows("id:download-entry")).toBe(true);
  expect(app.texts()).toContain("image/png · image · chunk");
});

test("TUI: 预览渲染抛错——显示可解释失败，默认行不受影响", async () => {
  const app = await openHarness("throwing");
  await app.waitFor("text:ERR_PLUGIN");
  expect(app.texts().join("\n")).toContain("附件预览渲染器故意抛错");
  expect(app.texts()).toContain("默认附件行 · shot.png · image/png · 39037");
  expect(app.shows("id:download-entry")).toBe(true);
  expect(app.texts()).not.toContain("W12 附件预览 · shot.png");
});

test("TUI: 预览装载失败——显示失败原因与重试，默认行保留", async () => {
  const app = await openHarness("loadfail");
  await app.waitFor("text:ERR_TARGET_UNAVAILABLE");
  expect(app.texts().join("\n")).toContain("终端只能加载随应用构建的 chunk");
  expect(app.shows("id:default-row")).toBe(true);
  expect(app.shows("label:重试插件 demo.attach")).toBe(true);
});

test("TUI: 预览加载超时——4 秒后回退到默认行并说明原因", async () => {
  const app = await openHarness("loading", true);
  await app.waitFor("text:/正在装载附件预览/");
  await app.clock?.runFor(4500);
  await app.waitFor("text:/附件预览加载超时/");
  expect(app.shows("id:default-row")).toBe(true);
  expect(app.texts().join("\n")).toContain("其他附件不受影响");
});

test("TUI: iframe 预览——终端显示需要图形界面与打开入口，不渲染空白", async () => {
  const app = await openHarness("iframe");
  await app.waitFor("text:/终端不可用（需要图形界面）/");
  expect(app.texts()).toContain("/plugins/demo.attach/ui/preview.html");
  expect(app.shows("label:在浏览器打开附件预览")).toBe(true);
  expect(app.shows("id:default-row")).toBe(true);
});

test("TUI: 无匹配贡献——只有默认行，不留空白预览", async () => {
  const app = await openHarness("none");
  await app.waitFor("id:default-row");
  expect(app.texts().join("\n")).not.toContain("W12 附件预览");
  expect(app.texts().join("\n")).not.toContain("正在装载附件预览");
  expect(app.shows("id:download-entry")).toBe(true);
});
