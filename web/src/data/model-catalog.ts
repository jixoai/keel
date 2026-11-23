// The model catalog vocabulary of the settings surface: the pure derivations
// the provider/model editor and the picker share. Nothing here performs IO or
// holds state — the tests beside it pin each rule without rendering.
//
// Three jobs:
//  1. read/write one model's metadata as the document's `providerModelRules`
//     entry (the canonical modelcfg layer: providerId + modelId + sparse
//     config), with RFC 7386 merge-patch semantics so a save never silently
//     drops a field the form did not edit (options.reasoningLevel carries the
//     request patches and MUST survive a pricing edit);
//  2. render a price/capability summary in the reader's words ("$0.27 / 1M",
//     "免费", "价格未声明");
//  3. merge a live provider line-up into a configured model list without
//     deleting anything and preserving display order.

import type { ModelPricingView, ModelView } from "./contract";

/** One model's editable metadata, as the model editor reads it. Every field is
 * optional: `undefined` is 未声明, which the editor shows as such and a save
 * leaves alone unless the reader sets it. */
export interface ModelMetadata {
  providerType?: string;
  contextWindow?: number;
  maxOutputTokens?: number;
  inputFormat: { text?: boolean; image?: boolean; video?: boolean; audio?: boolean; pdf?: boolean };
  outputFormat: { text?: boolean; image?: boolean; video?: boolean; audio?: boolean };
  supportsToolCall?: boolean;
  supportsJsonSchemaOutput?: boolean;
  supportsNativeWebSearch?: boolean;
  pricing?: ModelPricingView;
}

/** The model families the editor offers. `decision` is a capability endpoint,
 * not a session model, but it is a real row of the vocabulary (the resolver
 * accepts it); the editor labels it as such. */
export const MODEL_FAMILIES: Array<{ value: string; title: string }> = [
  { value: "llm", title: "文本（llm）" },
  { value: "realtime", title: "实时语音（realtime）" },
  { value: "image", title: "图像（image）" },
  { value: "video", title: "视频（video）" },
  { value: "tts", title: "语音合成（tts）" },
  { value: "stt", title: "语音识别（stt）" },
  { value: "decision", title: "决策端点（decision）" },
  { value: "generic", title: "通用（generic）" },
];

/** The input modalities (the modelcfg `inputFormat` vocabulary). */
export const INPUT_MODALITIES: Array<{ key: "text" | "image" | "video" | "audio" | "pdf"; label: string }> = [
  { key: "text", label: "文本" },
  { key: "image", label: "图片" },
  { key: "video", label: "视频" },
  { key: "audio", label: "音频" },
  { key: "pdf", label: "PDF" },
];

/** The output modalities (the modelcfg `outputFormat` vocabulary: no pdf). */
export const OUTPUT_MODALITIES: Array<{ key: "text" | "image" | "video" | "audio"; label: string }> = [
  { key: "text", label: "文本" },
  { key: "image", label: "图片" },
  { key: "video", label: "视频" },
  { key: "audio", label: "音频" },
];

/** The currencies the editor offers; a document may carry another ISO code and
 * keeps it (the picker shows the code verbatim). */
export const PRICING_CURRENCIES = ["USD", "CNY", "EUR"];

const asRecord = (value: unknown): Record<string, unknown> =>
  value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};

const numberOf = (value: unknown): number | undefined => {
  if (value === null || value === undefined) return undefined;
  const parsed = typeof value === "string" && value.trim() !== "" ? Number(value) : value;
  if (typeof parsed !== "number" || !Number.isFinite(parsed) || parsed < 0) return undefined;
  return parsed;
};

const textOf = (value: unknown): string | undefined => {
  if (typeof value !== "string" || value.trim() === "") return undefined;
  return value.trim();
};

const boolOf = (value: unknown): boolean | undefined =>
  value === null || value === undefined ? undefined : value === true;

/** A resolved model's declared metadata; the document's rule patch is merged
 * on top so an unsaved edit is visible while the dialog is open. */
export function metadataOf(model: ModelView | undefined): ModelMetadata {
  const properties = asRecord(model?.properties);
  const input = asRecord(properties.inputFormat);
  const output = asRecord(properties.outputFormat);
  const options = asRecord(model?.options);
  const maxOutput = asRecord(options.maxOutputTokens);
  return {
    providerType: textOf(model?.providerType),
    contextWindow: numberOf(properties.contextWindow),
    maxOutputTokens: numberOf(maxOutput.max),
    inputFormat: {
      text: boolOf(input.text),
      image: boolOf(input.image),
      video: boolOf(input.video),
      audio: boolOf(input.audio),
      pdf: boolOf(input.pdf),
    },
    outputFormat: {
      text: boolOf(output.text),
      image: boolOf(output.image),
      video: boolOf(output.video),
      audio: boolOf(output.audio),
    },
    supportsToolCall: boolOf(properties.supportsToolCall),
    supportsJsonSchemaOutput: boolOf(properties.supportsJsonSchemaOutput),
    supportsNativeWebSearch: boolOf(properties.supportsNativeWebSearch),
    pricing: model?.pricing ? { ...model.pricing } : undefined,
  };
}

