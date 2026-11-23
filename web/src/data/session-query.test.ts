// The bounded export's rendering — change keel-harness-v2, W12
// `keel.session-query`. The API returns one typed page; the client's only job
// is to render exactly that page as JSONL and say how much it copied.
import { expect, test } from "nsh/test";
import type { SessionExportView } from "./contract";
import { exportJSONL, exportSummary } from "./session-query";

const view = (patch: Partial<SessionExportView> = {}): SessionExportView => ({
  sessionId: "s-1",
  title: "发布准备周报",
  messages: [
    { seq: 1, kind: "user.message", role: "user", at: "2026-11-23T00:00:00.000Z", text: "帮我总结发布准备" },
    { seq: 2, kind: "assistant.message", role: "assistant", at: "2026-11-23T00:00:01.000Z", text: "已检查工作区" },
  ],
  count: 2,
  limit: 200,
  hasMore: false,
  ...patch,
});

test("session-query: 导出只写页面里的消息，一行一条", () => {
  const lines = exportJSONL(view()).split("\n");
  expect(lines.length).toBe(2);
  const first = JSON.parse(lines[0]) as Record<string, unknown>;
  expect(first.seq).toBe(1);
  expect(first.role).toBe("user");
  expect(first.text).toBe("帮我总结发布准备");
  const second = JSON.parse(lines[1]) as Record<string, unknown>;
  expect(second.text).toBe("已检查工作区");
  // Nothing the page did not carry is invented.
  expect(Object.keys(first).sort()).toEqual(["at", "kind", "role", "seq", "text"]);
});

test("session-query: 摘要说明本页数量与是否还有更多", () => {
  expect(exportSummary(view())).toBe("已复制 2 条消息。");
  expect(exportSummary(view({ hasMore: true, limit: 200 }))).toContain("本页上限 200 条");
  expect(exportSummary(view({ messages: [], count: 0 }))).toBe("这个会话还没有可导出的消息。");
});
