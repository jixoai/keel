// The 员工 / 团队 / 运行 state — change keel-bot-identity-access, A4.4b.
//
// One module owns what the 02 员工 and 03 团队 chapters read and write: the Bot
// index and one Bot (its harness draft, keys, try-run), the TeamBot index and
// one TeamBot (mission/roster draft, preflight, start), and the run projection
// built from the server's own session ledger.
//
// The server is authoritative in the same two ways the identity store states:
//   - every list/detail is the server's answer; a field it did not send stays
//     unknown (a Bot's 最近活动 is "未记录", not "just now");
//   - a draft is a local comparison against the last saved answer. A save sends
//     the diff, then re-reads the object, so the page shows the server's state
//     again.
//
// The run projection is deliberately a *projection*: it reads the captain
// session's events, the member task cards and the sub-agent registry, and draws
// only the nodes/edges those reads contain. Where a needed field has no route
// yet (a team run list, per-run cost and permission decisions, a Bot-scoped
// session index), the page says 待接入 and names the contract; it never fills the
// gap with a sample node.
import { signal } from "nsh";
import type { SessionSummary } from "./contract";
import { KeelApiError, errorMessageOf } from "./http";
import { can, canAny } from "./permissions";
import { store } from "./store";
import { identityAccess, type AccessFailure } from "./identity-access";
import { capabilityOf, isHighRisk, isKnownKey, type CapabilityRisk } from "./permission-catalog";
import { formatDuration, summaryOf } from "./wire";
import {
  eventPayload as payloadOfEvent,
  type ProfileView,
  type RoleView,
  type RunStartView,
  type SessionEventView,
  type SubagentView,
  type TeamTaskView,
} from "./bots-wire";
import type {
  BotKeyView,
  BotView,
  ModelRef,
  PermissionScope,
  RosterEntry,
  TeamBotView,
} from "./identity";
import type { IdentityApi, IssuedBotKeyView } from "./identity-api";
import type { SessionCancelView } from "./bots-wire";

const failureOf = (error: unknown): AccessFailure => {
  if (error instanceof KeelApiError) {
    const failure: AccessFailure = { status: error.status, message: errorMessageOf(error) };
    if (error.requiredKey) failure.requiredKey = error.requiredKey;
    if (error.requestId) failure.requestId = error.requestId;
    return failure;
  }
  return { status: null, message: errorMessageOf(error) };
};

const newRequestId = (): string => {
  const cryptoAny = globalThis.crypto as { randomUUID?: () => string } | undefined;
  if (cryptoAny?.randomUUID) return cryptoAny.randomUUID();
  return `${Date.now().toString(36)}${Math.random().toString(36).slice(2)}`;
};

const textOfError = (error: unknown): string => errorMessageOf(error);

const roleOfRoster = (role: string): "captain" | "member" => (role === "captain" ? "captain" : "member");

/** A one-line excerpt of a mission for a diff row. The full sentence stays in
 * the mission field; a diff must stay scannable. */
const snippet = (value: string): string => {
  const flat = value.replace(/\s+/g, " ").trim();
  if (!flat) return "空";
  return flat.length > 48 ? `${flat.slice(0, 47)}…` : flat;
};

const captainOf = (roster: RosterEntry[]): RosterEntry | undefined => roster.find((seat) => roleOfRoster(seat.role) === "captain");

const sameModel = (left: ModelRef | undefined, right: ModelRef | undefined): boolean =>
  (left?.providerId ?? "") === (right?.providerId ?? "") && (left?.modelId ?? "") === (right?.modelId ?? "");

const modelText = (model: ModelRef | undefined): string =>
  model && (model.providerId || model.modelId) ? `${model.providerId}/${model.modelId}` : "未设置";

// ---- the Bot's harness draft ----------------------------------------------

export interface BotDraft {
  modelKind: string;
  defaultModel: ModelRef;
  instruction: string;
  /** The strict allow-list. Empty means "no capability", not "inherit". */
  permissions: string[];
}

export type BotChangeKind = "edit" | "allow" | "narrow";

export interface BotChange {
  kind: BotChangeKind;
  /** `instruction` / `modelKind` / `defaultModel` / `permission`. */
  field: string;
  /** The permission key when `field` is `permission`. */
  key?: string;
  label: string;
  from: string;
  to: string;
  highRisk: boolean;
}

export interface BotOption {
  key: string;
  label: string;
  /** 常规 / 敏感 / 高危 — an independent scale, never the amber wash. */
  risk: CapabilityRisk;
  /** The owning account's effective scope for this key, when known. */
  accountScope?: PermissionScope;
  /** Is the key in the Bot's harness allow-list? */
  allowed: boolean;
  /** 可用 / 收窄 / 不可用. */
  effective: "usable" | "narrowed" | "missing";
  /** A catalogue key this build does not know: the save is refused until the
   * row is narrowed away. */
  unknown?: boolean;
}

// ---- the run projection ----------------------------------------------------

export type RunNodeStatus = "running" | "done" | "failed" | "stopped" | "pending" | "unknown";
export type RunEdgeKind = "mission" | "invoke" | "create" | "fork" | "unknown";

export interface RunNode {
  id: string;
  kind: "mission" | "captain" | "member";
  name: string;
  subtitle?: string;
  botId?: string;
  sessionId?: string;
  parentId?: string;
  depth: number;
  edge?: RunEdgeKind;
  status: RunNodeStatus;
  startedAt?: string;
  endedAt?: string;
  durationMs?: number;
  tokens?: number;
  model?: string;
  error?: string;
  summary?: string;
  /** The sub-agent registry's agent name, when it named one. */
  agent?: string;
  /** The Bot's owner account, when the Bot index could resolve it. */
  ownerAccountId?: string;
  source: "session" | "task" | "subagent";
}

export interface RunToolView {
  id: string;
  name: string;
  status: "running" | "ok" | "error";
  durationMs?: number;
  text?: string;
  startedAt?: string;
}

export interface RunPermissionView {
  key: string;
  label: string;
  decision: "denied" | "allowed";
  detail?: string;
}

export interface RunProjection {
  teamId: string;
  runId: string;
  teamName: string;
  mission: string;
  captainBotId?: string;
  captainName?: string;
  status: RunNodeStatus;
  title?: string;
  startedAt?: string;
  endedAt?: string;
  durationMs?: number;
  tokens?: number;
  model?: string;
  nodes: RunNode[];
  failures: RunNode[];
  /** Reads that answered; a false entry is a route/relation gap, not "empty". */
  sources: { session: boolean; tasks: boolean; subagents: boolean; events: boolean };
  gaps: string[];
}

/** A start receipt plus the moment this browser saw it. The server has no
 * team-run list route, so this is the only run name/history the page has. */
export interface StartedRun {
  start: RunStartView;
  startedAt: string;
}

const STATUS_LABEL: Record<RunNodeStatus, string> = {
  running: "运行中",
  done: "完成",
  failed: "失败",
  stopped: "已停止",
  pending: "等待中",
  unknown: "未知",
};

export const runStatusLabel = (status: RunNodeStatus): string => STATUS_LABEL[status];

const taskStatus = (status: string): RunNodeStatus => {
  switch (status) {
    case "in_progress":
      return "running";
    case "blocked":
      return "failed";
    case "done":
    case "closed":
      return "done";
    case "open":
      return "pending";
    default:
      return "unknown";
  }
};

const subagentStatus = (status: string): RunNodeStatus => {
  switch (status) {
    case "running":
    case "queued":
    case "pending":
      return "running";
    case "completed":
    case "done":
      return "done";
    case "failed":
    case "error":
      return "failed";
    case "cancelled":
    case "canceled":
    case "stopped":
    case "aborted":
      return "stopped";
    default:
      return "unknown";
  }
};

const sessionStatus = (status: string | undefined): RunNodeStatus => {
  switch ((status ?? "").toLowerCase()) {
    case "running":
    case "streaming":
      return "running";
    case "failed":
    case "error":
      return "failed";
    case "stopped":
    case "cancelled":
    case "canceled":
    case "interrupted":
      return "stopped";
    case "done":
    case "idle":
    case "completed":
      return "done";
    case "":
      return "unknown";
    default:
      return "unknown";
  }
};

const durationBetween = (start?: string, end?: string): number | undefined => {
  if (!start || !end) return undefined;
  const from = Date.parse(start);
  const to = Date.parse(end);
  if (!Number.isFinite(from) || !Number.isFinite(to) || to < from) return undefined;
  return to - from;
};

const tokensOfUsage = (usage: { inputTokens?: number; outputTokens?: number } | undefined): number | undefined => {
  if (!usage) return undefined;
  const input = usage.inputTokens ?? 0;
  const output = usage.outputTokens ?? 0;
  return input + output > 0 ? input + output : undefined;
};

const KEY_PATTERN = /\b([a-z][a-z0-9]*(?:_[a-z0-9]+)*\.[a-z][a-z0-9_.]*)\b/;
const DENIAL_PATTERN = /(denied|forbidden|permission|not allowed|not authorised|not authorized|missing|403|缺)/i;

/** The tools a session's ledger contains: tool calls from assistant messages
 * joined to their results. A call without a result stays running — the ledger
 * has no completion, so the page must not call it a success. */
