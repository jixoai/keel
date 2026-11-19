// The MCP Apps postMessage bridge (change feat-mcp-apps-events, P2.6).
//
// Pure protocol: no DOM, no NSH, no globals. `view.tsx` owns the iframe and
// hands every inbound string here; every outbound frame goes back through the
// `post` callback. Tests drive the same object with a fake clock.
//
// The handshake is the spec's (apps.mdx §Lifecycle, Stable 2026-01-26):
//
//   View → ui/initialize (request)
//   Host → McpUiInitializeResult (protocolVersion/hostInfo/hostCapabilities/
//          hostContext)
//   View → ui/notifications/initialized
//   Host → ui/notifications/tool-input (exactly once)
//   Host → ui/notifications/tool-result (once, when the result is available)
//
// Before `initialized` the host sends no `ui/notifications/*` at all. A View
// that violates the frame shape, exceeds 64 KiB, exceeds 200 frames/s, sends an
// unknown method, or misses the 5 s handshake is destroyed: the iframe goes
// away and the tool row falls back to text. One app's failure never touches
// another tool or the session.
import type { McpAppView } from "../../data/contract";

/** The extension's protocol version the host speaks. */
export const MCP_APP_PROTOCOL_VERSION = "2026-01-26";
/** The host name/version `hostInfo` carries. */
export const MCP_APP_HOST_NAME = "Keel";
export const MCP_APP_HOST_VERSION = "0.1.0";
export const HANDSHAKE_TIMEOUT_MS = 5000;
export const TEARDOWN_TIMEOUT_MS = 1000;
export const MAX_FRAME_BYTES = 64 * 1024;
export const MAX_FRAMES_PER_SECOND = 200;
/** Inline containers are flexible up to this height (design §3.4). */
export const INLINE_MAX_HEIGHT = 480;
export const INLINE_MIN_HEIGHT = 80;

export type DisplayMode = "inline" | "fullscreen";
export type BridgePhase = "idle" | "handshaking" | "ready" | "destroyed";

/** Why a View instance was torn down. The UI shows `message`, the audit/log
 * caller records `kind`. */
export interface BridgeFailure {
  kind: "timeout" | "protocol" | "transport" | "unmounted";
  message: string;
}

/** The standard `CallToolResult` the View receives (content,
 * structuredContent and isError; no server response headers). */
export interface AppToolResult {
  content?: unknown;
  structuredContent?: unknown;
  isError?: boolean;
}

export interface AppBridgeOptions {
  /** Sends one JSON frame to the View (the iframe's `postMessage`). */
  post: (frame: Record<string, unknown>) => void;
  /** The tool call's arguments, read when `tool-input` is sent. */
  toolInput: () => unknown;
  /** The complete result, read when it is available (null until then). */
  toolResult: () => AppToolResult | null;
  /** `McpUiInitializeResult.hostCapabilities` for this instance. */
  hostCapabilities: () => Record<string, unknown>;
  /** `McpUiInitializeResult.hostContext` for this instance. */
  hostContext: () => Record<string, unknown>;
  /** The app this View renders (the host does not invent context for it). */
  app: () => McpAppView;
  /** The deployment's configured second origin, when any. A concrete inbound
   * `origin` must be this origin (an opaque View is `"null"`). */
  sandboxOrigin?: () => string | undefined;
  /** Switch the host's display mode; returns the mode actually in effect. */
  setDisplayMode?: (mode: DisplayMode) => DisplayMode;
  onSize?: (size: { width: number; height: number }) => void;
  onDisplayMode?: (mode: DisplayMode) => void;
  onPhase?: (phase: BridgePhase) => void;
  onDiagnostic?: (failure: BridgeFailure) => void;
  /** The instance is gone: the UI unmounts the iframe and shows text. */
  onDestroyed?: (failure: BridgeFailure) => void;
  /** A host-initiated teardown was sent (and its bounded wait ended). */
  onTeardown?: () => void;
  /** The appearance/context changed after initialize (SHOULD, partial). */
  now?: () => number;
  schedule?: (fn: () => void, ms: number) => unknown;
  cancel?: (handle: unknown) => void;
}

