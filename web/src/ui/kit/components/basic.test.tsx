// C1 · kit components — TUI gate. Renders `harness-basic.tsx` on the terminal
// host and checks the three states the kit's contract promises: default content,
// a disabled control that is unfocusable and rejects presses, and a live state
// change through a token-styled control.
import { expect, open, test } from "nsh/test";

test("TUI: C1 基础件渲染（Button/Badge/Separator/Kbd/Spinner/Avatar/Skeleton）", async () => {
  const app = await open("./harness-basic.tsx");
  const texts = app.texts();
  expect(texts).toContain("主按钮");
  expect(texts).toContain("次按钮");
  expect(texts).toContain("描边按钮");
  expect(texts).toContain("幽灵按钮");
  expect(texts).toContain("危险按钮");
  expect(texts).toContain("链接按钮");
  expect(texts).toContain("禁用按钮");
  expect(texts).toContain("非法按钮");
  expect(texts).toContain("加载中");
  expect(texts).toContain("大按钮");
  expect(texts).toContain("默认");
  expect(texts).toContain("成功");
  expect(texts).toContain("分区");
  expect(texts).toContain("⌘");
  expect(texts).toContain("K");
  expect(texts).toContain("AL");
  expect(texts).toContain("计数 0");
});

test("TUI: C1 disabled 按钮不可按（ERR_NSH_TEST_DISABLED）", async () => {
  const app = await open("./harness-basic.tsx");
  expect(app.enabled("id:btn-disabled")).toBe(false);
  expect(app.enabled("id:btn-default")).toBe(true);
  await expect(app.press("id:btn-disabled")).rejects.toMatchObject({ code: "ERR_NSH_TEST_DISABLED" });
});

test("TUI: C1 状态切换（Button onPress 驱动应用状态）", async () => {
  const app = await open("./harness-basic.tsx");
  await app.press("id:btn-default");
  await app.waitFor("text:/计数 1/");
  await app.press("id:btn-default");
  await app.waitFor("text:/计数 2/");
});
