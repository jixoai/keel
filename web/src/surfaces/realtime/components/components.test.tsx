// Component gate · realtime surface pieces.
//
// Each component is mounted in states the surface actually reaches: the status
// strip (speaking, mic on), the assistant audio indicator, the level meter, a
// final user row and its partial, an interrupted assistant row with a tool
// request, a completed reply, and the call controls (mute flips its label, stop
// leaves the primary control on "开始实时对话").
import { expect, open, test } from "nsh/test";

test("实时语音组件：状态条、音频指示、波形与转写行", async () => {
  const app = await open("../components-harness.tsx");
  await app.waitFor("text:/实时语音/");

  const texts = app.texts();
  expect(texts).toContain("实时语音");
  expect(texts).toContain("说话");
  expect(texts).toContain("已连接");
  expect(texts).toContain("00:12");
  expect(texts).toContain("麦克风开启 · 持续收音");
  expect(texts).toContain("语音播放中");
  // The interrupted row keeps what arrived and says it will not complete.
  expect(texts).toContain("我查了一下明天的日程，");
  expect(texts).toContain("已打断");
  expect(texts).toContain("已停止生成与播放，不会补全成本轮完整回复。");
  expect(texts).toContain("播放已停止");
  // A partial is labelled, never a final row.
  expect(texts).toContain("正在说的这句…");
  expect(texts).toContain("转写中（仅界面）");
  expect(texts).toContain("实时转写");
  // The completed reply carries the session's own status.
  expect(texts).toContain("好的，已安排。");
  expect(texts).toContain("已完成");
  // The tool request is shown, and the panel says it neither runs nor answers it.
  expect(texts).toContain("calendar.lookup");
  expect(texts).toContain("已请求");
  expect(texts.join("\n")).toContain("不执行，也不回传结果");
  expect(texts.join("\n")).toContain("已重连：这是一段新的实时会话");
});

test("实时语音组件：通话控件——静音标签翻转，结束后回到开始", async () => {
  const app = await open("../components-harness.tsx");
  await app.waitFor("id:realtime-primary");
  expect(app.texts()).toContain("结束通话");
  expect(app.texts()).toContain("静音");

  await app.press("id:realtime-mute");
  await app.waitFor("text:取消静音");
  expect(app.texts()).toContain("麦克风已静音");

  await app.press("id:realtime-primary");
  await app.waitFor("text:开始实时对话");
  expect(app.texts()).toContain("麦克风未开启");
  // Mute is only meaningful during a call.
  expect(app.shows("id:realtime-mute")).toBe(false);
});
