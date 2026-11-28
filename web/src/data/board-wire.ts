// The board's wire contract (keel-board v0.1.1) and the pure vocabulary the
// data layer, the fixture and the two hosts share.
//
// The server is the one authority: `GET /v1/board/*` returns message/task
// projections, the SSE feed carries the same views, and every write is decided
// by the server. This module only names the shapes; it never folds the log.
//
// Design contract additions (documented in openspec/changes/keel-board-v1/ui
// DESIGN §15): `TaskView.moves`, `task.archived`, `origin` and the
// request-level `takeover`/`release.force` actions are optional here because a
// board version may not emit them yet. A view without `moves` is read only —
// the UI MUST NOT invent an action from `source=`, a label or a client state
// machine. The deleted mirror/bridge fields are not part of the contract.

/** A board reference: the decimal sequence as a string (`"812"`). */
export type BoardRef = string;

export interface BoardActorView {
  id?: string;
  kind?: string;
  handle?: string;
  name?: string;
  avatar?: string;
  platform?: string;
  verified?: boolean;
}

export interface BoardChannelView {
  path: string;
  lastIndex: number;
  messages: number;
  threads: number;
  can?: { read?: boolean; write?: boolean; manage?: boolean };
}

export interface BoardClaimView {
  actor: string;
  until: string;
}

export interface BoardHoldView {
  kind?: string;
  reason: string;
}

export interface BoardTaskOrigin {
  platform?: string;
  ref?: string;
}

/** The board's task projection, with the Phase 2 contract additions optional. */
export interface BoardTaskView {
  state: string;
  owner: string;
  by: string;
  for?: string | null;
  needs: BoardRef[];
  epoch: number;
  claim?: BoardClaimView | null;
  blocked?: BoardHoldView | null;
  result?: string | null;
  reason?: string | null;
  author?: BoardActorView | null;
  /** The server's authoritative action list. Absent means read-only. */
  moves?: string[];
  /** Archived tasks are read-only history, not a second task face. */
  archived?: boolean;
  origin?: BoardTaskOrigin;
  /** Optional external copies; absent when the deployment has no sync facts. */
  copies?: BoardTaskCopy[];
}

export interface BoardTaskCopy {
  target?: string;
  platform?: string;
  root?: BoardRef;
  status?: string;
  lastError?: string;
  attempts?: number;
  undelivered?: number;
}

export interface BoardMessageView {
  id: BoardRef;
  channel: string;
  text: string;
  re?: BoardRef | null;
  replyTo?: BoardRef | null;
  thread?: string;
  labels?: Record<string, string>;
  reactions?: Record<string, BoardRef[]>;
  noSync?: boolean;
  from?: string | null;
  createdAt: string;
  editedAt?: string | null;
  deleted?: boolean;
  undelivered?: Record<string, string>;
  attachments?: Array<{ id: string; name: string; type: string; size: number }>;
  links?: Array<{ url: string; message?: string | null; snapshot?: string | null; unavailable?: string }>;
  version?: number;
  revision?: number;
  task?: BoardTaskView | null;
  blind?: boolean;
  blocks?: Array<{ schema: string; version: number; data: Record<string, unknown> }>;
  origin?: unknown;
  by?: BoardActorView | null;
  author?: BoardActorView | null;
  /** The design's "from Slack" provenance; absent until G5 lands. */
  source?: BoardTaskOrigin | null;
  /** External copies on a message; absent until G5 lands. */
  copies?: BoardTaskCopy[];
}

export interface BoardThreadSummary {
  message: BoardMessageView;
  replies: number;
  last?: BoardMessageView | null;
}

export interface BoardTasksPage {
  tasks: BoardThreadSummary[];
  cursor: BoardRef;
}

export interface BoardThreadView {
  message: BoardMessageView;
  replies: BoardMessageView[];
  cursor: BoardRef;
  /** The reply count a queue summary reported (not a wire field). */
  replyCount?: number;
  /** Once the server answers a `before` cursor (G3). */
  older?: string | null;
}

export interface BoardChannelsPage {
  channels: BoardChannelView[];
  cursor: BoardRef;
  next?: string | null;
}

export interface BoardActorsPage {
  actors: Array<BoardActorView & { id: string }>;
  cursor: BoardRef;
}

