// The bridge's contract, pinned without a DOM (change feat-mcp-apps-events,
// P2.6). A fake clock schedules the 5 s handshake and 1 s teardown deadlines;
// `posted` is the exact list of frames the host sent, so order and counts are
// assertions, not hopes.
import { expect, test } from "nsh/test";
import type { McpAppView } from "../../data/contract";
import { mcpAppHostCapabilities, mcpAppToolInput } from "../../data/mcp-apps";
import {
  byteLength,
  createAppBridge,
  HANDSHAKE_TIMEOUT_MS,
  INLINE_MAX_HEIGHT,
  MAX_FRAME_BYTES,
  parseFrame,
  type AppBridge,
  type AppBridgeOptions,
  type BridgeFailure,
  type DisplayMode,
} from "./bridge";
import { frameLoadVerdict } from "./frame-load";

const APP: McpAppView = {
  appId: "mcpapp_0123456789abcdef",
  server: "weather",
  title: "天气面板",
  resourceUri: "ui://weather/dashboard",
  mimeType: "text/html;profile=mcp-app",
  visibility: ["model", "app"],
  tools: [],
  csp: "default-src 'none'",
  cspDomains: { connectDomains: ["https://api.example.com"] },
  permissions: {},
  hash: "a".repeat(64),
  documentUrl: `/v1/mcp/apps/mcpapp_0123456789abcdef/view?v=${"a".repeat(64)}`,
};

interface FakeTimer {
  at: number;
  id: number;
  fn: () => void;
}

function fakeClock() {
  let current = 0;
  let sequence = 0;
  let timers: FakeTimer[] = [];
  return {
    now: () => current,
    schedule: (fn: () => void, ms: number) => {
      const id = ++sequence;
      timers.push({ at: current + ms, id, fn });
      return id;
    },
    cancel: (handle: unknown) => {
      timers = timers.filter((timer) => timer.id !== handle);
    },
    advance(ms: number) {
      current += ms;
      const due = timers.filter((timer) => timer.at <= current).sort((a, b) => a.at - b.at);
      timers = timers.filter((timer) => timer.at > current);
      for (const timer of due) timer.fn();
    },
  };
}

interface Harness {
  bridge: AppBridge;
  posted: Record<string, unknown>[];
  destroyed: BridgeFailure[];
  sizes: { width?: number; height?: number }[];
  modes: DisplayMode[];
  clock: ReturnType<typeof fakeClock>;
  toolResult: { value: { content?: unknown; structuredContent?: unknown; isError?: boolean } | null };
}

function harness(overrides: Partial<AppBridgeOptions> = {}): Harness {
  const posted: Record<string, unknown>[] = [];
  const destroyed: BridgeFailure[] = [];
  const sizes: { width?: number; height?: number }[] = [];
  const modes: DisplayMode[] = [];
  const clock = fakeClock();
  const toolResult: Harness["toolResult"] = { value: null };
  const bridge = createAppBridge({
    post: (frame) => posted.push(frame),
    toolInput: () => ({ city: "上海" }),
    toolResult: () => toolResult.value,
    hostCapabilities: () => ({ logging: {} }),
    hostContext: () => ({ theme: "dark", displayMode: "inline" }),
    app: () => APP,
    setDisplayMode: (mode) => mode,
    onSize: (size) => sizes.push(size),
    onDisplayMode: (mode) => modes.push(mode),
    onDestroyed: (failure) => destroyed.push(failure),
    now: clock.now,
    schedule: clock.schedule,
    cancel: clock.cancel,
    ...overrides,
  });
  return { bridge, posted, destroyed, sizes, modes, clock, toolResult };
}

const initialize = (available: string[] = ["inline", "fullscreen"], id = 1) =>
  JSON.stringify({
    jsonrpc: "2.0",
    id,
    method: "ui/initialize",
    params: {
      protocolVersion: "2026-01-26",
      appInfo: { name: "weather-view", version: "1.0.0" },
      appCapabilities: { availableDisplayModes: available },
    },
  });

const INITIALIZED = JSON.stringify({ jsonrpc: "2.0", method: "ui/notifications/initialized" });

const notifications = (posted: Record<string, unknown>[]) =>
  posted.filter((frame) => typeof frame.method === "string");

