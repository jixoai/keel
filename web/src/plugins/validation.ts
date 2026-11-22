// Strict validation for slot declarations and contributions (W12).
//
// Required fields and enums fail closed; an unknown optional key is a warning
// (so a newer manifest can add fields without breaking an older client), but a
// typo of a required name still fails because the required field is missing.
// Every finding names the field the author wrote.
import type {
  BridgeCapability,
  SlotArity,
  SlotContribution,
  SlotDeclaration,
  SlotForm,
  SlotIssue,
  SlotTarget,
} from "./types";
import { BRIDGE_CAPABILITIES, SLOT_ARITIES, SLOT_FORMS, SLOT_TARGETS } from "./types";

type Raw = Record<string, unknown>;

const isRecord = (value: unknown): value is Raw =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const issue = (
  severity: "error" | "warning",
  code: string,
  pluginId: string,
  field: string,
  message: string,
): SlotIssue => ({ severity, code, pluginId, field, message });

/** Warn about keys the validation does not read (a typo, or a newer field). */
function checkKeys(
  raw: Raw,
  allowed: readonly string[],
  pluginId: string,
  prefix: string,
  issues: SlotIssue[],
): void {
  for (const key of Object.keys(raw)) {
    if (allowed.includes(key)) continue;
    issues.push(
      issue(
        "warning",
        "ERR_SLOT_INVALID",
        pluginId,
        `${prefix}${key}`,
        `字段「${key}」不在本客户端的槽位契约里，已忽略（可能是拼写错误或更新的字段）。`,
      ),
    );
  }
}

const SLOT_ID_PATTERN = /^[a-z][a-z0-9._-]{1,63}\.[a-z][a-z0-9._-]{0,127}$/;
const CONTRIBUTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;

/** The slot's namespace rule: an id is owned by the plugin that declares it. */
export function slotIDOwnedBy(id: string, provider: string): boolean {
  return id.startsWith(`${provider}.`) && SLOT_ID_PATTERN.test(id);
}

function stringArray(value: unknown, field: string, pluginId: string, issues: SlotIssue[]): string[] {
  if (value === undefined) return [];
  if (!Array.isArray(value)) {
    issues.push(issue("error", "ERR_SLOT_INVALID", pluginId, field, `${field} 必须是字符串数组。`));
    return [];
  }
  const out: string[] = [];
  for (const entry of value) {
    if (typeof entry !== "string" || entry.trim() === "") {
      issues.push(issue("error", "ERR_SLOT_INVALID", pluginId, field, `${field} 的每一项都必须是非空字符串。`));
      continue;
    }
    out.push(entry.trim());
  }
  return out;
}

function slotTargets(value: unknown, pluginId: string, field: string, issues: SlotIssue[]): SlotTarget[] {
  const names = stringArray(value, field, pluginId, issues);
  const out: SlotTarget[] = [];
  for (const name of names) {
    if (!(SLOT_TARGETS as readonly string[]).includes(name)) {
      issues.push(
        issue("error", "ERR_SLOT_INVALID", pluginId, field, `${field} 含未知 target「${name}」；只接受 web/native/tui。`),
      );
      continue;
    }
    if (!out.includes(name as SlotTarget)) out.push(name as SlotTarget);
  }
  return out;
}

function slotForms(value: unknown, pluginId: string, field: string, issues: SlotIssue[]): SlotForm[] {
  const names = stringArray(value, field, pluginId, issues);
  const out: SlotForm[] = [];
  for (const name of names) {
    if (!(SLOT_FORMS as readonly string[]).includes(name)) {
      issues.push(
        issue("error", "ERR_SLOT_INVALID", pluginId, field, `${field} 含未知形态「${name}」；只接受 chunk/package/iframe/data。`),
      );
      continue;
    }
    if (!out.includes(name as SlotForm)) out.push(name as SlotForm);
  }
  if (out.length === 0 && names.length === 0) {
    issues.push(issue("error", "ERR_SLOT_INVALID", pluginId, field, `${field} 必须声明至少一种贡献形态。`));
  }
  return out;
}

