// The board's client state — keel-board-v1 task 14.1/14.2/14.3.
//
// One normalized cache (channels, messages, threads, tasks, actors, labels,
// needs, claims) fed by three reads: the task list, a thread read, and the SSE
// feed. The server's projection is the only task truth; this module never
// folds the event log. Its jobs are:
//
//   - merge server views by id, de-duplicating the feed by cursor;
//   - reconnect with Last-Event-ID and refetch the open page/thread;
//   - hold the claim token in memory only (never localStorage);
//   - run one write at a time per task, with an optimistic face and rollback;
//   - expose sync/watcher/schedule reads with their five states.
//
// The UI imports this module; the store owns the transport seam
// (`store.boardApi()`), and a harness answers it in memory.
import { signal } from "nsh";
import type {
  BoardActorView,
  BoardApi,
  BoardCache,
  BoardChannelView,
  BoardClaimView,
  BoardEventView,
  BoardLiveStatus,
  BoardMeView,
  BoardMessageView,
  BoardScheduleView,
  BoardSyncCredentialView,
  BoardSyncEntryView,
  BoardSyncFailure,
  BoardSyncInboxItem,
  BoardSyncStatusView,
  BoardTaskClaim,
  BoardTaskQuery,
  BoardTaskView,
  BoardThreadView,
  BoardWatcherDelivery,
  BoardWatcherView,
} from "./board-wire";
import {
  emptyBoardCache,
  mergeActors,
  mergeChannels,
  mergeMessage,
  mergeTask,
  mergeThread,
  parseMessage,
  parseTasksPage,
  parseThreadView,
  taskQueryParams,
} from "./board-wire";
import { KeelApiError } from "./http";
import { can } from "./permissions";
import { store } from "./store";

// ---- vocabulary -----------------------------------------------------------

export type BoardStateTab =
  | "open"
  | "waiting"
  | "ready"
  | "claimed"
  | "blocked"
  | "completed"
  | "failed"
  | "canceled"
  | "all";

/** The state tabs, in the order the queue shows them. */
export const BOARD_STATE_TABS: BoardStateTab[] = [
  "open",
  "waiting",
  "ready",
  "claimed",
  "blocked",
  "completed",
  "failed",
  "canceled",
  "all",
];

export const BOARD_STATE_TAB_LABEL: Record<BoardStateTab, string> = {
  open: "打开",
  waiting: "等待依赖",
  ready: "可认领",
  claimed: "已认领",
  blocked: "待回复",
  completed: "已完成",
  failed: "已失败",
  canceled: "已取消",
  all: "全部",
};

/** The seven states plus the queue's two synthetic tabs. An unknown token is
 * printed verbatim instead of being folded into a safe-sounding label. */
const BOARD_STATE_LABEL: Record<string, string> = {
  waiting: "等待依赖",
  ready: "可认领",
  claimed: "已认领",
  blocked: "待回复",
  completed: "已完成",
  failed: "已失败",
  canceled: "已取消",
};

export const boardStateLabel = (state: string | undefined): string =>
  state ? (BOARD_STATE_LABEL[state] ?? state) : "未知";

export type BoardTone = "secondary" | "warning" | "info" | "success" | "destructive" | "outline";

export const boardStateTone = (state: string | undefined): BoardTone => {
  switch (state) {
    case "ready":
      return "info";
    case "claimed":
      return "success";
    case "blocked":
      return "warning";
    case "waiting":
      return "outline";
    case "completed":
      return "success";
    case "failed":
      return "destructive";
    case "canceled":
      return "secondary";
    default:
      return "secondary";
  }
};

/** Every state also has a glyph: colour is never the only channel. */
export const boardStateIcon = (state: string | undefined): "clock" | "check" | "alert" | "inbox" | "lock" | "x" => {
  switch (state) {
    case "ready":
      return "inbox";
    case "claimed":
      return "lock";
    case "blocked":
      return "alert";
    case "waiting":
      return "clock";
    case "completed":
      return "check";
    case "failed":
      return "alert";
    case "canceled":
      return "x";
    default:
      return "clock";
  }
};

/** The effective state: a claim whose lease lapsed reads as ready, because the
 * next claim may take it. This is presentation only; writes are still fenced
 * by the server. */
export function effectiveTaskState(task: BoardTaskView, now: number): string {
  if (task.state === "claimed" && task.claim) {
    const until = Date.parse(task.claim.until);
    if (Number.isFinite(until) && until <= now) return "ready";
  }
  return task.state;
}

export const terminalState = (state: string): boolean => state === "completed" || state === "failed" || state === "canceled";

const OPEN_STATES = new Set(["waiting", "ready", "claimed", "blocked"]);

/** Is this task open (one of the four working states, including a lapsed
 * claim)? */
export const openTask = (task: BoardTaskView, now: number): boolean => OPEN_STATES.has(effectiveTaskState(task, now));

// ---- the query ------------------------------------------------------------

/** The filter bar's state. The URL (web) and the TUI both write this object;
 * there is no second source. */
export interface BoardQuery {
  state: BoardStateTab;
  channel: string;
  labelKey: string;
  labelValue: string;
  owner: string;
  /** "all" (no filter), "pool" (no assignee), or an actor id. */
  for: string;
  archived: boolean;
}

export const defaultBoardQuery = (): BoardQuery => ({
  state: "open",
  channel: "",
  labelKey: "",
  labelValue: "",
  owner: "",
  for: "all",
  archived: false,
});

export function queryToTaskQuery(query: BoardQuery, after?: string, limit?: number): BoardTaskQuery {
  // board v0.1.1 has no `archived` query parameter. Archived tasks are the
  // server-maintained `board.archived=true` label, so the server filters and
  // paginates them natively; the client still checks task.archived as well.
  const labels: Array<{ key: string; value?: string }> = [];
  if (query.labelKey) {
    labels.push({ key: query.labelKey, ...(query.labelValue ? { value: query.labelValue } : {}) });
  }
  if (query.archived) labels.push({ key: "board.archived", value: "true" });
  return {
    state: query.state,
    ...(query.channel ? { channel: query.channel } : {}),
    ...(labels.length ? { labels } : {}),
    ...(query.owner ? { owner: query.owner } : {}),
    ...(query.for !== "all" && query.for !== "pool" ? { for: query.for } : {}),
    ...(after ? { after } : {}),
    ...(limit ? { limit } : {}),
  };
}

/** The board path's search, the web URL's form (`?state=ready&channel=work`). */
export function boardSearchOf(query: BoardQuery): string {
  const params: string[] = [];
  if (query.state !== "open") params.push(`state=${encodeURIComponent(query.state)}`);
  if (query.channel) params.push(`channel=${encodeURIComponent(query.channel)}`);
  if (query.labelKey) {
    params.push(`label=${encodeURIComponent(query.labelValue ? `${query.labelKey}=${query.labelValue}` : query.labelKey)}`);
  }
  if (query.owner) params.push(`owner=${encodeURIComponent(query.owner)}`);
  if (query.for !== "all") params.push(`for=${encodeURIComponent(query.for)}`);
  if (query.archived) params.push("archived=1");
  return params.join("&");
}

/** Parse a URL search (or the TUI's own string) into a query. Unknown values
 * stay out; the defaults are explicit. */
export function boardQueryFromSearch(search: string): BoardQuery {
  const query = defaultBoardQuery();
  const trimmed = search.startsWith("?") ? search.slice(1) : search;
  if (!trimmed) return query;
  for (const pair of trimmed.split("&")) {
    if (!pair) continue;
    const index = pair.indexOf("=");
    const rawKey = index >= 0 ? pair.slice(0, index) : pair;
    const rawValue = index >= 0 ? pair.slice(index + 1) : "";
    let key = rawKey;
    let value = rawValue;
    try {
      key = decodeURIComponent(rawKey);
    } catch {
      /* keep the raw key */
    }
    try {
      value = decodeURIComponent(rawValue);
    } catch {
      /* keep the raw value */
    }
    switch (key) {
      case "state":
        if ((BOARD_STATE_TABS as string[]).includes(value)) query.state = value as BoardStateTab;
        break;
      case "channel":
        query.channel = value;
        break;
      case "label": {
        const cut = value.indexOf("=");
        if (cut >= 0) {
          query.labelKey = value.slice(0, cut);
          query.labelValue = value.slice(cut + 1);
        } else {
          query.labelKey = value;
          query.labelValue = "";
        }
        break;
      }
      case "owner":
        query.owner = value;
        break;
      case "for":
        query.for = value || "all";
        break;
      case "archived":
        query.archived = value === "1" || value === "true";
        // Archived rows are terminal: the archive view is `state=all` unless
        // the URL names another state explicitly.
        if (query.archived && !trimmed.includes("state=")) query.state = "all";
        break;
      default:
        break;
    }
  }
  return query;
}

export const emptyQuery = (query: BoardQuery): boolean =>
  query.state === "open" &&
  !query.channel &&
  !query.labelKey &&
  !query.owner &&
  query.for === "all" &&
  !query.archived;

/** A human summary of the active filters (the empty state's copy and the
 * filter bar's badge). */
export function boardQuerySummary(query: BoardQuery, actorName?: (id: string) => string): string {
  const parts: string[] = [];
  if (query.channel) parts.push(`频道 ${query.channel}`);
  if (query.labelKey) parts.push(`标签 ${boardLabelFilterTitle(query.labelKey, query.labelValue)}`);
  if (query.owner) parts.push(`创建者 ${actorName ? actorName(query.owner) : query.owner}`);
  if (query.for !== "all") parts.push(query.for === "pool" ? "池中" : `指派 ${actorName ? actorName(query.for) : query.for}`);
  if (query.archived) parts.push("归档");
  return parts.join(" · ");
}

export const activeFilterCount = (query: BoardQuery): number =>
  (query.channel ? 1 : 0) +
  (query.labelKey ? 1 : 0) +
  (query.owner ? 1 : 0) +
  (query.for !== "all" ? 1 : 0) +
  (query.archived ? 1 : 0);

// ---- queue rows -----------------------------------------------------------

export interface BoardQueueRow {
  id: string;
  channel: string;
  message: BoardMessageView;
  task: BoardTaskView;
  state: string;
  labels: Record<string, string>;
  replies: number;
  last?: BoardMessageView;
}

/** The row a task root reads as. A root without a task is not a queue row. */
export function queueRowOf(cache: BoardCache, id: string): BoardQueueRow | undefined {
  const message = cache.messages[id];
  const task = cache.tasks[id];
  if (!message || !task) return undefined;
  const thread = cache.threads[id];
  return {
    id,
    channel: message.channel,
    message,
    task,
    state: task.state,
    labels: message.labels ?? {},
    replies: thread?.replyCount ?? thread?.replies.length ?? 0,
    ...(thread?.replies.length ? { last: thread.replies[thread.replies.length - 1] } : {}),
  };
}

