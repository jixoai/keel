#!/usr/bin/env node
// Real-browser happy path for the MCP Apps surface (change feat-mcp-apps-events,
// P6.2). The script drives the production web build against a real `keel serve`,
// a real stdio MCP server and a real Chrome:
//
//   login → open the session → expand the `mcp` tool row → the host mounts the
//   app iframe → handshake/tool-input/tool-result → the app calls an app-only
//   tool through the host proxy → fullscreen → back to inline.
//
// It writes one PNG per step, a machine-readable report.json (assertions,
// document headers, MCP server method counts, app state) and a self-contained
// report.html. Nothing credential-shaped is recorded: no cookie, token, DSN or
// Authorization value. The password is typed into the login field and never
// written.
//
// Env:
//   E2E_BASE            http://127.0.0.1:13682
//   E2E_EVIDENCE        output directory (created)
//   E2E_RUN             run label (run-1, run-2)
//   E2E_MCP_LOG         MCP fixture JSONL log
//   E2E_WIRE_LOG        stub provider JSONL log
//   KEEL_SERVE_PASSWORD passphrase for the login gate
//   PLAYWRIGHT_CORE     optional playwright-core entry
//   E2E_CHROME          optional Chrome executable

import fs from 'node:fs';
import path from 'node:path';

const BASE = process.env.E2E_BASE || 'http://127.0.0.1:13682';
const EVIDENCE = process.env.E2E_EVIDENCE || '/tmp/mcp-apps-e2e';
const RUN = process.env.E2E_RUN || 'run-1';
const MCP_LOG = process.env.E2E_MCP_LOG || '';
const WIRE_LOG = process.env.E2E_WIRE_LOG || '';
const PASSWORD = process.env.KEEL_SERVE_PASSWORD || '';
const CHROME = process.env.E2E_CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const PLAYWRIGHT =
  process.env.PLAYWRIGHT_CORE || '/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs';

if (!PASSWORD) {
  console.error('KEEL_SERVE_PASSWORD is not set');
  process.exit(2);
}

const SHOTS = path.join(EVIDENCE, 'screenshots');
fs.mkdirSync(SHOTS, { recursive: true });

const playwright = await import(PLAYWRIGHT);
const { chromium } = playwright;
const browser = await chromium.launch({ executablePath: CHROME, headless: true });
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
const page = await context.newPage();

const report = {
  lane: 'P6A1',
  run: RUN,
  startedAt: new Date().toISOString(),
  baseUrl: BASE,
  steps: [],
  assertions: {},
  mcp: { methods: {}, toolCalls: [] },
  app: null,
  fullscreen: null,
  documentHeaders: null,
  console: [],
  pageErrors: [],
  failedRequests: [],
  findings: [],
  pass: false,
};

let shotIndex = 0;
const shot = async (name) => {
  shotIndex += 1;
  const file = `${String(shotIndex).padStart(2, '0')}-${name}.png`;
  const full = path.join(SHOTS, file);
  // Let the kit's open/close and dialog animations settle before capturing.
  await page.waitForTimeout(300);
  await page.screenshot({ path: full, fullPage: true });
  return path.relative(EVIDENCE, full).replaceAll(path.sep, '/');
};

const step = async (name, detail, options = {}) => {
  const entry = {
    name,
    ok: options.ok !== false,
    at: new Date().toISOString(),
    detail: detail ?? null,
  };
  if (options.screenshot !== false) entry.screenshot = await shot(name);
  report.steps.push(entry);
  return entry;
};

const assertions = report.assertions;
function check(name, ok, detail) {
  assertions[name] = { ok: Boolean(ok), detail: detail ?? null };
  if (!ok) throw new Error(`assertion failed: ${name}${detail ? `: ${JSON.stringify(detail)}` : ''}`);
}

function readJsonl(file) {
  if (!file || !fs.existsSync(file)) return [];
  return fs
    .readFileSync(file, 'utf8')
    .split('\n')
    .filter((line) => line.trim())
    .map((line) => {
      try {
        return JSON.parse(line);
      } catch {
        return null;
      }
    })
    .filter(Boolean);
}

page.on('console', (message) => {
  const line = `${message.type()}: ${message.text()}`;
  report.console.push(line);
});
page.on('pageerror', (error) => report.pageErrors.push(String(error?.message ?? error)));
page.on('requestfailed', (request) =>
  report.failedRequests.push({ url: request.url(), error: request.failure()?.errorText ?? '' }),
);

