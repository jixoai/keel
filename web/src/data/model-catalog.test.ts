// The model-catalog vocabulary's derivations (change keel-model-catalog C/A):
// the price/capability summaries the picker and editor render, the merge-patch
// save path (one model, no collateral edits), the live-line-up merge (nothing
// removed, order preserved), and the masked preview mirror. Plain unit tests:
// no rendering, no store.
import { expect, test } from "nsh/test";
import type { ModelView, ProviderView } from "./contract";
import {
  FIXED_SECRET_MASK,
  REASONING_LEVELS,
  applyModelMetadata,
  editorSeed,
  formatPrice,
  formatTokens,
  isFree,
  maskedPreview,
  mergeJsonObjects,
  mergeLiveModels,
  metadataEditPatch,
  metadataOf,
  normalizeReasoningLevels,
  outputPriceSummary,
  priceSummary,
  providerCredentialSecretName,
  resolvedModelOf,
  secretPreviewLabel,
  windowSummary,
  type ModelMetadata,
} from "./model-catalog";

const model = (overrides: Partial<ModelView> = {}): ModelView => ({
  providerId: "gw",
  modelId: "gw-large",
  properties: {
    contextWindow: 1_000_000,
    inputFormat: { text: true, image: true, video: false, audio: false, pdf: true },
    outputFormat: { text: true },
    supportsToolCall: true,
    supportsJsonSchemaOutput: true,
    supportsNativeWebSearch: true,
  },
  options: {
    reasoningLevel: {
      values: ["low", "high"],
      map: { low: { reasoning_effort: "low" }, high: { reasoning_effort: "high" } },
    },
    maxOutputTokens: { max: 16384 },
  },
  ...overrides,
});

test("价格摘要：四档数字、免费标记与未声明三种说法", () => {
  expect(priceSummary(undefined)).toBe("价格未声明");
  expect(priceSummary({})).toBe("价格未声明");
  expect(priceSummary({ free: true })).toBe("免费");
  // A declared zero pair without the marker still reads as free (the badge
  // rule), while the summary states the numbers it declares.
  expect(priceSummary({ input: 0, output: 0, currency: "USD" })).toBe(
    "输入 USD 0 · 输出 USD 0 / 1M tokens",
  );
  expect(priceSummary({ input: 0.27, output: 1.1, currency: "USD" })).toBe(
    "输入 USD 0.27 · 输出 USD 1.10 / 1M tokens",
  );
  expect(priceSummary({ input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75, currency: "USD" })).toBe(
    "输入 USD 3.00 · 输出 USD 15.00 / 1M tokens",
  );
  // One side declared is a valid partial fact; no currency → the bare number.
  expect(priceSummary({ output: 2 })).toBe("输出 2.00 / 1M tokens");
  // Notes without a rate explain the absence instead of inventing one.
  expect(priceSummary({ notes: "off-peak only" })).toBe("价格未声明（off-peak only）");
});

test("价格格式：币种码在数字前，小数位数按精度需要", () => {
  expect(formatPrice(0, "USD")).toBe("USD 0");
  expect(formatPrice(0.27, "USD")).toBe("USD 0.27");
  expect(formatPrice(1.1, "USD")).toBe("USD 1.10");
  expect(formatPrice(0.00125, "USD")).toBe("USD 0.0013");
  expect(formatPrice(0.00125)).toBe("0.0013");
  expect(formatPrice(15, "CNY")).toBe("CNY 15.00");
});

test("上下文/输出：整数千位写成 K/M，其余按位保留，缺失说未声明", () => {
  expect(formatTokens(128000)).toBe("128K");
  expect(formatTokens(1000000)).toBe("1M");
  expect(formatTokens(200000)).toBe("200K");
  expect(formatTokens(65536)).toBe("65,536");
  expect(formatTokens(8192)).toBe("8,192");
  expect(formatTokens(999)).toBe("999");
  expect(windowSummary(model())).toBe("1M 上下文 · 16,384 输出");
  expect(windowSummary(undefined)).toBe("上下文未声明 · 输出未声明");
  expect(windowSummary(model({ properties: { contextWindow: 128000 }, options: {} }))).toBe(
    "128K 上下文 · 输出未声明",
  );
});

test("免费判定：显式标记或输入输出都为 0；未声明块不是免费", () => {
  expect(isFree(undefined)).toBe(false);
  expect(isFree({})).toBe(false);
  expect(isFree({ input: 0 })).toBe(false);
  expect(isFree({ input: 0, output: 0 })).toBe(true);
  expect(isFree({ free: true, input: 1 })).toBe(true);
});