function normalArity(value: unknown, pluginId: string, field: string, issues: SlotIssue[]): SlotArity {
  if (value === undefined) return "multi";
  const raw = typeof value === "string" ? value.trim() : "";
  const alias: Record<string, SlotArity> = {
    single: "single",
    one: "single",
    singleton: "single",
    multi: "multi",
    many: "multi",
    ordered: "ordered",
  };
  const arity = alias[raw];
  if (!arity) {
    issues.push(
      issue("error", "ERR_SLOT_INVALID", pluginId, field, `${field} 必须是 single/multi/ordered（收到「${raw}」）。`),
    );
    return "multi";
  }
  return arity;
}

/** Normalize one `provides.slots[]` declaration. */
export function normalizeSlot(
  raw: unknown,
  provider: string,
  builtin = false,
  index = 0,
): { slot: SlotDeclaration | null; issues: SlotIssue[] } {
  const issues: SlotIssue[] = [];
  const field = `provides.slots[${index}]`;
  if (!isRecord(raw)) {
    issues.push(issue("error", "ERR_SLOT_INVALID", provider, field, "槽位声明必须是对象。"));
    return { slot: null, issues };
  }
  checkKeys(raw, ["id", "forms", "arity", "targets", "schema", "lifecycle", "description", "provider", "builtin"], provider, `${field}.`, issues);
  const id = typeof raw.id === "string" ? raw.id.trim() : "";
  if (!id) {
    issues.push(issue("error", "ERR_SLOT_INVALID", provider, `${field}.id`, "槽位必须有 id。"));
  } else if (!slotIDOwnedBy(id, provider)) {
    issues.push(
      issue(
        "error",
        "ERR_SLOT_INVALID",
        provider,
        `${field}.id`,
        `槽位 id「${id}」必须以声明者命名空间「${provider}.」开头（如 ${provider}.route）。`,
      ),
    );
  }
  const forms = slotForms(raw.forms, provider, `${field}.forms`, issues);
  const targets = slotTargets(raw.targets, provider, `${field}.targets`, issues);
  if (targets.length === 0) {
    issues.push(issue("error", "ERR_SLOT_INVALID", provider, `${field}.targets`, "targets 必须声明至少一个 target。"));
  }
  const arity = normalArity(raw.arity, provider, `${field}.arity`, issues);
  if (raw.schema !== undefined && !isRecord(raw.schema)) {
    issues.push(issue("error", "ERR_SLOT_INVALID", provider, `${field}.schema`, "schema 必须是 JSON 对象。"));
  }
  const lifecycle =
    raw.lifecycle === undefined
      ? undefined
      : stringArray(raw.lifecycle, `${field}.lifecycle`, provider, issues);
  if (issues.some((entry) => entry.severity === "error")) return { slot: null, issues };
  return {
    slot: {
      id,
      provider,
      forms,
      arity,
      targets,
      schema: isRecord(raw.schema) ? raw.schema : undefined,
      lifecycle,
      builtin,
    },
    issues,
  };
}

/** Is `entry` safe for the form? It must not escape the package or the page. */
function entryProblem(form: SlotForm, entry: string): string | null {
  const value = entry.trim();
  if (!value) return "entry 不能为空。";
  if (value.includes("\\") || value.includes("..") || /[\u0000-\u001f]/.test(value)) {
    return "entry 不能含反斜杠、`..` 或控制字符。";
  }
  if (form === "iframe" && /^javascript:/i.test(value)) {
    return "iframe 的 entry 不能是 javascript: URL。";
  }
  if (form === "package" && !/\.nshpkg($|\?)/.test(value)) {
    return "package 的 entry 必须指向一个 .nshpkg 包。";
  }
  return null;
}

