// The OAR render projection — change keel-oar-renderer.
//
// One wire document is the only record model the client renders:
// `keel.oar.render-projection/1` (docs/OAR.md §6). `internal/oar/projection.go`
// folds calls and their results server-side; the client MUST NOT fold a second
// model, so this module only reads the document and answers what a row may say.
//
// Pure and transport-free on purpose: `http.ts` (the read), the fixture
// transport and the builtin renderer chunk all name the same types, and a test
// can exercise the reader without a network.

/** The schema id of the projection this client understands. A document with
 * another schema is refused rather than rendered as if it were this one. */
export const OAR_PROJECTION_SCHEMA = "keel.oar.render-projection/1";

export interface OarRecordErrorView {
  code: string;
  message: string;
  data?: unknown;
}

export interface OarRecordUsageView {
  metering?: string;
  model?: string;
  inputTokens?: number;
  outputTokens?: number;
  totalTokens?: number;
}

/** One agent path's rollup (`Projection.agents`). */
export interface OarProjectionAgentView {
  agentPath: string;
  runtime?: string;
  status?: string;
  calls: number;
  failures: number;
}

/** One renderer row (`Projection.items`): a call and its linked result share
 * one row (`seq` is the call's, `resultSeq` the result's). */
export interface OarProjectionItemView {
  seq: number;
  resultSeq?: number;
  agentPath: string;
  kind: string;
  callId?: string;
  name?: string;
  state?: string;
  input?: unknown;
  output?: unknown;
  error?: OarRecordErrorView;
  text?: string;
  usage?: OarRecordUsageView;
}

/** The whole projection: attribution, runtime facts and the folded rows. */
export interface ExternalProjectionView {
  schema: string;
  /** The runtime's opaque session handle. */
  backendSessionId: string;
  /** The Keel session from the out-of-band attribution. */
  hostSessionId?: string;
  /** The task card the external call belongs to. OAR never creates a card. */
  taskId?: string;
  /** The adapter id ("codex", "fake"). */
  runtime?: string;
  /** A fake runtime. A renderer MUST show this; it is never a live model run. */
  simulated?: boolean;
  agents: OarProjectionAgentView[];
  items: OarProjectionItemView[];
}

const asRecord = (value: unknown): Record<string, unknown> =>
  value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};

const text = (value: unknown): string => (typeof value === "string" ? value : "");

const optionalText = (value: unknown): string | undefined =>
  typeof value === "string" && value !== "" ? value : undefined;

const numberOr = (value: unknown, fallback: number): number =>
  typeof value === "number" && Number.isFinite(value) ? value : fallback;

const optionalNumber = (value: unknown): number | undefined =>
  typeof value === "number" && Number.isFinite(value) ? value : undefined;

const has = (row: Record<string, unknown>, key: string): boolean =>
  Object.prototype.hasOwnProperty.call(row, key) && row[key] !== null;

function readError(value: unknown): OarRecordErrorView | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const row = asRecord(value);
  const code = text(row.code);
  const message = text(row.message);
  if (!code && !message && !has(row, "data")) return undefined;
  return { code, message, data: has(row, "data") ? row.data : undefined };
}

function readUsage(value: unknown): OarRecordUsageView | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const row = asRecord(value);
  const usage: OarRecordUsageView = {
    metering: optionalText(row.metering),
    model: optionalText(row.model),
    inputTokens: optionalNumber(row.inputTokens),
    outputTokens: optionalNumber(row.outputTokens),
    totalTokens: optionalNumber(row.totalTokens),
  };
  const empty =
    usage.metering === undefined &&
    usage.model === undefined &&
    usage.inputTokens === undefined &&
    usage.outputTokens === undefined &&
    usage.totalTokens === undefined;
  return empty ? undefined : usage;
}

function readAgent(value: unknown): OarProjectionAgentView | null {
  const row = asRecord(value);
  const agentPath = text(row.agentPath);
  if (!agentPath) return null;
  return {
    agentPath,
    runtime: optionalText(row.runtime),
    status: optionalText(row.status),
    calls: numberOr(row.calls, 0),
    failures: numberOr(row.failures, 0),
  };
}

/** One row. A row without an agent path or a kind cannot be placed, so it is
 * dropped (the projection is lossless server-side; the client does not invent
 * a path). */
function readItem(value: unknown): OarProjectionItemView | null {
  const row = asRecord(value);
  const agentPath = text(row.agentPath);
  const kind = text(row.kind);
  if (!agentPath || !kind) return null;
  const seq = optionalNumber(row.seq);
  if (seq === undefined) return null;
  const item: OarProjectionItemView = {
    seq,
    resultSeq: optionalNumber(row.resultSeq),
    agentPath,
    kind,
    callId: optionalText(row.callId),
    name: optionalText(row.name),
    state: optionalText(row.state),
    // `input`/`output` are arbitrary JSON: the key's presence is the fact,
    // even when the value is `false`, `0` or an empty object.
    input: has(row, "input") ? row.input : undefined,
    output: has(row, "output") ? row.output : undefined,
    error: readError(row.error),
    text: optionalText(row.text),
    usage: readUsage(row.usage),
  };
  return item;
}

/** Read one projection document. `null` means "not this schema" or "not a
 * projection": the caller reports it instead of drawing guesses. */
export function parseExternalProjection(value: unknown): ExternalProjectionView | null {
  const row = asRecord(value);
  if (text(row.schema) !== OAR_PROJECTION_SCHEMA) return null;
  const itemsRaw = row.items;
  const agentsRaw = row.agents;
  if (itemsRaw !== undefined && !Array.isArray(itemsRaw)) return null;
  if (agentsRaw !== undefined && !Array.isArray(agentsRaw)) return null;
  const agents: OarProjectionAgentView[] = [];
  for (const candidate of Array.isArray(agentsRaw) ? agentsRaw : []) {
    const agent = readAgent(candidate);
    if (agent) agents.push(agent);
  }
  const items: OarProjectionItemView[] = [];
  for (const candidate of Array.isArray(itemsRaw) ? itemsRaw : []) {
    const item = readItem(candidate);
    if (item) items.push(item);
  }
  return {
    schema: OAR_PROJECTION_SCHEMA,
    backendSessionId: text(row.backendSessionId),
    hostSessionId: optionalText(row.hostSessionId),
    taskId: optionalText(row.taskId),
    runtime: optionalText(row.runtime),
    simulated: row.simulated === true ? true : undefined,
    agents,
    items,
  };
}

/** The projection's rows in `seq` order, stable: the server already folds in
 * seq order, and a transport that reorders must not reorder the card. */
export function orderItems(items: OarProjectionItemView[]): OarProjectionItemView[] {
  return [...items].sort((left, right) => left.seq - right.seq);
}

/** The usage a projection reports, added up. A missing usage is "not
 * measurable", never 0 (`RecordUsage`'s rule), so the answer is undefined when
 * no row carried one. */
export function projectionUsage(
  projection: ExternalProjectionView,
): { inputTokens: number; outputTokens: number; totalTokens: number } | undefined {
  let input = 0;
  let output = 0;
  let total = 0;
  let seen = false;
  for (const item of projection.items) {
    const usage = item.usage;
    if (!usage) continue;
    seen = true;
    input += usage.inputTokens ?? 0;
    output += usage.outputTokens ?? 0;
    total += usage.totalTokens ?? (usage.inputTokens ?? 0) + (usage.outputTokens ?? 0);
  }
  return seen ? { inputTokens: input, outputTokens: output, totalTokens: total } : undefined;
}
