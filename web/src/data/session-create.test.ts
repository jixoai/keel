// The workspace "+" contract: a live session is always created as a Bot-owned
// session (identity-access spec: each Session belongs to exactly one Bot), and
// the Bot is resolved only when the tenant has exactly one active employee.
import { expect, test } from "nsh/test";
import type { BotView } from "./identity";
import { KeelHttp, type FetchLike } from "./http";
import { resolveWorkspaceBot } from "./session-create";

const bot = (botId: string, archived = false): BotView => ({
  botId,
  name: botId,
  profileId: "p-1",
  accountId: "a-1",
  modelKind: "text",
  defaultModel: { providerId: "fake", modelId: "fake" },
  harness: { tools: [], context: { instruction: "", permissions: [] } },
  archived,
  sharedCount: 0,
});

test("createSession sends the Bot it was given", async () => {
  let body: unknown = null;
  const fetchImpl: FetchLike = async (input, init) => {
    if (String(input) === "/v1/sessions" && (init?.method ?? "GET").toUpperCase() === "POST") {
      body = init?.body ? JSON.parse(String(init.body)) : null;
      return new Response(JSON.stringify({ sessionId: "s-1", botId: "b-1" }), {
        status: 201,
        headers: { "content-type": "application/json" },
      });
    }
    return new Response(JSON.stringify({ error: "not found" }), { status: 404 });
  };
  const client = new KeelHttp("", fetchImpl);
  expect(await client.createSession("b-1")).toBe("s-1");
  expect(body).toEqual({ botId: "b-1" });
});

test("resolveWorkspaceBot: exactly one active Bot owns the workspace session", () => {
  expect(resolveWorkspaceBot([bot("b-1")])?.botId).toBe("b-1");
  expect(resolveWorkspaceBot([bot("b-1"), bot("b-2", true)])?.botId).toBe("b-1");
  expect(resolveWorkspaceBot([bot("b-1", true)])).toBeNull();
  expect(resolveWorkspaceBot([])).toBeNull();
  expect(resolveWorkspaceBot([bot("b-1"), bot("b-2")])).toBeNull();
});