export interface BridgeSnapshot {
  phase: BridgePhase;
  initialized: boolean;
  inputSent: boolean;
  resultSent: boolean;
  cancelled: boolean;
  displayMode: DisplayMode;
  size: { width?: number; height?: number };
  failure: BridgeFailure | null;
  appInfo: { name?: string; version?: string } | null;
  availableDisplayModes: DisplayMode[];
}

export interface AppBridge {
  /** The iframe loaded: arm the handshake timeout. Idempotent. */
  start(): void;
  /** One inbound string from the View. `origin` is checked when the transport
   * can report it (the NSH web host does not expose it; a browser host does). */
  handleMessage(data: string, origin?: string): void;
  /** The result became available (or changed). Sends `tool-result` once. */
  updateToolResult(result: AppToolResult | null): void;
  /** The run was cancelled before a result: notify and stop waiting. */
  cancel(reason?: string): void;
  /** Unmount: send `ui/resource-teardown` and complete within 1 s. */
  teardown(): void;
  /** Tear the instance down now (idempotent). */
  destroy(failure: BridgeFailure): void;
  /** Send a partial `host-context-changed` (theme/display-mode changes). */
  hostContextChanged(context: Record<string, unknown>): void;
  snapshot(): BridgeSnapshot;
}

type JsonObject = Record<string, unknown>;

const asRecord = (value: unknown): JsonObject =>
  value && typeof value === "object" && !Array.isArray(value) ? (value as JsonObject) : {};

const asString = (value: unknown): string | undefined =>
  typeof value === "string" && value.trim() !== "" ? value : undefined;

/** UTF-8 byte length of one frame, without allocating for the common ASCII
 * case (JSON frames are overwhelmingly ASCII). */
export function byteLength(value: string): number {
  let bytes = 0;
  for (let i = 0; i < value.length; i++) {
    const code = value.charCodeAt(i);
    if (code < 0x80) bytes += 1;
    else if (code < 0x800) bytes += 2;
    else if (code >= 0xd800 && code <= 0xdbff) {
      // Surrogate pair: one 4-byte scalar (the low surrogate is skipped).
      bytes += 4;
      i++;
    } else bytes += 3;
  }
  return bytes;
}

interface InboundFrame {
  jsonrpc: "2.0";
  method?: string;
  id?: string | number;
  params?: JsonObject;
  result?: unknown;
  error?: JsonObject;
}

const isId = (value: unknown): value is string | number =>
  typeof value === "string" || (typeof value === "number" && Number.isFinite(value));

/** Parse and shape-check one inbound frame. The result is either a frame or
 * the reason it is not renderable/answerable. */
export function parseFrame(data: string): { frame: InboundFrame } | { reason: string } {
  let raw: unknown;
  try {
    raw = JSON.parse(data);
  } catch {
    return { reason: "帧不是 JSON" };
  }
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return { reason: "帧不是对象" };
  const frame = raw as JsonObject;
  if (frame.jsonrpc !== "2.0") return { reason: "缺少 jsonrpc 2.0" };
  const method = frame.method;
  const id = frame.id;
  if (method !== undefined) {
    if (typeof method !== "string" || method.trim() === "") return { reason: "method 不是字符串" };
    if (id !== undefined && !isId(id)) return { reason: "id 不是字符串或数字" };
    if (frame.params !== undefined && (typeof frame.params !== "object" || Array.isArray(frame.params))) {
      return { reason: "params 不是对象" };
    }
    return {
      frame: {
        jsonrpc: "2.0",
        method,
        ...(id !== undefined ? { id } : {}),
        ...(frame.params !== undefined ? { params: asRecord(frame.params) } : {}),
      },
    };
  }
  // A response to a host request (`ui/resource-teardown`).
  if (!isId(id)) return { reason: "响应缺少 id" };
  const hasResult = "result" in frame;
  const hasError = "error" in frame;
  if (hasResult === hasError) return { reason: "响应必须恰有 result 或 error" };
  if (hasError && typeof frame.error !== "object") return { reason: "error 不是对象" };
  return {
    frame: {
      jsonrpc: "2.0",
      id,
      ...(hasResult ? { result: frame.result } : {}),
      ...(hasError ? { error: asRecord(frame.error) } : {}),
    },
  };
}