/** RFC 7386 JSON Merge Patch over decoded JSON values: objects merge, null
 * clears, arrays and scalars replace. The document's config layer is a merge
 * patch, so the editor's save uses the same semantics the resolver does. */
export function mergeJsonObjects(target: Record<string, unknown>, patch: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = { ...target };
  for (const [key, value] of Object.entries(patch)) {
    if (value === null || value === undefined) {
      delete out[key];
      continue;
    }
    const valueRecord = value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : null;
    if (valueRecord) {
      const base = out[key] && typeof out[key] === "object" && !Array.isArray(out[key])
        ? (out[key] as Record<string, unknown>)
        : {};
      out[key] = mergeJsonObjects(base, valueRecord);
      continue;
    }
    out[key] = value;
  }
  return out;
}

/** The provider-model rule a document declares for (providerId, modelId), or
 * null. The rule is the model's exact (non-baseline) declaration. */
export function findProviderModelRule(
  document: Record<string, unknown> | null,
  providerId: string,
  modelId: string,
): Record<string, unknown> | null {
  if (!document) return null;
  const rules = Array.isArray(document.providerModelRules) ? document.providerModelRules : [];
  for (const raw of rules) {
    const rule = asRecord(raw);
    if (rule.providerId === providerId && rule.modelId === modelId) return rule;
  }
  return null;
}

/** The sparse config a model editor save produces. Every section the form
 * edited is present; a cleared section is `null` (which clears under merge
 * patch); a section the form left untouched is absent. */
export function metadataPatch(metadata: ModelMetadata, cleared: { pricing?: boolean } = {}): Record<string, unknown> {
  const config: Record<string, unknown> = {};
  if (metadata.providerType) config.providerType = metadata.providerType;
  const properties: Record<string, unknown> = {};
  if (metadata.contextWindow !== undefined) properties.contextWindow = metadata.contextWindow;
  properties.inputFormat = { ...metadata.inputFormat };
  properties.outputFormat = { ...metadata.outputFormat };
  if (metadata.supportsToolCall !== undefined) properties.supportsToolCall = metadata.supportsToolCall;
  if (metadata.supportsJsonSchemaOutput !== undefined) {
    properties.supportsJsonSchemaOutput = metadata.supportsJsonSchemaOutput;
  }
  if (metadata.supportsNativeWebSearch !== undefined) {
    properties.supportsNativeWebSearch = metadata.supportsNativeWebSearch;
  }
  config.properties = properties;
  if (metadata.maxOutputTokens !== undefined) {
    config.options = { maxOutputTokens: { max: metadata.maxOutputTokens } };
  }
  if (cleared.pricing) config.pricing = null;
  else if (metadata.pricing) config.pricing = pricingPatch(metadata.pricing);
  return config;
}

/** The pricing part of a patch: only declared fields, `undefined` omitted.
 * A block with nothing declared is `null` (clears the block). */
function pricingPatch(pricing: ModelPricingView): Record<string, unknown> | null {
  const block: Record<string, unknown> = {};
  if (pricing.currency) block.currency = pricing.currency;
  if (pricing.input !== undefined) block.input = pricing.input;
  if (pricing.output !== undefined) block.output = pricing.output;
  if (pricing.cacheRead !== undefined) block.cacheRead = pricing.cacheRead;
  if (pricing.cacheWrite !== undefined) block.cacheWrite = pricing.cacheWrite;
  if (pricing.free) block.free = true;
  if (pricing.notes && pricing.notes.trim()) block.notes = pricing.notes.trim();
  return Object.keys(block).length > 0 ? block : null;
}

/** Apply an editor save to the document: merge the patch into the existing
 * provider-model rule's config (or append a new rule). Returns the document
 * (same object identity is not promised; the caller replaces it). */
