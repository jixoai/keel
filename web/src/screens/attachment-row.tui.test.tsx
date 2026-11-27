// TUI acceptance for the attachment row's five states and preview fallbacks
// (change keel-plugin-attachment, tasks 3.3-3.5 and the review's P1-3).
//
// Every case mounts the production `AttachmentRow` with a host-supplied
// projection, so the facts line, the placeholder rules and the failure faces
// are the real ones. The `facts-guard` text is the same facts the row must
// always keep; no state may remove it.
import { expect, open, test } from "nsh/test";

const openHarness = (scenario: string, manualClock = false) =>
  open("./attachment-row.harness.tsx", {
    ...(manualClock ? { clock: {} } : {}),
    host: { params: { scenario } },
  });

const factsVisible = (app: Awaited<ReturnType<typeof openHarness>>) => {
  expect(app.shows("id:facts-guard")).toBe(true);
};

test("TUI: 成功态——chunk 预览渲染，事实行与下载入口仍在", async () => {
  const app = await openHarness("ready");
  await app.waitFor("text:/W12 附件预览 · shot.png/");
  factsVisible(app);
  expect(app.texts()).toContain("image/png · image · chunk");
  expect(app.shows("label:下载附件 shot.png")).toBe(true);
});

test("TUI: 缩略图 pending——显示生成中，不把原图当缩略图", async () => {
  const app = await openHarness("pending");
  await app.waitFor("text:/图片附件 · 缩略图生成中/");
  factsVisible(app);
  expect(app.texts()).not.toContain("缩略图已就绪");
});

test("TUI: 缩略图 failed——显示不可用占位", async () => {
  const app = await openHarness("failed");
  await app.waitFor("text:/图片附件 · 缩略图不可用/");
  factsVisible(app);
});

test("TUI: 缩略图 none——显示没有缩略图，不回退到原图", async () => {
  const app = await openHarness("none");
  await app.waitFor("text:/图片附件 · 没有缩略图/");
  factsVisible(app);
  expect(app.texts()).not.toContain("缩略图已就绪");
});

test("TUI: 投影加载态——保留事实行并说明正在读取", async () => {
  const app = await openHarness("loading");
  await app.waitFor("text:/正在读取附件/");
  factsVisible(app);
});

test("TUI: 投影错误态——显示可解释原因与重试", async () => {
  const app = await openHarness("error");
  await app.waitFor("text:/附件读取失败/");
  expect(app.texts().join("\n")).toContain("投影读取失败（测试夹具）");
  expect(app.shows("label:重试读取附件")).toBe(true);
  factsVisible(app);
});

test("TUI: part 403——显示无权限态，不泄露字节", async () => {
  const app = await openHarness("part-forbidden");
  await app.waitFor("text:/无权限查看附件/");
  expect(app.texts().join("\n")).toContain("没有返回任何字节");
  factsVisible(app);
});

test("TUI: file.read 403——显示无权限并禁用下载", async () => {
  const app = await openHarness("file-forbidden");
  await app.waitFor("text:/无权限查看附件内容/");
  expect(app.shows("label:无权限下载附件 shot.png")).toBe(true);
  factsVisible(app);
});

test("TUI: 预览渲染抛错——中文失败面，事实行不受影响", async () => {
  const app = await openHarness("throwing");
  await app.waitFor("text:/附件预览失败/");
  expect(app.texts().join("\n")).toContain("附件预览渲染器故意抛错");
  factsVisible(app);
});

test("TUI: 预览装载失败——显示失败原因与重试，事实行保留", async () => {
  const app = await openHarness("loadfail");
  await app.waitFor("text:/附件预览失败/");
  expect(app.shows("label:重试附件预览 demo.attach")).toBe(true);
  factsVisible(app);
});

test("TUI: 预览加载超时——4 秒后回退并说明原因", async () => {
  const app = await openHarness("loading-preview", true);
  await app.waitFor("text:/正在装载附件预览/");
  await app.clock?.runFor(4500);
  await app.waitFor("text:/附件预览加载超时/");
  factsVisible(app);
  expect(app.texts().join("\n")).toContain("其他附件不受影响");
});

test("TUI: iframe 预览——终端显示需要图形界面与打开入口", async () => {
  const app = await openHarness("iframe");
  await app.waitFor("text:/终端不可用（需要图形界面）/");
  const iframeLine = app.texts().find((text) => text.startsWith("/plugins/demo.attach/ui/preview.html"));
  expect(!!iframeLine).toBe(true);
  expect(iframeLine).toContain("fileId=11111111-1111-4111-8111-111111111111");
  expect(iframeLine).toContain("mime=image%2Fpng");
  expect(app.shows("label:在浏览器打开附件预览")).toBe(true);
  factsVisible(app);
});

test("TUI: 无匹配贡献——只有事实行，不留空白预览", async () => {
  const app = await openHarness("no-claim");
  await app.waitFor("id:facts-guard");
  expect(app.texts().join("\n")).not.toContain("W12 附件预览");
  expect(app.texts().join("\n")).not.toContain("正在装载附件预览");
  expect(app.shows("label:下载附件 shot.png")).toBe(true);
});
