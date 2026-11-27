// Same-origin attachment links (change keel-plugin-attachment).
//
// The projection's URLs are relative (`/v1/files/{id}`); `openUrl` only accepts
// absolute URLs. The page’s own origin is the only source used here — never a
// tenant header, a query parameter or a second base URL.
import { host } from "nsh";

const ABSOLUTE = /^https?:\/\//i;

/** True when `value` resolves to the page's own origin. */
export function isSameOrigin(value: string, origin: string): boolean {
  try {
    const base = new URL(origin);
    return new URL(value, base).origin === base.origin;
  } catch {
    return false;
  }
}

async function pageOrigin(): Promise<string> {
  const location = (globalThis as { location?: { origin?: string } }).location;
  const origin = typeof location?.origin === "string" ? location.origin.trim() : "";
  if (origin && /^https?:\/\//i.test(origin)) return origin;
  if (host.available()) {
    try {
      const info = await host.call<{ origin?: string }>("keel.info", {}, { timeout: 1500 });
      const hostOrigin = typeof info?.origin === "string" ? info.origin.trim() : "";
      if (/^https?:\/\//i.test(hostOrigin)) return hostOrigin;
    } catch {
      /* fall through to the explicit refusal */
    }
  }
  throw new Error("当前宿主没有可用的页面来源，请在有图形界面的浏览器中打开");
}

/** Make a same-origin projection URL absolute for `openUrl`. An absolute URL
 * must name the page's own origin: a projection never turns into a
 * cross-origin navigation. */
export async function absoluteAttachmentUrl(path: string): Promise<string> {
  const value = String(path ?? "").trim();
  if (!value) throw new Error("附件没有下载地址");
  const origin = await pageOrigin();
  let absolute: string;
  try {
    absolute = ABSOLUTE.test(value) ? new URL(value).toString() : new URL(value, origin).toString();
  } catch {
    throw new Error("附件的地址无效");
  }
  if (!isSameOrigin(absolute, origin)) throw new Error("只允许打开同源附件地址");
  return absolute;
}
