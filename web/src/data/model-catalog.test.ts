// The model-catalog vocabulary's derivations (change keel-model-catalog C/A):
// the price/capability summaries the picker and editor render, the merge-patch
// save path (one model, no collateral edits), the live-line-up merge (nothing
// removed, order preserved), and the masked preview mirror. Plain unit tests:
// no rendering, no store.
import { expect, test } from "nsh/test";
import type { ModelView } from "./contract";
import {
  FIXED_SECRET_MASK,
  REASONING_LEVELS,
  applyModelMetadata,
  formatPrice,
  formatTokens,
  isFree,
  maskedPreview,
  mergeJsonObjects,
  mergeLiveModels,
  metadataOf,
  metadataPatch,
  normalizeReasoningLevels,
  outputPriceSummary,
  priceSummary,
  secretPreviewLabel,
  windowSummary,
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

test("保存只写这一个模型：精确 providerModelRules 的一条，不碰其它模型", () => {
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
  const patch = metadataPatch({
    contextWindow: 200000,
    inputFormat: { text: true },
    outputFormat: { text: true, image: true },
    maxOutputTokens: 4096,
    pricing: { currency: "USD", input: 1, output: 2 },
  });
  const next = applyModelMetadata(document, "gw", "a", patch);
  const rules = next.providerModelRules as Array<Record<string, unknown>>;
  const edited = rules.find((rule) => rule.modelId === "a") as Record<string, unknown>;
  const config = edited.config as Record<string, unknown>;
  const properties = config.properties as Record<string, unknown>;
  expect(properties.contextWindow).toBe(200000);
  const options = config.options as Record<string, unknown>;
  const maxOutput = options.maxOutputTokens as Record<string, unknown>;
  expect(maxOutput.max).toBe(4096);
  // The reasoning map (a field the dialog did not edit) survives the merge.
  const reasoning = options.reasoningLevel as Record<string, unknown>;
  expect(Array.isArray(reasoning.values)).toBe(true);
  // One model's edit does not add a rule for the sibling model.
  expect(rules.filter((rule) => rule.modelId === "b").length).toBe(0);
  expect(next.providers).toEqual(document.providers);
});

test("思考档：按运行时词汇升序保存，请求补丁保留并补齐", () => {
  const document: Record<string, unknown> = {
    schemaVersion: 1,
    providerModelRules: [
      {
        providerId: "gw",
        modelId: "a",
        config: {
          options: { reasoningLevel: { values: ["high"], map: { high: { reasoning_effort: "high" } } } },
        },
      },
    ],
  };
  const patch = metadataPatch({
    inputFormat: {},
    outputFormat: {},
    // Deliberately unsorted; normalizeReasoningLevels is the authority.
    reasoningLevels: ["high", "minimal"],
    reasoningLevelMap: { high: { reasoning_effort: "high" } },
  });
  const next = applyModelMetadata(document, "gw", "a", patch);
  const config = (next.providerModelRules as Array<Record<string, unknown>>)[0].config as Record<string, unknown>;
  const options = config.options as Record<string, unknown>;
  const reasoning = options.reasoningLevel as Record<string, unknown>;
  expect(reasoning.values).toEqual(["minimal", "high"]);
  expect(reasoning.map).toEqual({ minimal: {}, high: { reasoning_effort: "high" } });
  expect(normalizeReasoningLevels(["max", "high", "bogus", "high"])).toEqual(["high", "max"]);
  expect(REASONING_LEVELS.join(",")).toBe("off,minimal,low,medium,high,xhigh,max");
});

test("清除：空字段写成 null；未编辑的请求补丁与其它价格字段仍在", () => {
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
  const patch = metadataPatch(
    {
      inputFormat: { text: true },
      outputFormat: { text: true },
      reasoningLevels: [],
      pricing: { input: undefined, output: 2 },
    },
    { providerType: true, contextWindow: true, maxOutputTokens: true, reasoningLevels: true },
  );
  const next = applyModelMetadata(document, "gw", "a", patch);
  const config = (next.providerModelRules as Array<Record<string, unknown>>)[0].config as Record<string, unknown>;
  expect(config.providerType).toBe(null);
  expect((config.properties as Record<string, unknown>).contextWindow).toBe(null);
  const options = config.options as Record<string, unknown>;
  const maxOutput = options.maxOutputTokens as Record<string, unknown>;
  expect(maxOutput.max).toBe(null);
  // The default request patch is not collateral damage of clearing `max`.
  expect((maxOutput.map as Record<string, Record<string, unknown>>)[""].max_tokens).toBe(4096);
  expect(options.reasoningLevel).toBe(null);
  expect(config.pricing).toEqual({
    currency: null,
    input: null,
    output: 2,
    cacheRead: null,
    cacheWrite: null,
    free: false,
    notes: null,
  });
});

test("清除价格：已有规则里存 null，不把下层的价格放回来", () => {
  const document: Record<string, unknown> = {
    schemaVersion: 1,
    providerModelRules: [
      { providerId: "gw", modelId: "a", config: { pricing: { currency: "USD", input: 1 } } },
    ],
  };
  const next = applyModelMetadata(
    document,
    "gw",
    "a",
    metadataPatch({ inputFormat: {}, outputFormat: {} }, { pricing: true }),
  );
  const config = (next.providerModelRules as Array<Record<string, unknown>>)[0].config as Record<string, unknown>;
  expect(config.pricing).toBe(null);
});

test("保存追加未知模型的一条规则；清除价格用 null 表达", () => {
  const document: Record<string, unknown> = { schemaVersion: 1, providers: [] };
  const next = applyModelMetadata(
    document,
    "gw",
    "new-model",
    metadataPatch({ contextWindow: 64000, inputFormat: { text: true }, outputFormat: { text: true } }, { pricing: true }),
  );
  const rules = next.providerModelRules as Array<Record<string, unknown>>;
  expect(rules.length).toBe(1);
  const config = rules[0].config as Record<string, unknown>;
  expect(config.pricing).toBe(null);
  expect((config.properties as Record<string, unknown>).contextWindow).toBe(64000);
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
