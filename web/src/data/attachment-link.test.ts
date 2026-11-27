// Same-origin link policy for attachment/plugin previews (P2).
import { expect, test } from "nsh/test";
import { isSameOrigin } from "./attachment-link";

test("same-origin: a relative path resolves to the page origin", () => {
  expect(isSameOrigin("/v1/files/abc", "https://keel.example")).toBe(true);
  expect(isSameOrigin("https://keel.example/v1/files/abc", "https://keel.example")).toBe(true);
});

test("same-origin: another origin, protocol downgrade, data: and javascript: are refused", () => {
  expect(isSameOrigin("https://evil.example/v1/files/abc", "https://keel.example")).toBe(false);
  expect(isSameOrigin("http://keel.example/v1/files/abc", "https://keel.example")).toBe(false);
  expect(isSameOrigin("//evil.example/v1/files/abc", "https://keel.example")).toBe(false);
  expect(isSameOrigin("data:text/html,x", "https://keel.example")).toBe(false);
  expect(isSameOrigin("javascript:alert(1)", "https://keel.example")).toBe(false);
});
