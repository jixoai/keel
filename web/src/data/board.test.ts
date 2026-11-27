// The board data layer's decisions, pinned without rendering: normalized
// merges, cursor de-duplication, the strict `moves` rule, lease arithmetic with
// an injected clock, query round-trips, the optimistic write/rollback path and
// the sync/automation reads.
import { expect, test } from "nsh/test";
import type { BoardApi, BoardCache, BoardMessageView, BoardTaskView } from "./board-wire";
import {
  boardFailureCopy,
  boardFailureOf,
  boardAvailableMoves,
  boardEventRefLabel,
  boardLabelFilterTitle,
  boardLabelView,
  boardLeaseFacts,
  boardNeedsFact,
  boardPlatformLabel,
  boardProviderErrorCopy,
  boardQueryFromSearch,
  boardRenderableMoves,
  boardSearchOf,
  boardStateLabel,
  boardStore,
  boardThreadRefLabel,
  boardWatcherActionLabel,
  defaultBoardQuery,
  effectiveTaskState,
  insertQueueId,
  matchesBoardQuery,
  queryToTaskQuery,
  queueRowOf,
  taskMoves,
  visibleQueue,
} from "./board";
import {
  emptyBoardCache,
  mergeChannels,
  mergeEvent,
  mergeMessage,
  mergeTask,
  parseStreamFrame,
  taskQueryParams,
} from "./board-wire";
import { FixtureBoard } from "./fixture-board";
import { KeelApiError, KeelHttp, type FetchLike } from "./http";
import { setEffectivePermissions } from "./permissions";
import { store } from "./store";

const NOW = Date.parse("2026-11-27T10:00:00.000Z");

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));
const waitFor = async (predicate: () => boolean, what: string, timeout = 2000): Promise<void> => {
  const start = Date.now();
  for (;;) {
    if (predicate()) return;
    if (Date.now() - start > timeout) throw new Error(`timed out waiting for ${what}`);
    await sleep(5);
  }
};

const task = (overrides: Partial<BoardTaskView> = {}): BoardTaskView => ({
  state: "ready",
  owner: "acct-chenmo",
  by: "acct-chenmo",
  needs: [],
  epoch: 1,
  claim: null,
  blocked: null,
  result: null,
  reason: null,
  moves: ["claim", "reply"],
  ...overrides,
});

const message = (id: string, overrides: Partial<BoardMessageView> = {}): BoardMessageView => ({
  id,
  channel: "work",
  text: `任务 ${id}`,
  createdAt: "2026-11-27T09:00:00.000Z",
  labels: {},
  task: task(),
  ...overrides,
});

function cacheWith(root: BoardMessageView): BoardCache {
  return mergeMessage(emptyBoardCache(), root);
}

test("board：七态词表与租约到期按 ready 呈现", () => {
  expect(boardStateLabel("waiting")).toBe("等待依赖");
  expect(boardStateLabel("paused")).toBe("paused");
  const claimed = task({
    state: "claimed",
    claim: { actor: "acct-chenmo", until: "2026-11-27T10:00:30.000Z" },
  });
  expect(effectiveTaskState(claimed, NOW)).toBe("claimed");
  expect(effectiveTaskState(claimed, NOW + 31_000)).toBe("ready");
});

test("board：租约倒计时在 60 秒内变警告、到点变危险，持有者归属不冒充", () => {
  const claimed = task({
    state: "claimed",
    claim: { actor: "acct-chenmo", until: "2026-11-27T10:01:00.000Z" },
  });
  const fresh = boardLeaseFacts(claimed, NOW, { hasToken: true, leaseSeconds: 900 });
  expect(fresh.remainingSeconds).toBe(60);
  expect(fresh.tone).toBe("normal");
  expect(fresh.holder).toBe("this-tab");
  const mine = boardLeaseFacts(claimed, NOW + 30_000, { hasToken: false, mine: true });
  expect(mine.text).toBe("剩余 30 秒");
  expect(mine.tone).toBe("warning");
  expect(mine.holder).toBe("another-tab");
  const expired = boardLeaseFacts(claimed, NOW + 61_000, { hasToken: false, mine: false });
  expect(expired.expired).toBe(true);
  expect(expired.text).toBe("租约已到期");
  expect(expired.tone).toBe("danger");
  expect(expired.holder).toBe("someone");
});