const channelMatches = (candidate: string, filter: string): boolean =>
  candidate === filter || candidate.startsWith(filter.endsWith("/") ? filter : `${filter}/`);

/** Does a row belong to the current query? The server already filtered the
 * page; this keeps live events and client-only filters (pool, archive) honest. */
export function matchesBoardQuery(cache: BoardCache, row: BoardQueueRow, query: BoardQuery, now: number): boolean {
  const state = effectiveTaskState(row.task, now);
  if (query.state === "open") {
    if (!OPEN_STATES.has(state)) return false;
  } else if (query.state !== "all" && state !== query.state) {
    return false;
  }
  if (query.archived) {
    if (row.task.archived !== true) return false;
  } else if (row.task.archived === true) {
    return false;
  }
  if (query.channel && !channelMatches(row.channel, query.channel)) return false;
  if (query.labelKey) {
    const labels = { ...(row.labels ?? {}), ...(row.message.labels ?? {}) };
    if (labels[query.labelKey] !== query.labelValue) return false;
  }
  if (query.owner && row.task.owner !== query.owner && row.task.by !== query.owner) return false;
  if (query.for !== "all") {
    if (query.for === "pool") {
      if (row.task.for) return false;
    } else if (!row.task.for || row.task.for !== query.for) {
      return false;
    }
  }
  // A needs comparison is a server filter; a `needs` query is not exposed by
  // the filter bar.
  return true;
}

/** The visible queue in server order (oldest first; never re-sorted). */
export function visibleQueue(cache: BoardCache, ids: string[], query: BoardQuery, now: number): BoardQueueRow[] {
  const rows: BoardQueueRow[] = [];
  for (const id of ids) {
    const row = queueRowOf(cache, id);
    if (!row) continue;
    if (!matchesBoardQuery(cache, row, query, now)) continue;
    rows.push(row);
  }
  return rows;
}

/** Insert a live root in root-id order (the queue's fixed order). */
export function insertQueueId(ids: string[], id: string): string[] {
  if (ids.includes(id)) return ids;
  const value = Number(id);
  if (!Number.isFinite(value)) return [...ids, id];
  const next = [...ids];
  let at = next.length;
  for (let index = 0; index < next.length; index += 1) {
    const candidate = Number(next[index]);
    if (Number.isFinite(candidate) && candidate > value) {
      at = index;
      break;
    }
  }
  next.splice(at, 0, id);
  return next;
}

// ---- lease / needs / time -------------------------------------------------

export interface BoardLeaseFacts {
  expired: boolean;
  remainingSeconds: number;
  text: string;
  tone: "normal" | "warning" | "danger";
  /** 0..1 for a Progress bar; 0 when no claim. */
  fraction: number;
  /** `Held by this tab` / `yours, but not from this tab` / the holder. */
  holder: "this-tab" | "another-tab" | "someone" | "none";
}

const DEFAULT_LEASE_SECONDS = 900;

/** A claim's countdown, rebuilt from local time each second. The server's
 * `until` is the only deadline; a clock skew under ±2s is not compensated. */
export function boardLeaseFacts(
  task: BoardTaskView,
  now: number,
  opts: { hasToken?: boolean; mine?: boolean; leaseSeconds?: number } = {},
): BoardLeaseFacts {
  if (task.state !== "claimed" || !task.claim) {
    return { expired: false, remainingSeconds: 0, text: "", tone: "normal", fraction: 0, holder: "none" };
  }
  const until = Date.parse(task.claim.until);
  if (!Number.isFinite(until)) {
    return { expired: false, remainingSeconds: 0, text: "租约时间未知", tone: "warning", fraction: 0, holder: "someone" };
  }
  const remaining = Math.max(0, Math.round((until - now) / 1000));
  const total = Math.max(1, opts.leaseSeconds ?? DEFAULT_LEASE_SECONDS);
  const expired = remaining <= 0;
  const holder = opts.hasToken ? "this-tab" : opts.mine ? "another-tab" : "someone";
  const text = expired
    ? "租约已到期"
    : remaining < 60
      ? `剩余 ${remaining} 秒`
      : `剩余 ${Math.floor(remaining / 60)}:${String(remaining % 60).padStart(2, "0")}`;
  return {
    expired,
    remainingSeconds: remaining,
    text,
    tone: expired ? "danger" : remaining < 60 ? "warning" : "normal",
    fraction: Math.max(0, Math.min(1, remaining / total)),
    holder,
  };
}

/** The queue row's right-hand cell: a claimed task's countdown; an open task's
 * age; a finished task's finish time. */
export function boardDeadlineText(
  task: BoardTaskView,
  message: BoardMessageView | undefined,
  now: number,
  latest?: BoardMessageView,
): string {
  const state = effectiveTaskState(task, now);
  if (task.state === "claimed" && state === "ready") return "租约已到期";
  if (state === "claimed") {
    const facts = boardLeaseFacts(task, now);
    return facts.expired ? "租约已到期" : facts.text;
  }
  if (state === "waiting") {
    const created = message?.createdAt;
    const age = boardAgeText(created, now);
    return age ? (age === "刚刚" ? age : `${age}前`) : "等待依赖";
  }
  if (state === "blocked") {
    return task.blocked?.kind === "awaiting_approval" ? "等待审批" : "等待回复";
  }
  if (state === "completed") return `完成于 ${boardTimeShort(latest?.createdAt ?? message?.createdAt)}`;
  if (state === "failed") return `失败于 ${boardTimeShort(latest?.createdAt ?? message?.createdAt)}`;
  if (state === "canceled") return "已取消";
  const created = message?.createdAt;
  const age = boardAgeText(created, now);
  return age ? (age === "刚刚" ? age : `${age}前`) : "可认领";
}

const two = (value: number): string => (value < 10 ? `0${value}` : String(value));

/** `14:02` — one clock language for every row. */
export function boardTimeShort(iso: string | undefined): string {
  if (!iso) return "—";
  const at = new Date(iso);
  if (!Number.isFinite(at.getTime())) return "—";
  return `${two(at.getHours())}:${two(at.getMinutes())}`;
}

/** `14:02:11` — used for live tails and health facts. */
export function boardClock(iso: string | number | undefined): string {
  if (iso === undefined) return "—";
  const at = typeof iso === "number" ? new Date(iso) : new Date(iso);
  if (!Number.isFinite(at.getTime())) return "—";
  return `${two(at.getHours())}:${two(at.getMinutes())}:${two(at.getSeconds())}`;
}

/** `11-28 09:00` — a scheduled future needs its date, not only a clock. */
export function boardDateTime(iso: string | number | undefined): string {
  if (iso === undefined) return "—";
  const at = typeof iso === "number" ? new Date(iso) : new Date(iso);
  if (!Number.isFinite(at.getTime())) return "—";
  return `${two(at.getMonth() + 1)}-${two(at.getDate())} ${two(at.getHours())}:${two(at.getMinutes())}`;
}

/** `3 小时前` (without the 前): 刚刚 / 12 分钟 / 3 小时 / 2 天. */
export function boardAgeText(iso: string | undefined, now: number): string {
  if (!iso) return "";
  const at = Date.parse(iso);
  if (!Number.isFinite(at)) return "";
  const seconds = Math.max(0, Math.round((now - at) / 1000));
  if (seconds < 60) return "刚刚";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分钟`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时`;
  const days = Math.floor(hours / 24);
  return `${days} 天`;
}

export interface BoardNeedFact {
  id: string;
  state: string;
  title: string;
  done: boolean;
}

/** The task's prerequisites with the state the cache currently holds. An
 * unknown prerequisite is `unknown`, never "done". */
export function boardNeedsFact(cache: BoardCache, task: BoardTaskView): BoardNeedFact[] {
  return task.needs.map((id) => {
    const message = cache.messages[id];
    const needed = cache.tasks[id];
    return {
      id,
      state: needed ? needed.state : "unknown",
      title: message?.text?.split("\n")[0] ?? `#${id}`,
      done: needed?.state === "completed",
    };
  });
}

export function boardNeedsText(cache: BoardCache, task: BoardTaskView): string {
  if (!task.needs.length) return "无前置";
  const facts = boardNeedsFact(cache, task);
  const done = facts.filter((fact) => fact.done).length;
  const failed = facts.some((fact) => fact.state === "failed" || fact.state === "canceled");
  return failed ? `前置 ${done}/${facts.length} · 有失败` : `前置 ${done}/${facts.length}`;
}

// ---- actors / labels ------------------------------------------------------

export function boardActorName(cache: BoardCache, id: string | null | undefined): string {
  if (!id) return "池中";
  const actor = cache.actors[id];
  if (actor) return actor.name || actor.handle || actor.id;
  return id;
}

/** `@handle` for an actor that has one; an id that names nobody stays raw. */
export function boardActorHandle(cache: BoardCache, id: string | null | undefined): string {
  if (!id) return "池中";
  const actor = cache.actors[id];
  return actor?.handle ? `@${actor.handle}` : boardActorName(cache, id);
}

export interface BoardLabelFacet {
  key: string;
  values: string[];
}

/** The filter bar's label facets, excluding the reserved `board.*` namespace. */
export function boardLabelFacets(cache: BoardCache): BoardLabelFacet[] {
  return Object.entries(cache.labels)
    .filter(([key]) => !key.startsWith("board."))
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([key, values]) => ({ key, values: [...values] }));
}

/** The task's visible labels: `board.*` labels are system-maintained and are
 * shown separately, never offered as editable content. */
export function boardTaskLabels(task: BoardTaskView, message: BoardMessageView | undefined): {
  system: Record<string, string>;
  user: Record<string, string>;
} {
  const labels = message?.labels ?? {};
  const system: Record<string, string> = {};
  const user: Record<string, string> = {};
  for (const [key, value] of Object.entries(labels)) {
    if (key.startsWith("board.")) system[key] = value;
    else user[key] = value;
  }
  void task;
  return { system, user };
}

// ---- label presentation ---------------------------------------------------

const boardHumanize = (value: string): string => value.replace(/[_-]+/g, " ").trim();

const BOARD_LABEL_KEY_NAMES: Record<string, string> = {
  priority: "优先级",
  team: "团队",
  source: "来源",
  ticket: "工单",
  channel: "频道",
  owner: "创建者",
  for: "指派给",
};

const BOARD_PRIORITY_LABELS: Record<string, string> = {
  urgent: "紧急",
  high: "高优先级",
  medium: "中优先级",
  normal: "普通",
  low: "低优先级",
};

