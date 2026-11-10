#!/usr/bin/env node
// The plugin's backend: a stdio line-delimited JSON-RPC tool provider.
//
// The host spawns this file (manifest `entry.command`) with the package
// directory as its working directory and speaks the gateway protocol:
//
//   initialize  → {protocolVersion, capabilities:{tools}, serverInfo, provider:{type:"tool_provider"}}
//   tools/list  → {tools:[{name, description, inputSchema}]}
//   tools/call  → {content:[{type:"text", text}], structuredContent, isError}
//   close       → exit (notification; the host then walks its shutdown ladder)
//
// Frames are newline-delimited compact JSON, one message per line, on stdin and
// stdout. Diagnostics go to stderr only: stdout is the protocol stream.
//
// Replace `toolName`, `toolDescription` and `runTool` with what your plugin
// does. The scaffold substitutes the tool name.
import process from "node:process";
import { createInterface } from "node:readline";

const PROTOCOL_VERSION = "2026-11-07";
const PLUGIN_ID = process.env.KEEL_PLUGIN_ID || "plugin";
const PLUGIN_VERSION = process.env.KEEL_PLUGIN_VERSION || "0.0.0";

const toolName = "{{tool}}";
const startedAt = Date.now();
let calls = 0;

const tools = [
  {
    name: toolName,
    title: "Plugin tool",
    description: "Report this plugin's live state and echo the arguments it received.",
    inputSchema: {
      type: "object",
      properties: {
        note: { type: "string", description: "Free-form text echoed back" },
      },
      additionalProperties: true,
    },
  },
];

// runTool is the whole capability of this plugin. Keep it side-effect free
// unless the manifest declares matching permissions (fs / net / secrets).
function runTool(name, args) {
  calls += 1;
  const document = {
    plugin: PLUGIN_ID,
    version: PLUGIN_VERSION,
    tool: name,
    call: calls,
    uptimeMs: Date.now() - startedAt,
    at: new Date().toISOString(),
    args: args ?? {},
  };
  return {
    content: [{ type: "text", text: JSON.stringify(document) }],
    structuredContent: document,
  };
}

function write(message) {
  process.stdout.write(JSON.stringify(message) + "\n");
}

function handle(request) {
  const { id, method, params } = request;
  switch (method) {
    case "initialize":
      return {
        jsonrpc: "2.0",
        id,
        result: {
          protocolVersion: PROTOCOL_VERSION,
          capabilities: { tools: { listChanged: false } },
          serverInfo: { name: PLUGIN_ID, version: PLUGIN_VERSION, title: "Plugin backend" },
          provider: { type: "tool_provider" },
        },
      };
    case "tools/list":
      return { jsonrpc: "2.0", id, result: { tools } };
    case "tools/call": {
      const name = params && params.name;
      if (!tools.some((tool) => tool.name === name)) {
        return {
          jsonrpc: "2.0",
          id,
          error: { code: -32602, message: "unknown tool: " + String(name) },
        };
      }
      return { jsonrpc: "2.0", id, result: runTool(name, params.arguments) };
    }
    case "ping":
      return { jsonrpc: "2.0", id, result: {} };
    default:
      if (id === undefined) return null; // a notification we do not implement
      return {
        jsonrpc: "2.0",
        id,
        error: { code: -32601, message: "no such method: " + String(method) },
      };
  }
}

const reader = createInterface({ input: process.stdin, crlfDelay: Infinity });
reader.on("line", (line) => {
  if (!line.trim()) return;
  let message;
  try {
    message = JSON.parse(line);
  } catch (error) {
    process.stderr.write("plugin backend: bad frame: " + error.message + "\n");
    return;
  }
  if (message && message.method === "close") {
    reader.close();
    process.exit(0);
  }
  const answer = handle(message || {});
  if (answer) write(answer);
});
reader.on("close", () => process.exit(0));
