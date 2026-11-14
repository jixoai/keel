#!/usr/bin/env node
// Records sanitized provider fixtures from the real configured gateway.
//
// Usage:
//   node scripts/record-provider-fixtures.mjs                 # core fixtures
//   node scripts/record-provider-fixtures.mjs --capabilities  # + video/tts/stt/realtime probes
//
// The gateway URL and key are read from the main repo's .env.personal
// (KEEL_BASE_URL / KEEL_API_KEY). Override the file with KEEL_ENV_FILE.
//
// Nothing written here may contain: the API key, Authorization headers,
// request ids, provider timestamps, or signed asset URLs. The scrubbers below
// enforce that; the script aborts if the key somehow survives.

import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..");
const fixtureRoot = join(repoRoot, "testdata", "provider-fixtures");
const envFile =
  process.env.KEEL_ENV_FILE ||
  "/Users/kzf/Development/GitHub/jixoai/fde2e/.env.personal";

const withCapabilities = process.argv.includes("--capabilities");

function loadEnv(file) {
  const out = {};
  for (const line of readFileSync(file, "utf8").split(/\r?\n/)) {
    const match = /^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*?)\s*$/.exec(line);
    if (!match) continue;
    let value = match[2];
    if (
      (value.startsWith('"') && value.endsWith('"')) ||
      (value.startsWith("'") && value.endsWith("'"))
    ) {
      value = value.slice(1, -1);
    }
    out[match[1]] = value;
  }
  return out;
}

const env = loadEnv(envFile);
const baseUrl = (env.KEEL_BASE_URL || "").replace(/\/+$/, "");
const apiKey = env.KEEL_API_KEY || "";
if (!baseUrl) throw new Error("KEEL_BASE_URL is not set");
if (!apiKey) throw new Error("KEEL_API_KEY is not set");

const REDACTED_KEY = "<redacted-api-key>";
const REDACTED_ID = "<redacted-id>";
const REDACTED_REQUEST_ID = "<redacted-request-id>";
const REDACTED_TIMESTAMP = "<redacted-timestamp>";
const REDACTED_URL = "https://<redacted-asset-host>/...";

// --- scrubbers -------------------------------------------------------------