function toolsOf(events: SessionEventView[]): RunToolView[] {
  const calls = new Map<string, RunToolView>();
  for (const event of events) {
    const payload = payloadOfEvent(event);
    if (event.kind === "assistant.message") {
      const content = Array.isArray(payload.content) ? (payload.content as Array<Record<string, unknown>>) : [];
      for (const block of content) {
        if (block.type !== "toolCall") continue;
        const id = typeof block.id === "string" && block.id ? block.id : `tool-${event.seq}-${calls.size}`;
        const name = typeof block.name === "string" && block.name ? block.name : "tool";
        const timestamp = typeof payload.timestamp === "number" ? new Date(payload.timestamp).toISOString() : undefined;
        calls.set(id, { id, name, status: "running", startedAt: timestamp });
      }
      continue;
    }
    if (event.kind !== "tool.result") continue;
    const id = typeof payload.toolCallId === "string" ? payload.toolCallId : "";
    const name = typeof payload.toolName === "string" && payload.toolName ? payload.toolName : calls.get(id)?.name ?? "tool";
    const text = typeof payload.content === "string"
      ? payload.content
      : Array.isArray(payload.content)
        ? (payload.content as Array<Record<string, unknown>>)
            .filter((block) => block.type === "text" && typeof block.text === "string")
            .map((block) => block.text as string)
            .join("")
        : "";
    calls.set(id || `${name}-${event.seq}`, {
      id: id || `${name}-${event.seq}`,
      name,
      status: payload.isError === true ? "error" : "ok",
      durationMs: typeof payload.durationMs === "number" ? payload.durationMs : undefined,
      text: text || undefined,
      startedAt: calls.get(id)?.startedAt,
    });
  }
  return [...calls.values()];
}

function usageOf(events: SessionEventView[]): { tokens?: number; durationMs?: number; model?: string; status?: RunNodeStatus; error?: string } {
  let finishedTokens = 0;
  let finishedDuration = 0;
  let sawFinished = false;
  let assistantTokens = 0;
  let model: string | undefined;
  let status: RunNodeStatus | undefined;
  let error: string | undefined;
  for (const event of events) {
    const payload = payloadOfEvent(event);
    if (event.kind === "run.finished") {
      const summary = summaryOf(payload);
      const total = summary.tokensIn + summary.tokensOut;
      sawFinished = true;
      finishedTokens += total;
      if (summary.durationMs > 0) finishedDuration += summary.durationMs;
      const provider = typeof payload.provider === "string" ? payload.provider : "";
      const modelId = typeof payload.model === "string" ? payload.model : "";
      if (provider || modelId) model = `${provider}${provider && modelId ? "/" : ""}${modelId}`;
      const stop = String(payload.stopReason ?? summary.stopReason ?? "").toLowerCase();
      status =
        stop === "error" || stop === "failed"
          ? "failed"
          : stop === "aborted" || stop === "cancelled" || stop === "canceled" || stop === "interrupted"
            ? "stopped"
            : "done";
      const errorMessage = typeof payload.errorMessage === "string" ? payload.errorMessage : undefined;
      if (errorMessage) error = errorMessage;
    }
    if (event.kind === "assistant.message") {
      const usage = payload.usage as { inputTokens?: number; outputTokens?: number } | undefined;
      const turnTokens = tokensOfUsage(usage);
      if (turnTokens !== undefined) assistantTokens += turnTokens;
      const errorMessage = typeof payload.errorMessage === "string" ? payload.errorMessage : undefined;
      if (errorMessage) error = errorMessage;
    }
  }
  // A finished run's totals already include its assistant turns; summing both
  // would double-count. Prefer the ledger's run totals, fall back to the turns.
  const tokens = sawFinished ? (finishedTokens > 0 ? finishedTokens : undefined) : assistantTokens > 0 ? assistantTokens : undefined;
  const durationMs = sawFinished && finishedDuration > 0 ? finishedDuration : undefined;
  return { tokens, durationMs, model, status, error };
}

/** A denied tool result is the only denial fact the ledger has today. The page
 * shows exactly that: the tool, its own text, and the key it named (when it
 * named one). It does not invent a missing permission per node. */
function denialsOf(tools: RunToolView[]): RunPermissionView[] {
  const denials: RunPermissionView[] = [];
  for (const tool of tools) {
    if (tool.status !== "error" || !tool.text || !DENIAL_PATTERN.test(tool.text)) continue;
    const found = KEY_PATTERN.exec(tool.text);
    const key = found?.[1] ?? "";
    denials.push({
      key: key || tool.name,
      label: key ? capabilityOf(key).displayName : tool.name,
      decision: "denied",
      detail: tool.text,
    });
  }
  return denials;
}

// ---- the store -------------------------------------------------------------

class BotAccessStore {
  // ---- the Bot index -------------------------------------------------------
  readonly bots = signal<BotView[]>([]);
  readonly botsLoading = signal(false);
  readonly botsError = signal<AccessFailure | null>(null);
  readonly botsHasMore = signal(false);
  readonly botsLimit = signal(100);
  readonly botsQuery = signal("");
  readonly botsModelKind = signal("all");
  readonly botsStatus = signal<"all" | "active" | "archived">("all");
  private botsAttempted = false;
  private botsToken = 0;
  /** The in-flight index read. Callers that must decide from the list (the
   * workspace "+") await this instead of sampling `botsLoading`, which would
   * read an empty list as "the tenant has no Bots". */
  private botsLoad: Promise<void> | null = null;

  // ---- one Bot -------------------------------------------------------------
  readonly botDetail = signal<BotView | null>(null);
  readonly botDetailId = signal<string | null>(null);
  readonly botDetailLoading = signal(false);
  readonly botDetailError = signal<AccessFailure | null>(null);
  readonly botDraft = signal<BotDraft | null>(null);
  readonly botSaved = signal<BotDraft | null>(null);
  readonly botSaving = signal(false);
  readonly botSaveError = signal<string | null>(null);
  readonly botActionError = signal<string | null>(null);
  readonly botNotice = signal<string | null>(null);
  private botToken = 0;

  // ---- keys ----------------------------------------------------------------
  readonly botKeys = signal<BotKeyView[]>([]);
  readonly botKeysLoading = signal(false);
  readonly botKeysError = signal<AccessFailure | null>(null);
  readonly botKeysLoaded = signal(false);
  readonly issuedKey = signal<IssuedBotKeyView | null>(null);
  readonly keyBusy = signal<string | null>(null);
  private botKeysToken = 0;

  // ---- try-run -------------------------------------------------------------
  readonly tryRunPhase = signal<"idle" | "running" | "done" | "error">("idle");
  readonly tryRunError = signal<string | null>(null);
  readonly tryRunResult = signal<RunStartView | null>(null);

  // ---- profiles / roles ----------------------------------------------------
  readonly profiles = signal<ProfileView[]>([]);
  readonly roles = signal<RoleView[]>([]);
  readonly profilesLoading = signal(false);
  readonly profilesError = signal<AccessFailure | null>(null);
  private profilesAttempted = false;
  private profilesToken = 0;

  // ---- the TeamBot index ---------------------------------------------------
  readonly teams = signal<TeamBotView[]>([]);
  readonly teamsLoading = signal(false);
  readonly teamsError = signal<AccessFailure | null>(null);
  readonly teamsHasMore = signal(false);
  readonly teamsLimit = signal(100);
  readonly teamsQuery = signal("");
  readonly teamsStatus = signal<"all" | "active" | "archived">("all");
  private teamsAttempted = false;
  private teamsToken = 0;

  // ---- one TeamBot ---------------------------------------------------------
  readonly teamDetail = signal<TeamBotView | null>(null);
  readonly teamDetailId = signal<string | null>(null);
  readonly teamDetailLoading = signal(false);
  readonly teamDetailError = signal<AccessFailure | null>(null);
  readonly teamDraft = signal<{ mission: string; roster: RosterEntry[] } | null>(null);
  readonly teamSaved = signal<{ mission: string; roster: RosterEntry[] } | null>(null);
  readonly teamSaving = signal(false);
  readonly teamSaveError = signal<string | null>(null);
  readonly teamActionError = signal<string | null>(null);
  readonly teamNotice = signal<string | null>(null);
  readonly teamStarting = signal(false);
  private teamToken = 0;

  /** Runs started in this browser during this launch. The server has no team
   * run-list route yet, so a start receipt is the only real run name the page
   * has; history beyond it is 待接入, never a guessed row. */
  readonly startedRuns = signal<Record<string, StartedRun[]>>({});

  // ---- the run projection --------------------------------------------------
  readonly run = signal<RunProjection | null>(null);
  readonly runLoading = signal(false);
  readonly runError = signal<AccessFailure | null>(null);
  readonly runSelected = signal<string | null>(null);
  readonly runMode = signal<"lanes" | "graph">("lanes");
  readonly runStopping = signal(false);
  readonly runStopError = signal<string | null>(null);
  readonly runStopNotice = signal<string | null>(null);
  readonly nodeEvents = signal<Record<string, SessionEventView[]>>({});
  readonly nodeEventsLoading = signal<string | null>(null);
  readonly nodeEventsError = signal<{ sessionId: string; message: string } | null>(null);
  private runToken = 0;
  private nodeToken = 0;
  /** A run page reads once per (team, run); an error stays an error until the
   * reader presses 重试 or 刷新. Without this, an effect that re-runs on every
   * render would retry a failed read forever. */
  private readonly runAttempted = new Set<string>();
  private readonly nodeEventsAttempted = new Set<string>();

  private api(): IdentityApi | null {
    return store.identityApi();
  }

  // ---- the Bot index -------------------------------------------------------

  async loadBots(): Promise<void> {
    // Concurrent callers share one read: a second list request while the first
    // is in flight would race the token guard and leave one caller with an
    // empty list.
    if (this.botsLoad) return this.botsLoad;
    this.botsLoad = this.doLoadBots().finally(() => {
      this.botsLoad = null;
    });
    return this.botsLoad;
  }