test("握手：initialize 应答后 initialized 才发 tool-input，且恰好一次", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(initialize());

  // Only the response so far: no notification before `initialized`.
  expect(h.posted.length).toBe(1);
  const response = h.posted[0] as { result: Record<string, unknown> };
  expect(response.result.protocolVersion).toBe("2026-01-26");
  expect((response.result.hostInfo as { name: string }).name).toBe("Keel");
  expect((response.result.hostContext as { theme: string }).theme).toBe("dark");

  h.bridge.handleMessage(INITIALIZED);
  const input = h.posted.filter((frame) => frame.method === "ui/notifications/tool-input");
  expect(input.length).toBe(1);
  expect((input[0].params as { arguments: unknown }).arguments).toEqual({ city: "上海" });

  // A second initialized is ignored, never a second tool-input.
  h.bridge.handleMessage(INITIALIZED);
  expect(h.posted.filter((frame) => frame.method === "ui/notifications/tool-input").length).toBe(1);
  expect(h.bridge.snapshot().initialized).toBe(true);
});

test("tool-input：聚合 mcp 信封发出内层实参；直传对象原样", () => {
  const city = { city: "上海" };
  const envelope = { action: "call", server: "weather", tool: "dashboard", arguments: city };
  // The View's bridge hands `session.live.args` through `mcpAppToolInput`, so
  // the wire frame carries the called tool's own arguments, not the envelope.
  const h = harness({ toolInput: () => mcpAppToolInput(envelope) });
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  h.bridge.handleMessage(INITIALIZED);
  const input = h.posted.find((frame) => frame.method === "ui/notifications/tool-input") as {
    params: { arguments: unknown };
  };
  expect(input.params.arguments).toBe(city);
  expect(input.params.arguments).toEqual({ city: "上海" });
  expect(Object.keys(input.params.arguments as Record<string, unknown>).sort()).toEqual(["city"]);

  // A call row that already carries the tool's arguments is not re-shaped.
  const direct = { city: "北京" };
  const d = harness({ toolInput: () => mcpAppToolInput(direct) });
  d.bridge.start();
  d.bridge.handleMessage(initialize());
  d.bridge.handleMessage(INITIALIZED);
  const directInput = d.posted.find((frame) => frame.method === "ui/notifications/tool-input") as {
    params: { arguments: unknown };
  };
  expect(directInput.params.arguments).toBe(direct);
});

test("数据流：结果在手 → initialized 后 input 再 result；之后的结果不再重发", () => {
  const h = harness();
  h.toolResult.value = { content: [{ type: "text", text: "24°C" }], structuredContent: { tempC: 24 } };
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  expect(notifications(h.posted).length).toBe(0);
  h.bridge.handleMessage(INITIALIZED);

  const order = notifications(h.posted).map((frame) => frame.method);
  expect(order).toEqual(["ui/notifications/tool-input", "ui/notifications/tool-result"]);
  const result = notifications(h.posted)[1].params as Record<string, unknown>;
  expect(result.structuredContent).toEqual({ tempC: 24 });
  expect(result.isError).toBe(false);

  h.bridge.updateToolResult({ content: [{ type: "text", text: "again" }] });
  expect(notifications(h.posted).filter((frame) => frame.method === "ui/notifications/tool-result").length).toBe(1);
});

test("数据流：结果晚到 → 补发一次 tool-result", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  h.bridge.handleMessage(INITIALIZED);
  expect(notifications(h.posted).map((frame) => frame.method)).toEqual(["ui/notifications/tool-input"]);

  h.bridge.updateToolResult({ content: [{ type: "text", text: "late" }] });
  expect(notifications(h.posted).map((frame) => frame.method)).toEqual([
    "ui/notifications/tool-input",
    "ui/notifications/tool-result",
  ]);
});

test("超时：5 秒内没有 initialized → 销毁并给出一条 timeout 诊断", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  expect(h.destroyed.length).toBe(0);
  h.clock.advance(HANDSHAKE_TIMEOUT_MS);
  expect(h.destroyed.length).toBe(1);
  expect(h.destroyed[0].kind).toBe("timeout");
  expect(h.bridge.snapshot().phase).toBe("destroyed");
  // The response is the only frame; no notification leaked before the deadline.
  expect(notifications(h.posted).length).toBe(0);
});

