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
  expect(app.shows("id:provider-card-magpie")).toBe(true);
  expect(app.shows("id:provider-card-anthropic")).toBe(true);

  // Status badges: three credentials resolve; Anthropic's does not.
  const texts = app.texts();
  expect(texts.filter((text) => text === "已配置").length).toBe(3);
  expect(texts.filter((text) => text === "不可用").length).toBe(1);
  expect(app.shows("id:provider-model-magpie-free")).toBe(true);

  // Model chips live on the card, in the reader's words.
  expect(app.shows("id:provider-model-s2api-deepseek-v4.1-flash")).toBe(true);
  expect(app.shows("id:provider-model-anthropic-claude-sonnet-5")).toBe(true);
  expect(texts).toContain("Claude Sonnet 5");

  // The advanced raw document is closed at the bottom until asked for.
  await app.scroll("id:settings-models-scroll", 4000);
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
  // A6API joined Anthropic in the 不可用 state; S2API and Magpie stay 已配置.
  expect(texts.filter((text) => text === "不可用").length).toBe(2);
  expect(texts.filter((text) => text === "已配置").length).toBe(2);
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

  // Step 3: save and see the new card, already resolvable. The card is
  // appended after the existing providers, below the test viewport.
  await app.press("label:保存并生效");
  await app.waitFor("id:provider-config-dialog", { state: "gone" });
  await app.scroll("id:settings-models-scroll", 4000);
  await app.waitFor("id:provider-card-local");
  expect(app.texts()).toContain("Local Gateway");
  expect(app.texts().filter((text) => text === "已配置").length).toBe(4);
});

// ---- keel-model-catalog A: direct credential entry ------------------------

test("TUI: 凭据——直接粘贴密钥保存后输入框清空，显示掩码预览与「已保存」", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-configure-s2api");
  await app.press("label:配置 S2API");
  await app.waitFor("id:provider-config-dialog");

  // The one-shot field: type a >18-rune key and save it without touching the
  // document yet.
  await app.type("id:provider-config-direct-credential", "sk-live-0123456789-abcdefghijklmnop");
  await app.press("label:保存直接粘贴的密钥");

  // The value is gone from the field and never echoed: the masked preview is
  // first6 + … + last6 (the >18 rule).
  await app.waitFor("id:provider-config-credential-preview");
  const texts = app.texts();
  expect(texts.join("\n")).toContain("sk-liv…klmnop");
  expect(texts.join("\n")).not.toContain("sk-live-0123456789-abcdefghijklmnop");
  expect(app.value("id:provider-config-direct-credential")).toBe("");

  // A short key shows the fixed mask and no prefix.
  await app.type("id:provider-config-direct-credential", "shortkey");
  await app.press("label:保存直接粘贴的密钥");
  await app.waitFor("text:/••••••••/");
  expect(app.texts().join("\n")).not.toContain("shortkey");

  // Saving the document now puts the reference in, not the literal; the
  // credential field flipped to the minted reference, and the saved card shows
  // it (the resolved provider view). The whole screen is checked for the
  // literal, which must never appear anywhere.
  await app.press("label:保存并生效");
  await app.waitFor("id:provider-config-dialog", { state: "gone" });
  await app.waitFor("text:/secret:provider_s2api/");
  const settled = app.texts().join("\n");
  expect(settled).not.toContain("sk-live-0123456789-abcdefghijklmnop");
  expect(settled).not.toContain("shortkey");
  expect(settled).toContain("••••••••");
  // The stored row's masked preview reaches the card footer (it is read from
  // the secrets list the direct-entry write refreshed).
  await app.waitFor("id:provider-credential-preview-s2api");
});

// ---- keel-model-catalog B: live model discovery ---------------------------

test("TUI: 拉取——探测成功后并入新模型，已配置的不删，失败给出可读状态", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-configure-s2api");
  await app.press("label:配置 S2API");
  await app.waitFor("id:provider-config-dialog");

  // The probe answers the demo's live line-up: two ids are new, the configured
  // one stays exactly once and in place.
  await app.press("label:从服务端拉取模型");
  await app.waitFor("id:provider-probe-status");
  const texts = app.texts();
  expect(texts.join("\n")).toContain("可用");
  expect(texts.join("\n")).toContain("服务端 4 个模型");
  expect(app.shows("id:provider-config-model-deepseek-v4.1-pro")).toBe(true);
  expect(app.shows("id:provider-config-model-gemini-flash-3")).toBe(true);
  // The configured id was not duplicated: the dialog carries one chip per id
  // (testIDs are unique per model), and mergeLiveModels is unit-tested to
  // dedupe by identity.
  expect(app.shows("id:provider-config-model-deepseek-v4.1-flash")).toBe(true);
});

