// A keel:// link is untrusted input: only the two session shapes and settings
// parse, and only a UUID names a session.
import { expect, test } from "nsh/test";
import { parseKeelLink } from "./links";

const ID = "11111111-2222-3333-4444-555555555555";

test("keel 深链：session / decision / settings", () => {
  expect(parseKeelLink(`keel://session/${ID}`)).toEqual({ kind: "session", sessionId: ID });
  expect(parseKeelLink(`keel:///session/${ID}`)).toEqual({ kind: "session", sessionId: ID });
  expect(parseKeelLink(`keel://session/${ID}/decision/dec-1`)).toEqual({
    kind: "decision",
    sessionId: ID,
    decisionId: "dec-1",
  });
  expect(parseKeelLink("keel://settings")).toEqual({ kind: "settings" });
  expect(parseKeelLink("keel:///settings")).toEqual({ kind: "settings" });
});

test("keel 深链：非法/未知链接不解析", () => {
  expect(parseKeelLink(`keel://session/not-a-uuid`)).toBeNull();
  expect(parseKeelLink(`keel://session/${ID}/other`)).toBeNull();
  expect(parseKeelLink(`keel://session/${ID}/decision/../x`)).toBeNull();
  expect(parseKeelLink("https://example.com/")).toBeNull();
  expect(parseKeelLink("keel://")).toBeNull();
  expect(parseKeelLink("not a url")).toBeNull();
});