export interface BoardMeView {
  account?: { id?: string; username?: string; displayName?: string };
  bot?: { id?: string; name?: string } | null;
  actor: string;
  actorName?: string;
  can?: { board?: { read?: boolean; write?: boolean; manage?: boolean } };
  channels?: Record<string, { read?: boolean; write?: boolean; manage?: boolean }>;
}

/** One SSE frame: `cursor` is the board seq; `type` is the event kind. A view
 * carries `message` (and therefore its task when the event concerns a root). */
export interface BoardEventView {
  cursor: BoardRef;
  type: string;
  createdAt: string;
  payload?: Record<string, unknown>;
  message?: BoardMessageView | null;
  actor?: { id: string; kind?: string; handle?: string; name?: string } | null;
}

/** What the stream reports. `hidden` frames only advance the cursor. */
export interface BoardStreamFrame {
  id: BoardRef;
  event: BoardEventView | null;
}

// ---- sync management ------------------------------------------------------

export interface BoardSyncEntryView {
  key: string;
  platform: string;
  enabled: boolean;
  owned: boolean;
  createdAt: string;
  updatedAt: string;
  inbox?: { pending: number; leased: number; done: number; dead: number };
  outbox?: { pending: number; leased: number; done: number; dead: number };
  /** board.manage health facts. `undefined` means the mounted board didn't
   * send the field; `null` lag means the board couldn't measure it. */
  credential?: "valid" | "invalid" | "unknown";
  routes?: Array<{ from: string; to: string }>;
  targets?: BoardSyncTargetView[];
  /** Measured queues; a null member is an empty queue, not unknown. */
  lag?: BoardSyncLag | null;
  /** The design's flat view of the oldest outstanding row across both queues.
   * keel derives it from lag when the mounted board does not send one; null
   * means no outstanding row or no measurement, never zero seconds. */
  oldestPendingAgeSeconds?: number | null;
  failures24h?: number;
  bridgeHost?: string;
  bridgeErrors?: number;
}

/** One measured queue: no outstanding row is represented by a null queue at
 * the `BoardSyncLag` level, so it never reads as a zero-second lag. */
export interface BoardSyncQueueLag {
  outstanding: number;
  oldestAt?: string;
  ageSeconds?: number;
}

export interface BoardSyncLag {
  inbox: BoardSyncQueueLag | null;
  outbox: BoardSyncQueueLag | null;
}

export interface BoardSyncTargetView {
  target: string;
  root?: BoardRef;
  status: string;
  pending?: number;
  leased?: number;
  dead?: number;
  attempts?: number;
  lastError?: string;
  undelivered?: number;
  lastDeliveryAt?: string;
}

/** `GET /v1/board/sync/credentials` (keel adapter): whether each entry has a
 * usable credential. Never a value. */
export interface BoardSyncCredentialView {
  key: string;
  platform: string;
  enabled: boolean;
  credential: "valid" | "invalid" | "missing" | "unknown";
  restartRequired?: boolean;
}

export interface BoardSyncStatusView {
  keys: BoardSyncEntryView[];
  outbox: { pending: number; leased: number; done: number; dead: number };
  /** Tenant-wide measured queues; null/undefined means unknown. */
  lag?: BoardSyncLag | null;
  failures24h?: number;
  updatedAt?: string;
  syncEnabled?: boolean;
}

export interface BoardSyncInboxItem {
  id: string;
  key: string;
  thread: string;
  attempts: number;
  dedupe?: string;
  receivedAt?: string;
  status?: string;
  lastError?: string;
}

export interface BoardSyncFailure {
  delivery: string;
  message: string;
  target: string;
  attempts: number;
  error: string;
  key?: string;
  at?: string;
}

// ---- automation -----------------------------------------------------------

export interface BoardWatcherView {
  id: string;
  channel: string;
  filter?: { types?: string[]; labels?: Record<string, string> };
  action: string;
  bot?: string;
  enabled: boolean;
  cursor?: string;
  createdAt: string;
  updatedAt: string;
  /** Board v0.1.2 watcher health: the optional stats reader. */
  lastDeliveryAt?: string;
  dead?: number;
  lastError?: string;
}

export interface BoardWatcherDeliveryDepth {
  pending: number;
  leased: number;
  done: number;
  dead: number;
}

export interface BoardWatcherDelivery {
  watcherId: string;
  event: string;
  state: string;
  attempts: number;
  nextAt: string;
  lastError?: string;
  createdAt: string;
  updatedAt: string;
  completedAt?: string;
}

