#!/usr/bin/env node
// Minimal MCP stdio server for the P6 browser E2E (change
// feat-mcp-apps-events, P6.2). It is a fixture, not a product component:
// newline-delimited JSON-RPC on stdin/stdout, one newline per frame.
//
// It speaks enough of the 2025-11-25 dialect for the real keel client:
//
//   server/discover      → -32601 (the auto probe falls back to initialize)
//   initialize           → 2025-11-25 capabilities
//   notifications/*      → logged, no answer
//   tools/list           → render_weather (model+app) and refresh_weather (app)
//   tools/call           → a CallToolResult with text + structuredContent
//   resources/list       → the UI resource entry
//   resources/read       → the app document (text/html;profile=mcp-app)
//   prompts/list         → empty
//
// With MCP_E2E_DEGRADE=1 the same server also publishes the two degradation
// fixtures used by P6A2a (scripts/mcp-apps-degrade-e2e.sh):
//
//   ui://weather/broken  render_broken  an app that never answers ui/initialize
//   ui://weather/leaky   render_leaky   declares only https://api.fixture.test in
//                                       _meta.ui.csp.connectDomains and fetches
//                                       https://evil.fixture.test/leak
//
// The flag keeps the P6A1 happy-path fixture exactly as it was (one app).
//
// Every request is appended to MCP_E2E_LOG as one JSON line
// ({method, params, id}) so the browser runner can assert that the host's
// app-bridge tools/call really reached this process (View → Host → MCP).

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const LOG = process.env.MCP_E2E_LOG || '';
const DEGRADE = process.env.MCP_E2E_DEGRADE === '1';
const APP_HTML = fs.readFileSync(path.join(HERE, 'app.html'), 'utf8');
const BROKEN_HTML = DEGRADE ? fs.readFileSync(path.join(HERE, 'app-broken.html'), 'utf8') : '';
const LEAKY_HTML = DEGRADE ? fs.readFileSync(path.join(HERE, 'app-leaky.html'), 'utf8') : '';
const APP_URI = 'ui://weather/dashboard';
const BROKEN_URI = 'ui://weather/broken';
const LEAKY_URI = 'ui://weather/leaky';

function log(entry) {
  if (!LOG) return;
  try {
    fs.appendFileSync(LOG, JSON.stringify(entry) + '\n');
  } catch {
    /* the fixture must not fail a run because its log is unavailable */
  }
}

function send(message) {
  process.stdout.write(JSON.stringify(message) + '\n');
}

function ok(id, result) {
  send({ jsonrpc: '2.0', id, result });
}

function fail(id, code, message) {
  send({ jsonrpc: '2.0', id, error: { code, message } });
}

const TOOLS = [
  {
    name: 'render_weather',
    title: '天气面板',
    description: 'Render the weather dashboard for one city.',
    inputSchema: {
      type: 'object',
      properties: { city: { type: 'string', description: 'City name.' } },
      required: ['city'],
    },
    // The new structured key. The host derives appId = sha256(server, uri).
    _meta: { ui: { resourceUri: APP_URI, visibility: ['model', 'app'] } },
  },
  {
    name: 'refresh_weather',
    title: '刷新天气',
    description: 'Refresh the dashboard; app-only (not model-visible).',
    inputSchema: {
      type: 'object',
      properties: { city: { type: 'string' } },
      required: ['city'],
    },
    // App-only: must never appear in the model's aggregate `mcp` list, but the
    // app bridge may call it.
    _meta: { ui: { visibility: ['app'] } },
  },
  ...(DEGRADE
    ? [
        {
          name: 'render_broken',
          title: '坏面板（握手超时）',
          description: 'Render the app that never answers ui/initialize.',
          inputSchema: {
            type: 'object',
            properties: { city: { type: 'string' } },
            required: ['city'],
          },
          _meta: { ui: { resourceUri: BROKEN_URI, visibility: ['model', 'app'] } },
        },
        {
          name: 'render_leaky',
          title: '泄漏面板（CSP）',
          description: 'Render the app that fetches an undeclared domain.',
          inputSchema: {
            type: 'object',
            properties: { city: { type: 'string' } },
            required: ['city'],
          },
          _meta: { ui: { resourceUri: LEAKY_URI, visibility: ['model', 'app'] } },
        },
      ]
    : []),
];

const RESOURCES = [
  {
    uri: APP_URI,
    name: 'Weather dashboard',
    description: 'Interactive weather panel.',
    mimeType: 'text/html;profile=mcp-app',
  },
  ...(DEGRADE
    ? [
        {
          uri: BROKEN_URI,
          name: 'Broken weather panel',
          description: 'Never completes the app handshake.',
          mimeType: 'text/html;profile=mcp-app',
        },
        {
          uri: LEAKY_URI,
          name: 'Leaky weather panel',
          description: 'Declares connectDomains and fetches an undeclared host.',
          mimeType: 'text/html;profile=mcp-app',
        },
      ]
    : []),
];

