// TUI gate for the /command send gate (change keel-plugin-commands, review
// P1-1): a first-token `/name ` draft must never fall back to an ordinary
// user message while the catalog is loading or failed. `/learn` remains the
// one client template and still sends.
import { expect, open, test } from "nsh/test";

const HARNESS = "./commands-blocked.harness.tsx";
const openBlocked = (scenario: string) =>
  open(HARNESS, { host: { params: { fixture: true, scenario } } });

test("TUI: 目录加载中，输入 /compact 空格不发送普通消息", async () => {
  const app = await openBlocked("loading");
  await app.waitFor("text:目录加载中");
  await app.type("id:composer-input", "/compact ");
  await app.press("label:发送");
  await app.waitFor("text:/正在读取命令目录/");
  expect(app.texts()).toContain("sent:0");
});

test("TUI: 目录失败，/compact 空格被拦截；/learn 仍作为模板发送", async () => {
  const app = await openBlocked("error");
  await app.waitFor("text:/目录错误/");
  await app.type("id:composer-input", "/compact ");
  await app.press("label:发送");
  await app.waitFor("text:/命令目录不可用/");
  expect(app.texts()).toContain("sent:0");

  await app.clear("id:composer-input");
  await app.type("id:composer-input", "/learn 发布流程");
  await app.press("label:发送");
  await app.waitFor("text:sent:1");
  expect(app.texts().join("\n")).toContain("已发送沉淀请求");
});