export interface BoardScheduleView {
  id: string;
  channel: string;
  re?: string;
  labels?: Record<string, string>;
  creator: string;
  nextRunAt: string;
  everySeconds?: number;
  untilAt?: string;
  times?: number;
  sent: number;
  status: string;
  attempts: number;
  lastError?: string;
  cancelReason?: string;
  createdAt: string;
  updatedAt: string;
  completedAt?: string;
}

/** The read seam every transport implements. A transport that predates the
 * board surface simply does not have it (`store.boardApi()` answers null). */
export interface BoardApi {
  boardMe?(): Promise<BoardMeView>;
  boardChannels(after?: string, limit?: number): Promise<BoardChannelsPage>;
  boardActors(limit?: number): Promise<BoardActorsPage>;
  boardTasks(query: BoardTaskQuery): Promise<BoardTasksPage>;
  boardThread(id: BoardRef, after?: string, limit?: number): Promise<BoardThreadView>;
  boardCreateTask(input: BoardCreateTaskInput): Promise<{ message: BoardMessageView; claim?: BoardTaskClaim }>;
  boardReply(input: BoardReplyInput): Promise<{ message: BoardMessageView }>;
  boardClaim(id: BoardRef, input: { lease?: number; token?: string }): Promise<BoardTaskMutation>;
  boardClaimReady(input: {
    channel: string;
    label?: Record<string, string>;
    from?: string[];
    lease?: number;
    wait?: number;
    token?: string;
  }): Promise<{ task: BoardThreadSummary | null; claim?: BoardTaskClaim }>;
  boardRenew(id: BoardRef, input: { epoch: number; token: string; lease?: number }): Promise<BoardTaskMutation>;
  boardRelease(
    id: BoardRef,
    input: { epoch?: number; token?: string; force?: boolean },
  ): Promise<BoardTaskMutation>;
  boardTakeover?(id: BoardRef): Promise<BoardTaskMutation>;
  boardComplete(
    id: BoardRef,
    input: { epoch: number; token: string; result?: string },
  ): Promise<BoardTaskMutation>;
  boardFail(id: BoardRef, input: { epoch: number; token: string; reason: string }): Promise<BoardTaskMutation>;
  boardBlock(
    id: BoardRef,
    input: { reason: string; kind?: string; epoch?: number; token?: string },
  ): Promise<BoardTaskMutation>;
  boardUnblock(id: BoardRef, input: { reply?: string }): Promise<BoardTaskMutation>;
  boardReopen(id: BoardRef): Promise<BoardTaskMutation>;
  boardAssign(id: BoardRef, input: { for: string | null }): Promise<BoardTaskMutation>;
  boardNeeds(id: BoardRef, needs: BoardRef[]): Promise<BoardTaskMutation>;
  boardCancel(id: BoardRef, input: { reason: string }): Promise<BoardTaskMutation>;
  boardSyncEntries(): Promise<{ entries: BoardSyncEntryView[] }>;
  /** The credential health read (keel adapter); optional for a board v0.1.1
   * transport that has no tenant secret store yet. */
  boardSyncCredentials?(): Promise<{ entries: BoardSyncCredentialView[] }>;
  boardSyncStatus(): Promise<BoardSyncStatusView>;
  boardSyncInbox(key?: string): Promise<{ dead: BoardSyncInboxItem[] }>;
  boardSyncRetryInbox(id: string, key: string): Promise<{ retried: boolean }>;
  boardSyncFailures(key?: string): Promise<{ failures: BoardSyncFailure[] }>;
  boardSyncRetryOutbox(id: string): Promise<{ retried: boolean }>;
  boardWatchers(): Promise<{ watchers: BoardWatcherView[] }>;
  boardWatcherEnable(id: string, enabled: boolean): Promise<BoardWatcherView>;
  boardWatcherDeliveries(id: string): Promise<{ deliveries: BoardWatcherDelivery[]; depth?: BoardWatcherDeliveryDepth }>;
  boardWatcherRetryDelivery(id: string, seq: string): Promise<{ retried: boolean }>;
  boardSchedules(): Promise<{ schedules: BoardScheduleView[] }>;
  boardScheduleEnable(id: string, enabled: boolean): Promise<BoardScheduleView>;
  /** Cancel a schedule: DELETE for a live plan, `/clear` for a dead one. A
   * dead plan is removed (null), a live plan answers its canceled view. */
  boardScheduleCancel(
    id: string,
    opts: { reason?: string; status?: string },
  ): Promise<BoardScheduleView | null>;
  boardStream(opts: BoardStreamOptions): () => void;
}

