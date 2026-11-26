#!/usr/bin/env node
// Deterministic OpenAI Chat Completions provider for the live GitHub
// super-agent E2E. It drives one worker session through the real model-tool
// path (`ask` then `report waiting`, and `report ready` after the human
// answer) so the E2E proves the 4.5 model tools, not only the HTTP routes.
//
// It is test infrastructure: it never reads a credential, and every request is
// summarised into provider.log (roles, tool names, bounded text) for evidence.
//
//   node provider.mjs <port> <logfile>
import http from 'node:http';
import fs from 'node:fs';

const PORT = Number(process.argv[2] || process.env.LIVE_MODEL_PORT || 13712);
const LOG = process.argv[3] || '';

function log(line) {
  if (!LOG) return;
  try {
    fs.appendFileSync(LOG, line + '\n');
  } catch {
    /* evidence logging must never break the provider */
  }
}

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

function toolCallChunk(model, id, name, args) {
  return {
    id: 'chatcmpl-live-e2e',
    object: 'chat.completion.chunk',
    model,
    choices: [
      {
        index: 0,
        delta: {
          role: 'assistant',
          tool_calls: [
            {
              index: 0,
              id,
              type: 'function',
              function: { name, arguments: JSON.stringify(args) },
            },
          ],
        },
        finish_reason: null,
      },
    ],
  };
}

function textChunk(model, text) {
  return {
    id: 'chatcmpl-live-e2e',
    object: 'chat.completion.chunk',
    model,
    choices: [{ index: 0, delta: { role: 'assistant', content: text }, finish_reason: null }],
  };
}

function finishChunk(model, reason, promptTokens) {
  return {
    id: 'chatcmpl-live-e2e',
    object: 'chat.completion.chunk',
    model,
    choices: [{ index: 0, delta: {}, finish_reason: reason }],
    usage: {
      prompt_tokens: promptTokens,
      completion_tokens: 20,
      total_tokens: promptTokens + 20,
    },
  };
}

const sse = (res, chunks) => {
  const body = chunks.map((chunk) => 'data: ' + JSON.stringify(chunk) + '\n\n').join('') + 'data: [DONE]\n\n';
  res.writeHead(200, {
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-cache',
    'Content-Length': Buffer.byteLength(body),
  });
  res.end(body);
};

// toolNameOf returns the name of the most recent assistant tool call, which is
// what the last tool message answers.
function lastToolName(messages) {
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    const message = messages[index];
    if (message?.role !== 'assistant') continue;
    const calls = Array.isArray(message.tool_calls) ? message.tool_calls : [];
    if (calls.length > 0) return String(calls[calls.length - 1]?.function?.name || '');
  }
  return '';
}

function anyToolName(messages, name) {
  return messages.some(
    (message) =>
      message?.role === 'assistant' &&
      Array.isArray(message.tool_calls) &&
      message.tool_calls.some((call) => call?.function?.name === name),
  );
}

function fieldOf(raw, field) {
  if (typeof raw !== 'string') return '';
  try {
    const parsed = JSON.parse(raw);
    return typeof parsed?.[field] === 'string' ? parsed[field] : '';
  } catch {
    const match = new RegExp('"' + field + '"\\s*:\\s*"([^"]+)"').exec(raw);
    return match ? match[1] : '';
  }
}

// attentionIDOf reads the ask tool result. The tool nests the Attention under
// "attention" ({"attention":{"id":...},"ticket":{...}}).
function attentionIDOf(raw) {
  if (typeof raw !== 'string') return '';
  try {
    const parsed = JSON.parse(raw);
    const nested = parsed?.attention?.id;
    if (typeof nested === 'string') return nested;
    if (typeof parsed?.attentionId === 'string') return parsed.attentionId;
  } catch {
    /* fall through to the regex */
  }
  const match = /"attention"\s*:\s*\{[^}]*"id"\s*:\s*"([^"]+)"/.exec(raw);
  return match ? match[1] : '';
}

function decide(messages) {
  const last = messages[messages.length - 1] || {};
  const lastText = textOf(last.content);
  const tool = lastToolName(messages);

  if (last.role === 'tool') {
    if (tool === 'ask') {
      const attentionId = attentionIDOf(lastText);
      if (!attentionId) return { kind: 'text', text: 'ask returned no attention id; stopping.' };
      return {
        kind: 'tool',
        name: 'report',
        args: {
          state: 'waiting',
          summary: 'E2E worker asked for deploy approval and is waiting on the human.',
          attentionId,
        },
      };
    }
    if (tool === 'report') {
      const state = fieldOf(lastText, 'state');
      if (state === 'ready') return { kind: 'text', text: 'Work complete; reported ready to the ticket.' };
      if (state === 'waiting') return { kind: 'text', text: 'Waiting for the human answer on the open attention.' };
      return { kind: 'text', text: 'Report acknowledged; stopping.' };
    }
    return { kind: 'text', text: 'Tool result acknowledged.' };
  }

  if (last.role === 'user') {
    if (/Human (answer|selected)/i.test(lastText)) {
      return {
        kind: 'tool',
        name: 'report',
        args: {
          state: 'ready',
          summary: 'E2E worker applied the human decision and finished the ticket.',
          result: { outcome: 'applied', source: 'live-github-e2e' },
        },
      };
    }
    if (!anyToolName(messages, 'ask')) {
      return {
        kind: 'tool',
        name: 'ask',
        args: {
          prompt: 'E2E: approve the simulated deploy for this ticket?',
          kind: 'question',
          options: [
            { id: 'approve', label: 'Approve' },
            { id: 'reject', label: 'Reject' },
          ],
          allowFreeText: true,
        },
      };
    }
  }
  return { kind: 'text', text: 'E2E worker turn complete.' };
}

const server = http.createServer(async (req, res) => {
  if (req.method === 'GET' && req.url?.replace(/\/$/, '').endsWith('/models')) {
    const body = JSON.stringify({ object: 'list', data: [{ id: 'live-e2e-model', object: 'model' }] });
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
  const model = parsed.model || 'live-e2e-model';
  let chars = 0;
  for (const message of messages) chars += textOf(message?.content).length;
  chars += JSON.stringify(parsed.tools || []).length;
  const promptTokens = Math.max(1, Math.ceil(chars / 3.5));
  const choice = decide(messages);
  const roles = messages.map((message) => message?.role).join(',');
  log(
    JSON.stringify({
      at: new Date().toISOString(),
      roles,
      tools: Array.isArray(parsed.tools) ? parsed.tools.map((tool) => tool?.function?.name) : [],
      last: {
        role: messages[messages.length - 1]?.role,
        text: textOf(messages[messages.length - 1]?.content).slice(0, 2000),
        humanAnswer: /Human (answer|selected)/i.test(textOf(messages[messages.length - 1]?.content)),
      },
      choice: choice.kind === 'tool' ? { tool: choice.name, args: choice.args } : { text: choice.text },
    }),
  );
  if (choice.kind === 'tool') {
    sse(res, [
      toolCallChunk(model, 'call_' + choice.name + '_' + Date.now(), choice.name, choice.args),
      finishChunk(model, 'tool_calls', promptTokens),
    ]);
    return;
  }
  sse(res, [textChunk(model, choice.text), finishChunk(model, 'stop', promptTokens)]);
});

server.listen(PORT, '127.0.0.1', () => {
  log(JSON.stringify({ at: new Date().toISOString(), event: 'listening', port: PORT }));
  console.log(`live-github provider listening on 127.0.0.1:${PORT}`);
});
