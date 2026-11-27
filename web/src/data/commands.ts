// Chat command catalog, argument mapping and presentation helpers
// (change keel-plugin-commands, tasks 4.1/4.4).
//
// A command is a named USER action served by the host. This module is the one
// client-side reading of the server catalog: parsing, the `/name` draft rules,
// the args_schema form subset, and the stable words a row shows. It never
// reorders the catalog (the server's order is builtin → plugin → skill) and
// never invents a command or an argument.
//
// The form subset is deliberate and closed: string, number/integer, boolean,
// enum, array of primitives, and a raw-JSON field for anything else. A command
// whose schema falls outside it still runs — through the JSON field — rather
// than having its arguments guessed from free text.
import type {
  CommandCatalogView,
  CommandDanger,
  CommandSource,
  CommandTarget,
  CommandView,
  DecisionView,
  SettingsIssue,
} from "./contract";

/** The closed form subset. `json` is the fallback for object/nested schemas:
 * the reader types the document and the server validates it field by field. */
export type CommandFieldKind = "string" | "number" | "integer" | "boolean" | "enum" | "array" | "json";

export interface CommandFieldOption {
  /** The Select's string identity. */
  value: string;
  /** The JSON value the schema declared (numbers/booleans keep their type). */
  raw: string | number | boolean;
  label: string;
}

export interface CommandField {
  name: string;
  label: string;
  kind: CommandFieldKind;
  required: boolean;
  hint?: string;
  multiline?: boolean;
  options?: CommandFieldOption[];
  /** The value a fresh form starts with (schema default). */
  defaultValue?: string | boolean;
  minLength?: number;
  maxLength?: number;
  minimum?: number;
  maximum?: number;
  pattern?: string;
  /** For `array`: the primitive item kind. */
  itemsKind?: "string" | "number" | "integer";
}

/** The form's value map: booleans for switches, text for every other control. */
export type CommandValues = Record<string, string | boolean>;

export interface CommandDraftMenu {
  kind: "menu";
  /** What follows the leading '/' (empty for a bare '/'). */
  query: string;
}

export interface CommandDraftMatch {
  kind: "command";
  command: CommandView;
  /** The text after `/name` (trimmed). */
  rest: string;
}

export type CommandDraft = CommandDraftMenu | CommandDraftMatch;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value);

const text = (value: unknown): string => (typeof value === "string" ? value : "");

const stringList = (value: unknown): string[] =>
  Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];

const numberValue = (value: unknown): number | undefined =>
  typeof value === "number" && Number.isFinite(value) ? value : undefined;

/** Strip ANSI escape sequences and C0/C1 controls from copy a plugin/server
 * supplied. A command name/summary is data, not terminal output: a control
 * byte must never move the cursor or forge a row. Newlines are flattened in
 * one-line contexts (summaries, labels). */
