// The development transport's board surface — keel-board-v1, Phase 2.
//
// A scripted tenant with tasks in every state, an archived thread, a
// downloaded-claim recovery case, sync failures and automation. It is only
// used when a launch asks for the fixture (`?fixture=1` /
// `host.params.fixture`); the product never selects it.
//
// The fixture is the *contract stand-in* for the Phase 2 additions a board
// version may not emit yet: `moves`, `origin`, `archived` and the sync health
// facts. A real deployment without them is read-only (taskMoves returns []);
// this file is never evidence that the server provides them.
import type {
  BoardActorsPage,
  BoardApi,
  BoardChannelsPage,
  BoardCreateTaskInput,
  BoardMeView,
  BoardMessageView,
  BoardReplyInput,
  BoardScheduleView,
  BoardStreamOptions,
  BoardSyncCredentialView,
  BoardSyncEntryView,
  BoardSyncFailure,
  BoardSyncInboxItem,
  BoardSyncStatusView,
  BoardTaskClaim,
  BoardTaskMutation,
  BoardTaskQuery,
  BoardTasksPage,
  BoardTaskView,
  BoardThreadSummary,
  BoardThreadView,
  BoardWatcherDelivery,
  BoardWatcherDeliveryDepth,
  BoardWatcherView,
} from "./board-wire";
import { fixtureAccountId, fixtureCurrentAccountId, fixtureTenantId } from "./fixture-identity";

const iso = (offsetSeconds: number): string => new Date(Date.now() + offsetSeconds * 1000).toISOString();

const ACTORS: Array<{ id: string; kind?: string; handle?: string; name?: string }> = [
  { id: fixtureCurrentAccountId, kind: "human", handle: "lina", name: "李娜" },
  { id: fixtureAccountId("chenmo"), kind: "human", handle: "chenmo", name: "陈墨" },
  { id: fixtureAccountId("linlan"), kind: "human", handle: "linlan", name: "林岚" },
  { id: fixtureAccountId("zhoujin"), kind: "human", handle: "zhoujin", name: "周谨" },
  { id: fixtureAccountId("butler"), kind: "bot", handle: "butler", name: "发布员·小铖" },
  { id: fixtureAccountId("release-bot"), kind: "bot", handle: "release-bot", name: "接单器" },
];

const actorName = (id: string | null | undefined): string => {
  if (!id) return "池中";
  return ACTORS.find((actor) => actor.id === id)?.name ?? id;
};

const ACTOR_BY_HANDLE = (handle: string): string | undefined =>
  ACTORS.find((actor) => actor.handle === handle)?.id;

const MOVES_BY_STATE: Record<string, string[]> = {
  waiting: ["assign", "needs", "cancel", "block", "reply"],
  ready: ["claim", "block", "assign", "needs", "cancel", "reply"],
  claimed: ["renew", "release", "release.force", "takeover", "complete", "fail", "block", "reply"],
  blocked: ["unblock", "assign", "needs", "cancel", "reply"],
  completed: ["reply"],
  failed: ["reopen", "cancel", "reply"],
  canceled: ["reply"],
};

interface FixtureTask {
  id: string;
  channel: string;
  text: string;
  state: string;
  owner: string;
  by: string;
  forId: string | null;
  needs: string[];
  labels: Record<string, string>;
  claim?: { actor: string; until: string };
  blocked?: { kind?: string; reason: string };
  result?: string;
  reason?: string | null;
  replies: BoardMessageView[];
  archived?: boolean;
  origin?: BoardTaskView["origin"];
  copies?: BoardTaskView["copies"];
  createdAt: string;
}

const ROOT_LABELS: Record<string, Record<string, string>> = {
  "798": { priority: "high", team: "payments" },
  "809": { priority: "normal", team: "payments" },
  "812": { priority: "high", team: "payments" },
  "814": { priority: "normal", team: "growth" },
  "816": { priority: "low", team: "release" },
  "818": { priority: "normal", team: "release" },
  "820": { priority: "high", team: "release" },
  "822": { priority: "low", team: "release" },
  "824": { priority: "normal", team: "payments" },
  "826": { priority: "normal", team: "archive" },
};