  private async doLoadBots(): Promise<void> {
    const api = this.api();
    const token = ++this.botsToken;
    if (!api?.listBots) {
      this.botsError.set({ status: null, message: "这个传输不提供 Bot 索引。", unavailable: true });
      return;
    }
    this.botsLoading.set(true);
    this.botsError.set(null);
    try {
      // Archived Bots stay readable: the list's 状态 filter is the reader's
      // tool for finding them, so the read includes them and the filter decides.
      const page = await api.listBots(this.botsLimit(), true);
      if (token !== this.botsToken) return;
      this.bots.set(page.items.map((bot) => ({ ...bot })));
      this.botsHasMore.set(page.hasMore);
      // The account names beside each Bot are the identity index's rows. A
      // reader without account.read keeps the raw accountId and the page says
      // why the name is missing; it is not an invented label.
      if (can("account.read") && identityAccess.accounts().length === 0 && !identityAccess.accountsLoading()) {
        void identityAccess.loadAccounts();
      }
    } catch (error) {
      if (token !== this.botsToken) return;
      this.botsError.set(failureOf(error));
    } finally {
      if (token === this.botsToken) this.botsLoading.set(false);
    }
  }

  async loadMoreBots(): Promise<void> {
    const next = Math.min(500, this.botsLimit() + 50);
    if (next === this.botsLimit() || this.botsLoading()) return;
    this.botsLimit.set(next);
    await this.loadBots();
  }

  ensureBots(): Promise<void> {
    if (this.botsAttempted) return this.botsLoad ?? Promise.resolve();
    this.botsAttempted = true;
    return this.loadBots();
  }

  visibleBots(): BotView[] {
    const query = this.botsQuery().trim().toLowerCase();
    const kind = this.botsModelKind();
    const status = this.botsStatus();
    return this.bots().filter((bot) => {
      if (kind !== "all" && bot.modelKind !== kind) return false;
      if (status === "active" && bot.archived) return false;
      if (status === "archived" && !bot.archived) return false;
      if (!query) return true;
      const account = this.accountNameOf(bot.accountId);
      return [bot.name, bot.botId, account, bot.modelKind, bot.defaultModel.modelId, bot.defaultModel.providerId]
        .filter(Boolean)
        .some((part) => part.toLowerCase().includes(query));
    });
  }

  accountNameOf(accountId: string | undefined): string {
    if (!accountId) return "";
    const account = identityAccess.accounts().find((row) => row.accountId === accountId);
    return account?.displayName || account?.username || "";
  }

  // ---- one Bot -------------------------------------------------------------

  async loadBot(botId: string): Promise<void> {
    const api = this.api();
    const token = ++this.botToken;
    this.botDetailId.set(botId);
    if (!api?.getBot) {
      this.botDetailError.set({ status: null, message: "这个传输不提供 Bot 详情。", unavailable: true });
      return;
    }
    this.botDetailLoading.set(true);
    this.botDetailError.set(null);
    this.botSaveError.set(null);
    this.botActionError.set(null);
    if (this.botDetail()?.botId !== botId) {
      this.botDetail.set(null);
      this.botDraft.set(null);
      this.botSaved.set(null);
      this.botKeys.set([]);
      this.botKeysLoaded.set(false);
      this.tryRunPhase.set("idle");
      this.tryRunResult.set(null);
      this.tryRunError.set(null);
    }
    try {
      const bot = await api.getBot(botId);
      if (token !== this.botToken) return;
      this.botDetail.set(bot);
      this.botDraft.set(draftOf(bot));
      this.botSaved.set(draftOf(bot));
      this.bots.update((rows) => rows.map((row) => (row.botId === bot.botId ? { ...row, ...bot } : row)));
      this.ensureProfiles();
      if (can("permission.read") && identityAccess.groups().length === 0 && !identityAccess.groupsLoading() && identityAccess.groupsError() === null) {
        void identityAccess.loadGroups();
      }
      if (can("account.read") && identityAccess.accounts().length === 0 && !identityAccess.accountsLoading()) {
        void identityAccess.loadAccounts();
      }
    } catch (error) {
      if (token !== this.botToken) return;
      this.botDetailError.set(failureOf(error));
      this.botDetail.set(null);
    } finally {
      if (token === this.botToken) this.botDetailLoading.set(false);
    }
  }

  /** The Bot's Profile (Account + Role) and the Role documents. A reader without
   * profile.read gets the server's 403; the page then shows the Bot's own
   * accountId and says the persona was not readable. */
  ensureProfiles(): void {
    if (this.profilesAttempted) return;
    this.profilesAttempted = true;
    const api = this.api();
    if (!api?.listProfiles) {
      this.profilesError.set({ status: null, message: "这个传输不提供身份档案。", unavailable: true });
      return;
    }
    const token = ++this.profilesToken;
    this.profilesLoading.set(true);
    this.profilesError.set(null);
    void (async () => {
      try {
        const page = await api.listProfiles!(500);
        if (token !== this.profilesToken) return;
        this.profiles.set(page.items);
        if (api.listRoles) {
          try {
            const roles = await api.listRoles(500);
            if (token === this.profilesToken) this.roles.set(roles.items);
          } catch {
            /* the persona name is an enhancement; its absence is visible */
          }
        }
      } catch (error) {
        if (token !== this.profilesToken) return;
        this.profilesError.set(failureOf(error));
      } finally {
        if (token === this.profilesToken) this.profilesLoading.set(false);
      }
    })();
  }

  profileOf(bot: BotView | null): ProfileView | undefined {
    if (!bot) return undefined;
    return this.profiles().find((profile) => profile.profileId === bot.profileId);
  }

  roleOf(profile: ProfileView | undefined): RoleView | undefined {
    if (!profile) return undefined;
    return this.roles().find((role) => role.roleId === profile.roleId);
  }

  /** The owning account's effective scope for one key (the union of its groups,
   * the server's rule). `undefined` means either the account has no grant or the
   * group details were not readable; the page distinguishes the two by checking
   * `identityAccess.groupsError()`. */
  accountScopeOf(accountId: string | undefined, key: string): PermissionScope | undefined {
    if (!accountId) return undefined;
    const account = identityAccess.accounts().find((row) => row.accountId === accountId);
    if (account?.isSuperAdmin) return "all";
    let scope: PermissionScope | undefined;
    for (const group of identityAccess.groups()) {
      const detail = identityAccess.groupDetailOf(group.groupId);
      if (!detail) continue;
      if (!detail.members.some((member) => member.accountId === accountId)) continue;
      for (const grant of detail.grants) {
        if (grant.key !== key) continue;
        if (scope === undefined || rank(grant.scope) > rank(scope)) scope = grant.scope;
      }
    }
    return scope;
  }

  /** The account range is knowable when the group details were read, or when
   * the account is the deployment administrator (the server short-circuits it
   * to every key). */
  accountScopeKnown(accountId?: string): boolean {
    const account = accountId ? identityAccess.accounts().find((row) => row.accountId === accountId) : undefined;
    if (account?.isSuperAdmin) return true;
    return identityAccess.groupsError() === null && identityAccess.groups().length > 0;
  }

  // ---- the Bot harness draft ----------------------------------------------

  setBotField(field: "modelKind" | "instruction", value: string): void {
    this.botDraft.update((draft) => (draft ? { ...draft, [field]: value } : draft));
  }

  setBotDefaultModel(model: ModelRef): void {
    this.botDraft.update((draft) => (draft ? { ...draft, defaultModel: { ...model } } : draft));
  }

  setHarnessAllowed(key: string, allowed: boolean): void {
    this.botDraft.update((draft) => {
      if (!draft) return draft;
      const permissions = draft.permissions.filter((entry) => entry !== key);
      if (allowed) permissions.push(key);
      return { ...draft, permissions };
    });
  }

  botChanges(): BotChange[] {
    const saved = this.botSaved();
    const draft = this.botDraft();
    if (!saved || !draft) return [];
    const changes: BotChange[] = [];
    if (saved.instruction !== draft.instruction) {
      changes.push({
        kind: "edit",
        field: "instruction",
        label: "工作说明",
        from: saved.instruction ? "已填写" : "空",
        to: draft.instruction ? "已修改" : "清空",
        highRisk: false,
      });
    }
    if (saved.modelKind !== draft.modelKind) {
      changes.push({
        kind: "edit",
        field: "modelKind",
        label: "模型类型",
        from: saved.modelKind || "未设置",
        to: draft.modelKind || "未设置",
        highRisk: false,
      });
    }
    if (!sameModel(saved.defaultModel, draft.defaultModel)) {
      changes.push({
        kind: "edit",
        field: "defaultModel",
        label: "默认模型",
        from: modelText(saved.defaultModel),
        to: modelText(draft.defaultModel),
        highRisk: false,
      });
    }
    const savedSet = new Set(saved.permissions);
    const draftSet = new Set(draft.permissions);
    for (const key of draftSet) {
      if (savedSet.has(key)) continue;
      changes.push({
        kind: "allow",
        field: "permission",
        key,
        label: capabilityOf(key).displayName,
        from: "收窄",
        to: "允许",
        highRisk: isHighRisk(key),
      });
    }
    for (const key of savedSet) {
      if (draftSet.has(key)) continue;
      changes.push({
        kind: "narrow",
        field: "permission",
        key,
        label: capabilityOf(key).displayName,
        from: "允许",
        to: "收窄",
        highRisk: false,
      });
    }
    return changes;
  }

  unknownDraftKeys(): string[] {
    return (this.botDraft()?.permissions ?? []).filter((key) => !isKnownKey(key));
  }

  discardBotDraft(): void {
    this.botDraft.set(this.botSaved());
    this.botSaveError.set(null);
  }

