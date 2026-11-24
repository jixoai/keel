// The app's own links (`keel://`), parsed as untrusted input.
//
// A link may come from any app on the machine, so this module is deliberately
// pure: it accepts only the routes below and a UUID session id, and it decodes
// nothing into behavior on its own. The shell hands a valid result to the
// store; an invalid one is reported, never navigated.

export type KeelLink =
  | { kind: "session"; sessionId: string }
  | { kind: "decision"; sessionId: string; decisionId: string }
  | { kind: "settings" };

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const DECISION = /^[A-Za-z0-9._-]{1,100}$/;

/** `keel://session/<uuid>[/decision/<id>]`, `keel://settings`; null otherwise. */
export function parseKeelLink(raw: string): KeelLink | null {
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return null;
  }
  if (url.protocol !== "keel:") return null;
  const host = url.hostname.toLowerCase();
  const path = url.pathname.split("/").filter(Boolean);
  const segments = host ? [host, ...path] : path;
  if (segments.length === 1 && segments[0] === "settings") return { kind: "settings" };
  if (segments[0] !== "session" || segments.length < 2) return null;
  const sessionId = segments[1];
  if (!UUID.test(sessionId)) return null;
  if (segments.length === 2) return { kind: "session", sessionId: sessionId.toLowerCase() };
  if (segments.length === 4 && segments[2] === "decision" && DECISION.test(segments[3])) {
    return {
      kind: "decision",
      sessionId: sessionId.toLowerCase(),
      decisionId: segments[3],
    };
  }
  return null;
}