const reply = (id: string, root: string, channel: string, actor: string, text: string, offset: number): BoardMessageView => ({
  id,
  channel,
  text,
  re: root,
  thread: root,
  labels: {},
  reactions: {},
  createdAt: iso(offset),
  by: { id: actor, name: actorName(actor), handle: ACTORS.find((row) => row.id === actor)?.handle },
  author: { id: actor, name: actorName(actor), handle: ACTORS.find((row) => row.id === actor)?.handle },
  version: 1,
  revision: 1,
});

export class FixtureBoard implements BoardApi {
  private readonly now = Date.now();
  private sequence = 900;
  private readonly tasks: FixtureTask[] = [
    {
      id: "798",
      channel: "work/release",
      text: "上线新的渠道对账任务\n把渠道对账从手工表迁到定时任务；失败时回滚，不要留下半套数据。",
      state: "waiting",
      owner: fixtureCurrentAccountId,
      by: fixtureCurrentAccountId,
      forId: null,
      needs: ["806", "807"],
      labels: ROOT_LABELS["798"],
      replies: [reply("7981", "798", "work/release", fixtureAccountId("linlan"), "对账口径已和财务对齐，等前置任务。", -4800)],
      createdAt: iso(-5 * 3600),
    },
    {
      id: "806",
      channel: "work/release",
      text: "补齐渠道对账的字段映射\n前置任务：字段映射表与空值策略。",
      state: "completed",
      owner: fixtureCurrentAccountId,
      by: fixtureCurrentAccountId,
      forId: null,
      needs: [],
      labels: { priority: "high", team: "release" },
      result: "字段映射已合并，空值按 0 处理。",
      replies: [],
      createdAt: iso(-8 * 3600),
    },
    {
      id: "807",
      channel: "work/release",
      text: "核对渠道 B 的结算周期\n前置任务：结算周期以渠道合同为准。",
      state: "failed",
      owner: fixtureCurrentAccountId,
      by: fixtureCurrentAccountId,
      forId: null,
      needs: [],
      labels: { priority: "high", team: "release" },
      reason: "合同附件缺失，无法确认结算周期。",
      replies: [],
      createdAt: iso(-7 * 3600),
    },
    {
      id: "809",
      channel: "work",
      text: "补齐退款审计日志\n把退款审批、执行和追偿写进同一份审计；需要可检索。",
      state: "ready",
      owner: fixtureAccountId("linlan"),
      by: fixtureAccountId("linlan"),
      forId: null,
      needs: [],
      labels: ROOT_LABELS["809"],
      replies: [reply("8091", "809", "work", fixtureAccountId("linlan"), "审计字段清单在文档末尾，直接照着补。", -3000)],
      createdAt: iso(-3 * 3600),
    },
    {
      id: "812",
      channel: "work/production",
      text: "修复结算页偶发重复扣款\n弱网下重复提交会生成两笔退款；先复现，再修，再压测。",
      state: "claimed",
      owner: fixtureCurrentAccountId,
      by: fixtureCurrentAccountId,
      forId: null,
      needs: [],
      labels: ROOT_LABELS["812"],
      claim: { actor: fixtureCurrentAccountId, until: iso(14 * 60) },
      replies: [
        reply("8121", "812", "work/production", fixtureCurrentAccountId, "已复现：重复点击会把 submit token 带两次。", -2400),
        reply("8122", "812", "work/production", fixtureAccountId("butler"), "已加防抖和幂等键，正在压测。", -1200),
      ],
      copies: [{ target: "slack:#work-production", platform: "slack", root: "812", status: "failed", attempts: 5, lastError: "provider answered 429" }],
      createdAt: iso(-90 * 60),
    },
    {
      id: "814",
      channel: "work/production",
      text: "为移动端接入任务通知\n任务进入待回复或完成时推送给移动端；先走现有通知通道。",
      state: "blocked",
      owner: fixtureCurrentAccountId,
      by: fixtureCurrentAccountId,
      forId: null,
      needs: [],
      labels: ROOT_LABELS["814"],
      blocked: { kind: "needs_input", reason: "移动端通知文案由谁定稿？是产品还是客服？" },
      replies: [reply("8141", "814", "work/production", fixtureAccountId("linlan"), "文案先由产品出草稿，客服只做术语校对。", -1800)],
      origin: { platform: "slack", ref: "C08:172.018" },
      copies: [{ target: "slack:#work-production", platform: "slack", root: "814", status: "synced" }],
      createdAt: iso(-140 * 60),
    },
    {
      id: "816",
      channel: "work/release",
      text: "归档 Q3 事故复盘\n把三次事故的时间线和修复项归档；不需要新结论。",
      state: "ready",
      owner: fixtureAccountId("zhoujin"),
      by: fixtureAccountId("zhoujin"),
      forId: null,
      needs: [],
      labels: ROOT_LABELS["816"],
      replies: [],
      createdAt: iso(-70 * 60),
    },
    {
      id: "818",
      channel: "work/release",
      text: "发布前依赖版本对表\n确认发布分支与 lockfile 一致。",
      state: "completed",
      owner: fixtureCurrentAccountId,
      by: fixtureCurrentAccountId,
      forId: null,
      needs: [],
      labels: ROOT_LABELS["818"],
      result: "已核对，发布分支与 lockfile 一致。",
      replies: [reply("8181", "818", "work/release", fixtureCurrentAccountId, "版本对表结果贴在发布单。", -600)],
      createdAt: iso(-26 * 3600),
    },
    {
      id: "820",
      channel: "work/release",
      text: "生成发布说明草稿\n按合并记录生成发布说明；失败时保留日志。",
      state: "failed",
      owner: fixtureCurrentAccountId,
      by: fixtureCurrentAccountId,
      forId: null,
      needs: [],
      labels: ROOT_LABELS["820"],
      reason: "上游合并记录接口超时，草稿没有生成。",
      replies: [],
      createdAt: iso(-30 * 3600),
    },
    {
      id: "822",
      channel: "work/release",
      text: "补齐灰度回滚脚本\n当前只支持整包回滚，不满足灰度发布。",
      state: "canceled",
      owner: fixtureCurrentAccountId,
      by: fixtureCurrentAccountId,
      forId: null,
      needs: [],
      labels: ROOT_LABELS["822"],
      reason: "本季度不发布灰度能力。",
      replies: [],
      createdAt: iso(-48 * 3600),
    },
    {
      id: "824",
      channel: "work/production",
      text: "核对渠道 B 的重复退款流水\n这笔重复扣款需要按渠道规则追偿。",
      state: "ready",
      owner: fixtureCurrentAccountId,
      by: fixtureCurrentAccountId,
      forId: ACTOR_BY_HANDLE("butler") ?? null,
      needs: [],
      labels: ROOT_LABELS["824"],
      replies: [],
      createdAt: iso(-40 * 60),
    },
    {
      id: "826",
      channel: "ops",
      text: "历史工单：渠道 C 对账差异（已归档）\n旧超级助理工单归档保留，只读。",
      state: "completed",
      owner: fixtureCurrentAccountId,
      by: fixtureCurrentAccountId,
      forId: null,
      needs: [],
      labels: ROOT_LABELS["826"],
      result: "归档；不再恢复旧看板。",
      replies: [reply("8261", "826", "ops", fixtureCurrentAccountId, "归档记录只读保留。", -200000)],
      archived: true,
      origin: { platform: "superagent", ref: "ticket-77" },
      createdAt: iso(-300000),
    },
  ];
  private readonly messages = new Map<string, BoardMessageView>();
  private readonly createByKey = new Map<string, { message: BoardMessageView; claim?: BoardTaskClaim }>();
  private readonly replyByKey = new Map<string, BoardMessageView>();
  private readonly syncEntries: BoardSyncEntryView[] = [
    {
      key: "slack",
      platform: "slack",
      enabled: true,
      owned: true,
      createdAt: iso(-90 * 86400),
      updatedAt: iso(-3600),
      inbox: { pending: 2, leased: 0, done: 128, dead: 1 },
      outbox: { pending: 1, leased: 0, done: 120, dead: 1 },
      targets: [
        { target: "slack:C08:174.002", root: "814", status: "pending", pending: 1, undelivered: 1, attempts: 2 },
        { target: "slack:C08:172.018", root: "812", status: "failed", dead: 1, attempts: 5, lastError: "provider answered 429" },
        { target: "slack:C09:site", root: "820", status: "synced", undelivered: 0 },
      ],
      lag: {
        inbox: { outstanding: 2, oldestAt: iso(-640), ageSeconds: 640 },
        outbox: { outstanding: 1, oldestAt: iso(-120), ageSeconds: 120 },
      },
      failures24h: 1,
    },
    {
      key: "github",
      platform: "github",
      enabled: true,
      owned: true,
      createdAt: iso(-60 * 86400),
      updatedAt: iso(-7200),
      inbox: { pending: 0, leased: 0, done: 42, dead: 0 },
      outbox: { pending: 0, leased: 0, done: 42, dead: 0 },
      targets: [{ target: "github:acme/www:pulls:123", root: "816", status: "synced", undelivered: 0 }],
      lag: { inbox: null, outbox: null },
      failures24h: 0,
    },
    {
      key: "linear",
      platform: "linear",
      enabled: false,
      owned: false,
      createdAt: iso(-20 * 86400),
      updatedAt: iso(-86400),
      inbox: { pending: 0, leased: 0, done: 0, dead: 0 },
      outbox: { pending: 0, leased: 0, done: 0, dead: 0 },
      targets: [],
      lag: { inbox: null, outbox: null },
      failures24h: 0,
    },
  ];
  /** The adapter's credential status read; never part of the entry DTO. */
  private syncCredentials: BoardSyncCredentialView[] = [
    { key: "slack", platform: "slack", enabled: true, credential: "valid" },
    { key: "github", platform: "github", enabled: true, credential: "invalid" },
    { key: "linear", platform: "linear", enabled: false, credential: "missing" },
  ];
  private dead: BoardSyncInboxItem[] = [
    {
      id: "inbox-1",
      key: "slack",
      thread: "slack:C08:174.002",
      attempts: 5,
      dedupe: "slack:evt_9a1c",
      receivedAt: iso(-640),
      status: "dead",
      lastError: "provider answered 429",
    },
    {
      id: "inbox-2",
      key: "slack",
      thread: "slack:C08:172.018",
      attempts: 5,
      dedupe: "slack:evt_9a1d",
      receivedAt: iso(-580),
      status: "dead",
      lastError: "signature window expired",
    },
  ];
  private failures: BoardSyncFailure[] = [
    {
      delivery: "delivery-77",
      message: "814",
      target: "slack:C08:174.002",
      attempts: 5,
      error: "rate limited",
      key: "slack",
      at: iso(-700),
    },
  ];
  private readonly watchers: BoardWatcherView[] = [
    {
      id: "triage-inbox",
      channel: "work/",
      filter: { types: ["message.created"], labels: { state: "ready" } },
      action: "assign-by-label",
      bot: ACTOR_BY_HANDLE("butler"),
      enabled: true,
      cursor: "812",
      dead: 1,
      lastError: "rate limited",
      lastDeliveryAt: iso(-300),
      createdAt: iso(-40 * 86400),
      updatedAt: iso(-300),
    },
    {
      id: "escalate-blocked",
      channel: "work/production",
      filter: { types: ["task.blocked"] },
      action: "notify-owner",
      bot: ACTOR_BY_HANDLE("release-bot"),
      enabled: true,
      cursor: "814",
      dead: 0,
      lastDeliveryAt: iso(-900),
      createdAt: iso(-30 * 86400),
      updatedAt: iso(-900),
    },
  ];
  private readonly schedules: BoardScheduleView[] = [
    {
      id: "morning-queue",
      channel: "work",
      labels: {},
      creator: fixtureCurrentAccountId,
      nextRunAt: iso(18 * 3600),
      everySeconds: 86400,
      sent: 14,
      status: "pending",
      attempts: 0,
      createdAt: iso(-14 * 86400),
      updatedAt: iso(-3600),
    },
    {
      id: "weekly-reconcile",
      channel: "work/release",
      labels: {},
      creator: fixtureAccountId("zhoujin"),
      nextRunAt: iso(2 * 86400),
      everySeconds: 604800,
      sent: 3,
      status: "disabled",
      attempts: 0,
      createdAt: iso(-21 * 86400),
      updatedAt: iso(-7200),
    },
    {
      id: "legacy-reminder",
      channel: "work/release",
      labels: {},
      creator: fixtureAccountId("linlan"),
      nextRunAt: iso(-3600),
      everySeconds: 86400,
      sent: 9,
      status: "canceled",
      cancelReason: "revoked",
      attempts: 3,
      createdAt: iso(-30 * 86400),
      updatedAt: iso(-3600),
    },
    {
      id: "stuck-reminder",
      channel: "work",
      labels: {},
      creator: fixtureAccountId("linlan"),
      nextRunAt: iso(-7200),
      everySeconds: 86400,
      sent: 5,
      status: "dead",
      attempts: 5,
      lastError: "provider timeout",
      createdAt: iso(-20 * 86400),
      updatedAt: iso(-7200),
    },
  ];