export interface BoardTaskClaim {
  epoch: number;
  token?: string;
  until: string;
}

export interface BoardTaskMutation {
  message?: BoardMessageView;
  task?: BoardTaskView;
  claim?: BoardTaskClaim;
  until?: string;
}

export interface BoardCreateTaskInput {
  channel: string;
  text: string;
  labels?: Record<string, string>;
  needs?: BoardRef[];
  for?: string | null;
  claim?: boolean;
  lease?: number;
  token?: string;
  idempotencyKey?: string;
}

export interface BoardReplyInput {
  re: BoardRef;
  text: string;
  idempotencyKey?: string;
  ifRevision?: number;
}

/** The queue read's query. `state` is the server's tab (open / one state /
 * all); the rest are the filter bar's facts. */
export interface BoardTaskQuery {
  state?: string;
  channel?: string;
  labels?: Array<{ key: string; value?: string }>;
  owner?: string;
  for?: string;
  by?: string;
  needs?: BoardRef;
  after?: string;
  limit?: number;
}

export interface BoardStreamOptions {
  after?: string;
  query?: BoardTaskQuery;
  onFrame: (frame: BoardStreamFrame) => void;
  onState: (status: BoardLiveStatus, attempt: number) => void;
}

export type BoardLiveStatus = "connecting" | "live" | "reconnecting" | "offline" | "closed";

// ---- parsing --------------------------------------------------------------

const recordOf = (value: unknown): Record<string, unknown> | null =>
  value !== null && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : null;

const textOf = (value: unknown): string | undefined =>
  typeof value === "string" && value.trim() !== "" ? value : undefined;

const stringRecord = (value: unknown): Record<string, string> => {
  const row = recordOf(value);
  if (!row) return {};
  const out: Record<string, string> = {};
  for (const [key, entry] of Object.entries(row)) {
    if (typeof entry === "string") out[key] = entry;
  }
  return out;
};

const refOf = (value: unknown): string | undefined => {
  if (typeof value === "number" && Number.isFinite(value) && value > 0) return String(value);
  if (typeof value === "string" && value.trim() !== "") return value;
  return undefined;
};

const refList = (value: unknown): string[] => {
  if (!Array.isArray(value)) return [];
  const out: string[] = [];
  for (const entry of value) {
    const ref = refOf(entry);
    if (ref) out.push(ref);
  }
  return out;
};

const numberOr = (value: unknown, fallback = 0): number =>
  typeof value === "number" && Number.isFinite(value) ? value : fallback;

export function parseActor(value: unknown): BoardActorView | undefined {
  const row = recordOf(value);
  if (!row) return undefined;
  const id = textOf(row.id);
  const handle = textOf(row.handle);
  const name = textOf(row.name);
  if (!id && !handle && !name) return undefined;
  const actor: BoardActorView = {};
  if (id) actor.id = id;
  if (handle) actor.handle = handle;
  if (name) actor.name = name;
  const kind = textOf(row.kind);
  if (kind) actor.kind = kind;
  const avatar = textOf(row.avatar);
  if (avatar) actor.avatar = avatar;
  const platform = textOf(row.platform);
  if (platform) actor.platform = platform;
  if (row.verified === true) actor.verified = true;
  return actor;
}

