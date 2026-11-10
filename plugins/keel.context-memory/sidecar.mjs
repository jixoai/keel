#!/usr/bin/env node
// keel.context-memory sidecar: durable, local, keyword/time searchable memory.
//
// Wire protocol: the Keel external-provider gateway (internal/gateway), stdio
// newline-delimited JSON-RPC 2.0. Diagnostics go to stderr only.
//
// Storage: an append-only NDJSON journal owned by the plugin itself. It never
// touches the host's database, secrets or configuration:
//
//   $KEEL_CONTEXT_MEMORY_DIR              (explicit override)
//   else <plugins root>/.data/<plugin id> (sibling of the managed package dir,
//                                         derived from KEEL_PLUGIN_DIR, so an
//                                         upgrade or uninstall of the package
//                                         does not destroy the memory)
//   else <cwd>/.data                      (standalone runs / README checks)
//
// Concurrency: writes are serialized in-process through a promise chain and
// across processes through an atomic lock directory (`mkdir` is the compare-and-
// swap); each record is one `write` call to an O_APPEND descriptor followed by
// fsync, and a torn trailing line is ignored on replay (and reported).
//
// Runtime: Node >= 20. No dependencies.

import process from "node:process";
import path from "node:path";
import fs from "node:fs/promises";

const PROTOCOL_VERSION = "2026-11-07";
const SERVER_NAME = "keel-context-memory-sidecar";
const SERVER_VERSION = "1.0.0";

const PLUGIN_ID = process.env.KEEL_PLUGIN_ID || "keel.context-memory";
const PLUGIN_DIR = process.env.KEEL_PLUGIN_DIR || "";

const MAX_TEXT = 20_000;
const MAX_TAGS = 32;
const MAX_TAG_LEN = 64;
const MAX_QUERY_LEN = 1_000;
const DEFAULT_LIMIT = 10;
const MAX_LIMIT = 50;
const LOCK_TIMEOUT_MS = 5_000;
const LOCK_STALE_MS = 10_000;
const EOF_DRAIN_MS = 8_000;

const log = (...args) => console.error(`[${PLUGIN_ID}]`, ...args);
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// --- storage ---------------------------------------------------------------

function resolveDataDir() {
  if (process.env.KEEL_CONTEXT_MEMORY_DIR) return path.resolve(process.env.KEEL_CONTEXT_MEMORY_DIR);
  if (PLUGIN_DIR) return path.join(path.dirname(path.resolve(PLUGIN_DIR)), ".data", PLUGIN_ID);
  return path.join(process.cwd(), ".data");
}

const DATA_DIR = resolveDataDir();
const JOURNAL = path.join(DATA_DIR, "memory.ndjson");
const LOCK = path.join(DATA_DIR, "write.lock");

const store = {
  ready: false,
  entries: [],
  byId: new Map(),
  corruptLines: 0,
  lastError: null,
};

let writeChain = Promise.resolve();

let readyPromise = null;

function ensureReady() {
  if (!readyPromise) readyPromise = loadStore();
  return readyPromise;
}