  constructor() {
    for (const task of this.tasks) {
      const message: BoardMessageView = {
        id: task.id,
        channel: task.channel,
        text: task.text,
        labels: { ...task.labels, "board.task": task.state },
        reactions: {},
        createdAt: task.createdAt,
        by: { id: task.by, name: actorName(task.by), handle: ACTORS.find((row) => row.id === task.by)?.handle },
        author: { id: task.by, name: actorName(task.by), handle: ACTORS.find((row) => row.id === task.by)?.handle },
        version: 5,
        revision: 2,
      };
      this.messages.set(task.id, message);
      for (const item of task.replies) this.messages.set(item.id, item);
    }
  }

  private tenant(): string {
    return fixtureTenantId;
  }

  private taskView(task: FixtureTask): BoardTaskView {
    const view: BoardTaskView = {
      state: task.state,
      owner: task.owner,
      by: task.by,
      for: task.forId,
      needs: [...task.needs],
      epoch: task.claim ? 2 : 1,
      claim: task.claim ? { actor: task.claim.actor, until: task.claim.until } : null,
      blocked: task.blocked ?? null,
      result: task.result ?? null,
      reason: task.reason ?? null,
      moves: task.archived ? [] : [...(MOVES_BY_STATE[task.state] ?? ["reply"])],
      ...(task.archived ? { archived: true } : {}),
      ...(task.origin ? { origin: { ...task.origin } } : {}),
      ...(task.copies ? { copies: task.copies.map((copy) => ({ ...copy })) } : {}),
    };
    if (task.state === "claimed" && !task.archived) {
      const expired = Date.parse(task.claim?.until ?? "") <= Date.now();
      view.moves = expired
        ? ["claim", "release.force", "takeover", "fail", "block", "reply"]
        : ["renew", "release", "release.force", "takeover", "complete", "fail", "block", "reply"];
    }
    if (task.state === "ready" && task.forId && task.forId !== fixtureCurrentAccountId) {
      view.moves = (view.moves ?? []).filter((move) => move !== "claim");
    }
    return view;
  }