const BOARD_TEAM_LABELS: Record<string, string> = {
  payments: "支付团队",
  growth: "增长团队",
  release: "发布团队",
  archive: "归档团队",
  ops: "运维团队",
  work: "工作团队",
};

const BOARD_SOURCE_LABELS: Record<string, string> = {
  superagent: "超级助理",
  slack: "Slack",
  github: "GitHub",
  linear: "Linear",
  chatsdk: "Chat SDK",
  board: "任务面",
};

export interface BoardLabelView {
  key: string;
  value: string;
  /** Human-facing text; never `key=value`. */
  text: string;
  tone: BoardTone;
}

/** A user label in product words. `board.*` is system state, not content, so
 * it is omitted; an unknown key keeps only its value as a light tag. */
export function boardLabelView(key: string, rawValue: string | undefined): BoardLabelView | null {
  if (!key || key.startsWith("board.")) return null;
  const value = (rawValue ?? "").trim();
  if (!value) return null;
  if (key === "priority") {
    return {
      key,
      value,
      text: BOARD_PRIORITY_LABELS[value.toLowerCase()] ?? `优先级 · ${boardHumanize(value)}`,
      tone: value.toLowerCase() === "high" || value.toLowerCase() === "urgent" ? "secondary" : "outline",
    };
  }
  if (key === "team") {
    return { key, value, text: BOARD_TEAM_LABELS[value.toLowerCase()] ?? `团队 · ${boardHumanize(value)}`, tone: "outline" };
  }
  if (key === "source") {
    return { key, value, text: BOARD_SOURCE_LABELS[value.toLowerCase()] ?? `来源 · ${boardHumanize(value)}`, tone: "outline" };
  }
  if (key === "ticket") {
    return { key, value, text: `工单 #${value.replace(/^#/, "")}`, tone: "outline" };
  }
  return { key, value, text: boardHumanize(value), tone: "outline" };
}

export function boardVisibleLabels(labels: Record<string, string> | undefined): BoardLabelView[] {
  const out: BoardLabelView[] = [];
  for (const [key, value] of Object.entries(labels ?? {})) {
    const view = boardLabelView(key, value);
    if (view) out.push(view);
  }
  return out;
}

/** The filter facet's human title; the raw key=value stays in the form value
 * only, never in what the reader sees. */
export function boardLabelFilterTitle(key: string, value: string): string {
  if (!key) return "全部标签";
  if (key.startsWith("board.")) return "系统标签";
  const view = boardLabelView(key, value);
  const keyName = BOARD_LABEL_KEY_NAMES[key] ?? "标签";
  if (view) return keyName === "标签" ? view.text : `${keyName} · ${view.text}`;
  if (!value) return keyName;
  return boardHumanize(value);
}

export function boardPlatformLabel(platform: string | undefined): string {
  switch ((platform ?? "").toLowerCase()) {
    case "slack":
      return "Slack";
    case "github":
      return "GitHub";
    case "linear":
      return "Linear";
    case "chatsdk":
      return "Chat SDK";
    case "superagent":
      return "超级助理";
    case "board":
      return "任务面";
    default:
      return platform ? boardHumanize(platform) : "外部平台";
  }
}

const BOARD_KNOWN_PLATFORMS = new Set(["slack", "github", "linear", "chatsdk", "superagent", "board"]);

/** An inbound event reference (`slack:evt_9a1c`,
 * `reaction:slack:C…:1700009999.000100|…`) becomes a short product reference;
 * the provider's raw id never reaches the reader. */
export function boardEventRefLabel(ref: string | undefined): string {
  const raw = (ref ?? "").trim();
  if (!raw) return "入站事件";
  const parts = raw.split(/[:|]/).map((part) => part.trim()).filter(Boolean);
  const platform = parts.find((part) => BOARD_KNOWN_PLATFORMS.has(part.toLowerCase()));
  const rest = platform ? parts.slice(parts.indexOf(platform) + 1) : parts;
  const candidate =
    rest.find((part) => /^evt[_-]?/i.test(part)) ??
    rest.find((part) => /\d{6,}/.test(part)) ??
    rest[0] ??
    "";
  const short = candidate.replace(/^evt[_-]?/i, "").slice(0, 8);
  const name = platform ? boardPlatformLabel(platform) : "";
  return name ? `${name} 事件 ${short || "…"}` : `事件 ${short || "…"}`;
}

/** A platform thread target (`slack:C08:…`, `slack:#work-production`) becomes
 * "Slack 线程"; only human-readable channel names are kept, never provider
 * coordinates. */
export function boardThreadRefLabel(ref: string | undefined): string {
  const raw = (ref ?? "").trim();
  if (!raw) return "外部线程";
  const parts = raw.split(/[:|]/).map((part) => part.trim()).filter(Boolean);
  const platform = parts.find((part) => BOARD_KNOWN_PLATFORMS.has(part.toLowerCase()));
  const rest = platform ? parts.slice(parts.indexOf(platform) + 1) : parts;
  const human = rest.find((part) => part.startsWith("#")) ?? rest.find((part) => /^[a-z][a-z0-9_-]*/.test(part) && !/^[a-z]$/.test(part));
  const name = platform ? boardPlatformLabel(platform) : "";
  if (name) return human ? `${name} 线程 · ${human}` : `${name} 线程`;
  return human ? human : raw.startsWith("#") ? raw : "外部线程";
}

/** Provider errors are operator facts, not raw UI copy. Return the product
 * category and its next step; the raw provider sentence stays in server logs. */
export interface BoardProviderErrorDetail {
  summary: string;
  action: string;
}

export function boardProviderErrorDetail(error: string | undefined | null): BoardProviderErrorDetail {
  const raw = (error ?? "").trim();
  if (!raw) return { summary: "", action: "" };
  const lower = raw.toLowerCase().replaceAll("_", " ").replaceAll("-", " ");
  if (lower.includes("429") || lower.includes("rate limit") || lower.includes("rate limited")) {
    return { summary: "平台限流，稍后重试", action: "系统会按退避自动重试；无需手动操作。" };
  }
  if (lower.includes("timeout") || lower.includes("timed out") || lower.includes("deadline")) {
    return { summary: "平台响应超时", action: "系统会重试；持续超时请检查网络与平台状态。" };
  }
  if (lower.includes("signature") && lower.includes("expired")) {
    return { summary: "平台签名已过期，需要重新投递", action: "重试会生成新的签名；无需轮换凭据。" };
  }
  if (
    lower.includes("401") ||
    lower.includes("403") ||
    lower.includes("unauthorized") ||
    lower.includes("forbidden") ||
    lower.includes("permission") ||
    lower.includes("not in channel") ||
    lower.includes("missing scope")
  ) {
    return { summary: "平台凭据或频道权限被拒绝", action: "确认 Bot 仍在频道内且凭据 scope 未变；轮换后自动恢复。" };
  }
  if (
    lower.includes("5xx") ||
    lower.includes("500") ||
    lower.includes("502") ||
    lower.includes("503") ||
    lower.includes("504") ||
    lower.includes("server error") ||
    lower.includes("unavailable") ||
    lower.includes("bad gateway")
  ) {
    return { summary: "平台暂时不可用", action: "平台恢复后重试；任务主路径不受影响。" };
  }
  if (lower.includes("hmac") || lower.includes("nonce")) {
    return { summary: "桥接签名校验失败", action: "检查 bridge 密钥与双方时钟；重放窗口外的请求会被拒绝。" };
  }
  if (lower.includes("connection") || lower.includes("network") || lower.includes("refused") || lower.includes("dns")) {
    return { summary: "无法连接平台", action: "检查网络、代理与平台状态后重试。" };
  }
  if (lower.includes("permanent")) {
    return { summary: "平台拒绝且不会自动恢复", action: "先修复平台侧配置，再手动重试这条投递。" };
  }
  return { summary: "平台返回错误", action: "详情在服务端日志；先按失败类别排查后重试。" };
}

/** The one-line category. Kept for callers that only need the summary. */
export function boardProviderErrorCopy(error: string | undefined | null): string {
  return boardProviderErrorDetail(error).summary;
}

/** Category plus next step, for a row that has room for a sentence. */
export function boardProviderErrorLine(error: string | undefined | null): string {
  const detail = boardProviderErrorDetail(error);
  if (!detail.summary) return "";
  return `${detail.summary} · ${detail.action}`;
}

export function boardWatcherActionLabel(action: string | undefined): string {
  switch ((action ?? "").toLowerCase()) {
    case "post":
      return "发布消息";
    case "assign-by-label":
      return "按标签指派";
    case "notify-owner":
      return "通知负责人";
    case "wake-session":
      return "唤醒会话";
    case "run-tool":
      return "执行工具";
    default:
      return action ? "已声明的动作" : "未记录动作";
  }
}

// ---- moves ----------------------------------------------------------------

/** Every action the UI knows how to render. `moves` is the authority: an
 * unknown token is ignored (never guessed). The design's matrix is a server
 * rule reference, not a client fallback. */
export type BoardMove =
  | "claim"
  | "renew"
  | "release"
  | "release.force"
  | "takeover"
  | "complete"
  | "fail"
  | "block"
  | "unblock"
  | "answer"
  | "reopen"
  | "assign"
  | "needs"
  | "cancel"
  | "reply";

const KNOWN_MOVES = new Set<string>([
  "claim",
  "renew",
  "release",
  "release.force",
  "takeover",
  "complete",
  "fail",
  "block",
  "unblock",
  "answer",
  "reopen",
  "assign",
  "needs",
  "cancel",
  "reply",
]);

export function taskMoves(task: BoardTaskView | undefined): BoardMove[] {
  if (!task || !Array.isArray(task.moves)) return [];
  return task.moves.filter((move): move is BoardMove => KNOWN_MOVES.has(move));
}

export function hasMove(task: BoardTaskView | undefined, move: BoardMove): boolean {
  return taskMoves(task).includes(move);
}

/** Interim moves while an SSE task with no Phase 2 fields is being re-read:
 * keep only actions whose state still could allow them. This is deliberately
 * conservative; the augmented GET response replaces it. */
export function interimMovesForState(cached: BoardTaskView, incoming: BoardTaskView): BoardMove[] {
  if (incoming.archived === true || cached.mirror?.readOnly === true) return [];
  const allowed: Record<string, BoardMove[]> = {
    waiting: ["assign", "needs", "cancel", "reply"],
    ready: ["claim", "block", "assign", "needs", "cancel", "reply"],
    claimed: ["renew", "release", "release.force", "takeover", "complete", "fail", "block", "reply"],
    blocked: ["unblock", "assign", "needs", "cancel", "reply", "answer"],
    completed: ["reply"],
    failed: ["reopen", "cancel", "reply"],
    canceled: ["reply"],
  };
  const list = allowed[incoming.state] ?? [];
  return taskMoves(cached).filter((move) => list.includes(move));
}

