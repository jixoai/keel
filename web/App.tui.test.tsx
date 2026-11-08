// TUI verification: the same App.tsx on NSH's terminal host.
//
//   nsh test --target tui
//
// Each test opens the app on an in-memory terminal (the real TUI host, not a
// second UI): it finds views by their accessibility labels/test ids and reads
// the terminal's texts. The flows cover the change's TUI scenario: session
// list → open → type → reply → approve, plus guide/stop and the partial
// switch.
import { expect, open, test } from "nsh/test";

const TITLE = (name: string) => `label:打开会话 ${name}`;

test("TUI: 会话列表 → 打开会话 → 输入 → 显示回复", async () => {
  const app = await open("./App.tsx");
  expect(app.texts()).toContain("发布准备周报");
  expect(app.texts()).toContain("发布审批");
  expect(app.texts()).toContain("演示数据 · 未连接服务");

  await app.press(TITLE("发布准备周报"));
  await app.waitFor("text:/本周发布准备已基本就绪/");
  await app.waitFor("text:/完成 ·/");

  await app.type("id:composer-input", "请给我一份发布检查清单");
  await app.press("label:发送");
  await app.waitFor("text:/已检查当前工作区/", { timeout: 10_000 });
  await app.waitFor("text:/下一步建议/");
});

test("TUI: 审批卡一键作答", async () => {
  const app = await open("./App.tsx");
  await app.press(TITLE("发布审批"));
  await app.waitFor("text:需要你的确认");
  expect(app.texts()).toContain("推荐");
  await app.press("text:批准发布");
  await app.waitFor("text:/已确认/");
  await app.waitFor("text:/已记录你的选择/");
});

test("TUI: guide 等待与停止", async () => {
  const app = await open("./App.tsx");
  await app.press(TITLE("会议纪要整理"));
  await app.waitFor("text:等待引导当前任务…");
  await app.press("text:引导");
  await app.type("id:composer-input", "把行动项按负责人分组，并标注截止日期。");
  await app.press("label:发送");
  await app.waitFor("text:等待引导当前任务…");
  await app.press("label:停止当前任务");
  await app.waitFor("text:/已停止/");
});

test("TUI: partial 开关", async () => {
  const app = await open("./App.tsx");
  expect(app.texts()).toContain("partial 开");
  await app.press("label:切换 token 级 partial");
  await app.waitFor("text:partial 关");
  expect(app.texts()).toContain("partial 关");
});