test("board：队列按服务端顺序过滤，pool/archived/标签/频道都在客户端兜底", () => {
  const root = message("812", {
    channel: "work/production",
    labels: { priority: "high" },
    task: task({ state: "ready", for: null }),
  });
  const cache = cacheWith(root);
  const row = queueRowOf(cache, "812");
  expect(row).toBeDefined();
  const open = defaultBoardQuery();
  expect(visibleQueue(cache, ["812"], open, NOW)).toHaveLength(1);
  expect(visibleQueue(cache, ["812"], { ...open, channel: "work" }, NOW)).toHaveLength(1);
  expect(visibleQueue(cache, ["812"], { ...open, channel: "ops" }, NOW)).toHaveLength(0);
  expect(visibleQueue(cache, ["812"], { ...open, labelKey: "priority", labelValue: "high" }, NOW)).toHaveLength(1);
  expect(visibleQueue(cache, ["812"], { ...open, labelKey: "priority", labelValue: "low" }, NOW)).toHaveLength(0);
  expect(visibleQueue(cache, ["812"], { ...open, for: "pool" }, NOW)).toHaveLength(1);
  expect(visibleQueue(cache, ["812"], { ...open, archived: true }, NOW)).toHaveLength(0);
  // The server's order is kept: a newer id is appended, never sorted in front.
  expect(insertQueueId(["798", "812"], "809")).toEqual(["798", "809", "812"]);
  expect(insertQueueId(["798", "812"], "900")).toEqual(["798", "812", "900"]);
});

test("board：筛选 search 往返一致，任务查询按 board 的 label=key=value 编码", () => {
  const query = {
    ...defaultBoardQuery(),
    state: "ready" as const,
    channel: "work/production",
    labelKey: "priority",
    labelValue: "high",
    owner: "acct-chenmo",
    for: "pool",
    archived: true,
  };
  const search = boardSearchOf(query);
  expect(boardQueryFromSearch(search)).toEqual(query);
  expect(boardQueryFromSearch("?state=claimed&for=all")).toEqual({ ...defaultBoardQuery(), state: "claimed" });
  const params = taskQueryParams({ state: "ready", channel: "work", labels: [{ key: "priority", value: "high" }] });
  expect(params).toContain("state=ready");
  expect(params).toContain("channel=work");
  expect(params).toContain("label=priority%3Dhigh");
  expect(taskQueryParams({ state: "all" })).toContain("state=all");
  // Archive is a server-maintained label, never a board query parameter.
  const archive = queryToTaskQuery({ ...defaultBoardQuery(), archived: true, state: "all" });
  expect(archive.labels).toEqual([{ key: "board.archived", value: "true" }]);
  expect("archived" in archive).toBe(false);
  const archiveParams = taskQueryParams(archive);
  expect(archiveParams).toContain("label=board.archived%3Dtrue");
  expect(archiveParams.some((value) => value.startsWith("archived="))).toBe(false);
  // The default open view must not request the archive label.
  expect(queryToTaskQuery(defaultBoardQuery()).labels).toBeUndefined();
});

test("board：租约过期后 token 动作即使有 token 也不可用，claim 为首选", () => {
  const claimed = task({
    state: "claimed",
    claim: { actor: "acct-chenmo", until: "2026-11-27T10:00:00.000Z" },
    moves: ["claim", "renew", "release", "release.force", "takeover", "complete", "fail", "block", "reply"],
  });
  const expired = boardAvailableMoves(claimed, { hasToken: true, mine: true, now: NOW + 1000 });
  expect(expired).toEqual(["claim", "release.force", "takeover", "reply"]);
  expect(expired).not.toContain("complete");
  expect(expired).not.toContain("renew");
  expect(expired).not.toContain("release");
  expect(expired).not.toContain("fail");
  expect(expired).not.toContain("block");
  // Before expiry the token holder keeps the settlement moves.
  const live = boardAvailableMoves(claimed, { hasToken: true, mine: true, now: NOW - 1000 });
  expect(live).toContain("complete");
  expect(live).toContain("renew");
  // A stale token from another actor never unlocks settlement moves once
  // identity is known.
  const stale = boardAvailableMoves(claimed, { hasToken: true, mine: false, identityKnown: true, now: NOW - 1000 });
  expect(stale).not.toContain("complete");
  expect(stale).not.toContain("renew");
  expect(stale).not.toContain("release");
  // If /me is unavailable, the token itself is the only fence; the server
  // still rejects it if it is stale.
  const unknown = boardAvailableMoves(claimed, { hasToken: true, mine: false, identityKnown: false, now: NOW - 1000 });
  expect(unknown).toContain("complete");
  expect(unknown).toContain("renew");
});