export function applyModelMetadata(
  document: Record<string, unknown>,
  providerId: string,
  modelId: string,
  patch: Record<string, unknown>,
): Record<string, unknown> {
  const rules = Array.isArray(document.providerModelRules) ? [...document.providerModelRules] : [];
  const index = rules.findIndex((raw) => {
    const rule = asRecord(raw);
    return rule.providerId === providerId && rule.modelId === modelId;
  });
  if (index >= 0) {
    const rule = { ...asRecord(rules[index]) };
    rule.config = mergeJsonObjects(asRecord(rule.config), patch);
    rules[index] = rule;
  } else {
    rules.push({ providerId, modelId, config: patch });
  }
  document.providerModelRules = rules;
  return document;
}

/** `$0.27 / 1M` (currency as the declared code; no code → the bare number).
 * Sub-cent prices keep the precision that makes them legible (0.07, 0.00125). */
export function formatPrice(value: number, currency?: string): string {
  const digits = value === 0 ? 0 : value < 1 ? Math.min(4, Math.max(2, decimalsOf(value))) : 2;
  const number = value.toFixed(digits);
    return currency ? `${currency} ${number}` : number;
}

const decimalsOf = (value: number): number => {
  const text = String(value);
  const dot = text.indexOf(".");
  if (dot < 0) return 2;
  // At least two decimals; a value like 0.00125 keeps all five it declares.
  return Math.max(2, Math.min(6, text.length - dot - 1));
};

/** The one-line price summary the picker row and the default-model dialog
 * show: 免费 / 价格未声明 / "输入 $0.27 · 输出 $1.10 （每 1M tokens）". */
export function priceSummary(pricing: ModelPricingView | undefined): string {
  if (!pricing) return "价格未声明";
  if (pricing.free) return "免费";
  const input = pricing.input;
  const output = pricing.output;
  if (input === undefined && output === undefined) {
    return pricing.notes?.trim() ? `价格未声明（${pricing.notes.trim()}）` : "价格未声明";
  }
  const parts: string[] = [];
  if (input !== undefined) parts.push(`输入 ${formatPrice(input, pricing.currency)}`);
  if (output !== undefined) parts.push(`输出 ${formatPrice(output, pricing.currency)}`);
  return `${parts.join(" · ")} / 1M tokens`;
}

/** A compact context/max-output line: `128K 上下文 · 8K 输出`, with 未声明 for
 * an absent value. */
export function windowSummary(model: ModelView | undefined): string {
  const metadata = metadataOf(model);
  const window = metadata.contextWindow;
  const maxOutput = metadata.maxOutputTokens;
  const windowText = window === undefined ? "上下文未声明" : `${formatTokens(window)} 上下文`;
  const outputText = maxOutput === undefined ? "输出未声明" : `${formatTokens(maxOutput)} 输出`;
  return `${windowText} · ${outputText}`;
}

/** `128K` / `1M` for an exact thousand/million multiple; the raw count (with
 * thousands separators) otherwise — 65536 stays legible as 65,536 rather than
 * a rounded "65.5K" that misstates the declared limit. */
export function formatTokens(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return String(value);
  if (value >= 1_000_000 && value % 1_000_000 === 0) return `${value / 1_000_000}M`;
  if (value >= 1_000 && value % 1_000 === 0) return `${value / 1_000}K`;
  return value.toLocaleString("en-US");
}

/** Whether a model's declared price is free-or-zero (the badge's fact). */
export function isFree(pricing: ModelPricingView | undefined): boolean {
  if (!pricing) return false;
  if (pricing.free) return true;
  return pricing.input === 0 && pricing.output === 0;
}

/** Merge a live line-up into a configured list. Live ids are added at the end
 * in their own order (deduped, case-sensitively — the id is the identity), and
 * nothing configured is removed: a catalog change must never silently delete a
 * model someone set up. Returns the merged list plus what was added. */
export function mergeLiveModels(
  configured: string[],
  live: string[],
): { models: string[]; added: string[] } {
  const seen = new Set(configured);
  const added: string[] = [];
  for (const raw of live) {
    const id = raw.trim();
    if (!id || seen.has(id)) continue;
    seen.add(id);
    added.push(id);
  }
  return { models: [...configured, ...added], added };
}

/** The masked preview rule, mirrored client-side for the editor's optimistic
 * display before the server answers (the server is the authority and its
 * answer replaces it). >18 runes keeps first/last six; otherwise the mask. */
export const FIXED_SECRET_MASK = "••••••••";

export function maskedPreview(value: string): string {
  const runes = [...value];
  if (runes.length <= 18) return FIXED_SECRET_MASK;
  return `${runes.slice(0, 6).join("")}…${runes.slice(-6).join("")}`;
}

/** The preview a stored secret row shows: the server's masked form, or
 * 「已保存」 for a row that predates the preview column. Never a value. */
export function secretPreviewLabel(preview: string | undefined): string {
  return preview && preview.trim() ? preview : "已保存";
}
