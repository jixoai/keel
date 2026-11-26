#!/usr/bin/env node
// Deterministic scripted model for the keel-creation browser E2E.
//
// The deployment's built-in demo provider never calls a tool, so the creation
// flow (an agent writes a plugin through the `creation` tool) cannot be driven
// by it. This stub speaks the OpenAI Chat Completions streaming wire and emits
// the creation actions in order:
//
//   scaffold → validate → install (opens the durable approval)
//   ... a human approves the question ...
//   install {decisionId} → enabled
//
// A second user request drives remove the same way. The model is test
// infrastructure only: it is configured through KEEL_MODELS_CONFIG by
// run.sh and never runs inside the product.
//
// It stops instead of repeating a call after an approval question opens, so
// the product's duplicate-call guardrail is not what ends the turn.
import http from 'node:http';
import fs from 'node:fs';

const PORT = Number(process.env.SCRIPTED_MODEL_PORT || 13745);
const LOG = process.env.SCRIPTED_MODEL_LOG || '';
const PLUGIN_ID = process.env.SCRIPTED_PLUGIN_ID || 'e2e.panel';
export const AI_WRITTEN_MARKER = 'E2E-AI-WRITTEN-MARKER';
// Authored through creation.write, not scaffolded: the E2E must prove the model
// can actually write the package it installs, not only pick template options.
const AI_SETTINGS_CHUNK = `// Authored by the scripted model through creation.write.
const realm = typeof globalThis === "object" ? globalThis.__nsh_browser_realm : undefined;
const modules = realm && realm.modules;
const nsh = (modules && typeof modules.get === "function" && modules.get("nsh")) || {};
const Text = nsh.Text;
const View = nsh.View;
export function render(context) {
  if (typeof Text !== "function" || typeof View !== "function") return null;
  return View({
    testID: "plugin-chunk-settings",
    children: [
      Text({ children: "由 ${PLUGIN_ID} 的 chunk 贡献渲染" }),
      Text({ children: "${AI_WRITTEN_MARKER}" }),
    ],
  });
}
`;