/** Moves that need the claim token itself. A reload loses the token, and an
 * expired lease has no holder, so none of these can be fenced from this tab. */
const TOKEN_BEARING_MOVES: BoardMove[] = ["renew", "release", "complete", "fail", "block"];

/** Moves this board version cannot execute: v0.1.1/keel exposes no takeover or
 * force-release endpoint, so the UI never renders them. They may still appear
 * in a server list; that is a protocol token, not a product action. */
const UI_UNSUPPORTED_MOVES: BoardMove[] = ["takeover", "release.force"];

/** The moves this tab may actually render: the server's list minus the
 * token-bearing ones when the claim is mine but its credential is not in this
 * tab (a reload) or the lease has expired. */
export function boardAvailableMoves(
  task: BoardTaskView | undefined,
  opts: { hasToken: boolean; mine: boolean; now: number; identityKnown?: boolean },
): BoardMove[] {
  const moves = taskMoves(task);
  if (!task || task.state !== "claimed" || !task.claim) return moves;
  const until = Date.parse(task.claim.until);
  const expired = Number.isFinite(until) && until <= opts.now;
  // An expired lease has no holder: no token-bearing move can be fenced, even
  // if this tab still has a stale token. Claim becomes primary when the server
  // lists it (the queue presents the task as ready).
  if (expired) return moves.filter((move) => !TOKEN_BEARING_MOVES.includes(move));
  // A token is the fence: when /me is unavailable we cannot prove the claim is
  // ours, but this tab can still try the token-bearing action and let the
  // server reject a stale token. Once identity is known, another actor's claim
  // must not inherit this tab's token.
  if (opts.hasToken && (opts.mine || opts.identityKnown === false)) return moves;
  return moves.filter((move) => !TOKEN_BEARING_MOVES.includes(move));
}

/** `boardAvailableMoves` filtered to what this board version can execute. */
export function boardRenderableMoves(
  task: BoardTaskView | undefined,
  opts: { hasToken: boolean; mine: boolean; now: number; identityKnown?: boolean },
): BoardMove[] {
  return boardAvailableMoves(task, opts).filter((move) => !UI_UNSUPPORTED_MOVES.includes(move));
}

export const BOARD_MOVE_LABEL: Record<BoardMove, string> = {
  claim: "认领",
  renew: "续租",
  release: "释放",
  "release.force": "收回认领",
  takeover: "重新认领",
  complete: "完成",
  fail: "标记失败",
  block: "阻塞",
  unblock: "解除阻塞",
  answer: "回答",
  reopen: "重新打开",
  assign: "指派",
  needs: "前置依赖",
  cancel: "取消任务",
  reply: "回复",
};

/** The primary action: at most one, in product order. */
export const BOARD_PRIMARY_ORDER: BoardMove[] = ["complete", "claim", "unblock", "reopen", "answer"];

export function boardPrimaryMove(task: BoardTaskView | undefined): BoardMove | undefined {
  const moves = taskMoves(task);
  return BOARD_PRIMARY_ORDER.find((move) => moves.includes(move));
}

/** Single-letter action keys, bound only while the key's move is present. */
export const BOARD_KEY_MOVES: Record<string, BoardMove> = {
  c: "claim",
  r: "renew",
  l: "release",
  e: "complete",
  f: "fail",
  b: "block",
  u: "unblock",
  o: "reopen",
  a: "assign",
  n: "needs",
  x: "cancel",
};

// ---- failures -------------------------------------------------------------

export type BoardFailureKind = "auth" | "permission" | "not_found" | "disabled" | "unavailable" | "network" | "conflict" | "invalid";

export interface BoardFailure {
  kind: BoardFailureKind;
  status?: number;
  code?: string;
  message: string;
  requestId?: string;
  missing?: string;
}

/** Classify a read/write failure. 404 is not proof of absence unless the code
 * says so: the board merges "not found" and "not visible" on purpose. */
export function boardFailureOf(error: unknown): BoardFailure {
  if (error instanceof KeelApiError) {
    const code = error.code ?? "";
    if (error.status === 401) {
      return { kind: "auth", status: 401, code, message: "登录已失效。", ...(error.requestId ? { requestId: error.requestId } : {}) };
    }
    if (code === "board_disabled") {
      return {
        kind: "disabled",
        status: error.status,
        code,
        message: "任务看板未启用。",
        ...(error.requestId ? { requestId: error.requestId } : {}),
      };
    }
    if (error.status === 403) {
      return {
        kind: "permission",
        status: 403,
        code,
        message: error.message || "缺少权限。",
        ...(error.requiredKey ? { missing: error.requiredKey } : {}),
        ...(error.requestId ? { requestId: error.requestId } : {}),
      };
    }
    if (error.status === 404) {
      return {
        kind: "not_found",
        status: 404,
        code,
        message: "任务不存在，或你没有访问这个频道。",
        ...(error.requestId ? { requestId: error.requestId } : {}),
      };
    }
    if (error.status === 409) {
      return { kind: "conflict", status: 409, code, message: error.message || "任务状态已经变化。", ...(error.requestId ? { requestId: error.requestId } : {}) };
    }
    if (error.status === 400) {
      return { kind: "invalid", status: 400, code, message: error.message || "提交的内容无效。", ...(error.requestId ? { requestId: error.requestId } : {}) };
    }
    if (error.status === 503) {
      return { kind: "unavailable", status: 503, code, message: "服务暂时不可用。", ...(error.requestId ? { requestId: error.requestId } : {}) };
    }
    return { kind: "network", status: error.status, code, message: error.message || "读取失败。", ...(error.requestId ? { requestId: error.requestId } : {}) };
  }
  const message = error instanceof Error && error.message ? error.message : String(error);
  return { kind: "network", message };
}

/** The error page's copy: a human sentence, then the one next step (design
 * §8.4). The action is returned as a key the screen maps to a button. */
export function boardFailureCopy(failure: BoardFailure): {
  title: string;
  description: string;
  action: "retry" | "login" | "back" | "refresh" | null;
} {
  switch (failure.kind) {
    case "auth":
      return { title: "登录已失效", description: "重新登录后回到这个页面。", action: "login" };
    case "permission":
      return {
        title: "缺少权限",
        description: failure.missing ? `需要 ${failure.missing}；当前账号缺少这项权限。` : failure.message,
        action: "back",
      };
    case "not_found":
      return {
        title: "任务不存在，或你没有访问这个频道",
        description: "地址没有对应到你可读的任务；它可能已被删除，或频道权限已变化。",
        action: "back",
      };
    case "disabled":
      return { title: "任务看板未启用", description: "这个租户还没有开启任务看板。找租户管理员开启后再试。", action: "back" };
    case "unavailable":
      return { title: "服务暂时不可用", description: "没有写入任何内容；稍后重试。", action: "retry" };
    case "conflict":
      return { title: "任务状态已经变化", description: failure.message, action: "refresh" };
    case "invalid":
      return { title: "没有提交成功", description: failure.message, action: null };
    case "network":
    default:
      return { title: "连接中断，结果未确认", description: "可能已经写入。先刷新任务再决定是否重试。", action: "refresh" };
  }
}

// ---- the data -------------------------------------------------------------

export type BoardPhase = "idle" | "loading" | "ready" | "error" | "permission" | "not_found" | "disabled";
export type BoardSectionPhase = "idle" | "loading" | "ready" | "error" | "permission" | "empty";

export interface BoardSyncData {
  entries: BoardSyncEntryView[];
  status: BoardSyncStatusView;
  dead: BoardSyncInboxItem[];
  failures: BoardSyncFailure[];
  /** key → credential facts from `/sync/credentials` (never a value). */
  credentials: Record<string, BoardSyncCredentialView>;
  /** Whether the credential read answered; false is 未知, not "valid". */
  credentialsKnown: boolean;
}

export interface BoardAutomationData {
  watchers: BoardWatcherView[];
  schedules: BoardScheduleView[];
}

export interface BoardActionFailure {
  taskId: string;
  move: string;
  failure: BoardFailure;
}

export interface BoardNotice {
  tone: "info" | "success" | "warning";
  text: string;
}

const PAGE_LIMIT = 50;

/** A 32-byte base64url token, generated in this tab. The server only stores
 * its SHA-256; localStorage never sees it. */
function newClaimToken(): string {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  const bytes = new Uint8Array(32);
  const cryptoAny = globalThis.crypto as { getRandomValues?: (buffer: Uint8Array) => Uint8Array } | undefined;
  if (cryptoAny?.getRandomValues) {
    cryptoAny.getRandomValues(bytes);
  } else {
    for (let index = 0; index < bytes.length; index += 1) {
      bytes[index] = Math.floor(Math.random() * 256);
    }
  }
  let out = "";
  for (let index = 0; index < bytes.length; index += 1) out += alphabet[bytes[index] % alphabet.length];
  return out;
}

/** The board client's in-memory state. One per app; `reset()` on logout. */
class BoardStoreImpl {
  readonly phase = signal<BoardPhase>("idle");
  readonly failure = signal<BoardFailure | null>(null);
  readonly cache = signal<BoardCache>(emptyBoardCache());
  readonly query = signal<BoardQuery>(defaultBoardQuery());
  readonly queueIds = signal<string[]>([]);
  readonly queueLoading = signal(false);
  readonly queueMore = signal(false);
  readonly queueError = signal<BoardFailure | null>(null);
  readonly queueStale = signal(false);
  readonly hasMore = signal(false);
  /** The board answered an archive query without any `archived` field. */
  readonly archiveUnsupported = signal(false);
  readonly totalLoaded = signal(0);
  readonly channels = signal<BoardChannelView[]>([]);
  /** Whether the channel read answered; an empty list on a failure is not
   * proof that the reader has no readable channel. */
  readonly channelsLoaded = signal(false);
  readonly actors = signal<Array<BoardActorView & { id: string }>>([]);
  readonly me = signal<BoardMeView | null>(null);
  /** The identity read answered (true) or failed (false): a board without
   * `GET /me` cannot prove who holds a claim. */
  readonly meLoaded = signal(false);
  readonly meFailure = signal<BoardFailure | null>(null);
  readonly threadId = signal<string | null>(null);
  readonly threadPhase = signal<BoardSectionPhase>("idle");
  readonly threadError = signal<BoardFailure | null>(null);
  readonly threadStale = signal(false);
  readonly replySending = signal(false);
  readonly replyError = signal<BoardFailure | null>(null);
  readonly pendingReplies = signal<Array<{ id: string; text: string; createdAt: string }>>([]);
  readonly liveStatus = signal<BoardLiveStatus>("connecting");
  readonly liveAttempt = signal(0);
  readonly liveCursor = signal("");
  readonly lastEventAt = signal<number | undefined>(undefined);
  readonly actionInFlight = signal<{ taskId: string; move: string } | null>(null);
  readonly actionError = signal<BoardActionFailure | null>(null);
  readonly notice = signal<BoardNotice | null>(null);
  readonly sync = signal<BoardSyncData | null>(null);
  readonly syncPhase = signal<BoardSectionPhase>("idle");
  readonly syncError = signal<BoardFailure | null>(null);
  readonly syncStale = signal(false);
  readonly syncInboxKey = signal("");
  readonly syncRetrying = signal<Record<string, boolean>>({});
  readonly automation = signal<BoardAutomationData | null>(null);
  readonly automationPhase = signal<BoardSectionPhase>("idle");
  readonly automationError = signal<BoardFailure | null>(null);
  readonly automationStale = signal(false);
  readonly automationBusy = signal<Record<string, boolean>>({});
  /** The selected watcher's recent deliveries (the read-only drawer). */
  readonly watcherDeliveries = signal<Record<string, BoardWatcherDelivery[]>>({});
  readonly watcherDeliveryBusy = signal<string | null>(null);
  /** The wall clock for countdowns; one ticker for every lease row. */
  readonly clock = signal(Date.now());

