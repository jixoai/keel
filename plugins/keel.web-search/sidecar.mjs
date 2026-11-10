#!/usr/bin/env node
// keel.web-search sidecar: a real web-search tool provider.
//
// Wire protocol: the Keel external-provider gateway (internal/gateway), stdio
// newline-delimited JSON-RPC 2.0. One JSON frame per line on stdin/stdout;
// diagnostics go to stderr only, so the protocol stream stays clean.
//
// Methods:
//   initialize   -> protocol 2026-11-07, provider tool_provider, capabilities.tools
//   tools/list   -> the tool catalogue (one tool: keel_web_search_search)
//   tools/call   -> run one search
//   close        -> notification; exit on it (the host's shutdown ladder has
//                   stdin EOF -> SIGTERM group -> SIGKILL group behind it)
//   notifications/cancelled -> aborts the matching in-flight HTTP request
//
// Search providers are tried in order until one returns at least one result.
// Each failure is preserved in the result (attempts[]) and a total failure is a
// structured error result (isError: true) -- never a fabricated success.
//
// Runtime: Node >= 20 (global fetch, AbortSignal.timeout). No dependencies.

import process from "node:process";

const PROTOCOL_VERSION = "2026-11-07";
const SERVER_NAME = "keel-web-search-sidecar";
const SERVER_VERSION = "1.0.0";

// Host-injected identity (the sidecar loader sets these; standalone runs do not).
const PLUGIN_ID = process.env.KEEL_PLUGIN_ID || "keel.web-search";

// --- configuration ---------------------------------------------------------

const DEFAULT_PROVIDERS = "bing,duckduckgo,hn,github,npm";
const MAX_LIMIT = 10;
const DEFAULT_LIMIT = 5;
const DEFAULT_PROVIDER_TIMEOUT_MS = 12_000;
const DEFAULT_TOTAL_BUDGET_MS = 30_000;

function intEnv(name, fallback) {
  const raw = process.env[name];
  if (!raw) return fallback;
  const value = Number.parseInt(raw, 10);
  return Number.isFinite(value) && value > 0 ? value : fallback;
}

function listEnv(name, fallback) {
  const raw = process.env[name];
  if (raw === undefined) return fallback;
  const items = raw
    .split(",")
    .map((item) => item.trim().toLowerCase())
    .filter(Boolean);
  return items.length ? items : fallback;
}

const CONFIG = {
  providers: listEnv("KEEL_WEB_SEARCH_PROVIDERS", DEFAULT_PROVIDERS.split(",")),
  providerTimeoutMs: intEnv("KEEL_WEB_SEARCH_TIMEOUT_MS", DEFAULT_PROVIDER_TIMEOUT_MS),
  totalBudgetMs: intEnv("KEEL_WEB_SEARCH_BUDGET_MS", DEFAULT_TOTAL_BUDGET_MS),
  defaultLimit: Math.min(intEnv("KEEL_WEB_SEARCH_LIMIT", DEFAULT_LIMIT), MAX_LIMIT),
};

const log = (...args) => console.error(`[${PLUGIN_ID}]`, ...args);

// --- small HTTP/HTML helpers ----------------------------------------------

class ProviderError extends Error {
  constructor(provider, code, message, extra = {}) {
    super(message);
    this.provider = provider;
    this.code = code;
    this.extra = extra;
  }
}

function classifyFetchError(provider, url, error) {
  const cause = error?.cause;
  const errno = cause?.code || error?.code || error?.name || "unknown";
  const detail = cause?.message || error?.message || String(error);
  let kind = "network_error";
  if (error?.name === "AbortError" || error?.name === "TimeoutError") kind = "timeout";
  else if (["ENOTFOUND", "EAI_AGAIN"].includes(errno)) kind = "dns_error";
  else if (["ECONNREFUSED", "ECONNRESET", "EHOSTUNREACH", "ENETUNREACH", "EPIPE"].includes(errno)) kind = "unreachable";
  else if (String(detail).includes("SSL") || String(detail).includes("TLS")) kind = "tls_error";
  return new ProviderError(provider, kind, `${kind}: ${detail}`, { url, errno: String(errno) });
}

