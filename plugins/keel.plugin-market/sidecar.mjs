#!/usr/bin/env node
// keel.plugin-market sidecar: the queryable face of the DSH coverage index.
//
// Wire protocol: the Keel external-provider gateway (internal/gateway), stdio
// newline-delimited JSON-RPC 2.0. Diagnostics go to stderr only.
//
// The index is a static file shipped inside this package (catalog.json). It
// carries one entry per DSH package group plus the community projects observed
// on GitHub, each with the verified DSH package names, the source link and the
// keel plugin that covers (or will cover) that capability. Nothing is fetched
// at run time: the market is offline, deterministic and auditable.
//
// Runtime: Node >= 20. No dependencies.

import process from "node:process";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";

const PROTOCOL_VERSION = "2026-11-07";
const SERVER_NAME = "keel-plugin-market-sidecar";
const SERVER_VERSION = "1.0.0";

const PLUGIN_ID = process.env.KEEL_PLUGIN_ID || "keel.plugin-market";
const CATALOG_PATH = fileURLToPath(new URL("./catalog.json", import.meta.url));

const DEFAULT_LIMIT = 10;
const MAX_LIMIT = 100;
const EOF_DRAIN_MS = 8_000;

const SOURCE_KINDS = ["dsh-official", "dsh-community", "keel-official"];
const STATUSES = ["delivered", "partial", "planned", "registered", "absent"];
const STATUS_RANK = { delivered: 0, partial: 1, planned: 2, registered: 3, absent: 4 };

const log = (...args) => console.error(`[${PLUGIN_ID}]`, ...args);
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// --- catalog ---------------------------------------------------------------

const catalog = { ready: false, error: null, data: null };

async function loadCatalog() {
  if (catalog.ready) return catalog;
  try {
    const raw = await fs.readFile(CATALOG_PATH, "utf8");
    const data = JSON.parse(raw);
    if (!data || data.schema !== "keel.plugin-market.catalog/1" || !Array.isArray(data.entries)) {
      throw new Error(`unexpected catalog schema (${data?.schema ?? "missing"})`);
    }
    catalog.data = data;
    catalog.ready = true;
    log(`catalog ready: ${data.entries.length} entries (${CATALOG_PATH})`);
  } catch (error) {
    catalog.error = error;
    throw error;
  }
  return catalog;
}

function summaryOf(data) {
  const byStatus = Object.fromEntries(STATUSES.map((status) => [status, 0]));
  const bySource = Object.fromEntries(SOURCE_KINDS.map((kind) => [kind, 0]));
  for (const entry of data.entries) {
    byStatus[entry.coverage?.status] = (byStatus[entry.coverage?.status] ?? 0) + 1;
    bySource[entry.source?.kind] = (bySource[entry.source?.kind] ?? 0) + 1;
  }
  return {
    schema: data.schema,
    generatedAt: data.generatedAt,
    sources: data.sources,
    entries: data.entries.length,
    byStatus,
    bySource,
  };
}

// --- search ----------------------------------------------------------------

function invalidArguments(message) {
  return Object.assign(new Error(message), { code: -32602 });
}

function tokenize(value) {
  return [...new Set((String(value).toLowerCase().match(/[\p{L}\p{N}][\p{L}\p{N}._-]*/gu) || []).filter((token) => token.length > 0))];
}

function haystack(entry) {
  return [
    entry.id,
    entry.name,
    entry.summary,
    ...(entry.capabilities || []),
    ...(entry.dshPackages || []),
    entry.coverage?.keelPluginId || "",
    ...(entry.coverage?.tools || []),
    entry.source?.repo || "",
    entry.source?.path || "",
  ]
    .join(" ")
    .toLowerCase();
}

function scoreEntry(entry, tokens, rawQuery) {
  if (!tokens.length) return 0;
  const id = entry.id.toLowerCase();
  const keelID = (entry.coverage?.keelPluginId || "").toLowerCase();
  const capabilities = (entry.capabilities || []).map((value) => value.toLowerCase());
  const packages = (entry.dshPackages || []).map((value) => value.toLowerCase());
  const text = haystack(entry);
  let score = 0;
  const query = rawQuery.trim().toLowerCase();
  if (query && id === query) score += 100;
  if (query && keelID === query) score += 90;
  if (query && packages.includes(query)) score += 60;
  for (const token of tokens) {
    if (id.includes(token)) score += 12;
    if (keelID.includes(token)) score += 10;
    if (capabilities.some((cap) => cap === token)) score += 8;
    else if (capabilities.some((cap) => cap.includes(token))) score += 4;
    if (packages.some((pkg) => pkg === token)) score += 3;
    else if (packages.some((pkg) => pkg.includes(token))) score += 1;
    const occurrences = text.split(token).length - 1;
    if (occurrences) score += Math.min(occurrences, 5);
  }
  return score;
}