export function parseTask(value: unknown): BoardTaskView | undefined {
  const row = recordOf(value);
  if (!row) return undefined;
  const state = textOf(row.state);
  if (!state) return undefined;
  const task: BoardTaskView = {
    state,
    owner: String(row.owner ?? ""),
    by: String(row.by ?? ""),
    needs: refList(row.needs),
    epoch: numberOr(row.epoch),
  };
  if (row.for !== undefined) task.for = row.for === null ? null : String(row.for);
  const claim = recordOf(row.claim);
  if (claim) {
    task.claim = { actor: String(claim.actor ?? ""), until: String(claim.until ?? "") };
  } else if (row.claim === null) {
    task.claim = null;
  }
  const blocked = recordOf(row.blocked);
  if (blocked) {
    task.blocked = { reason: String(blocked.reason ?? "") };
    const kind = textOf(blocked.kind);
    if (kind) task.blocked.kind = kind;
  } else if (row.blocked === null) {
    task.blocked = null;
  }
  if (row.result !== undefined) task.result = row.result === null ? null : String(row.result);
  if (row.reason !== undefined) task.reason = row.reason === null ? null : String(row.reason);
  const author = parseActor(row.author);
  if (author) task.author = author;
  if (Array.isArray(row.moves)) {
    task.moves = row.moves.filter((move): move is string => typeof move === "string");
  }
  // The deleted superagent mirror/bridge fields (`mirror.readOnly`,
  // `mirror.bridge`) are intentionally not parsed: a projector's task is an
  // ordinary board task, and `moves` is the only action authority.
  if (typeof row.archived === "boolean") task.archived = row.archived;
  const origin = recordOf(row.origin);
  if (origin) {
    task.origin = {};
    if (typeof origin.platform === "string") task.origin.platform = origin.platform;
    if (typeof origin.ref === "string") task.origin.ref = origin.ref;
  }
  if (Array.isArray(row.copies)) task.copies = parseCopies(row.copies);
  return task;
}

function parseCopies(value: unknown): BoardTaskCopy[] {
  if (!Array.isArray(value)) return [];
  const out: BoardTaskCopy[] = [];
  for (const entry of value) {
    const row = recordOf(entry);
    if (!row) continue;
    const copy: BoardTaskCopy = {};
    if (typeof row.target === "string") copy.target = row.target;
    if (typeof row.platform === "string") copy.platform = row.platform;
    const root = refOf(row.root);
    if (root) copy.root = root;
    if (typeof row.status === "string") copy.status = row.status;
    if (typeof row.lastError === "string") copy.lastError = row.lastError;
    if (typeof row.attempts === "number") copy.attempts = row.attempts;
    if (typeof row.undelivered === "number") copy.undelivered = row.undelivered;
    out.push(copy);
  }
  return out;
}

export function parseMessage(value: unknown): BoardMessageView | undefined {
  const row = recordOf(value);
  if (!row) return undefined;
  const id = refOf(row.id);
  if (!id) return undefined;
  const message: BoardMessageView = {
    id,
    channel: String(row.channel ?? ""),
    text: String(row.text ?? ""),
    createdAt: String(row.createdAt ?? ""),
  };
  const re = refOf(row.re);
  if (re) message.re = re;
  else if (row.re === null) message.re = null;
  const replyTo = refOf(row.replyTo);
  if (replyTo) message.replyTo = replyTo;
  if (typeof row.thread === "string") message.thread = row.thread;
  message.labels = stringRecord(row.labels);
  const reactions = recordOf(row.reactions);
  if (reactions) {
    const out: Record<string, string[]> = {};
    for (const [emoji, refs] of Object.entries(reactions)) out[emoji] = refList(refs);
    message.reactions = out;
  }
  if (row.noSync === true) message.noSync = true;
  if (row.from !== undefined) message.from = row.from === null ? null : String(row.from);
  if (row.editedAt !== undefined) message.editedAt = row.editedAt === null ? null : String(row.editedAt);
  if (row.deleted === true) message.deleted = true;
  const undelivered = recordOf(row.undelivered);
  if (undelivered) {
    const out: Record<string, string> = {};
    for (const [target, status] of Object.entries(undelivered)) {
      if (typeof status === "string") out[target] = status;
    }
    message.undelivered = out;
  }
  if (Array.isArray(row.attachments)) {
    message.attachments = row.attachments
      .map((entry) => recordOf(entry))
      .filter((entry): entry is Record<string, unknown> => entry !== null)
      .map((entry) => ({
        id: String(entry.id ?? ""),
        name: String(entry.name ?? ""),
        type: String(entry.type ?? ""),
        size: numberOr(entry.size),
      }));
  }
  if (Array.isArray(row.links)) {
    message.links = row.links
      .map((entry) => recordOf(entry))
      .filter((entry): entry is Record<string, unknown> => entry !== null)
      .map((entry) => ({
        url: String(entry.url ?? ""),
        message: typeof entry.message === "string" ? entry.message : null,
        snapshot: typeof entry.snapshot === "string" ? entry.snapshot : null,
        unavailable: typeof entry.unavailable === "string" ? entry.unavailable : undefined,
      }));
  }
  if (typeof row.version === "number") message.version = row.version;
  if (typeof row.revision === "number") message.revision = row.revision;
  const task = parseTask(row.task);
  if (task) message.task = task;
  if (row.blind === true) message.blind = true;
  if (Array.isArray(row.blocks)) {
    message.blocks = row.blocks
      .map((entry) => recordOf(entry))
      .filter((entry): entry is Record<string, unknown> => entry !== null)
      .map((entry) => ({
        schema: String(entry.schema ?? ""),
        version: numberOr(entry.version, 1),
        data: recordOf(entry.data) ?? {},
      }));
  }
  const by = parseActor(row.by);
  if (by) message.by = by;
  const author = parseActor(row.author);
  if (author) message.author = author;
  const source = recordOf(row.source);
  if (source) {
    message.source = {};
    if (typeof source.platform === "string") message.source.platform = source.platform;
    if (typeof source.ref === "string") message.source.ref = source.ref;
  }
  if (Array.isArray(row.copies)) message.copies = parseCopies(row.copies);
  return message;
}

