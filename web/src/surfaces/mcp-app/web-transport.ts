// The web target's real postMessage path (change feat-mcp-apps-events, P2.6).
//
// Why this module exists: on NSH 0.4.0 nightly the web runtime only builds its
// `MessageChannel` / injects `window.nsh` into a WebView's **srcdoc** form
// (`op_web`: `_token = html != null && js ? … : null`). A URL-form WebView
// gets only host→page `postMessage` (`frame.contentWindow.postMessage`) and
// load events; a page's `parent.postMessage` never reaches the app. The app
// document must stay at its own hash-named URL (the response carries the CSP
// and `X-MCP-App-Hash` the spec requires), so the transport is:
//
//   view → host: the sandboxed app document calls `parent.postMessage`; the
//                app's JS realm is same-origin with the page, so it listens on
//                `window.top` (the visible page) and maps `event.source` back
//                to the one iframe whose `src` carries its `instance` nonce.
//   host → view: `WebViewRef.postMessage` (the runtime's URL-form path).
//
// The frame is opaque (`event.origin === "null"`): it cannot read the host's
// DOM, storage or cookies. Only a `"null"` origin is accepted; anything else
// is another document and is ignored (and the caller's `onLoad`/navigation
// defenses discard the instance).
import { host, platform } from "nsh";
import { INSTANCE_PATTERN } from "../../data/mcp-apps";

interface MessageEventLike {
  origin?: string;
  data?: unknown;
  source?: unknown;
}

interface DomFrame {
  src?: string;
  contentWindow?: unknown;
}

interface DomDocumentLike {
  querySelectorAll: (selector: string) => ArrayLike<DomFrame>;
}

interface DomWindowLike {
  addEventListener?: (name: string, listener: (event: MessageEventLike) => void) => void;
  removeEventListener?: (name: string, listener: (event: MessageEventLike) => void) => void;
  document?: DomDocumentLike;
  /** Same-origin page only; the last-resort origin when the host call cannot
   * answer (the app's own realm has no usable `location`). */
  location?: { origin?: string };
}

interface GlobalWithPage {
  top?: DomWindowLike;
  document?: DomDocumentLike;
}

type FrameHandler = (data: string, origin: string) => void;

const pageWindow = (): DomWindowLike | null => {
  try {
    const candidate = (globalThis as unknown as GlobalWithPage).top;
    if (candidate?.document) return candidate;
  } catch {
    /* a cross-origin embedding page: no same-origin page to listen on */
  }
  return null;
};

/** Whether this target can actually carry a View's messages. */
export function webTransportAvailable(): boolean {
  return platform.os === "web" && pageWindow() !== null;
}

let originRequest: Promise<string> | null = null;

/** `location` is not part of the app's sandbox API; the web host answers the
 * page's own origin through `keel.info` (the same call the route gate uses).
 * The result is validated as an http(s) origin and cached for the launch. */
export function webPageOrigin(): Promise<string> {
  if (originRequest) return originRequest;
  const request = (async () => {
    if (platform.os !== "web") throw new Error("页面来源只在 web 目标上可用");
    if (!host.available()) throw new Error("页面宿主不可用");
    let raw = "";
    try {
      // The official route: the web host owns the page URL. A host call that
      // does not answer in time is raced below; the same-origin page window is
      // the fallback (its `location` is readable from the app's realm).
      const info = await Promise.race([
        host.call<{ origin?: string }>("keel.info", {}, { timeout: 1500 }),
        new Promise<never>((_, reject) => setTimeout(() => reject(new Error("读取页面来源超时")), 1800)),
      ]);
      raw = typeof info?.origin === "string" ? info.origin.trim() : "";
    } catch {
      raw = "";
    }
    if (!raw) {
      try {
        raw = pageWindow()?.location?.origin?.trim() ?? "";
      } catch {
        raw = "";
      }
    }
    if (!raw) throw new Error("宿主没有返回页面来源");
    let parsed: URL;
    try {
      parsed = new URL(raw);
    } catch {
      throw new Error("页面来源不是合法地址");
    }
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
      throw new Error("页面来源只接受 http(s)");
    }
    if (parsed.username || parsed.password) throw new Error("页面来源不得携带凭据");
    return parsed.origin;
  })();
  originRequest = request;
  // Do not cache a failure: the retry button re-probes. The caller's own
  // rejection is untouched (this handler only resets the cache entry).
  void request.catch(() => {
    if (originRequest === request) originRequest = null;
  });
  return request;
}

const handlers = new Map<string, FrameHandler>();
let installed = false;

function instanceOf(src: string | undefined): string | null {
  if (!src) return null;
  let url: URL;
  try {
    url = new URL(src);
  } catch {
    return null;
  }
  const instance = url.searchParams.get("instance") ?? "";
  return INSTANCE_PATTERN.test(instance) ? instance : null;
}

function onPageMessage(event: MessageEventLike): void {
  // The View is the opaque-origin frame. A concrete origin is some other
  // document (or the frame navigated away); never route it.
  if (event.origin !== "null") return;
  if (typeof event.data !== "string") return;
  const page = pageWindow();
  if (!page?.document) return;
  const frames = page.document.querySelectorAll("iframe");
  for (let i = 0; i < frames.length; i++) {
    const frame = frames[i];
    if (frame.contentWindow !== event.source) continue;
    const instance = instanceOf(frame.src);
    if (!instance) return;
    handlers.get(instance)?.(event.data, "null");
    return;
  }
}

/** Route one instance's frames from the page window. Returns a cleanup. */
export function registerWebFrame(instance: string, handler: FrameHandler): () => void {
  if (!webTransportAvailable() || !INSTANCE_PATTERN.test(instance)) return () => {};
  if (!installed) {
    pageWindow()?.addEventListener?.("message", onPageMessage);
    installed = true;
  }
  handlers.set(instance, handler);
  return () => {
    handlers.delete(instance);
  };
}

/** Tests only. */
export function resetWebTransportForTest(): void {
  const page = pageWindow();
  if (installed && page?.removeEventListener) page.removeEventListener("message", onPageMessage);
  handlers.clear();
  installed = false;
  originRequest = null;
}
