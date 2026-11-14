// TUI gate for settings「模型接入」— the two-level structure.
//
// The page must answer three questions without a long scrolling form:
//   ① which model is in force?           → the 当前默认模型 card
//   ② which providers are available?     → one card per provider, with a badge
//   ③ what does adding one involve?      → the「配置」dialog /「添加 Provider」
//
// These tests run the real section on the fixture transport. They pin the
// information architecture (cards, not rows), the dialog's prefilled fields,
// and the round trip: a save closes the dialog and the card's badge changes.
import { expect, open, test } from "nsh/test";

const HARNESS = "./settings-models.harness.tsx";

const openModels = () => open(HARNESS, { host: { params: { fixture: true } } });

test("TUI: 模型分区——当前默认区显示 provider 与模型，只有更换/刷新两个动作", async () => {
  const app = await openModels();
  await app.waitFor("id:settings-default-model");

  const texts = app.texts();
  // ① Who is in force: the provider, the model and its reasoning level.
  expect(texts).toContain("S2API");
  expect(texts).toContain("DeepSeek V4.1 Flash · 高");
  expect(texts.join("\n")).toContain("s2api/deepseek-v4.1-flash");
  // The source and the version stay visible.
  expect(texts).toContain("部署默认");
  expect(texts).toContain("第 3 版");

  // The card's whole action set is 更换 + 刷新.
  expect(app.shows("id:settings-change-default")).toBe(true);
  expect(app.shows("id:settings-refresh-models")).toBe(true);
});

test("TUI: 模型分区——一个 provider 一张卡片，带已配置/不可用徽标与模型 chips", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-card-s2api");

  // ② One card per provider, grouped by provider (not rows in one list).
  expect(app.shows("id:provider-card-a6api")).toBe(true);
  expect(app.shows("id:provider-card-anthropic")).toBe(true);

  // Status badges: two credentials resolve; Anthropic's does not.
  const texts = app.texts();
  expect(texts.filter((text) => text === "已配置").length).toBe(2);
  expect(texts.filter((text) => text === "不可用").length).toBe(1);

  // Model chips live on the card, in the reader's words.
  expect(app.shows("id:provider-model-s2api-deepseek-v4.1-flash")).toBe(true);
  expect(app.shows("id:provider-model-anthropic-claude-sonnet-5")).toBe(true);
  expect(texts).toContain("Claude Sonnet 5");

  // The advanced raw document is closed at the bottom until asked for.
  expect(app.shows("id:settings-raw-document")).toBe(false);
  expect(app.shows("id:settings-advanced")).toBe(true);
});

test("TUI: 模型分区——点「配置」打开 Dialog，字段按该 provider 回填", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-configure-s2api");
  await app.press("label:配置 S2API");
  await app.waitFor("id:provider-config-dialog");

  // ③ The dialog edits exactly one provider, and every field is prefilled.
  expect(app.value("id:provider-config-id")).toBe("s2api");
  expect(app.value("id:provider-config-name")).toBe("S2API");
  expect(app.value("id:provider-config-endpoint")).toBe("https://s2api.top/v1");
  expect(app.value("id:provider-config-credential")).toBe("env:S2API_KEY");
  const texts = app.texts();
  expect(texts).toContain("OpenAI 兼容");
  expect(texts).toContain("DeepSeek V4.1 Flash");
  // The credential hint names the three spellings.
  expect(texts.join("\n")).toContain("secret:NAME");
  expect(texts.join("\n")).toContain("env:NAME");
  expect(texts).toContain("删除该 Provider");
  expect(texts).toContain("取消");
  expect(texts).toContain("保存并生效");
});

test("TUI: 模型分区——保存后回到列表，卡片状态与徽标随之更新", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-configure-a6api");
  await app.press("label:配置 A6API");
  await app.waitFor("id:provider-config-dialog");

  // Point A6API at a credential this process cannot resolve.
  await app.clear("id:provider-config-credential");
  await app.type("id:provider-config-credential", "env:MISSING_KEY");
  await app.press("label:保存并生效");

  await app.waitFor("id:provider-config-dialog", { state: "gone" });
  await app.waitFor("text:/credential MISSING_KEY is not set in this process/");

  const texts = app.texts();
  // A6API joined Anthropic in the 不可用 state; S2API is still 已配置.
  expect(texts.filter((text) => text === "不可用").length).toBe(2);
  expect(texts.filter((text) => text === "已配置").length).toBe(1);
  expect(app.value("id:provider-config-credential")).toBe(null);
});

test("TUI: 模型分区——保存失败时给出可读原因，Dialog 不关闭且输入不丢", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-configure-s2api");
  await app.press("label:配置 S2API");
  await app.waitFor("id:provider-config-dialog");

  // Remove the only model of the provider that the default selection points
  // at: the server rejects the whole document (the selection no longer names
  // a declared model).
  await app.press("label:移除模型 DeepSeek V4.1 Flash");
  await app.press("label:保存并生效");
  await app.waitFor("text:/保存被拒绝/");

  const texts = app.texts();
  expect(texts).toContain("selection.modelId");
  expect(texts.join("\n")).toContain(
    "selection s2api/deepseek-v4.1-flash does not match any declared model",
  );
  // The dialog is still the top layer, with the typed values intact.
  expect(app.shows("id:provider-config-dialog")).toBe(true);
  expect(app.value("id:provider-config-endpoint")).toBe("https://s2api.top/v1");
  expect(app.value("id:provider-config-credential")).toBe("env:S2API_KEY");
});

test("TUI: 模型分区——添加 Provider 的三步路径（点添加→填地址与凭据→保存即出现卡片）", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-add");

  // Step 1: the list's one add action opens the same dialog in new mode.
  await app.press("label:添加 Provider");
  await app.waitFor("id:provider-config-dialog");

  // Step 2: the two essentials — an endpoint and a credential reference — plus
  // a model id. No raw JSON is involved.
  await app.type("id:provider-config-id", "local");
  await app.type("id:provider-config-name", "Local Gateway");
  await app.type("id:provider-config-endpoint", "https://local.example.com/v1");
  await app.type("id:provider-config-credential", "env:A6API_KEY");
  await app.type("id:provider-config-model-input", "deepseek-v4.1-flash");
  await app.press("label:添加模型");
  await app.waitFor("id:provider-config-model-deepseek-v4.1-flash");

  // Step 3: save and see the new card, already resolvable.
  await app.press("label:保存并生效");
  await app.waitFor("id:provider-config-dialog", { state: "gone" });
  await app.waitFor("id:provider-card-local");
  expect(app.texts()).toContain("Local Gateway");
  expect(app.texts().filter((text) => text === "已配置").length).toBe(3);
});