test("模型元数据读回：属性、选项上限与价格块", () => {
  const metadata = metadataOf(model({ pricing: { currency: "USD", input: 0.5, free: true } }));
  expect(metadata.contextWindow).toBe(1000000);
  expect(metadata.maxOutputTokens).toBe(16384);
  expect(metadata.inputFormat.image).toBe(true);
  expect(metadata.inputFormat.pdf).toBe(true);
  expect(metadata.outputFormat.text).toBe(true);
  expect(metadata.supportsNativeWebSearch).toBe(true);
  expect(metadata.reasoningLevels).toEqual(["low", "high"]);
  expect(metadata.reasoningLevelMap?.high).toEqual({ reasoning_effort: "high" });
  expect(metadata.pricing?.free).toBe(true);
  expect(metadata.pricing?.input).toBe(0.5);
  // A view with no pricing block reads as 未声明, not the zero value.
  expect(metadataOf(model({ pricing: undefined })).pricing).toBe(undefined);
  // 0 is the resolver's "incomplete" default, not a declared limit: the UI
  // says 未声明 instead of printing "0 上下文 · 0 输出".
  const zeroed = metadataOf(model({ properties: { contextWindow: 0 }, options: { maxOutputTokens: { max: 0 } } }));
  expect(zeroed.contextWindow).toBe(undefined);
  expect(zeroed.maxOutputTokens).toBe(undefined);
  expect(windowSummary(model({ properties: { contextWindow: 0 }, options: { maxOutputTokens: { max: 0 } } }))).toBe(
    "上下文未声明 · 输出未声明",
  );
});

/** The form's own canonical values: every switch is a real boolean, no level is
 * selected, no price is declared. */
const blankForm: ModelMetadata = {
  inputFormat: { text: false, image: false, video: false, audio: false, pdf: false },
  outputFormat: { text: false, image: false, video: false, audio: false },
  supportsToolCall: false,
  supportsJsonSchemaOutput: false,
  supportsNativeWebSearch: false,
  reasoningLevels: [],
  reasoningLevelMap: {},
};

test("保存只写读者改动的字段：未动的模态/能力/思考档不进规则", () => {
  const document: Record<string, unknown> = {
    schemaVersion: 1,
    providers: [{ id: "gw", modelIds: ["a", "b"] }],
    providerModelRules: [
      {
        providerId: "gw",
        modelId: "a",
        config: {
          properties: { contextWindow: 128000 },
          options: { reasoningLevel: { values: ["high"], map: { high: { reasoning_effort: "high" } } } },
        },
      },
    ],
  };
  const before = editorSeed({
    contextWindow: 128000,
    inputFormat: { text: true, image: false },
    outputFormat: { text: true },
    supportsToolCall: true,
    reasoningLevels: ["high"],
    reasoningLevelMap: { high: { reasoning_effort: "high" } },
  });
  const after: ModelMetadata = {
    ...before,
    contextWindow: 200000,
    inputFormat: { text: true, image: true },
    maxOutputTokens: 4096,
    pricing: { currency: "USD", input: 1, output: 2 },
  };
  const patch = metadataEditPatch(before, after);
  // One exact rule: only the changed sections are present — the toggle writes
  // its own key, never the whole input format.
  expect(patch).toEqual({
    properties: { contextWindow: 200000, inputFormat: { image: true } },
    options: { maxOutputTokens: { max: 4096 } },
    pricing: { currency: "USD", input: 1, output: 2 },
  });
  const next = applyModelMetadata(document, "gw", "a", patch);
  const rules = next.providerModelRules as Array<Record<string, unknown>>;
  const config = (rules.find((rule) => rule.modelId === "a") as Record<string, unknown>)
    .config as Record<string, unknown>;
  const properties = config.properties as Record<string, unknown>;
  expect(properties.contextWindow).toBe(200000);
  expect(properties.inputFormat).toEqual({ image: true });
  // The capability flag the form did not change is not written; the resolved
  // value survives the merge.
  expect(properties.supportsToolCall).toBe(undefined);
  const options = config.options as Record<string, unknown>;
  expect(options.reasoningLevel).toEqual({
    values: ["high"],
    map: { high: { reasoning_effort: "high" } },
  });
  // One model's edit does not add a rule for the sibling model.
  expect(rules.filter((rule) => rule.modelId === "b").length).toBe(0);
  expect(next.providers).toEqual(document.providers);
});

test("思考档：只写改动后的档位，按运行时词汇升序，请求补丁保留并补齐", () => {
  const before = editorSeed({
    inputFormat: {},
    outputFormat: {},
    reasoningLevels: ["high"],
    reasoningLevelMap: { high: { reasoning_effort: "high" } },
  });
  // Deliberately unsorted; normalizeReasoningLevels is the authority.
  const patch = metadataEditPatch(before, { ...before, reasoningLevels: ["high", "minimal"] });
  expect(patch).toEqual({
    options: {
      reasoningLevel: {
        values: ["minimal", "high"],
        map: { minimal: {}, high: { reasoning_effort: "high" } },
      },
    },
  });
  expect(normalizeReasoningLevels(["max", "high", "bogus", "high"])).toEqual(["high", "max"]);
  expect(REASONING_LEVELS.join(",")).toBe("off,minimal,low,medium,high,xhigh,max");
});