  private summary(task: FixtureTask): BoardThreadSummary {
    const message = this.messages.get(task.id);
    if (message) message.task = this.taskView(task);
    const last = task.replies.length ? task.replies[task.replies.length - 1] : null;
    return {
      message: message ?? {
        id: task.id,
        channel: task.channel,
        text: task.text,
        createdAt: task.createdAt,
        labels: {},
        task: this.taskView(task),
      },
      replies: task.replies.length,
      ...(last ? { last } : {}),
    };
  }

  private find(id: string): FixtureTask | undefined {
    return this.tasks.find((task) => task.id === id);
  }

  private appendReply(task: FixtureTask, text: string): BoardMessageView {
    const id = String(++this.sequence);
    const message = reply(id, task.id, task.channel, fixtureCurrentAccountId, text, 0);
    task.replies.push(message);
    this.messages.set(id, message);
    return message;
  }

  private mutation(task: FixtureTask, claim?: BoardTaskClaim, until?: string): BoardTaskMutation {
    const summary = this.summary(task);
    return {
      message: summary.message,
      ...(claim ? { claim } : {}),
      ...(until ? { until } : {}),
    };
  }

  // ---- reads --------------------------------------------------------------

  async boardMe(): Promise<BoardMeView> {
    return {
      account: { id: fixtureCurrentAccountId, username: "lina", displayName: "李娜" },
      actor: fixtureCurrentAccountId,
      actorName: "李娜",
      can: { board: { read: true, write: true, manage: true } },
    };
  }

