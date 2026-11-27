// The pending destructive command machine (change keel-plugin-commands,
// review P1-2): the arguments live only on the issuing client, so the store
// persists them for the decision window and replays the SAME requestId when
// the decision is answered (even from another client), times out, or the 202
// answer was lost. Covered through the real store and an in-memory transport,
// with a memory sessionStorage for the reload path.
import { expect, test } from "nsh/test";
import type { CommandApi, CommandCatalogView, CommandExecutionView, DecisionView } from "./contract";
import { KeelApiError, KeelHttp } from "./http";
import { store } from "./store";

type Call = { sessionId: string; requestId: string; name: string; args: Record<string, unknown> };

const pendingResult = (decisionId: string, timeoutMs = 60_000): CommandExecutionView => ({
  requestId: "r",
  command: "e2e.commands:purge",
  source: "plugin",
  status: "pending_decision",
  decisionId,
  pending: true,
  timeoutMs,
});

const okResult = (requestId: string): CommandExecutionView => ({
  requestId,
  command: "e2e.commands:purge",
  source: "plugin",
  status: "ok",
  data: { purged: 7 },
});

const decision = (decisionId: string, status: DecisionView["status"]): DecisionView => ({
  decisionId,
  seq: 1,
  question: "Run the destructive command /e2e.commands:purge?",
  options: [{ optionId: "allow", label: "Run" }],
  status,
});

class MemoryStorage {
  private readonly data = new Map<string, string>();
  getItem(key: string): string | null {
    return this.data.get(key) ?? null;
  }
  setItem(key: string, value: string): void {
    this.data.set(key, value);
  }
  removeItem(key: string): void {
    this.data.delete(key);
  }
}

function installStorage(): MemoryStorage {
  const storage = new MemoryStorage();
  (globalThis as { sessionStorage?: MemoryStorage }).sessionStorage = storage;
  return storage;
}

const waitFor = async (predicate: () => boolean, timeoutMs = 5000): Promise<void> => {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  throw new Error("condition did not settle in time");
};

function fakeTransport(options: {
  calls: Call[];
  decisionStatus: () => DecisionView["status"];
  decisionId?: string;
  onExecute: (call: Call, attempt: number) => Promise<CommandExecutionView>;
}): CommandApi {
  const decisionId = options.decisionId ?? "dec-1";
  return {
    commandCatalog: async (): Promise<CommandCatalogView> => ({ commands: [], truncated: false }),
    executeCommand: async (sessionId, name, args, requestId) => {
      options.calls.push({ sessionId, requestId, name, args });
      return await options.onExecute(
        { sessionId, requestId, name, args },
        options.calls.length,
      );
    },
    decisions: async () => [decision(decisionId, options.decisionStatus())],
  };
}

test("pending: 切走后另一客户端允许审批 → 同 requestId 只执行一次", async () => {
  const storage = installStorage();
  store.resetCommandStateForTest();
  store.activeId.set("session-a");
  const calls: Call[] = [];
  let status: DecisionView["status"] = "open";
  const api = fakeTransport({
    calls,
    decisionStatus: () => status,
    onExecute: async (call, attempt) => (attempt === 1 ? pendingResult("dec-1") : okResult(call.requestId)),
  });
  store.setCommandTransport(api);
  try {
    const outcome = await store.runCommand("e2e.commands:purge", { confirm: true });
    expect(outcome.status).toBe("pending");
    const requestId = outcome.requestId;
    expect(calls).toHaveLength(1);
    expect(storage.getItem("keel.commands.pending.v1")).toContain(requestId);

    // The reader switches sessions; the pending set is not dropped.
    store.activeId.set("session-b");
    status = "answered";
    await waitFor(() => calls.length >= 2, 5000);
    expect(calls[1]!.sessionId).toBe("session-a");
    expect(calls[1]!.requestId).toBe(requestId);
    expect(calls[1]!.args).toEqual({ confirm: true });
    // Terminal: the persisted copy is gone and no further replay happens.
    await waitFor(() => storage.getItem("keel.commands.pending.v1") === null, 2000);
    await new Promise((resolve) => setTimeout(resolve, 1200));
    expect(calls).toHaveLength(2);
  } finally {
    store.resetCommandStateForTest();
    delete (globalThis as { sessionStorage?: MemoryStorage }).sessionStorage;
  }
});

test("pending: 决策超时自动重放一次 → timed_out，不会永久 pending", async () => {
  installStorage();
  store.resetCommandStateForTest();
  store.activeId.set("session-a");
  const calls: Call[] = [];
  let status: DecisionView["status"] = "open";
  const api = fakeTransport({
    calls,
    decisionStatus: () => status,
    onExecute: async (call, attempt) => {
      if (attempt === 1) return pendingResult("dec-1", 100);
      throw new KeelApiError(
        408,
        "the destructive command's decision timed out",
        [],
        undefined,
        "decision_timeout",
        {},
        undefined,
        7,
      );
    },
  });
  store.setCommandTransport(api);
  try {
    const outcome = await store.runCommand("e2e.commands:purge", {});
    expect(outcome.status).toBe("pending");
    status = "timeout";
    await waitFor(() => calls.length >= 2, 5000);
    expect(calls[1]!.requestId).toBe(outcome.requestId);
    // The timeout replay is terminal: no third call.
    await new Promise((resolve) => setTimeout(resolve, 800));
    expect(calls).toHaveLength(2);
  } finally {
    store.resetCommandStateForTest();
  }
});

