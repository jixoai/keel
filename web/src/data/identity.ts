// The identity/access wire (`/v1/accounts`, `/v1/permission-groups`,
// `/v1/bots`, `/v1/team-bots`, `/v1/auth/whoami`) — change keel-bot-identity-access,
// A4.3.
//
// The server is the authority: this module only names what the API returns and
// parses it defensively. Nothing here invents a row, an account or a grant; a
// field the server did not send stays absent, and an unknown kind/scope travels
// verbatim instead of being guessed into a safe-looking value.
//
// Two facts drive the shapes:
//   - Account is the login and permission subject; Bot/TeamBot are resources
//     (TeamBot is never a permission subject, L12);
//   - a permission key is `domain.action`, its scope is `own|team|tenant|all`,
//     and the effective set is the server's answer, never a client union.

/** The scopes, broad to narrow. `own < team < tenant < all`. */
export const PERMISSION_SCOPES = ["own", "team", "tenant", "all"] as const;
export type PermissionScope = (typeof PERMISSION_SCOPES)[number];

export const scopeRank = (scope: string): number => {
  const index = PERMISSION_SCOPES.indexOf(scope as PermissionScope);
  return index < 0 ? -1 : index;
};

/** `own|team|tenant|all` only; anything else is dropped at the parser. */
export function parseScope(value: unknown): PermissionScope | undefined {
  return typeof value === "string" && (PERMISSION_SCOPES as readonly string[]).includes(value)
    ? (value as PermissionScope)
    : undefined;
}

/** key -> scope. Unknown scopes are dropped rather than widened. */
export function parsePermissionMap(value: unknown): Record<string, PermissionScope> {
  const row = asRecord(value);
  const permissions: Record<string, PermissionScope> = {};
  for (const [key, raw] of Object.entries(row)) {
    const scope = parseScope(raw);
    if (key && scope) permissions[key] = scope;
  }
  return permissions;
}

export interface AccountView {
  accountId: string;
  username: string;
  displayName: string;
  kind: string;
  isSuperAdmin: boolean;
  disabledAt?: string;
  createdAt?: string;
  updatedAt?: string;
}

export interface PermissionGroupSummary {
  groupId: string;
  name: string;
  builtin: boolean;
  createdAt?: string;
  updatedAt?: string;
}

/** The built-in group tokens the server seeds (`internal/access/builtin.go`)
 * and the words the product uses for them. A custom group keeps its own name;
 * an unknown built-in token travels verbatim rather than being guessed. */
const BUILTIN_GROUP_LABELS: Record<string, string> = {
  "super-admin": "超级管理员",
  "tenant-admin": "租户管理员",
  "bot-operator": "Bot 运营",
  viewer: "只读成员",
  auditor: "审计员",
};

export const groupDisplayName = (name: string | undefined): string => {
  const token = (name ?? "").trim();
  return BUILTIN_GROUP_LABELS[token] ?? token;
};

/** The deployment's full-authority preset, in either the server token or the
 * product word (the fixture and a live deployment use different spellings). */
export const isSuperAdminGroup = (name: string | undefined): boolean => {
  const token = (name ?? "").trim();
  return token === "super-admin" || token === "超级管理员";
};

export interface PermissionGrantView {
  key: string;
  scope: PermissionScope;
}

export interface PermissionGroupMember {
  accountId: string;
  addedBy?: string;
  createdAt?: string;
}

export interface PermissionGroupDetail {
  group: PermissionGroupSummary;
  grants: PermissionGrantView[];
  members: PermissionGroupMember[];
}

export interface ModelRef {
  providerId: string;
  modelId: string;
}

export interface HarnessView {
  tools: string[];
  context: {
    instruction: string;
    permissions: string[];
  };
}

export interface BotView {
  botId: string;
  name: string;
  profileId: string;
  accountId?: string;
  modelKind: string;
  defaultModel: ModelRef;
  harness: HarnessView;
  archived: boolean;
  createdAt?: string;
  updatedAt?: string;
  /** Viewer/editor tuples on this Bot (the 「已共享」 badge). */
  sharedCount?: number;
}

export interface RosterEntry {
  botId: string;
  role: string;
}