  async boardChannels(after?: string, limit = 100): Promise<BoardChannelsPage> {
    const paths = ["work", "work/production", "work/release", "ops"];
    const filtered = after ? paths.filter((path) => path > after) : paths;
    return {
      channels: filtered.slice(0, limit).map((path) => ({
        path,
        lastIndex: 900,
        messages: 12,
        threads: 4,
        can: { read: true, write: true, manage: true },
      })),
      cursor: "900",
    };
  }

  async boardActors(limit = 200): Promise<BoardActorsPage> {
    return { actors: ACTORS.slice(0, limit).map((actor) => ({ ...actor, id: actor.id })), cursor: "900" };
  }

  async boardTasks(query: BoardTaskQuery): Promise<BoardTasksPage> {
    const archivedLabel = (query.labels ?? []).find((label) => label.key === "board.archived");
    const wantArchived = archivedLabel?.value === "true";
    let rows = [...this.tasks];
    rows = wantArchived ? rows.filter((task) => task.archived) : rows.filter((task) => !task.archived);
    if (query.channel) {
      const channel = query.channel;
      rows = rows.filter(
        (task) => task.channel === channel || task.channel.startsWith(`${channel.replace(/\/$/, "")}/`),
      );
    }
    if (query.state && query.state !== "all") {
      if (query.state === "open") {
        rows = rows.filter((task) => ["waiting", "ready", "claimed", "blocked"].includes(task.state));
      } else if (query.state === "claimed") {
        rows = rows.filter((task) => task.state === "claimed");
      } else {
        rows = rows.filter((task) => task.state === query.state);
      }
    }
    for (const label of query.labels ?? []) {
      if (label.key === "board.archived") continue;
      rows = rows.filter((task) => task.labels[label.key] === label.value);
    }
    if (query.owner) rows = rows.filter((task) => task.owner === query.owner || task.by === query.owner);
    if (query.for && query.for !== "pool") rows = rows.filter((task) => task.forId === query.for);
    if (query.after) rows = rows.filter((task) => Number(task.id) > Number(query.after));
    rows.sort((left, right) => Number(left.id) - Number(right.id));
    const limit = query.limit ?? 50;
    const page = rows.slice(0, limit);
    return {
      tasks: page.map((task) => this.summary(task)),
      cursor: page.length ? page[page.length - 1].id : "0",
    };
  }