export function parseThreadSummary(value: unknown): BoardThreadSummary | undefined {
  const row = recordOf(value);
  if (!row) return undefined;
  const message = parseMessage(row.message);
  if (!message) return undefined;
  const last = parseMessage(row.last);
  return {
    message,
    replies: numberOr(row.replies),
    ...(last ? { last } : {}),
  };
}

export function parseTasksPage(value: unknown): BoardTasksPage {
  const row = recordOf(value) ?? {};
  const tasks: BoardThreadSummary[] = [];
  if (Array.isArray(row.tasks)) {
    for (const entry of row.tasks) {
      const summary = parseThreadSummary(entry);
      if (summary) tasks.push(summary);
    }
  }
  return { tasks, cursor: refOf(row.cursor) ?? "" };
}

export function parseThreadView(value: unknown): BoardThreadView | undefined {
  const row = recordOf(value);
  if (!row) return undefined;
  const message = parseMessage(row.message);
  if (!message) return undefined;
  const replies: BoardMessageView[] = [];
  if (Array.isArray(row.replies)) {
    for (const entry of row.replies) {
      const reply = parseMessage(entry);
      if (reply) replies.push(reply);
    }
  }
  return {
    message,
    replies,
    cursor: refOf(row.cursor) ?? "",
    ...(typeof row.older === "string" ? { older: row.older } : {}),
  };
}

export function parseChannelsPage(value: unknown): BoardChannelsPage {
  const row = recordOf(value) ?? {};
  const channels: BoardChannelView[] = [];
  if (Array.isArray(row.channels)) {
    for (const entry of row.channels) {
      const channel = recordOf(entry);
      if (!channel || typeof channel.path !== "string") continue;
      const can = recordOf(channel.can);
      channels.push({
        path: channel.path,
        lastIndex: numberOr(channel.lastIndex),
        messages: numberOr(channel.messages),
        threads: numberOr(channel.threads),
        ...(can
          ? {
              can: {
                read: can.read === true,
                write: can.write === true,
                manage: can.manage === true,
              },
            }
          : {}),
      });
    }
  }
  return {
    channels,
    cursor: refOf(row.cursor) ?? "",
    ...(typeof row.next === "string" ? { next: row.next } : {}),
  };
}

export function parseActorsPage(value: unknown): BoardActorsPage {
  const row = recordOf(value) ?? {};
  const actors: Array<BoardActorView & { id: string }> = [];
  if (Array.isArray(row.actors)) {
    for (const entry of row.actors) {
      const actor = parseActor(entry);
      const id = recordOf(entry)?.id;
      if (!actor || typeof id !== "string") continue;
      actors.push({ ...actor, id });
    }
  }
  return { actors, cursor: refOf(row.cursor) ?? "" };
}

export function parseEventView(value: unknown): BoardEventView | undefined {
  const row = recordOf(value);
  if (!row) return undefined;
  const cursor = refOf(row.cursor);
  const type = textOf(row.type);
  if (!cursor || !type) return undefined;
  const message = parseMessage(row.message);
  const event: BoardEventView = {
    cursor,
    type,
    createdAt: String(row.createdAt ?? ""),
    payload: recordOf(row.payload) ?? {},
  };
  if (message) event.message = message;
  const actor = recordOf(row.actor);
  if (actor) {
    event.actor = { id: String(actor.id ?? "") };
    if (typeof actor.kind === "string") event.actor.kind = actor.kind;
    if (typeof actor.handle === "string") event.actor.handle = actor.handle;
    if (typeof actor.name === "string") event.actor.name = actor.name;
  }
  return event;
}