export function cleanDisplayText(value: string, keepNewlines = false): string {
  const withoutAnsi = value.replace(/\u001B\[[0-9;?]*[ -/]*[@-~]/g, "");
  const pattern = keepNewlines
    ? /[\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F-\u009F]/g
    : /[\u0000-\u001F\u007F-\u009F]/g;
  const cleaned = withoutAnsi.replace(pattern, " ");
  return keepNewlines
    ? cleaned.replace(/[ \t]+/g, " ").trim()
    : cleaned.replace(/\s+/g, " ").trim();
}

/** One catalog entry from the wire. An entry without a name is unusable and is
 * dropped by the caller; every other field falls back to the safe value. */
export function parseCommand(value: unknown): CommandView | null {
  const row = isRecord(value) ? value : {};
  const name = text(row.name).trim();
  if (!name || /[\u0000-\u001F\u007F]/.test(name)) return null;
  const schema = row.argsSchema;
  const argsSchema = isRecord(schema) ? schema : { type: "object" };
  const targets = stringList(row.targets);
  const reason = cleanDisplayText(text(row.reason));
  const missing = stringList(row.missingPermissions).map((key) => cleanDisplayText(key));
  return {
    name,
    source: cleanDisplayText(text(row.source) || "plugin"),
    summary: cleanDisplayText(text(row.summary)),
    argsSchema,
    danger: text(row.danger) || "write",
    idempotent: row.idempotent === true,
    targets: targets.length ? targets : ["web", "native", "tui"],
    requiredPermissions: stringList(row.requiredPermissions),
    available: row.available === true,
    ...(reason ? { reason } : {}),
    ...(missing.length ? { missingPermissions: missing } : {}),
  };
}

/** The catalog document. The server order is preserved exactly. */
export function parseCommandCatalog(value: unknown): CommandCatalogView {
  const body = isRecord(value) ? value : {};
  const commands: CommandView[] = [];
  for (const raw of Array.isArray(body.commands) ? body.commands : []) {
    const command = parseCommand(raw);
    if (command) commands.push(command);
  }
  const generation = numberValue(body.generation);
  const warnings = stringList(body.warnings).map((warning) => cleanDisplayText(warning));
  return {
    commands,
    truncated: body.truncated === true,
    ...(generation !== undefined ? { generation } : {}),
    ...(warnings.length ? { warnings } : {}),
  };
}

/**
 * Apply the catalog's own target rule on the client too: a command whose
 * `targets` exclude this surface is unavailable with `target_unsupported`.
 * Only a downgrade is possible — the client never makes an unavailable command
 * available, and never reorders.
 */
export function applyTargetAvailability(catalog: CommandCatalogView, target: CommandTarget): CommandCatalogView {
  return {
    ...catalog,
    commands: catalog.commands.map((command) => {
      if (!command.available) return command;
      if (!command.targets.length || command.targets.includes(target)) return command;
      return { ...command, available: false, reason: "target_unsupported" };
    }),
  };
}

/** The current surface, the same vocabulary plugins use. */
export function commandTargetFor(idiom: string, os: string): CommandTarget {
  if (idiom === "terminal") return "tui";
  if (os === "web") return "web";
  return "native";
}

// ---- the `/` draft -------------------------------------------------------

/** The menu's query while the FIRST token is still being typed (`/`, `/comp`),
 * or null once the draft has whitespace (the command word is settled) or is
 * not a command draft. */
export function commandMenuQuery(draft: string): string | null {
  if (!draft.startsWith("/")) return null;
  const token = draft.slice(1);
  if (/\s/.test(token)) return null;
  return token;
}

export interface CommandWord {
  /** The first token without the leading '/'. */
  name: string;
  /** Everything after the first token, trimmed. */
  rest: string;
}

/**
 * The first command word of a draft, independent of the catalog. The Composer
 * uses it to decide "this is a command attempt" BEFORE it can resolve a name:
 * a first token starting with '/' must never fall back to an ordinary prompt
 * when the catalog is still loading, failed, or does not know the name.
 */
export function commandWord(draft: string): CommandWord | null {
  const trimmed = draft.trimStart();
  if (!trimmed.startsWith("/")) return null;
  const token = trimmed.slice(1);
  const match = /^([^\s]*)([\s\S]*)$/.exec(token);
  if (!match) return null;
  return { name: match[1] ?? "", rest: (match[2] ?? "").trim() };
}

/**
 * What the composer's current draft means:
 *   · null — not a command draft;
 *   · menu — the first token is still being typed (`/`, `/comp`), so the
 *     catalog menu is open and filters by `query`;
 *   · command — `/name` followed by whitespace, resolved to a catalog entry
 *     (the rest of the draft is its free-text remainder).
 *
 * Only the FIRST token is a command word. A slash in prose (`请看 /compact`)
 * is not a command, and an unknown first token is not one either (it stays a
 * plain message; `/learn` is handled before this function by the composer).
 */
export function commandDraft(draft: string, commands: readonly CommandView[]): CommandDraft | null {
  if (!draft.startsWith("/")) return null;
  const token = draft.slice(1);
  const match = /^[^\s]*/.exec(token);
  const word = match ? match[0] : "";
  const rest = token.slice(word.length);
  if (!rest) return { kind: "menu", query: word };
  const command = commands.find((candidate) => candidate.name === word);
  if (!command) return null;
  return { kind: "command", command, rest: rest.replace(/^\s+/, "") };
}

/** The rows a menu query shows, in catalog order (unavailable rows included:
 * a reason must be visible, never a silent disappearance). */
export function matchingCommands(
  commands: readonly CommandView[],
  query: string,
): CommandView[] {
  const needle = query.trim().toLowerCase();
  if (!needle) return [...commands];
  return commands.filter((command) => {
    const haystack = `${command.name} ${command.summary}`.toLowerCase();
    return haystack.includes(needle);
  });
}

export interface CommandGroupView {
  /** A known source, or `other` for a source this build does not know. */
  id: CommandSource | "other";
  label: string;
  commands: CommandView[];
}

const SOURCE_ORDER: Array<{ id: CommandSource; label: string }> = [
  { id: "builtin", label: "内置命令" },
  { id: "plugin", label: "插件命令" },
  { id: "skill", label: "技能" },
];

/**
 * Group the catalog for display. Because the server order is builtin → plugin
 * → skill, grouping preserves it; an unknown source still appears (under its
 * own heading after the known ones), so the client cannot hide a row it does
 * not understand.
 */
export function commandGroups(commands: readonly CommandView[]): CommandGroupView[] {
  const groups: CommandGroupView[] = [];
  for (const source of SOURCE_ORDER) {
    const rows = commands.filter((command) => command.source === source.id);
    if (rows.length) groups.push({ id: source.id, label: source.label, commands: rows });
  }
  const other = commands.filter((command) => !SOURCE_ORDER.some((source) => source.id === command.source));
  if (other.length) {
    groups.push({ id: "other", label: "其他来源", commands: other });
  }
  return groups;
}

// ---- words ---------------------------------------------------------------

export function commandDangerLabel(danger: string): string {
  switch (danger as CommandDanger) {
    case "read":
      return "只读";
    case "destructive":
      return "破坏性";
    default:
      return "写入";
  }
}

export function commandSourceLabel(source: string): string {
  switch (source as CommandSource) {
    case "builtin":
      return "内置";
    case "skill":
      return "技能";
    case "plugin":
      return "插件";
    default:
      return source || "命令";
  }
}

/** Why a row cannot run now, in the reader's words. Empty when it can. */
export function commandAvailabilityText(command: CommandView): string {
  if (command.available) return "";
  switch (command.reason) {
    case "plugin_pending":
      return "插件仍在装配，暂时不能执行";
    case "plugin_failed":
      return "插件装配失败，暂时不能执行";
    case "missing_permissions": {
      const keys = command.missingPermissions?.length
        ? command.missingPermissions
        : command.requiredPermissions;
      return keys.length ? `缺少权限：${keys.join("、")}` : "缺少执行权限";
    }
    case "target_unsupported":
      return command.targets.length
        ? `当前客户端不支持（仅 ${command.targets.join(" / ")}）`
        : "当前客户端不支持";
    case "not_wired":
      return "此部署未接通命令执行";
    default:
      return command.reason ? `不可用：${command.reason}` : "不可用";
  }
}

export function commandStatusLabel(status: string): string {
  switch (status) {
    case "ok":
      return "成功";
    case "error":
      return "失败";
    case "refused":
      return "已拒绝";
    case "timed_out":
      return "审批超时";
    case "cancelled":
      return "已取消";
    case "pending_decision":
      return "等待审批";
    case "running":
      return "执行中";
    default:
      return status || "未知状态";
  }
}

export type CommandStatusTone = "success" | "destructive" | "warning" | "info" | "secondary";

export function commandStatusTone(status: string): CommandStatusTone {
  switch (status) {
    case "ok":
      return "success";
    case "error":
      return "destructive";
    case "refused":
      return "destructive";
    case "timed_out":
      return "warning";
    case "cancelled":
      return "secondary";
    case "pending_decision":
    case "running":
      return "info";
    default:
      return "secondary";
  }
}

/** The compact result line a row shows (bounded; the full document stays in
 * the fold). Never says more than it has: an absent result is no line. */
export function commandResultSummary(data: unknown, max = 240): string {
  if (data === undefined || data === null) return "";
  // A plugin tool result is the gateway envelope (`content` + structured
  // content). The structured half is the command's own result; show it first,
  // and fall back to the envelope only when the tool returned text alone.
  const envelope = isRecord(data) && isRecord(data.structuredContent) ? data.structuredContent : data;
  let text: string;
  if (typeof envelope === "string") text = envelope;
  else {
    try {
      text = JSON.stringify(envelope);
    } catch {
      text = String(envelope);
    }
  }
  if (!text) return "";
  const clean = cleanDisplayText(text, true);
  return clean.length > max ? `${clean.slice(0, max)}…` : clean;
}

// ---- args_schema → form --------------------------------------------------

const propertySchema = (value: unknown): Record<string, unknown> | null => (isRecord(value) ? value : null);

const enumOptions = (schema: Record<string, unknown>): CommandFieldOption[] | undefined => {
  if (!Array.isArray(schema.enum)) return undefined;
  const options: CommandFieldOption[] = [];
  for (const raw of schema.enum) {
    if (typeof raw === "string" || typeof raw === "number" || typeof raw === "boolean") {
      options.push({ value: String(raw), raw, label: String(raw) });
    }
  }
  return options.length ? options : undefined;
};

/**
 * The fields a schema form draws, in property declaration order. Anything the
 * closed subset does not cover becomes a `json` field — the reader supplies
 * the document and the server is the judge — instead of being dropped.
 */
export function commandFields(command: CommandView): CommandField[] {
  const schema = isRecord(command.argsSchema) ? command.argsSchema : {};
  const properties = isRecord(schema.properties) ? schema.properties : {};
  const required = new Set(stringList(schema.required));
  const fields: CommandField[] = [];
  for (const [name, rawProperty] of Object.entries(properties)) {
    const property = propertySchema(rawProperty) ?? {};
    const type = text(property.type);
    const options = enumOptions(property);
    const hint = cleanDisplayText(text(property.description)) || undefined;
    const defaultValueRaw = property.default;
    const defaultValue =
      typeof defaultValueRaw === "string" || typeof defaultValueRaw === "boolean"
        ? defaultValueRaw
        : typeof defaultValueRaw === "number"
          ? String(defaultValueRaw)
          : undefined;
    const items = propertySchema(property.items);
    const itemsKindRaw = items ? text(items.type) : "";
    const itemsKind =
      itemsKindRaw === "string" || itemsKindRaw === "number" || itemsKindRaw === "integer"
        ? (itemsKindRaw as CommandField["itemsKind"])
        : undefined;
    let kind: CommandFieldKind = "json";
    if (options) kind = "enum";
    else if (type === "boolean") kind = "boolean";
    else if (type === "number") kind = "number";
    else if (type === "integer") kind = "integer";
    else if (type === "array") kind = itemsKind ? "array" : "json";
    else if (type === "string" || type === "") kind = "string";
    fields.push({
      name,
      label: cleanDisplayText(name) || name,
      kind,
      required: required.has(name),
      ...(hint ? { hint } : {}),
      ...(kind === "string" && (text(property.format) === "textarea" || (numberValue(property.maxLength) ?? 0) > 200)
        ? { multiline: true }
        : {}),
      ...(options ? { options } : {}),
      ...(defaultValue !== undefined ? { defaultValue } : {}),
      ...(numberValue(property.minLength) !== undefined ? { minLength: numberValue(property.minLength) } : {}),
      ...(numberValue(property.maxLength) !== undefined ? { maxLength: numberValue(property.maxLength) } : {}),
      ...(numberValue(property.minimum) !== undefined ? { minimum: numberValue(property.minimum) } : {}),
      ...(numberValue(property.maximum) !== undefined ? { maximum: numberValue(property.maximum) } : {}),
      ...(text(property.pattern) ? { pattern: text(property.pattern) } : {}),
      ...(itemsKind ? { itemsKind } : {}),
    });
  }
  return fields;
}

/** A fresh form's values: schema defaults, `false` for switches, otherwise "". */
export function initialCommandValues(fields: readonly CommandField[]): CommandValues {
  const values: CommandValues = {};
  for (const field of fields) {
    if (field.kind === "boolean") values[field.name] = field.defaultValue === true;
    else values[field.name] = field.defaultValue === undefined ? "" : String(field.defaultValue);
  }
  return values;
}

/**
 * `/name` is followed by free text only when the schema's sole property is a
 * string named `text`. Every other shape needs the form: the rest of the line
 * is NOT packed into a guessed argument.
 */
export function commandUsesFreeText(command: CommandView): boolean {
  const fields = commandFields(command);
  return fields.length === 1 && fields[0]!.name === "text" && fields[0]!.kind === "string";
}

export function commandNeedsForm(command: CommandView): boolean {
  const fields = commandFields(command);
  if (!fields.length) return false;
  return !commandUsesFreeText(command);
}

const utf8Length = (value: string): number => {
  // Rune count, matching the server's minLength/maxLength (utf8.RuneCount).
  return Array.from(value).length;
};

export interface BuildArgsResult {
  args: Record<string, unknown>;
  errors: Record<string, string>;
}

/**
 * Build the argument document from the form values, validating the same subset
 * the form draws. Empty optional values are omitted (never sent as ""); a
 * required empty value and a malformed number/JSON are field errors. The
 * server remains authoritative (a 400's field errors are merged by the store).
 */
export function buildCommandArgs(fields: readonly CommandField[], values: CommandValues): BuildArgsResult {
  const args: Record<string, unknown> = {};
  const errors: Record<string, string> = {};
  for (const field of fields) {
    const raw = values[field.name];
    if (field.kind === "boolean") {
      args[field.name] = raw === true;
      continue;
    }
    const input = typeof raw === "string" ? raw : "";
    if (field.kind === "array") {
      const lines = input
        .split(/\r?\n/)
        .map((line) => line.trim())
        .filter((line) => line.length > 0);
      if (!lines.length) {
        if (field.required) errors[field.name] = "必填：至少一项";
        continue;
      }
      if (field.itemsKind === "number" || field.itemsKind === "integer") {
        const numbers: number[] = [];
        let invalid = false;
        for (const line of lines) {
          const value = Number(line);
          if (!Number.isFinite(value) || (field.itemsKind === "integer" && !Number.isInteger(value))) {
            invalid = true;
            break;
          }
          numbers.push(value);
        }
        if (invalid) {
          errors[field.name] = field.itemsKind === "integer" ? "每项必须是整数" : "每项必须是数字";
          continue;
        }
        args[field.name] = numbers;
      } else {
        args[field.name] = lines;
      }
      continue;
    }
    if (field.kind === "json") {
      const trimmed = input.trim();
      if (!trimmed) {
        if (field.required) errors[field.name] = "必填";
        continue;
      }
      try {
        args[field.name] = JSON.parse(trimmed);
      } catch {
        errors[field.name] = "必须是合法 JSON";
      }
      continue;
    }
    const trimmed = input.trim();
    if (field.kind === "enum") {
      if (!trimmed) {
        if (field.required) errors[field.name] = "必填";
        continue;
      }
      const option = field.options?.find((candidate) => candidate.value === trimmed);
      if (!option) {
        errors[field.name] = "不在允许的取值内";
        continue;
      }
      args[field.name] = option.raw;
      continue;
    }
    if (field.kind === "number" || field.kind === "integer") {
      if (!trimmed) {
        if (field.required) errors[field.name] = "必填";
        continue;
      }
      const value = Number(trimmed);
      if (!Number.isFinite(value) || (field.kind === "integer" && !Number.isInteger(value))) {
        errors[field.name] = field.kind === "integer" ? "必须是整数" : "必须是数字";
        continue;
      }
      if (field.minimum !== undefined && value < field.minimum) {
        errors[field.name] = `不得小于 ${field.minimum}`;
        continue;
      }
      if (field.maximum !== undefined && value > field.maximum) {
        errors[field.name] = `不得大于 ${field.maximum}`;
        continue;
      }
      args[field.name] = value;
      continue;
    }
    // string
    if (!trimmed) {
      if (field.required) errors[field.name] = "必填";
      continue;
    }
    const length = utf8Length(trimmed);
    if (field.minLength !== undefined && length < field.minLength) {
      errors[field.name] = `至少 ${field.minLength} 个字符`;
      continue;
    }
    if (field.maxLength !== undefined && length > field.maxLength) {
      errors[field.name] = `最多 ${field.maxLength} 个字符`;
      continue;
    }
    if (field.pattern) {
      try {
        if (!new RegExp(field.pattern).test(trimmed)) {
          errors[field.name] = "格式不符合要求";
          continue;
        }
      } catch {
        // An invalid pattern is not the reader's mistake: the server decides.
      }
    }
    args[field.name] = trimmed;
  }
  return { args, errors };
}

/** The field name the server's error path names (`args.path` → `path`). */
export function commandFieldName(path: string): string {
  const trimmed = path.trim();
  if (!trimmed) return "";
  if (trimmed === "args") return "";
  const name = trimmed.startsWith("args.") ? trimmed.slice("args.".length) : trimmed;
  // An array item (`tags[0]`) or a nested property (`nested.a`) belongs to the
  // form field that owns it; the field renders the error beside that control.
  const top = name.split(/[.[]/, 1)[0] ?? "";
  return top.trim();
}

/** Merge a 400's field issues into the form's field errors. */
export function commandFieldErrors(issues: readonly SettingsIssue[]): Record<string, string> {
  const errors: Record<string, string> = {};
  for (const issue of issues) {
    const name = commandFieldName(issue.field);
    if (name) errors[name] = issue.message;
  }
  return errors;
}

/** A command's durable failure text in the UI language. Known codes map to
 * one sentence; an unknown code keeps the server's message (never hidden). */
export function commandErrorText(errorCode: string | undefined, error: string | undefined): string {
  switch (errorCode) {
    case "decision_refused":
      return "审批被拒绝，命令没有执行。";
    case "decision_timeout":
      return "审批超时，命令没有执行。";
    case "decision_cancelled":
      return "审批已取消，命令没有执行。";
    case "session_busy":
      return "会话正忙，命令没有执行。";
    case "unknown_command":
      return "命令不在当前目录中。";
    case "permission_denied":
      return "没有执行该命令的权限。";
    case "plugin_unavailable":
      return "插件当前不可用，命令没有执行。";
    case "tool_error":
      return "命令执行失败（工具错误）。";
    case "not_wired":
      return "此部署未接通命令执行。";
    case "invalid_arguments":
      return "命令参数不合法。";
    case "compaction_disabled":
      return "压缩策略已关闭，无法手动压缩。";
    case "compaction_conflict":
      return "当前上下文无法安全压缩。";
    default:
      return cleanDisplayText(error ?? "", true) || "命令执行失败。";
  }
}

/** The destructive approval question is server text (English); render it in
 * the product language while keeping the argument preview the approver needs. */
export function localizeDecision(decision: DecisionView): DecisionView {
  const prefix = "Run the destructive command /";
  if (!decision.question.startsWith(prefix)) return decision;
  const afterPrefix = decision.question.slice(prefix.length);
  const name = afterPrefix.split("?", 1)[0] ?? "";
  const argsAt = decision.question.indexOf("Arguments: ");
  const args = argsAt >= 0 ? decision.question.slice(argsAt + "Arguments: ".length) : "";
  const question = `是否执行破坏性命令 /${cleanDisplayText(name)}？批准后只会执行一次。${
    args ? `\n参数：${cleanDisplayText(args, true)}` : ""
  }`;
  return {
    ...decision,
    question,
    options: decision.options.map((option) => {
      if (option.optionId === "allow") return { ...option, label: "执行" };
      if (option.optionId === "deny") return { ...option, label: "取消" };
      return option;
    }),
  };
}

/** The full (bounded) result document a row's detail fold shows. */
export function commandResultDetail(data: unknown, max = 8000): string {
  if (data === undefined || data === null) return "";
  let text: string;
  try {
    text = typeof data === "string" ? data : JSON.stringify(data, null, 2);
  } catch {
    text = String(data);
  }
  const clean = cleanDisplayText(text, true);
  return clean.length > max ? `${clean.slice(0, max)}…（已截断）` : clean;
}

/** The one-line label a menu/palette row shows. */
export function commandRowLabel(command: CommandView): string {
  const name = `/${command.name}`;
  const reason = commandAvailabilityText(command);
  const detail = reason ? `${command.summary || commandSourceLabel(command.source)} · ${reason}` : command.summary;
  return detail ? `${name}  ${detail}` : name;
}
