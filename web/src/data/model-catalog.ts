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

import type { ModelPricingView, ModelView, ProviderView } from "./contract";

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
  /** The thinking levels this model accepts, ascending. An empty/absent list
   * is 未声明; the editor offers the runtime's closed vocabulary. */
  reasoningLevels?: string[];
  /** The request patch per level. The editor edits the visible levels, not
   * the wire patches, so a save must carry the maps through untouched. */
  reasoningLevelMap?: Record<string, Record<string, unknown>>;
  pricing?: ModelPricingView;
}

/** The runtime's unified thinking vocabulary, ascending (modelcfg's set). */
export const REASONING_LEVELS = ["off", "minimal", "low", "medium", "high", "xhigh", "max"] as const;

const reasoningLevelSet = new Set<string>(REASONING_LEVELS);

/** Keep only runtime levels, deduplicated and in ascending order. */
export function normalizeReasoningLevels(values: readonly string[] | undefined): string[] {
  const wanted = new Set((values ?? []).filter((value) => reasoningLevelSet.has(value)));
  return REASONING_LEVELS.filter((level) => wanted.has(level));
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

/** A token limit is declared only when it is a positive count: 0 is the
 * resolver's "incomplete" value, never a fact the UI should print as "0". */
const tokenCountOf = (value: unknown): number | undefined => {
  const parsed = numberOf(value);
  return parsed !== undefined && parsed > 0 ? parsed : undefined;
};

/** A resolved model's declared metadata; the document's rule patch is merged
 * on top so an unsaved edit is visible while the dialog is open. */
export function metadataOf(model: ModelView | undefined): ModelMetadata {
  const properties = asRecord(model?.properties);
  const input = asRecord(properties.inputFormat);
  const output = asRecord(properties.outputFormat);
  const options = asRecord(model?.options);
  const maxOutput = asRecord(options.maxOutputTokens);
  const reasoning = asRecord(options.reasoningLevel);
  const rawLevels = Array.isArray(reasoning.values)
    ? reasoning.values.filter((value): value is string => typeof value === "string")
    : [];
  const reasoningLevels = normalizeReasoningLevels(rawLevels);
  const patches = asRecord(reasoning.map);
  const reasoningLevelMap: Record<string, Record<string, unknown>> = {};
  for (const level of reasoningLevels) {
    reasoningLevelMap[level] = asRecord(patches[level]);
  }
  return {
    providerType: textOf(model?.providerType),
    contextWindow: tokenCountOf(properties.contextWindow),
    maxOutputTokens: tokenCountOf(maxOutput.max),
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
    reasoningLevels,
    reasoningLevelMap,
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

/** Compose two merge patches stored at the same layer. The second patch must
 * keep its `null`s: a stored rule's null is the instruction that clears a lower
 * layer's value, so applying RFC 7386 to the rule itself (which would delete
 * the key) would silently re-inherit the value the reader cleared. */
function composeModelPatch(target: unknown, patch: unknown): unknown {
  if (patch === null || patch === undefined) return patch ?? null;
  const patchRecord =
    typeof patch === "object" && !Array.isArray(patch) ? (patch as Record<string, unknown>) : null;
  if (!patchRecord) return patch;
  const base =
    target && typeof target === "object" && !Array.isArray(target)
      ? { ...(target as Record<string, unknown>) }
      : {};
  for (const [key, value] of Object.entries(patchRecord)) {
    if (value === undefined) continue;
    base[key] = composeModelPatch(base[key], value);
  }
  return base;
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

/** The resolved settings view's model (`GET /v1/providers`). ProviderViews
 * keeps every member model — including one `/v1/models` omits because the
 * provider's credential does not resolve here or the model is disabled — so
 * the editor seeds from the facts the server actually resolved, not from the
 * executable registry. An absent model is a real state (hidden provider, a
 * document the runtime did not resolve); the caller must show it as such. */
export function resolvedModelOf(
  providers: readonly ProviderView[] | undefined,
  providerId: string,
  modelId: string,
): ModelView | undefined {
  const provider = (providers ?? []).find((row) => row.providerId === providerId);
  return provider?.models?.find((row) => row.modelId === modelId);
}

/** The editor's canonical seed: booleans become real booleans and an empty
 * reasoning list stays empty, so the form's own values diff against the seed
 * without treating 未声明 as a change. A pricing block that declares no fact
 * (`{}`, `free: false`) is not a fact either. */
export function editorSeed(metadata: ModelMetadata): ModelMetadata {
  const reasoningLevels = normalizeReasoningLevels(metadata.reasoningLevels);
  const reasoningLevelMap: Record<string, Record<string, unknown>> = {};
  for (const level of reasoningLevels) {
    reasoningLevelMap[level] = { ...asRecord(metadata.reasoningLevelMap?.[level]) };
  }
  return {
    providerType: metadata.providerType,
    contextWindow: metadata.contextWindow,
    maxOutputTokens: metadata.maxOutputTokens,
    inputFormat: {
      text: metadata.inputFormat.text === true,
      image: metadata.inputFormat.image === true,
      video: metadata.inputFormat.video === true,
      audio: metadata.inputFormat.audio === true,
      pdf: metadata.inputFormat.pdf === true,
    },
    outputFormat: {
      text: metadata.outputFormat.text === true,
      image: metadata.outputFormat.image === true,
      video: metadata.outputFormat.video === true,
      audio: metadata.outputFormat.audio === true,
    },
    supportsToolCall: metadata.supportsToolCall === true,
    supportsJsonSchemaOutput: metadata.supportsJsonSchemaOutput === true,
    supportsNativeWebSearch: metadata.supportsNativeWebSearch === true,
    reasoningLevels,
    reasoningLevelMap,
    pricing: pricingFactsOf(metadata.pricing),
  };
}

/** The declared pricing facts of a block, or undefined when it declares none. */
function pricingFactsOf(pricing: ModelPricingView | undefined): ModelPricingView | undefined {
  if (!pricing) return undefined;
  const facts: ModelPricingView = {
    currency: pricing.currency?.trim() || undefined,
    input: pricing.input,
    output: pricing.output,
    cacheRead: pricing.cacheRead,
    cacheWrite: pricing.cacheWrite,
    free: pricing.free === true ? true : undefined,
    notes: pricing.notes?.trim() || undefined,
  };
  return Object.values(facts).some((value) => value !== undefined) ? facts : undefined;
}

const INPUT_MODALITY_KEYS = ["text", "image", "video", "audio", "pdf"] as const;
const OUTPUT_MODALITY_KEYS = ["text", "image", "video", "audio"] as const;

/** The modality keys the reader toggled, as a per-key merge patch: an
 * untouched key is absent (the resolved fact stays), a cleared one is `false`
 * and a newly accepted one is `true`. */
function mediaEditPatch(
  before: Record<string, boolean | undefined>,
  after: Record<string, boolean | undefined>,
  keys: readonly string[],
): Record<string, unknown> {
  const patch: Record<string, unknown> = {};
  for (const key of keys) {
    const was = before[key] === true;
    const is = after[key] === true;
    if (was !== is) patch[key] = is;
  }
  return patch;
}

/** Pricing fields the reader changed. A block that had no declared fact stays
 * untouched; a field the form showed and the reader blanked is `null`; a field
 * the form never showed is never invented as a clear. */
function pricingEditPatch(
  before: ModelPricingView | undefined,
  after: ModelPricingView | undefined,
): Record<string, unknown> | null | undefined {
  if (!before) {
    if (!after) return undefined;
    const declared: Record<string, unknown> = {};
    if (after.currency?.trim()) declared.currency = after.currency.trim();
    if (after.input !== undefined) declared.input = after.input;
    if (after.output !== undefined) declared.output = after.output;
    if (after.cacheRead !== undefined) declared.cacheRead = after.cacheRead;
    if (after.cacheWrite !== undefined) declared.cacheWrite = after.cacheWrite;
    if (after.free === true) declared.free = true;
    if (after.notes?.trim()) declared.notes = after.notes.trim();
    return Object.keys(declared).length > 0 ? declared : undefined;
  }
  if (!after) return null;
  const patch: Record<string, unknown> = {};
  if ((before.currency ?? "").trim() !== (after.currency ?? "").trim()) {
    patch.currency = after.currency?.trim() || null;
  }
  if (before.input !== after.input) patch.input = after.input ?? null;
  if (before.output !== after.output) patch.output = after.output ?? null;
  if (before.cacheRead !== after.cacheRead) patch.cacheRead = after.cacheRead ?? null;
  if (before.cacheWrite !== after.cacheWrite) patch.cacheWrite = after.cacheWrite ?? null;
  if ((before.free === true) !== (after.free === true)) patch.free = after.free === true;
  if ((before.notes ?? "").trim() !== (after.notes ?? "").trim()) {
    patch.notes = after.notes?.trim() || null;
  }
  return Object.keys(patch).length > 0 ? patch : undefined;
}

/** The sparse config one model editor save produces: only the sections (and,
 * inside a section, only the keys) whose value differs from the seed the form
 * showed. An untouched save is `{}` — the caller must then write nothing at
 * all. This is what keeps a blank dialog from overwriting capabilities the
 * resolved view never displayed (an unauthenticated provider's model would
 * otherwise lose every modality and its reasoning levels, and become
 * unusable). */
export function metadataEditPatch(before: ModelMetadata, after: ModelMetadata): Record<string, unknown> {
  const config: Record<string, unknown> = {};
  if (after.providerType !== before.providerType) config.providerType = after.providerType ?? null;

  const properties: Record<string, unknown> = {};
  if (after.contextWindow !== before.contextWindow) properties.contextWindow = after.contextWindow ?? null;
  const input = mediaEditPatch(before.inputFormat, after.inputFormat, INPUT_MODALITY_KEYS);
  if (Object.keys(input).length > 0) properties.inputFormat = input;
  const output = mediaEditPatch(before.outputFormat, after.outputFormat, OUTPUT_MODALITY_KEYS);
  if (Object.keys(output).length > 0) properties.outputFormat = output;
  if (after.supportsToolCall !== before.supportsToolCall) {
    properties.supportsToolCall = after.supportsToolCall === true;
  }
  if (after.supportsJsonSchemaOutput !== before.supportsJsonSchemaOutput) {
    properties.supportsJsonSchemaOutput = after.supportsJsonSchemaOutput === true;
  }
  if (after.supportsNativeWebSearch !== before.supportsNativeWebSearch) {
    properties.supportsNativeWebSearch = after.supportsNativeWebSearch === true;
  }
  if (Object.keys(properties).length > 0) config.properties = properties;

  const options: Record<string, unknown> = {};
  if (after.maxOutputTokens !== before.maxOutputTokens) {
    options.maxOutputTokens = { max: after.maxOutputTokens ?? null };
  }
  const beforeLevels = normalizeReasoningLevels(before.reasoningLevels);
  const afterLevels = normalizeReasoningLevels(after.reasoningLevels);
  if (beforeLevels.join(",") !== afterLevels.join(",")) {
    if (afterLevels.length === 0) {
      options.reasoningLevel = null;
    } else {
      // Reuse the patches the form carried (it edits levels, not wire
      // patches); a newly added level gets the empty patch the resolver
      // requires, and an untouched level keeps its patch.
      const map: Record<string, Record<string, unknown>> = {};
      for (const level of afterLevels) {
        map[level] = {
          ...asRecord(after.reasoningLevelMap?.[level] ?? before.reasoningLevelMap?.[level]),
        };
      }
      options.reasoningLevel = { values: afterLevels, map };
    }
  }
  if (Object.keys(options).length > 0) config.options = options;

  const pricing = pricingEditPatch(before.pricing, after.pricing);
  if (pricing !== undefined) config.pricing = pricing;
  return config;
}

/** Apply an editor save to the document: compose the patch into the existing
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
    rule.config = composeModelPatch(asRecord(rule.config), patch) as Record<string, unknown>;
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

/** The picker row's second line: the max-output fact `modelFacts` does not
 * carry, plus the price summary. The context window is already on the row's
 * first line, so it is deliberately not repeated here. */
export function outputPriceSummary(model: ModelView | undefined): string {
  const metadata = metadataOf(model);
  const output =
    metadata.maxOutputTokens === undefined ? "输出未声明" : `输出上限 ${formatTokens(metadata.maxOutputTokens)}`;
  return `${output} · ${priceSummary(model?.pricing)}`;
}

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

/** The secret-name grammar's maximum length (the server's `secretNamePattern`). */
const SECRET_NAME_MAX = 64;

/** A stable short digest of the original provider id. The direct-credential
 * route and this mirror both derive names from it; ASCII ids (the route's
 * grammar) hash identically on both sides. It exists so two ids that sanitize
 * to the same fragment (`a.b` and `a-b` → `a_b`) can never share one secret
 * row; it is not a security boundary. */
const providerIDDigest = (value: string): string => {
  let hash = 0x811c9dc5;
  for (let index = 0; index < value.length; index += 1) {
    hash ^= value.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash.toString(16).padStart(8, "0");
};

/** The tenant-secret name the direct-credential route derives for a provider
 * (`provider_<id>`), mirrored client-side for the one fallback reference the
 * editor mints when it keeps a stored credential. An id the secret grammar
 * refuses is sanitized and gets a short digest suffix, so sanitization cannot
 * collide two providers onto one row; an over-long id is truncated to the
 * grammar's bound with the same suffix. The server's answer is the authority. */
export function providerCredentialSecretName(providerId: string): string {
  const prefix = "provider_";
  const sanitized = providerId.replace(/[^A-Za-z0-9_]/g, "_");
  if (sanitized === providerId && prefix.length + sanitized.length <= SECRET_NAME_MAX) {
    return `${prefix}${sanitized}`;
  }
  const suffix = `_${providerIDDigest(providerId)}`;
  const room = SECRET_NAME_MAX - prefix.length - suffix.length;
  return `${prefix}${sanitized.slice(0, Math.max(0, room))}${suffix}`;
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