/** The app documents keyed by resource URI. Only apps that resources/list
 * published are readable: a URI that is not in RESOURCES is not found. */
const DOCUMENTS = new Map([
  [APP_URI, { bytes: APP_HTML }],
  ...(DEGRADE
    ? [
        [BROKEN_URI, { bytes: BROKEN_HTML }],
        // The CSP declaration lives on the content item's `_meta.ui`, the
        // level apps.mdx defines; the host header is derived from it.
        [
          LEAKY_URI,
          {
            bytes: LEAKY_HTML,
            meta: { ui: { csp: { connectDomains: ['https://api.fixture.test'] } } },
          },
        ],
      ]
    : []),
]);

function toolResult(name, city) {
  if (name === 'render_broken') {
    return {
      content: [{ type: 'text', text: '坏面板的文本结果仍然可读（交互界面预期握手超时）。' }],
      structuredContent: { city, fixture: 'broken' },
      isError: false,
    };
  }
  if (name === 'render_leaky') {
    return {
      content: [{ type: 'text', text: '泄漏夹具的文本结果仍然可读（外部请求应被 CSP 阻断）。' }],
      structuredContent: { city, fixture: 'leaky' },
      isError: false,
    };
  }
  return {
    content: [{ type: 'text', text: `上海现在 24°C，湿度 61%（${city}）。` }],
    structuredContent: { city, temperatureC: 24, humidity: 61 },
    isError: false,
  };
}

function handle(message) {
  const method = typeof message.method === 'string' ? message.method : '';
  const id = message.id;
  const hasId = id !== undefined && id !== null;
  log({ method, id: hasId ? id : null, params: message.params ?? null });

  if (!method) {
    // A response from the client (none expected in this fixture): ignore.
    return;
  }
  if (!hasId) {
    // notifications/initialized, notifications/cancelled, ...
    return;
  }
  switch (method) {
    case 'server/discover':
      // The auto-era probe: a legacy server answers "method not found", and the
      // client retries with initialize (internal/mcp/era.go probeModern).
      fail(id, -32601, 'Method not found');
      return;
    case 'initialize':
      ok(id, {
        protocolVersion: '2025-11-25',
        capabilities: { tools: { listChanged: false }, resources: { listChanged: false }, prompts: { listChanged: false } },
        serverInfo: { name: 'e2e-weather', version: '1.0.0' },
        instructions: 'Deterministic fixture for the MCP Apps browser E2E.',
      });
      return;
    case 'ping':
      ok(id, {});
      return;
    case 'tools/list':
      ok(id, { tools: TOOLS });
      return;
    case 'tools/call': {
      const params = message.params ?? {};
      const name = params.name;
      const args = params.arguments ?? {};
      if (!TOOLS.some((tool) => tool.name === name)) {
        fail(id, -32602, `unknown tool: ${String(name)}`);
        return;
      }
      ok(id, toolResult(name, typeof args.city === 'string' && args.city ? args.city : 'Shanghai'));
      return;
    }
    case 'resources/list':
      ok(id, { resources: RESOURCES });
      return;
    case 'resources/templates/list':
      ok(id, { resourceTemplates: [] });
      return;
    case 'resources/read': {
      const uri = (message.params ?? {}).uri;
      const document = DOCUMENTS.get(uri);
      if (!document) {
        fail(id, -32002, `resource not found: ${String(uri)}`);
        return;
      }
      const contents = { uri, mimeType: 'text/html;profile=mcp-app', text: document.bytes };
      if (document.meta) contents._meta = document.meta;
      ok(id, { contents: [contents] });
      return;
    }
    case 'prompts/list':
      ok(id, { prompts: [] });
      return;
    default:
      fail(id, -32601, `Method not found: ${method}`);
  }
}

let buffer = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (chunk) => {
  buffer += chunk;
  for (;;) {
    const newline = buffer.indexOf('\n');
    if (newline < 0) break;
    const line = buffer.slice(0, newline).trim();
    buffer = buffer.slice(newline + 1);
    if (!line) continue;
    let message;
    try {
      message = JSON.parse(line);
    } catch (error) {
      log({ method: '<bad-json>', error: String(error), line: line.slice(0, 200) });
      continue;
    }
    try {
      handle(message);
    } catch (error) {
      log({ method: message?.method ?? '<handler>', error: String(error) });
      if (message && message.id !== undefined) fail(message.id, -32603, String(error));
    }
  }
});
process.stdin.on('end', () => process.exit(0));
