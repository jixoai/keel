// TUI gate for the composer's `$` menu (change keel-harness-v2, W7 skills).
//
// One wake-up character, three groups, one list. The test types `$` into the
// real composer on the fixture transport, reads the three group headings, and
// presses a row of each group: the chosen value is appended to the input as
// `$<label> ` and nothing is sent. `/learn` is not a menu row; the pure test
// pins its instruction (`composer-completion.test.ts`).
import { expect, open, test } from "nsh/test";

const HARNESS = "./composer-completion.harness.tsx";

const joined = (app: { texts(): string[] }): string => app.texts().join("\n");

const openComposer = () => open(HARNESS, { host: { params: { fixture: true } } });

test("TUI: $ 补全——技能/文件/任务三组共用唤醒符，空查询列出全部", async () => {
  const app = await openComposer();
  await app.waitFor("id:composer-input");
  await app.type("id:composer-input", "$");

  await app.waitFor("id:composer-completion");
  expect(app.shows("id:composer-completion-group-skills")).toBe(true);
  expect(app.shows("id:composer-completion-group-files")).toBe(true);
  expect(app.shows("id:composer-completion-group-tasks")).toBe(true);
  const text = joined(app);
  expect(text).toContain("技能");
  expect(text).toContain("文件");
  expect(text).toContain("任务");
  expect(text).toContain("deploy-flow");
  expect(text).toContain("src/composer.tsx");
  expect(text).toContain("回滚演练");
});

test("TUI: $ 补全——选择技能写入输入框，不发送", async () => {
  const app = await openComposer();
  await app.waitFor("id:composer-input");
  await app.type("id:composer-input", "$deploy");

  await app.waitFor("id:composer-completion");
  // The query filters: only the skill group survives a skill-only query.
  expect(app.shows("id:composer-completion-group-skills")).toBe(true);
  expect(app.shows("id:composer-completion-group-files")).toBe(false);

  await app.press("label:插入 技能 deploy-flow");
  await app.waitFor("id:composer-completion", { state: "gone" });
  expect(app.value("id:composer-input")).toBe("$deploy-flow ");
});

test("TUI: $ 补全——文件与任务走同一唤醒符，分别插入路径与任务名", async () => {
  const app = await openComposer();
  await app.waitFor("id:composer-input");

  await app.type("id:composer-input", "$src/");
  await app.waitFor("id:composer-completion");
  expect(app.shows("id:composer-completion-group-files")).toBe(true);
  expect(app.shows("id:composer-completion-group-skills")).toBe(false);
  await app.press("label:插入 文件 src/composer.tsx");
  expect(app.value("id:composer-input")).toBe("$src/composer.tsx ");

  await app.clear("id:composer-input");
  await app.type("id:composer-input", "$回滚");
  await app.waitFor("id:composer-completion");
  expect(app.shows("id:composer-completion-group-tasks")).toBe(true);
  await app.press("label:插入 任务 回滚演练");
  expect(app.value("id:composer-input")).toBe("$回滚演练 ");
});

test("TUI: $ 补全——没有匹配时给出可读空态，不留空白菜单", async () => {
  const app = await openComposer();
  await app.waitFor("id:composer-input");
  await app.type("id:composer-input", "$zzz");

  await app.waitFor("id:composer-completion");
  expect(joined(app)).toContain("没有匹配的技能、文件或任务");
  expect(app.shows("id:composer-completion-group-skills")).toBe(false);
});
