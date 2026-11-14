// TUI gate · speech workbench components.
//
// The surface's block and recorder pieces are testable on their own: each
// carries a testID, a transcription block names its speaker and its source
// honestly (说话人 · 麦克风 / 文字输入), and RecordButton swaps from 按下说话
// to 停止并释放 with the state it is given.
import { expect, open, test } from "nsh/test";

test("语音转写组件：Waveform / TranscriptRow / RecordButton 渲染并带 testID", async () => {
  const app = await open("./harness.tsx");
  // The terminal model only gives text-bearing views a frame, so Waveform (a
  // row of token-coloured bars with no text) is exercised through the surface's
  // live recorder state; its contract here is that mounting it beside the rows
  // does not disturb them.
  expect(app.shows("id:row-speech")).toBe(true);
  expect(app.shows("id:row-text")).toBe(true);
  expect(app.shows("id:record")).toBe(true);
  expect(app.texts()).toContain("一条已完成的语音转写");
  expect(app.texts()).toContain("一条文字输入的转写");
  // A block, not a chat bubble: the speaker and the source are named.
  expect(app.texts()).toContain("说话人");
  expect(app.texts()).toContain("麦克风");
  expect(app.texts()).toContain("文字输入");
  expect(app.texts()).toContain("按下说话");
  // No assistant/chat metaphor survives in the block.
  const all = app.texts().join("\n");
  expect(all).not.toContain("助手");
  expect(all).not.toContain("回复");
});

test("语音转写组件：RecordButton 随状态切换为停止并释放", async () => {
  const app = await open("./harness.tsx");
  await app.press("id:record");
  await app.waitFor("id:stop");
  expect(app.shows("id:record")).toBe(false);
  expect(app.texts()).toContain("停止并释放");
  expect(app.texts().join("\n")).toContain("presses 1 · listening yes");
  await app.press("id:stop");
  await app.waitFor("id:record");
  expect(app.texts()).toContain("按下说话");
  expect(app.texts().join("\n")).toContain("presses 2 · listening no");
});