test("TUI: 拉取——凭据不可用时给出可读失败，一个模型也不并入", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-configure-anthropic");
  await app.press("label:配置 Anthropic");
  await app.waitFor("id:provider-config-dialog");

  await app.press("label:从服务端拉取模型");
  await app.waitFor("id:provider-probe-status");
  const text = app.texts().join("\n");
  // The measured state is a result, not a crash: the badge names the state and
  // the reason names the missing credential without a value.
  expect(text).toContain("凭据不可用");
  expect(text).toContain("no credential could be resolved");
  // Nothing was merged into the editor's configured line-up.
  expect(app.shows("id:provider-config-model-claude-sonnet-5")).toBe(true);
  expect(app.shows("id:provider-config-model-gemini-flash-3")).toBe(false);
});

// ---- keel-model-catalog C: per-model metadata + pricing -------------------

test("TUI: 模型参数——编辑上下文/输出/模态/能力与价格，保存后卡片摘要随之更新", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-model-edit-s2api-deepseek-v4.1-flash");
  await app.press("id:provider-model-edit-s2api-deepseek-v4.1-flash");
  await app.waitFor("id:model-metadata-dialog");

  // Every field is prefilled from the resolved facts — no raw JSON, no id as
  // the label.
  expect(app.value("id:model-context-window")).toBe("128000");
  expect(app.value("id:model-max-output")).toBe("8192");
  expect(app.value("id:model-pricing-input")).toBe("0.27");
  expect(app.value("id:model-pricing-output")).toBe("1.1");
  expect(app.texts().join("\n")).toContain("每 1M tokens");

  // Edit the window, max output and price; toggle image input and the free
  // marker off.
  await app.clear("id:model-context-window");
  await app.type("id:model-context-window", "200000");
  await app.clear("id:model-max-output");
  await app.type("id:model-max-output", "16384");
  await app.clear("id:model-pricing-input");
  await app.type("id:model-pricing-input", "0.5");
  await app.clear("id:model-pricing-cache-read");
  await app.press("id:model-input-image");
  await app.press("id:model-reasoning-minimal");
  // The form is longer than the hosted viewport; the reader scrolls to the
  // action row. The dialog's Scroll is the real one a reader uses.
  await app.scroll("id:model-metadata-dialog-scroll", 4000);
  await app.press("id:model-metadata-save");

  await app.waitFor("id:model-metadata-dialog", { state: "gone" });
  // The card's fact line reflects the saved values (the fixture resolves the
  // exact providerModelRules entry, as the server does).
  await app.waitFor("text:/200K 上下文 · 16,384 输出 · 输入 USD 0.50/");
  // The saved document carries ONE exact rule for this model. The advanced
  // editor shows the app's live document (the test's module store is a
  // different instance, so the UI is the honest read).
  await app.scroll("id:settings-models-scroll", 4000);
  await app.press("id:settings-advanced");
  const raw = app.value("id:settings-raw-document") ?? "";
  expect(raw.length).toBeGreaterThan(0);
  const document = JSON.parse(raw) as Record<string, unknown>;
  const rules = (document.providerModelRules as Array<Record<string, unknown>>) ?? [];
  const rule = rules.find(
    (row) => row.providerId === "s2api" && row.modelId === "deepseek-v4.1-flash",
  ) as Record<string, unknown>;
  const config = rule.config as Record<string, unknown>;
  const properties = config.properties as Record<string, unknown>;
  expect(properties.contextWindow).toBe(200000);
  expect((properties.inputFormat as Record<string, unknown>).image).toBe(true);
  const pricing = config.pricing as Record<string, unknown>;
  expect(pricing.input).toBe(0.5);
  // A cleared price field is an explicit null, not "leave the old rate".
  expect(pricing.cacheRead).toBe(null);
  // The thinking levels are written in runtime order and the new level got
  // its (empty) request patch; the existing patches/levels are still there.
  const options = config.options as Record<string, unknown>;
  const reasoning = options.reasoningLevel as Record<string, unknown>;
  expect(reasoning.values).toEqual(["off", "minimal", "low", "medium", "high"]);
  expect((reasoning.map as Record<string, unknown>).minimal).toEqual({});
});

