// C2 · kit form components — TUI gate: typing into the kit's fields (Field /
// Label / Input / Textarea), the disabled/readonly states, and the choice
// controls driving app-owned state (Switch / Checkbox / RadioGroup / Select /
// Combobox / Command).
import { expect, open, test } from "nsh/test";

test("TUI: C2 输入件——Field 三态与键入回读", async () => {
  const app = await open("./harness-form.tsx");
  expect(app.texts()).toContain("会话标题");
  expect(app.texts()).toContain("用于列表显示");
  expect(app.texts()).toContain("备注过长");
  expect(app.texts()).toContain("描述");
  expect(app.texts()).toContain("只读字段");

  await app.type("id:input-title", "发布周报");
  await app.waitFor("text:/标题=发布周报/");
  await app.type("id:input-desc", "第一行备注");
  await app.waitFor("text:/描述=第一行备注/");

  expect(app.enabled("id:input-disabled")).toBe(false);
  expect(app.enabled("id:input-readonly")).toBe(false);
  expect(app.value("id:input-readonly")).toBe("已锁定");
});

test("TUI: C2 开关件——Switch / Checkbox / RadioGroup 状态切换", async () => {
  const app = await open("./harness-choice.tsx");
  await app.waitFor("text:重试=开");
  await app.press("id:switch-retry");
  await app.waitFor("text:重试=关");
  await app.press("id:switch-retry");
  await app.waitFor("text:重试=开");

  await app.waitFor("text:记住=关");
  await app.press("id:check-remember");
  await app.waitFor("text:记住=开");
  await expect(app.press("id:check-disabled")).rejects.toMatchObject({ code: "ERR_NSH_TEST_DISABLED" });

  await app.waitFor("text:档位=中");
  await app.press("label:低");
  await app.waitFor("text:档位=低");
  await app.press("label:高");
  await app.waitFor("text:档位=高");
});

test("TUI: C2 Select 选择与 Command 过滤选择", async () => {
  const app = await open("./harness-choice.tsx");
  await app.waitFor("text:模型=无");

  // Select: the trigger opens the list; the disabled row is not choosable.
  // (The native Select's press target is its trigger row, not the wrapper the
  // testID lands on, so the trigger is found by its shown placeholder.)
  await app.press("text:请选择模型");
  await app.waitFor("text:deepseek-pro");
  await app.press("text:deepseek-pro");
  await app.waitFor("text:模型=pro");

  // Command: the search field filters, Return/a press chooses.
  await app.waitFor("text:未选择");
  await app.type("id:command-palette", "回滚");
  await app.press("text:回滚上一个版本");
  await app.waitFor("text:/已选 rollback/");
});

test("TUI: C2 Combobox 输入过滤与多选回读", async () => {
  const app = await open("./harness-choice.tsx");
  await app.waitFor("text:多选=无");
  await app.type("id:combobox-models", "pro");
  await app.press("text:deepseek-pro");
  await app.waitFor("text:/多选=pro/");
});