export interface TeamBotView {
  teamBotId: string;
  name: string;
  mission: string;
  roster: RosterEntry[];
  revision: number;
  hash: string;
  archived: boolean;
  createdAt?: string;
  updatedAt?: string;
  /** The account named by the TeamBot's owner relation; sharing is owner-only. */
  ownerAccountId?: string;
  /** Viewer/editor tuples on this TeamBot (visibility shares). */
  sharedCount?: number;
}

// ---- resource sharing (A6) ------------------------------------------------

/** The three shareable resource families. */
export type ShareResourceKind = "session" | "bot" | "team_bot";

/** Only viewer/editor are writable through sharing; owner/team_member are
 * system-managed and the server answers 400 for them. */
export type ShareRelation = "viewer" | "editor";

/** The subject types a share tuple may name. The product UI offers accounts and
 * permission groups; team_bot exists on the API for team-scoped visibility. */
export type ShareSubjectType = "account" | "group" | "team_bot";

export interface ShareView {
  subjectType: ShareSubjectType;
  subjectId: string;
  relation: ShareRelation;
  /** Resolved display name (account display name, group/team name). */
  displayName?: string;
  /** Account username, when the subject is an account. Never a credential. */
  username?: string;
}

export interface ShareInput {
  subjectType: ShareSubjectType;
  subjectId: string;
  relation: ShareRelation;
}

export interface ShareWriteResult {
  /** Present on add: false for an idempotent duplicate tuple. */
  created?: boolean;
  /** Present on revoke. */
  revoked?: boolean;
  share: ShareView;
}

export interface BotKeyView {
  keyId: string;
  prefix: string;
  accountId: string;
  botId: string;
  label: string;
  createdAt?: string;
  revokedAt?: string;
  lastUsedAt?: string;
}

/** The caller's identity and materialized permissions (GET /v1/auth/whoami). */
export interface WhoAmI {
  tenantId: string;
  accountId: string;
  username?: string;
  displayName?: string;
  kind?: string;
  disabled: boolean;
  isSuperAdmin: boolean;
  /** Bot ID when the credential is a bot key; absent for a human session. */
  botId?: string;
  keyId?: string;
  keyPrefix?: string;
  permissions: Record<string, PermissionScope>;
}

/** The session document (GET /v1/auth/session, POST login/password). */
export interface AuthSessionView {
  authenticated: boolean;
  mode: string;
  bootstrapRequired: boolean;
  csrfToken?: string;
  mustChangePassword: boolean;
  tenantId?: string;
  accountId?: string;
  username?: string;
  displayName?: string;
  isSuperAdmin: boolean;
  localAdmin: boolean;
  botId?: string;
  sessionId?: string;
  expiresAt?: string;
}

/** One page of an identity listing. The API's only paging control is `limit`
 * (bounded server-side); `hasMore` is a lower bound: a full page may be the end. */
export interface IdentityPage<T> {
  items: T[];
  limit: number;
  hasMore: boolean;
}

// ---- the audit trail (the 记录 chapter's read) -----------------------------
//
// The deployment's audit ledger is one table (`auth_events`), written by the
// server on every grant/deny/login/relation change. A4.4a draws its projection;
// the read route itself has not landed yet, so `KeelHttp.listAuditEvents`
// answers the server's own 404 and the page renders its 待接入 face — it never
// fabricates rows. The shape below is the intended contract: the server names
// the event's own action token and, when it can, a human sentence; the UI maps
// the token through a small display vocabulary and prints the sentence it was
// given.

/** How a record's kind reads in the filter. The wire's `kind` token travels
 * verbatim; a token outside this set is kept as `other` with `action` intact. */
export type AuditKind = "login" | "grant" | "deny" | "super-admin" | "relation" | "invoke" | "account" | "team" | "other";

