// Same-origin attachment links (change keel-plugin-attachment).
//
// The projection's URLs are relative (`/v1/files/{id}`); `openUrl` only accepts
// absolute URLs. The page’s own origin is the only source used here — never a
// tenant header, a query parameter or a second base URL.
import { host } from "nsh";

const ABSOLUTE = /^https?:\/\//i;

/** Make a same-origin projection URL absolute for `openUrl`. Rejects a link
 * the host cannot resolve (a terminal with no page origin and no host info). */
export async function absoluteAttachmentUrl(path: string): Promise<string> {
  const value = String(path ?? "").trim();
  if (!value) throw new Error("附件没有下载地址");
  if (ABSOLUTE.test(value)) return value;
  const location = (globalThis as { location?: { origin?: string } }).location;
  const origin = typeof location?.origin === "string" ? location.origin.trim() : "";
  if (origin && /^https?:\/\//i.test(origin)) {
    return new URL(value, origin).toString();
  }
  if (host.available()) {
    try {
      const info = await host.call<{ origin?: string }>("keel.info", {}, { timeout: 1500 });
      const hostOrigin = typeof info?.origin === "string" ? info.origin.trim() : "";
      if (/^https?:\/\//i.test(hostOrigin)) return new URL(value, hostOrigin).toString();
    } catch {
      /* fall through to the explicit refusal */
    }
  }
  throw new Error("当前宿主没有可用的页面来源，请在有图形界面的浏览器中打开");
}