test("首载：initialize 早于 load 时 start() 幂等，不重置 5 秒期限", () => {
  const h = harness();
  // A conforming app may initialize while the document is still parsing.
  h.bridge.handleMessage(initialize());
  expect(h.bridge.snapshot().phase).toBe("handshaking");
  const postedBefore = h.posted.length;
  h.clock.advance(3000);
  // The first load event arrives afterwards: the View maps this to start().
  expect(frameLoadVerdict(0, h.bridge.snapshot().phase, h.bridge.snapshot().initialized)).toBe("start");
  h.bridge.start();
  expect(h.bridge.snapshot().phase).toBe("handshaking");
  expect(h.posted.length).toBe(postedBefore);
  // The deadline still belongs to the original initialize, not to the load.
  h.clock.advance(HANDSHAKE_TIMEOUT_MS - 3000);
  expect(h.destroyed.length).toBe(1);
  expect(h.destroyed[0].kind).toBe("timeout");
});

test("协议违规：未知方法回 -32601 且实例销毁", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  h.bridge.handleMessage(INITIALIZED);
  h.bridge.handleMessage(JSON.stringify({ jsonrpc: "2.0", id: 7, method: "ui/nope" }));

  const refusal = h.posted.find((frame) => frame.id === 7) as { error: { code: number } };
  expect(refusal.error.code).toBe(-32601);
  expect(h.destroyed.length).toBe(1);
  expect(h.destroyed[0].kind).toBe("protocol");
});

test("P3 面未接线：tools/call 明确回 -32601，但不销毁实例", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  h.bridge.handleMessage(INITIALIZED);
  h.bridge.handleMessage(JSON.stringify({ jsonrpc: "2.0", id: 9, method: "tools/call", params: { name: "x" } }));

  const refusal = h.posted.find((frame) => frame.id === 9) as { error: { code: number } };
  expect(refusal.error.code).toBe(-32601);
  expect(h.destroyed.length).toBe(0);
  expect(h.bridge.snapshot().phase).toBe("ready");
});

test("协议违规：单帧超过 64 KiB 直接销毁且不回帧", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  h.bridge.handleMessage(INITIALIZED);
  const before = h.posted.length;
  h.bridge.handleMessage(JSON.stringify({ jsonrpc: "2.0", id: 3, method: "ping", params: { pad: "x".repeat(MAX_FRAME_BYTES) } }));
  expect(h.destroyed.length).toBe(1);
  expect(h.destroyed[0].kind).toBe("protocol");
  expect(h.posted.length).toBe(before);
});

test("来源：非沙箱 origin 的消息被拒绝并销毁", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(initialize(), "https://evil.example");
  expect(h.destroyed.length).toBe(1);
  expect(h.destroyed[0].kind).toBe("protocol");
  expect(h.posted.length).toBe(0);
  // The opaque origin the sandbox gives the frame is accepted.
  const ok = harness();
  ok.bridge.start();
  ok.bridge.handleMessage(initialize(), "null");
  expect(ok.destroyed.length).toBe(0);
  expect(ok.posted.length).toBe(1);
});

test("未初始化就发 initialized → 协议错误", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(INITIALIZED);
  expect(h.destroyed.length).toBe(1);
  expect(h.destroyed[0].kind).toBe("protocol");
});

test("尺寸：size-changed 驱动弹性高度，封顶 480 且不低于下限", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  h.bridge.handleMessage(INITIALIZED);
  h.bridge.handleMessage(JSON.stringify({ jsonrpc: "2.0", method: "ui/notifications/size-changed", params: { width: 640, height: 900 } }));
  expect(h.sizes[h.sizes.length - 1]).toEqual({ width: 640, height: INLINE_MAX_HEIGHT });
  h.bridge.handleMessage(JSON.stringify({ jsonrpc: "2.0", method: "ui/notifications/size-changed", params: { width: 640, height: 4 } }));
  expect(h.sizes[h.sizes.length - 1].height).toBe(80);
});

test("显示模式：只切到 View 声明的模式，pip 回当前模式", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(initialize(["inline"]));
  h.bridge.handleMessage(INITIALIZED);

  h.bridge.handleMessage(JSON.stringify({ jsonrpc: "2.0", id: 21, method: "ui/request-display-mode", params: { mode: "fullscreen" } }));
  expect((h.posted.find((frame) => frame.id === 21) as { result: { mode: string } }).result.mode).toBe("inline");
  h.bridge.handleMessage(JSON.stringify({ jsonrpc: "2.0", id: 22, method: "ui/request-display-mode", params: { mode: "pip" } }));
  expect((h.posted.find((frame) => frame.id === 22) as { result: { mode: string } }).result.mode).toBe("inline");
  expect(h.bridge.snapshot().displayMode).toBe("inline");
});

