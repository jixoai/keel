#!/usr/bin/env node
// Deterministic scripted model for the keel-plugin-attachment browser E2E.
//
// It speaks the OpenAI Chat Completions streaming wire (the provider sends
// `stream: true`) and records every request body it receives, so the lane can
// assert what the model really got: a native image part for a PNG and a text
// fallback (blob id + MIME, no bytes) for a PDF the model does not declare.
//
// The model is test infrastructure only: run.sh configures it through
// KEEL_MODELS_CONFIG and it never runs inside the product. No prompt, name or
// byte is printed; the JSONL log stays under the run's evidence directory.
import fs from 'node:fs';
import http from 'node:http';

const PORT = Number(process.env.SCRIPTED_MODEL_PORT || 13745);
const LOG = process.env.SCRIPTED_MODEL_LOG || '';

const log = (entry) => {
  if (!LOG) return;
  try {
    fs.appendFileSync(LOG, JSON.stringify({ at: new Date().toISOString(), ...entry }) + '\n');
  } catch {
    /* diagnostics only */
  }
};

const chunk = (model, delta, finish = '') => ({
  id: `chatcmpl-scripted-${model}`,
  object: 'chat.completion.chunk',
  model,
  choices: [{ index: 0, delta, finish_reason: finish || null }],
});

const say = (res, model, text) => {
  res.writeHead(200, {
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-cache',
    Connection: 'keep-alive',
  });
  for (const part of [
    chunk(model, { role: 'assistant' }),
    chunk(model, { content: text }),
    chunk(model, {}, 'stop'),
    {
      id: `chatcmpl-scripted-${model}`,
      object: 'chat.completion.chunk',
      model,
      choices: [],
      usage: { prompt_tokens: 10, completion_tokens: 5 },
    },
  ]) {
    res.write(`data: ${JSON.stringify(part)}\n\n`);
  }
  res.write('data: [DONE]\n\n');
  res.end();
};

const server = http.createServer((req, res) => {
  if (req.method === 'GET' && req.url === '/health') {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ ok: true }));
    return;
  }
  if (req.method !== 'POST' || !req.url.startsWith('/v1/chat/completions')) {
    res.writeHead(404, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: 'not found' }));
    return;
  }
  let raw = '';
  req.on('data', (data) => {
    raw += data.toString();
  });
  req.on('end', () => {
    let body = {};
    try {
      body = JSON.parse(raw);
    } catch {
      res.writeHead(400, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: 'invalid json' }));
      return;
    }
    const model = String(body.model || 'e2e-attachment');
    log({ kind: 'chat.completions', model, body });
    say(res, model, '已收到附件，正在查看。');
  });
});

server.listen(PORT, '127.0.0.1', () => {
  console.log(`scripted-model: listening on 127.0.0.1:${PORT}`);
});