test("清除：表单展示过的字段写 null，未展示的请求补丁与组成的字段不受影响", () => {
  const document: Record<string, unknown> = {
    schemaVersion: 1,
    providerModelRules: [
      {
        providerId: "gw",
        modelId: "a",
        config: {
          providerType: "llm",
          properties: { contextWindow: 128000, inputFormat: { text: true }, outputFormat: { text: true } },
          options: {
            reasoningLevel: { values: ["high"], map: { high: { reasoning_effort: "high" } } },
            maxOutputTokens: { max: 4096, map: { "": { max_tokens: 4096 } } },
          },
          pricing: { currency: "USD", input: 1, output: 2, cacheRead: 0.1, free: true, notes: "list" },
        },
      },
    ],
  };
  const before = editorSeed({
    providerType: "llm",
    contextWindow: 128000,
    maxOutputTokens: 4096,
    inputFormat: { text: true },
    outputFormat: { text: true },
    reasoningLevels: ["high"],
    reasoningLevelMap: { high: { reasoning_effort: "high" } },
    pricing: { currency: "USD", input: 1, output: 2, cacheRead: 0.1, free: true, notes: "list" },
  });
  const after: ModelMetadata = {
    ...before,
    providerType: undefined,
    contextWindow: undefined,
    maxOutputTokens: undefined,
    reasoningLevels: [],
    reasoningLevelMap: {},
    pricing: undefined,
  };
  const patch = metadataEditPatch(before, after);
  expect(patch.providerType).toBe(null);
  expect((patch.properties as Record<string, unknown>).contextWindow).toBe(null);
  const patchOptions = patch.options as Record<string, unknown>;
  expect(patchOptions.maxOutputTokens).toEqual({ max: null });
  expect(patchOptions.reasoningLevel).toBe(null);
  expect(patch.pricing).toBe(null);
  const next = applyModelMetadata(document, "gw", "a", patch);
  const config = (next.providerModelRules as Array<Record<string, unknown>>)[0].config as Record<string, unknown>;
  expect(config.providerType).toBe(null);
  expect((config.properties as Record<string, unknown>).contextWindow).toBe(null);
  // The default request patch is not collateral damage of clearing `max`.
  const maxOutput = (config.options as Record<string, unknown>).maxOutputTokens as Record<string, unknown>;
  expect(maxOutput.max).toBe(null);
  expect((maxOutput.map as Record<string, Record<string, unknown>>)[""].max_tokens).toBe(4096);
});

test("价格：只写改动的档位；整块清空写 null；未展示的字段不凭空写 null", () => {
  const priced = { currency: "USD", input: 1, output: 2, cacheRead: 0.1, free: true, notes: "list" };
  const before = editorSeed({ inputFormat: {}, outputFormat: {}, pricing: priced });
  // Clearing one field (the form blanked a value it showed) writes that null
  // only; the other rates stay inherited.
  expect(metadataEditPatch(before, { ...before, pricing: { ...priced, cacheRead: undefined } })).toEqual({
    pricing: { cacheRead: null },
  });
  // Clearing the whole block is one null, not a set of nulls.
  expect(metadataEditPatch(before, { ...before, pricing: undefined })).toEqual({ pricing: null });
  // A block the resolved view never showed cannot invent clears for its fields.
  expect(metadataEditPatch(editorSeed({ inputFormat: {}, outputFormat: {} }), {
    ...blankForm,
    pricing: { free: true },
  })).toEqual({ pricing: { free: true } });
});

test("空白保存是 no-op：无解析事实的模型不会被写空能力", () => {
  const seed = editorSeed(metadataOf(undefined));
  // The form always reports its booleans; they are the seed's own values, so
  // the save has nothing to write.
  expect(metadataEditPatch(seed, blankForm)).toEqual({});
  // Editing one field writes only that field (and appends one exact rule).
  const patch = metadataEditPatch(seed, { ...blankForm, contextWindow: 64000 });
  expect(patch).toEqual({ properties: { contextWindow: 64000 } });
  const next = applyModelMetadata({ schemaVersion: 1, providers: [] }, "gw", "new-model", patch);
  const rules = next.providerModelRules as Array<Record<string, unknown>>;
  expect(rules.length).toBe(1);
  const config = rules[0].config as Record<string, unknown>;
  expect((config.properties as Record<string, unknown>).contextWindow).toBe(64000);
  expect(config.pricing).toBe(undefined);
});

