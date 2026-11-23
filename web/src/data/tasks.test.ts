// The task panel's fold, pinned without rendering anything.
//
// `taskTree`, `eventLines`, `panelGaps` and `panelPhase` are the panel's
// decision points: which card is drawn at which depth, which event line says
// what, which bound was hit, and whether a read is an empty state or a failure.
// The cases below are the ones a live read can produce (a fork member, an
// external parent, a truncated page, a foreign kind, a corrupt payload).
import { expect, test } from "nsh/test";
import type { TaskPanelView } from "./contract";
import {
  cardOriginLabel,
  cardStatusLabel,
  eventDetail,
  eventKindLabel,
  eventLines,
  forkBranches,
  panelCounts,
  panelGaps,
  panelPhase,
  previewSummary,
  resourceChanged,
  shortHash,
  shouldShowTaskPanel,
  taskTree,
} from "./tasks";

const HASH_A = "sha256:" + "a".repeat(64);

const panel = (overrides: Partial<TaskPanelView> = {}): TaskPanelView => ({
  sessionId: "s-1",
  cards: [
    {
      taskId: "root",
      sessionId: "s-1",
      title: "本周退款原因分析",
      status: "in_progress",
      origin: "session",
      createdSeq: 2,
      updatedSeq: 9,
    },
  ],
  members: [],
  ancestors: [],
  attachments: [],
  events: [
    { taskId: "root", kind: "task.created", seq: 2, title: "本周退款原因分析", status: "open", origin: "session" },
  ],
  limits: { memberLimit: 200, eventLimit: 500 },
  ...overrides,
});

test("taskTree: 会话自身的卡按创建序，成员卡挂在父卡下面，外层父卡在最上面", () => {
  const view = panel({
    cards: [
      { taskId: "root", sessionId: "s-1", title: "本周退款原因分析", status: "in_progress", origin: "session", createdSeq: 2 },
      { taskId: "summary", sessionId: "s-1", title: "汇总渠道异常", status: "done", parentTaskId: "root", createdSeq: 6 },
    ],
    members: [
      { taskId: "order", sessionId: "child-a", title: "订单查询", status: "done", parentTaskId: "root", origin: "subagent", member: true, updatedAt: "2026-01-01T00:00:00Z" },
      { taskId: "fork", sessionId: "child-b", title: "退款条款对照", status: "open", parentTaskId: "order", origin: "fork", member: true },
    ],
    ancestors: [
      { taskId: "upstream", sessionId: "outer", title: "宿主的卡", status: "open", member: true },
    ],
  });
  const rows = taskTree(view);
  // The ancestor is first (where the session branched from), then the root,
  // then the root's own child, then the members in the read's order.
  expect(rows[0].card.taskId).toBe("upstream");
  expect(rows[0].depth).toBe(0);
  expect(rows[1].card.taskId).toBe("root");
  expect(rows[1].edge).toBe("会话初始卡");
  const summary = rows.find((row) => row.card.taskId === "summary");
  expect(summary?.depth).toBe(1);
  expect(summary?.member).toBe(false);
  const order = rows.find((row) => row.card.taskId === "order");
  expect(order?.depth).toBe(1);
  expect(order?.edge).toBe("子代理");
  expect(order?.member).toBe(true);
  const fork = rows.find((row) => row.card.taskId === "fork");
  // The fork hangs under the member that owns it: depth 2, edge 分叉.
  expect(fork?.depth).toBe(2);
  expect(fork?.edge).toBe("分叉");
  expect(fork?.member).toBe(true);
  // Every card the read carried appears exactly once.
  expect(rows.filter((row) => row.card.taskId === "order")).toHaveLength(1);
});

test("taskTree: 父卡不在读数里时标另一会话，不编一个标题", () => {
  const view = panel({
    cards: [{ taskId: "root", sessionId: "s-1", title: "根卡", status: "open", parentTaskId: "missing-parent", createdSeq: 1 }],
  });
  const row = taskTree(view)[0];
  expect(row.card.taskId).toBe("root");
  expect(row.externalParent).toBe(true);
  expect(row.parentId).toBe("missing-parent");
});

test("forkBranches: 只报带 originSeq 的 fork 卡，并按 originSeq 排序", () => {
  const view = panel({
    members: [
      { taskId: "b", sessionId: "s-b", title: "分支 B", status: "open", origin: "fork", originSeq: 40, member: true },
      { taskId: "a", sessionId: "s-a", title: "分支 A", status: "open", origin: "fork", originSeq: 12, member: true },
      { taskId: "c", sessionId: "s-c", title: "子代理", status: "open", origin: "subagent", member: true },
    ],
  });
  const branches = forkBranches(view);
  expect(branches.map((branch) => branch.taskId)).toEqual(["a", "b"]);
  expect(branches[0].originSeq).toBe(12);
});