  private attached: BoardApi | null = null;
  private opened = false;
  private closeStream: (() => void) | null = null;
  private queueToken = 0;
  private threadToken = 0;
  private clockTimer: number | undefined;
  private noticeTimer: number | undefined;
  /** Claim credentials: this tab's memory only (never localStorage). */
  private tokens = new Map<string, BoardTaskClaim>();
  /** The pre-write task for a rollback, keyed by root id. */
  private optimistic = new Map<string, { task: BoardTaskView; message: BoardMessageView }>();
  /** Cursors already merged; at-least-once delivery must not double-apply. */
  private seen = new Set<string>();
  /** Stable idempotency keys for retries of the same draft/form. A successful
   * write drops its key; a failed one keeps it while the text is unchanged. */
  private replyKeys = new Map<string, { text: string; key: string }>();
  private newTaskKey: { fingerprint: string; key: string } | null = null;
  private idempotencySeq = 0;
  private augmentTokens = new Map<string, number>();

  /** The transport in force. A harness installs one before `open()`. */
  api(): BoardApi | null {
    if (this.attached) return this.attached;
    return store.boardApi();
  }

  setTransport(api: BoardApi | null): void {
    this.attached = api;
  }

  /** Drop every read, token and stream (a session change). */
  reset(): void {
    this.close();
    this.opened = false;
    this.phase.set("idle");
    this.failure.set(null);
    this.cache.set(emptyBoardCache());
    this.query.set(defaultBoardQuery());
    this.queueIds.set([]);
    this.queueLoading.set(false);
    this.queueMore.set(false);
    this.queueError.set(null);
    this.queueStale.set(false);
    this.hasMore.set(false);
    this.archiveUnsupported.set(false);
    this.totalLoaded.set(0);
    this.channels.set([]);
    this.channelsLoaded.set(false);
    this.actors.set([]);
    this.me.set(null);
    this.meLoaded.set(false);
    this.meFailure.set(null);
    this.threadId.set(null);
    this.threadPhase.set("idle");
    this.threadError.set(null);
    this.threadStale.set(false);
    this.replySending.set(false);
    this.replyError.set(null);
    this.pendingReplies.set([]);
    this.liveStatus.set("connecting");
    this.liveAttempt.set(0);
    this.liveCursor.set("");
    this.lastEventAt.set(undefined);
    this.augmenting.set({});
    this.actionInFlight.set(null);
    this.actionError.set(null);
    this.notice.set(null);
    this.sync.set(null);
    this.syncPhase.set("idle");
    this.syncError.set(null);
    this.syncStale.set(false);
    this.syncInboxKey.set("");
    this.syncRetrying.set({});
    this.automation.set(null);
    this.automationPhase.set("idle");
    this.automationError.set(null);
    this.automationStale.set(false);
    this.automationBusy.set({});
    this.watcherDeliveries.set({});
    this.watcherDeliveryBusy.set(null);
    this.tokens.clear();
    this.optimistic.clear();
    this.seen.clear();
    this.replyKeys.clear();
    this.newTaskKey = null;
    this.augmentTokens.clear();
    this.queueToken += 1;
    this.threadToken += 1;
  }

  /** Close only the SSE feed (leaving the board route); the cache and clock
   * stay, so returning reconnects from the last cursor. */
  stopLive(): void {
    this.closeStream?.();
    this.closeStream = null;
    this.liveStatus.set("closed");
  }

  /** Reconnect after returning to the board (the initial open already
   * connected once; an idempotent open alone would not). */
  resumeLive(): void {
    if (!this.opened || this.closeStream !== null) return;
    this.connectStream();
  }

  /** Stop the stream and ticker (leaving the route, or a test teardown). */
  close(): void {
    this.closeStream?.();
    this.closeStream = null;
    if (this.clockTimer !== undefined) {
      clearInterval(this.clockTimer);
      this.clockTimer = undefined;
    }
    if (this.noticeTimer !== undefined) {
      clearTimeout(this.noticeTimer);
      this.noticeTimer = undefined;
    }
    this.liveStatus.set("closed");
  }

  /** One boot read: me/channels/actors, then the queue. Idempotent. */
  async open(): Promise<void> {
    if (this.opened) return;
    this.opened = true;
    this.phase.set("loading");
    this.failure.set(null);
    this.startClock();
    const api = this.api();
    if (!api) {
      this.phase.set("error");
      this.failure.set({ kind: "network", message: "这个客户端没有任务面传输。" });
      return;
    }
    const [meResult, channelsResult, actorsResult] = await Promise.allSettled([
      api.boardMe ? api.boardMe() : Promise.reject(new Error("board.me unavailable")),
      api.boardChannels(undefined, 100),
      api.boardActors(200),
    ]);
    if (channelsResult.status === "fulfilled") {
      const page = channelsResult.value;
      this.channels.set(page.channels);
      this.channelsLoaded.set(true);
      this.cache.set(mergeChannels(this.cache(), page.channels));
    } else {
      const failure = boardFailureOf(channelsResult.reason);
      if (failure.kind === "disabled" || failure.kind === "permission" || failure.kind === "auth" || failure.kind === "not_found") {
        this.phase.set(failure.kind === "disabled" ? "disabled" : failure.kind === "permission" ? "permission" : failure.kind === "not_found" ? "not_found" : "error");
        this.failure.set(failure);
        return;
      }
      // A channel read failure does not hide an otherwise readable queue: the
      // queue read carries its own channel filter.
    }
    if (actorsResult.status === "fulfilled") {
      const page = actorsResult.value;
      this.actors.set(page.actors);
      this.cache.set(mergeActors(this.cache(), page.actors));
    }
    if (meResult.status === "fulfilled") {
      this.me.set(meResult.value);
      this.meLoaded.set(true);
      this.meFailure.set(null);
    } else {
      // `/me` is a Phase 2 contract addition. An older board answers 404;
      // the page keeps working, but it must say that identity (and therefore
      // claim recovery) is unverified instead of silently treating the viewer
      // as a stranger.
      this.me.set(null);
      this.meLoaded.set(false);
      this.meFailure.set(boardFailureOf(meResult.reason));
    }
    await this.loadQueue(true);
    if (this.phase() !== "error" && this.phase() !== "permission" && this.phase() !== "not_found" && this.phase() !== "disabled") {
      this.phase.set("ready");
      this.connectStream();
    }
  }

  /** Re-read the first page (a manual refresh; keeps the query). */
  async refresh(): Promise<void> {
    if (!this.opened) {
      await this.open();
      return;
    }
    await this.loadQueue(true);
  }

  /** Replace the query and read the first page. The caller owns the URL. */
  async setQuery(patch: Partial<BoardQuery>): Promise<void> {
    const next = { ...this.query(), ...patch };
    this.query.set(next);
    await this.loadQueue(true);
    this.connectStream();
  }

  async clearQuery(): Promise<void> {
    await this.setQuery(defaultBoardQuery());
  }

  async loadMore(): Promise<void> {
    if (!this.hasMore() || this.queueMore() || this.queueLoading()) return;
    await this.loadQueue(false);
  }

  private async loadQueue(reset: boolean): Promise<void> {
    const api = this.api();
    if (!api) return;
    const token = ++this.queueToken;
    if (reset) {
      this.queueLoading.set(true);
      this.queueError.set(null);
    } else {
      this.queueMore.set(true);
    }
    const after = reset ? undefined : this.queueIds().length ? this.queueIds()[this.queueIds().length - 1] : undefined;
    try {
      const page = await api.boardTasks(queryToTaskQuery(this.query(), after, PAGE_LIMIT));
      if (token !== this.queueToken) return;
      let cache = this.cache();
      for (const summary of page.tasks) {
        cache = mergeMessage(cache, summary.message);
        if (summary.last) cache = mergeMessage(cache, summary.last);
        const existing = cache.threads[summary.message.id];
        cache = {
          ...cache,
          threads: {
            ...cache.threads,
            [summary.message.id]: {
              message: cache.messages[summary.message.id] ?? summary.message,
              replies: existing?.replies ?? [],
              cursor: existing?.cursor ?? "",
              replyCount: summary.replies,
              ...(existing?.older !== undefined ? { older: existing.older } : {}),
            },
          },
        };
      }
      this.cache.set(cache);
      const ids = page.tasks.map((summary) => summary.message.id);
      if (reset) {
        this.queueIds.set(ids);
      } else {
        const existing = this.queueIds();
        const merged = [...existing];
        for (const id of ids) if (!merged.includes(id)) merged.push(id);
        this.queueIds.set(merged);
      }
      this.totalLoaded.set((reset ? 0 : this.totalLoaded()) + ids.length);
      // An archive page from a board that has no `archived` field can never
      // satisfy the filter; stop paging instead of walking the whole queue.
      const archiveUnsupported =
        this.query().archived &&
        page.tasks.length > 0 &&
        page.tasks.every((summary) => summary.message.task?.archived !== true);
      this.archiveUnsupported.set(archiveUnsupported);
      this.hasMore.set(!archiveUnsupported && ids.length >= PAGE_LIMIT);
      if (page.cursor) this.liveCursor.set(page.cursor);
      this.queueStale.set(false);
      if (reset) {
        this.phase.set("ready");
        this.failure.set(null);
      }
    } catch (error) {
      if (token !== this.queueToken) return;
      const failure = boardFailureOf(error);
      this.queueError.set(failure);
      if (reset) {
        if (failure.kind === "permission") this.phase.set("permission");
        else if (failure.kind === "disabled") this.phase.set("disabled");
        else if (failure.kind === "not_found") this.phase.set("not_found");
        else if (failure.kind === "auth") this.phase.set("error");
        else this.phase.set("error");
        this.failure.set(failure);
      } else {
        this.queueStale.set(true);
      }
    } finally {
      if (token === this.queueToken) {
        this.queueLoading.set(false);
        this.queueMore.set(false);
      }
    }
  }