test("pending: 丢掉 202 响应 → 同 requestId 重发并接回审批", async () => {
  installStorage();
  store.resetCommandStateForTest();
  store.activeId.set("session-a");
  const calls: Call[] = [];
  let status: DecisionView["status"] = "open";
  const api = fakeTransport({
    calls,
    decisionStatus: () => status,
    onExecute: async (call, attempt) => {
      if (attempt === 1) throw new Error("network down");
      if (attempt === 2) return pendingResult("dec-1");
      return okResult(call.requestId);
    },
  });
  store.setCommandTransport(api);
  try {
    const outcome = await store.runCommand("e2e.commands:purge", {});
    expect(outcome.status).toBe("pending");
    const requestId = outcome.requestId;
    // The first call failed before a 202; the resend carries the same id.
    await waitFor(() => calls.length >= 2, 4000);
    expect(calls[1]!.requestId).toBe(requestId);
    expect(calls[1]!.args).toEqual({});
    status = "answered";
    await waitFor(() => calls.length >= 3, 4000);
    expect(calls[2]!.requestId).toBe(requestId);
  } finally {
    store.resetCommandStateForTest();
  }
});

test("pending: 本客户端点击允许 → answerDecision 立即用原 requestId 重放", async () => {
  installStorage();
  store.resetCommandStateForTest();
  store.activeId.set("session-a");
  const calls: Call[] = [];
  const api = fakeTransport({
    calls,
    decisionStatus: () => "open",
    onExecute: async (call, attempt) => (attempt === 1 ? pendingResult("dec-1") : okResult(call.requestId)),
  });
  const answered: string[] = [];
  const http = new KeelHttp("http://keel.test", async (input, init) => {
    answered.push(`${init?.method ?? "GET"} ${String(input)}`);
    return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
  });
  store.useHTTPForTest(http);
  store.setCommandTransport(api);
  try {
    const outcome = await store.runCommand("e2e.commands:purge", {});
    expect(outcome.status).toBe("pending");
    await store.answerDecision("dec-1", "allow");
    await waitFor(() => calls.length >= 2, 3000);
    expect(answered.some((line) => line.includes("/decisions/dec-1"))).toBe(true);
    expect(calls[1]!.requestId).toBe(outcome.requestId);
    expect(calls[1]!.args).toEqual({});
  } finally {
    store.resetCommandStateForTest();
    store.useHTTPForTest(null);
    delete (globalThis as { sessionStorage?: MemoryStorage }).sessionStorage;
  }
});

test("pending: 审批后 403 无审计行 → 本地错误可见，不静默丢弃", async () => {
  const storage = installStorage();
  store.resetCommandStateForTest();
  store.activeId.set("session-a");
  const calls: Call[] = [];
  const api = fakeTransport({
    calls,
    decisionId: "dec-403",
    decisionStatus: () => "open",
    onExecute: async (call, attempt) => {
      if (attempt === 1) return pendingResult("dec-403");
      throw new KeelApiError(403, "permission denied", [], undefined, "permission_denied");
    },
  });
  const http = new KeelHttp("http://keel.test", async () =>
    new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }),
  );
  store.useHTTPForTest(http);
  store.setCommandTransport(api);
  try {
    const outcome = await store.runCommand("e2e.commands:purge", { confirm: true });
    await store.answerDecision("dec-403", "allow");
    await waitFor(() => calls.length >= 2, 3000);
    const local = store.commandLocalOutcome(outcome.requestId);
    expect(local?.local).toBe(true);
    expect(local?.errorCode).toBe("permission_denied");
    expect(local?.error).toContain("权限");
    await waitFor(() => storage.getItem("keel.commands.pending.v1") === null, 2000);
  } finally {
    store.resetCommandStateForTest();
    store.useHTTPForTest(null);
    delete (globalThis as { sessionStorage?: MemoryStorage }).sessionStorage;
  }
});

