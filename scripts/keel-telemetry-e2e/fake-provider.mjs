#!/usr/bin/env node
// Deterministic OpenAI Chat Completions provider for the keel.telemetry live
// E2E. The shape follows scripts/keel-checkpoint-e2e/fake-provider.mjs; this
// copy adds a /health probe and a request journal so the driver can prove the
// real run path called the model.
//
//   MODEL_LOG=/path/model-requests.jsonl node fake-provider.mjs <port>
//
// The journal records method/path/model/message count and whether an
// Authorization header arrived. It never records a header value or a message
// body. No credential is read or checked.
import fs from 'node:fs';
import http from 'node:http';

const PORT = Number(process.argv[2] || process.env.E2E_MODEL_PORT || 13712);
const LOG = process.env.MODEL_LOG || '';
if (!LOG) {
  console.error('MODEL_LOG is required');
  process.exit(2);
}
fs.writeFileSync(LOG, '');

const REPLY = 'E2E telemetry reply: the scripted model received this turn.';
let requests = 0;

const textOf = (content) => {
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    return content.map((part) => (typeof part === 'string' ? part : part?.text || '')).join('');
  }
  return '';
};

const readBody = (req) =>
  new Promise((resolve) => {
    let raw = '';
    req.on('data', (chunk) => {
      raw += chunk.toString();
    });
    req.on('end', () => resolve(raw));
  });

const record = (entry) => {
  requests += 1;
  fs.appendFileSync(LOG, JSON.stringify({ seq: requests, receivedAt: new Date().toISOString(), ...entry }) + '\n');
};

const json = (res, status, value) => {
  const body = JSON.stringify(value);
  res.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) });
  res.end(body);
};

const sse = (res, chunk, done) => {
  const body =
    'data: ' + JSON.stringify(chunk) + '\n\n' +
    'data: ' + JSON.stringify(done) + '\n\n' +
    'data: [DONE]\n\n';
  res.writeHead(200, {
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-cache',
    'Content-Length': Buffer.byteLength(body),
  });
  res.end(body);
};

const server = http.createServer(async (req, res) => {
  const path = (req.url || '').split('?')[0];
  if (req.method === 'GET' && path === '/health') {
    json(res, 200, { ok: true, requests, log: LOG });
    return;
  }
  if (req.method === 'GET' && path.replace(/\/$/, '').endsWith('/models')) {
    record({ method: req.method, path, authorization: Boolean(req.headers.authorization) });
    json(res, 200, { object: 'list', data: [{ id: 'e2e-model', object: 'model' }] });
    return;
  }
  if (req.method !== 'POST' || path !== '/v1/chat/completions') {
    json(res, 404, { error: 'not found' });
    return;
  }
  let parsed = {};
  try {
    parsed = JSON.parse((await readBody(req)) || '{}');
  } catch {
    /* a malformed request still gets a valid empty turn */
  }
  const messages = Array.isArray(parsed.messages) ? parsed.messages : [];
  record({
    method: req.method,
    path,
    model: parsed.model || 'e2e-model',
    stream: parsed.stream !== false,
    messageCount: messages.length,
    toolCount: Array.isArray(parsed.tools) ? parsed.tools.length : 0,
    authorization: Boolean(req.headers.authorization),
  });
  const model = parsed.model || 'e2e-model';
  if (parsed.stream === false) {
    json(res, 200, {
      id: 'chatcmpl-e2e',
      object: 'chat.completion',
      model,
      choices: [{ index: 0, message: { role: 'assistant', content: REPLY }, finish_reason: 'stop' }],
      usage: { prompt_tokens: 32, completion_tokens: 12, total_tokens: 44 },
    });
    return;
  }
  sse(
    res,
    {
      id: 'chatcmpl-e2e',
      object: 'chat.completion.chunk',
      model,
      choices: [{ index: 0, delta: { content: REPLY }, finish_reason: null }],
    },
    {
      id: 'chatcmpl-e2e',
      model,
      choices: [{ index: 0, delta: {}, finish_reason: 'stop' }],
      usage: { prompt_tokens: 32, completion_tokens: 12, total_tokens: 44 },
    },
  );
});

server.listen(PORT, '127.0.0.1', () => {
  console.log(`fake provider listening on 127.0.0.1:${PORT} (evidence ${LOG})`);
});

function shutdown(signal) {
  console.log(`[provider] ${signal}; stopping with ${requests} request(s) recorded`);
  server.close(() => process.exit(0));
  setTimeout(() => process.exit(0), 1500).unref();
}
process.on('SIGTERM', () => shutdown('SIGTERM'));
process.on('SIGINT', () => shutdown('SIGINT'));