  // ---- thread -------------------------------------------------------------

  async openThread(id: string): Promise<void> {
    this.threadId.set(id);
    await this.loadThread(id, true);
  }

  closeThread(): void {
    this.threadId.set(null);
    this.threadPhase.set("idle");
    this.threadError.set(null);
    this.threadStale.set(false);
    this.replyError.set(null);
  }

  async refreshThread(): Promise<void> {
    const id = this.threadId();
    if (id) await this.loadThread(id, true);
  }

  private async loadThread(id: string, reset: boolean): Promise<void> {
    const api = this.api();
    if (!api) return;
    const token = ++this.threadToken;
    if (reset) {
      this.threadPhase.set("loading");
      this.threadError.set(null);
    }
    try {
      const view = await api.boardThread(id);
      if (token !== this.threadToken) return;
      this.mergeThreadView(view);
      this.threadPhase.set("ready");
      this.threadStale.set(false);
    } catch (error) {
      if (token !== this.threadToken) return;
      const failure = boardFailureOf(error);
      this.threadError.set(failure);
      if (this.cache().threads[id]) {
        this.threadStale.set(true);
        this.threadPhase.set("ready");
      } else {
        this.threadPhase.set("error");
      }
    }
  }

  private mergeThreadView(view: BoardThreadView): void {
    let cache = mergeThread(this.cache(), view);
    // The root's task is authoritative for the queue row; make sure the task
    // cache sees the same view when the server carries one.
    if (view.message.task) cache = mergeTask(cache, view.message.id, view.message.task);
    this.cache.set(cache);
  }

  /** A reply write. The composer keeps its draft on failure. */
  async reply(text: string): Promise<boolean> {
    const id = this.threadId();
    const api = this.api();
    const trimmed = text.trim();
    if (!id || !api || !trimmed) return false;
    const pendingId = `pending-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`;
    this.pendingReplies.update((list) => [...list, { id: pendingId, text: trimmed, createdAt: new Date().toISOString() }]);
    this.replySending.set(true);
    this.replyError.set(null);
    const root = this.cache().messages[id];
    const saved = this.replyKeys.get(id);
    const key =
      saved && saved.text === trimmed
        ? saved.key
        : `ui-reply-${id}-${Date.now()}-${++this.idempotencySeq}`;
    this.replyKeys.set(id, { text: trimmed, key });
    try {
      const body = await api.boardReply({
        re: id,
        text: trimmed,
        idempotencyKey: key,
        ...(root?.revision !== undefined ? { ifRevision: root.revision } : {}),
      });
      this.replyKeys.delete(id);
      this.pendingReplies.update((list) => list.filter((item) => item.id !== pendingId));
      const message = body.message ?? parseMessage((body as unknown as { message?: unknown }).message);
      if (message) {
        this.cache.set(mergeMessage(this.cache(), message));
      }
      await this.refreshThread();
      return true;
    } catch (error) {
      const failure = boardFailureOf(error);
      this.replyError.set(failure);
      this.pendingReplies.update((list) => list.filter((item) => item.id !== pendingId));
      return false;
    } finally {
      this.replySending.set(false);
    }
  }

  // ---- claim tokens -------------------------------------------------------

  tokenFor(taskId: string): BoardTaskClaim | undefined {
    return this.tokens.get(taskId);
  }

  /** Is the claim held by this tab (the token is here and unexpired)? */
  heldByThisTab(taskId: string, now = this.clock()): boolean {
    const token = this.tokens.get(taskId);
    if (!token) return false;
    const until = Date.parse(token.until);
    return Number.isFinite(until) ? until > now : true;
  }

  isMyActor(actor: string | undefined): boolean {
    if (!actor) return false;
    const me = this.me();
    if (me?.actor && me.actor === actor) return true;
    if (me?.account?.id && me.account.id === actor) return true;
    return false;
  }

  // ---- writes -------------------------------------------------------------

  /** One action, with an optimistic face and rollback. `patch` is what the
   * reader should see while the write is in flight. */
  private async act<T>(
    taskId: string,
    move: string,
    patch: BoardTaskView | null,
    call: () => Promise<T>,
    result: (value: T) => BoardMessageView | undefined,
    okText: string,
  ): Promise<boolean> {
    if (this.actionInFlight()) return false;
    const message = this.cache().messages[taskId];
    const task = this.cache().tasks[taskId];
    if (patch && task && message) {
      this.optimistic.set(taskId, { task, message });
      this.cache.set(mergeTask(this.cache(), taskId, patch));
    }
    this.actionInFlight.set({ taskId, move });
    this.actionError.set(null);
    try {
      const value = await call();
      const next = result(value);
      if (next) {
        // A live event may already be newer than this write's response; keep
        // the newer view (the message's version is monotonic).
        const current = this.cache().messages[next.id];
        if (!current || (current.version ?? 0) <= (next.version ?? 0)) {
          this.cache.set(mergeMessage(this.cache(), next));
          if (next.task) this.cache.set(mergeTask(this.cache(), next.id, next.task));
        }
      }
      this.optimistic.delete(taskId);
      this.flash(okText);
      return true;
    } catch (error) {
      const saved = this.optimistic.get(taskId);
      if (saved) {
        // Roll back only if the cache still holds the optimistic face. A live
        // event during the write already moved it on; restoring the old view
        // would undo newer server truth.
        const current = this.cache().tasks[taskId];
        const stillOptimistic =
          current !== undefined && patch !== null && JSON.stringify(current) === JSON.stringify(patch);
        if (stillOptimistic) this.cache.set(mergeTask(this.cache(), taskId, saved.task));
        this.optimistic.delete(taskId);
      }
      const failure = boardFailureOf(error);
      this.actionError.set({ taskId, move, failure });
      this.flash(failure.message, "warning");
      return false;
    } finally {
      this.actionInFlight.set(null);
    }
  }

  private mutationMessage(value: unknown): BoardMessageView | undefined {
    if (!value || typeof value !== "object") return undefined;
    const row = value as Record<string, unknown>;
    // board's writes answer `{task: MessageView}` (the field predates the
    // task/message split). A harness may answer `{message: MessageView}`.
    const message = parseMessage(row.task) ?? parseMessage(row.message);
    return message;
  }

  private mutationClaim(value: unknown): BoardTaskClaim | undefined {
    if (!value || typeof value !== "object") return undefined;
    const row = value as Record<string, unknown>;
    const claim = row.claim && typeof row.claim === "object" ? (row.claim as Record<string, unknown>) : null;
    if (!claim) return undefined;
    const epoch = typeof claim.epoch === "number" ? claim.epoch : Number(claim.epoch);
    if (!Number.isFinite(epoch)) return undefined;
    return {
      epoch,
      until: String(claim.until ?? ""),
      ...(typeof claim.token === "string" && claim.token ? { token: claim.token } : {}),
    };
  }