function normalizeLimit(value) {
  if (value === undefined) return DEFAULT_LIMIT;
  if (!Number.isInteger(value) || value < 1 || value > MAX_LIMIT) {
    throw invalidArguments(`limit must be an integer between 1 and ${MAX_LIMIT}`);
  }
  return value;
}

function normalizeList(value, name) {
  if (value === undefined) return [];
  const raw = Array.isArray(value) ? value : [value];
  const out = [];
  for (const item of raw) {
    if (typeof item !== "string") throw invalidArguments(`${name} must be a string or an array of strings`);
    for (const part of item.split(",")) {
      const trimmed = part.trim().toLowerCase();
      if (trimmed) out.push(trimmed);
    }
  }
  return [...new Set(out)];
}

function filterEntries(entries, { statuses, sources, capabilities }) {
  return entries.filter((entry) => {
    if (statuses.length && !statuses.includes(String(entry.coverage?.status || "").toLowerCase())) return false;
    if (sources.length && !sources.includes(String(entry.source?.kind || "").toLowerCase())) return false;
    if (capabilities.length) {
      const caps = (entry.capabilities || []).map((value) => value.toLowerCase());
      const hit = capabilities.every((wanted) => caps.some((cap) => cap === wanted || cap.includes(wanted)));
      if (!hit) return false;
    }
    return true;
  });
}

function compareEntries(left, right, tokens) {
  const leftScore = left.score ?? 0;
  const rightScore = right.score ?? 0;
  if (rightScore !== leftScore) return rightScore - leftScore;
  const leftStatus = STATUS_RANK[left.entry.coverage?.status] ?? 9;
  const rightStatus = STATUS_RANK[right.entry.coverage?.status] ?? 9;
  if (leftStatus !== rightStatus) return leftStatus - rightStatus;
  if (tokens.length === 0 || leftScore === 0) return left.entry.id.localeCompare(right.entry.id);
  return left.entry.id.localeCompare(right.entry.id);
}

async function marketSearch(args) {
  if (args === null || typeof args !== "object" || Array.isArray(args)) throw invalidArguments("arguments must be an object");
  const query = typeof args.query === "string" ? args.query.trim() : "";
  if (query.length > 500) throw invalidArguments("query must be at most 500 characters");
  const limit = normalizeLimit(args.limit);
  const statuses = normalizeList(args.status, "status");
  const sources = normalizeList(args.source, "source");
  const capabilities = normalizeList(args.capability, "capability");
  for (const status of statuses) if (!STATUSES.includes(status)) throw invalidArguments(`unknown status ${status}`);
  for (const source of sources) if (!SOURCE_KINDS.includes(source)) throw invalidArguments(`unknown source ${source}`);

  const { data } = await loadCatalog();
  const tokens = tokenize(query);
  const filtered = filterEntries(data.entries, { statuses, sources, capabilities });
  const scored = filtered.map((entry) => ({ entry, score: scoreEntry(entry, tokens, query) }));
  const matched = tokens.length ? scored.filter((item) => item.score > 0) : scored;
  matched.sort((left, right) => compareEntries(left, right, tokens));
  const page = matched.slice(0, limit);
  return {
    query,
    filters: { status: statuses, source: sources, capability: capabilities },
    tokens,
    totalMatches: matched.length,
    count: page.length,
    entries: page.map(({ entry, score }) => ({ ...entry, score })),
    catalog: summaryOf(data),
  };
}

function renderSearch(result) {
  if (!result.count) {
    return `plugin market: no entry matched (query=${JSON.stringify(result.query)}); the catalog holds ${result.catalog.entries} entries.`;
  }
  const lines = [
    `plugin market: ${result.count} of ${result.totalMatches} matching entries (catalog ${result.catalog.generatedAt}, ${result.catalog.entries} entries)`,
  ];
  for (const entry of result.entries) {
    const coverage = entry.coverage?.status === "delivered" ? "delivered" : `${entry.coverage?.status} → ${entry.coverage?.keelPluginId ?? "—"}`;
    lines.push(`- ${entry.id} · ${entry.name}`);
    lines.push(`  ${entry.summary}`);
    lines.push(`  coverage: ${coverage} · source: ${entry.source?.kind} ${entry.source?.url}`);
  }
  return lines.join("\n");
}

