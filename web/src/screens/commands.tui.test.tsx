// TUI gate for chat commands (change keel-plugin-commands, tasks 4.4/5.5).
//
// The whole app on the terminal host against the fixture transport: the real
// Composer, the real catalog menu, the generated form, the destructive
// approval card and the `command.invoked` result row. The fixture serves the
// same catalog shape a live host does (builtin → plugin → skill), including a
// plugin command that does not target the terminal, so "unavailable" is shown
// with its reason instead of being hidden.
import { expect, open, test } from "nsh/test";

const app = (width = 1440) => open("../../App.tsx", { width, host: { params: { fixture: true } } });

const joined = (view: { texts(): string[] }): string => view.texts().join("\n");

const openSession = async (width = 1440) => {
  const view = await app(width);
  await view.waitFor("id:session-list");
  await view.press("label:打开会话 发布准备周报");
  await view.waitFor("id:composer-input");
  return view;
};

test("TUI: 命令目录——同序、来源分组与不可用原因", async () => {
  const view = await openSession();
  await view.type("id:composer-input", "/");
  await view.waitFor("id:composer-commands");

  const text = joined(view);
  expect(text).toContain("内置命令");
  expect(text).toContain("插件命令");
  expect(text).toContain("技能");
  const compact = text.indexOf("/compact");
  const plan = text.indexOf("/demo.workflow:plan");
  const skill = text.indexOf("/skill:deploy-flow");
  expect(compact).toBeGreaterThanOrEqual(0);
  expect(plan).toBeGreaterThan(compact);
  expect(skill).toBeGreaterThan(plan);
  // The audit command declares targets [web, native]: the terminal keeps its
  // row and explains why it cannot run.
  expect(text).toContain("/demo.workflow:audit");
  expect(text).toContain("当前客户端不支持");
  expect(view.shows("label:命令 /demo.workflow:audit 不可用：当前客户端不支持（仅 web / native）")).toBe(true);

  // The query filters by name and summary, not by a second local list.
  await view.clear("id:composer-input");
  await view.type("id:composer-input", "/pl");
  await view.waitFor("text:/demo\\.workflow:plan/");
  expect(joined(view)).not.toContain("/compact");
});

test("TUI: 内置 /compact——选中即执行，结果行与压缩分隔线都在", async () => {
  const view = await openSession();
  await view.type("id:composer-input", "/compact");
  await view.waitFor("label:执行命令 /compact");
  await view.press("label:发送");
  await view.waitFor("text:/上下文已压缩/");
  // The result row names the command and its status; the compaction keeps its
  // own presentation (a divider), not a fake assistant message.
  const text = joined(view);
  expect(text).toContain("/compact");
  expect(text).toContain("成功");
  expect(view.shows("id:composer-commands")).toBe(false);
});

test("TUI: 参数表单——字段级校验、执行与结构化结果", async () => {
  const view = await openSession();
  await view.type("id:composer-input", "/demo.workflow:plan");
  await view.waitFor("id:composer-commands-pick-demo.workflow:plan");
  await view.press("id:composer-commands-pick-demo.workflow:plan");
  await view.waitFor("id:composer-command-form");

  const formText = joined(view);
  expect(formText).toContain("/demo.workflow:plan");
  expect(formText).toContain("goal");

  // An empty required field is a field-level error, not a message.
  await view.press("id:composer-command-execute");
  await view.waitFor("text:必填");

  await view.type("id:command-field-goal", "发布计划");
  await view.clear("id:command-field-depth");
  await view.type("id:command-field-depth", "4");
  await view.press("id:composer-command-execute");
  await view.waitFor("text:/\"goal\":\"发布计划\"/");
  await view.waitFor("text:/成功/");
  expect(view.shows("id:composer-command-form")).toBe(false);
  const text = joined(view);
  expect(text).toContain("/demo.workflow:plan");
  expect(text).toContain("depth");
});

test("TUI: 单 text 属性——剩余文本作为 args.text，不套表单", async () => {
  const view = await openSession();
  await view.type("id:composer-input", "/demo.workflow:note");
  await view.waitFor("label:执行命令 /demo.workflow:note");
  await view.type("id:composer-input", " 今天先发布");
  await view.press("label:发送");
  await view.waitFor("text:/今天先发布/");
  await view.waitFor("text:/成功/");
  expect(view.shows("id:composer-command-form")).toBe(false);
  const text = joined(view);
  expect(text).toContain("/demo.workflow:note");
  expect(text).toContain('"note":"今天先发布"');
});

test("TUI: destructive——审批卡拒绝后命令行显示已拒绝，不产生成功结果", async () => {
  const view = await openSession();
  await view.type("id:composer-input", "/demo.workflow:purge");
  await view.waitFor("label:执行命令 /demo.workflow:purge");
  await view.press("label:发送");
  await view.waitFor("text:/demo\\.workflow:purge/");
  await view.waitFor("text:/等待审批|Run the destructive command/");
  await view.press("text:取消");
  await view.waitFor("text:/已拒绝|审批被拒绝/");
  const text = joined(view);
  expect(text).toContain("已拒绝");
  expect(text).toContain("decision_refused");
  // The refusal is a command row, not an assistant message: the command's
  // own result stays visible beside the answered approval card.
  expect(text).toContain("the destructive command was refused");
});

test("TUI: destructive——审批允许后只执行一次并给出结果行", async () => {
  const view = await openSession();
  await view.type("id:composer-input", "/demo.workflow:purge");
  await view.waitFor("label:执行命令 /demo.workflow:purge");
  await view.press("label:发送");
  await view.waitFor("text:/Run the destructive command/");
  await view.press("text:执行");
  await view.waitFor("text:/\"ok\":true/");
  await view.waitFor("text:/成功/");
});

test("TUI: 命令面板——Mod+K 的“命令”分组与 Composer 同目录", async () => {
  const view = await openSession();
  await view.keyboard.press("mod+k");
  await view.waitFor("id:command-palette");
  await view.type("id:command-palette", "compact");
  await view.waitFor("text:/\\/compact/");
  await view.press("id:command-palette >> text:/compact/");
  await view.waitFor("text:/上下文已压缩/");
  expect(view.shows("id:command-palette")).toBe(false);
});