  async saveBotDraft(): Promise<{ ok: boolean; applied: number; total: number }> {
    const api = this.api();
    const bot = this.botDetail();
    const draft = this.botDraft();
    const changes = this.botChanges();
    if (!api || !bot || !draft || !changes.length) return { ok: true, applied: 0, total: 0 };
    const unknown = this.unknownDraftKeys();
    if (unknown.length) {
      this.botSaveError.set(
        `这些 key 不在当前能力目录里，服务端会拒绝保存：${unknown.join("、")}。先把对应的行改为「收窄」移除它们。`,
      );
      return { ok: false, applied: 0, total: changes.length };
    }
    if (!api.updateBot) {
      this.botSaveError.set("这个传输不支持保存 Bot。");
      return { ok: false, applied: 0, total: changes.length };
    }
    this.botSaving.set(true);
    this.botSaveError.set(null);
    try {
      const updated = await api.updateBot(bot.botId, {
        modelKind: draft.modelKind,
        defaultModel: draft.defaultModel,
        harness: {
          tools: bot.harness.tools,
          context: { instruction: draft.instruction, permissions: draft.permissions },
        },
        expectedArchived: bot.archived,
      });
      this.botDetail.set(updated);
      this.botDraft.set(draftOf(updated));
      this.botSaved.set(draftOf(updated));
      this.bots.update((rows) => rows.map((row) => (row.botId === updated.botId ? { ...row, ...updated } : row)));
      this.botNotice.set(`已登记 ${changes.length} 项变更 · 已写入记录 05。`);
      return { ok: true, applied: changes.length, total: changes.length };
    } catch (error) {
      const failure = failureOf(error);
      this.botSaveError.set(
        failure.status === 409
          ? `这个 Bot 刚被改过（版本冲突）。先刷新，再核对你的修改，避免覆盖对方的改动。`
          : failure.message,
      );
      await this.loadBot(bot.botId);
      // Keep the reader's intent after the re-read so a retry sends only what is
      // left; the diff is computed against the server's fresh state.
      this.botDraft.set(draft);
      return { ok: false, applied: 0, total: changes.length };
    } finally {
      this.botSaving.set(false);
    }
  }

  /** The five-column capability table's rows. The default is the Bot-shaped
   * slice of the catalogue; `showAll` adds every other key the account holds so
   * a grant never hides behind a curated list. A harness key this build does not
   * know is always included, with `unknown: true`, so it can be narrowed away. */
  botCapabilityRows(showAll: boolean, query: string): BotOption[] {
    const bot = this.botDetail();
    if (!bot) return [];
    const allowed = new Set(this.botDraft()?.permissions ?? []);
    const keys = new Set(BOT_CAPABILITY_KEYS);
    if (showAll) {
      for (const key of Object.keys(identityAccessEffective(this.botDetail()?.accountId))) keys.add(key);
      for (const key of allowed) keys.add(key);
    }
    for (const key of allowed) {
      if (!isKnownKey(key)) keys.add(key);
    }
    const trimmed = query.trim().toLowerCase();
    const rows: BotOption[] = [];
    for (const key of keys) {
      const spec = capabilityOf(key);
      const accountScope = this.accountScopeOf(bot.accountId, key);
      const isAllowed = allowed.has(key);
      const unknown = !isKnownKey(key);
      const effective: BotOption["effective"] =
        accountScope === undefined ? "missing" : isAllowed ? "usable" : "narrowed";
      if (
        trimmed &&
        ![spec.displayName, key, spec.domain].some((part) => part.toLowerCase().includes(trimmed))
      ) {
        continue;
      }
      rows.push({
        key,
        label: spec.displayName,
        risk: spec.risk,
        accountScope,
        allowed: isAllowed,
        effective,
        unknown,
      });
    }
    return rows.sort((left, right) => {
      const leftIndex = BOT_CAPABILITY_KEYS.indexOf(left.key);
      const rightIndex = BOT_CAPABILITY_KEYS.indexOf(right.key);
      return (leftIndex < 0 ? 999 : leftIndex) - (rightIndex < 0 ? 999 : rightIndex);
    });
  }

  // ---- Bot keys ------------------------------------------------------------

  async loadBotKeys(botId: string): Promise<void> {
    const api = this.api();
    const token = ++this.botKeysToken;
    if (!api?.listBotKeys) {
      this.botKeysError.set({ status: null, message: "这个传输不提供 Bot 密钥。", unavailable: true });
      this.botKeysLoaded.set(true);
      return;
    }
    this.botKeysLoading.set(true);
    this.botKeysError.set(null);
    try {
      const keys = await api.listBotKeys(botId);
      if (token !== this.botKeysToken) return;
      this.botKeys.set(keys);
      this.botKeysLoaded.set(true);
    } catch (error) {
      if (token !== this.botKeysToken) return;
      this.botKeysError.set(failureOf(error));
      this.botKeys.set([]);
      this.botKeysLoaded.set(true);
    } finally {
      if (token === this.botKeysToken) this.botKeysLoading.set(false);
    }
  }

  async issueBotKey(botId: string, label: string): Promise<boolean> {
    const api = this.api();
    if (!api?.issueBotKey) {
      this.botActionError.set("这个传输不支持签发密钥。");
      return false;
    }
    this.keyBusy.set("issue");
    this.botActionError.set(null);
    try {
      const issued = await api.issueBotKey(botId, label.trim());
      this.issuedKey.set(issued);
      await this.loadBotKeys(botId);
      return true;
    } catch (error) {
      this.botActionError.set(textOfError(error));
      return false;
    } finally {
      this.keyBusy.set(null);
    }
  }

  closeIssuedKey(): void {
    this.issuedKey.set(null);
  }

  async revokeBotKey(botId: string, keyId: string): Promise<boolean> {
    const api = this.api();
    if (!api?.revokeBotKey) {
      this.botActionError.set("这个传输不支持吊销密钥。");
      return false;
    }
    this.keyBusy.set(keyId);
    this.botActionError.set(null);
    try {
      await api.revokeBotKey(botId, keyId);
      await this.loadBotKeys(botId);
      this.botNotice.set("密钥已吊销。使用它的触发器或 Webhook 会在下一次调用时失败。");
      return true;
    } catch (error) {
      this.botActionError.set(textOfError(error));
      return false;
    } finally {
      this.keyBusy.set(null);
    }
  }

  // ---- archive / rebind / create ------------------------------------------

  async archiveBot(botId: string): Promise<boolean> {
    const api = this.api();
    if (!api?.archiveBot) {
      this.botActionError.set("这个传输不支持归档 Bot。");
      return false;
    }
    this.botActionError.set(null);
    try {
      await api.archiveBot(botId);
      this.botNotice.set("Bot 已归档：它不能再新建会话或签发密钥，历史会话仍然可读。");
      await this.loadBot(botId);
      await this.loadBots();
      return true;
    } catch (error) {
      this.botActionError.set(textOfError(error));
      return false;
    }
  }

  async rebindProfile(botId: string, profileId: string): Promise<boolean> {
    const api = this.api();
    if (!api?.updateBot) {
      this.botActionError.set("这个传输不支持改绑身份档案。");
      return false;
    }
    this.botActionError.set(null);
    try {
      await api.updateBot(botId, { profileId });
      this.botNotice.set("已改绑身份档案：Bot 的归属账号随之改变，下一次调用按新账号的范围执行。");
      await this.loadBot(botId);
      await this.loadBots();
      return true;
    } catch (error) {
      this.botActionError.set(humanWriteError(error, "改绑"));
      return false;
    }
  }

  async createBot(input: {
    name: string;
    profileId: string;
    modelKind: string;
    defaultModel?: ModelRef;
  }): Promise<BotView | null> {
    const api = this.api();
    if (!api?.createBot) {
      this.botActionError.set("这个传输不支持创建 Bot。");
      return null;
    }
    this.botActionError.set(null);
    try {
      const bot = await api.createBot({
        name: input.name,
        profileId: input.profileId,
        modelKind: input.modelKind,
        ...(input.defaultModel ? { defaultModel: input.defaultModel } : {}),
      });
      await this.loadBots();
      return bot;
    } catch (error) {
      this.botActionError.set(humanWriteError(error, "创建"));
      return null;
    }
  }

  // ---- try-run -------------------------------------------------------------

  async tryRun(botId: string, text: string): Promise<RunStartView | null> {
    const api = this.api();
    if (!api?.invokeBot) {
      this.tryRunPhase.set("error");
      this.tryRunError.set("这个部署没有接上 Bot 调用接口。");
      return null;
    }
    this.tryRunPhase.set("running");
    this.tryRunError.set(null);
    this.tryRunResult.set(null);
    try {
      const result = await api.invokeBot(botId, text, undefined, newRequestId());
      this.tryRunPhase.set("done");
      this.tryRunResult.set(result);
      return result;
    } catch (error) {
      const failure = failureOf(error);
      this.tryRunPhase.set("error");
      this.tryRunError.set(
        failure.status === 403
          ? `你不能调用这个 Bot。需要它的归属者授予你「调用 Bot」能力${failure.requiredKey ? `（${failure.requiredKey}）` : ""}。`
          : failure.status === 409
            ? "这个 Bot 已归档，不能再开始新的会话。"
            : failure.status === 404
              ? "没有找到这个 Bot，或者你没有访问权限。"
              : failure.status === 501 || failure.status === 503
                ? "这个部署还没有接上 Bot 运行器（服务端 501/503）。"
                : `这次试跑没有开始：${failure.message}`,
      );
      return null;
    }
  }

  resetTryRun(): void {
    this.tryRunPhase.set("idle");
    this.tryRunError.set(null);
    this.tryRunResult.set(null);
  }

  // ---- the TeamBot index ---------------------------------------------------

  async loadTeams(): Promise<void> {
    const api = this.api();
    const token = ++this.teamsToken;
    if (!api?.listTeamBots) {
      this.teamsError.set({ status: null, message: "这个传输不提供团队索引。", unavailable: true });
      return;
    }
    this.teamsLoading.set(true);
    this.teamsError.set(null);
    try {
      const page = await api.listTeamBots(this.teamsLimit(), true);
      if (token !== this.teamsToken) return;
      this.teams.set(page.items.map((team) => ({ ...team })));
      this.teamsHasMore.set(page.hasMore);
      this.ensureBots();
    } catch (error) {
      if (token !== this.teamsToken) return;
      this.teamsError.set(failureOf(error));
    } finally {
      if (token === this.teamsToken) this.teamsLoading.set(false);
    }
  }

