// The top band's plugin slot — A4.4f. A contribution renders in the slot; no
// contribution renders nothing at all (the band must never show a fake panel or
// an empty strip where a plugin would be).
import { expect, open, test } from "nsh/test";

test("TUI: 顶部插件槽——贡献渲染，空态不出现假面板", async () => {
  const filled = await open("./plugin-top-panels.fixture.tsx");
  await filled.waitFor("text:插件面板内容");
  expect(filled.shows("id:plugin-top-panels")).toBe(true);
  expect(filled.shows("id:plugin-top-panel-demo")).toBe(true);

  const empty = await open("./plugin-top-panels.empty.fixture.tsx");
  expect(empty.shows("id:plugin-top-panels")).toBe(false);
  expect(empty.texts().join("\n")).not.toContain("插件面板内容");
});