export function createAppBridge(options: AppBridgeOptions): AppBridge {
  const now = options.now ?? (() => Date.now());
  const schedule =
    options.schedule ??
    ((fn: () => void, ms: number) => {
      const handle = setTimeout(fn, ms);
      return handle;
    });
  const cancelTimer =
    options.cancel ??
    ((handle: unknown) => {
      clearTimeout(handle as ReturnType<typeof setTimeout>);
    });

  let phase: BridgePhase = "idle";
  let initializeDone = false;
  let initialized = false;
  let inputSent = false;
  let resultSent = false;
  let cancelled = false;
  let cancelReason = "cancelled";
  let failure: BridgeFailure | null = null;
  let displayMode: DisplayMode = "inline";
  let availableDisplayModes: DisplayMode[] = ["inline"];
  let appInfo: { name?: string; version?: string } | null = null;
  let size: { width?: number; height?: number } = {};
  let handshakeTimer: unknown;
  let teardownTimer: unknown;
  let resultOnHandshake: AppToolResult | null = null;
  let teardownId = 0;
  let teardownSent = false;

  /** At most 200 inbound frames in any 1 s sliding window. The window belongs
   * to one View instance: a rebuild (display-mode change, navigation reload)
   * starts a fresh instance and therefore a fresh window; a host-level budget
   * across instances is P3.8's concern. */
  const stamps: number[] = [];

  const emitPhase = () => options.onPhase?.(phase);

  const send = (frame: JsonObject) => {
    if (phase === "destroyed") return;
    options.post(frame);
  };

  const respond = (id: string | number, result: unknown) =>
    send({ jsonrpc: "2.0", id, result });

  const respondError = (id: string | number, code: number, message: string) =>
    send({ jsonrpc: "2.0", id, error: { code, message } });

  const notify = (method: string, params?: JsonObject) => {
    // The spec's hard rule: no `ui/notifications/*` before `initialized`.
    if (!initialized) return;
    send({ jsonrpc: "2.0", method, ...(params !== undefined ? { params } : {}) });
  };

  const finish = (next: BridgeFailure) => {
    if (phase === "destroyed") return;
    failure = next;
    phase = "destroyed";
    if (handshakeTimer !== undefined) cancelTimer(handshakeTimer);
    if (teardownTimer !== undefined) cancelTimer(teardownTimer);
    handshakeTimer = undefined;
    teardownTimer = undefined;
    emitPhase();
    options.onDiagnostic?.(next);
    options.onDestroyed?.(next);
  };

  const protocol = (message: string) => finish({ kind: "protocol", message });

  /** Arm the 5 s handshake deadline once; `initialized` disarms it. */
  const armHandshake = () => {
    if (handshakeTimer !== undefined || initialized || phase === "destroyed") return;
    handshakeTimer = schedule(() => {
      if (initialized || phase === "destroyed") return;
      handshakeTimer = undefined;
      finish({ kind: "timeout", message: `交互界面在 ${HANDSHAKE_TIMEOUT_MS / 1000} 秒内未完成握手` });
    }, HANDSHAKE_TIMEOUT_MS);
  };

  const normalizedSize = () => {
    const width =
      typeof size.width === "number" && Number.isFinite(size.width)
        ? Math.max(1, Math.round(size.width))
        : 1;
    const height =
      typeof size.height === "number" && Number.isFinite(size.height)
        ? Math.max(INLINE_MIN_HEIGHT, Math.min(INLINE_MAX_HEIGHT, Math.round(size.height)))
        : INLINE_MIN_HEIGHT;
    return { width, height };
  };

  const sendInput = () => {
    if (inputSent) return;
    inputSent = true;
    const args = options.toolInput();
    const argumentsValue =
      args && typeof args === "object" && !Array.isArray(args) ? args : args === undefined ? {} : { value: args };
    notify("ui/notifications/tool-input", { arguments: argumentsValue });
  };

  const sendResult = (result: AppToolResult | null) => {
    if (resultSent || cancelled || !result) return;
    resultSent = true;
    const params: JsonObject = { content: Array.isArray(result.content) ? result.content : [], isError: result.isError === true };
    if (result.structuredContent !== undefined) params.structuredContent = result.structuredContent;
    notify("ui/notifications/tool-result", params);
  };

  const handleInitialize = (id: string | number, params: JsonObject) => {
    if (initializeDone) {
      respondError(id, -32602, "initialize was already completed");
      protocol("重复的 ui/initialize");
      return;
    }
    const info = asRecord(params.appInfo);
    const name = asString(info.name);
    if (!name) {
      respondError(id, -32602, "appInfo.name is required");
      protocol("ui/initialize 缺少 appInfo.name");
      return;
    }
    appInfo = { name, version: asString(info.version) };
    const declared = asRecord(params.appCapabilities).availableDisplayModes;
    if (Array.isArray(declared)) {
      const modes = declared.filter((mode): mode is DisplayMode => mode === "inline" || mode === "fullscreen");
      // An app that declares nothing is inline-only; `pip` is not a host mode.
      availableDisplayModes = modes.length ? modes : ["inline"];
    }
    if (handshakeTimer === undefined) armHandshake();
    respond(id, {
      protocolVersion: MCP_APP_PROTOCOL_VERSION,
      hostInfo: { name: MCP_APP_HOST_NAME, version: MCP_APP_HOST_VERSION },
      hostCapabilities: options.hostCapabilities(),
      hostContext: options.hostContext(),
    });
    initializeDone = true;
    phase = "handshaking";
    emitPhase();
  };

  const handleInitialized = () => {
    if (initialized) return;
    if (!initializeDone) {
      protocol("initialized 早于 ui/initialize");
      return;
    }
    initialized = true;
    if (handshakeTimer !== undefined) {
      cancelTimer(handshakeTimer);
      handshakeTimer = undefined;
    }
    phase = "ready";
    emitPhase();
    sendInput();
    if (cancelled) {
      notify("ui/notifications/tool-cancelled", { reason: cancelReason });
      return;
    }
    // The result may already be in the ledger when the View finishes its
    // handshake, so the live source is consulted too (not only a push that
    // happened while `initialized` was still false).
    sendResult(resultOnHandshake ?? options.toolResult());
    resultOnHandshake = null;
  };

  const handleDisplayMode = (id: string | number, params: JsonObject) => {
    const requested = params.mode;
    if (requested !== "inline" && requested !== "fullscreen" && requested !== "pip") {
      respondError(id, -32602, "mode must be inline, fullscreen or pip");
      return;
    }
    let actual: DisplayMode = displayMode;
    if (requested === "inline" || requested === "fullscreen") {
      if (availableDisplayModes.includes(requested)) {
        actual = options.setDisplayMode ? options.setDisplayMode(requested) : requested;
      }
    }
    displayMode = actual;
    respond(id, { mode: actual });
    options.onDisplayMode?.(actual);
    notify("ui/notifications/host-context-changed", { displayMode: actual });
  };

  const handleRequest = (frame: InboundFrame, method: string, id: string | number) => {
    const params = frame.params ?? {};
    switch (method) {
      case "ui/initialize":
        handleInitialize(id, params);
        return;
      case "ui/request-display-mode":
        handleDisplayMode(id, params);
        return;
      case "ping":
        respond(id, {});
        return;
      // Wired in P3. Until then the host does not advertise these in
      // `hostCapabilities`, so a compliant View never sends them; a View that
      // does gets an explicit refusal, never a fake success.
      case "tools/call":
      case "resources/read":
      case "ui/message":
      case "ui/open-link":
      case "ui/update-model-context":
      case "ui/download-file":
        respondError(id, -32601, `${method} is not supported by this host`);
        return;
      default:
        respondError(id, -32601, `unknown method: ${method}`);
        protocol(`未知方法 ${method}`);
    }
  };

  const handleNotification = (method: string, params: JsonObject) => {
    switch (method) {
      case "ui/notifications/initialized":
        handleInitialized();
        return;
      case "ui/notifications/size-changed": {
        if (displayMode !== "inline") return;
        const width = params.width;
        const height = params.height;
        if (typeof width !== "number" || typeof height !== "number" || !Number.isFinite(width) || !Number.isFinite(height)) {
          protocol("size-changed 的宽高不是有限数字");
          return;
        }
        size = { width, height };
        options.onSize?.(normalizedSize());
        return;
      }
      case "notifications/message": {
        // One-way to the host log; never a chat bubble, never the ledger. The
        // level is whitelisted and the payload is not copied into the UI.
        const level = params.level;
        if (level === "debug" || level === "info" || level === "warning" || level === "error") {
          const message = asString(asRecord(params.data).message) ?? asString(params.message) ?? "";
          const line = `[mcp-app ${options.app().appId}] ${level}: ${message.slice(0, 500)}`;
          if (level === "error" || level === "warning") console.warn(line);
          else console.info(line);
          return;
        }
        protocol("notifications/message 的 level 不在白名单");
        return;
      }
      default:
        // Includes `ui/notifications/tool-*`, which only flow host → View.
        protocol(`未知通知 ${method}`);
    }
  };

  return {
    start() {
      if (phase === "destroyed") return;
      if (phase === "idle") {
        phase = "handshaking";
        emitPhase();
      }
      armHandshake();
    },
    handleMessage(data: string, origin?: string) {
      if (phase === "destroyed") return;
      // The web host gives the View an opaque origin (`"null"`). A concrete
      // origin must be the deployment's sandbox origin; anything else is
      // another page and never reaches the protocol.
      if (origin !== undefined && origin !== "null" && origin !== "about:srcdoc") {
        const sandbox = options.sandboxOrigin?.();
        if (!sandbox || origin !== sandbox) {
          protocol(`消息来自非沙箱来源 ${origin}`);
          return;
        }
      }
      if (byteLength(data) > MAX_FRAME_BYTES) {
        protocol(`单帧超过 ${MAX_FRAME_BYTES / 1024} KiB`);
        return;
      }
      const at = now();
      stamps.push(at);
      while (stamps.length && at - stamps[0] > 1000) stamps.shift();
      if (stamps.length > MAX_FRAMES_PER_SECOND) {
        protocol(`超过每 app ${MAX_FRAMES_PER_SECOND} 帧/秒`);
        return;
      }
      const parsed = parseFrame(data);
      if ("reason" in parsed) {
        protocol(parsed.reason);
        return;
      }
      const frame = parsed.frame;
      if (frame.method !== undefined) {
        if (frame.id !== undefined) handleRequest(frame, frame.method, frame.id);
        else handleNotification(frame.method, frame.params ?? {});
        return;
      }
      // A response: only the host's own teardown request has an id in the
      // host segment. Everything else is ignored without touching the ledger.
      if (typeof frame.id === "string" && frame.id.startsWith("host-")) {
        if (teardownTimer !== undefined) {
          cancelTimer(teardownTimer);
          teardownTimer = undefined;
        }
        options.onTeardown?.();
      }
    },
    updateToolResult(result) {
      if (resultSent || cancelled) return;
      if (!initialized) {
        resultOnHandshake = result;
        return;
      }
      sendResult(result);
    },
    cancel(reason = "cancelled") {
      if (cancelled) return;
      cancelled = true;
      cancelReason = reason;
      if (initialized && !resultSent) notify("ui/notifications/tool-cancelled", { reason });
    },
    teardown() {
      if (phase === "destroyed" || teardownSent) return;
      teardownSent = true;
      if (!initialized) {
        options.onTeardown?.();
        return;
      }
      const id = `host-${++teardownId}`;
      // The spec says the host SHOULD wait for the answer; it MUST be bounded
      // (1 s). The caller unmounts on `onTeardown` either way.
      teardownTimer = schedule(() => {
        teardownTimer = undefined;
        options.onTeardown?.();
      }, TEARDOWN_TIMEOUT_MS);
      send({ jsonrpc: "2.0", id, method: "ui/resource-teardown", params: { reason: "unmounted" } });
    },
    destroy(next) {
      finish(next);
    },
    hostContextChanged(context) {
      notify("ui/notifications/host-context-changed", context);
    },
    snapshot() {
      return {
        phase,
        initialized,
        inputSent,
        resultSent,
        cancelled,
        displayMode,
        size,
        failure,
        appInfo,
        availableDisplayModes,
      };
    },
  };
}
