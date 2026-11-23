// The development fixture's task-panel fidelity — change keel-harness-v2, W4
// follow-ups. The fixture is the run surface's demo read, so its tree must have
// the projection's own shape: members are cards whose parent is an own card,
// ancestors are the parents that live elsewhere, the stream is the asked
// session's own ledger only, and the event page is the newest N.
import { expect, test } from "nsh/test";
import { FixtureIdentity, fixtureForkSessionId, fixtureRunSessionId } from "./fixture-identity";

test("fixture taskPanel: 成员只认本会话卡的子卡，事件流不抄别的会话", async () => {
  const identity = new FixtureIdentity();
  const run = await identity.taskPanel(fixtureRunSessionId);
  expect(run.cards?.map((card) => card.taskId)).toEqual(["task-captain", "task-captain-summary"]);
  // Only the fork's parent is an own card. The order/knowledge/aftersales cards
  // have parents elsewhere (or none), so they are not members of this session.
  expect(run.members?.map((card) => card.taskId)).toEqual(["task-refund-fork"]);
  const memberIds = new Set(run.members?.map((card) => card.taskId));
  expect(memberIds.has("task-knowledge")).toBe(false);
  expect(memberIds.has("task-aftersales")).toBe(false);
  expect(memberIds.has("task-refund")).toBe(false);
  // A member's row lives in its own session: its events do not appear in this
  // session's stream.
  expect(run.events?.some((event) => event.taskId === "task-knowledge")).toBe(false);
  expect(run.events?.some((event) => event.taskId === "task-refund-fork")).toBe(false);
});

test("fixture taskPanel: 祖先与 parentTaskId 跟投影一致", async () => {
  const identity = new FixtureIdentity();
  const refund = await identity.taskPanel(fixtureForkSessionId);
  // The asked session's own rows are the rows whose sessionId is that session.
  expect(refund.cards?.map((card) => card.taskId).sort()).toEqual(["task-refund", "task-refund-fork"]);
  const fork = refund.cards?.find((card) => card.taskId === "task-refund-fork");
  expect(fork?.parentTaskId).toBe("task-captain");
  // Both external parents are named, not copied: the order card (the refund
  // row's parent) and the captain card (the fork's parent).
  expect(refund.ancestors?.map((card) => card.taskId).sort()).toEqual(["task-captain", "task-order"]);
  for (const ancestor of refund.ancestors ?? []) {
    expect(ancestor.member).toBe(true);
  }
});

test("fixture taskPanel: 事件页保留最新 N 条并说明截断", async () => {
  const identity = new FixtureIdentity();
  const full = await identity.taskPanel(fixtureRunSessionId);
  const paged = await identity.taskPanel(fixtureRunSessionId, 1, 2);
  const allSeqs = (full.events ?? []).map((event) => event.seq);
  expect(paged.events?.map((event) => event.seq)).toEqual(allSeqs.slice(-2));
  expect(paged.limits?.eventsTruncated).toBe(true);
  expect(paged.limits?.eventLimit).toBe(2);
  expect(paged.members).toHaveLength(1);
  expect(paged.limits?.truncated).toBe(false);
});