const visibleAppFrame = async (scope = 'body') => {
  const handles = await page.$$(`${scope} iframe[src*="/v1/mcp/apps/"]`);
  for (const handle of handles.reverse()) {
    const box = await handle.boundingBox();
    if (!box || box.height <= 0) continue;
    const frame = await handle.contentFrame();
    if (frame) return { handle, frame, box };
  }
  return null;
};

const waitForAppFrame = async (timeout = 25000, scope = 'body') => {
  const deadline = Date.now() + timeout;
  for (;;) {
    const found = await visibleAppFrame(scope);
    if (found) return found;
    if (Date.now() > deadline) throw new Error('the app iframe did not mount');
    await page.waitForTimeout(100);
  }
};

const waitForHandshake = async (frame, timeout = 25000) => {
  await frame.waitForFunction(
    () => {
      const state = window.__e2e;
      if (!state) return false;
      const methods = state.frames.map((entry) => entry.method);
      return (
        methods.includes('ui/notifications/tool-input') &&
        methods.includes('ui/notifications/tool-result')
      );
    },
    undefined,
    { timeout },
  );
  return frame.evaluate(() => window.__e2e);
};

const waitForState = async (frame, predicate, timeout = 25000) => {
  await frame.waitForFunction(predicate, undefined, { timeout });
  return frame.evaluate(() => window.__e2e);
};

const UNSUPPORTED_TEXT = '此结果带交互界面；当前客户端以文本显示。';

/** Open the session row (if the list is showing) and wait for the tool row. */
const openSession = async () => {
  const row = await page.$('[aria-label^="打开会话"]');
  if (row) await row.click();
  await page.waitForSelector('[aria-label="工具 mcp"]', { timeout: 30000, state: 'visible' });
};

/** Expand the row and report how the client answered: a mounted iframe, or
 * the text fallback (which on this build means the capability gate is stale). */
const expandToolRow = async (waitMs) => {
  if (await page.$('iframe[src*="/v1/mcp/apps/"]')) return { iframe: true };
  if (await page.$(`text=${UNSUPPORTED_TEXT}`)) return { iframe: false, stale: true };
  await page.locator('[aria-label="工具 mcp"]').last().click();
  const deadline = Date.now() + waitMs;
  while (Date.now() < deadline) {
    if (await page.$('iframe[src*="/v1/mcp/apps/"]')) return { iframe: true };
    if (await page.$(`text=${UNSUPPORTED_TEXT}`)) return { iframe: false, stale: true };
    await page.waitForTimeout(100);
  }
  return { iframe: false };
};