  async claim(taskId: string, leaseSeconds?: number): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    const token = newClaimToken();
    const lease = leaseSeconds ?? DEFAULT_LEASE_SECONDS;
    const until = new Date(this.clock() + lease * 1000).toISOString();
    const patch: BoardTaskView = {
      ...task,
      state: "claimed",
      epoch: task.epoch + 1,
      claim: { actor: this.me()?.actor ?? task.owner, until },
    };
    return await this.act(
      taskId,
      "claim",
      patch,
      () => api.boardClaim(taskId, { lease, token }),
      (value) => {
        const message = this.mutationMessage(value);
        const claim = this.mutationClaim(value);
        if (claim) this.tokens.set(taskId, { ...claim, token: claim.token ?? token });
        else this.tokens.set(taskId, { epoch: patch.epoch, token, until });
        return message;
      },
      `已认领 #${taskId}`,
    );
  }

  async renew(taskId: string, leaseSeconds?: number): Promise<boolean> {
    const api = this.api();
    const token = this.tokens.get(taskId) ?? this.tokensByTask(taskId);
    if (!api || !token?.token) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    const until = new Date(this.clock() + (leaseSeconds ?? DEFAULT_LEASE_SECONDS) * 1000).toISOString();
    const patch: BoardTaskView = { ...task, claim: { actor: task.claim?.actor ?? "", until } };
    return await this.act(
      taskId,
      "renew",
      patch,
      () => api.boardRenew(taskId, { epoch: token.epoch, token: token.token ?? "", ...(leaseSeconds ? { lease: leaseSeconds } : {}) }),
      (value) => {
        const message = this.mutationMessage(value);
        const untilValue = (value as { until?: unknown })?.until;
        this.tokens.set(taskId, { ...token, ...(typeof untilValue === "string" ? { until: untilValue } : { until }) });
        return message;
      },
      `租约延至 ${boardTimeShort(this.tokens.get(taskId)?.until ?? until)}`,
    );
  }

  async release(taskId: string): Promise<boolean> {
    const api = this.api();
    const token = this.tokens.get(taskId) ?? this.tokensByTask(taskId);
    if (!api || !token?.token) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    return await this.act(
      taskId,
      "release",
      { ...task, state: "ready", claim: null },
      () => api.boardRelease(taskId, { epoch: token.epoch, token: token.token }),
      (value) => {
        this.tokens.delete(taskId);
        return this.mutationMessage(value);
      },
      `已释放 #${taskId}`,
    );
  }

  async forceRelease(taskId: string): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    return await this.act(
      taskId,
      "release.force",
      { ...task, state: "ready", claim: null },
      () => api.boardRelease(taskId, { force: true }),
      (value) => {
        this.tokens.delete(taskId);
        return this.mutationMessage(value);
      },
      `已收回 #${taskId} 的认领`,
    );
  }

  async takeover(taskId: string): Promise<boolean> {
    const api = this.api();
    if (!api?.boardTakeover) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    return await this.act(
      taskId,
      "takeover",
      task,
      () => api.boardTakeover!(taskId),
      (value) => {
        const claim = this.mutationClaim(value);
        if (claim) this.tokens.set(taskId, { ...claim, token: claim.token ?? newClaimToken() });
        return this.mutationMessage(value);
      },
      `已重新认领 #${taskId}`,
    );
  }

  async complete(taskId: string, resultText?: string): Promise<boolean> {
    const api = this.api();
    const token = this.tokens.get(taskId) ?? this.tokensByTask(taskId);
    if (!api || !token?.token) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    return await this.act(
      taskId,
      "complete",
      { ...task, state: "completed", claim: null, result: resultText ?? null },
      () => api.boardComplete(taskId, { epoch: token.epoch, token: token.token ?? "", ...(resultText ? { result: resultText } : {}) }),
      (value) => {
        this.tokens.delete(taskId);
        return this.mutationMessage(value);
      },
      `已完成 #${taskId}`,
    );
  }

  async fail(taskId: string, reason: string): Promise<boolean> {
    const api = this.api();
    const token = this.tokens.get(taskId) ?? this.tokensByTask(taskId);
    if (!api || !token?.token) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    return await this.act(
      taskId,
      "fail",
      { ...task, state: "failed", claim: null, reason },
      () => api.boardFail(taskId, { epoch: token.epoch, token: token.token ?? "", reason }),
      (value) => {
        this.tokens.delete(taskId);
        return this.mutationMessage(value);
      },
      `已标记 #${taskId} 失败`,
    );
  }

  async block(taskId: string, reason: string, kind?: string): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    const token = this.tokens.get(taskId);
    const patch: BoardTaskView = {
      ...task,
      state: "blocked",
      claim: null,
      blocked: { reason, ...(kind ? { kind } : {}) },
    };
    return await this.act(
      taskId,
      "block",
      patch,
      () =>
        api.boardBlock(taskId, {
          reason,
          ...(kind ? { kind } : {}),
          ...(token?.token ? { epoch: token.epoch, token: token.token } : {}),
        }),
      (value) => {
        if (token?.token) this.tokens.delete(taskId);
        return this.mutationMessage(value);
      },
      `已阻塞 #${taskId}`,
    );
  }

  async unblock(taskId: string, replyText?: string): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    return await this.act(
      taskId,
      "unblock",
      { ...task, state: task.needs.length ? "waiting" : "ready", blocked: null },
      () => api.boardUnblock(taskId, { ...(replyText ? { reply: replyText } : {}) }),
      (value) => this.mutationMessage(value),
      "已解除阻塞",
    );
  }

  async reopen(taskId: string): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    return await this.act(
      taskId,
      "reopen",
      { ...task, state: "ready" },
      () => api.boardReopen(taskId),
      (value) => this.mutationMessage(value),
      `已重新打开 #${taskId}`,
    );
  }

  async assign(taskId: string, actor: string | null): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    return await this.act(
      taskId,
      "assign",
      { ...task, for: actor },
      () => api.boardAssign(taskId, { for: actor }),
      (value) => this.mutationMessage(value),
      actor ? `已指派 #${taskId}` : `#${taskId} 已回到池中`,
    );
  }

  async setNeeds(taskId: string, needs: string[]): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    return await this.act(
      taskId,
      "needs",
      { ...task, needs: [...needs], state: task.state === "waiting" || task.state === "ready" ? (needs.length ? "waiting" : "ready") : task.state },
      () => api.boardNeeds(taskId, needs),
      (value) => this.mutationMessage(value),
      "前置依赖已更新",
    );
  }

  async cancel(taskId: string, reason: string): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    const task = this.cache().tasks[taskId];
    if (!task) return false;
    return await this.act(
      taskId,
      "cancel",
      { ...task, state: "canceled", claim: null, reason },
      () => api.boardCancel(taskId, { reason }),
      (value) => {
        this.tokens.delete(taskId);
        return this.mutationMessage(value);
      },
      `已取消 #${taskId}`,
    );
  }

  async answer(taskId: string, text: string): Promise<boolean> {
    const api = this.api();
    if (!api?.boardAnswer) {
      this.actionError.set({
        taskId,
        move: "answer",
        failure: { kind: "unavailable", message: "这个部署没有开启超级助理命令桥；请在超级助理中回答。" },
      });
      return false;
    }
    return await this.act(
      taskId,
      "answer",
      null,
      () => api.boardAnswer!(taskId, { text }),
      (value) => this.mutationMessage(value),
      "回答已提交",
    );
  }

  /** A new task (the NewTask sheet). Returns the new root id. */
  async createTask(input: {
    channel: string;
    text: string;
    labels?: Record<string, string>;
    needs?: string[];
    for?: string | null;
    claim?: boolean;
    lease?: number;
  }): Promise<string | null> {
    const api = this.api();
    if (!api) return null;
    const token = input.claim ? newClaimToken() : undefined;
    const fingerprint = JSON.stringify([
      input.channel,
      input.text,
      input.labels ?? {},
      input.needs ?? [],
      input.for ?? null,
      input.claim === true,
      input.lease ?? null,
    ]);
    if (!this.newTaskKey || this.newTaskKey.fingerprint !== fingerprint) {
      this.newTaskKey = {
        fingerprint,
        key: `ui-new-${Date.now()}-${++this.idempotencySeq}`,
      };
    }
    const idempotencyKey = this.newTaskKey.key;
    this.actionInFlight.set({ taskId: "new", move: "create" });
    this.actionError.set(null);
    try {
      const value = await api.boardCreateTask({
        ...input,
        ...(token ? { token } : {}),
        idempotencyKey,
      });
      const message = value.message;
      if (message) {
        this.cache.set(mergeMessage(this.cache(), message));
        if (message.task) {
          this.cache.set(mergeTask(this.cache(), message.id, message.task));
          this.queueIds.update((ids) => insertQueueId(ids, message.id));
        }
        if (value.claim && token) this.tokens.set(message.id, { ...value.claim, token: value.claim.token ?? token });
        this.newTaskKey = null;
        this.flash(`已创建任务 #${message.id}`);
        return message.id;
      }
      return null;
    } catch (error) {
      const failure = boardFailureOf(error);
      this.actionError.set({ taskId: "new", move: "create", failure });
      return null;
    } finally {
      this.actionInFlight.set(null);
    }
  }

  /** A claim that answers an auth/token situation from another path; looks a
   * token up by the task's claim actor when a page was restored. */
  private tokensByTask(taskId: string): BoardTaskClaim | undefined {
    return this.tokens.get(taskId);
  }

  /** Re-read one task through the augmented GET after an SSE frame carried
   * no Phase 2 fields. Per-task tokens keep two events from racing. */
  private async augmentTask(id: string): Promise<void> {
    const api = this.api();
    if (!api) return;
    const token = (this.augmentTokens.get(id) ?? 0) + 1;
    this.augmentTokens.set(id, token);
    try {
      const view = await api.boardThread(id);
      if (this.augmentTokens.get(id) !== token) return;
      this.mergeThreadView(view);
    } catch {
      /* keep the conservative interim view; a later event or manual refresh retries */
    } finally {
      if (this.augmentTokens.get(id) === token) {
        this.augmenting.update((current) => {
          const next = { ...current };
          delete next[id];
          return next;
        });
      }
    }
  }

  // ---- stream -------------------------------------------------------------

  private connectStream(): void {
    const api = this.api();
    if (!api) return;
    this.closeStream?.();
    this.seen.clear();
    this.liveStatus.set("connecting");
    this.closeStream = api.boardStream({
      after: this.liveCursor() || undefined,
      query: queryToTaskQuery(this.query()),
      onState: (status, attempt) => {
        const was = this.liveStatus();
        this.liveStatus.set(status);
        this.liveAttempt.set(attempt);
        if (status === "live" && (was === "reconnecting" || was === "offline")) {
          this.flash("已恢复实时更新");
          void this.loadQueue(true);
          if (this.threadId()) void this.refreshThread();
        }
      },
      onFrame: (frame) => this.mergeFrame(frame),
    });
  }

  /** Roots whose SSE task view lacked Phase 2 fields and are being re-read
   * through the augmented GET path. */
  readonly augmenting = signal<Record<string, boolean>>({});

  /** Test seam: deliver one SSE frame through the same cursor/seen gates as
   * the live feed. A test can prove hidden/duplicate/out-of-order frames do
   * not overwrite the queue. */
  applyFrameForTest(frame: { id: string; event: BoardEventView | null }): void {
    this.mergeFrame(frame);
  }

  private mergeFrame(frame: { id: string; event: BoardEventView | null }): void {
    if (this.seen.has(frame.id)) return;
    const cursor = Number(frame.id);
    const current = Number(this.liveCursor());
    if (Number.isFinite(cursor) && Number.isFinite(current) && cursor <= current) return;
    this.seen.add(frame.id);
    this.liveCursor.set(frame.id);
    if (!frame.event) {
      // A hidden event advances the cursor only.
      return;
    }
    this.lastEventAt.set(Date.now());
    let cache = this.cache();
    const event = frame.event;
    const incoming = event.message;
    if (incoming) {
      let effective = incoming;
      let needsAugment = false;
      const cachedTask = cache.tasks[incoming.id];
      if (incoming.task && cachedTask && cachedTask.moves !== undefined && incoming.task.moves === undefined) {
        // The SSE feed carries the board's raw projection; `moves`/`mirror`/
        // `archived` exist only on the augmented GET response. Treat such an
        // event as an invalidation: keep viewer-dependent facts, prune the
        // old moves to the new state (never show an illegal claim), and
        // refetch the task through the augmented read.
        const interim: BoardTaskView = {
          ...incoming.task,
          moves: interimMovesForState(cachedTask, incoming.task),
          ...(cachedTask.mirror ? { mirror: cachedTask.mirror } : {}),
          ...(cachedTask.archived !== undefined ? { archived: cachedTask.archived } : {}),
          ...(cachedTask.origin ? { origin: cachedTask.origin } : {}),
          ...(cachedTask.copies ? { copies: cachedTask.copies } : {}),
        };
        effective = { ...incoming, task: interim };
        needsAugment = true;
      }
      const activeThread = this.threadId();
      const belongs = activeThread ? effective.id === activeThread || effective.re === activeThread : false;
      if (belongs) {
        cache = mergeMessage(cache, effective);
        const thread = activeThread ? cache.threads[activeThread] : undefined;
        if (
          thread &&
          effective.id !== activeThread &&
          !thread.replies.some((reply: BoardMessageView) => reply.id === effective.id)
        ) {
          // A reply we did not have: the server may have trimmed the page.
          // Refetch rather than leave a hole.
          void this.refreshThread();
        }
      } else {
        cache = mergeMessage(cache, effective);
      }
      this.cache.set(cache);
      if (needsAugment) {
        this.augmenting.update((current) => ({ ...current, [incoming.id]: true }));
        void this.augmentTask(incoming.id);
      }
      // A root's task drives the queue's membership. Only re-evaluate when the
      // event carries a task root; replies leave the row alone.
      if (effective.task) {
        const row = queueRowOf(this.cache(), incoming.id);
        const ids = this.queueIds();
        if (row) {
          const inQuery = matchesBoardQuery(this.cache(), row, this.query(), Date.now());
          if (inQuery && !ids.includes(row.id)) this.queueIds.set(insertQueueId(ids, row.id));
          if (!inQuery && ids.includes(row.id)) this.queueIds.set(ids.filter((id) => id !== row.id));
        } else if (ids.includes(effective.id)) {
          this.queueIds.set(ids.filter((id) => id !== effective.id));
        }
      }
    }
  }

  // ---- sync ---------------------------------------------------------------

  /** `board.manage` is the sync/automation gate; an account without it gets
   * the permission face without an API call. */
  canManage(): boolean {
    return can("board.manage");
  }

  async loadSync(): Promise<void> {
    if (!this.canManage()) {
      this.syncPhase.set("permission");
      this.syncError.set(null);
      return;
    }
    const api = this.api();
    if (!api) {
      this.syncPhase.set("error");
      this.syncError.set({ kind: "network", message: "这个客户端没有任务面传输。" });
      return;
    }
    this.syncPhase.set("loading");
    this.syncError.set(null);
    try {
      const [entries, status, inbox, failures, credentials] = await Promise.all([
        api.boardSyncEntries(),
        api.boardSyncStatus(),
        api.boardSyncInbox(),
        api.boardSyncFailures(),
        api.boardSyncCredentials
          ? api.boardSyncCredentials().catch(() => null)
          : Promise.resolve(null),
      ]);
      const credentialMap: Record<string, BoardSyncCredentialView> = {};
      for (const entry of credentials?.entries ?? []) credentialMap[entry.key] = entry;
      this.sync.set({
        entries: entries.entries,
        status,
        dead: inbox.dead,
        failures: failures.failures,
        credentials: credentialMap,
        credentialsKnown: credentials !== null,
      });
      this.syncPhase.set(entries.entries.length || status.keys.length ? "ready" : "empty");
      this.syncStale.set(false);
    } catch (error) {
      const failure = boardFailureOf(error);
      this.syncError.set(failure);
      if (this.sync()) {
        this.syncStale.set(true);
        this.syncPhase.set("ready");
      } else {
        this.syncPhase.set(failure.kind === "permission" ? "permission" : "error");
      }
    }
  }

  async retryInbox(id: string, key: string): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    this.syncRetrying.update((current) => ({ ...current, [id]: true }));
    try {
      await api.boardSyncRetryInbox(id, key);
      await this.loadSync();
      this.flash("已重新排队");
      return true;
    } catch (error) {
      this.syncError.set(boardFailureOf(error));
      return false;
    } finally {
      this.syncRetrying.update((current) => {
        const next = { ...current };
        delete next[id];
        return next;
      });
    }
  }

  async retryOutbox(id: string): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    this.syncRetrying.update((current) => ({ ...current, [id]: true }));
    try {
      await api.boardSyncRetryOutbox(id);
      await this.loadSync();
      this.flash("已重新排队");
      return true;
    } catch (error) {
      this.syncError.set(boardFailureOf(error));
      return false;
    } finally {
      this.syncRetrying.update((current) => {
        const next = { ...current };
        delete next[id];
        return next;
      });
    }
  }

  /** Retry every dead letter of the selected key (or every key). */
  async retryAllDead(): Promise<void> {
    const data = this.sync();
    if (!data) return;
    const key = this.syncInboxKey();
    const dead = data.dead.filter((item) => !key || item.key === key);
    for (const item of dead) await this.retryInbox(item.id, item.key);
    const failed = data.failures.filter((item) => !key || item.key === key);
    for (const item of failed) await this.retryOutbox(item.delivery);
  }

  // ---- automation ---------------------------------------------------------

  async loadAutomation(): Promise<void> {
    if (!this.canManage()) {
      this.automationPhase.set("permission");
      this.automationError.set(null);
      return;
    }
    const api = this.api();
    if (!api) {
      this.automationPhase.set("error");
      this.automationError.set({ kind: "network", message: "这个客户端没有任务面传输。" });
      return;
    }
    this.automationPhase.set("loading");
    this.automationError.set(null);
    try {
      const [watchers, schedules] = await Promise.all([api.boardWatchers(), api.boardSchedules()]);
      this.automation.set({ watchers: watchers.watchers, schedules: schedules.schedules });
      this.automationPhase.set(watchers.watchers.length || schedules.schedules.length ? "ready" : "empty");
      this.automationStale.set(false);
    } catch (error) {
      const failure = boardFailureOf(error);
      this.automationError.set(failure);
      if (this.automation()) {
        this.automationStale.set(true);
        this.automationPhase.set("ready");
      } else {
        this.automationPhase.set(failure.kind === "permission" ? "permission" : "error");
      }
    }
  }

  async setWatcherEnabled(id: string, enabled: boolean): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    this.automationBusy.update((current) => ({ ...current, [id]: true }));
    const before = this.automation();
    if (before) {
      this.automation.set({
        ...before,
        watchers: before.watchers.map((watcher) => (watcher.id === id ? { ...watcher, enabled } : watcher)),
      });
    }
    try {
      const updated = await api.boardWatcherEnable(id, enabled);
      const data = this.automation();
      if (data) {
        this.automation.set({
          ...data,
          watchers: data.watchers.map((watcher) => (watcher.id === id ? { ...watcher, ...updated } : watcher)),
        });
      }
      this.flash(enabled ? `已启用 ${id}` : `已停用 ${id}`);
      return true;
    } catch (error) {
      if (before) this.automation.set(before);
      this.automationError.set(boardFailureOf(error));
      this.flash("开关没有生效", "warning");
      return false;
    } finally {
      this.automationBusy.update((current) => {
        const next = { ...current };
        delete next[id];
        return next;
      });
    }
  }

  async setScheduleEnabled(id: string, enabled: boolean): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    this.automationBusy.update((current) => ({ ...current, [id]: true }));
    const before = this.automation();
    if (before) {
      this.automation.set({
        ...before,
        schedules: before.schedules.map((schedule) =>
          schedule.id === id
            ? { ...schedule, status: enabled ? "pending" : "disabled", nextRunAt: schedule.nextRunAt }
            : schedule,
        ),
      });
    }
    try {
      const updated = await api.boardScheduleEnable(id, enabled);
      const data = this.automation();
      if (data) {
        this.automation.set({
          ...data,
          schedules: data.schedules.map((schedule) => (schedule.id === id ? { ...schedule, ...updated } : schedule)),
        });
      }
      this.flash(enabled ? "定时任务已继续" : "定时任务已暂停");
      return true;
    } catch (error) {
      if (before) this.automation.set(before);
      this.automationError.set(boardFailureOf(error));
      this.flash("开关没有生效", "warning");
      return false;
    } finally {
      this.automationBusy.update((current) => {
        const next = { ...current };
        delete next[id];
        return next;
      });
    }
  }

  async cancelSchedule(id: string, reason = "取消定时任务"): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    this.automationBusy.update((current) => ({ ...current, [id]: true }));
    const status = this.automation()?.schedules.find((schedule) => schedule.id === id)?.status;
    try {
      const updated = await api.boardScheduleCancel(id, { reason, status });
      const data = this.automation();
      if (data) {
        // A dead plan is removed (`null`); a live plan answers its canceled
        // view and stays in the list.
        const schedules = updated
          ? data.schedules.map((schedule) => (schedule.id === id ? { ...schedule, ...updated } : schedule))
          : data.schedules.filter((schedule) => schedule.id !== id);
        this.automation.set({ ...data, schedules });
      }
      this.flash("定时任务已取消");
      return true;
    } catch (error) {
      this.automationError.set(boardFailureOf(error));
      return false;
    } finally {
      this.automationBusy.update((current) => {
        const next = { ...current };
        delete next[id];
        return next;
      });
    }
  }

  async retryWatcherDelivery(watcherId: string, seq: string): Promise<boolean> {
    const api = this.api();
    if (!api) return false;
    try {
      await api.boardWatcherRetryDelivery(watcherId, seq);
      await this.loadAutomation();
      await this.loadWatcherDeliveries(watcherId);
      this.flash("已重新排队");
      return true;
    } catch (error) {
      this.automationError.set(boardFailureOf(error));
      return false;
    }
  }

  /** The watcher's recent deliveries (read-only). */
  async loadWatcherDeliveries(id: string): Promise<void> {
    const api = this.api();
    if (!api) return;
    this.watcherDeliveryBusy.set(id);
    try {
      const result = await api.boardWatcherDeliveries(id);
      this.watcherDeliveries.update((current) => ({ ...current, [id]: result.deliveries }));
    } catch (error) {
      this.automationError.set(boardFailureOf(error));
    } finally {
      this.watcherDeliveryBusy.set(null);
    }
  }

  /** Retry every dead delivery of one watcher (the row's own action). */
  async retryWatcherDead(id: string): Promise<void> {
    const api = this.api();
    if (!api) return;
    this.watcherDeliveryBusy.set(id);
    try {
      const result = await api.boardWatcherDeliveries(id);
      this.watcherDeliveries.update((current) => ({ ...current, [id]: result.deliveries }));
      for (const delivery of result.deliveries) {
        if (delivery.state === "dead" || delivery.lastError) {
          await api.boardWatcherRetryDelivery(id, delivery.event);
        }
      }
      await this.loadAutomation();
      this.flash("已重新排队");
    } catch (error) {
      this.automationError.set(boardFailureOf(error));
    } finally {
      this.watcherDeliveryBusy.set(null);
    }
  }

  // ---- misc ---------------------------------------------------------------

  flash(text: string, tone: BoardNotice["tone"] = "success"): void {
    this.notice.set({ tone, text });
    if (this.noticeTimer !== undefined) clearTimeout(this.noticeTimer);
    this.noticeTimer = setTimeout(() => this.notice.set(null), 4000) as unknown as number;
  }

  dismissNotice(): void {
    this.notice.set(null);
  }

  private startClock(): void {
    if (this.clockTimer !== undefined) return;
    this.clock.set(Date.now());
    this.clockTimer = setInterval(() => this.clock.set(Date.now()), 1000) as unknown as number;
  }
}

/** The token map is keyed by the root id. */
export const boardStore = new BoardStoreImpl();
export { BoardStoreImpl as BoardStore };
export type { BoardApi, BoardEventView, BoardMessageView, BoardTaskView, BoardThreadView, BoardClaimView };
