// The bridge's contract, pinned without a DOM (change feat-mcp-apps-events,
// P2.6). A fake clock schedules the 5 s handshake and 1 s teardown deadlines;
// `posted` is the exact list of frames the host sent, so order and counts are
// assertions, not hopes.
import { expect, test } from "nsh/test";
import type { McpAppView } from "../../data/contract";
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

const APP: McpAppView = {
  appId: "mcpapp_0123456789abcdef",
  server: "weather",
  title: "天气面板",
  resourceUri: "ui://weather/dashboard",
  mimeType: "text/html;profile=mcp-app",
  visibility: ["model", "app"],
  tools: [],
  csp: "default-src 'none'",
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
