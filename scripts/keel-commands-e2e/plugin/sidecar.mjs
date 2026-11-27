#!/usr/bin/env node
// E2E tool provider for the chat-commands change.
//
// One sidecar serves three tools that the plugin's `provides.commands` bind to:
// `refresh(path)` (write), `note(text)` (write) and `purge()` (destructive).
// Every call appends one NDJSON line under $KEEL_COMMANDS_E2E_STATE, which the
// E2E reads to prove a replayed requestId executed exactly once.
//
// Wire protocol: newline-delimited JSON-RPC 2.0 (the Keel external-provider
// gateway); diagnostics on stderr only. Node >= 20, no dependencies.
import fs from "node:fs";
import path from "node:path";
import process from "node:process";

const PROTOCOL_VERSION = "2026-11-07";
const SERVER_NAME = "keel-commands-e2e-sidecar";
const STATE_DIR = process.env.KEEL_COMMANDS_E2E_STATE || "";
const LOG = STATE_DIR ? path.join(STATE_DIR, "calls.ndjson") : "";

const TOOLS = [
  {
    name: "refresh",
    title: "Refresh index",
    description: "Rebuild the E2E demo index for one path.",
    inputSchema: {
      type: "object",
      properties: { path: { type: "string", minLength: 1 } },
      required: ["path"],
      additionalProperties: false,
    },
  },
  {
    name: "note",
    title: "Save note",
    description: "Store one note in the E2E plan.",
    inputSchema: {
      type: "object",
      properties: { text: { type: "string" } },
      required: ["text"],
      additionalProperties: false,
    },
  },
  {
    name: "purge",
    title: "Purge index",
    description: "Delete every E2E index document.",
    inputSchema: { type: "object", properties: {}, additionalProperties: false },
  },
];

function record(tool, args) {
  if (!LOG) return;
  fs.mkdirSync(path.dirname(LOG), { recursive: true });
  fs.appendFileSync(LOG, JSON.stringify({ tool, args, at: new Date().toISOString() }) + "\n");
}

function callTool(name, args) {
  record(name, args);
  if (name === "refresh") {
    const target = typeof args.path === "string" ? args.path : "";
    return {
      text: `indexed ${target || "workspace"}: 7 documents`,
      structuredContent: { indexed: 7, path: target },
    };
  }
  if (name === "note") {
    const text = typeof args.text === "string" ? args.text : "";
    return { text: `saved note: ${text}`, structuredContent: { saved: true, note: text } };
  }
  if (name === "purge") {
    return { text: "purged 7 documents", structuredContent: { purged: 7 } };
  }
  const error = new Error(`no such tool: ${name}`);
  error.code = -32602;
  throw error;
}

function reply(id, result) {
  process.stdout.write(JSON.stringify({ jsonrpc: "2.0", id, result }) + "\n");
}

function replyError(id, code, message) {
  process.stdout.write(JSON.stringify({ jsonrpc: "2.0", id, error: { code, message } }) + "\n");
}

function initializeResult() {
  return {
    protocolVersion: PROTOCOL_VERSION,
    capabilities: { tools: { listChanged: false } },
    serverInfo: { name: SERVER_NAME, version: "1.0.0" },
    provider: { type: "tool_provider" },
    instructions: "E2E command fixture. Every tool call is appended to the state log.",
  };
}

function handle(message) {
  const { id, method, params } = message;
  if (method === "initialize") {
    reply(id, initializeResult());
    return;
  }
  if (method === "tools/list") {
    reply(id, { tools: TOOLS });
    return;
  }
  if (method === "tools/call") {
    const call = params && typeof params === "object" ? params : {};
    if (typeof call.name !== "string" || !call.name) {
      replyError(id, -32602, "tools/call params.name is required");
      return;
    }
    try {
      const result = callTool(call.name, call.arguments ?? {});
      reply(id, {
        content: [{ type: "text", text: result.text }],
        structuredContent: result.structuredContent,
      });
    } catch (error) {
      if (error?.code === -32602) replyError(id, -32602, error.message);
      else {
        reply(id, {
          content: [{ type: "text", text: `e2e command failed: ${error?.message || String(error)}` }],
          isError: true,
        });
      }
    }
    return;
  }
  if (method === "close") {
    setTimeout(() => process.exit(0), 20).unref?.();
    return;
  }
  replyError(id, -32601, `no such method: ${method}`);
}

let buffer = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  buffer += chunk;
  let newline = buffer.indexOf("\n");
  while (newline >= 0) {
    const line = buffer.slice(0, newline).replace(/\r$/, "").trim();
    buffer = buffer.slice(newline + 1);
    if (line) {
      try {
        const message = JSON.parse(line);
        if (message && typeof message === "object") {
          if (message.id === undefined || message.id === null) {
            if (message.method === "close") setTimeout(() => process.exit(0), 20).unref?.();
          } else {
            handle(message);
          }
        }
      } catch (error) {
        console.error(`[${SERVER_NAME}] bad frame: ${error.message}`);
      }
    }
    newline = buffer.indexOf("\n");
  }
});
process.stdin.on("end", () => process.exit(0));