/** One SSE frame from its `id:` and `data:` fields. A frame with no data only
 * advances the cursor (`event: null`). */
export function parseStreamFrame(id: string, data: string): BoardStreamFrame | null {
  const ref = refOf(id);
  if (!ref) return null;
  if (!data.trim()) return { id: ref, event: null };
  try {
    return { id: ref, event: parseEventView(JSON.parse(data)) ?? null };
  } catch {
    return { id: ref, event: null };
  }
}

/** `label=key=value` query values, as the board's list API reads them. */
export function labelQueryValue(label: { key: string; value?: string }): string {
  return label.value === undefined || label.value === "" ? label.key : `${label.key}=${label.value}`;
}

/** The query string a tasks read sends. A `for=pool` filter is client-side
 * (board v0.1.1 has no pool token, G9), so it is not encoded here. */
export function taskQueryParams(query: BoardTaskQuery): string[] {
  const params: string[] = [];
  if (query.state && query.state !== "all") params.push(`state=${encodeURIComponent(query.state)}`);
  if (query.state === "all") params.push("state=all");
  if (query.channel) params.push(`channel=${encodeURIComponent(query.channel)}`);
  for (const label of query.labels ?? []) {
    params.push(`label=${encodeURIComponent(labelQueryValue(label))}`);
  }
  if (query.owner) params.push(`owner=${encodeURIComponent(query.owner)}`);
  if (query.for && query.for !== "pool") params.push(`for=${encodeURIComponent(query.for)}`);
  if (query.by) params.push(`by=${encodeURIComponent(query.by)}`);
  if (query.needs) params.push(`needs=${encodeURIComponent(query.needs)}`);
  if (query.after) params.push(`after=${encodeURIComponent(query.after)}`);
  if (query.limit) params.push(`limit=${encodeURIComponent(String(query.limit))}`);
  return params;
}

/** The stream query string. The board's SSE route takes channel/label/all;
 * state/owner/for are client-side hides (the server replays every visible
 * event and the client filters the queue), matching design §12.1's note that
 * a hidden event still advances the cursor. */
export function streamQueryParams(query: BoardTaskQuery | undefined, after: string | undefined): string[] {
  const params: string[] = [];
  if (query?.channel) params.push(`channel=${encodeURIComponent(query.channel)}`);
  for (const label of query?.labels ?? []) {
    params.push(`label=${encodeURIComponent(labelQueryValue(label))}`);
  }
  if (after) params.push(`after=${encodeURIComponent(after)}`);
  return params;
}

// ---- the cache ------------------------------------------------------------

export interface BoardCache {
  channels: Record<string, BoardChannelView>;
  messages: Record<string, BoardMessageView>;
  threads: Record<string, BoardThreadView>;
  tasks: Record<string, BoardTaskView>;
  actors: Record<string, BoardActorView & { id: string }>;
  labels: Record<string, string[]>;
  needs: Record<string, string[]>;
  claims: Record<string, BoardClaimView>;
}

export const emptyBoardCache = (): BoardCache => ({
  channels: {},
  messages: {},
  threads: {},
  tasks: {},
  actors: {},
  labels: {},
  needs: {},
  claims: {},
});

const copyRecord = <T>(value: Record<string, T>): Record<string, T> => ({ ...value });

/** The label facets the filter bar suggests: key → its sorted distinct values.
 * `board.*` keys are reserved and never offered as editable filters. */
function indexLabels(cache: BoardCache, message: BoardMessageView): void {
  for (const [key, value] of Object.entries(message.labels ?? {})) {
    if (key === "board.task" || key.startsWith("board.")) continue;
    const values = cache.labels[key] ?? [];
    if (!values.includes(value)) {
      cache.labels[key] = [...values, value].sort();
    }
  }
}

/** Merge one message (and its task) into the cache. The message is the only
 * carrier of a task view, so both move together. */