export interface AuditEventView {
  eventId: string;
  occurredAt?: string;
  kind: AuditKind;
  /** The server's stable action token (`login`, `grant`, `deny`, …). */
  action: string;
  /** The human sentence when the server sent one; absent otherwise. */
  summary?: string;
  actorName?: string;
  actorKind?: string;
  /** The acting account's id, when the record carries it. It lets the account
   * index derive a real 「最近活动」 instead of guessing by display name. */
  actorId?: string;
  objectType?: string;
  objectName?: string;
  /** The object's own id, when the record carries one (the actionable links). */
  objectId?: string;
  result: "ok" | "denied" | "error";
  requestId?: string;
  /** A masked source (IP), already masked by the server. */
  source?: string;
  /** The capability a refusal named (the server's `detail.key`), so the
   * detail panel can print `bot.invoke` instead of a generic 「被拒绝」. */
  capabilityKey?: string;
  /** The object's owning account, when the record names it. */
  objectOwnerName?: string;
  objectOwnerId?: string;
  /** The groups or people who can grant the missing capability, when named. */
  grantableBy?: string[];
  /** A run a team event points at (`team_bot_id` + captain `session_id`). */
  teamId?: string;
  runId?: string;
}

export interface AuditQuery {
  limit?: number;
  /** ISO timestamp: the oldest event to include. */
  since?: string;
  /** Comma-separated kind tokens. */
  kind?: string;
  result?: "ok" | "denied" | "error";
}

const AUDIT_KINDS: AuditKind[] = ["login", "grant", "deny", "super-admin", "relation", "invoke", "account", "team"];

export function parseAuditKind(value: unknown): AuditKind {
  const token = text(value);
  return (AUDIT_KINDS as string[]).includes(token) ? (token as AuditKind) : "other";
}

/** A string list from the wire: an array of strings, or one comma/、 separated
 * string. Empty entries are dropped, never turned into an empty chip. */
const parseTextList = (value: unknown): string[] => {
  const raw = Array.isArray(value) ? value : typeof value === "string" ? value.split(/[,，、]/) : [];
  return raw.map((entry) => text(entry).trim()).filter((entry) => entry !== "");
};

/** The first non-empty text among the record's top level and its detail map
 * (the server writes identifiers into `detail`; a newer answer may hoist them). */
const firstText = (row: Record<string, unknown>, detail: Record<string, unknown>, names: string[]): string | undefined => {
  for (const name of names) {
    const found = textOrUndefined(row[name]) ?? textOrUndefined(detail[name]);
    if (found) return found;
  }
  return undefined;
};

export function parseAuditEvent(value: unknown): AuditEventView | undefined {
  const row = asRecord(value);
  const detail = asRecord(row.detail);
  const eventId = text(row.eventId ?? row.id);
  if (!eventId) return undefined;
  const resultToken = text(row.result);
  const grantable = row.grantableBy ?? row.grantable_by ?? detail.grantable_by ?? detail.grantableBy;
  return {
    eventId,
    occurredAt: textOrUndefined(row.occurredAt ?? row.createdAt),
    kind: parseAuditKind(row.kind ?? row.eventKind),
    action: text(row.action ?? row.event),
    summary: textOrUndefined(row.summary),
    actorName: textOrUndefined(row.actorName),
    actorKind: textOrUndefined(row.actorKind),
    actorId: firstText(row, detail, ["actorId", "actorAccountId", "actor_account_id"]),
    objectType: textOrUndefined(row.objectType),
    objectName: textOrUndefined(row.objectName),
    objectId: firstText(row, detail, ["objectId", "resourceId", "resource_id"]),
    result: resultToken === "denied" || resultToken === "error" ? resultToken : "ok",
    requestId: textOrUndefined(row.requestId),
    source: textOrUndefined(row.source),
    capabilityKey: firstText(row, detail, ["capabilityKey", "requiredKey", "required_key", "key"]),
    objectOwnerName: firstText(row, detail, ["objectOwnerName", "ownerName", "owner_name"]),
    objectOwnerId: firstText(row, detail, ["objectOwnerId", "ownerId", "owner_id"]),
    grantableBy: parseTextList(grantable),
    teamId: firstText(row, detail, ["teamId", "teamBotId", "team_bot_id"]),
    runId: firstText(row, detail, ["runId", "sessionId", "session_id"]),
  };
}


export interface CreateAccountInput {
  username: string;
  displayName?: string;
  kind?: "human" | "bot" | "service";
  password?: string;
}

export interface CreateBotInput {
  botId?: string;
  name: string;
  profileId: string;
  modelKind: string;
  defaultModel?: ModelRef;
  harness?: HarnessView;
}