test("设置视图查找：ProviderViews 的模型（含不可执行/停用项）是编辑器的种子", () => {
  const disabled = model({
    providerId: "gw",
    modelId: "legacy",
    enabled: false,
    executable: true,
    selectable: false,
  });
  const providers: ProviderView[] = [
    { providerId: "gw", models: [model({ providerId: "gw", modelId: "gw-large" }), disabled] },
  ];
  expect(resolvedModelOf(providers, "gw", "gw-large")?.modelId).toBe("gw-large");
  // A disabled model is still carried by the settings view; the executable
  // registry is what drops it. Its resolved facts seed the editor, and an
  // untouched save stays a no-op.
  expect(resolvedModelOf(providers, "gw", "legacy")?.enabled).toBe(false);
  const seed = editorSeed(metadataOf(disabled));
  expect(seed.contextWindow).toBe(1000000);
  expect(metadataEditPatch(seed, seed)).toEqual({});
  expect(resolvedModelOf(providers, "gw", "missing")).toBe(undefined);
  expect(resolvedModelOf(providers, "other", "gw-large")).toBe(undefined);
  expect(resolvedModelOf(undefined, "gw", "gw-large")).toBe(undefined);
});

test("直写凭据名字：未改写的 id 稳定，改写过的 id 带摘要后缀且互不碰撞", () => {
  expect(providerCredentialSecretName("s2api")).toBe("provider_s2api");
  const dotted = providerCredentialSecretName("a.b");
  const dashed = providerCredentialSecretName("a-b");
  expect(dotted).toBe("provider_a_b_108bf50c");
  expect(dashed).toBe("provider_a_b_2a89df63");
  expect(dotted).not.toBe(dashed);
  expect(providerCredentialSecretName("a.b")).toBe(dotted);
  // An id longer than the secret-name grammar is truncated and digested.
  const long = providerCredentialSecretName("x".repeat(120));
  expect(long).toBe(`provider_${"x".repeat(46)}_c14b2ba5`);
  expect(long.length).toBeLessThanOrEqual(64);
});

test("合并补丁：对象递归、null 清除、数组替换", () => {
  const merged = mergeJsonObjects(
    { a: { b: 1, c: 2 }, list: [1, 2], keep: "x" },
    { a: { c: null, d: 3 }, list: [9] },
  );
  expect(merged).toEqual({ a: { b: 1, d: 3 }, list: [9], keep: "x" });
});

test("实时清单合并：去重、追加在后、已配置不删", () => {
  expect(mergeLiveModels(["a", "b"], ["b", "c", "a", "d"]).models).toEqual(["a", "b", "c", "d"]);
  expect(mergeLiveModels(["a", "b"], ["b", "c", "a", "d"]).added).toEqual(["c", "d"]);
  expect(mergeLiveModels([], ["z"]).models).toEqual(["z"]);
  // Blank entries and whitespace are not model ids.
  expect(mergeLiveModels(["a"], ["  ", " a "])).toEqual({ models: ["a"], added: [] });
  // Case matters: a differently-cased id is a different identity.
  expect(mergeLiveModels(["a"], ["A"]).models).toEqual(["a", "A"]);
});

test("掩码预览：18/19 边界、CJK 按码点、固定掩码不透露长度", () => {
  expect(maskedPreview("short")).toBe(FIXED_SECRET_MASK);
  expect(maskedPreview("a".repeat(18))).toBe(FIXED_SECRET_MASK);
  expect(maskedPreview("a".repeat(19))).toBe("aaaaaa…aaaaaa");
  expect(maskedPreview("密".repeat(18))).toBe(FIXED_SECRET_MASK);
  expect(maskedPreview("密".repeat(19))).toBe("密密密密密密…密密密密密密");
  expect(maskedPreview("sk-live-0123456789-abcdefghijklmnop")).toBe("sk-liv…klmnop");
  expect(secretPreviewLabel(undefined)).toBe("已保存");
  expect(secretPreviewLabel("")).toBe("已保存");
  expect(secretPreviewLabel(FIXED_SECRET_MASK)).toBe(FIXED_SECRET_MASK);
});

test("picker 第二行只补第一行没有的事实（输出上限 + 价格）", () => {
  // `modelFacts` already states the context window; the new line must not
  // repeat it, and must still say 未声明 and 免费 where those are the facts.
  expect(outputPriceSummary(model({ pricing: { currency: "USD", input: 0.27, output: 1.1 } }))).toBe(
    "输出上限 16,384 · 输入 USD 0.27 · 输出 USD 1.10 / 1M tokens",
  );
  expect(outputPriceSummary(model({ pricing: { free: true }, options: {} }))).toBe(
    "输出未声明 · 免费",
  );
  expect(outputPriceSummary(undefined)).toBe("输出未声明 · 价格未声明");
});