async function getText(provider, url, { timeoutMs, headers = {}, signal } = {}) {
  const timeoutSignal = AbortSignal.timeout(timeoutMs);
  const combined = signal ? AbortSignal.any([signal, timeoutSignal]) : timeoutSignal;
  let response;
  try {
    response = await fetch(url, {
      redirect: "follow",
      signal: combined,
      headers: {
        "user-agent": "keel-web-search/1.0 (+https://github.com/jixoai/keel)",
        accept: "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8",
        "accept-language": "en-US,en;q=0.9",
        ...headers,
      },
    });
  } catch (error) {
    throw classifyFetchError(provider, url, error);
  }
  if (!response.ok) {
    throw new ProviderError(provider, "http_error", `HTTP ${response.status} from ${new URL(url).host}`, {
      url,
      status: response.status,
    });
  }
  return { text: await response.text(), url: response.url || url, status: response.status };
}

async function getJSON(provider, url, options = {}) {
  const { text, url: finalURL, status } = await getText(provider, url, {
    ...options,
    headers: { accept: "application/json", ...(options.headers || {}) },
  });
  try {
    return { json: JSON.parse(text), url: finalURL, status };
  } catch (error) {
    throw new ProviderError(provider, "parse_error", `response from ${new URL(url).host} is not JSON: ${error.message}`, {
      url,
      status,
    });
  }
}

const ENTITIES = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  apos: "'",
  nbsp: " ",
  ensp: " ",
  emsp: " ",
  middot: "·",
  hellip: "…",
  mdash: "—",
  ndash: "–",
  laquo: "«",
  raquo: "»",
  ldquo: "“",
  rdquo: "”",
  lsquo: "‘",
  rsquo: "’",
};

function decodeEntities(value) {
  return value.replace(/&(#x?[0-9a-fA-F]+|[a-zA-Z]+);/g, (match, body) => {
    if (body.startsWith("#x") || body.startsWith("#X")) {
      const code = Number.parseInt(body.slice(2), 16);
      return Number.isFinite(code) ? String.fromCodePoint(code) : match;
    }
    if (body.startsWith("#")) {
      const code = Number.parseInt(body.slice(1), 10);
      return Number.isFinite(code) ? String.fromCodePoint(code) : match;
    }
    return ENTITIES[body.toLowerCase()] ?? match;
  });
}

function textOf(htmlFragment) {
  return decodeEntities(
    htmlFragment
      .replace(/<script[\s\S]*?<\/script>/gi, " ")
      .replace(/<style[\s\S]*?<\/style>/gi, " ")
      .replace(/<[^>]+>/g, " "),
  )
    .replace(/\s+/g, " ")
    .trim();
}