test("board：UI 动作去掉 takeover/force-release，丢失 token 只留可执行动作", () => {
  const claimed = task({
    state: "claimed",
    claim: { actor: "acct-chenmo", until: "2026-11-27T10:00:00.000Z" },
    moves: ["renew", "release", "release.force", "takeover", "complete", "fail", "block", "reply"],
  });
  const lost = boardRenderableMoves(claimed, { hasToken: false, mine: true, now: NOW - 1000 });
  expect(lost).toEqual(["reply"]);
  const holder = boardRenderableMoves(claimed, { hasToken: true, mine: true, now: NOW - 1000 });
  expect(holder).toContain("complete");
  expect(holder).not.toContain("takeover");
  expect(holder).not.toContain("release.force");
});

test("board：标签与平台错误只出产品文案，不出 key=value/provider 原文", () => {
  expect(boardLabelView("board.task", "ready")).toBeNull();
  expect(boardLabelView("priority", "high")?.text).toBe("高优先级");
  expect(boardLabelView("team", "payments")?.text).toBe("支付团队");
  expect(boardLabelView("foo", "bar")?.text).toBe("bar");
  expect(boardLabelFilterTitle("priority", "high")).toBe("优先级 · 高优先级");
  expect(boardLabelFilterTitle("foo", "hotel")).toBe("hotel");
  expect(boardLabelFilterTitle("foo", "")).toBe("标签");
  expect(boardLabelFilterTitle("board.archived", "true")).toBe("系统标签");
  expect(boardEventRefLabel("slack:evt_9a1c")).toBe("Slack 事件 9a1c");
  expect(boardEventRefLabel("reaction:slack:C_SUPPORT:1700009999.000100|thread")).toBe("Slack 事件 17000099");
  expect(boardThreadRefLabel("slack:#work-production")).toBe("Slack 线程 · #work-production");
  expect(boardThreadRefLabel("slack:C08:174.002")).toBe("Slack 线程");
  expect(boardWatcherActionLabel("post")).toBe("发布消息");
  expect(boardPlatformLabel("slack")).toBe("Slack");
  expect(boardProviderErrorCopy("provider answered 429")).toBe("平台限流，稍后重试");
  expect(boardProviderErrorCopy("provider timeout")).toBe("平台响应超时");
  expect(boardProviderErrorCopy("signature window expired")).toBe("平台签名已过期，需要重新投递");
});

test("board：moves 是唯一动作来源；缺失时只读，未知动作被忽略", () => {
  expect(taskMoves(task({ moves: ["claim", "reply"] }))).toEqual(["claim", "reply"]);
  expect(taskMoves(task({ moves: ["claim", "fly", "complete"] }))).toEqual(["claim", "complete"]);
  expect(taskMoves(task({ moves: undefined }))).toEqual([]);
  expect(taskMoves(undefined)).toEqual([]);
});

test("board：缓存按 id 合并，线程根更新不把自己塞进回复，回复追加一次", () => {
  let cache = cacheWith(message("812"));
  cache = mergeMessage(cache, message("812", { text: "改过的标题" }));
  expect(cache.threads["812"]).toBeUndefined();
  expect(cache.messages["812"]?.text).toBe("改过的标题");
  cache = {
    ...cache,
    threads: { "812": { message: cache.messages["812"]!, replies: [], cursor: "812", replyCount: 0 } },
  };
  cache = mergeMessage(cache, message("813", { re: "812", text: "第一条", task: null }));
  cache = mergeMessage(cache, message("813", { re: "812", text: "第一条（编辑）", task: null }));
  expect(cache.threads["812"]?.replies).toHaveLength(1);
  expect(cache.threads["812"]?.replies[0]?.text).toBe("第一条（编辑）");
});