function scrubString(value) {
  let out = value;
  if (apiKey) out = out.split(apiKey).join(REDACTED_KEY);
  out = out.replace(/Bearer\s+[A-Za-z0-9._~+/=-]+/g, `Bearer ${REDACTED_KEY}`);
  out = out.replace(/\bchatcmpl-[A-Za-z0-9-]+\b/g, REDACTED_ID);
  out = out.replace(
    /\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/gi,
    REDACTED_REQUEST_ID,
  );
  out = out.replace(/https?:\/\/[^\s"'<>\\]+/g, REDACTED_URL);
  return out;
}

function scrubValue(value, key = "") {
  if (Array.isArray(value)) return value.map((item) => scrubValue(item, key));
  if (value && typeof value === "object") {
    const out = {};
    for (const [k, v] of Object.entries(value)) out[k] = scrubValue(v, k);
    return out;
  }
  if (typeof value === "number") {
    if (/^(created|expires|expires_at|expires_in|timestamp)$/i.test(key)) return 0;
    return value;
  }
  if (typeof value !== "string") return value;

  if (/^request[_-]?id$/i.test(key)) return REDACTED_REQUEST_ID;
  if (/^(system_fingerprint|fingerprint)$/i.test(key)) return REDACTED_ID;
  if (/^(created_at|updated_at|expires_at)$/i.test(key)) return REDACTED_TIMESTAMP;
  if (key === "id" && /^(chatcmpl-|resp-|req-|[0-9a-f]{8}-[0-9a-f-]{8,})/i.test(value)) {
    return REDACTED_ID;
  }
  return scrubString(value);
}

function scrubText(text, label) {
  let out = scrubString(text);
  if (apiKey && out.includes(apiKey)) {
    throw new Error(`refusing to write ${label}: API key survived scrubbing`);
  }
  if (out.includes("Authorization:")) {
    throw new Error(`refusing to write ${label}: authorization header survived scrubbing`);
  }
  return out;
}

function writeFixture(relativePath, content) {
  const target = join(fixtureRoot, relativePath);
  mkdirSync(dirname(target), { recursive: true });
  const body =
    typeof content === "string" ? content : JSON.stringify(content, null, 2);
  const text = scrubText(body.endsWith("\n") ? body : `${body}\n`, relativePath);
  writeFileSync(target, text);
  console.log(`wrote ${relativePath} (${Buffer.byteLength(text)} bytes)`);
  return text;
}

// --- HTTP ------------------------------------------------------------------

async function request(method, path, body) {
  const headers = { Authorization: `Bearer ${apiKey}` };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(`${baseUrl}${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  return {
    status: res.status,
    contentType: res.headers.get("content-type") || "",
    text,
  };
}

function parseJsonSafe(text) {
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

function errorEnvelope({ method, path, body, status, contentType, text }) {
  const parsed = parseJsonSafe(text);
  return {
    request: { method, path, body: scrubValue(body) },
    response: {
      status,
      contentType,
      body: parsed === null ? text : scrubValue(parsed),
    },
  };
}

function sanitizeSse(raw) {
  const lines = raw.split(/\r?\n/).filter((line) => line.length > 0);
  const out = [];
  for (const line of lines) {
    if (line.startsWith("data:")) {
      const payload = line.slice("data:".length).trim();
      if (payload === "[DONE]") {
        out.push("data: [DONE]");
        continue;
      }
      const parsed = parseJsonSafe(payload);
      out.push(
        parsed === null
          ? `data: ${scrubString(payload)}`
          : `data: ${JSON.stringify(scrubValue(parsed))}`,
      );
      continue;
    }
    out.push(scrubString(line));
  }
  return `${out.join("\n")}\n`;
}

// --- fixtures --------------------------------------------------------------

const statuses = [];

async function recordCore() {
  // 1. LLM chat (non-streaming)
  {
    const method = "POST";
    const path = "/chat/completions";
    const body = {
      model: "bailian-token-plan/qwen3.8-flash",
      messages: [{ role: "user", content: "用一句话介绍你自己" }],
      max_tokens: 64,
    };
    const res = await request(method, path, body);
    statuses.push({ fixture: "llm/chat-qwen38-flash.json", method, path, status: res.status });
    const parsed = parseJsonSafe(res.text);
    if (parsed === null) {
      writeFixture(
        "llm/chat-qwen38-flash.json",
        errorEnvelope({ method, path, body, ...res }),
      );
    } else {
      writeFixture("llm/chat-qwen38-flash.json", scrubValue(parsed));
    }
  }

  // 2. LLM chat (streaming SSE)
  {
    const method = "POST";
    const path = "/chat/completions";
    const body = {
      model: "bailian-token-plan/qwen3.8-flash",
      messages: [{ role: "user", content: "用一句话介绍你自己" }],
      max_tokens: 64,
      stream: true,
    };
    const res = await request(method, path, body);
    statuses.push({ fixture: "llm/chat-qwen38-flash.stream.txt", method, path, status: res.status });
    writeFixture("llm/chat-qwen38-flash.stream.txt", sanitizeSse(res.text));
  }

  // 3. Image generation
  {
    const method = "POST";
    const path = "/images/generations";
    const body = {
      model: "bailian-token-plan/wan2.7-image",
      prompt: "极简玻璃信鸽，深蓝背景，产品渲染",
      size: "1024x1024",
      n: 1,
    };
    const res = await request(method, path, body);
    statuses.push({ fixture: "image/generate-wan27.json", method, path, status: res.status });
    const parsed = parseJsonSafe(res.text);
    if (parsed === null) {
      writeFixture(
        "image/generate-wan27.json",
        errorEnvelope({ method, path, body, ...res }),
      );
    } else {
      writeFixture("image/generate-wan27.json", scrubValue(parsed));
    }
  }

  // 4. Decision: choice
  {
    const method = "POST";
    const path = "/decisions";
    const body = {
      model: "bailian-token-plan/decision-model-preview",
      input: "Pick A or B, answer one letter.",
      questions: [
        {
          type: "choice",
          predicate: "the better option",
          choices: [{ value: "A" }, { value: "B" }],
        },
      ],
    };
    const res = await request(method, path, body);
    statuses.push({ fixture: "decision/choice.json", method, path, status: res.status });
    const parsed = parseJsonSafe(res.text);
    writeFixture(
      "decision/choice.json",
      parsed === null
        ? errorEnvelope({ method, path, body, ...res })
        : scrubValue(parsed),
    );
  }

  // 5. Decision: predicate
  {
    const method = "POST";
    const path = "/decisions";
    const body = {
      model: "bailian-token-plan/decision-model-preview",
      input: "Pick A or B, answer one letter.",
      questions: [{ type: "predicate", predicate: "the message is urgent" }],
    };
    const res = await request(method, path, body);
    statuses.push({ fixture: "decision/predicate.json", method, path, status: res.status });
    const parsed = parseJsonSafe(res.text);
    writeFixture(
      "decision/predicate.json",
      parsed === null
        ? errorEnvelope({ method, path, body, ...res })
        : scrubValue(parsed),
    );
  }

  // 6. Decision: score
  // 6a. Requested shape (criteria) is rejected by the provider.
  {
    const method = "POST";
    const path = "/decisions";
    const body = {
      model: "bailian-token-plan/decision-model-preview",
      input: "Rate the quality of this message: hello.",
      questions: [
        {
          type: "score",
          predicate: "quality",
          criteria: [
            { value: 1, description: "bad" },
            { value: 5, description: "great" },
          ],
        },
      ],
    };
    const res = await request(method, path, body);
    statuses.push({ fixture: "decision/score-error.json", method, path, status: res.status });
    writeFixture(
      "decision/score-error.json",
      errorEnvelope({ method, path, body, ...res }),
    );
  }

  // 6b. Provider hint: levels must be objects, not strings.
  {
    const method = "POST";
    const path = "/decisions";
    const body = {
      model: "bailian-token-plan/decision-model-preview",
      input: "Rate the quality of this message: hello.",
      questions: [
        { type: "score", predicate: "quality", levels: ["bad", "great"] },
      ],
    };
    const res = await request(method, path, body);
    statuses.push({ fixture: "decision/score-hint.json", method, path, status: res.status });
    writeFixture(
      "decision/score-hint.json",
      errorEnvelope({ method, path, body, ...res }),
    );
  }

  // 6c. Working shape: levels = [{label, description}].
  {
    const method = "POST";
    const path = "/decisions";
    const body = {
      model: "bailian-token-plan/decision-model-preview",
      input: "Rate the quality of this message: hello.",
      questions: [
        {
          type: "score",
          predicate: "quality",
          levels: [
            { label: "bad", description: "bad" },
            { label: "great", description: "great" },
          ],
        },
      ],
    };
    const res = await request(method, path, body);
    statuses.push({ fixture: "decision/score.json", method, path, status: res.status });
    const parsed = parseJsonSafe(res.text);
    writeFixture(
      "decision/score.json",
      parsed === null
        ? errorEnvelope({ method, path, body, ...res })
        : scrubValue(parsed),
    );
  }

  // 7. Models catalog projection.
  {
    const method = "GET";
    const path = "/models";
    const res = await request(method, path);
    statuses.push({ fixture: "models/catalog.json", method, path, status: res.status });
    const parsed = parseJsonSafe(res.text);
    if (parsed === null || !Array.isArray(parsed.data)) {
      writeFixture(
        "models/catalog.json",
        errorEnvelope({ method, path, body: undefined, ...res }),
      );
      return { catalog: null };
    }
    const data = parsed.data.map((model) => {
      const out = { id: model.id };
      if (model.type !== undefined) out.type = model.type;
      if (model.kind !== undefined) out.kind = model.kind;
      if (model.modalities !== undefined) out.modalities = model.modalities;
      if (model.native_endpoints !== undefined) out.native_endpoints = model.native_endpoints;
      return out;
    });
    writeFixture("models/catalog.json", {
      object: parsed.object || "list",
      data: scrubValue(data),
    });
    return { catalog: { raw: parsed, projected: data } };
  }
}

async function recordCapabilities() {
  // Video generation (exact route is not advertised; OpenAI-style probe).
  {
    const method = "POST";
    const path = "/video/generations";
    const body = {
      model: "bailian-token-plan/wan3.0-video",
      prompt: "a glass dove",
    };
    const res = await request(method, path, body);
    statuses.push({ fixture: "capabilities/video-generate.json", method, path, status: res.status });
    writeFixture(
      "capabilities/video-generate.json",
      errorEnvelope({ method, path, body, ...res }),
    );
  }

  // Video model exposed on the chat endpoint rejects text-only prompts.
  {
    const method = "POST";
    const path = "/chat/completions";
    const body = {
      model: "bailian-token-plan/wan3.0-video",
      messages: [{ role: "user", content: "a glass dove" }],
      max_tokens: 64,
    };
    const res = await request(method, path, body);
    statuses.push({ fixture: "capabilities/video-chat.json", method, path, status: res.status });
    writeFixture(
      "capabilities/video-chat.json",
      errorEnvelope({ method, path, body, ...res }),
    );
  }

  // Text to speech.
  {
    const method = "POST";
    const path = "/audio/speech";
    const body = { model: "bailian-token-plan/tts-1", input: "hello", voice: "alloy" };
    const res = await request(method, path, body);
    statuses.push({ fixture: "capabilities/tts.json", method, path, status: res.status });
    writeFixture(
      "capabilities/tts.json",
      errorEnvelope({ method, path, body, ...res }),
    );
  }

  // Speech to text, JSON body: provider asks for multipart.
  {
    const method = "POST";
    const path = "/audio/transcriptions";
    const body = { model: "bailian-token-plan/stt-1" };
    const res = await request(method, path, body);
    statuses.push({ fixture: "capabilities/stt-json.json", method, path, status: res.status });
    writeFixture(
      "capabilities/stt-json.json",
      errorEnvelope({ method, path, body, ...res }),
    );
  }

  // Speech to text, multipart with a real (tiny, silent) WAV.
  {
    const method = "POST";
    const path = "/audio/transcriptions";
    const wav = tinySilentWav();
    const form = new FormData();
    form.set("model", "bailian-token-plan/paraformer-v2");
    form.set("file", new Blob([wav], { type: "audio/wav" }), "silence.wav");
    const res = await fetch(`${baseUrl}${path}`, {
      method,
      headers: { Authorization: `Bearer ${apiKey}` },
      body: form,
    });
    const text = await res.text();
    const captured = {
      status: res.status,
      contentType: res.headers.get("content-type") || "",
      text,
    };
    statuses.push({ fixture: "capabilities/stt-multipart.json", method, path, status: captured.status });
    writeFixture(
      "capabilities/stt-multipart.json",
      errorEnvelope({
        method,
        path,
        body: { model: "bailian-token-plan/paraformer-v2", file: "<tiny-silent-wav>" },
        ...captured,
      }),
    );
  }

  // Realtime sessions.
  {
    const method = "POST";
    const path = "/realtime/sessions";
    const body = { model: "bailian-token-plan/qwen3.8-flash" };
    const res = await request(method, path, body);
    statuses.push({ fixture: "capabilities/realtime.json", method, path, status: res.status });
    writeFixture(
      "capabilities/realtime.json",
      errorEnvelope({ method, path, body, ...res }),
    );
  }
}

function tinySilentWav() {
  const rate = 16000;
  const seconds = 0.2;
  const dataLength = Math.floor(rate * seconds) * 2;
  const buffer = Buffer.alloc(44 + dataLength);
  buffer.write("RIFF", 0);
  buffer.writeUInt32LE(36 + dataLength, 4);
  buffer.write("WAVE", 8);
  buffer.write("fmt ", 12);
  buffer.writeUInt32LE(16, 16);
  buffer.writeUInt16LE(1, 20); // PCM
  buffer.writeUInt16LE(1, 22); // mono
  buffer.writeUInt32LE(rate, 24);
  buffer.writeUInt32LE(rate * 2, 28);
  buffer.writeUInt16LE(2, 32);
  buffer.writeUInt16LE(16, 34);
  buffer.write("data", 36);
  buffer.writeUInt32LE(dataLength, 40);
  return buffer;
}

// --- capability analysis ---------------------------------------------------

function analyzeCatalog(catalog) {
  if (!catalog) {
    return {
      modelCount: 0,
      kindVideo: [],
      outputVideo: [],
      outputAudio: [],
      audioInputOnly: [],
      videoNamed: [],
      mediaNamed: [],
      note: "catalog was not a model list",
    };
  }
  const models = catalog.raw.data;
  const withKind = models.filter((model) => model.kind !== undefined);
  return {
    modelCount: models.length,
    kinds: [...new Set(withKind.map((model) => model.kind))],
    kindVideo: models.filter((model) => model.kind === "video").map((model) => model.id),
    outputVideo: models
      .filter((model) => (model.modalities?.output || []).includes("video"))
      .map((model) => model.id),
    outputAudio: models
      .filter((model) => {
        const output = model.modalities?.output || [];
        return output.includes("audio") || output.includes("speech");
      })
      .map((model) => model.id),
    audioInputOnly: models
      .filter((model) => (model.modalities?.input || []).includes("audio"))
      .map((model) => model.id),
    videoInputOnly: models
      .filter((model) => (model.modalities?.input || []).includes("video"))
      .map((model) => model.id),
    videoNamed: models.filter((model) => /video|wan\d/i.test(model.id)).map((model) => model.id),
    mediaNamed: models
      .filter((model) => /tts|speech|audio|realtime|transcri|whisper|asr|embed|rerank/i.test(model.id))
      .map((model) => model.id),
  };
}

function capabilityFindings(analysis) {
  const lines = [];
  lines.push(`- \`/models\` returned **${analysis.modelCount}** entries.`);
  lines.push(
    `- \`kind\` field: ${
      analysis.kinds && analysis.kinds.length
        ? `present with values ${analysis.kinds.map((k) => `\`${k}\``).join(", ")}`
        : "**absent from every entry**"
    }. Models with \`kind=video\`: ${analysis.kindVideo.length ? analysis.kindVideo.join(", ") : "**none**"}.`,
  );
  lines.push(
    `- Models with \`modalities.output\` containing video: ${
      analysis.outputVideo.length ? analysis.outputVideo.join(", ") : "**none**"
    }; audio/speech output: ${
      analysis.outputAudio.length ? analysis.outputAudio.join(", ") : "**none**"
    }.`,
  );
  lines.push(
    `- Video-named ids: ${
      analysis.videoNamed.length ? analysis.videoNamed.map((id) => `\`${id}\``).join(", ") : "none"
    } (their \`native_endpoints\` are chat/messages only, not a video API).`,
  );
  lines.push(
    `- Audio/TTS/STT/realtime/embedding-named ids: ${
      analysis.mediaNamed.length ? analysis.mediaNamed.map((id) => `\`${id}\``).join(", ") : "**none**"
    }.`,
  );
  if (analysis.videoInputOnly?.length) {
    const examples = analysis.videoInputOnly
      .slice(0, 3)
      .map((id) => `\`${id}\``)
      .join(", ");
    lines.push(
      `- Chat models that accept \`video\` **input** (understanding, not generation): ${analysis.videoInputOnly.length} in total (e.g. ${examples}).`,
    );
  }
  if (analysis.audioInputOnly?.length) {
    lines.push(
      `- Chat models that accept \`audio\` **input** (not speech synthesis): ${analysis.audioInputOnly
        .map((id) => `\`${id}\``)
        .join(", ")}.`,
    );
  }
  return lines.join("\n");
}

function readFixtureText(relativePath) {
  return readFileSync(join(fixtureRoot, relativePath), "utf8");
}

function summarizeProbe(relativePath) {
  const parsed = JSON.parse(readFixtureText(relativePath));
  const body = parsed.response?.body;
  const message =
    typeof body === "string"
      ? body
      : body?.error?.message || body?.message || JSON.stringify(body);
  return { status: parsed.response?.status, message };
}

// --- README ----------------------------------------------------------------

function buildReadme(analysis) {
  const capabilitySummary = capabilityFindings(analysis);
  const probe = (rel) => {
    try {
      const s = summarizeProbe(rel);
      return `- \`${rel}\` — HTTP ${s.status}: \`${s.message.replace(/\n/g, " ")}\``;
    } catch {
      return `- \`${rel}\` — not recorded in this run (pass \`--capabilities\`)`;
    }
  };

  return `# Provider fixtures (recorded from the real gateway)

These fixtures were captured from the real configured provider
(\`KEEL_BASE_URL\`, model-prefix \`bailian-token-plan\`) by
\`scripts/record-provider-fixtures.mjs\`. They are meant to be replayed by mock
tests; no network is needed to consume them.

## Redaction policy

Nothing in this directory contains the API key or an Authorization header.
Also removed/replaced:

- request ids (\`request_id\`, \`chatcmpl-*\` ids) → \`<redacted-id>\` / \`<redacted-request-id>\`
- provider timestamps (\`created\`, \`created_at\`, asset expiry) → \`0\` / \`<redacted-timestamp>\`
- signed asset URLs → \`https://<redacted-asset-host>/...\` (query strings are dropped entirely)

## Replay

All calls use bearer auth sourced from \`KEEL_API_KEY\` against \`KEEL_BASE_URL\`
(no \`/v1\` prefix in this deployment). Do not commit the key.

| Fixture | Request | Expected status |
| --- | --- | --- |
| \`llm/chat-qwen38-flash.json\` | \`POST /chat/completions\` \`{"model":"bailian-token-plan/qwen3.8-flash","messages":[{"role":"user","content":"用一句话介绍你自己"}],"max_tokens":64}\` | 200 |
| \`llm/chat-qwen38-flash.stream.txt\` | same body plus \`"stream":true\`; response is SSE (\`data: {...}\` lines, terminated by \`data: [DONE]\`) | 200 |
| \`image/generate-wan27.json\` | \`POST /images/generations\` \`{"model":"bailian-token-plan/wan2.7-image","prompt":"极简玻璃信鸽，深蓝背景，产品渲染","size":"1024x1024","n":1}\` | 200 |
| \`decision/choice.json\` | \`POST /decisions\` \`{"model":"bailian-token-plan/decision-model-preview","input":"Pick A or B, answer one letter.","questions":[{"type":"choice","predicate":"the better option","choices":[{"value":"A"},{"value":"B"}]}]}\` | 200 |
| \`decision/predicate.json\` | \`POST /decisions\` same input, \`questions:[{"type":"predicate","predicate":"the message is urgent"}]\` | 200 |
| \`decision/score-error.json\` | requested score shape with \`criteria:[{value,description}]\` | **400** (recorded deliberately) |
| \`decision/score-hint.json\` | \`levels:["bad","great"]\` (strings) | **400**; the message reveals the required struct |
| \`decision/score.json\` | \`levels:[{"label":"bad","description":"bad"},{"label":"great","description":"great"}]\` | 200 |
| \`models/catalog.json\` | \`GET /models\` projected to \`id\`/\`type\`/\`kind\`/\`modalities\`/\`native_endpoints\` (no pricing) | 200 |
| \`capabilities/*.json\` | see below | 401/404/400 |

### Decision score schema

The requested \`criteria\` field is **not** the provider's schema. The provider
returns:

\`\`\`
HTTP 400: questions.question_1.criteria: 0 score levels outside the allowed 1..255
\`\`\`

The accepted shape is \`levels\`, an array of objects with \`label\` and
\`description\` (string levels are rejected with
\`cannot unmarshal string into Go struct field question.levels of type struct { Label string; Description string }\`).
Response probabilities echo the supplied labels and use zero-based \`value\`
indices.

## Capability findings for this token plan

${capabilitySummary}

### Probe results (exact errors, sanitized)

${probe("capabilities/video-generate.json")}

${probe("capabilities/video-chat.json")}

${probe("capabilities/tts.json")}

${probe("capabilities/stt-json.json")}

${probe("capabilities/stt-multipart.json")}

${probe("capabilities/realtime.json")}

Conclusion: **video generation, TTS, STT and realtime are not served by this
token plan**. The gateway's \`/models\` list advertises no such models, and the
dedicated routes either 404 upstream ("Bailian Token Plan may not serve this
API") or 401 against the gateway's browser-cookie fallback. The only
video-named id, \`bailian-token-plan/wan3.0-video\`, declares chat/messages
native endpoints only; a text-only chat call to it returns HTTP 400
(\`Bailian Token Plan: url error, please check url！\`), so there is no
text-to-video generation path.

Image generation **is** served (\`/images/generations\`, HTTP 200), even though
\`wan2.7-image\` is not listed in \`/models\`.

## Regenerate

\`\`\`sh
node scripts/record-provider-fixtures.mjs                # core fixtures
node scripts/record-provider-fixtures.mjs --capabilities # + capability probes
\`\`\`

The script reads the main repo \`.env.personal\` by default; override with
\`KEEL_ENV_FILE\`. It never prints the key and refuses to write any fixture that
still contains it.
`;
}

// --- main ------------------------------------------------------------------

const { catalog } = await recordCore();
if (withCapabilities) await recordCapabilities();

const analysis = analyzeCatalog(catalog);
writeFixture("README.md", buildReadme(analysis));

console.log("\nHTTP statuses:");
for (const row of statuses) {
  console.log(`  ${row.status} ${row.method} ${row.path} -> ${row.fixture}`);
}
console.log("\nCapability summary:");
console.log(capabilityFindings(analysis));
if (withCapabilities) {
  for (const rel of [
    "capabilities/video-generate.json",
    "capabilities/video-chat.json",
    "capabilities/tts.json",
    "capabilities/stt-json.json",
    "capabilities/stt-multipart.json",
    "capabilities/realtime.json",
  ]) {
    const s = summarizeProbe(rel);
    console.log(`  ${rel}: HTTP ${s.status} — ${s.message.replace(/\n/g, " ")}`);
  }
}