test("显示模式：声明 fullscreen 时切换并返回实际模式", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(initialize(["inline", "fullscreen"]));
  h.bridge.handleMessage(INITIALIZED);

  h.bridge.handleMessage(JSON.stringify({ jsonrpc: "2.0", id: 31, method: "ui/request-display-mode", params: { mode: "fullscreen" } }));
  expect((h.posted.find((frame) => frame.id === 31) as { result: { mode: string } }).result.mode).toBe("fullscreen");
  expect(h.bridge.snapshot().displayMode).toBe("fullscreen");
  const change = h.posted.find(
    (frame) => frame.method === "ui/notifications/host-context-changed",
  ) as { params: { displayMode: string } };
  expect(change.params.displayMode).toBe("fullscreen");
});

test("teardown：卸载前发 resource-teardown，回包或 1 秒后完成", () => {
  const teardowns: number[] = [];
  const h = harness({ onTeardown: () => teardowns.push(1) });
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  h.bridge.handleMessage(INITIALIZED);

  h.bridge.teardown();
  const request = h.posted.find((frame) => frame.method === "ui/resource-teardown") as {
    id: string;
    params: { reason: string };
  };
  expect(request.id).toBe("host-1");
  expect(request.params.reason).toBe("unmounted");
  expect(teardowns.length).toBe(0);

  h.bridge.handleMessage(JSON.stringify({ jsonrpc: "2.0", id: "host-1", result: {} }));
  expect(teardowns.length).toBe(1);
  h.clock.advance(1000);
  expect(teardowns.length).toBe(1);
});

test("teardown：View 不回包时 1 秒后完成", () => {
  const teardowns: number[] = [];
  const h = harness({ onTeardown: () => teardowns.push(1) });
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  h.bridge.handleMessage(INITIALIZED);
  h.bridge.teardown();
  expect(teardowns.length).toBe(0);
  h.clock.advance(1000);
  expect(teardowns.length).toBe(1);
});

test("取消：无结果时发 tool-cancelled，且不再发 tool-result", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  h.bridge.handleMessage(INITIALIZED);
  h.bridge.cancel("user stopped");
  const cancelled = h.posted.find((frame) => frame.method === "ui/notifications/tool-cancelled") as {
    params: { reason: string };
  };
  expect(cancelled.params.reason).toBe("user stopped");
  h.toolResult.value = { content: [{ type: "text", text: "late" }] };
  h.bridge.updateToolResult(h.toolResult.value);
  expect(h.posted.filter((frame) => frame.method === "ui/notifications/tool-result").length).toBe(0);
});

test("帧解析：非对象/错版本/坏 id 都拒绝", () => {
  expect("reason" in parseFrame("[]")).toBe(true);
  expect("reason" in parseFrame(JSON.stringify({ jsonrpc: "1.0", method: "ping", id: 1 }))).toBe(true);
  expect("reason" in parseFrame(JSON.stringify({ jsonrpc: "2.0", method: "ping", id: {} }))).toBe(true);
  expect("frame" in parseFrame(JSON.stringify({ jsonrpc: "2.0", method: "ping", id: 1 }))).toBe(true);
});

test("字节长度：中文按 UTF-8 计", () => {
  expect(byteLength("abc")).toBe(3);
  expect(byteLength("中")).toBe(3);
  expect(byteLength("🙂")).toBe(4);
});

test("限速：单实例每秒最多 200 帧，超出即销毁", () => {
  const h = harness();
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  h.bridge.handleMessage(INITIALIZED);
  // Two frames are already in this second (initialize + initialized).
  for (let i = 0; i < 198; i++) {
    h.bridge.handleMessage(JSON.stringify({ jsonrpc: "2.0", id: 1000 + i, method: "ping" }));
  }
  expect(h.destroyed.length).toBe(0);
  h.bridge.handleMessage(JSON.stringify({ jsonrpc: "2.0", id: 2000, method: "ping" }));
  expect(h.destroyed.length).toBe(1);
  expect(h.destroyed[0].kind).toBe("protocol");
  expect(h.destroyed[0].message).toContain("200");
});