  async loadMoreTeams(): Promise<void> {
    const next = Math.min(500, this.teamsLimit() + 50);
    if (next === this.teamsLimit() || this.teamsLoading()) return;
    this.teamsLimit.set(next);
    await this.loadTeams();
  }

  ensureTeams(): void {
    if (this.teamsAttempted) return;
    this.teamsAttempted = true;
    void this.loadTeams();
  }

  visibleTeams(): TeamBotView[] {
    const query = this.teamsQuery().trim().toLowerCase();
    const status = this.teamsStatus();
    return this.teams().filter((team) => {
      if (status === "active" && team.archived) return false;
      if (status === "archived" && !team.archived) return false;
      if (!query) return true;
      return [team.name, team.mission, team.teamBotId]
        .filter(Boolean)
        .some((part) => part.toLowerCase().includes(query));
    });
  }

  captainOfTeam(team: TeamBotView | null): BotView | undefined {
    if (!team) return undefined;
    const seat = captainOf(team.roster);
    if (!seat) return undefined;
    return this.bots().find((bot) => bot.botId === seat.botId);
  }

  recentRunOf(teamId: string): StartedRun | undefined {
    return this.startedRuns()[teamId]?.[0];
  }

  // ---- one TeamBot ---------------------------------------------------------

  async loadTeam(teamId: string): Promise<void> {
    const api = this.api();
    const token = ++this.teamToken;
    this.teamDetailId.set(teamId);
    if (!api?.getTeamBot) {
      this.teamDetailError.set({ status: null, message: "这个传输不提供团队详情。", unavailable: true });
      return;
    }
    this.teamDetailLoading.set(true);
    this.teamDetailError.set(null);
    this.teamSaveError.set(null);
    this.teamActionError.set(null);
    if (this.teamDetail()?.teamBotId !== teamId) {
      this.teamDetail.set(null);
      this.teamDraft.set(null);
      this.teamSaved.set(null);
    }
    try {
      const team = await api.getTeamBot(teamId);
      if (token !== this.teamToken) return;
      this.teamDetail.set(team);
      const draft = { mission: team.mission, roster: team.roster.map((seat) => ({ ...seat })) };
      this.teamDraft.set(draft);
      this.teamSaved.set({ mission: draft.mission, roster: draft.roster.map((seat) => ({ ...seat })) });
      this.teams.update((rows) => rows.map((row) => (row.teamBotId === team.teamBotId ? { ...row, ...team } : row)));
      this.ensureBots();
    } catch (error) {
      if (token !== this.teamToken) return;
      this.teamDetailError.set(failureOf(error));
      this.teamDetail.set(null);
    } finally {
      if (token === this.teamToken) this.teamDetailLoading.set(false);
    }
  }

  setTeamMission(mission: string): void {
    this.teamDraft.update((draft) => (draft ? { ...draft, mission } : draft));
  }

  addRosterMember(botId: string): void {
    this.teamDraft.update((draft) => {
      if (!draft) return draft;
      if (draft.roster.some((seat) => seat.botId === botId)) return draft;
      return { ...draft, roster: [...draft.roster, { botId, role: "member" }] };
    });
  }

  removeRosterMember(botId: string): void {
    this.teamDraft.update((draft) => {
      if (!draft) return draft;
      return { ...draft, roster: draft.roster.filter((seat) => seat.botId !== botId) };
    });
  }

  /** A roster has exactly one captain: setting a new one demotes the old seat to
   * `member` in the same write. */
  setCaptain(botId: string): void {
    this.teamDraft.update((draft) => {
      if (!draft) return draft;
      const roster = draft.roster.map((seat) =>
        seat.botId === botId ? { ...seat, role: "captain" } : { ...seat, role: "member" },
      );
      return { ...draft, roster };
    });
  }

  teamChanges(): TeamChange[] {
    const saved = this.teamSaved();
    const draft = this.teamDraft();
    if (!saved || !draft) return [];
    const changes: TeamChange[] = [];
    if (saved.mission !== draft.mission) {
      changes.push({
        kind: "mission",
        label: "使命",
        from: snippet(saved.mission),
        to: snippet(draft.mission),
        highRisk: false,
      });
    }
    const savedCaptain = captainOf(saved.roster)?.botId ?? "";
    const draftCaptain = captainOf(draft.roster)?.botId ?? "";
    if (savedCaptain !== draftCaptain) {
      changes.push({
        kind: "captain",
        label: "队长",
        memberId: draftCaptain,
        from: this.botNameOf(savedCaptain) || "未设置",
        to: this.botNameOf(draftCaptain) || "未设置",
        highRisk: true,
      });
    }
    const savedIds = new Set(saved.roster.map((seat) => seat.botId));
    const draftIds = new Set(draft.roster.map((seat) => seat.botId));
    for (const botId of draftIds) {
      if (savedIds.has(botId)) continue;
      changes.push({
        kind: "add",
        label: "成员",
        memberId: botId,
        from: "不在名单",
        to: this.botNameOf(botId) || "已加入",
        highRisk: false,
      });
    }
    for (const botId of savedIds) {
      if (draftIds.has(botId)) continue;
      changes.push({
        kind: "remove",
        label: "成员",
        memberId: botId,
        from: this.botNameOf(botId) || "在名单",
        to: "已移除",
        highRisk: false,
      });
    }
    return changes;
  }

  discardTeamDraft(): void {
    this.teamDraft.set(this.teamSaved());
    this.teamSaveError.set(null);
  }

  async saveTeamDraft(): Promise<{ ok: boolean; applied: number; total: number }> {
    const api = this.api();
    const team = this.teamDetail();
    const draft = this.teamDraft();
    const changes = this.teamChanges();
    if (!api || !team || !draft || !changes.length) return { ok: true, applied: 0, total: 0 };
    if (!api.updateTeamBot) {
      this.teamSaveError.set("这个传输不支持保存团队。");
      return { ok: false, applied: 0, total: changes.length };
    }
    const intended = { mission: draft.mission, roster: draft.roster.map((seat) => ({ ...seat })) };
    this.teamSaving.set(true);
    this.teamSaveError.set(null);
    try {
      const updated = await api.updateTeamBot(team.teamBotId, {
        mission: draft.mission,
        roster: draft.roster,
        expectedRevision: team.revision,
      });
      this.teamDetail.set(updated);
      this.teamDraft.set({ mission: updated.mission, roster: updated.roster.map((seat) => ({ ...seat })) });
      this.teamSaved.set({ mission: updated.mission, roster: updated.roster.map((seat) => ({ ...seat })) });
      this.teams.update((rows) => rows.map((row) => (row.teamBotId === updated.teamBotId ? { ...row, ...updated } : row)));
      this.teamNotice.set(`已登记 ${changes.length} 项变更 · 影响下一次启动，不影响正在运行的会话。`);
      return { ok: true, applied: changes.length, total: changes.length };
    } catch (error) {
      const failure = failureOf(error);
      this.teamSaveError.set(
        failure.status === 409
          ? "这个团队刚被改过（版本已更新）。先刷新，再核对你的修改，避免覆盖对方的改动。"
          : failure.message,
      );
      await this.loadTeam(team.teamBotId);
      this.teamDraft.set(intended);
      return { ok: false, applied: 0, total: changes.length };
    } finally {
      this.teamSaving.set(false);
    }
  }

  // ---- preflight -----------------------------------------------------------

  /** The three checks the start button depends on. A check that cannot be
   * decided from the reads names the missing fact; it is never shown as green. */
  preflight(): PreflightRow[] {
    const draft = this.teamDraft();
    const mission = draft?.mission.trim() ?? "";
    const roster = draft?.roster ?? [];
    const captain = captainOf(roster);
    const members = roster.filter((seat) => seat.botId !== captain?.botId);
    const missionRow: PreflightRow = {
      id: "mission",
      ok: mission.length > 0,
      title: mission.length > 0 ? "使命已填写" : "使命还没有填写",
      detail: mission.length > 0 ? "每次成员会话都会带上这句使命。" : "先写一句使命：目标、边界、什么时候升级。",
    };
    const captainBot = captain ? this.bots().find((bot) => bot.botId === captain.botId) : undefined;
    const captainProblem = !captain
      ? "还没有队长。先指定一个成员当队长。"
      : !captainBot
        ? "队长不在可见 Bot 里（可能已归档，或你没有查看权）。"
        : captainBot.archived
          ? "队长已归档，不能启动。"
          : !captainBot.accountId
            ? "队长的身份档案缺失，无法确定归属账号。"
            : "";
    const captainRow: PreflightRow = {
      id: "captain",
      ok: captainProblem === "",
      title: captainProblem === "" ? "已选队长" : captainProblem,
      detail:
        captainProblem === ""
          ? `${captainBot?.name ?? this.botNameOf(captain?.botId ?? "")} · 负责任务拆分与调用`
          : "队长负责拆分任务、调用成员；它仍以自己的权限执行。",
    };
    const memberProblems: string[] = [];
    if (!can("bot.invoke")) {
      memberProblems.push("缺少「调用 Bot」能力");
    }
    for (const seat of roster) {
      const bot = this.bots().find((entry) => entry.botId === seat.botId);
      const name = bot?.name || seat.botId;
      if (!bot) memberProblems.push(`${name} 不在可见 Bot 里`);
      else if (bot.archived) memberProblems.push(`${name} 已归档`);
      else if (!bot.accountId) memberProblems.push(`${name} 的身份档案缺失`);
    }
    const membersRow: PreflightRow = {
      id: "members",
      ok: memberProblems.length === 0 && roster.length > 0,
      title:
        roster.length === 0
          ? "还没有成员"
          : memberProblems.length === 0
            ? "所有成员都可调用"
            : `${memberProblems.length} 个成员不能调用`,
      detail:
        roster.length === 0
          ? "先加一个队长，再添加成员。"
          : memberProblems.length === 0
            ? "启动时服务端会再核对一次每个人的「调用 Bot」能力。"
            : `${memberProblems.join("；")}。成员的归属者可以授予「调用 Bot」。`,
    };
    return [missionRow, captainRow, membersRow];
  }

