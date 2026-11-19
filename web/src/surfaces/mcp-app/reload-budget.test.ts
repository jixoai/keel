// The navigation reload budget (change feat-mcp-apps-events, P2 hygiene):
// ten automatic reloads are allowed; the eleventh is refused, which is what
// stops a self-navigating app from looping the host forever.
import { expect, test } from "nsh/test";
import { createReloadBudget, MAX_NAVIGATION_RELOADS, reloadBudgetAuditEntry } from "./reload-budget";

test("reload 预算：默认 10 次自动重载，第 11 次拒绝", () => {
  const budget = createReloadBudget();
  expect(budget.limit).toBe(MAX_NAVIGATION_RELOADS);
  for (let i = 0; i < MAX_NAVIGATION_RELOADS; i++) {
    expect(budget.consume()).toBe("reload");
  }
  expect(budget.used).toBe(MAX_NAVIGATION_RELOADS);
  expect(budget.consume()).toBe("exhausted");
  expect(budget.consume()).toBe("exhausted");
  expect(budget.used).toBe(MAX_NAVIGATION_RELOADS);
});

test("reload 预算：小上限可按实例设定；非法值回落默认", () => {
  const small = createReloadBudget(3);
  expect(small.consume()).toBe("reload");
  expect(small.consume()).toBe("reload");
  expect(small.consume()).toBe("reload");
  expect(small.consume()).toBe("exhausted");
  expect(createReloadBudget(0).limit).toBe(MAX_NAVIGATION_RELOADS);
  expect(createReloadBudget(-2).limit).toBe(MAX_NAVIGATION_RELOADS);
});

test("reload 预算：导航循环最多重载 10 次后停止（scheduleReload 计数模型）", () => {
  const budget = createReloadBudget();
  let stopped = false;
  for (let i = 0; i < 100 && !stopped; i++) {
    if (budget.consume() === "exhausted") stopped = true;
  }
  expect(stopped).toBe(true);
  expect(budget.used).toBe(MAX_NAVIGATION_RELOADS);
});

test("预算耗尽的审计项：action/result 在闭集内，detail 不含 URL query", () => {
  const entry = reloadBudgetAuditEntry("mcpapp_0123456789abcdef", "weather", "sess-1");
  expect(entry.action).toBe("protocol.violation");
  expect(entry.result).toBe("refused");
  expect(entry.detail).toBe(`reload budget exhausted (${MAX_NAVIGATION_RELOADS})`);
  expect(entry.detail.includes("?")).toBe(false);
  expect(entry.appId).toBe("mcpapp_0123456789abcdef");
  expect(entry.server).toBe("weather");
  expect(entry.sessionId).toBe("sess-1");
  expect(reloadBudgetAuditEntry("mcpapp_0123456789abcdef").server).toBe(undefined);
});