// ---- host requests (change feat-mcp-apps-events, F2) ----------------------
//
// A View request that reaches a host proxy is answered asynchronously: the
// response is a JSON-RPC result on success and the host's own code on failure.
// The capability the host advertised at initialize is the gate; an undeclared
// method is refused with -32601 before any callback runs.

const ALL_CAPS = {
  serverTools: true,
  serverResources: true,
  message: true,
  openLinks: true,
  updateModelContext: true,
};

const allCapabilities = () => mcpAppHostCapabilities(ALL_CAPS, APP);
const noCapabilities = () =>
  mcpAppHostCapabilities(
    {
      serverTools: false,
      serverResources: false,
      message: false,
      openLinks: false,
      updateModelContext: false,
    },
    APP,
  );

const ready = (h: Harness) => {
  h.bridge.start();
  h.bridge.handleMessage(initialize());
  h.bridge.handleMessage(INITIALIZED);
};

const request = (id: number, method: string, params?: Record<string, unknown>) =>
  JSON.stringify({ jsonrpc: "2.0", id, method, ...(params !== undefined ? { params } : {}) });

const responseFor = (h: Harness, id: number) =>
  h.posted.find((frame) => frame.id === id) as
    | { result?: unknown; error?: { code: number; message: string } }
    | undefined;

/** Let every resolved promise chain run (the bridge does not await callbacks). */
const flush = async () => {
  for (let i = 0; i < 8; i++) await Promise.resolve();
};

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

test("tools/call：声明 serverTools 时回调被调用，结果原样回填", async () => {
  const calls: { name: string; args: unknown }[] = [];
  const h = harness({
    hostCapabilities: allCapabilities,
    callTool: async (name, args) => {
      calls.push({ name, args });
      return { content: [{ type: "text", text: "24°C" }], structuredContent: { tempC: 24 }, isError: false };
    },
  });
  ready(h);
  h.bridge.handleMessage(request(41, "tools/call", { name: "refresh", arguments: { city: "上海" } }));
  await flush();

  expect(calls).toEqual([{ name: "refresh", args: { city: "上海" } }]);
  expect(responseFor(h, 41)?.result).toEqual({
    content: [{ type: "text", text: "24°C" }],
    structuredContent: { tempC: 24 },
    isError: false,
  });
  expect(h.bridge.snapshot().phase).toBe("ready");
});

test("tools/call：回调 reject 时透传 host 的 code；无 code 用 -32603", async () => {
  const mapped = harness({
    hostCapabilities: allCapabilities,
    callTool: async () => {
      throw { code: -32602, message: "bad arguments" };
    },
  });
  ready(mapped);
  mapped.bridge.handleMessage(request(42, "tools/call", { name: "refresh" }));
  await flush();
  expect(responseFor(mapped, 42)?.error).toEqual({ code: -32602, message: "bad arguments" });

  const generic = harness({
    hostCapabilities: allCapabilities,
    callTool: async () => {
      throw new Error("upstream is down");
    },
  });
  ready(generic);
  generic.bridge.handleMessage(request(43, "tools/call", { name: "refresh" }));
  await flush();
  expect(responseFor(generic, 43)?.error?.code).toBe(-32603);
  expect(responseFor(generic, 43)?.error?.message).toBe("upstream is down");
  // A failed host request is not a protocol violation: the View stays alive.
  expect(generic.destroyed.length).toBe(0);
});

test("resources/read：成功回 {contents}；失败同 code", async () => {
  const h = harness({
    hostCapabilities: allCapabilities,
    readResource: async (uri) => ({ contents: [{ uri, text: "<html></html>" }] }),
  });
  ready(h);
  h.bridge.handleMessage(request(44, "resources/read", { uri: "ui://weather/dashboard" }));
  await flush();
  expect(responseFor(h, 44)?.result).toEqual({
    contents: [{ uri: "ui://weather/dashboard", text: "<html></html>" }],
  });

  const rejected = harness({
    hostCapabilities: allCapabilities,
    readResource: async () => {
      throw { code: -32602, message: "resource URI rejected" };
    },
  });
  ready(rejected);
  rejected.bridge.handleMessage(request(45, "resources/read", { uri: "file:///etc/passwd" }));
  await flush();
  expect(responseFor(rejected, 45)?.error).toEqual({ code: -32602, message: "resource URI rejected" });
});

