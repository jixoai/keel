// TUI gate for the placeholder surface (change keel-session-surfaces-v1).
//
// The rule this pins: a medium with no shipped panel says so and offers one way
// forward — it never renders a composer, never invents a result, and never
// leaves the user on a dead end. Both actions are exercised because the
// settings route and the preview exit are the only escapes.
import { expect, open, test } from "nsh/test";

const openSurface = () => open("./harness.tsx", { width: 390 });

test("TUI: 视频无专用面板——显示类型名与原因，不渲染输入框", async () => {
  const app = await openSurface();
  await app.waitFor("id:unsupported-empty");
  expect(app.texts()).toContain("视频生成");
  expect(app.texts()).toContain(
    "视频生成面板尚未提供：当前没有已接入的视频 Provider，所以这里不显示生成输入框，也不会生成任何占位结果。",
  );
  // No composer: nothing on this screen can be mistaken for a working session.
  expect(app.shows("id:unsupported-composer")).toBe(false);
});

test("TUI: 唯一主动作是打开模型设置，次动作返回对话", async () => {
  const app = await openSurface();
  await app.press("id:unsupported-open-settings");
  expect(app.shows("id:pressed-settings")).toBe(true);
  await app.press("id:unsupported-leave");
  expect(app.shows("id:pressed-leave")).toBe(true);
});