test("board：SSE 帧有 data 是事件，无 data 是隐藏事件只推进游标", () => {
  const hidden = parseStreamFrame("812", "");
  expect(hidden?.event).toBe(null);
  const shown = parseStreamFrame(
    "813",
    JSON.stringify({
      cursor: "813",
      type: "task.claimed",
      createdAt: "2026-11-27T09:00:00.000Z",
      message: message("812", { task: task({ state: "claimed" }) }),
    }),
  );
  expect(shown?.event?.type).toBe("task.claimed");
  let cache = cacheWith(message("812"));
  cache = mergeEvent(cache, shown!.event!);
  expect(cache.tasks["812"]?.state).toBe("claimed");
  // An event without a message leaves the cache exactly as it was.
  expect(mergeEvent(cache, { cursor: "814", type: "task.renewed", createdAt: "" })).toBe(cache);
});

test("board：needs 事实不把未加载的前置冒充完成", () => {
  let cache = cacheWith(message("798", { task: task({ state: "waiting", needs: ["806", "807"] }) }));
  cache = mergeMessage(cache, message("806", { task: task({ state: "completed" }) }));
  const facts = boardNeedsFact(cache, cache.tasks["798"]!);
  expect(facts.find((fact) => fact.id === "806")?.done).toBe(true);
  expect(facts.find((fact) => fact.id === "807")?.done).toBe(false);
  expect(facts.find((fact) => fact.id === "807")?.state).toBe("unknown");
});

test("board：错误分类与文案区分 404/403/board_disabled", () => {
  expect(boardFailureOf(new KeelApiError(404, "not found", [], undefined, "not_found")).kind).toBe("not_found");
  expect(boardFailureOf(new KeelApiError(403, "no", [], undefined, "forbidden", { requiredKey: "board.write" })).kind).toBe("permission");
  expect(boardFailureOf(new KeelApiError(404, "off", [], undefined, "board_disabled")).kind).toBe("disabled");
  expect(boardFailureCopy({ kind: "disabled", message: "" }).title).toBe("任务看板未启用");
  expect(boardFailureCopy({ kind: "not_found", message: "" }).description).toContain("地址没有对应到");
});

test("board：fixture 的读、写、claim token 与同步重试走同一套契约", async () => {
  const fixture = new FixtureBoard();
  const page = await fixture.boardTasks({ state: "open" });
  expect(page.tasks.length).toBeGreaterThanOrEqual(4);
  expect(page.tasks.every((summary) => summary.message.task !== undefined)).toBe(true);
  const claimed = await fixture.boardClaim("809", { lease: 60, token: "test-token" });
  expect(claimed.claim?.token).toBe("test-token");
  expect(claimed.message?.task?.state).toBe("claimed");
  const completed = await fixture.boardComplete("809", { epoch: 2, token: "test-token", result: "已交付" });
  expect(completed.message?.task?.state).toBe("completed");
  expect(completed.message?.task?.result).toBe("已交付");
  const dead = await fixture.boardSyncInbox();
  expect(dead.dead.length).toBeGreaterThan(0);
  await fixture.boardSyncRetryInbox(dead.dead[0].id, dead.dead[0].key);
  expect((await fixture.boardSyncInbox()).dead.length).toBe(dead.dead.length - 1);
  await fixture.boardWatcherEnable("triage-inbox", false);
  const watchers = await fixture.boardWatchers();
  expect(watchers.watchers.find((watcher) => watcher.id === "triage-inbox")?.enabled).toBe(false);
});