test("ui/message：固定 user/queue，回 {}；同一帧 id 的重放复用 requestId", async () => {
  const seen: { content: unknown; requestId: string }[] = [];
  const h = harness({
    hostCapabilities: allCapabilities,
    sendMessage: async (content, requestId) => {
      seen.push({ content, requestId });
      return {};
    },
  });
  ready(h);
  const params = { role: "user", content: [{ type: "text", text: "看看天气" }], delivery: "queue" };
  h.bridge.handleMessage(request(46, "ui/message", params));
  await flush();
  h.bridge.handleMessage(request(46, "ui/message", params));
  await flush();

  expect(seen.length).toBe(2);
  expect(seen[0].requestId).toBe(seen[1].requestId);
  expect(seen[0].requestId).toContain("46");
  expect(responseFor(h, 46)?.result).toEqual({});
});

test("ui/message：非 user 角色回 -32602，不调用回调", async () => {
  let calls = 0;
  const h = harness({
    hostCapabilities: allCapabilities,
    sendMessage: async () => {
      calls++;
      return {};
    },
  });
  ready(h);
  h.bridge.handleMessage(request(47, "ui/message", { role: "assistant", content: [{ type: "text", text: "x" }] }));
  await flush();
  expect(calls).toBe(0);
  expect(responseFor(h, 47)?.error?.code).toBe(-32602);
});

test("ui/open-link：确认后的 URL 交给回调，回 {}；拒绝由回调给 isError", async () => {
  const urls: string[] = [];
  const h = harness({
    hostCapabilities: allCapabilities,
    openLink: async (url) => {
      urls.push(url);
      return {};
    },
  });
  ready(h);
  h.bridge.handleMessage(request(48, "ui/open-link", { url: "https://example.com/a" }));
  await flush();
  expect(urls).toEqual(["https://example.com/a"]);
  expect(responseFor(h, 48)?.result).toEqual({});

  const denied = harness({
    hostCapabilities: allCapabilities,
    openLink: async () => ({ isError: true }),
  });
  ready(denied);
  denied.bridge.handleMessage(request(49, "ui/open-link", { url: "https://example.com/b" }));
  await flush();
  expect(responseFor(denied, 49)?.result).toEqual({ isError: true });
});

test("ui/update-model-context：content/structuredContent 交给回调，回 {}", async () => {
  const seen: { content: unknown; structured: unknown; requestId: string }[] = [];
  const h = harness({
    hostCapabilities: allCapabilities,
    updateModelContext: async (content, structuredContent, requestId) => {
      seen.push({ content, structured: structuredContent, requestId });
      return {};
    },
  });
  ready(h);
  h.bridge.handleMessage(
    request(50, "ui/update-model-context", {
      content: [{ type: "text", text: "当前城市：上海" }],
      structuredContent: { city: "上海" },
    }),
  );
  await flush();
  expect(seen.length).toBe(1);
  expect(seen[0].content).toEqual([{ type: "text", text: "当前城市：上海" }]);
  expect(seen[0].structured).toEqual({ city: "上海" });
  expect(seen[0].requestId).toContain("50");
  expect(responseFor(h, 50)?.result).toEqual({});

  const empty = harness({ hostCapabilities: allCapabilities, updateModelContext: async () => ({}) });
  ready(empty);
  empty.bridge.handleMessage(request(51, "ui/update-model-context", {}));
  await flush();
  expect(responseFor(empty, 51)?.error?.code).toBe(-32602);
});

test("错误路径：message/open-link/context 的拒绝同样回 JSON-RPC error", async () => {
  const h = harness({
    hostCapabilities: allCapabilities,
    sendMessage: async () => {
      throw { code: -32602, message: "content must contain at least one text block" };
    },
    openLink: async () => {
      throw new Error("link denied");
    },
    updateModelContext: async () => {
      throw { code: -32602, message: "sessionId is required" };
    },
  });
  ready(h);
  h.bridge.handleMessage(request(81, "ui/message", { role: "user", content: [{ type: "image" }] }));
  h.bridge.handleMessage(request(82, "ui/open-link", { url: "https://example.com" }));
  h.bridge.handleMessage(request(83, "ui/update-model-context", { content: [{ type: "text", text: "x" }] }));
  await flush();

  expect(responseFor(h, 81)?.error).toEqual({
    code: -32602,
    message: "content must contain at least one text block",
  });
  expect(responseFor(h, 82)?.error?.code).toBe(-32603);
  expect(responseFor(h, 82)?.error?.message).toBe("link denied");
  expect(responseFor(h, 83)?.error?.code).toBe(-32602);
  expect(h.destroyed.length).toBe(0);
});

