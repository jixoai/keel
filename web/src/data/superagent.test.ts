// The super-agent board's pure vocabulary — change keel-collab-superagent,
// 4.2/4.3.
//
// The four headline counters must be disjoint (one ticket, one bucket) and the
// deadline words must not claim a question is still open when it is past due.
// These cases pin the decisions the page draws; rendering is covered by
// `superagent.tui.test.tsx`.
import { expect, test } from "nsh/test";
import type { SuperagentAnswerResult, SuperagentApi, SuperagentBoardView, SuperagentTicketView } from "./contract";
import { setEffectivePermissions } from "./permissions";
import { superagentAccess, superagentCounts, superagentDeadline, superagentSourceText, superagentStatusLabel } from "./superagent";
import { store } from "./store";

const ticket = (id: string, status: string, openAttentionCount = 0): SuperagentTicketView => ({
  id,
  title: id,
  status,
  openAttentionCount,
});

test("superagent 计数：running 只含 active，open/cancelling 单独归类，待人工优先", () => {
  const counts = superagentCounts([
    ticket("t-open", "open"),
    ticket("t-active", "active"),
    ticket("t-asking", "active", 1),
    ticket("t-blocked", "blocked"),
    ticket("t-cancelling", "cancelling"),
    ticket("t-done", "done"),
    ticket("t-failed", "failed"),
    ticket("t-cancelled", "cancelled"),
    ticket("t-odd", "paused"),
  ]);
  // 执行中 is only `active`; an active ticket with an open question is 待人工.
  expect(counts.running).toBe(1);
  expect(counts.waiting).toBe(2);
  expect(counts.pending).toBe(1);
  expect(counts.stopping).toBe(1);
  expect(counts.done).toBe(1);
  expect(counts.failed).toBe(2);
  expect(counts.other).toBe(1);
  expect(counts.total).toBe(9);
  // Every row lands in exactly one bucket, including the transitional states.
  expect(
    counts.waiting + counts.running + counts.pending + counts.stopping + counts.done + counts.failed + counts.other,
  ).toBe(counts.total);
});

test("superagent 时限：今天/明天/过期/无期限各说各的，已过期不会被说成还能答", () => {
  const now = new Date("2026-11-26T10:00:00").getTime();
  const minutes = (count: number) => new Date(now + count * 60_000).toISOString();

  const later = superagentDeadline(minutes(90), now);
  expect(later.expired).toBe(false);
  expect(later.text).toContain("今天");
  expect(later.text).toContain("剩 1 小时 30 分");

  const tomorrow = superagentDeadline(new Date("2026-11-27T09:30:00").toISOString(), now);
  expect(tomorrow.expired).toBe(false);
  expect(tomorrow.text).toContain("明天");

  const past = superagentDeadline(minutes(-5), now);
  expect(past.expired).toBe(true);
  expect(past.text).toContain("已过期");
  expect(past.text).not.toContain("剩");

  expect(superagentDeadline(undefined, now)).toEqual({ expired: false, text: "无期限" });
  expect(superagentDeadline("0001-01-01T00:00:00Z", now)).toEqual({ expired: false, text: "无期限" });
  expect(superagentDeadline("not-a-date", now).expired).toBe(false);
});

test("superagent 词表：未知状态保留原文，来源行不编造来源", () => {
  expect(superagentStatusLabel("blocked")).toBe("待人工");
  expect(superagentStatusLabel("paused")).toBe("paused");
  expect(superagentStatusLabel(undefined)).toBe("未知");
  expect(superagentSourceText({ source: "github", kind: "issue", id: "1042" })).toBe("GitHub · Issue #1042");
  expect(superagentSourceText(undefined)).toBe("未记录来源");
});

test("superagent 后台重读保留写反馈，只有手动刷新才清空", async () => {
  let boardCalls = 0;
  let inboxCalls = 0;
  const transport: SuperagentApi = {
    async superagentBoard() {
      boardCalls += 1;
      return { tenantId: "t-1", modeId: "superagent", tickets: [] };
    },
    async superagentAttention() {
      inboxCalls += 1;
      return { tenantId: "t-1", items: [] };
    },
    async answerSuperagentAttention() {
      throw new Error("not used");
    },
    async resumeSuperagentTicket() {
      throw new Error("not used");
    },
    async stopSuperagentTicket() {
      throw new Error("not used");
    },
    async retrySuperagentPending() {
      throw new Error("not used");
    },
    async dismissSuperagentPending() {
      throw new Error("not used");
    },
  };
  store.setSuperagentTransport(transport);
  superagentAccess.reset();

  await superagentAccess.refresh();
  expect(boardCalls).toBe(1);
  expect(inboxCalls).toBe(1);

  superagentAccess.notice.set("答复已提交，工单已继续执行。");
  superagentAccess.actionError.set({
    id: "a-1",
    kind: "answer",
    failure: { status: 403, message: "答复失败" },
  });

  // A background/SSE re-read keeps the operator's last write feedback.
  await superagentAccess.refreshForEvent(new Set(["superagent"]));
  expect(boardCalls).toBe(2);
  expect(inboxCalls).toBe(2);
  expect(superagentAccess.notice()).toBe("答复已提交，工单已继续执行。");
  expect(superagentAccess.actionError()?.id).toBe("a-1");

  // The manual 刷新 is an explicit read: it clears both.
  await superagentAccess.refresh();
  expect(boardCalls).toBe(3);
  expect(inboxCalls).toBe(3);
  expect(superagentAccess.notice()).toBe(null);
  expect(superagentAccess.actionError()).toBe(null);

  store.setSuperagentTransport(null);
  superagentAccess.reset();
});