test("board store：打开、认领、完成；刷新/换标签页丢 token 后不再冒充持有者", async () => {
  setEffectivePermissions({ "board.read": "tenant", "board.write": "tenant", "board.manage": "tenant" }, false);
  store.setBoardTransport(new FixtureBoard());
  boardStore.reset();
  try {
    await boardStore.open();
    expect(boardStore.phase()).toBe("ready");
    expect(boardStore.queueIds()).toContain("812");
    expect(boardStore.channels().map((channel) => channel.path)).toContain("work/production");
    // #812 is claimed by this account in the fixture but this tab holds no
    // token — exactly the reload case.
    const row = boardStore.cache().tasks["812"]!;
    expect(boardStore.isMyActor(row.claim?.actor)).toBe(true);
    expect(boardStore.heldByThisTab("812")).toBe(false);
    // Claim #809, then complete it with the token the write returned.
    await boardStore.claim("809");
    expect(boardStore.cache().tasks["809"]?.state).toBe("claimed");
    expect(boardStore.heldByThisTab("809")).toBe(true);
    await boardStore.complete("809", "完成");
    expect(boardStore.cache().tasks["809"]?.state).toBe("completed");
    expect(boardStore.heldByThisTab("809")).toBe(false);
    // A reply lands in the thread and only once.
    await boardStore.openThread("809");
    const before = boardStore.cache().threads["809"]?.replies.length ?? 0;
    await boardStore.reply("补一条工作日志");
    expect((boardStore.cache().threads["809"]?.replies.length ?? 0)).toBe(before + 1);
    expect(boardStore.cache().threads["809"]?.replies.at(-1)?.text).toBe("补一条工作日志");
  } finally {
    boardStore.reset();
    store.setBoardTransport(null);
  }
});

test("board store：响应丢失后同一草稿/表单重试只落一条", async () => {
  class LostResponse extends FixtureBoard {
    creates = 0;
    replies = 0;
    override async boardCreateTask(input: Parameters<FixtureBoard["boardCreateTask"]>[0]) {
      this.creates += 1;
      const value = await super.boardCreateTask(input);
      if (this.creates === 1) throw new Error("network lost after write");
      return value;
    }
    override async boardReply(input: Parameters<FixtureBoard["boardReply"]>[0]) {
      this.replies += 1;
      const value = await super.boardReply(input);
      if (this.replies === 1) throw new Error("network lost after write");
      return value;
    }
  }
  const transport = new LostResponse();
  setEffectivePermissions({ "board.read": "tenant", "board.write": "tenant", "board.manage": "tenant" }, false);
  store.setBoardTransport(transport);
  boardStore.reset();
  try {
    await boardStore.open();
    const before = await transport.boardTasks({ state: "all" });
    expect(await boardStore.createTask({ channel: "work", text: "同一个表单" })).toBeNull();
    const id = await boardStore.createTask({ channel: "work", text: "同一个表单" });
    expect(id).not.toBeNull();
    const after = await transport.boardTasks({ state: "all" });
    expect(after.tasks.length).toBe(before.tasks.length + 1);
    expect(transport.creates).toBe(2);

    await boardStore.openThread(id!);
    const repliesBefore = boardStore.cache().threads[id!]?.replies.length ?? 0;
    expect(await boardStore.reply("同一条草稿")).toBe(false);
    expect(await boardStore.reply("同一条草稿")).toBe(true);
    expect(boardStore.cache().threads[id!]?.replies.length).toBe(repliesBefore + 1);
    expect(transport.replies).toBe(2);
  } finally {
    boardStore.reset();
    store.setBoardTransport(null);
  }
});

test("board store：写失败回滚乐观状态并保留服务器错误码", async () => {
  class Failing extends FixtureBoard {
    override async boardComplete(): Promise<never> {
      throw new KeelApiError(409, "claim lost", [], undefined, "claim_lost", {}, "req-claim-1");
    }
  }
  setEffectivePermissions({ "board.read": "tenant", "board.write": "tenant", "board.manage": "tenant" }, false);
  store.setBoardTransport(new Failing());
  boardStore.reset();
  try {
    await boardStore.open();
    await boardStore.claim("809");
    expect(boardStore.cache().tasks["809"]?.state).toBe("claimed");
    const ok = await boardStore.complete("809", "不会写回");
    expect(ok).toBe(false);
    expect(boardStore.cache().tasks["809"]?.state).toBe("claimed");
    expect(boardStore.actionError()?.failure.code).toBe("claim_lost");
  } finally {
    boardStore.reset();
    store.setBoardTransport(null);
  }
});

