#!/usr/bin/env node
// Deterministic scripted model for the A6 TeamBot delegation lane.
//
// The deployment's built-in fake provider only emits `echo` / `mcp` calls, so a
// captain can never exercise the `agent` spawn tool. This stub speaks the
// OpenAI Chat Completions streaming wire (the format `openai-chat-completions`
// providers use) and emits exactly one `agent` tool call for a captain model
// when the last user message asks to start/assemble the team.
//
// It is test infrastructure: it never runs in the product and is configured
// only through `KEEL_MODELS_CONFIG` in the isolated E2E stack.
//
// Usage (run.sh starts it):
//   SCRIPTED_MODEL_PORT=13793 node scripted-model.mjs
//
// Env:
//   SCRIPTED_MODEL_PORT       listen port (default 13793)
//   SCRIPTED_MODEL_LOG        JSONL request log (optional)
//   SCRIPTED_MODEL_MEMBER_TASK default child task

import http from 'node:http';
import fs from 'node:fs';

const PORT = Number(process.env.SCRIPTED_MODEL_PORT || 13793);
const LOG = process.env.SCRIPTED_MODEL_LOG || '';
const MEMBER_TASK =
  process.env.SCRIPTED_MODEL_MEMBER_TASK || '请处理一位客户问题并给出结论。';

const log = (entry) => {
  if (!LOG) return;
  try {
    fs.appendFileSync(LOG, JSON.stringify({ at: new Date().toISOString(), ...entry }) + '\n');
  } catch {
    /* logging is diagnostic only */
  }
};

const textOf = (content) => {
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    return content
      .map((part) => (typeof part === 'string' ? part : part?.text || ''))
      .join('');
  }
  return '';
};

const teamMember = (system) => {
  const match = String(system || '').match(/<team_members>\s*\n-\s*([A-Za-z0-9][A-Za-z0-9._:-]*)/);
  return match ? match[1] : '';
};

const hasToolResult = (messages) =>
  messages.some(
    (message) =>
      message?.role === 'tool' ||
      (message?.role === 'assistant' && Array.isArray(message?.tool_calls) && message.tool_calls.length > 0),
  );

const lastUserText = (messages) => {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    if (messages[i]?.role === 'user') return textOf(messages[i].content);
  }
  return '';
};

const sse = (res, chunks) => {
  res.writeHead(200, {
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-cache',
    Connection: 'keep-alive',
  });
  for (const chunk of chunks) res.write(`data: ${JSON.stringify(chunk)}\n\n`);
  res.write('data: [DONE]\n\n');
  res.end();
};

const chunk = (model, delta, finish = '') => ({
  id: `chatcmpl-scripted-${model}`,
  object: 'chat.completion.chunk',
  model,
  choices: [{ index: 0, delta, finish_reason: finish || null }],
});

const server = http.createServer((req, res) => {
  if (req.method === 'GET' && req.url === '/health') {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ ok: true, model: 'e2e-scripted' }));
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
    const model = String(body.model || '');
    const messages = Array.isArray(body.messages) ? body.messages : [];
    const system = textOf(messages.find((message) => message?.role === 'system')?.content);
    const userText = lastUserText(messages);
    const member = teamMember(system);
    const toolNames = (Array.isArray(body.tools) ? body.tools : [])
      .map((tool) => tool?.function?.name || tool?.name || '')
      .filter(Boolean);
    const hasAgentTool = toolNames.includes('agent');
    const isCaptain = model.endsWith('-captain') || model.includes('captain');
    const isMember = model.endsWith('-member') || model.includes('member');
    const wantsStart = /开始|组建|启动/.test(userText);
    const toolResult = hasToolResult(messages);
    const captainSpawn =
      isCaptain && wantsStart && !toolResult && member !== '' && hasAgentTool;
    // A member probe proves the member's own Account ∩ Harness: it emits one
    // spawn call that the member Harness must refuse (no bot.invoke) even though
    // the member Account may hold it. The denial is the assertion.
    const memberProbe = isMember && !toolResult && member !== '' && hasAgentTool;

    log({
      model,
      isCaptain,
      isMember,
      wantsStart,
      toolResult,
      member,
      hasAgentTool,
      toolNames,
      captainSpawn,
      memberProbe,
      userText: userText.slice(0, 160),
    });

    if (captainSpawn || memberProbe) {
      const args = JSON.stringify({
        action: 'spawn',
        task: captainSpawn ? MEMBER_TASK : 'permission probe from member harness',
        botId: member,
        idempotencyKey: captainSpawn ? 'e2e-captain-member-1' : 'e2e-member-permission-probe',
      });
      sse(res, [
        chunk(model, { role: 'assistant' }),
        chunk(model, {
          tool_calls: [
            {
              index: 0,
              id: 'call_e2e_captain_member_1',
              type: 'function',
              function: { name: 'agent', arguments: args },
            },
          ],
        }),
        chunk(model, {}, 'tool_calls'),
        {
          id: `chatcmpl-scripted-${model}`,
          object: 'chat.completion.chunk',
          model,
          choices: [],
          usage: { prompt_tokens: 12, completion_tokens: 8 },
        },
      ]);
      return;
    }

    const text = `scripted model: ${model} handled "${userText.slice(0, 40)}"`;
    sse(res, [
      chunk(model, { role: 'assistant' }),
      chunk(model, { content: text }),
      chunk(model, {}, 'stop'),
      {
        id: `chatcmpl-scripted-${model}`,
        object: 'chat.completion.chunk',
        model,
        choices: [],
        usage: { prompt_tokens: 8, completion_tokens: 6 },
      },
    ]);
  });
});

server.listen(PORT, '127.0.0.1', () => {
  console.log(`scripted-model listening on 127.0.0.1:${PORT}`);
});
