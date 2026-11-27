// TUI gate for the local command-failure overlay (review P1-B): the row keeps
// the 202's pending status in the ledger view but renders the local 403 error
// and labels it as a local record.
import { expect, open, test } from "nsh/test";

test("TUI: 本地命令错误覆盖 pending 行并标明无审计行", async () => {
  const app = await open("./commands-local-error.harness.tsx");
  await app.waitFor("id:command-invocation-local-r-local");
  const text = app.texts().join("\n");
  expect(text).toContain("本地记录");
  expect(text).toContain("没有执行该命令的权限。");
  expect(text).toContain("失败");
  expect(text).not.toContain("等待审批");
});