export interface UpdateBotInput {
  name?: string;
  profileId?: string;
  modelKind?: string;
  defaultModel?: ModelRef;
  harness?: HarnessView;
  expectedArchived?: boolean;
}

export interface CreateTeamBotInput {
  teamBotId?: string;
  name: string;
  mission: string;
  roster?: RosterEntry[];
}

export interface UpdateTeamBotInput {
  name?: string;
  mission?: string;
  roster?: RosterEntry[];
  expectedRevision: number;
}

// ---- parsers ---------------------------------------------------------------

const asRecord = (value: unknown): Record<string, unknown> =>
  value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};

const text = (value: unknown): string => (typeof value === "string" ? value : "");
const textOrUndefined = (value: unknown): string | undefined => {
  const valueText = text(value);
  return valueText ? valueText : undefined;
};
const bool = (value: unknown): boolean => value === true;
const list = (value: unknown): unknown[] => (Array.isArray(value) ? value : []);
const count = (value: unknown, fallback = 0): number =>
  typeof value === "number" && Number.isFinite(value) ? value : fallback;
const countOrUndefined = (value: unknown): number | undefined =>
  typeof value === "number" && Number.isFinite(value) ? value : undefined;

export function parseAccount(value: unknown): AccountView {
  const row = asRecord(value);
  return {
    accountId: text(row.accountId),
    username: text(row.username),
    displayName: text(row.displayName),
    kind: text(row.kind),
    isSuperAdmin: bool(row.isSuperAdmin),
    disabledAt: textOrUndefined(row.disabledAt),
    createdAt: textOrUndefined(row.createdAt),
    updatedAt: textOrUndefined(row.updatedAt),
  };
}

export function parsePermissionGroup(value: unknown): PermissionGroupSummary {
  const row = asRecord(value);
  return {
    groupId: text(row.groupId),
    name: text(row.name),
    builtin: bool(row.builtin),
    createdAt: textOrUndefined(row.createdAt),
    updatedAt: textOrUndefined(row.updatedAt),
  };
}

export function parsePermissionGrant(value: unknown): PermissionGrantView | undefined {
  const row = asRecord(value);
  const key = text(row.key);
  const scope = parseScope(row.scope);
  return key && scope ? { key, scope } : undefined;
}

export function parsePermissionGroupMember(value: unknown): PermissionGroupMember | undefined {
  const row = asRecord(value);
  const accountId = text(row.accountId);
  if (!accountId) return undefined;
  return {
    accountId,
    addedBy: textOrUndefined(row.addedBy),
    createdAt: textOrUndefined(row.createdAt),
  };
}

export function parsePermissionGroupDetail(value: unknown): PermissionGroupDetail {
  const row = asRecord(value);
  return {
    group: parsePermissionGroup(row.group),
    grants: list(row.grants).flatMap((item) => {
      const grant = parsePermissionGrant(item);
      return grant ? [grant] : [];
    }),
    members: list(row.members).flatMap((item) => {
      const member = parsePermissionGroupMember(item);
      return member ? [member] : [];
    }),
  };
}

const parseModelRef = (value: unknown): ModelRef => {
  const row = asRecord(value);
  return { providerId: text(row.providerId), modelId: text(row.modelId) };
};

const parseHarness = (value: unknown): HarnessView => {
  const row = asRecord(value);
  const context = asRecord(row.context);
  return {
    tools: list(row.tools).map(text).filter((tool) => tool),
    context: {
      instruction: text(context.instruction),
      permissions: list(context.permissions).map(text).filter((key) => key),
    },
  };
};

export function parseBot(value: unknown): BotView {
  const row = asRecord(value);
  return {
    botId: text(row.botId),
    name: text(row.name),
    profileId: text(row.profileId),
    accountId: textOrUndefined(row.accountId),
    modelKind: text(row.modelKind),
    defaultModel: parseModelRef(row.defaultModel),
    harness: parseHarness(row.harness),
    archived: bool(row.archived),
    createdAt: textOrUndefined(row.createdAt),
    updatedAt: textOrUndefined(row.updatedAt),
    sharedCount: countOrUndefined(row.sharedCount),
  };
}

