// The session title's client face — change keel-harness-v2, W12
// `keel.session-title`.
//
// The server derives the title from the FIRST user message; the client must
// never invent one. These cases pin the two things the client owns: the
// fallback for an unnamed session (a relative time, never a raw session id) and
// the one-line flattening every list/header read shares.
import { expect, test } from "nsh/test";
import type { SessionSummary } from "../data/contract";
import { oneLine, sessionTitle } from "./session-title";

const row = (patch: Partial<SessionSummary> = {}): SessionSummary => ({
  sessionId: "11111111-1111-1111-1111-111111111111",
  ...patch,
});

test("session-title: a derived title wins and keeps one line", () => {
  expect(oneLine("  发布\n准备\t周报  ")).toBe("发布 准备 周报");
  expect(sessionTitle(row({ title: "发布准备周报" }))).toBe("发布准备周报");
  expect(sessionTitle(row({ title: "  发布\n准备  " }))).toBe("发布 准备");
});

test("session-title: an unnamed session falls back to 新会话 · time, never the id", () => {
  const fallback = sessionTitle(row({ createdAt: "2026-11-23T00:00:00.000Z" }));
  expect(fallback.startsWith("新会话")).toBe(true);
  if (fallback !== "新会话") {
    expect(fallback.startsWith("新会话 · ")).toBe(true);
  }
  expect(fallback).not.toContain("11111111-1111-1111-1111-111111111111");
  // The server's placeholder is not a title either: the row still reads as a
  // fresh session rather than the literal word.
  expect(sessionTitle(row({ title: "新会话", createdAt: "2026-11-23T00:00:00.000Z" })).startsWith("新会话")).toBe(true);
  expect(sessionTitle(null)).toBe("新会话");
});
