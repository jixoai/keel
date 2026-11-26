#!/usr/bin/env node
// Deterministic OpenAI Chat Completions SSE provider for the checkpoint E2E.
//
// Two answers: a normal turn and a context summary (recognized by the summary
// assistant's system prompt). The reported prompt_tokens mirror the request
// text with Keel's own heuristic (ceil(chars / 3.5)); the context-usage
// estimator prefers provider usage, so a fixed tiny number would make the
// automatic compaction threshold unreachable.
//
// It is test infrastructure only: `run.sh` starts it on a loopback port and
// `KEEL_MODELS_CONFIG` points the deployment at it. No credential is read.
//
//   node fake-provider.mjs <port>
import http from 'node:http';

const PORT = Number(process.argv[2] || process.env.E2E_MODEL_PORT || 13712);
const NORMAL = 'E2E 回答：已收到本轮输入。';
const SUMMARY = `## Goal
E2E 压缩摘要：继续验证检查点。
## Constraints & Preferences
- (none)
## Progress
### Done
- [x] 自动/手动压缩与重启恢复验证
### In Progress
- [ ] 策略收窄
## Key Decisions
- **checkpoint 是投影**：事件日志保持唯一真值
## Next Steps
1. 继续会话
## Critical Context
- E2E marker CHECKPOINT-E2E`;

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
  if (req.method === 'GET' && req.url?.replace(/\/$/, '').endsWith('/models')) {
    const body = JSON.stringify({ object: 'list', data: [{ id: 'e2e-model', object: 'model' }] });
    res.writeHead(200, { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) });
    res.end(body);
    return;
  }
  if (req.method !== 'POST' || !req.url?.startsWith('/v1/chat/completions')) {
    res.writeHead(404, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: 'not found' }));
    return;
  }
  let parsed = {};
  try {
    parsed = JSON.parse((await readBody(req)) || '{}');
  } catch {
    /* a malformed request still gets a valid empty turn */
  }
  const messages = Array.isArray(parsed.messages) ? parsed.messages : [];
  let system = '';
  let chars = 0;
  for (const message of messages) {
    const text = textOf(message?.content);
    chars += text.length;
    if (message?.role === 'system' && !system) system = text;
  }
  chars += JSON.stringify(parsed.tools || []).length;
  const promptTokens = Math.max(1, Math.ceil(chars / 3.5));
  const text = system.includes('context summarization assistant') ? SUMMARY : NORMAL;
  const model = parsed.model || 'e2e-model';
  sse(
    res,
    {
      id: 'chatcmpl-e2e',
      object: 'chat.completion.chunk',
      model,
      choices: [{ index: 0, delta: { content: text }, finish_reason: null }],
    },
    {
      id: 'chatcmpl-e2e',
      model,
      choices: [{ index: 0, delta: {}, finish_reason: 'stop' }],
      usage: { prompt_tokens: promptTokens, completion_tokens: 10, total_tokens: promptTokens + 10 },
    },
  );
});

server.listen(PORT, '127.0.0.1', () => {
  console.log(`fake-provider listening on 127.0.0.1:${PORT}`);
});