try {
  // ---- 0. login gate -----------------------------------------------------
  await page.goto(BASE, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('input[aria-label="服务口令"], input[type="password"], input', {
    timeout: 30000,
    state: 'visible',
  });
  await step('login-gate', { url: page.url() });

  const passwordInput = await page.$('input[aria-label="服务口令"]');
  const input = passwordInput ?? (await page.$('input[type="password"]')) ?? (await page.$('input'));
  await input.fill(PASSWORD);
  const submit =
    (await page.$('[aria-label="进入 Keel"]')) ??
    (await page.$('[aria-label="进入"]')) ??
    (await page.$('text=进入'));
  if (!submit) throw new Error('login submit button not found');
  await submit.click();
  await page.waitForSelector('[aria-label^="打开会话"], [aria-label="工具 mcp"]', {
    timeout: 30000,
    state: 'visible',
  });
  await step('login-submitted', { authenticated: true });

  // ---- 1. open the session ----------------------------------------------
  await openSession();
  await step('session-open', { toolRow: '工具 mcp' });

  // ---- 2. expand the tool row; the View mounts ---------------------------
  // A fresh browser arrives through the login gate. This build loads the
  // deployment capabilities once in detect(), before the gate; the gate path
  // answers 401, stores an empty deployment, and login() does not re-read it.
  // `can("mcp.apps")` is therefore false until the next page load, and the
  // slot renders its text fallback. Record the defect, then reload (the
  // legitimate next user action) and continue on the real happy path.
  let expanded = await expandToolRow(8000);
  if (expanded.stale) {
    const finding = {
      id: 'login-capability-stale',
      summary:
        '登录后能力位未刷新：首屏 can("mcp.apps") 仍为 false，带 app 的 tool.result 以「当前客户端以文本显示」兜底；刷新页面才挂载 View。',
      repro:
        'fresh browser → 登录 → 打开含 app 工具的会话 → 展开工具行 → 文本兜底；page.reload() 后同一操作挂载 iframe。',
      impact: 'P2/P6 产品缺陷（前端 store 登录路径）；本 lane 不改产品代码。',
    };
    report.findings.push(finding);
    await step('login-capability-stale', finding);
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[aria-label^="打开会话"], [aria-label="工具 mcp"]', {
      timeout: 30000,
      state: 'visible',
    });
    await openSession();
    expanded = await expandToolRow(20000);
    if (!expanded.iframe) throw new Error('the app iframe still does not mount after a reload');
    await step('post-reload-session', { reloaded: true, iframeMounted: true });
  } else if (!expanded.iframe) {
    throw new Error('the app iframe did not mount');
  }
  await step('tool-row-expanded', { iframeMounted: true });

  const mounted = await waitForAppFrame();
  const handle = mounted.handle;
  const src = await handle.getAttribute('src');
  const sandbox = await handle.getAttribute('sandbox');
  const parsed = new URL(src);
  const hash = parsed.searchParams.get('v') ?? '';
  const appMatch = parsed.pathname.match(/^\/v1\/mcp\/apps\/(mcpapp_[a-f0-9]{16})\/view$/);
  check('iframe-path', Boolean(appMatch), { pathname: parsed.pathname });
  check('iframe-hash', /^[a-f0-9]{64}$/.test(hash), { v: hash });
  check('iframe-instance', /^[a-z0-9]{16,64}$/.test(parsed.searchParams.get('instance') ?? ''), {
    instance: parsed.searchParams.get('instance'),
  });
  check('sandbox-allow-scripts', (sandbox ?? '').split(/\s+/).includes('allow-scripts'), { sandbox });
  check('sandbox-no-same-origin', !(sandbox ?? '').split(/\s+/).includes('allow-same-origin'), { sandbox });
  await step('iframe-mounted', { src, sandbox, box: mounted.box });

  // ---- 3. document headers ----------------------------------------------
  const documentResponse = await page.request.get(src);
  const responseHeaders = documentResponse.headers();
  report.documentHeaders = {
    status: documentResponse.status(),
    contentType: responseHeaders['content-type'] ?? null,
    csp: responseHeaders['content-security-policy'] ?? null,
    nosniff: responseHeaders['x-content-type-options'] ?? null,
    referrer: responseHeaders['referrer-policy'] ?? null,
    appMime: responseHeaders['x-mcp-app-mime'] ?? null,
    appHash: responseHeaders['x-mcp-app-hash'] ?? null,
    cacheControl: responseHeaders['cache-control'] ?? null,
  };
  check('document-status', report.documentHeaders.status === 200, report.documentHeaders.status);
  check('document-csp-sandbox', String(report.documentHeaders.csp).includes('sandbox allow-scripts'), {
    csp: report.documentHeaders.csp,
  });
  check('document-csp-default-none', String(report.documentHeaders.csp).includes("default-src 'none'"), {
    csp: report.documentHeaders.csp,
  });
  check('document-csp-frame-ancestors', String(report.documentHeaders.csp).includes("frame-ancestors 'self'"), {
    csp: report.documentHeaders.csp,
  });
  check('document-nosniff', report.documentHeaders.nosniff === 'nosniff', report.documentHeaders.nosniff);
  check('document-no-referrer', report.documentHeaders.referrer === 'no-referrer', report.documentHeaders.referrer);
  check('document-app-mime', report.documentHeaders.appMime === 'text/html;profile=mcp-app', report.documentHeaders.appMime);
  check('document-app-hash', report.documentHeaders.appHash === hash, {
    header: report.documentHeaders.appHash,
    url: hash,
  });
  check('document-immutable', String(report.documentHeaders.cacheControl).includes('immutable'), {
    cacheControl: report.documentHeaders.cacheControl,
  });
  await step('document-headers', report.documentHeaders);

  // ---- 4. handshake + tool input/result ---------------------------------
  const appState = await waitForHandshake(mounted.frame);
  report.app = {
    bootedAt: appState.bootedAt,
    host: appState.host,
    methods: appState.frames.map((entry) => entry.method),
    toolInput: appState.toolInput,
    toolResult: appState.toolResult,
    toolsCall: appState.toolsCall,
    origin: appState.origin,
    locationOrigin: appState.locationOrigin,
    isolation: appState.isolation,
    errors: appState.errors,
  };
  check('host-protocol', appState.host?.protocolVersion === '2026-01-26', appState.host?.protocolVersion);
  check('host-name', appState.host?.hostInfo?.name === 'Keel', appState.host?.hostInfo?.name);
  check('tool-input-once', appState.frames.filter((f) => f.method === 'ui/notifications/tool-input').length === 1, appState.frames);
  check('tool-result-once', appState.frames.filter((f) => f.method === 'ui/notifications/tool-result').length === 1, appState.frames);
  check('tool-result-text', JSON.stringify(appState.toolResult).includes('24°C'), appState.toolResult);
  check('tool-result-structured', appState.toolResult?.structuredContent?.temperatureC === 24, appState.toolResult?.structuredContent);
  check('app-opaque-origin', appState.origin === 'null', { origin: appState.origin, locationOrigin: appState.locationOrigin });
  check('app-parent-dom-blocked', String(appState.isolation?.parentDom ?? '').startsWith('blocked'), appState.isolation);
  check('app-storage-blocked', String(appState.isolation?.localStorage ?? '').startsWith('blocked'), appState.isolation);

  // Observation, not a gate: with the aggregate `mcp` tool the transcript's
  // tool_call args are the aggregate envelope. The spec says tool-input carries
  // "本次调用实参"; an app bound to render_weather expects {city}, but receives
  // {action,server,tool,arguments}. The fixture reads both shapes; the product
  // behavior is recorded for the owning lane.
  const inputArgs = appState.toolInput?.arguments;
  if (
    inputArgs &&
    typeof inputArgs === 'object' &&
    'action' in inputArgs &&
    'server' in inputArgs &&
    'tool' in inputArgs
  ) {
    report.findings.push({
      id: 'app-tool-input-aggregate-envelope',
      summary:
        'View 的 ui/notifications/tool-input.params.arguments 是聚合 mcp 工具的参数信封（action/server/tool/arguments），不是所属 MCP 工具自身的实参；真实 app 按 params.arguments.city 读取会拿不到。',
      repro:
        '模型经聚合 mcp 调用 render_weather → 展开工具行 → View 收到 {"arguments":{"action":"call","server":"e2e","tool":"render_weather","arguments":{"city":"Shanghai"}}}。',
      spec: 'specs/mcp-apps/spec.md「tool-input 参数为本次调用实参」；design §3.5 表写「从 store 的 assistant tool_call args 取」——聚合工具下二者不同。',
      impact: '前端 McpAppSlot 把 props.call.args 原样传给 View；本 lane 不改产品代码，fixture 兼容两种形状。',
      evidence: { toolInput: appState.toolInput },
    });
  }
  await page.waitForFunction(
    () => {
      const el = document.querySelector('iframe[src*="/v1/mcp/apps/"]');
      return Boolean(el && el.getBoundingClientRect().height >= 359);
    },
    undefined,
    { timeout: 15000 },
  );
  const bodyText = await page.locator('body').innerText();
  check('host-status-connected', bodyText.includes('已连接'), { sample: bodyText.slice(0, 200) });
  check('host-resource-uri', bodyText.includes('ui://weather/dashboard'), { sample: bodyText.slice(-300) });
  await step('app-handshake', report.app);

  // ---- 5. the View's tools/call reaches the real MCP server -------------
  await waitForState(mounted.frame, () => Boolean(window.__e2e && window.__e2e.toolsCall), 20000);
  const afterCall = await mounted.frame.evaluate(() => window.__e2e);
  report.app.toolsCall = afterCall.toolsCall;
  check('app-tools-call-ok', !afterCall.toolsCall?.error, afterCall.toolsCall);

  const mcpRows = readJsonl(MCP_LOG);
  const counts = {};
  for (const row of mcpRows) counts[row.method] = (counts[row.method] ?? 0) + 1;
  report.mcp = {
    methods: counts,
    toolCalls: mcpRows
      .filter((row) => row.method === 'tools/call')
      .map((row) => ({ name: row.params?.name, arguments: row.params?.arguments })),
    resourcesRead: mcpRows
      .filter((row) => row.method === 'resources/read')
      .map((row) => row.params?.uri),
  };
  check('mcp-initialize', (counts.initialize ?? 0) >= 1, counts);
  check('mcp-tools-list', (counts['tools/list'] ?? 0) >= 1, counts);
  check('mcp-resources-read', (counts['resources/read'] ?? 0) >= 1, counts);
  check('mcp-model-tool-call', report.mcp.toolCalls.some((call) => call.name === 'render_weather'), report.mcp.toolCalls);
  check('mcp-app-tool-call', report.mcp.toolCalls.some((call) => call.name === 'refresh_weather'), report.mcp.toolCalls);
  await step('app-tools-call', { app: afterCall.toolsCall, mcp: report.mcp });

  const wireRows = readJsonl(WIRE_LOG);
  const modelRequests = wireRows.filter((row) => row.kind === 'request');
  const wireToolNames = modelRequests.flatMap((row) => row.tools ?? []);
  check('wire-mcp-tool-present', wireToolNames.includes('mcp'), { tools: wireToolNames });
  check('wire-tool-result-round', modelRequests.some((row) => row.hasToolResult === true), {
    requests: modelRequests.length,
    hasToolResult: modelRequests.map((row) => row.hasToolResult),
  });
  await step('wire-request', {
    requests: modelRequests.length,
    tools: wireToolNames,
    hasToolResult: modelRequests.map((row) => row.hasToolResult),
  });

  // ---- 6. fullscreen: a new instance must mount and re-handshake ---------
  const inlineBootedAt = report.app.bootedAt;
  await (await page.$('[aria-label="全屏显示交互界面"]')).click();
  await page.waitForSelector('[aria-label="天气面板 全屏"]', {
    timeout: 15000,
    state: 'visible',
  });
  const fullMounted = await waitForAppFrame(25000, '[aria-label="天气面板 全屏"]');
  const fullState = await waitForHandshake(fullMounted.frame);
  report.fullscreen = {
    bootedAt: fullState.bootedAt,
    newInstance: fullState.bootedAt !== inlineBootedAt,
    methods: fullState.frames.map((entry) => entry.method),
    dialogVisible: true,
    hostContext: fullState.host?.hostContext,
  };
  check('fullscreen-new-instance', report.fullscreen.newInstance, {
    inline: inlineBootedAt,
    fullscreen: fullState.bootedAt,
  });
  check('fullscreen-host-context', fullState.host?.hostContext?.displayMode === 'fullscreen', {
    displayMode: fullState.host?.hostContext?.displayMode,
    availableDisplayModes: fullState.host?.hostContext?.availableDisplayModes,
  });
  check('fullscreen-handshake', report.fullscreen.methods.includes('ui/notifications/tool-input'), report.fullscreen.methods);
  check('fullscreen-tool-result', report.fullscreen.methods.includes('ui/notifications/tool-result'), report.fullscreen.methods);
  const fullState2 = await waitForState(fullMounted.frame, () => Boolean(window.__e2e?.toolsCall), 15000);
  report.fullscreen.toolsCall = fullState2.toolsCall;
  check('fullscreen-tools-call-ok', !fullState2.toolsCall?.error, fullState2.toolsCall);
  await step('fullscreen', report.fullscreen);

  // ---- 7. back to inline: another fresh instance ------------------------
  await (await page.$('[aria-label="退出全屏"]')).click();
  await page.waitForSelector('[aria-label="天气面板 全屏"]', { timeout: 10000, state: 'hidden' });
  const inlineMounted = await waitForAppFrame();
  const inlineState = await waitForHandshake(inlineMounted.frame);
  report.fullscreen.exitBootedAt = inlineState.bootedAt;
  report.fullscreen.exitHostContext = inlineState.host?.hostContext;
  check('inline-remount', inlineState.bootedAt !== fullState.bootedAt, {
    fullscreen: fullState.bootedAt,
    inline: inlineState.bootedAt,
  });
  check('inline-host-context', inlineState.host?.hostContext?.displayMode === 'inline', {
    displayMode: inlineState.host?.hostContext?.displayMode,
  });
  check('inline-handshake', inlineState.frames.some((f) => f.method === 'ui/notifications/tool-result'), inlineState.frames);
  const inlineState2 = await waitForState(inlineMounted.frame, () => Boolean(window.__e2e?.toolsCall), 15000);
  check('inline-tools-call-ok', !inlineState2.toolsCall?.error, inlineState2.toolsCall);
  await step('fullscreen-exit', {
    bootedAt: inlineState.bootedAt,
    methods: inlineState.frames.map((entry) => entry.method),
    toolsCall: inlineState2.toolsCall,
  });

  // The final log read runs after all three instances (inline, fullscreen,
  // back-to-inline) have issued their own tools/call: the count proves each
  // remount really reached the MCP server, not just the first one.
  const refreshCalls = () =>
    readJsonl(MCP_LOG).filter((row) => row.method === 'tools/call' && row.params?.name === 'refresh_weather').length;
  const refreshDeadline = Date.now() + 15000;
  while (refreshCalls() < 3 && Date.now() < refreshDeadline) await page.waitForTimeout(100);
  const finalRows = readJsonl(MCP_LOG);
  const finalCounts = {};
  for (const row of finalRows) finalCounts[row.method] = (finalCounts[row.method] ?? 0) + 1;
  report.mcp = {
    methods: finalCounts,
    toolCalls: finalRows
      .filter((row) => row.method === 'tools/call')
      .map((row) => ({ name: row.params?.name, arguments: row.params?.arguments })),
    resourcesRead: finalRows.filter((row) => row.method === 'resources/read').map((row) => row.params?.uri),
  };
  check(
    'mcp-app-tool-calls-all-instances',
    report.mcp.toolCalls.filter((call) => call.name === 'refresh_weather').length >= 3,
    report.mcp.toolCalls,
  );

  // ---- 8. no unexpected page errors --------------------------------------
  // A reload and the display-mode remount abort in-flight fetches/streams by
  // design; ERR_ABORTED is the browser's cancellation, not a failure.
  const unexpectedFailures = report.failedRequests.filter((request) => request.error !== 'net::ERR_ABORTED');
  check('no-page-errors', report.pageErrors.length === 0, report.pageErrors);
  check('no-failed-requests', unexpectedFailures.length === 0, {
    unexpected: unexpectedFailures,
    aborted: report.failedRequests.filter((request) => request.error === 'net::ERR_ABORTED').map((request) => request.url),
  });

  report.pass = true;
} catch (error) {
  report.error = String(error?.stack ?? error);
  try {
    report.failureScreenshot = await shot('failure');
  } catch {
    /* a screenshot of a dead page is best effort */
  }
  try {
    report.debug = await page.evaluate(() => ({
      bodyText: document.body?.innerText?.slice(-2500) ?? '',
      iframes: Array.from(document.querySelectorAll('iframe')).map((node) => ({
        src: node.src,
        sandbox: node.getAttribute('sandbox'),
      })),
    }));
  } catch {
    /* debug is best effort */
  }
  report.pass = false;
} finally {
  report.finishedAt = new Date().toISOString();
  fs.writeFileSync(path.join(EVIDENCE, 'report.json'), JSON.stringify(report, null, 2));

  const escapeHtml = (value) =>
    String(value)
      .replaceAll('&', '&amp;')
      .replaceAll('<', '&lt;')
      .replaceAll('>', '&gt;')
      .replaceAll('"', '&quot;');
  const img = (relative) => {
    const full = path.join(EVIDENCE, relative);
    if (!fs.existsSync(full)) return '';
    return `<img alt="${escapeHtml(relative)}" src="data:image/png;base64,${fs.readFileSync(full).toString('base64')}">`;
  };
  const assertionRows = Object.entries(assertions)
    .map(
      ([name, value]) =>
        `<tr class="${value.ok ? 'ok' : 'bad'}"><td>${escapeHtml(name)}</td><td>${value.ok ? 'PASS' : 'FAIL'}</td><td><code>${escapeHtml(
          JSON.stringify(value.detail),
        )}</code></td></tr>`,
    )
    .join('\n');
  const stepBlocks = report.steps
    .map(
      (entry) =>
        `<section class="step ${entry.ok ? 'ok' : 'bad'}"><h2>${escapeHtml(entry.name)} ${entry.ok ? '✓' : '✗'}</h2>` +
        (entry.screenshot ? img(entry.screenshot) : '') +
        `<pre>${escapeHtml(JSON.stringify(entry.detail, null, 2))}</pre></section>`,
    )
    .join('\n');
  const methodRows = Object.entries(report.mcp.methods)
    .map(([method, count]) => `<tr><td>${escapeHtml(method)}</td><td>${count}</td></tr>`)
    .join('\n');
  const headerRows = Object.entries(report.documentHeaders ?? {})
    .map(([key, value]) => `<tr><td>${escapeHtml(key)}</td><td><code>${escapeHtml(value)}</code></td></tr>`)
    .join('\n');
  const html = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>P6A1 · MCP Apps 浏览器 E2E · ${escapeHtml(RUN)}</title>
<style>
  body{font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;margin:24px;max-width:1180px;color:#18181b}
  h1{font-size:22px;margin:0 0 4px} h2{font-size:16px;margin:0 0 8px}
  .verdict{display:inline-block;padding:2px 10px;border-radius:999px;font-weight:700}
  .verdict.pass{background:#dcfce7;color:#166534}.verdict.fail{background:#fee2e2;color:#991b1b}
  table{border-collapse:collapse;width:100%;margin:8px 0 18px;font-size:12px}
  th,td{border:1px solid #e4e4e7;padding:5px 8px;text-align:left;vertical-align:top}
  tr.ok td:nth-child(2){color:#166534;font-weight:700} tr.bad td:nth-child(2){color:#991b1b;font-weight:700}
  code{font:11px/1.4 ui-monospace,SFMono-Regular,Menlo,monospace;word-break:break-word}
  pre{font:11px/1.4 ui-monospace,SFMono-Regular,Menlo,monospace;background:#f4f4f5;padding:8px;border-radius:6px;white-space:pre-wrap;word-break:break-word}
  section.step{border:1px solid #e4e4e7;border-radius:8px;padding:12px;margin:14px 0}
  section.step.bad{border-color:#fca5a5;background:#fff1f2}
  img{max-width:100%;border:1px solid #e4e4e7;border-radius:6px;margin:6px 0}
  .meta{color:#52525b;font-size:12px;margin-bottom:16px}
  h3{font-size:14px;margin:18px 0 4px}
</style></head><body>
<h1>P6A1 · 真实浏览器 E2E（MCP Apps happy path + 全屏）</h1>
<div class="meta">
  run <strong>${escapeHtml(RUN)}</strong> · ${escapeHtml(report.startedAt)} → ${escapeHtml(report.finishedAt)} ·
  <span class="verdict ${report.pass ? 'pass' : 'fail'}">${report.pass ? 'PASS' : 'FAIL'}</span>
  ${report.error ? `<div><strong>error:</strong> <code>${escapeHtml(report.error)}</code></div>` : ''}
</div>
<p>链路：真实 <code>keel serve</code> → 真实 stdio MCP server → 真实 Chrome（playwright-core + 系统 Chrome）。
证据只记录断言、方法名、状态与大小；不含 cookie/token/DSN/Authorization。</p>
<h3>断言</h3><table><thead><tr><th>断言</th><th>结果</th><th>证据</th></tr></thead><tbody>${assertionRows}</tbody></table>
${report.findings.length ? `<h3>产品缺陷 / 走查发现</h3>${report.findings.map((finding) => `<section class="step bad"><h2>${escapeHtml(finding.id)}</h2><p>${escapeHtml(finding.summary)}</p><pre>${escapeHtml(JSON.stringify(finding, null, 2))}</pre></section>`).join('\n')}` : ''}
<h3>MCP server 收到的方法</h3><table><thead><tr><th>method</th><th>次数</th></tr></thead><tbody>${methodRows}</tbody></table>
<h3>文档响应头</h3><table><thead><tr><th>header</th><th>值</th></tr></thead><tbody>${headerRows}</tbody></table>
<h3>逐步截图</h3>${stepBlocks}
</body></html>`;
  fs.writeFileSync(path.join(EVIDENCE, 'report.html'), html);
  await browser.close();
  console.log(
    JSON.stringify(
      {
        run: RUN,
        pass: report.pass,
        error: report.error ?? null,
        assertions: Object.fromEntries(Object.entries(assertions).map(([k, v]) => [k, v.ok])),
        mcpMethods: report.mcp.methods,
        toolCalls: report.mcp.toolCalls.map((call) => call.name),
      },
      null,
      2,
    ),
  );
  process.exit(report.pass ? 0 : 1);
}