test("pending: 插件卸载/未知命令 404 → 本地错误可见，不静默丢弃", async () => {
  installStorage();
  store.resetCommandStateForTest();
  store.activeId.set("session-a");
  const calls: Call[] = [];
  const api = fakeTransport({
    calls,
    decisionId: "dec-404",
    decisionStatus: () => "open",
    onExecute: async (call, attempt) => {
      if (attempt === 1) return pendingResult("dec-404");
      throw new KeelApiError(404, "unknown command", [], undefined, "unknown_command");
    },
  });
  const http = new KeelHttp("http://keel.test", async () =>
    new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }),
  );
  store.useHTTPForTest(http);
  store.setCommandTransport(api);
  try {
    const outcome = await store.runCommand("e2e.commands:purge", {});
    await store.answerDecision("dec-404", "allow");
    await waitFor(() => calls.length >= 2, 3000);
    const local = store.commandLocalOutcome(outcome.requestId);
    expect(local?.errorCode).toBe("unknown_command");
    expect(local?.error).toContain("不在当前目录");
  } finally {
    store.resetCommandStateForTest();
    store.useHTTPForTest(null);
    delete (globalThis as { sessionStorage?: MemoryStorage }).sessionStorage;
  }
});

test("pending: 忙等重试耗尽 → 中文错误可见，可重新执行", async () => {
  const storage = installStorage();
  store.resetCommandStateForTest();
  store.activeId.set("session-a");
  const calls: Call[] = [];
  const api = fakeTransport({
    calls,
    decisionId: "dec-busy",
    decisionStatus: () => "open",
    onExecute: async (call, attempt) => {
      if (attempt === 1) return pendingResult("dec-busy");
      throw new KeelApiError(409, "session is busy", [], undefined, "session_busy");
    },
  });
  const http = new KeelHttp("http://keel.test", async () =>
    new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }),
  );
  store.useHTTPForTest(http);
  store.setCommandTransport(api);
  try {
    const outcome = await store.runCommand("e2e.commands:purge", {});
    await store.answerDecision("dec-busy", "allow");
    await waitFor(() => calls.length >= 5, 12000);
    const local = store.commandLocalOutcome(outcome.requestId);
    expect(local?.errorCode).toBe("session_busy");
    expect(local?.error).toContain("会话持续繁忙");
    await waitFor(() => storage.getItem("keel.commands.pending.v1") === null, 2000);
  } finally {
    store.resetCommandStateForTest();
    store.useHTTPForTest(null);
    delete (globalThis as { sessionStorage?: MemoryStorage }).sessionStorage;
  }
});

test("pending: durable busy（eventSeq）不标本地记录", async () => {
  installStorage();
  store.resetCommandStateForTest();
  store.activeId.set("session-a");
  const calls: Call[] = [];
  const api = fakeTransport({
    calls,
    decisionStatus: () => "open",
    onExecute: async () => {
      throw new KeelApiError(409, "session is busy", [], undefined, "session_busy", {}, undefined, 9);
    },
  });
  store.setCommandTransport(api);
  try {
    const outcome = await store.runCommand("e2e.commands:purge", {});
    expect(outcome.status).toBe("error");
    expect(store.commandLocalOutcome(outcome.requestId)).toBeUndefined();
  } finally {
    store.resetCommandStateForTest();
  }
});

test("pending: 初始 403（未产生审计行）也留下本地错误", async () => {
  installStorage();
  store.resetCommandStateForTest();
  store.activeId.set("session-a");
  const calls: Call[] = [];
  const api = fakeTransport({
    calls,
    decisionStatus: () => "open",
    onExecute: async () => {
      throw new KeelApiError(403, "permission denied", [], undefined, "permission_denied");
    },
  });
  store.setCommandTransport(api);
  try {
    const outcome = await store.runCommand("e2e.commands:purge", {});
    expect(outcome.status).toBe("error");
    await waitFor(() => store.commandLocalOutcome(outcome.requestId) !== undefined, 1000);
    expect(store.commandLocalOutcome(outcome.requestId)?.local).toBe(true);
  } finally {
    store.resetCommandStateForTest();
  }
});

test("pending: 重载后从 sessionStorage 恢复并用原参数重放", async () => {
  const storage = installStorage();
  store.resetCommandStateForTest();
  store.activeId.set("session-a");
  const calls: Call[] = [];
  let status: DecisionView["status"] = "open";
  const api = fakeTransport({
    calls,
    decisionStatus: () => status,
    onExecute: async (call, attempt) => (attempt === 1 ? pendingResult("dec-1") : okResult(call.requestId)),
  });
  store.setCommandTransport(api);
  try {
    const outcome = await store.runCommand("e2e.commands:purge", { confirm: true });
    expect(outcome.status).toBe("pending");
    expect(storage.getItem("keel.commands.pending.v1")).not.toBeNull();
    // Reload: the in-memory set and timers are gone; only sessionStorage is left.
    store.simulateCommandReloadForTest("session-a");
    expect(calls).toHaveLength(1);
    status = "answered";
    await waitFor(() => calls.length >= 2, 5000);
    expect(calls[1]!.requestId).toBe(outcome.requestId);
    expect(calls[1]!.args).toEqual({ confirm: true });
    await waitFor(() => storage.getItem("keel.commands.pending.v1") === null, 2000);
  } finally {
    store.resetCommandStateForTest();
    delete (globalThis as { sessionStorage?: MemoryStorage }).sessionStorage;
  }
});