test("board HTTP：live schedule 走 DELETE，dead 走 clear", async () => {
  const calls: Array<{ url: string; method: string }> = [];
  const fetchImpl = (async (url: unknown, init?: RequestInit) => {
    calls.push({ url: String(url), method: String(init?.method ?? "GET") });
    return {
      ok: true,
      status: 200,
      headers: { get: () => null },
      json: async () => ({}),
      text: async () => "",
    };
  }) as unknown as FetchLike;
  const http = new KeelHttp("", fetchImpl);
  expect(await http.boardScheduleCancel("live-1", { status: "pending" })).toBeNull();
  expect(calls.at(-1)?.method).toBe("DELETE");
  expect(calls.at(-1)?.url).toContain("/v1/board/schedules/live-1");
  expect(await http.boardScheduleCancel("dead-1", { status: "dead" })).toBeNull();
  expect(calls.at(-1)?.method).toBe("POST");
  expect(calls.at(-1)?.url).toContain("/v1/board/schedules/dead-1/clear");
});

test("board store：/me 缺失时身份未知但队列仍可用", async () => {
  class NoMe extends FixtureBoard {
    override async boardMe(): Promise<never> {
      throw new KeelApiError(404, "not found", [], undefined, "not_found", {}, "req-me-1");
    }
  }
  setEffectivePermissions({ "board.read": "tenant", "board.write": "tenant", "board.manage": "tenant" }, false);
  store.setBoardTransport(new NoMe());
  boardStore.reset();
  try {
    await boardStore.open();
    expect(boardStore.phase()).toBe("ready");
    expect(boardStore.me()).toBeNull();
    expect(boardStore.meLoaded()).toBe(false);
    expect(boardStore.meFailure()?.status).toBe(404);
    expect(boardStore.queueIds()).toContain("812");
  } finally {
    boardStore.reset();
    store.setBoardTransport(null);
  }
});

test("board store：新建任务携带 needs/指派/即时认领与租约", async () => {
  setEffectivePermissions({ "board.read": "tenant", "board.write": "tenant", "board.manage": "tenant" }, false);
  store.setBoardTransport(new FixtureBoard());
  boardStore.reset();
  try {
    await boardStore.open();
    const id = await boardStore.createTask({
      channel: "work",
      text: "带前置与认领的新任务",
      needs: ["806"],
      for: null,
      claim: true,
      lease: 30,
    });
    expect(id).not.toBeNull();
    const task = boardStore.cache().tasks[id!]!;
    expect(task.needs).toEqual(["806"]);
    expect(task.state).toBe("claimed");
    expect(boardStore.heldByThisTab(id!)).toBe(true);
  } finally {
    boardStore.reset();
    store.setBoardTransport(null);
  }
});

test("board store：写失败时若实时事件已推进状态，回滚不覆盖新视图", async () => {
  class Racy extends FixtureBoard {
    release: (() => void) | null = null;
    override async boardComplete(): Promise<never> {
      await new Promise<void>((resolve) => {
        this.release = resolve;
      });
      throw new KeelApiError(409, "claim lost", [], undefined, "claim_lost");
    }
  }
  const transport = new Racy();
  setEffectivePermissions({ "board.read": "tenant", "board.write": "tenant", "board.manage": "tenant" }, false);
  store.setBoardTransport(transport);
  boardStore.reset();
  try {
    await boardStore.open();
    await boardStore.claim("809");
    const pending = boardStore.complete("809", "not written");
    await waitFor(() => transport.release !== null, "write in flight");
    // A live event moves #809 to blocked while the write is still pending.
    boardStore.applyFrameForTest({
      id: "900",
      event: {
        cursor: "900",
        type: "task.blocked",
        createdAt: "2026-11-27T10:00:00.000Z",
        payload: {},
        message: {
          id: "809",
          channel: "work",
          text: "补齐退款审计日志",
          createdAt: "2026-11-27T09:00:00.000Z",
          labels: {},
          task: { state: "blocked", owner: "a", by: "a", needs: [], epoch: 1, blocked: { reason: "newer" }, moves: [] },
        },
      },
    });
    transport.release?.();
    expect(await pending).toBe(false);
    expect(boardStore.cache().tasks["809"]?.state).toBe("blocked");
  } finally {
    boardStore.reset();
    store.setBoardTransport(null);
  }
});