  preflightPass(): boolean {
    return this.preflight().every((row) => row.ok);
  }

  blockingReason(): string {
    const row = this.preflight().find((entry) => !entry.ok);
    return row ? `${row.title}。${row.detail}` : "";
  }

  // ---- start / archive -----------------------------------------------------

  async startTeam(teamId: string): Promise<RunStartView | null> {
    const api = this.api();
    if (!api?.startTeamBot) {
      this.teamActionError.set("这个传输不支持启动团队。");
      return null;
    }
    this.teamStarting.set(true);
    this.teamActionError.set(null);
    this.teamNotice.set(null);
    try {
      const result = await api.startTeamBot(teamId, undefined, undefined, newRequestId());
      // A new run with a previously seen session id must be read again: the
      // page's one-shot guard and the old projection both belong to the old run.
      this.runAttempted.delete(`${teamId}:${result.sessionId}`);
      this.run.set(null);
      this.runSelected.set(null);
      this.nodeEvents.set({});
      this.nodeEventsAttempted.clear();
      this.runStopNotice.set(null);
      this.runStopError.set(null);
      this.startedRuns.update((map) => ({
        ...map,
        [teamId]: [{ start: { ...result }, startedAt: new Date().toISOString() }, ...(map[teamId] ?? [])].slice(0, 20),
      }));
      this.teamNotice.set("团队已启动。队长正在准备，运行页面随事件更新。");
      return result;
    } catch (error) {
      const failure = failureOf(error);
      this.teamActionError.set(
        failure.status === 403
          ? `启动被拒：${failure.requiredKey ? `缺「${capabilityOf(failure.requiredKey).displayName}」能力` : "你对队长或成员没有调用权"}。让对象的归属者授予「调用 Bot」后再试。`
          : failure.status === 409
            ? "启动被拒：队长或成员已归档，或者队长被撤权。先检查名单。"
            : failure.status === 501 || failure.status === 503
              ? "这个部署还没有接上团队运行器（服务端 501/503）。"
              : `启动没有成功：${failure.message}`,
      );
      return null;
    } finally {
      this.teamStarting.set(false);
    }
  }

  async archiveTeam(teamId: string): Promise<boolean> {
    const api = this.api();
    if (!api?.archiveTeamBot) {
      this.teamActionError.set("这个传输不支持归档团队。");
      return false;
    }
    this.teamActionError.set(null);
    try {
      await api.archiveTeamBot(teamId);
      this.teamNotice.set("团队已归档：不能再启动，历史记录仍然保留。");
      await this.loadTeam(teamId);
      await this.loadTeams();
      return true;
    } catch (error) {
      this.teamActionError.set(textOfError(error));
      return false;
    }
  }

  async createTeam(input: { name: string; mission: string; roster: RosterEntry[] }): Promise<TeamBotView | null> {
    const api = this.api();
    if (!api?.createTeamBot) {
      this.teamActionError.set("这个传输不支持创建团队。");
      return null;
    }
    this.teamActionError.set(null);
    try {
      const created = await api.createTeamBot(input);
      await this.loadTeams();
      return created;
    } catch (error) {
      this.teamActionError.set(humanWriteError(error, "创建团队"));
      return null;
    }
  }

  botNameOf(botId: string): string {
    return this.bots().find((bot) => bot.botId === botId)?.name ?? botId;
  }

  /** The 待我处理 queue this chapter can prove from its own reads: a team whose
   * roster names a Bot that is archived or not visible. A run failure and a
   * missing `bot.invoke` need a run list / relation read the server does not
   * expose yet, so they are not counted here (an invented number is worse than
   * a short queue). */
  todoItems(): Array<{ teamId: string; teamName: string; reason: string }> {
    const items: Array<{ teamId: string; teamName: string; reason: string }> = [];
    for (const team of this.teams()) {
      if (team.archived) continue;
      for (const seat of team.roster) {
        const bot = this.bots().find((entry) => entry.botId === seat.botId);
        if (!bot) {
          items.push({ teamId: team.teamBotId, teamName: team.name, reason: "名单里的一个 Bot 不在可见范围里" });
          break;
        }
        if (bot.archived) {
          items.push({ teamId: team.teamBotId, teamName: team.name, reason: `${bot.name} 已归档，团队不能启动` });
          break;
        }
      }
    }
    return items;
  }

  // ---- the run projection --------------------------------------------------

  ensureRun(teamId: string, runId: string): void {
    const key = `${teamId}:${runId}`;
    const current = this.run();
    if (current && current.teamId === teamId && current.runId === runId) return;
    if (this.runAttempted.has(key)) return;
    if (this.runLoading()) return;
    this.runAttempted.add(key);
    void this.loadRun(teamId, runId);
  }

  async loadRun(teamId: string, runId: string): Promise<void> {
    const api = this.api();
    const token = ++this.runToken;
    this.runLoading.set(true);
    this.runError.set(null);
    if (this.run() !== null && this.run()?.runId !== runId) {
      this.runStopError.set(null);
      this.runStopNotice.set(null);
    }
    this.runSelected.set(null);
    this.nodeEvents.set({});
    this.run.set(null);
    if (!api) {
      this.runError.set({ status: null, message: "这个传输不提供团队运行接口。", unavailable: true });
      this.runLoading.set(false);
      return;
    }
    try {
      if (this.teamDetail()?.teamBotId !== teamId) await this.loadTeam(teamId);
      if (token !== this.runToken) return;
      const team = this.teamDetail();
      if (!team) throw new KeelApiError(404, "没有找到这个团队。");

      const gaps: string[] = [];
      const sources = { session: false, tasks: false, subagents: false, events: false };
      let summary = store.sessions().find((row) => row.sessionId === runId);
      if (!summary && api.listSessions) {
        // A deep link may land before the workspace list has been read. One
        // bounded read gives the run its own summary; a failure stays a gap.
        try {
          summary = (await api.listSessions(200)).find((row) => row.sessionId === runId);
        } catch {
          /* the run's events and tasks still carry the projection */
        }
      }
      sources.session = summary !== undefined;
      const localStart = (this.startedRuns()[teamId] ?? []).find((row) => row.start.sessionId === runId);
      let events: SessionEventView[] = [];
      if (api.sessionEvents) {
        try {
          events = await api.sessionEvents(runId, 500);
          sources.events = true;
          if (events.length >= 500) gaps.push("会话事件达到 500 条上限：更早的事件没有读入，工具与用量可能不完整。");
          this.nodeEvents.update((map) => ({ ...map, [runId]: events }));
        } catch (error) {
          const failure = failureOf(error);
          if (failure.status !== 403 && failure.status !== 404) throw error;
          gaps.push("会话事件读不到：换成有这次会话查看关系的人，或让归属者共享会话。");
        }
      } else {
        gaps.push("需要 GET /v1/sessions/{id}/events 才能读出工具、用量与停止原因。");
      }
      if (token !== this.runToken) return;

      let tasks: TeamTaskView[] = [];
      if (api.teamTasks) {
        try {
          const page = await api.teamTasks(runId);
          tasks = page.tasks ?? [];
          if (page.hasMore === true) gaps.push("成员任务卡还有下一页：当前只列了第一页。");
          sources.tasks = true;
        } catch (error) {
          const failure = failureOf(error);
          if (failure.status !== 403 && failure.status !== 404) throw error;
          gaps.push("成员任务卡读不到：缺这次会话的查看关系。");
        }
      } else {
        gaps.push("需要 GET /v1/sessions/{id}/team-tasks 才能画出成员任务。");
      }
      if (token !== this.runToken) return;

      let subagents: SubagentView[] = [];
      if (api.listSubagents) {
        try {
          const all = await api.listSubagents(undefined, 200);
          if (all.length >= 200) gaps.push("子会话清单达到 200 条上限：更早的调用可能没有画出来。");
          subagents = subagentClosure(all, runId);
          sources.subagents = true;
        } catch {
          try {
            subagents = await api.listSubagents(runId, 200);
            sources.subagents = true;
          } catch (error) {
            const failure = failureOf(error);
            if (failure.status !== 403 && failure.status !== 404) throw error;
            gaps.push("子会话注册表读不到：缺这次会话的查看关系。");
          }
        }
        // The registry is a warm in-process index: after a server restart it is
        // empty even though the delegation tree is durable. The projection read
        // (rootSessionId) answers the same tree from the spawn links; records
        // carry depth/parent/status but not the task text.
        if (subagents.length === 0 && api.listSubagentTree) {
          try {
            const durable = await api.listSubagentTree(runId, 200);
            if (durable.length > 0) {
              subagents = durable;
              gaps.push("子会话注册表已冷：树来自持久化派生子链接，任务文本与用量需打开子会话查看。");
            }
          } catch (error) {
            const failure = failureOf(error);
            if (failure.status !== 403 && failure.status !== 404) throw error;
          }
        }
      } else {
        gaps.push("需要 GET /v1/subagents 才能画出 create/fork/invoke 边。");
      }
      if (token !== this.runToken) return;

      const captainSeat = captainOf(team.roster);
      const captainBot = captainSeat ? this.bots().find((bot) => bot.botId === captainSeat.botId) : undefined;
      const captainUsage = usageOf(events);
      const worked = buildRun({
        team,
        runId,
        summary,
        localStart: localStart?.start,
        localStartedAt: localStart?.startedAt,
        events,
        tasks,
        subagents,
        captainBot,
        accountNameOf: (accountId) => this.accountNameOf(accountId),
      });
      if (!summary) gaps.push("会话列表没有这次运行的摘要：状态与时间来自事件账本和本次启动回执。");
      if (summary && !("botId" in summary)) {
        gaps.push("会话摘要不返回 botId / teamBotId：运行归属由团队名单与子会话记录还原。");
      }
      gaps.push("成本字段未返回：会话账本只给 tokens 与时长；需要 run.cost 才能在老虎机上显示金额。");
      gaps.push("权限判定还没有一等读法：检查器只显示工具结果里的拒绝原文；需要 run.permission.decisions。");
      gaps.push("团队运行历史没有列表接口：这里只列本次浏览器启动的运行；需要 GET /v1/team-bots/{id}/runs。");
      const nodeTokens = worked.nodes.reduce((sum, node) => sum + (node.tokens ?? 0), 0);
      const projection: RunProjection = {
        ...worked,
        sources,
        tokens: captainUsage.tokens ?? (nodeTokens > 0 ? nodeTokens : undefined),
        model: captainUsage.model ?? (captainBot?.defaultModel ? modelText(captainBot.defaultModel) : undefined),
        gaps: dedupe([...worked.gaps, ...gaps]),
      };
      this.run.set(projection);
      const failure = projection.failures[0];
      this.runSelected.set(failure?.id ?? projection.nodes.find((node) => node.kind === "captain")?.id ?? null);
    } catch (error) {
      if (token !== this.runToken) return;
      this.runError.set(failureOf(error));
      this.run.set(null);
    } finally {
      if (token === this.runToken) this.runLoading.set(false);
    }
  }

