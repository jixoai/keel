// The super-agent board's pure vocabulary — change keel-collab-superagent,
// 4.2/4.3.
//
// The four headline counters must be disjoint (one ticket, one bucket) and the
// deadline words must not claim a question is still open when it is past due.
// These cases pin the decisions the page draws; rendering is covered by
// `superagent.tui.test.tsx`.
import { expect, test } from "nsh/test";
import type { SuperagentTicketView } from "./contract";
import { superagentCounts, superagentDeadline, superagentSourceText, superagentStatusLabel } from "./superagent";

const ticket = (id: string, status: string, openAttentionCount = 0): SuperagentTicketView => ({
  id,
  title: id,
  status,
  openAttentionCount,
});

test("superagent 四类计数：一票只进一个桶，待人工优先于底层状态", () => {
  const counts = superagentCounts([
    ticket("t-open", "open"),
    ticket("t-active", "active"),
    ticket("t-asking", "active", 1),
    ticket("t-blocked", "blocked"),
    ticket("t-done", "done"),
    ticket("t-failed", "failed"),
    ticket("t-cancelled", "cancelled"),
    ticket("t-odd", "paused"),
  ]);
  // open + active are 执行中; an active ticket with an open question is 待人工.
  expect(counts.running).toBe(2);
  expect(counts.waiting).toBe(2);
  expect(counts.done).toBe(1);
  expect(counts.failed).toBe(2);
  expect(counts.other).toBe(1);
  expect(counts.total).toBe(8);
  // The four headline buckets plus `other` account for every row.
  expect(counts.waiting + counts.running + counts.done + counts.failed + counts.other).toBe(counts.total);
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