const log = (entry) => {
  if (!LOG) return;
  try {
    fs.appendFileSync(LOG, JSON.stringify({ at: new Date().toISOString(), ...entry }) + '\n');
  } catch {
    /* diagnostics only */
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

const lastUserText = (messages) => {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    if (messages[i]?.role === 'user') return textOf(messages[i].content);
  }
  return '';
};

const allText = (messages) => messages.map((message) => textOf(message.content)).join('\n');

const toolNames = (body) =>
  (Array.isArray(body.tools) ? body.tools : [])
    .map((tool) => tool?.function?.name || tool?.name || '')
    .filter(Boolean);

const decisionIdOf = (messages) => {
  // The newest decision, not the first: a remove turn follows the install
  // turn, and answering with the install decisionId is a 400 ("does not
  // authorize this request").
  const matches = [...allText(messages).matchAll(/"decisionId"\s*:\s*"([0-9a-f-]{36})"/g)];
  return matches.length ? matches[matches.length - 1][1] : '';
};

const hasToolResult = (messages, needle) =>
  messages.some((message) => message?.role === 'tool' && textOf(message.content).includes(needle));

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

const toolCall = (res, model, name, args) =>
  sse(res, [
    chunk(model, { role: 'assistant' }),
    chunk(model, {
      tool_calls: [
        {
          index: 0,
          id: `call_${Date.now()}`,
          type: 'function',
          function: { name, arguments: JSON.stringify(args) },
        },
      ],
    }),
    chunk(model, {}, 'tool_calls'),
    {
      id: `chatcmpl-scripted-${model}`,
      object: 'chat.completion.chunk',
      model,
      choices: [],
      usage: { prompt_tokens: 10, completion_tokens: 5 },
    },
  ]);

const say = (res, model, text) =>
  sse(res, [
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
  ]);

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
    const model = String(body.model || '');
    const messages = Array.isArray(body.messages) ? body.messages : [];
    const user = lastUserText(messages);
    const tools = toolNames(body);
    const creation = tools.includes('creation');
    const all = allText(messages);
    const wantsContinue = /继续|已批准|批准|同意|approve/i.test(user);
    const installed = hasToolResult(messages, '"status":"enabled"');
    const removed = hasToolResult(messages, '"status":"removed"');
    const scaffolded = hasToolResult(messages, '"status":"scaffolded"');
    const packageRead = hasToolResult(messages, '"status":"ok"') && hasToolResult(messages, '"file":"ui/settings.js"');
    const packageWritten = hasToolResult(messages, '"status":"written"');
    const validated = hasToolResult(messages, '"status":"valid"');
    const installApprovalAsked = hasToolResult(messages, '"action":"install"');
    // `remove` is the tool action; the approval document names the lifecycle
    // operation it gates (`plugin.uninstall`).
    const removeApprovalAsked = hasToolResult(messages, '"action":"uninstall"');
    const decisionId = decisionIdOf(messages);
    const lastTool = [...messages].reverse().find((message) => message?.role === 'tool');
    const lastToolText = lastTool ? textOf(lastTool.content) : '';
    // The same assistant tool call followed by its result means the model
    // already acted in this turn. An open approval must then end the turn.
    const lastUserIndex = messages.map((message) => message?.role).lastIndexOf('user');
    const lastAssistantToolIndex = messages
      .map((message) => (message?.role === 'assistant' && Array.isArray(message.tool_calls) && message.tool_calls.length ? true : false))
      .lastIndexOf(true);
    const awaitingAfterCall =
      lastToolText.includes('"approvalRequired":true') && lastAssistantToolIndex > lastUserIndex;

    log({
      model,
      user: user.slice(0, 160),
      tools,
      creation,
      scaffolded,
      packageRead,
      packageWritten,
      validated,
      installApprovalAsked,
      removeApprovalAsked,
      installed,
      removed,
      decisionId,
      wantsContinue,
      awaitingAfterCall,
      lastTool: lastToolText.slice(0, 240),
    });

    if (!creation) {
      say(res, model, 'creation tool unavailable');
      return;
    }
    // A refusal or an internal failure must not be retried in a loop.
    if (
      all.includes('permission_denied') ||
      all.includes('not authorized') ||
      lastToolText.includes('guardrail_duplicate_call') ||
      lastToolText.includes('"status":"failed"')
    ) {
      say(res, model, 'creation 调用失败或权限不足，停止重试。');
      return;
    }

    if (user.includes('卸载') || user.includes('移除')) {
      if (removed) {
        say(res, model, `插件 ${PLUGIN_ID} 已卸载，暂存包已删除。`);
        return;
      }
      if (!removeApprovalAsked) {
        toolCall(res, model, 'creation', { action: 'remove', id: PLUGIN_ID, requestId: 'e2e-remove-1' });
        return;
      }
      if (decisionId && wantsContinue && !awaitingAfterCall) {
        toolCall(res, model, 'creation', {
          action: 'remove',
          id: PLUGIN_ID,
          requestId: 'e2e-remove-1',
          decisionId,
        });
        return;
      }
      say(res, model, '卸载审批已打开，请批准后回复继续。');
      return;
    }

    if (installed) {
      say(res, model, `插件 ${PLUGIN_ID} 已安装并启用；chunk 在 /plugins/${PLUGIN_ID}/ui/settings.js。`);
      return;
    }
    if (!scaffolded) {
      toolCall(res, model, 'creation', {
        action: 'scaffold',
        id: PLUGIN_ID,
        purpose: '端到端创建插件验收面板',
        title: 'E2E Creation Panel',
        tool: 'panel_refresh',
        version: '0.1.0',
      });
      return;
    }
    // Read the scaffolded chunk, then author it: the product claim is that the
    // model writes the plugin, not that it picks template options.
    if (!packageRead) {
      toolCall(res, model, 'creation', { action: 'read', id: PLUGIN_ID, file: 'ui/settings.js' });
      return;
    }
    if (!packageWritten) {
      toolCall(res, model, 'creation', {
        action: 'write',
        id: PLUGIN_ID,
        file: 'ui/settings.js',
        content: AI_SETTINGS_CHUNK,
      });
      return;
    }
    if (!validated) {
      toolCall(res, model, 'creation', { action: 'validate', id: PLUGIN_ID });
      return;
    }
    if (!installApprovalAsked) {
      toolCall(res, model, 'creation', { action: 'install', id: PLUGIN_ID, requestId: 'e2e-install-1' });
      return;
    }
    if (decisionId && wantsContinue && !awaitingAfterCall) {
      toolCall(res, model, 'creation', {
        action: 'install',
        id: PLUGIN_ID,
        requestId: 'e2e-install-1',
        decisionId,
      });
      return;
    }
    say(res, model, '安装审批已打开，请批准后回复继续。');
  });
});

server.listen(PORT, '127.0.0.1', () => {
  console.log(`scripted-creation-model listening on 127.0.0.1:${PORT}`);
});