export function mergeMessage(cache: BoardCache, message: BoardMessageView): BoardCache {
  const existing = cache.messages[message.id];
  // A view's version is monotonic. A late GET/replay must not overwrite a
  // newer SSE view; equal/absent versions still merge (labels, edits without
  // a version bump in a fixture).
  if (
    existing &&
    existing.version !== undefined &&
    message.version !== undefined &&
    existing.version > message.version
  ) {
    return cache;
  }
  const next: BoardCache = {
    ...cache,
    messages: copyRecord(cache.messages),
    tasks: copyRecord(cache.tasks),
    claims: copyRecord(cache.claims),
    needs: copyRecord(cache.needs),
    labels: copyRecord(cache.labels),
    threads: copyRecord(cache.threads),
    channels: copyRecord(cache.channels),
    actors: copyRecord(cache.actors),
  };
  next.messages[message.id] = message;
  if (message.channel) {
    const existing = next.channels[message.channel];
    if (existing) {
      next.channels[message.channel] = {
        ...existing,
        lastIndex: Math.max(existing.lastIndex, Number(message.id) || 0),
        messages: existing.messages + (cache.messages[message.id] ? 0 : 1),
      };
    } else {
      next.channels[message.channel] = {
        path: message.channel,
        lastIndex: Number(message.id) || 0,
        messages: 1,
        threads: 0,
      };
    }
  }
  const task = message.task;
  if (task) {
    next.tasks[message.id] = task;
    next.needs[message.id] = [...task.needs];
    if (task.claim) next.claims[message.id] = { ...task.claim };
    else delete next.claims[message.id];
  } else if (cache.tasks[message.id]) {
    // A view without a task is not proof that a task disappeared: the event
    // may be a reply. Only an explicit null on a root drops the task.
    if (message.task === null) {
      delete next.tasks[message.id];
      delete next.needs[message.id];
      delete next.claims[message.id];
    }
  }
  indexLabels(next, message);
  const threadKey = message.re ?? null;
  if (threadKey) {
    const thread = next.threads[threadKey];
    if (thread) {
      const replies = thread.replies.map((reply) => (reply.id === message.id ? message : reply));
      if (!replies.some((reply) => reply.id === message.id)) replies.push(message);
      next.threads[threadKey] = {
        ...thread,
        replies,
        replyCount: Math.max(thread.replyCount ?? 0, replies.length),
      };
    }
  } else {
    const thread = next.threads[message.id];
    if (thread) next.threads[message.id] = { ...thread, message };
  }
  return next;
}

/** Merge a thread read: the root and its replies replace the cached thread. */
export function mergeThread(cache: BoardCache, view: BoardThreadView): BoardCache {
  let next = mergeMessage(cache, view.message);
  for (const reply of view.replies) next = mergeMessage(next, reply);
  next = {
    ...next,
    threads: {
      ...next.threads,
      [view.message.id]: {
        message: next.messages[view.message.id] ?? view.message,
        replies: view.replies.map((reply) => next.messages[reply.id] ?? reply),
        cursor: view.cursor,
        replyCount: view.replyCount ?? view.replies.length,
        ...(view.older !== undefined ? { older: view.older } : {}),
      },
    },
  };
  return next;
}

export function mergeChannels(cache: BoardCache, channels: BoardChannelView[]): BoardCache {
  const next = copyRecord(cache.channels);
  for (const channel of channels) next[channel.path] = channel;
  return { ...cache, channels: next };
}

export function mergeActors(cache: BoardCache, actors: Array<BoardActorView & { id: string }>): BoardCache {
  const next = copyRecord(cache.actors);
  for (const actor of actors) next[actor.id] = actor;
  return { ...cache, actors: next };
}

/** Merge only a task view into its root message (a write's response carries
 * the root message; a test may hold the task alone). */
export function mergeTask(cache: BoardCache, rootId: string, task: BoardTaskView): BoardCache {
  const next: BoardCache = {
    ...cache,
    tasks: { ...cache.tasks, [rootId]: task },
    claims: { ...cache.claims },
    needs: { ...cache.needs, [rootId]: [...task.needs] },
    messages: { ...cache.messages },
  };
  if (task.claim) next.claims[rootId] = { ...task.claim };
  else delete next.claims[rootId];
  const message = next.messages[rootId];
  if (message) next.messages[rootId] = { ...message, task };
  return next;
}

/** Merge one SSE event. The event's cursor gates de-duplication; a hidden
 * frame is handled by the caller (it carries no view). */
export function mergeEvent(cache: BoardCache, event: BoardEventView): BoardCache {
  if (!event.message) return cache;
  // A reply can arrive before its root (the thread was never opened): cache
  // the message, but do not invent a thread.
  return mergeMessage(cache, event.message);
}