test("eventLines: 账本序、卡名、以及 kind 自己的字段；不认识的 kind 标为未解析", () => {
  const view = panel({
    cards: [{ taskId: "root", sessionId: "s-1", title: "根卡", status: "open", origin: "session", createdSeq: 2 }],
    events: [
      { taskId: "root", kind: "task.note", seq: 9, text: "渠道 B 翻倍" },
      { taskId: "root", kind: "task.created", seq: 2, title: "根卡", status: "open" },
      { taskId: "root", kind: "task.attached", seq: 4, path: "reports/log.md", hash: HASH_A },
      { taskId: "elsewhere", kind: "task.future", seq: 11, unparsed: true },
    ],
  });
  const lines = eventLines(view);
  expect(lines.map((line) => line.event.seq)).toEqual([2, 4, 9, 11]);
  expect(lines[0].cardTitle).toBe("根卡");
  expect(lines[0].detail).toBe("根卡 · 待处理");
  expect(lines[1].detail).toContain("reports/log.md");
  expect(lines[1].detail).toContain("sha256:aaaaaaaaaa…");
  expect(lines[2].detail).toBe("渠道 B 翻倍");
  // A card the read did not carry keeps its id and is marked unknown.
  expect(lines[3].cardKnown).toBe(false);
  expect(lines[3].cardTitle).toBe("elsewhere");
  expect(lines[3].detail).toBe("这个版本不认识的事件");
});

test("eventDetail: 每种 kind 只说自己带的字段", () => {
  expect(eventDetail({ taskId: "t", kind: "task.updated", seq: 1, title: "新标题", status: "done" })).toBe("新标题 · 完成");
  expect(eventDetail({ taskId: "t", kind: "task.detached", seq: 1, path: "a.md", hash: HASH_A })).toContain("a.md");
  expect(eventDetail({ taskId: "t", kind: "task.closed", seq: 1, reason: "完成" })).toBe("原因：完成");
  expect(eventDetail({ taskId: "t", kind: "task.closed", seq: 1 })).toBe("");
  expect(eventDetail({ taskId: "t", kind: "task.created", seq: 1 })).toBe("");
});

test("panelGaps: 每个上限一句话，外来的 kind 只报一次", () => {
  const view = panel({
    members: [{ taskId: "m", sessionId: "s-m", title: "成员", status: "open", member: true }],
    events: [
      { taskId: "root", kind: "task.created", seq: 2, title: "根卡" },
      { taskId: "root", kind: "task.future_one", seq: 5, unparsed: true },
      { taskId: "root", kind: "task.future_two", seq: 6, unparsed: true },
    ],
    limits: { memberLimit: 2, truncated: true, truncatedMemberCount: 3, eventLimit: 10, eventsTruncated: true, ancestorsTruncated: true },
  });
  const gaps = panelGaps(view);
  expect(gaps).toHaveLength(4);
  expect(gaps[0]).toContain("5 张");
  expect(gaps[1]).toContain("10 条");
  expect(gaps[2]).toContain("父卡");
  expect(gaps[3]).toContain("kind");
  // A clean read has no gaps at all.
  expect(panelGaps(panel())).toHaveLength(0);
});

test("panelPhase: loading / empty / error / permission / success 各有来源", () => {
  expect(panelPhase({ loading: true, error: null, panel: null })).toBe("loading");
  expect(panelPhase({ loading: false, error: { status: 403 }, panel: null })).toBe("permission");
  expect(panelPhase({ loading: false, error: { status: 500 }, panel: null })).toBe("error");
  expect(panelPhase({ loading: false, error: null, panel: null })).toBe("empty");
  const blank = panel({ cards: [], members: [], events: [] });
  expect(panelPhase({ loading: false, error: null, panel: blank })).toBe("empty");
  expect(panelPhase({ loading: false, error: null, panel: panel() })).toBe("success");
});

test("resourceChanged / previewSummary: 一致、已变化、未核对三态", () => {
  const attachment = { taskId: "t", path: "a.md", hash: HASH_A };
  expect(resourceChanged(attachment, { path: "a.md", hash: HASH_A, size: 10, offset: 0, limit: 1, totalLines: 1, returnedLines: 1, nextOffset: 1, content: "", matchesReference: true })).toBe(false);
  expect(resourceChanged(attachment, { path: "a.md", hash: "sha256:" + "b".repeat(64), size: 10, offset: 0, limit: 1, totalLines: 1, returnedLines: 1, nextOffset: 1, content: "", matchesReference: false })).toBe(true);
  expect(resourceChanged(attachment, { path: "a.md", hash: HASH_A, size: 10, offset: 0, limit: 1, totalLines: 1, returnedLines: 1, nextOffset: 1, content: "" })).toBe(null);
  expect(previewSummary({ path: "a", hash: "", size: 2048, offset: 0, limit: 1, totalLines: 30, returnedLines: 20, nextOffset: 20, content: "" })).toBe("2 KB · 20/30 行");
});

test("shortHash / labels: 截断的只是显示，原值留在事件字段里", () => {
  expect(shortHash(HASH_A)).toBe("sha256:aaaaaaaaaa…");
  expect(shortHash("abc")).toBe("abc");
  expect(shortHash(undefined)).toBe("");
  expect(cardStatusLabel("blocked")).toBe("受阻");
  expect(cardStatusLabel("future_status")).toBe("future_status");
  expect(cardOriginLabel("fork")).toBe("分叉");
  expect(eventKindLabel("task.attached")).toBe("挂载资源");
  expect(eventKindLabel("task.future")).toBe("task.future");
});

test("panelCounts / shouldShowTaskPanel: 计数来自读数，入口由能力位决定", () => {
  const counts = panelCounts(panel());
  expect(counts).toEqual({ cards: 1, members: 0, attachments: 0, events: 1 });
  // A deployment that answered the capability decides; without an answer the
  // transport's own read decides.
  expect(shouldShowTaskPanel({ "task.panel": false }, true)).toBe(false);
  expect(shouldShowTaskPanel({ "task.panel": true }, false)).toBe(true);
  expect(shouldShowTaskPanel(null, false)).toBe(false);
  expect(shouldShowTaskPanel(undefined, true)).toBe(true);
});