  runNode(nodeId: string): RunNode | undefined {
    return this.run()?.nodes.find((node) => node.id === nodeId);
  }

  async ensureNodeEvents(node: RunNode): Promise<void> {
    const sessionId = node.sessionId;
    if (!sessionId) return;
    if (this.nodeEvents()[sessionId] || this.nodeEventsAttempted.has(sessionId)) return;
    const api = this.api();
    if (!api?.sessionEvents) return;
    this.nodeEventsAttempted.add(sessionId);
    const token = ++this.nodeToken;
    this.nodeEventsLoading.set(sessionId);
    this.nodeEventsError.set(null);
    try {
      const events = await api.sessionEvents(sessionId, 500);
      if (token !== this.nodeToken) return;
      this.nodeEvents.update((map) => ({ ...map, [sessionId]: events }));
    } catch (error) {
      if (token !== this.nodeToken) return;
      this.nodeEventsError.set({ sessionId, message: textOfError(error) });
    } finally {
      if (token === this.nodeToken) this.nodeEventsLoading.set(null);
    }
  }

  /** The inspector's 重试: one explicit re-read of a failed node ledger. */
  retryNodeEvents(node: RunNode): void {
    if (!node.sessionId) return;
    this.nodeEventsAttempted.delete(node.sessionId);
    void this.ensureNodeEvents(node);
  }

  nodeTools(node: RunNode): RunToolView[] {
    const sessionId = node.sessionId;
    if (!sessionId) return [];
    const events = this.nodeEvents()[sessionId];
    if (!events) return [];
    return toolsOf(events);
  }

  nodeDenials(node: RunNode): RunPermissionView[] {
    return denialsOf(this.nodeTools(node));
  }

  /** The stop action: cancel the captain session. The server's 501 is a real
   * answer (the cancel seam is not wired) and is shown as such. */
  async stopRun(runId: string): Promise<SessionCancelView | null> {
    const api = this.api();
    if (!api?.cancelSession) {
      this.runStopError.set("这个传输不提供停止接口。");
      return null;
    }
    this.runStopping.set(true);
    this.runStopError.set(null);
    this.runStopNotice.set(null);
    try {
      const result = await api.cancelSession(runId);
      this.runStopNotice.set(
        result.cancelled
          ? "已发出停止信号：队长与成员会收到停止；已完成的会话保留。"
          : result.detail || "服务端接受了停止请求，但没有报告已取消的会话。",
      );
      return result;
    } catch (error) {
      const failure = failureOf(error);
      this.runStopError.set(
        failure.status === 501
          ? "这个部署还没有接上停止接口（服务端 501）。运行不会被假装停止。"
          : failure.status === 409
            ? "这个会话由外部后端运行，没有可停止的原生运行。"
            : `停止没有成功：${failure.message}`,
      );
      return null;
    } finally {
      this.runStopping.set(false);
    }
  }

  // ---- invalidation stream ------------------------------------------------

  /** Re-read the Bot/TeamBot projections an invalidation fact names (change
   * keel-harness-v2's identity stream). Only views the reader already loaded
   * are refreshed; a chapter that was never opened stays quiet. */
  async refreshForEvent(kinds: ReadonlySet<string>): Promise<void> {
    const bots = kinds.has("bots");
    const teams = kinds.has("teams");
    if (bots && can("bot.read") && (this.botsAttempted || this.bots().length > 0)) {
      await this.loadBots();
    }
    if (bots && can("bot.read")) {
      const botId = this.botDetailId();
      if (botId && this.botDetail()) {
        await this.loadBot(botId);
        if (this.botKeysLoaded() && can("bot.manage")) await this.loadBotKeys(botId);
      }
    }
    if (teams && can("team_bot.read") && (this.teamsAttempted || this.teams().length > 0)) {
      await this.loadTeams();
    }
    if (teams && can("team_bot.read")) {
      const teamId = this.teamDetailId();
      if (teamId && this.teamDetail()) await this.loadTeam(teamId);
    }
    // A role/profile change alters the persona names beside a Bot; profiles are
    // only re-read when that projection had been loaded.
    if (kinds.has("permissions") && this.profilesAttempted && canAny(["profile.read", "role.read"])) {
      this.profilesAttempted = false;
      this.ensureProfiles();
    }
  }

  // ---- boot / clear --------------------------------------------------------

  clearSession(): void {
    this.botsToken += 1;
    this.botToken += 1;
    this.botKeysToken += 1;
    this.profilesToken += 1;
    this.teamsToken += 1;
    this.teamToken += 1;
    this.runToken += 1;
    this.nodeToken += 1;
    this.bots.set([]);
    this.botsError.set(null);
    this.botsLoading.set(false);
    this.botsLimit.set(100);
    this.botsAttempted = false;
    this.teams.set([]);
    this.teamsError.set(null);
    this.teamsLoading.set(false);
    this.teamsLimit.set(100);
    this.teamsAttempted = false;
    this.botDetail.set(null);
    this.botDetailError.set(null);
    this.botDetailLoading.set(false);
    this.botDraft.set(null);
    this.botSaved.set(null);
    this.botSaving.set(false);
    this.botSaveError.set(null);
    this.botActionError.set(null);
    this.botNotice.set(null);
    this.botKeys.set([]);
    this.botKeysError.set(null);
    this.botKeysLoading.set(false);
    this.botKeysLoaded.set(false);
    this.issuedKey.set(null);
    this.keyBusy.set(null);
    this.tryRunPhase.set("idle");
    this.tryRunError.set(null);
    this.tryRunResult.set(null);
    this.profiles.set([]);
    this.roles.set([]);
    this.profilesError.set(null);
    this.profilesLoading.set(false);
    this.profilesAttempted = false;
    this.teamDetail.set(null);
    this.teamDetailError.set(null);
    this.teamDetailLoading.set(false);
    this.teamDraft.set(null);
    this.teamSaved.set(null);
    this.teamSaving.set(false);
    this.teamSaveError.set(null);
    this.teamActionError.set(null);
    this.teamNotice.set(null);
    this.teamStarting.set(false);
    this.startedRuns.set({});
    this.run.set(null);
    this.runError.set(null);
    this.runLoading.set(false);
    this.runSelected.set(null);
    this.runMode.set("lanes");
    this.runStopping.set(false);
    this.runStopError.set(null);
    this.runStopNotice.set(null);
    this.nodeEvents.set({});
    this.nodeEventsLoading.set(null);
    this.nodeEventsError.set(null);
    this.runAttempted.clear();
    this.nodeEventsAttempted.clear();
    this.botsQuery.set("");
    this.botsModelKind.set("all");
    this.botsStatus.set("all");
    this.teamsQuery.set("");
    this.teamsStatus.set("all");
  }

  resetForTest(): void {
    this.clearSession();
  }
}

/** The keys the Bot page shows by default: the capabilities a Bot actually
 * meets in a run (sessions, bots, teams, keys). Everything else is reachable
 * through 「完整清单」 so a grant never hides. */
const BOT_CAPABILITY_KEYS = [
  "session.read",
  "session.write",
  "session.delete",
  "session.share",
  "bot.read",
  "bot.manage",
  "bot.bind",
  "bot.invoke",
  "team_bot.read",
  "team_bot.manage",
  "account.read",
  "secret.read.metadata",
  "secret.read.value",
];

const rank = (scope: PermissionScope): number => ["own", "team", "tenant", "all"].indexOf(scope);

/** The keys an account holds, used only by the Bot page's full list. */
const identityAccessEffective = (accountId: string | undefined): Record<string, PermissionScope> => {
  const map: Record<string, PermissionScope> = {};
  if (!accountId) return map;
  for (const group of identityAccess.groups()) {
    const detail = identityAccess.groupDetailOf(group.groupId);
    if (!detail) continue;
    if (!detail.members.some((member) => member.accountId === accountId)) continue;
    for (const grant of detail.grants) {
      if (!map[grant.key] || rank(grant.scope) > rank(map[grant.key])) map[grant.key] = grant.scope;
    }
  }
  return map;
};