function truncate(value, max) {
  if (typeof value !== "string") return "";
  const text = value.replace(/\s+/g, " ").trim();
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

// --- providers -------------------------------------------------------------

async function searchBing({ query, limit, timeoutMs, signal }) {
  const url = `https://www.bing.com/search?q=${encodeURIComponent(query)}&count=${Math.min(limit * 2, 20)}&setlang=en`;
  const { text } = await getText("bing", url, { timeoutMs, signal, headers: { "accept-language": "en-US,en;q=0.9" } });
  const chunks = text.split(/<li[^>]+class="b_algo"/).slice(1);
  const results = [];
  for (const chunk of chunks) {
    if (results.length >= limit) break;
    const anchor = chunk.match(/<h2[^>]*>[\s\S]*?<a[^>]*href="([^"]+)"[^>]*>([\s\S]*?)<\/a>/);
    if (!anchor) continue;
    const href = decodeEntities(anchor[1]);
    if (!/^https?:\/\//i.test(href)) continue;
    const title = textOf(anchor[2]);
    if (!title) continue;
    const snippetMatch =
      chunk.match(/<p[^>]*class="[^"]*b_lineclamp[^"]*"[^>]*>([\s\S]*?)<\/p>/) ||
      chunk.match(/<p[^>]*>([\s\S]*?)<\/p>/) ||
      chunk.match(/<div[^>]+class="[^"]*b_caption[^"]*"[^>]*>([\s\S]*?)<\/div>/);
    results.push({
      rank: results.length + 1,
      title,
      url: href,
      snippet: snippetMatch ? truncate(textOf(snippetMatch[1]), 320) : "",
      provider: "bing",
    });
  }
  if (!results.length) {
    throw new ProviderError("bing", "empty_result", "the Bing result page carried no parsable organic results", {
      url,
      bytes: text.length,
    });
  }
  return results;
}

async function searchDuckDuckGo({ query, limit, timeoutMs, signal }) {
  const url = `https://html.duckduckgo.com/html/?q=${encodeURIComponent(query)}`;
  const { text } = await getText("duckduckgo", url, { timeoutMs, signal });
  const chunks = text.split(/<div[^>]+class="[^"]*result[^"]*results_links[^"]*"/i).slice(1);
  const results = [];
  for (const chunk of chunks) {
    if (results.length >= limit) break;
    const anchor = chunk.match(/<a[^>]+class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>([\s\S]*?)<\/a>/i);
    if (!anchor) continue;
    let href = decodeEntities(anchor[1]);
    try {
      const parsed = new URL(href, "https://html.duckduckgo.com");
      href = parsed.searchParams.get("uddg") || parsed.href;
    } catch {
      continue;
    }
    if (!/^https?:\/\//i.test(href)) continue;
    const snippetMatch = chunk.match(/<a[^>]+class="[^"]*result__snippet[^"]*"[^>]*>([\s\S]*?)<\/a>/i);
    results.push({
      rank: results.length + 1,
      title: textOf(anchor[2]),
      url: href,
      snippet: snippetMatch ? truncate(textOf(snippetMatch[1]), 320) : "",
      provider: "duckduckgo",
    });
  }
  if (!results.length) {
    throw new ProviderError("duckduckgo", "empty_result", "the DuckDuckGo HTML page carried no parsable results", {
      url,
      bytes: text.length,
    });
  }
  return results;
}

async function searchHN({ query, limit, timeoutMs, signal }) {
  const url = `https://hn.algolia.com/api/v1/search?query=${encodeURIComponent(query)}&hitsPerPage=${limit}&tags=story`;
  const { json } = await getJSON("hn", url, { timeoutMs, signal });
  const hits = Array.isArray(json?.hits) ? json.hits : [];
  const results = [];
  for (const hit of hits) {
    if (results.length >= limit) break;
    const title = truncate(hit.title || hit.story_title || "", 180);
    if (!title) continue;
    const discussion = `https://news.ycombinator.com/item?id=${hit.objectID}`;
    const story = truncate(textOf(String(hit.story_text || hit.comment_text || "").replace(/<\/(p|br)>/g, " ")), 300);
    results.push({
      rank: results.length + 1,
      title,
      url: /^https?:\/\//i.test(hit.url || "") ? hit.url : discussion,
      snippet: story || `Hacker News discussion · ${hit.points ?? 0} points · ${hit.num_comments ?? 0} comments`,
      provider: "hn",
      discussion,
    });
  }
  if (!results.length) {
    throw new ProviderError("hn", "empty_result", "Hacker News search returned no stories", { url });
  }
  return results;
}

async function searchGitHub({ query, limit, timeoutMs, signal }) {
  const url = `https://api.github.com/search/repositories?q=${encodeURIComponent(query)}&per_page=${limit}&sort=stars&order=desc`;
  const { json } = await getJSON("github", url, {
    timeoutMs,
    signal,
    headers: { accept: "application/vnd.github+json", "x-github-api-version": "2022-11-28" },
  });
  const items = Array.isArray(json?.items) ? json.items : [];
  const results = [];
  for (const item of items) {
    if (results.length >= limit) break;
    if (!item?.html_url || !item?.full_name) continue;
    const bits = [];
    if (item.description) bits.push(truncate(item.description, 240));
    bits.push(`★ ${item.stargazers_count ?? 0}`);
    if (item.language) bits.push(item.language);
    results.push({
      rank: results.length + 1,
      title: item.full_name,
      url: item.html_url,
      snippet: bits.join(" · "),
      provider: "github",
    });
  }
  if (!results.length) {
    throw new ProviderError("github", "empty_result", "GitHub repository search returned no repositories", { url });
  }
  return results;
}

async function searchNPM({ query, limit, timeoutMs, signal }) {
  const url = `https://registry.npmjs.org/-/v1/search?text=${encodeURIComponent(query)}&size=${limit}`;
  const { json } = await getJSON("npm", url, { timeoutMs, signal });
  const objects = Array.isArray(json?.objects) ? json.objects : [];
  const results = [];
  for (const object of objects) {
    if (results.length >= limit) break;
    const pkg = object?.package;
    if (!pkg?.name) continue;
    const link = pkg.links?.npm || pkg.links?.homepage || `https://www.npmjs.com/package/${pkg.name}`;
    const bits = [];
    if (pkg.description) bits.push(truncate(pkg.description, 240));
    if (pkg.version) bits.push(`v${pkg.version}`);
    results.push({
      rank: results.length + 1,
      title: pkg.name,
      url: link,
      snippet: bits.join(" · "),
      provider: "npm",
    });
  }
  if (!results.length) {
    throw new ProviderError("npm", "empty_result", "the npm registry search returned no packages", { url });
  }
  return results;
}

const PROVIDERS = {
  bing: searchBing,
  duckduckgo: searchDuckDuckGo,
  hn: searchHN,
  github: searchGitHub,
  npm: searchNPM,
};

// --- the tool --------------------------------------------------------------

const TOOL_NAME = "search";

const TOOL_SCHEMA = {
  type: "object",
  properties: {
    query: { type: "string", description: "The search query. Natural-language questions work; operators are treated literally." },
    limit: {
      type: "integer",
      minimum: 1,
      maximum: MAX_LIMIT,
      description: `How many results to return (default ${CONFIG.defaultLimit}, max ${MAX_LIMIT}).`,
    },
    provider: {
      type: "string",
      enum: ["auto", ...Object.keys(PROVIDERS)],
      description: "Pin one upstream provider instead of walking the fallback chain.",
    },
  },
  required: ["query"],
  additionalProperties: false,
};

function invalidArguments(message) {
  const error = new Error(message);
  error.code = -32602;
  return error;
}

function validateArguments(args) {
  if (args === null || typeof args !== "object" || Array.isArray(args)) {
    throw invalidArguments("arguments must be an object");
  }
  const query = typeof args.query === "string" ? args.query.trim() : "";
  if (!query) throw invalidArguments("query is required and must be a non-empty string");
  if (query.length > 2000) throw invalidArguments("query must be at most 2000 characters");
  let limit = CONFIG.defaultLimit;
  if (args.limit !== undefined) {
    if (!Number.isInteger(args.limit) || args.limit < 1 || args.limit > MAX_LIMIT) {
      throw invalidArguments(`limit must be an integer between 1 and ${MAX_LIMIT}`);
    }
    limit = args.limit;
  }
  let providers = CONFIG.providers;
  if (typeof args.provider === "string" && args.provider !== "auto") {
    const pinned = args.provider.toLowerCase();
    if (!PROVIDERS[pinned]) throw invalidArguments(`unknown provider ${args.provider}`);
    providers = [pinned];
  }
  return { query, limit, providers };
}

async function runSearch(args, signal) {
  const { query, limit, providers } = validateArguments(args);
  const startedAt = Date.now();
  const deadline = startedAt + CONFIG.totalBudgetMs;
  const attempts = [];
  for (const provider of providers) {
    const remaining = deadline - Date.now();
    if (remaining <= 0) {
      attempts.push({ provider, ok: false, resultCount: 0, ms: 0, error: { code: "budget_exhausted", message: "the total search budget was exhausted before this provider ran" } });
      continue;
    }
    const timeoutMs = Math.max(1000, Math.min(CONFIG.providerTimeoutMs, remaining));
    const attemptStart = Date.now();
    try {
      const results = await PROVIDERS[provider]({ query, limit, timeoutMs, signal });
      attempts.push({ provider, ok: true, resultCount: results.length, ms: Date.now() - attemptStart });
      return {
        query,
        provider,
        results,
        resultCount: results.length,
        attempts,
        degraded: attempts.some((attempt) => !attempt.ok),
        fetchedAt: new Date().toISOString(),
      };
    } catch (error) {
      const failure =
        error instanceof ProviderError
          ? { code: error.code, message: error.message, ...error.extra }
          : (() => {
              const classified = classifyFetchError(provider, "", error);
              return { code: classified.code, message: classified.message, ...classified.extra };
            })();
      attempts.push({
        provider,
        ok: false,
        resultCount: 0,
        ms: Date.now() - attemptStart,
        error: {
          ...failure,
          code: failure.code,
          message: failure.message,
        },
      });
      if (error?.name === "AbortError") {
        throw error; // caller cancellation must not be swallowed
      }
    }
  }
  return {
    query,
    provider: null,
    results: [],
    resultCount: 0,
    attempts,
    degraded: true,
    fetchedAt: new Date().toISOString(),
  };
}

function renderText(payload) {
  if (!payload.provider) {
    const lines = [`web search failed for ${JSON.stringify(payload.query)}: no provider returned results.`];
    for (const attempt of payload.attempts) {
      lines.push(`- ${attempt.provider}: ${attempt.error?.code || "failed"} — ${attempt.error?.message || "no results"}`);
    }
    lines.push("This is an upstream/network failure reported as-is; nothing was fabricated.");
    return lines.join("\n");
  }
  const lines = [
    `${payload.resultCount} result(s) for ${JSON.stringify(payload.query)} via ${payload.provider}` +
      (payload.degraded ? " (fallback chain: earlier providers failed, see attempts)" : ""),
  ];
  for (const result of payload.results) {
    lines.push(`${result.rank}. ${result.title} — ${result.url}`);
    if (result.snippet) lines.push(`   ${result.snippet}`);
  }
  return lines.join("\n");
}

async function callTool(name, args, signal) {
  if (name !== TOOL_NAME) {
    const error = new Error(`no such tool: ${name}`);
    error.code = -32602;
    throw error;
  }
  let payload;
  try {
    payload = await runSearch(args, signal);
  } catch (error) {
    if (error?.code === -32602) throw error;
    payload = {
      query: typeof args?.query === "string" ? args.query : "",
      provider: null,
      results: [],
      resultCount: 0,
      attempts: [],
      degraded: true,
      fetchedAt: new Date().toISOString(),
      error: { code: "internal_error", message: error?.message || String(error) },
    };
  }
  if (payload.provider === null) {
    return {
      content: [{ type: "text", text: renderText(payload) }],
      structuredContent: payload,
      isError: true,
    };
  }
  return {
    content: [{ type: "text", text: renderText(payload) }],
    structuredContent: payload,
  };
}

// --- protocol loop ---------------------------------------------------------

const inflight = new Map(); // request id -> AbortController
let pending = 0;
let closing = false;

// EOF drain: a standalone run (`... | node sidecar.mjs`) closes stdin right
// after the last frame, so an in-flight search needs a bounded grace before the
// orphan-defence exit. The host keeps stdin open during a call, and its unload
// path cancels in-flight requests first, so this grace never delays a teardown.
const EOF_DRAIN_MS = 8_000;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function writeFrame(message) {
  if (!process.stdout.writable) return false;
  process.stdout.write(`${JSON.stringify(message)}\n`);
  return true;
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
      "Use the search tool to look things up on the live web. The result carries provider and attempt records; a total network failure is an error result, never an empty success.",
  };
}

function toolsList() {
  return {
    tools: [
      {
        name: TOOL_NAME,
        title: "Web search",
        description:
          "Search the live web and return structured results (title, URL, snippet, provider). Falls back across Bing, DuckDuckGo, Hacker News, GitHub and npm; reports upstream failures verbatim.",
        inputSchema: TOOL_SCHEMA,
      },
    ],
  };
}

async function handleRequest(message) {
  pending += 1;
  try {
    await routeRequest(message);
  } finally {
    pending -= 1;
  }
}

async function routeRequest(message) {
  const { id, method, params } = message;
  if (method === "initialize") {
    reply(id, initializeResult());
    return;
  }
  if (method === "tools/list") {
    reply(id, toolsList());
    return;
  }
  if (method === "tools/call") {
    const call = params && typeof params === "object" ? params : {};
    if (typeof call.name !== "string" || !call.name) {
      replyError(id, -32602, "tools/call params.name is required");
      return;
    }
    const controller = new AbortController();
    inflight.set(idKey(id), controller);
    try {
      const result = await callTool(call.name, call.arguments ?? {}, controller.signal);
      reply(id, result);
    } catch (error) {
      if (error?.code === -32602) replyError(id, -32602, error.message);
      else if (error?.name === "AbortError") {
        // The host cancels the request and discards the answer; still answer so
        // the frame is not mistaken for a protocol violation.
        replyError(id, -32002, "the request was canceled");
      } else replyError(id, -32603, error?.message || String(error));
    } finally {
      inflight.delete(idKey(id));
    }
    return;
  }
  replyError(id, -32601, `no such method: ${method}`);
}

function idKey(id) {
  return typeof id === "string" ? `s:${id}` : `n:${JSON.stringify(id)}`;
}

function handleNotification(message) {
  if (message.method === "notifications/cancelled") {
    const target = message.params?.requestId;
    const controller = inflight.get(idKey(target));
    if (controller) controller.abort();
    return;
  }
  if (message.method === "close") {
    closing = true;
    // Give the last frame a chance to flush, then exit on our own so the host
    // does not need the SIGTERM rung of the ladder.
    setTimeout(() => process.exit(0), 20).unref?.();
  }
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
  // stdin EOF: the first orphan defence of the host's ladder. Give an in-flight
  // call a bounded grace (standalone pipes close stdin immediately), then abort
  // whatever remains and exit on our own.
  const deadline = Date.now() + EOF_DRAIN_MS;
  while (pending > 0 && Date.now() < deadline) await sleep(25);
  for (const controller of inflight.values()) controller.abort();
  await sleep(25);
  process.exit(0);
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
  handleRequest(message).catch((error) => {
    log("request failed:", error?.stack || String(error));
    replyError(message.id, -32603, error?.message || String(error));
  });
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