/** Normalize one `contributes[]` entry against the slot it targets. */
export function normalizeContribution(
  raw: unknown,
  pluginId: string,
  slot: SlotDeclaration | undefined,
  index = 0,
): { contribution: SlotContribution | null; issues: SlotIssue[] } {
  const issues: SlotIssue[] = [];
  const field = `contributes[${index}]`;
  if (!isRecord(raw)) {
    issues.push(issue("error", "ERR_CONTRIBUTION_INVALID", pluginId, field, "贡献必须是对象。"));
    return { contribution: null, issues };
  }
  checkKeys(
    raw,
    ["id", "slot", "form", "entry", "title", "description", "payload", "capabilities", "targets", "tool", "toolName", "part", "partKind", "priority"],
    pluginId,
    `${field}.`,
    issues,
  );
  const slotID = typeof raw.slot === "string" ? raw.slot.trim() : "";
  if (!slotID) {
    issues.push(issue("error", "ERR_CONTRIBUTION_INVALID", pluginId, `${field}.slot`, "贡献必须指定 slot。"));
  } else if (!slot) {
    issues.push(
      issue(
        "error",
        "ERR_SLOT_UNKNOWN",
        pluginId,
        `${field}.slot`,
        `槽位「${slotID}」没有声明者；贡献不会装载（先安装声明该槽位的插件）。`,
      ),
    );
  }
  const form = typeof raw.form === "string" ? raw.form.trim() : "";
  if (!(SLOT_FORMS as readonly string[]).includes(form)) {
    issues.push(
      issue("error", "ERR_CONTRIBUTION_INVALID", pluginId, `${field}.form`, `贡献形态「${form}」不是 chunk/package/iframe/data。`),
    );
  }
  if (slot && form && !slot.forms.includes(form as SlotForm)) {
    issues.push(
      issue(
        "error",
        "ERR_FORM_NOT_ALLOWED",
        pluginId,
        `${field}.form`,
        `槽位「${slot.id}」只接受 ${slot.forms.join("/")}，不接受 ${form}。`,
      ),
    );
  }
  const entry = typeof raw.entry === "string" ? raw.entry.trim() : "";
  if (form === "chunk" || form === "package" || form === "iframe") {
    if (!entry) {
      issues.push(issue("error", "ERR_CONTRIBUTION_INVALID", pluginId, `${field}.entry`, `${form} 贡献必须提供 entry。`));
    } else {
      const problem = entryProblem(form as SlotForm, entry);
      if (problem) issues.push(issue("error", "ERR_CONTRIBUTION_INVALID", pluginId, `${field}.entry`, problem));
    }
  }
  if (form === "data" && raw.payload === undefined) {
    issues.push(issue("error", "ERR_CONTRIBUTION_INVALID", pluginId, `${field}.payload`, "data 贡献必须提供 payload。"));
  }
  const declaredTargets =
    raw.targets === undefined ? undefined : slotTargets(raw.targets, pluginId, `${field}.targets`, issues);
  const targets = declaredTargets ?? (slot ? [...slot.targets] : []);
  if (slot && declaredTargets) {
    for (const target of declaredTargets) {
      if (!slot.targets.includes(target)) {
        issues.push(
          issue(
            "error",
            "ERR_TARGET_UNAVAILABLE",
            pluginId,
            `${field}.targets`,
            `槽位「${slot.id}」不支持 target「${target}」；贡献不能声明它。`,
          ),
        );
      }
    }
  }
  // A manifest that does not declare capabilities is a legacy/full bridge
  // declaration (the installed webview packages predate this field); a
  // manifest that declares them is gated to exactly that set.
  const capabilitiesRaw =
    raw.capabilities === undefined
      ? [...BRIDGE_CAPABILITIES]
      : stringArray(raw.capabilities, `${field}.capabilities`, pluginId, issues);
  const capabilities: BridgeCapability[] = [];
  for (const capability of capabilitiesRaw) {
    if (!(BRIDGE_CAPABILITIES as readonly string[]).includes(capability)) {
      issues.push(
        issue(
          "error",
          "ERR_ACCESS_DENIED",
          pluginId,
          `${field}.capabilities`,
          `桥能力「${capability}」未声明；只接受 state/call/approve/subscribe。`,
        ),
      );
      continue;
    }
    if (!capabilities.includes(capability as BridgeCapability)) capabilities.push(capability as BridgeCapability);
  }
  if (slot && (form === "data" || form === "chunk") && slot.schema && form === "data") {
    const finding = validateSchemaValue(slot.schema, raw.payload, `${field}.payload`);
    if (finding) issues.push(issue("error", "ERR_CONTRIBUTION_INVALID", pluginId, finding.field, finding.message));
  }
  if (issues.some((entryIssue) => entryIssue.severity === "error")) return { contribution: null, issues };
  const id = typeof raw.id === "string" && raw.id.trim() ? raw.id.trim() : `${pluginId}:${slotID}:${index}`;
  if (!CONTRIBUTION_ID_PATTERN.test(id)) {
    issues.push(
      issue("error", "ERR_CONTRIBUTION_INVALID", pluginId, `${field}.id`, `贡献 id「${id}」不符合文法。`),
    );
    return { contribution: null, issues };
  }
  const toolNameRaw = raw.toolName ?? raw.tool;
  const partKindRaw = raw.partKind ?? raw.part;
  return {
    contribution: {
      id,
      pluginId,
      slot: slotID,
      form: form as SlotForm,
      title: typeof raw.title === "string" ? raw.title : undefined,
      description: typeof raw.description === "string" ? raw.description : undefined,
      entry: entry || undefined,
      payload: raw.payload,
      targets,
      capabilities,
      toolName: typeof toolNameRaw === "string" ? toolNameRaw : undefined,
      partKind: typeof partKindRaw === "string" ? partKindRaw : undefined,
    },
    issues,
  };
}