const draftOf = (bot: BotView): BotDraft => ({
  modelKind: bot.modelKind,
  defaultModel: { ...bot.defaultModel },
  instruction: bot.harness.context.instruction,
  permissions: [...bot.harness.context.permissions],
});

const humanWriteError = (error: unknown, action: string): string => {
  const failure = failureOf(error);
  if (failure.status === 403) {
    return `${action}被拒：${
      failure.requiredKey ? `缺「${capabilityOf(failure.requiredKey).displayName}」能力` : "你的账号没有这个操作的能力"
    }。找对象归属者或超级管理员。`;
  }
  if (failure.status === 404) return `${action}失败：没有找到这个对象，或者你没有访问权限。`;
  if (failure.status === 409) return `${action}失败：对象刚被改过或已归档；刷新后再试。`;
  if (failure.status === 400) return `${action}失败：${failure.message}`;
  return `${action}失败：${failure.message}`;
};

/** The sub-agent node's one-line subtitle. `depth` is the collaboration depth
 * (main is 0); a fork shows its edge instead because it does not consume depth. */
function subagentSubtitle(record: SubagentView): string {
  if (record.origin === "fork") return "分叉 · 子会话";
  if (typeof record.depth === "number" && record.depth > 0) {
    return record.maxDepth ? `调用 · depth ${record.depth}/${record.maxDepth}` : `调用 · depth ${record.depth}`;
  }
  return "调用 · 子会话";
}

/** The sub-agent closure rooted at one captain session. A record outside the
 * closure belongs to another run; drawing it here would invent a node. */
function subagentClosure(records: SubagentView[], rootSessionId: string): SubagentView[] {
  const included = new Map<string, SubagentView>();
  let grew = true;
  while (grew) {
    grew = false;
    for (const record of records) {
      if (included.has(record.sessionId)) continue;
      const parent = record.parentSessionId;
      if (parent !== rootSessionId && !included.has(parent)) continue;
      included.set(record.sessionId, record);
      grew = true;
    }
  }
  return [...included.values()];
}

function dedupe(values: string[]): string[] {
  return [...new Set(values.filter((value) => value.trim() !== ""))];
}

export interface PreflightRow {
  id: "mission" | "captain" | "members";
  ok: boolean;
  title: string;
  detail: string;
}

export interface TeamChange {
  kind: "mission" | "captain" | "add" | "remove";
  label: string;
  memberId?: string;
  from: string;
  to: string;
  highRisk: boolean;
}

/** Build the run projection from the three reads. The function is pure so the
 * shape can be reasoned about (and tested) without a transport. */
function buildRun(input: {
  team: TeamBotView;
  runId: string;
  summary?: SessionSummary;
  localStart?: RunStartView;
  localStartedAt?: string;
  events: SessionEventView[];
  tasks: TeamTaskView[];
  subagents: SubagentView[];
  captainBot?: BotView;
  accountNameOf: (accountId: string | undefined) => string;
}): Omit<RunProjection, "gaps" | "tokens" | "model" | "sources"> & { gaps: string[] } {
  const { team, runId, summary, localStart, localStartedAt, events, tasks, subagents, captainBot } = input;
  const nodes = new Map<string, RunNode>();
  const captainSeat = captainOf(team.roster);
  const captainName = captainBot?.name ?? (captainSeat ? team.name : "队长");
  const captainUsage = usageOf(events);
  const summaryStatus = summary ? sessionStatus(summary.runStatus) : undefined;
  const captainStatus = captainUsage.status ?? summaryStatus ?? (localStart ? "running" : "unknown");
  const startedAt = summary?.createdAt ?? eventIso(events[0]) ?? localStartedAt;
  const endedAt = captainStatus === "running" ? undefined : summary?.updatedAt ?? eventIso(events[events.length - 1]);
  const liveDuration =
    captainStatus === "running" && startedAt && Number.isFinite(Date.parse(startedAt))
      ? Math.max(0, Date.now() - Date.parse(startedAt))
      : undefined;
  const captainId = `captain:${captainSeat?.botId ?? "captain"}`;
  nodes.set(captainId, {
    id: captainId,
    kind: "captain",
    name: captainName,
    subtitle: [captainSeat ? "队长" : "运行发起", captainBot ? input.accountNameOf(captainBot.accountId) : ""]
      .filter(Boolean)
      .join(" · "),
    botId: captainSeat?.botId,
    sessionId: runId,
    depth: 1,
    edge: "mission",
    status: captainStatus,
    startedAt,
    endedAt,
    durationMs: captainUsage.durationMs ?? durationBetween(startedAt, endedAt) ?? liveDuration,
    tokens: captainUsage.tokens,
    model: captainUsage.model,
    error: captainUsage.error,
    ownerAccountId: captainBot?.accountId,
    source: "session",
  });

  const taskBySession = new Map<string, TeamTaskView>();
  const taskByTaskId = new Map<string, TeamTaskView>();
  for (const task of tasks) {
    if (task.sessionId) taskBySession.set(task.sessionId, task);
    if (task.taskId) taskByTaskId.set(task.taskId, task);
  }

  for (const task of tasks) {
    const parentTask = task.parentTaskId ? taskByTaskId.get(task.parentTaskId) : undefined;
    const parentNode = parentTask ? nodes.get(`task:${parentTask.taskId}`) : undefined;
    const parentId = parentNode ? parentNode.id : captainId;
    const id = `task:${task.taskId}`;
    const status = taskStatus(task.status);
    nodes.set(id, {
      id,
      kind: "member",
      name: task.title || "成员任务",
      subtitle: "成员任务卡",
      sessionId: task.sessionId || undefined,
      parentId,
      depth: (parentNode?.depth ?? 1) + 1,
      edge: "create",
      status,
      startedAt: task.createdAt,
      endedAt: status === "running" || status === "pending" ? undefined : task.updatedAt,
      durationMs: status === "running" || status === "pending" ? undefined : durationBetween(task.createdAt, task.updatedAt),
      error: status === "failed" ? "任务卡被阻塞（服务端未返回原因）" : undefined,
      source: "task",
    });
  }

  for (const record of subagents) {
    const existing =
      [...nodes.values()].find((node) => node.sessionId === record.sessionId) ??
      (taskBySession.get(record.sessionId) ? nodes.get(`task:${taskBySession.get(record.sessionId)!.taskId}`) : undefined);
    const parent =
      [...nodes.values()].find((node) => node.sessionId === record.parentSessionId) ??
      (taskBySession.get(record.parentSessionId)
        ? nodes.get(`task:${taskBySession.get(record.parentSessionId)!.taskId}`)
        : undefined);
    const status = subagentStatus(record.status);
    const usageTokens = tokensOfUsage(record.usage);
    const edge: RunEdgeKind = record.origin === "fork" ? "fork" : record.origin === "spawn" ? "invoke" : "unknown";
    if (existing) {
      nodes.set(existing.id, {
        ...existing,
        status: existing.status === "failed" ? existing.status : status,
        edge: existing.edge ?? edge,
        parentId: existing.parentId ?? parent?.id ?? captainId,
        depth: existing.depth,
        startedAt: existing.startedAt ?? record.startedAt,
        endedAt: record.endedAt ?? existing.endedAt,
        durationMs: existing.durationMs ?? record.usage?.durationMs ?? durationBetween(record.startedAt, record.endedAt),
        tokens: existing.tokens ?? usageTokens,
        error: record.error ?? existing.error,
        summary: record.summary ?? existing.summary,
        agent: record.agent,
        source: existing.source,
      });
      continue;
    }
    const id = `sub:${record.sessionId}`;
    nodes.set(id, {
      id,
      kind: "member",
      name: record.agent || record.task || "子会话",
      subtitle: subagentSubtitle(record),
      sessionId: record.sessionId,
      parentId: parent?.id ?? captainId,
      depth: (parent?.depth ?? 1) + 1,
      edge,
      status,
      startedAt: record.startedAt,
      endedAt: record.endedAt,
      durationMs: record.usage?.durationMs ?? durationBetween(record.startedAt, record.endedAt),
      tokens: usageTokens,
      error: record.error,
      summary: record.summary,
      agent: record.agent,
      source: "subagent",
    });
  }

  const all = [...nodes.values()].sort((left, right) => {
    if (left.kind === "captain" && right.kind !== "captain") return -1;
    if (right.kind === "captain" && left.kind !== "captain") return 1;
    const leftTime = left.startedAt ? Date.parse(left.startedAt) : Number.POSITIVE_INFINITY;
    const rightTime = right.startedAt ? Date.parse(right.startedAt) : Number.POSITIVE_INFINITY;
    return leftTime - rightTime;
  });
  const failures = all.filter((node) => node.status === "failed");
  return {
    teamId: team.teamBotId,
    runId,
    teamName: team.name,
    mission: team.mission,
    captainBotId: captainSeat?.botId,
    captainName,
    status: captainStatus,
    title: summary?.title,
    startedAt,
    endedAt,
    durationMs: captainUsage.durationMs ?? durationBetween(startedAt, endedAt) ?? liveDuration,
    nodes: all,
    failures,
    gaps: [],
  };
}

const eventIso = (event: SessionEventView | undefined): string | undefined => {
  if (!event) return undefined;
  const payload = payloadOfEvent(event);
  const timestamp = payload.timestamp;
  if (typeof timestamp !== "number" || !Number.isFinite(timestamp)) return undefined;
  return new Date(timestamp < 1e12 ? timestamp * 1000 : timestamp).toISOString();
};

export const botAccess = new BotAccessStore();
export const formatRunDuration = (ms: number | undefined): string => formatDuration(ms) || "—";