test("board store：schedule 取消按状态走 DELETE/clear，dead 行走 clear 并移除行", async () => {
  const calls: Array<{ id: string; status?: string }> = [];
  class Recorder extends FixtureBoard {
    override async boardScheduleCancel(id: string, opts: { reason?: string; status?: string } = {}) {
      calls.push({ id, status: opts.status });
      return await super.boardScheduleCancel(id, opts);
    }
  }
  setEffectivePermissions({ "board.read": "tenant", "board.manage": "tenant" }, false);
  store.setBoardTransport(new Recorder());
  boardStore.reset();
  try {
    await boardStore.loadAutomation();
    expect(boardStore.automation()?.schedules.some((schedule) => schedule.id === "stuck-reminder")).toBe(true);
    expect(boardStore.automation()?.schedules.find((schedule) => schedule.id === "stuck-reminder")?.status).toBe("dead");
    await boardStore.cancelSchedule("stuck-reminder");
    expect(calls.at(-1)).toEqual({ id: "stuck-reminder", status: "dead" });
    expect(boardStore.automation()?.schedules.some((schedule) => schedule.id === "stuck-reminder")).toBe(false);
    await boardStore.cancelSchedule("morning-queue");
    expect(calls.at(-1)).toEqual({ id: "morning-queue", status: "pending" });
    expect(boardStore.automation()?.schedules.some((schedule) => schedule.id === "morning-queue")).toBe(false);
  } finally {
    boardStore.reset();
    store.setBoardTransport(null);
  }
});

test("board store：离开/回到任务面只回收并重连 SSE，不重建缓存", async () => {
  class StreamCount extends FixtureBoard {
    starts = 0;
    override boardStream(opts: Parameters<FixtureBoard["boardStream"]>[0]) {
      this.starts += 1;
      opts.onState("live", 0);
      return () => opts.onState("closed", 0);
    }
  }
  const transport = new StreamCount();
  setEffectivePermissions({ "board.read": "tenant", "board.write": "tenant", "board.manage": "tenant" }, false);
  store.setBoardTransport(transport);
  boardStore.reset();
  try {
    await boardStore.open();
    expect(transport.starts).toBe(1);
    const ids = [...boardStore.queueIds()];
    boardStore.stopLive();
    expect(boardStore.liveStatus()).toBe("closed");
    boardStore.resumeLive();
    expect(transport.starts).toBe(2);
    expect(boardStore.queueIds()).toEqual(ids);
  } finally {
    boardStore.reset();
    store.setBoardTransport(null);
  }
});

test("board store：无 board.manage 时 sync/automation 直接给权限面，不发请求", async () => {
  let calls = 0;
  const fixture = new FixtureBoard();
  const original = fixture.boardSyncEntries.bind(fixture);
  fixture.boardSyncEntries = async () => {
    calls += 1;
    return await original();
  };
  setEffectivePermissions({ "board.read": "tenant" }, false);
  store.setBoardTransport(fixture);
  boardStore.reset();
  try {
    await boardStore.loadSync();
    expect(boardStore.syncPhase()).toBe("permission");
    expect(calls).toBe(0);
    await boardStore.loadAutomation();
    expect(boardStore.automationPhase()).toBe("permission");
  } finally {
    boardStore.reset();
    store.setBoardTransport(null);
    setEffectivePermissions({}, false);
  }
});

test("board store：watcher 开关失败时回滚到原值", async () => {
  class ReadOnlyAutomation extends FixtureBoard {
    override async boardWatcherEnable(): Promise<never> {
      throw new KeelApiError(409, "switched by someone else", [], undefined, "conflict");
    }
  }
  setEffectivePermissions({ "board.read": "tenant", "board.manage": "tenant" }, false);
  store.setBoardTransport(new ReadOnlyAutomation());
  boardStore.reset();
  try {
    await boardStore.loadAutomation();
    const start = boardStore.automation()?.watchers.find((watcher) => watcher.id === "triage-inbox")?.enabled;
    expect(start).toBe(true);
    const ok = await boardStore.setWatcherEnabled("triage-inbox", false);
    expect(ok).toBe(false);
    expect(boardStore.automation()?.watchers.find((watcher) => watcher.id === "triage-inbox")?.enabled).toBe(true);
  } finally {
    boardStore.reset();
    store.setBoardTransport(null);
  }
});