// --- info ------------------------------------------------------------------

function entryMatches(entry, raw) {
  const wanted = raw.trim().toLowerCase();
  if (!wanted) return [];
  const reasons = [];
  if (entry.id.toLowerCase() === wanted) reasons.push("id");
  if ((entry.coverage?.keelPluginId || "").toLowerCase() === wanted) reasons.push("keelPluginId");
  if ((entry.coverage?.tools || []).some((tool) => tool.toLowerCase() === wanted)) reasons.push("tool");
  if ((entry.name || "").toLowerCase() === wanted) reasons.push("name");
  if ((entry.dshPackages || []).some((pkg) => pkg.toLowerCase() === wanted)) reasons.push("dshPackage");
  return reasons;
}

async function marketInfo(args) {
  if (args === null || typeof args !== "object" || Array.isArray(args)) throw invalidArguments("arguments must be an object");
  const id = typeof args.id === "string" ? args.id.trim() : "";
  if (!id) throw invalidArguments("id is required (an entry id, a keel plugin id, a tool name or a DSH package name)");
  const { data } = await loadCatalog();
  const matches = [];
  for (const entry of data.entries) {
    const reasons = entryMatches(entry, id);
    if (reasons.length) matches.push({ entry, matchedBy: reasons });
  }
  return { query: id, found: matches.length > 0, count: matches.length, matches, catalog: summaryOf(data) };
}

function renderInfo(result) {
  if (!result.found) {
    return `plugin market: ${JSON.stringify(result.query)} is not in the catalog (${result.catalog.entries} entries).`;
  }
  const lines = [`plugin market: ${result.count} match(es) for ${JSON.stringify(result.query)}`];
  for (const { entry, matchedBy } of result.matches) {
    lines.push(`- ${entry.id} · ${entry.name} (matched by ${matchedBy.join(", ")})`);
    lines.push(`  ${entry.summary}`);
    lines.push(
      `  coverage: ${entry.coverage?.status} → ${entry.coverage?.keelPluginId ?? "—"}` +
        (entry.coverage?.tools?.length ? ` · tools ${entry.coverage.tools.join(", ")}` : ""),
    );
    lines.push(`  source: ${entry.source?.kind} ${entry.source?.url}`);
    if (entry.dshPackages?.length) lines.push(`  DSH packages: ${entry.dshPackages.join(", ")}`);
  }
  return lines.join("\n");
}

// --- protocol loop ---------------------------------------------------------

const TOOLS = [
  {
    name: "search",
    title: "Plugin market search",
    description:
      "Search the bundled DSH plugin coverage index by keyword, capability, coverage status or source. Returns entries with the verified DSH package names and the keel plugin covering them.",
    inputSchema: {
      type: "object",
      properties: {
        query: { type: "string", description: "Free-text query over ids, names, summaries, capabilities, DSH package names and source repos." },
        capability: {
          type: "array",
          items: { type: "string" },
          description: "Only entries whose capability list contains every given term.",
        },
        status: {
          type: "array",
          items: { type: "string", enum: STATUSES },
          description: "Coverage status filter (delivered, partial, planned, registered, absent).",
        },
        source: {
          type: "array",
          items: { type: "string", enum: SOURCE_KINDS },
          description: "Source kind filter.",
        },
        limit: { type: "integer", minimum: 1, maximum: MAX_LIMIT, description: `Max entries (default ${DEFAULT_LIMIT}, max ${MAX_LIMIT}).` },
      },
      additionalProperties: false,
    },
  },
  {
    name: "info",
    title: "Plugin market entry",
    description:
      "Look up one catalog entry by entry id, keel plugin id, tool name or DSH package name. Answers with the coverage verdict and the source link.",
    inputSchema: {
      type: "object",
      properties: {
        id: { type: "string", description: "Entry id (e.g. dsh.context), keel plugin id (e.g. keel.context-memory), tool name or DSH package name." },
      },
      required: ["id"],
      additionalProperties: false,
    },
  },
];