async function loadStore() {
  if (store.ready) return;
  await fs.mkdir(DATA_DIR, { recursive: true });
  let raw = "";
  try {
    raw = await fs.readFile(JOURNAL, "utf8");
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
  if (raw.length > 0 && !raw.endsWith("\n")) {
    // A crash can leave a torn trailing line. Terminate it before any append so
    // the next record starts on a clean line (the torn line is counted as
    // corrupt on the next load).
    await fs.appendFile(JOURNAL, "\n");
  }
  for (const line of raw.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    try {
      const record = JSON.parse(trimmed);
      if (!record || typeof record.id !== "string" || typeof record.text !== "string") {
        store.corruptLines += 1;
        continue;
      }
      if (store.byId.has(record.id)) continue;
      const entry = normalizeStored(record);
      store.entries.push(entry);
      store.byId.set(entry.id, entry);
    } catch {
      store.corruptLines += 1;
    }
  }
  store.ready = true;
  log(`memory ready: ${store.entries.length} entries, ${store.corruptLines} corrupt line(s), dir=${DATA_DIR}`);
}

function normalizeStored(record) {
  return {
    id: record.id,
    text: record.text,
    tags: Array.isArray(record.tags) ? record.tags.filter((tag) => typeof tag === "string") : [],
    createdAt: typeof record.createdAt === "string" ? record.createdAt : new Date(0).toISOString(),
    ...(typeof record.source === "string" && record.source ? { source: record.source } : {}),
  };
}

async function acquireLock() {
  const deadline = Date.now() + LOCK_TIMEOUT_MS;
  for (;;) {
    try {
      await fs.mkdir(LOCK);
      return;
    } catch (error) {
      if (error.code !== "EEXIST") throw error;
      // A crashed writer leaves the directory behind; break only a stale one.
      try {
        const info = await fs.stat(LOCK);
        if (Date.now() - info.mtimeMs > LOCK_STALE_MS) {
          await fs.rmdir(LOCK).catch(() => {});
          continue;
        }
      } catch {
        continue;
      }
      if (Date.now() > deadline) {
        throw Object.assign(new Error(`timed out waiting for the memory write lock at ${LOCK}`), {
          code: "storage_busy",
        });
      }
      await sleep(20);
    }
  }
}

async function releaseLock() {
  await fs.rmdir(LOCK).catch(() => {});
}

async function appendRecord(record) {
  await ensureReady();
  await acquireLock();
  let handle;
  try {
    handle = await fs.open(JOURNAL, "a");
    await handle.write(`${JSON.stringify(record)}\n`);
    await handle.sync();
  } finally {
    if (handle) await handle.close().catch(() => {});
    await releaseLock();
  }
  store.entries.push(record);
  store.byId.set(record.id, record);
}

// serializeWrites keeps one process's writers strictly ordered (the record id
// already sorts by time, but the journal must stay parseable line by line).
function serializeWrites(operation) {
  const next = writeChain.then(operation, operation);
  writeChain = next.catch(() => {});
  return next;
}

// --- argument validation ---------------------------------------------------

function invalidArguments(message) {
  return Object.assign(new Error(message), { code: -32602 });
}

function normalizeTag(tag) {
  return String(tag).trim().toLowerCase().slice(0, MAX_TAG_LEN);
}

function validateTags(value) {
  if (value === undefined) return [];
  if (!Array.isArray(value)) throw invalidArguments("tags must be an array of strings");
  if (value.length > MAX_TAGS) throw invalidArguments(`tags must have at most ${MAX_TAGS} entries`);
  return [...new Set(value.map(normalizeTag).filter(Boolean))];
}

function validateTimestamp(name, value) {
  if (value === undefined || value === null || value === "") return null;
  if (typeof value === "number") {
    if (!Number.isFinite(value)) throw invalidArguments(`${name} must be a finite epoch-millisecond number`);
    return new Date(value).toISOString();
  }
  if (typeof value === "string") {
    const parsed = Date.parse(value);
    if (!Number.isFinite(parsed)) throw invalidArguments(`${name} is not a parsable date: ${value}`);
    return new Date(parsed).toISOString();
  }
  throw invalidArguments(`${name} must be an ISO date string or epoch milliseconds`);
}

function validateLimit(value) {
  if (value === undefined) return DEFAULT_LIMIT;
  if (!Number.isInteger(value) || value < 1 || value > MAX_LIMIT) {
    throw invalidArguments(`limit must be an integer between 1 and ${MAX_LIMIT}`);
  }
  return value;
}

function rememberArguments(args) {
  if (!args || typeof args !== "object" || Array.isArray(args)) throw invalidArguments("arguments must be an object");
  const text = typeof args.text === "string" ? args.text.trim() : "";
  if (!text) throw invalidArguments("text is required and must be a non-empty string");
  if (text.length > MAX_TEXT) throw invalidArguments(`text must be at most ${MAX_TEXT} characters`);
  const tags = validateTags(args.tags);
  let source = "";
  if (args.source !== undefined) {
    if (typeof args.source !== "string" || args.source.length > 200) throw invalidArguments("source must be a string of at most 200 characters");
    source = args.source.trim();
  }
  return { text, tags, source };
}

function recallArguments(args) {
  if (!args || typeof args !== "object" || Array.isArray(args)) throw invalidArguments("arguments must be an object");
  const query = typeof args.query === "string" ? args.query.trim() : "";
  if (query.length > MAX_QUERY_LEN) throw invalidArguments(`query must be at most ${MAX_QUERY_LEN} characters`);
  const tags = validateTags(args.tags);
  const since = validateTimestamp("since", args.since);
  const until = validateTimestamp("until", args.until);
  if (since && until && since > until) throw invalidArguments("since must not be after until");
  const mode = args.mode === undefined ? "keyword" : args.mode;
  if (mode !== "keyword" && mode !== "recent") throw invalidArguments('mode must be "keyword" or "recent"');
  return { query, tags, since, until, mode, limit: validateLimit(args.limit) };
}

// --- the tools -------------------------------------------------------------

function newID(now) {
  const random = Math.random().toString(36).slice(2, 8);
  return `mem_${now.getTime().toString(36)}_${random}`;
}

function tokenize(value) {
  return [...new Set((value.toLowerCase().match(/[\p{L}\p{N}][\p{L}\p{N}_-]*/gu) || []).filter((token) => token.length > 1))];
}

async function remember(args) {
  const { text, tags, source } = rememberArguments(args);
  const now = new Date();
  const record = {
    id: newID(now),
    text,
    tags,
    createdAt: now.toISOString(),
    ...(source ? { source } : {}),
  };
  await serializeWrites(() => appendRecord(record));
  return {
    entry: record,
    total: store.entries.length,
    store: { path: JOURNAL, entries: store.entries.length, corruptLines: store.corruptLines },
  };
}

function matchesFilters(entry, { tags, since, until }) {
  if (since && entry.createdAt < since) return false;
  if (until && entry.createdAt > until) return false;
  if (tags.length) {
    const have = new Set(entry.tags);
    for (const tag of tags) if (!have.has(tag)) return false;
  }
  return true;
}

function scoreEntry(entry, tokens) {
  if (!tokens.length) return 0;
  const haystack = entry.text.toLowerCase();
  const tags = new Set(entry.tags);
  let score = 0;
  for (const token of tokens) {
    if (tags.has(token)) score += 3;
    if (haystack.includes(token)) score += 1;
  }
  return score;
}

async function recall(args) {
  const { query, tags, since, until, mode, limit } = recallArguments(args);
  await ensureReady();
  const tokens = mode === "recent" ? [] : tokenize(query);
  const scoped = store.entries.filter((entry) => matchesFilters(entry, { tags, since, until }));
  const scored = [];
  for (const entry of scoped) {
    const score = scoreEntry(entry, tokens);
    if (tokens.length && score === 0) continue;
    scored.push({ entry, score });
  }
  if (tokens.length) {
    scored.sort((left, right) => {
      if (right.score !== left.score) return right.score - left.score;
      return right.entry.createdAt.localeCompare(left.entry.createdAt);
    });
  } else {
    scored.sort((left, right) => right.entry.createdAt.localeCompare(left.entry.createdAt));
  }
  const matches = scored.slice(0, limit).map(({ entry, score }) => ({ ...entry, score }));
  return {
    query,
    mode,
    tokens,
    filters: { tags, since, until },
    count: matches.length,
    totalMatches: scored.length,
    totalEntries: store.entries.length,
    entries: matches,
    store: { path: JOURNAL, entries: store.entries.length, corruptLines: store.corruptLines },
  };
}

function renderRemember(result) {
  return `remembered ${result.entry.id} (${result.entry.tags.length} tag(s)); store now holds ${result.total} entr${result.total === 1 ? "y" : "ies"}.`;
}

function renderRecall(result) {
  if (!result.count) {
    return `no memory matched (query=${JSON.stringify(result.query)}, mode=${result.mode}); the store holds ${result.totalEntries} entr${result.totalEntries === 1 ? "y" : "ies"}.`;
  }
  const lines = [`${result.count} of ${result.totalMatches} matching memories (store: ${result.totalEntries}):`];
  for (const entry of result.entries) {
    const tags = entry.tags.length ? ` [${entry.tags.join(", ")}]` : "";
    lines.push(`- ${entry.createdAt} ${entry.id}${tags}: ${entry.text.length > 240 ? `${entry.text.slice(0, 239)}…` : entry.text}`);
  }
  return lines.join("\n");
}

async function callTool(name, args) {
  if (name === "remember") {
    const result = await remember(args);
    return { text: renderRemember(result), structuredContent: result };
  }
  if (name === "recall") {
    const result = await recall(args);
    return { text: renderRecall(result), structuredContent: result };
  }
  throw invalidArguments(`no such tool: ${name}`);
}

// --- protocol loop ---------------------------------------------------------

const TOOLS = [
  {
    name: "remember",
    title: "Remember",
    description:
      "Persist one durable memory entry (free text + tags) in the plugin's own local journal. Safe under concurrent calls; entries survive restarts and package upgrades.",
    inputSchema: {
      type: "object",
      properties: {
        text: { type: "string", description: "The memory to keep (non-empty, at most 20000 characters)." },
        tags: {
          type: "array",
          items: { type: "string", maxLength: MAX_TAG_LEN },
          maxItems: MAX_TAGS,
          description: "Optional lowercase-normalized tags used by recall filtering and scoring.",
        },
        source: { type: "string", description: "Optional provenance label (conversation id, file, URL)." },
      },
      required: ["text"],
      additionalProperties: false,
    },
  },
  {
    name: "recall",
    title: "Recall",
    description:
      "Search the local memory journal by keyword, tag and time window. Keyword mode scores token hits (tags weigh 3x); recent mode returns the newest entries.",
    inputSchema: {
      type: "object",
      properties: {
        query: { type: "string", description: "Keyword query; empty means match everything (use mode=recent for newest first)." },
        tags: { type: "array", items: { type: "string" }, description: "Only entries carrying all of these tags." },
        since: { type: ["string", "number"], description: "Inclusive lower bound (ISO 8601 or epoch ms)." },
        until: { type: ["string", "number"], description: "Inclusive upper bound (ISO 8601 or epoch ms)." },
        mode: { type: "string", enum: ["keyword", "recent"], description: "keyword (default) or recent." },
        limit: { type: "integer", minimum: 1, maximum: MAX_LIMIT, description: `Max entries (default ${DEFAULT_LIMIT}, max ${MAX_LIMIT}).` },
      },
      additionalProperties: false,
    },
  },
];

const inflight = new Map();
const cancelled = new Set();
let pending = 0;
let requestChain = Promise.resolve();

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

function idKey(id) {
  return typeof id === "string" ? `s:${id}` : `n:${JSON.stringify(id)}`;
}

function initializeResult() {
  return {
    protocolVersion: PROTOCOL_VERSION,
    capabilities: { tools: { listChanged: false } },
    serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    provider: { type: "tool_provider" },
    instructions:
      "remember/recall keep durable notes in the plugin's own local journal. Nothing leaves this machine.",
  };
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
      reply(id, { content: [{ type: "text", text: result.text }], structuredContent: result.structuredContent });
    } catch (error) {
      if (error?.code === -32602) {
        replyError(id, -32602, error.message);
      } else {
        // Storage failures are tool results (the model can react), and the
        // structuredContent keeps the OS error visible instead of flattening it.
        const payload = {
          error: { code: error?.code ?? "storage_error", message: error?.message || String(error) },
        };
        reply(id, {
          content: [{ type: "text", text: `context-memory failed: ${payload.error.message}` }],
          structuredContent: payload,
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
    const target = message.params?.requestId;
    const key = idKey(target);
    const controller = inflight.get(key);
    if (controller) controller.abort();
    else cancelled.add(key); // queued but not started yet
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
  // Requests are answered in arrival order: a memory write must be visible to a
  // recall that the caller sent after it, even when the connection pipelines.
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