const parseRosterEntry = (value: unknown): RosterEntry | undefined => {
  const row = asRecord(value);
  const botId = text(row.botId);
  if (!botId) return undefined;
  return { botId, role: text(row.role) };
};

export function parseTeamBot(value: unknown): TeamBotView {
  const row = asRecord(value);
  return {
    teamBotId: text(row.teamBotId),
    name: text(row.name),
    mission: text(row.mission),
    roster: list(row.roster).flatMap((item) => {
      const seat = parseRosterEntry(item);
      return seat ? [seat] : [];
    }),
    revision: count(row.revision),
    hash: text(row.hash),
    archived: bool(row.archived),
    createdAt: textOrUndefined(row.createdAt),
    updatedAt: textOrUndefined(row.updatedAt),
    ownerAccountId: textOrUndefined(row.ownerAccountId),
    sharedCount: countOrUndefined(row.sharedCount),
  };
}

/** One share tuple from the wire. An unknown relation/subject token is dropped
 * rather than rendered as a share the server did not confirm. */
export function parseShareView(value: unknown): ShareView | undefined {
  const row = asRecord(value);
  const subjectType = text(row.subjectType);
  const relation = text(row.relation);
  const subjectId = text(row.subjectId);
  if (
    (subjectType !== "account" && subjectType !== "group" && subjectType !== "team_bot") ||
    (relation !== "viewer" && relation !== "editor") ||
    !subjectId
  ) {
    return undefined;
  }
  return {
    subjectType,
    subjectId,
    relation,
    displayName: textOrUndefined(row.displayName),
    username: textOrUndefined(row.username),
  };
}

export function parseBotKey(value: unknown): BotKeyView {
  const row = asRecord(value);
  return {
    keyId: text(row.keyId),
    prefix: text(row.prefix),
    accountId: text(row.accountId),
    botId: text(row.botId),
    label: text(row.label),
    createdAt: textOrUndefined(row.createdAt),
    revokedAt: textOrUndefined(row.revokedAt),
    lastUsedAt: textOrUndefined(row.lastUsedAt),
  };
}

/** The whoami answer. The account block may be partial (an edge principal with
 * no account row resolved); the parser keeps what is there. */
export function parseWhoAmI(value: unknown): WhoAmI {
  const row = asRecord(value);
  const account = asRecord(row.account);
  const bot = asRecord(row.bot);
  return {
    tenantId: text(row.tenantId),
    accountId: text(account.id),
    username: textOrUndefined(account.username),
    displayName: textOrUndefined(account.displayName),
    kind: textOrUndefined(account.kind),
    disabled: bool(account.disabled),
    isSuperAdmin: bool(account.isSuperAdmin),
    botId: textOrUndefined(bot.id),
    keyId: textOrUndefined(row.keyId),
    keyPrefix: textOrUndefined(row.keyPrefix),
    permissions: parsePermissionMap(row.permissions),
  };
}

export function parseAuthSession(value: unknown): AuthSessionView {
  const row = asRecord(value);
  const account = asRecord(row.account);
  return {
    authenticated: bool(row.authenticated),
    mode: text(row.mode),
    bootstrapRequired: bool(row.bootstrapRequired),
    csrfToken: textOrUndefined(row.csrfToken),
    mustChangePassword: bool(row.mustChangePassword),
    tenantId: textOrUndefined(row.tenantId),
    accountId: textOrUndefined(account.id),
    username: textOrUndefined(account.username),
    displayName: textOrUndefined(account.displayName),
    isSuperAdmin: bool(account.isSuperAdmin),
    localAdmin: bool(row.localAdmin),
    botId: textOrUndefined(row.botId),
    sessionId: textOrUndefined(row.sessionId),
    expiresAt: textOrUndefined(row.expiresAt),
  };
}

/** One listing: `items` plus the limit it was read at. `hasMore` is
 * `items.length >= limit` — a full page may still be the last one, and the
 * page says so rather than claiming a total it was not given. */
export function identityPage<T>(items: unknown[], limit: number): IdentityPage<T> {
  return { items: items as T[], limit, hasMore: items.length >= limit };
}