/** A tiny JSON-schema subset: the shapes slot owners use for `data`
 * contributions. Returns the first finding, field-path-qualified. */
export function validateSchemaValue(
  schema: unknown,
  value: unknown,
  path: string,
): { field: string; message: string } | null {
  if (!isRecord(schema)) return null;
  const type = schema.type;
  if (typeof type === "string" && !matchesType(type, value)) {
    return { field: path, message: `${path} 必须是 ${type}。` };
  }
  if (Array.isArray(schema.enum) && !schema.enum.some((entry) => entry === value)) {
    return { field: path, message: `${path} 必须是 ${schema.enum.map((entry) => JSON.stringify(entry)).join(" / ")} 之一。` };
  }
  if (isRecord(value)) {
    const required = Array.isArray(schema.required) ? schema.required.filter((entry) => typeof entry === "string") : [];
    for (const key of required) {
      if (!(key in value)) return { field: `${path}.${key}`, message: `${path} 缺少必填字段「${key}」。` };
    }
    const properties = isRecord(schema.properties) ? schema.properties : {};
    for (const [key, child] of Object.entries(properties)) {
      if (!(key in value)) continue;
      const finding = validateSchemaValue(child, value[key], `${path}.${key}`);
      if (finding) return finding;
    }
    if (schema.additionalProperties === false) {
      for (const key of Object.keys(value)) {
        if (!(key in properties)) {
          return { field: `${path}.${key}`, message: `${path} 不接受字段「${key}」。` };
        }
      }
    }
  }
  if (Array.isArray(value) && schema.items !== undefined) {
    for (let index = 0; index < value.length; index += 1) {
      const finding = validateSchemaValue(schema.items, value[index], `${path}[${index}]`);
      if (finding) return finding;
    }
  }
  if (typeof value === "string") {
    if (typeof schema.minLength === "number" && value.length < schema.minLength) {
      return { field: path, message: `${path} 至少 ${schema.minLength} 个字符。` };
    }
    if (typeof schema.maxLength === "number" && value.length > schema.maxLength) {
      return { field: path, message: `${path} 最多 ${schema.maxLength} 个字符。` };
    }
  }
  if (typeof value === "number") {
    if (typeof schema.minimum === "number" && value < schema.minimum) {
      return { field: path, message: `${path} 不能小于 ${schema.minimum}。` };
    }
    if (typeof schema.maximum === "number" && value > schema.maximum) {
      return { field: path, message: `${path} 不能大于 ${schema.maximum}。` };
    }
  }
  return null;
}

function matchesType(type: string, value: unknown): boolean {
  switch (type) {
    case "object":
      return isRecord(value);
    case "array":
      return Array.isArray(value);
    case "string":
      return typeof value === "string";
    case "number":
      return typeof value === "number" && Number.isFinite(value);
    case "integer":
      return typeof value === "number" && Number.isInteger(value);
    case "boolean":
      return typeof value === "boolean";
    case "null":
      return value === null;
    default:
      return true;
  }
}

/** Read a server-provided `provides.slots` / `contributes` shape without
 * trusting it: arrays or `undefined`. */
export function readArray(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

/** The normalized arity set, for tests and labels. */
export const ARITIES = SLOT_ARITIES;
