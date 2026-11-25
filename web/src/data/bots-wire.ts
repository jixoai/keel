// The 员工 / 团队 wire additions — change keel-bot-identity-access, A4.4b.
//
// The identity surface already names Account, PermissionGroup, Bot, TeamBot and
// BotKey in `identity.ts`. This module adds the reads the Bot and TeamBot pages
// need on top of that surface, in the same defensive style: a field the server
// did not send stays absent, and nothing is invented to make a page look
// complete.
//
//   - Profile / Role: a Bot's Profile is (Account + Role). The role is what the
//     Bot's persona is; it never grants authority.
//   - SubagentRecord: one spawned/forked child session (`GET /v1/subagents`).
//   - SessionEvent: one ledger event of one session (`GET
//     /v1/sessions/{id}/events`), used by the run projection.
//   - RunStart: the receipt of `POST /v1/bots/{id}/invoke` and
//     `POST /v1/team-bots/{id}/start`.
import { payloadOf } from "./wire";

export interface ProfileView {
  profileId: string;
  accountId: string;
  roleId: string;
  createdAt?: string;
  updatedAt?: string;
}

export interface RoleView {
  roleId: string;
  name: string;
  instruction?: string;
  createdAt?: string;
  updatedAt?: string;
}

export interface SubagentUsageView {
  turns?: number;
  inputTokens?: number;
  outputTokens?: number;
  durationMs?: number;
}

/** One child run as the sub-agent registry reports it. `origin` is the real
 * edge the run recorded: `spawn` (an invoke) or `fork`. A record whose origin
 * is absent keeps no edge label rather than guessing one. `depth` is the
 * collaboration depth of the child (main is 0); a fork record carries none
 * because a fork is a ledger branch, not a delegation. */
export interface SubagentView {
  id: string;
  parentSessionId: string;
  sessionId: string;
  task: string;
  agent?: string;
  status: string;
  startedAt?: string;
  endedAt?: string;
  origin?: string;
  parentTaskId?: string;
  error?: string;
  summary?: string;
  usage?: SubagentUsageView;
  depth?: number;
  maxDepth?: number;
  maxChildren?: number;
  modeId?: string;
}

/** One session-ledger event. `payload` keeps the raw JSON value: the run page
 * reads it through the shared `payloadOf` helpers so a partial payload never
 * breaks the projection. */
export interface SessionEventView {
  seq: number;
  kind: string;
  payload: unknown;
}

/** The receipt of a Bot invoke / TeamBot start. The turn itself runs in the
 * session; this document only names where it landed. */
export interface RunStartView {
  botId?: string;
  teamBotId?: string;
  captainBotId?: string;
  sessionId: string;
  seq?: number;
  requestId?: string;
  deduped?: boolean;
}

/** One member task card linked to a captain session. The shape already lives in
 * `contract.ts` (the workspace's team panel reads it); this module re-exports it
 * so the identity transport can name one import. */
export type { TeamTaskView, TeamTasksView } from "./contract";

export interface SessionCancelView {
  status: string;
  runStatus?: string;
  cancelled: boolean;
  detail?: string;
}

const asRecord = (value: unknown): Record<string, unknown> =>
  value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};

const text = (value: unknown): string => (typeof value === "string" ? value : "");
const textOrUndefined = (value: unknown): string | undefined => {
  const valueText = text(value);
  return valueText ? valueText : undefined;
};
const numberOrUndefined = (value: unknown): number | undefined =>
  typeof value === "number" && Number.isFinite(value) ? value : undefined;
const list = (value: unknown): unknown[] => (Array.isArray(value) ? value : []);

export function parseProfile(value: unknown): ProfileView {
  const row = asRecord(value);
  return {
    profileId: text(row.profileId),
    accountId: text(row.accountId),
    roleId: text(row.roleId),
    createdAt: textOrUndefined(row.createdAt),
    updatedAt: textOrUndefined(row.updatedAt),
  };
}

export function parseRole(value: unknown): RoleView {
  const row = asRecord(value);
  return {
    roleId: text(row.roleId),
    name: text(row.name),
    instruction: textOrUndefined(row.instruction),
    createdAt: textOrUndefined(row.createdAt),
    updatedAt: textOrUndefined(row.updatedAt),
  };
}

/** The sub-agent record, flattened to the display facts. The registry's
 * `result` carries the child's own usage/error; a failed spawn has no result,
 * so its top-level `error` is kept instead. */
export function parseSubagent(value: unknown): SubagentView | undefined {
  const row = asRecord(value);
  const sessionId = text(row.sessionId);
  if (!sessionId) return undefined;
  const result = asRecord(row.result);
  const usageRow = asRecord(result.usage);
  const usage: SubagentUsageView = {
    turns: numberOrUndefined(usageRow.turns),
    inputTokens: numberOrUndefined(usageRow.inputTokens),
    outputTokens: numberOrUndefined(usageRow.outputTokens),
    durationMs: numberOrUndefined(usageRow.durationMs),
  };
  const hasUsage = Object.values(usage).some((entry) => entry !== undefined);
  const depth = numberOrUndefined(row.depth);
  return {
    id: text(row.id) || sessionId,
    parentSessionId: text(row.parentSessionId),
    sessionId,
    task: text(row.task),
    agent: textOrUndefined(row.agent),
    status: text(row.status) || "unknown",
    startedAt: textOrUndefined(row.startedAt),
    endedAt: textOrUndefined(row.endedAt),
    origin: textOrUndefined(row.origin),
    parentTaskId: textOrUndefined(row.parentTaskId),
    error: textOrUndefined(row.error) ?? textOrUndefined(result.error),
    summary: textOrUndefined(result.summary),
    usage: hasUsage ? usage : undefined,
    depth,
    maxDepth: numberOrUndefined(row.maxDepth),
    maxChildren: numberOrUndefined(row.maxChildren),
    modeId: textOrUndefined(row.modeId),
  };
}

export function parseSessionEvent(value: unknown): SessionEventView | undefined {
  const row = asRecord(value);
  const seq = numberOrUndefined(row.seq);
  if (seq === undefined || seq <= 0) return undefined;
  return { seq, kind: text(row.kind) || "message", payload: row.payload };
}

/** A received turn payload as the run page reads it: the shared `payloadOf`
 * helper normalises an object or a JSON string. */
export const eventPayload = (event: SessionEventView): Record<string, unknown> => payloadOf(event);

export function parseRunStart(value: unknown): RunStartView {
  const row = asRecord(value);
  return {
    botId: textOrUndefined(row.botId),
    teamBotId: textOrUndefined(row.teamBotId),
    captainBotId: textOrUndefined(row.captainBotId),
    sessionId: text(row.sessionId),
    seq: numberOrUndefined(row.seq),
    requestId: textOrUndefined(row.requestId),
    deduped: row.deduped === true,
  };
}

export function parseSessionCancel(value: unknown): SessionCancelView {
  const row = asRecord(value);
  return {
    status: text(row.status) || "unknown",
    runStatus: textOrUndefined(row.runStatus),
    cancelled: row.cancelled === true,
    detail: textOrUndefined(row.detail),
  };
}

/** One listing of the new reads; the API bounds every identity page. */
export function pageItems<T>(value: unknown, key: string, parse: (item: unknown) => T | undefined): T[] {
  return list(asRecord(value)[key]).flatMap((item) => {
    const parsed = parse(item);
    return parsed === undefined ? [] : [parsed];
  });
}