test("未声明能力：五个 host 方法都回 -32601，回调不执行，实例不销毁", async () => {
  let calls = 0;
  const h = harness({
    hostCapabilities: noCapabilities,
    callTool: async () => {
      calls++;
      return {};
    },
    readResource: async () => {
      calls++;
      return {};
    },
    sendMessage: async () => {
      calls++;
      return {};
    },
    openLink: async () => {
      calls++;
      return {};
    },
    updateModelContext: async () => {
      calls++;
      return {};
    },
  });
  ready(h);
  h.bridge.handleMessage(request(61, "tools/call", { name: "x" }));
  h.bridge.handleMessage(request(62, "resources/read", { uri: "ui://x/y" }));
  h.bridge.handleMessage(request(63, "ui/message", { role: "user", content: [{ type: "text", text: "x" }] }));
  h.bridge.handleMessage(request(64, "ui/open-link", { url: "https://example.com" }));
  h.bridge.handleMessage(request(65, "ui/update-model-context", { content: [{ type: "text", text: "x" }] }));
  await flush();

  expect(calls).toBe(0);
  for (const id of [61, 62, 63, 64, 65]) {
    expect(responseFor(h, id)?.error?.code).toBe(-32601);
  }
  expect(h.destroyed.length).toBe(0);
  expect(h.bridge.snapshot().phase).toBe("ready");
});

test("能力快照在 initialize 冻结：之后能力变真也不会接受未声明的请求", async () => {
  let caps: Record<string, unknown> = { logging: {} };
  let calls = 0;
  const h = harness({
    hostCapabilities: () => caps,
    callTool: async () => {
      calls++;
      return {};
    },
  });
  ready(h);
  caps = { logging: {}, serverTools: {} };
  h.bridge.handleMessage(request(66, "tools/call", { name: "refresh" }));
  await flush();
  expect(calls).toBe(0);
  expect(responseFor(h, 66)?.error?.code).toBe(-32601);
});

test("异步不阻塞：一个 pending host 请求期间仍处理 ping 与 size-changed", async () => {
  const pending = deferred<unknown>();
  const h = harness({
    hostCapabilities: allCapabilities,
    callTool: () => pending.promise,
  });
  ready(h);
  h.bridge.handleMessage(request(71, "tools/call", { name: "slow" }));
  await flush();
  expect(responseFor(h, 71)).toBe(undefined);

  // A second request and a notification are handled while the first is open.
  h.bridge.handleMessage(request(72, "ping"));
  h.bridge.handleMessage(
    JSON.stringify({ jsonrpc: "2.0", method: "ui/notifications/size-changed", params: { width: 600, height: 300 } }),
  );
  await flush();
  expect(responseFor(h, 72)?.result).toEqual({});
  expect(h.sizes[h.sizes.length - 1]).toEqual({ width: 600, height: 300 });
  expect(responseFor(h, 71)).toBe(undefined);

  pending.resolve({ content: [{ type: "text", text: "done" }] });
  await flush();
  expect(responseFor(h, 71)?.result).toEqual({ content: [{ type: "text", text: "done" }] });
});

test("能力快照：initialize 的 hostCapabilities 全开声明五项，全关只留 logging/sandbox", () => {
  const all = harness({ hostCapabilities: allCapabilities });
  all.bridge.start();
  all.bridge.handleMessage(initialize());
  const opened = all.posted[0] as { result: { hostCapabilities: Record<string, unknown> } };
  expect(Object.keys(opened.result.hostCapabilities).sort()).toEqual(
    ["logging", "message", "openLinks", "sandbox", "serverResources", "serverTools", "updateModelContext"].sort(),
  );

  const none = harness({ hostCapabilities: noCapabilities });
  none.bridge.start();
  none.bridge.handleMessage(initialize());
  const closed = none.posted[0] as { result: { hostCapabilities: Record<string, unknown> } };
  expect(closed.result.hostCapabilities).toEqual({
    logging: {},
    sandbox: { csp: APP.cspDomains },
  });
});