test("TUI: 模型参数——非法价格被拒，Dialog 不关闭且说明原因", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-model-edit-anthropic-claude-sonnet-5");
  await app.press("id:provider-model-edit-anthropic-claude-sonnet-5");
  await app.waitFor("id:model-metadata-dialog");

  await app.clear("id:model-pricing-input");
  await app.type("id:model-pricing-input", "-1");
  await app.press("id:model-metadata-save");
  await app.waitFor("text:/输入价格必须是非负数字/");
  expect(app.shows("id:model-metadata-dialog")).toBe(true);

  // A non-numeric value is refused the same way, and nothing was saved.
  await app.clear("id:model-pricing-input");
  await app.type("id:model-pricing-input", "cheap");
  await app.press("id:model-metadata-save");
  await app.waitFor("text:/输入价格必须是非负数字/");
  expect(app.shows("id:model-metadata-dialog")).toBe(true);
});

test("TUI: 模型参数——清空上下文与输出上限写成 null，卡片显示未声明", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-model-edit-s2api-deepseek-v4.1-flash");
  await app.press("id:provider-model-edit-s2api-deepseek-v4.1-flash");
  await app.waitFor("id:model-metadata-dialog");

  await app.clear("id:model-context-window");
  await app.clear("id:model-max-output");
  await app.press("id:model-metadata-save");
  await app.waitFor("id:model-metadata-dialog", { state: "gone" });
  // Missing is 未声明, never the resolver's zero default.
  await app.waitFor("text:/上下文未声明 · 输出未声明/");

  await app.scroll("id:settings-models-scroll", 4000);
  await app.press("id:settings-advanced");
  const raw = app.value("id:settings-raw-document") ?? "";
  expect(raw.length).toBeGreaterThan(0);
  const document = JSON.parse(raw) as Record<string, unknown>;
  const rule = ((document.providerModelRules as Array<Record<string, unknown>>) ?? []).find(
    (row) => row.providerId === "s2api" && row.modelId === "deepseek-v4.1-flash",
  ) as Record<string, unknown>;
  const config = rule.config as Record<string, unknown>;
  expect((config.properties as Record<string, unknown>).contextWindow).toBe(null);
  const options = config.options as Record<string, unknown>;
  expect((options.maxOutputTokens as Record<string, unknown>).max).toBe(null);
});

test("TUI: 模型参数——原样保存是 no-op，不新增规则也不写 false 模态", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-model-edit-s2api-deepseek-v4.1-flash");
  await app.press("id:provider-model-edit-s2api-deepseek-v4.1-flash");
  await app.waitFor("id:model-metadata-dialog");

  await app.press("id:model-metadata-save");
  await app.waitFor("id:model-metadata-dialog", { state: "gone" });

  await app.scroll("id:settings-models-scroll", 4000);
  await app.press("id:settings-advanced");
  const document = JSON.parse(app.value("id:settings-raw-document") ?? "") as Record<string, unknown>;
  const rules = (document.providerModelRules as Array<Record<string, unknown>>) ?? [];
  // The untouched dialog writes nothing at all: no exact rule is invented, so
  // no capability can be silently rewritten.
  expect(rules.some((row) => row.providerId === "s2api" && row.modelId === "deepseek-v4.1-flash")).toBe(false);
  // The card's facts are intact.
  expect(app.texts().join("\n")).toContain("128K 上下文");
});