// The answer route's two recorded-not-delivered states must reach the page as
// "recorded, waiting" / "recorded, failed" instead of a blank failure.
test("superagent 答复：202 等待投递与 409 投递失败都是已记录事实", async () => {
  setEffectivePermissions({ "task.write": "team" }, false);
  let result: SuperagentAnswerResult = {
    attention: { id: "a-1", workItemId: "t-1", prompt: "q" },
    deliveryPending: true,
  };
  const transport: SuperagentApi = {
    async superagentBoard() {
      return { tenantId: "t-1", modeId: "superagent", tickets: [] };
    },
    async superagentAttention() {
      return { tenantId: "t-1", items: [] };
    },
    async answerSuperagentAttention() {
      return result;
    },
    async resumeSuperagentTicket() {
      throw new Error("not used");
    },
    async stopSuperagentTicket() {
      throw new Error("not used");
    },
    async retrySuperagentPending() {
      throw new Error("not used");
    },
    async dismissSuperagentPending() {
      throw new Error("not used");
    },
  };
  store.setSuperagentTransport(transport);
  superagentAccess.reset();
  await superagentAccess.refresh();

  expect(await superagentAccess.answer("a-1", { text: "yes" })).toBe(true);
  expect(superagentAccess.notice()).toContain("等待投递");
  expect(superagentAccess.actionError()).toBe(null);

  result = { deliveryFailed: true, deliveryCode: "answer_delivery_failed" };
  expect(await superagentAccess.answer("a-1", { text: "yes" })).toBe(true);
  expect(superagentAccess.notice()).toContain("投递失败");

  store.setSuperagentTransport(null);
  superagentAccess.reset();
});

// The board carries the durable pending row itself; retry/dismiss are the two
// operator decisions and each calls its own route.
test("superagent 待投递入口：失败行透传，重试/清除调用各自路由", async () => {
  setEffectivePermissions({ "task.write": "team" }, false);
  const calls: string[] = [];
  const current: SuperagentBoardView = {
    tenantId: "t-1",
    modeId: "superagent",
    tickets: [
      {
        id: "t-1",
        title: "对账",
        status: "failed",
        pending: [
          {
            id: "fixture:manual-76:edit-1",
            kind: "followup",
            text: "body",
            trusted: true,
            allowDispatch: true,
            attempts: 12,
            lastError: "deployment cannot deliver",
            failed: true,
          },
        ],
        pendingCount: 1,
        failedPendingCount: 1,
      },
    ],
  };
  const transport: SuperagentApi = {
    async superagentBoard() {
      return current;
    },
    async superagentAttention() {
      return { tenantId: "t-1", items: [] };
    },
    async answerSuperagentAttention() {
      throw new Error("not used");
    },
    async resumeSuperagentTicket() {
      throw new Error("not used");
    },
    async stopSuperagentTicket() {
      throw new Error("not used");
    },
    async retrySuperagentPending(ticketId: string, pendingId: string) {
      calls.push(`retry:${ticketId}:${pendingId}`);
      return current.tickets[0];
    },
    async dismissSuperagentPending(ticketId: string, pendingId: string) {
      calls.push(`dismiss:${ticketId}:${pendingId}`);
      return current.tickets[0];
    },
  };
  store.setSuperagentTransport(transport);
  superagentAccess.reset();
  await superagentAccess.refresh();
  const row = superagentAccess.board()?.tickets[0];
  expect(row?.pending?.[0]?.failed).toBe(true);
  expect(row?.failedPendingCount).toBe(1);
  expect(row?.pending?.[0]?.lastError).toBe("deployment cannot deliver");

  expect(await superagentAccess.retryPending("t-1", "fixture:manual-76:edit-1")).toBe(true);
  expect(superagentAccess.notice()).toContain("重新投递");
  expect(await superagentAccess.dismissPending("t-1", "fixture:manual-76:edit-1")).toBe(true);
  expect(superagentAccess.notice()).toContain("已清除");
  expect(calls).toEqual([
    "retry:t-1:fixture:manual-76:edit-1",
    "dismiss:t-1:fixture:manual-76:edit-1",
  ]);

  store.setSuperagentTransport(null);
  superagentAccess.reset();
});