  async boardThread(id: string, after?: string, limit = 50): Promise<BoardThreadView> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    let replies = [...task.replies];
    if (after) replies = replies.filter((item) => Number(item.id) > Number(after));
    replies = replies.slice(-limit);
    return {
      message: this.summary(task).message,
      replies,
      cursor: String(this.sequence),
      replyCount: task.replies.length,
    };
  }

  // ---- writes -------------------------------------------------------------

  async boardCreateTask(input: BoardCreateTaskInput): Promise<{ message: BoardMessageView; claim?: BoardTaskClaim }> {
    if (input.idempotencyKey && this.createByKey.has(input.idempotencyKey)) {
      return this.createByKey.get(input.idempotencyKey)!;
    }
    const id = String(++this.sequence);
    const task: FixtureTask = {
      id,
      channel: input.channel,
      text: input.text,
      state: "ready",
      owner: fixtureCurrentAccountId,
      by: fixtureCurrentAccountId,
      forId: input.for ?? null,
      needs: input.needs ?? [],
      labels: input.labels ?? {},
      replies: [],
      createdAt: new Date().toISOString(),
    };
    if (input.claim) {
      task.state = "claimed";
      task.claim = { actor: fixtureCurrentAccountId, until: iso(input.lease ?? 900) };
    }
    this.tasks.push(task);
    const message: BoardMessageView = {
      id,
      channel: task.channel,
      text: task.text,
      labels: { ...task.labels, "board.task": task.state },
      reactions: {},
      createdAt: task.createdAt,
      by: { id: fixtureCurrentAccountId, name: actorName(fixtureCurrentAccountId), handle: "lina" },
      author: { id: fixtureCurrentAccountId, name: actorName(fixtureCurrentAccountId), handle: "lina" },
      version: 1,
      revision: 1,
      task: this.taskView(task),
    };
    this.messages.set(id, message);
    const result = {
      message,
      ...(input.claim ? { claim: { epoch: 1, token: input.token ?? "fixture-token", until: task.claim!.until } } : {}),
    };
    if (input.idempotencyKey) this.createByKey.set(input.idempotencyKey, result);
    return result;
  }

  async boardReply(input: BoardReplyInput): Promise<{ message: BoardMessageView }> {
    if (input.idempotencyKey && this.replyByKey.has(input.idempotencyKey)) {
      return { message: this.replyByKey.get(input.idempotencyKey)! };
    }
    const task = this.find(input.re);
    if (!task) throw new Error(`not found: ${input.re}`);
    const message = this.appendReply(task, input.text);
    if (input.idempotencyKey) this.replyByKey.set(input.idempotencyKey, message);
    return { message };
  }

  async boardClaim(id: string, input: { lease?: number; token?: string }): Promise<BoardTaskMutation> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    const until = iso(input.lease ?? 900);
    task.state = "claimed";
    task.claim = { actor: fixtureCurrentAccountId, until };
    return this.mutation(task, { epoch: 2, token: input.token ?? "fixture-token", until });
  }

  async boardClaimReady(input: {
    channel: string;
    label?: Record<string, string>;
    from?: string[];
    lease?: number;
    wait?: number;
    token?: string;
  }): Promise<{ task: BoardThreadSummary | null; claim?: BoardTaskClaim }> {
    const task = this.tasks.find((row) => row.state === "ready" && !row.forId);
    if (!task) return { task: null };
    const until = iso(input.lease ?? 900);
    task.state = "claimed";
    task.claim = { actor: fixtureCurrentAccountId, until };
    return { task: this.summary(task), claim: { epoch: 2, token: input.token ?? "fixture-token", until } };
  }

  async boardRenew(id: string, input: { epoch: number; token: string; lease?: number }): Promise<BoardTaskMutation> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    const until = iso(input.lease ?? 900);
    if (task.claim) task.claim.until = until;
    return this.mutation(task, undefined, until);
  }

  async boardRelease(id: string, input: { epoch?: number; token?: string; force?: boolean }): Promise<BoardTaskMutation> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    task.state = "ready";
    task.claim = undefined;
    void input;
    return this.mutation(task);
  }

  async boardTakeover(id: string): Promise<BoardTaskMutation> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    const until = iso(900);
    task.state = "claimed";
    task.claim = { actor: fixtureCurrentAccountId, until };
    return this.mutation(task, { epoch: 3, token: "fixture-takeover-token", until });
  }

  async boardComplete(id: string, input: { epoch: number; token: string; result?: string }): Promise<BoardTaskMutation> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    task.state = "completed";
    task.claim = undefined;
    task.result = input.result ?? undefined;
    return this.mutation(task);
  }

  async boardFail(id: string, input: { epoch: number; token: string; reason: string }): Promise<BoardTaskMutation> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    task.state = "failed";
    task.claim = undefined;
    task.reason = input.reason;
    return this.mutation(task);
  }

  async boardBlock(
    id: string,
    input: { reason: string; kind?: string; epoch?: number; token?: string },
  ): Promise<BoardTaskMutation> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    task.state = "blocked";
    task.claim = undefined;
    task.blocked = { reason: input.reason, ...(input.kind ? { kind: input.kind } : {}) };
    return this.mutation(task);
  }

  async boardUnblock(id: string, input: { reply?: string }): Promise<BoardTaskMutation> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    task.state = task.needs.length ? "waiting" : "ready";
    task.blocked = undefined;
    if (input.reply?.trim()) this.appendReply(task, input.reply.trim());
    return this.mutation(task);
  }

  async boardReopen(id: string): Promise<BoardTaskMutation> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    task.state = "ready";
    task.reason = null;
    return this.mutation(task);
  }

  async boardAssign(id: string, input: { for: string | null }): Promise<BoardTaskMutation> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    task.forId = input.for;
    return this.mutation(task);
  }

  async boardNeeds(id: string, needs: string[]): Promise<BoardTaskMutation> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    task.needs = [...needs];
    if (task.state === "waiting" || task.state === "ready") task.state = needs.length ? "waiting" : "ready";
    return this.mutation(task);
  }

  async boardCancel(id: string, input: { reason: string }): Promise<BoardTaskMutation> {
    const task = this.find(id);
    if (!task) throw new Error(`not found: ${id}`);
    task.state = "canceled";
    task.claim = undefined;
    task.reason = input.reason;
    return this.mutation(task);
  }

  // ---- sync ---------------------------------------------------------------

  async boardSyncEntries(): Promise<{ entries: BoardSyncEntryView[] }> {
    return { entries: this.syncEntries.map((entry) => ({ ...entry })) };
  }

  async boardSyncCredentials(): Promise<{ entries: BoardSyncCredentialView[] }> {
    return { entries: this.syncCredentials.map((entry) => ({ ...entry })) };
  }

  async boardSyncStatus(): Promise<BoardSyncStatusView> {
    return {
      keys: this.syncEntries.map((entry) => ({ ...entry })),
      outbox: { pending: 1, leased: 1, done: 30, dead: this.failures.length },
      lag: {
        inbox: { outstanding: 2, oldestAt: iso(-640), ageSeconds: 640 },
        outbox: { outstanding: 1, oldestAt: iso(-120), ageSeconds: 120 },
      },
      failures24h: 1,
      updatedAt: new Date().toISOString(),
      syncEnabled: true,
    };
  }

  async boardSyncInbox(key?: string): Promise<{ dead: BoardSyncInboxItem[] }> {
    return { dead: this.dead.filter((item) => !key || item.key === key).map((item) => ({ ...item })) };
  }

  async boardSyncRetryInbox(id: string, _key: string): Promise<{ retried: boolean }> {
    const found = this.dead.some((item) => item.id === id);
    this.dead = this.dead.filter((item) => item.id !== id);
    return { retried: found };
  }

  async boardSyncFailures(key?: string): Promise<{ failures: BoardSyncFailure[] }> {
    return {
      failures: this.failures.filter((item) => !key || item.key === key).map((item) => ({ ...item })),
    };
  }

  async boardSyncRetryOutbox(id: string): Promise<{ retried: boolean }> {
    const found = this.failures.some((item) => item.delivery === id);
    this.failures = this.failures.filter((item) => item.delivery !== id);
    return { retried: found };
  }

  // ---- automation ---------------------------------------------------------

  async boardWatchers(): Promise<{ watchers: BoardWatcherView[] }> {
    return { watchers: this.watchers.map((watcher) => ({ ...watcher })) };
  }

  async boardWatcherEnable(id: string, enabled: boolean): Promise<BoardWatcherView> {
    const watcher = this.watchers.find((row) => row.id === id);
    if (!watcher) throw new Error(`not found: ${id}`);
    watcher.enabled = enabled;
    return { ...watcher };
  }

  async boardWatcherDeliveries(id: string): Promise<{ deliveries: BoardWatcherDelivery[]; depth?: BoardWatcherDeliveryDepth }> {
    const watcher = this.watchers.find((row) => row.id === id);
    if (!watcher) return { deliveries: [], depth: { pending: 0, leased: 0, done: 0, dead: 0 } };
    return {
      depth: { pending: watcher.enabled ? 0 : 1, leased: 0, done: 40, dead: watcher.dead ?? 0 },
      deliveries: [
        {
          watcherId: id,
          event: "812",
          state: watcher.enabled ? "done" : "pending",
          attempts: 1,
          nextAt: watcher.lastDeliveryAt ?? iso(-60),
          ...(watcher.lastError ? { lastError: watcher.lastError } : {}),
          createdAt: watcher.lastDeliveryAt ?? iso(-60),
          updatedAt: watcher.updatedAt,
        },
      ],
    };
  }

  async boardWatcherRetryDelivery(id: string, _seq: string): Promise<{ retried: boolean }> {
    const watcher = this.watchers.find((row) => row.id === id);
    if (watcher) {
      watcher.dead = 0;
      watcher.lastError = undefined;
    }
    return { retried: watcher !== undefined };
  }

  async boardSchedules(): Promise<{ schedules: BoardScheduleView[] }> {
    return { schedules: this.schedules.map((schedule) => ({ ...schedule })) };
  }

  async boardScheduleEnable(id: string, enabled: boolean): Promise<BoardScheduleView> {
    const schedule = this.schedules.find((row) => row.id === id);
    if (!schedule) throw new Error(`not found: ${id}`);
    schedule.status = enabled ? "pending" : "disabled";
    return { ...schedule };
  }

  async boardScheduleCancel(
    id: string,
    _opts: { reason?: string; status?: string } = {},
  ): Promise<BoardScheduleView | null> {
    // Both DELETE (live) and `/clear` (dead) remove the plan; the fixture has
    // no row to return.
    const index = this.schedules.findIndex((row) => row.id === id);
    if (index < 0) throw new Error(`not found: ${id}`);
    this.schedules.splice(index, 1);
    return null;
  }

  boardStream(opts: BoardStreamOptions): () => void {
    // A scripted tenant has no feed, but it is a live-shaped stand-in: the
    // badge says 实时 and the last-visible time stays empty until an event.
    opts.onState("live", 0);
    return () => opts.onState("closed", 0);
  }

  nameOf(id: string | null | undefined): string {
    return actorName(id);
  }

  tenantId(): string {
    return this.tenant();
  }
}