test("TUI: 模型参数——凭据不可解析的模型按解析视图回填，原样保存不写空能力", async () => {
  const app = await openModels();
  // The executable registry omits this model (its credential does not resolve),
  // but the settings view resolves its facts: the card shows them instead of
  // 未声明.
  await app.waitFor("id:provider-card-anthropic");
  await app.waitFor("text:/200K 上下文/");
  await app.press("id:provider-model-edit-anthropic-claude-sonnet-5");
  await app.waitFor("id:model-metadata-dialog");

  // The seed is the resolved fact sheet, not a blank form.
  expect(app.value("id:model-context-window")).toBe("200000");
  expect(app.value("id:model-max-output")).toBe("8192");
  expect(app.value("id:model-pricing-input")).toBe("3");
  // The dialog says why the model is not usable, and that untouched facts are
  // not written back.
  expect(app.shows("id:model-unavailable-note")).toBe(true);
  expect(app.texts().join("\n")).toContain("缺少凭据");
  expect(app.texts().join("\n")).toContain("未修改的字段不会被写回");

  // Save without edits.
  await app.press("id:model-metadata-save");
  await app.waitFor("id:model-metadata-dialog", { state: "gone" });

  await app.scroll("id:settings-models-scroll", 4000);
  await app.press("id:settings-advanced");
  const document = JSON.parse(app.value("id:settings-raw-document") ?? "") as Record<string, unknown>;
  const rules = (document.providerModelRules as Array<Record<string, unknown>>) ?? [];
  // No rule, therefore no `text:false` flags and no `reasoningLevel:null`: the
  // model keeps every capability it had.
  expect(rules.some((row) => row.providerId === "anthropic" && row.modelId === "claude-sonnet-5")).toBe(false);
  expect(app.texts().join("\n")).toContain("200K 上下文");
});

test("TUI: 模型参数——只改一个字段时其它已解析事实原样保留", async () => {
  const app = await openModels();
  await app.waitFor("id:provider-model-edit-anthropic-claude-sonnet-5");
  await app.press("id:provider-model-edit-anthropic-claude-sonnet-5");
  await app.waitFor("id:model-metadata-dialog");

  await app.clear("id:model-context-window");
  await app.type("id:model-context-window", "210000");
  await app.press("id:model-metadata-save");
  await app.waitFor("id:model-metadata-dialog", { state: "gone" });

  await app.scroll("id:settings-models-scroll", 4000);
  await app.press("id:settings-advanced");
  const document = JSON.parse(app.value("id:settings-raw-document") ?? "") as Record<string, unknown>;
  const rules = (document.providerModelRules as Array<Record<string, unknown>>) ?? [];
  const rule = rules.find(
    (row) => row.providerId === "anthropic" && row.modelId === "claude-sonnet-5",
  ) as Record<string, unknown>;
  const config = rule.config as Record<string, unknown>;
  // Exactly one section holds exactly the edited value. The modalities, the
  // capability flags, the reasoning levels and the price list are absent, so
  // they keep their resolved values.
  expect(config.properties).toEqual({ contextWindow: 210000 });
  expect(config.options).toBe(undefined);
  expect(config.pricing).toBe(undefined);

  // The card keeps the untouched facts.
  const text = app.texts().join("\n");
  expect(text).toContain("210K 上下文");
  expect(text).toContain("8,192 输出");
  expect(text).toContain("输入 USD 3.00 · 输出 USD 15.00");
});

// ---- 390 / 1440: the two columns -------------------------------------------

const openModelsAt = (width: number) => open(HARNESS, { width, host: { params: { fixture: true } } });

test("TUI: 模型分区——390 窄屏：卡片事实摘要与「参数」入口仍可读", async () => {
  const app = await openModelsAt(390);
  await app.waitFor("id:provider-card-s2api");
  const text = app.texts().join("\n");
  // The facts line wraps instead of truncating away the price.
  expect(text).toContain("128K 上下文");
  expect(text).toContain("输入 USD 0.27");
  expect(app.shows("id:provider-model-edit-s2api-deepseek-v4.1-flash")).toBe(true);
  expect(app.shows("id:provider-configure-s2api")).toBe(true);
});

test("TUI: 模型参数——390 窄屏：对话框字段与保存动作可见", async () => {
  const app = await openModelsAt(390);
  await app.waitFor("id:provider-model-edit-s2api-deepseek-v4.1-flash");
  await app.press("id:provider-model-edit-s2api-deepseek-v4.1-flash");
  await app.waitFor("id:model-metadata-dialog");
  expect(app.shows("id:model-context-window")).toBe(true);
  expect(app.shows("id:model-pricing-input")).toBe(true);
  expect(app.value("id:model-context-window")).toBe("128000");
});

test("TUI: 模型分区——1440 宽屏：默认模型摘要与 provider 卡片同时可读", async () => {
  const app = await openModelsAt(1440);
  await app.waitFor("id:settings-default-facts");
  const text = app.texts().join("\n");
  expect(text).toContain("128K 上下文 · 8,192 输出 · 输入 USD 0.27 · 输出 USD 1.10 / 1M tokens");
  expect(app.shows("id:provider-card-a6api")).toBe(true);
});