const inflight = new Map();
const cancelled = new Set();
let pending = 0;
let requestChain = Promise.resolve();

function idKey(id) {
  return typeof id === "string" ? `s:${id}` : `n:${JSON.stringify(id)}`;
}

function writeFrame(message) {
  if (!process.stdout.writable) return;
  process.stdout.write(`${JSON.stringify(message)}\n`);
}

function reply(id, result) {
  writeFrame({ jsonrpc: "2.0", id, result });
}

function replyError(id, code, message) {
  writeFrame({ jsonrpc: "2.0", id, error: { code, message } });
}

function initializeResult() {
  return {
    protocolVersion: PROTOCOL_VERSION,
    capabilities: { tools: { listChanged: false } },
    serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    provider: { type: "tool_provider" },
    instructions:
      "search/info answer from a static DSH coverage catalog shipped with this plugin. The catalog records the verified DSH group/package names, source links and the keel plugin covering each capability.",
  };
}

async function callTool(name, args) {
  if (name === "search") {
    const result = await marketSearch(args);
    return { text: renderSearch(result), structuredContent: result };
  }
  if (name === "info") {
    const result = await marketInfo(args);
    return { text: renderInfo(result), structuredContent: result, isError: !result.found };
  }
  throw invalidArguments(`no such tool: ${name}`);
}

async function handleRequest(message) {
  await routeRequest(message);
}

async function routeRequest(message) {
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
    if (cancelled.delete(idKey(id))) {
      replyError(id, -32002, "the request was canceled");
      return;
    }
    const controller = new AbortController();
    inflight.set(idKey(id), controller);
    try {
      const result = await callTool(call.name, call.arguments ?? {});
      reply(id, {
        content: [{ type: "text", text: result.text }],
        structuredContent: result.structuredContent,
        ...(result.isError ? { isError: true } : {}),
      });
    } catch (error) {
      if (error?.code === -32602) {
        replyError(id, -32602, error.message);
      } else {
        reply(id, {
          content: [{ type: "text", text: `plugin market failed: ${error?.message || String(error)}` }],
          structuredContent: { error: { code: "catalog_unavailable", message: error?.message || String(error), path: CATALOG_PATH } },
          isError: true,
        });
      }
    } finally {
      inflight.delete(idKey(id));
    }
    return;
  }
  replyError(id, -32601, `no such method: ${method}`);
}

function handleNotification(message) {
  if (message.method === "notifications/cancelled") {
    const key = idKey(message.params?.requestId);
    const controller = inflight.get(key);
    if (controller) controller.abort();
    else cancelled.add(key);
    return;
  }
  if (message.method === "close") {
    setTimeout(() => process.exit(0), 20).unref?.();
  }
}

function dispatch(line) {
  let message;
  try {
    message = JSON.parse(line);
  } catch (error) {
    log("discarding a non-JSON frame:", error.message);
    return;
  }
  if (!message || typeof message !== "object") return;
  if (message.id === undefined || message.id === null) {
    handleNotification(message);
    return;
  }
  pending += 1;
  const run = () =>
    handleRequest(message)
      .catch((error) => {
        log("request failed:", error?.stack || String(error));
        replyError(message.id, -32603, error?.message || String(error));
      })
      .finally(() => {
        pending -= 1;
      });
  requestChain = requestChain.then(run, run);
}

async function main() {
  let buffer = "";
  process.stdin.setEncoding("utf8");
  for await (const chunk of process.stdin) {
    buffer += chunk;
    let newline = buffer.indexOf("\n");
    while (newline >= 0) {
      const line = buffer.slice(0, newline).replace(/\r$/, "").trim();
      buffer = buffer.slice(newline + 1);
      if (line) dispatch(line);
      newline = buffer.indexOf("\n");
    }
  }
  if (buffer.trim()) dispatch(buffer.trim());
  const deadline = Date.now() + EOF_DRAIN_MS;
  while (pending > 0 && Date.now() < deadline) await sleep(25);
  process.exit(0);
}

process.on("SIGTERM", () => process.exit(0));
process.on("SIGINT", () => process.exit(0));
process.on("uncaughtException", (error) => {
  log("uncaught exception:", error?.stack || String(error));
  process.exit(1);
});
process.on("unhandledRejection", (error) => {
  log("unhandled rejection:", error?.stack || String(error));
});

main().catch((error) => {
  log("fatal:", error?.stack || String(error));
  process.exit(1);
});
