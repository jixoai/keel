#!/usr/bin/env node
// Real-browser degradation E2E for MCP Apps (change feat-mcp-apps-events,
// P6A2a). It drives the production web build against a real `keel serve`, a
// real stdio MCP server and a real Chrome, and asserts two failure surfaces of
// the app host:
//
//   handshake  ui://weather/broken answers ui/initialize but never sends
//              ui/notifications/initialized. Within the host's 5 s deadline the
//              row must become the readable text fallback (reason + retry), the
//              broken iframe must be gone, and the good app in the same session
//              must keep its instance and its app-only tools/call result.
//
//   csp        ui://weather/leaky declares only https://api.fixture.test in
//              _meta.ui.csp.connectDomains and fetches
//              https://evil.fixture.test/leak. The browser must block it
//              (in-frame securitypolicyviolation + console), the local TCP trap
//              mapped to evil.fixture.test must see zero connections, and the
//              host page must not navigate.
//
// A per-run PNG per step, report.json, and a self-contained report.html are
// written to E2E_EVIDENCE. Nothing credential-shaped is recorded: the password
// is typed into the login field and never written; there is no cookie jar,
// token, DSN or Authorization value in the output.
//
// Env:
//   E2E_BASE            http://127.0.0.1:13682
//   E2E_EVIDENCE        output directory (created)
//   E2E_RUN             run label (handshake, csp)
//   E2E_SCENARIO        handshake | csp
//   E2E_MCP_LOG         MCP fixture JSONL log
//   E2E_WIRE_LOG        stub provider JSONL log
//   E2E_LEAK_LOG        TCP trap JSONL log (csp only)
//   E2E_LEAK_PORT       TCP trap port (csp only, default 13685)
//   E2E_LEAK_HOST       fake host mapped to the trap (default evil.fixture.test)
//   E2E_USERNAME        account username for the login gate
//   E2E_PASSWORD        account password (or KEEL_ACCOUNT_USERNAME/PASSWORD)
//   PLAYWRIGHT_CORE     optional playwright-core entry
//   E2E_CHROME          optional Chrome executable

import fs from 'node:fs';
import path from 'node:path';

const BASE = process.env.E2E_BASE || 'http://127.0.0.1:13682';
const EVIDENCE = process.env.E2E_EVIDENCE || '/tmp/mcp-apps-degrade-e2e';
const RUN = process.env.E2E_RUN || 'run-1';
const SCENARIO = process.env.E2E_SCENARIO || 'handshake';
const MCP_LOG = process.env.E2E_MCP_LOG || '';
const WIRE_LOG = process.env.E2E_WIRE_LOG || '';
const LEAK_LOG = process.env.E2E_LEAK_LOG || '';
const LEAK_PORT = Number(process.env.E2E_LEAK_PORT || '13685');
const LEAK_HOST = process.env.E2E_LEAK_HOST || 'evil.fixture.test';
const USERNAME = process.env.E2E_USERNAME || process.env.KEEL_ACCOUNT_USERNAME || '';
const PASSWORD = process.env.E2E_PASSWORD || process.env.KEEL_ACCOUNT_PASSWORD || '';
const CHROME = process.env.E2E_CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const PLAYWRIGHT =
  process.env.PLAYWRIGHT_CORE || '/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs';

if (!USERNAME || !PASSWORD) {
  console.error('E2E_USERNAME/E2E_PASSWORD (or KEEL_ACCOUNT_USERNAME/PASSWORD) are not set');
  process.exit(2);
}
if (SCENARIO !== 'handshake' && SCENARIO !== 'csp') {
  console.error(`unknown E2E_SCENARIO: ${SCENARIO}`);
  process.exit(2);
}

const SHOTS = path.join(EVIDENCE, 'screenshots');
fs.mkdirSync(SHOTS, { recursive: true });

const playwright = await import(PLAYWRIGHT);
const { chromium } = playwright;
const launchArgs = ['--no-proxy-server'];
if (SCENARIO === 'csp') {
  // Any non-blocked request to the fixture's undeclared host lands on the local
  // trap, so "zero connections" is a real server-side observation, not an
  // assumption about DNS.
  launchArgs.push(`--host-resolver-rules=MAP ${LEAK_HOST} 127.0.0.1:${LEAK_PORT}`);
}
const browser = await chromium.launch({ executablePath: CHROME, headless: true, args: launchArgs });
const context = await browser.newContext({ viewport: { width: 1280, height: 940 } });
const page = await context.newPage();
// Every wait is bounded: the lane's outer guard is `timeout 240`, and no single
// step may hang longer than 20 s.
page.setDefaultTimeout(20000);
page.setDefaultNavigationTimeout(20000);

const report = {
  lane: 'P6A2a',
  scenario: SCENARIO,
  run: RUN,
  startedAt: new Date().toISOString(),
  baseUrl: BASE,
  steps: [],
  assertions: {},
  catalogue: null,
  app: null,
  timing: null,
  csp: null,
  console: [],
  pageErrors: [],
  failedRequests: [],
  mainFrameNavigations: [],
  findings: [],
  pass: false,
};

let shotIndex = 0;
const shot = async (name) => {
  shotIndex += 1;
  const file = `${String(shotIndex).padStart(2, '0')}-${name}.png`;
  const full = path.join(SHOTS, file);
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
  if (!ok) throw new Error(`assertion failed: ${name}${detail !== undefined ? `: ${JSON.stringify(detail)}` : ''}`);
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

page.on('console', (message) => report.console.push(`${message.type()}: ${message.text()}`));
page.on('pageerror', (error) => report.pageErrors.push(String(error?.message ?? error)));
page.on('requestfailed', (request) =>
  report.failedRequests.push({ url: request.url(), error: request.failure()?.errorText ?? '' }),
);
page.on('framenavigated', (frame) => {
  if (frame === page.mainFrame()) report.mainFrameNavigations.push(frame.url());
});

const bodyText = () => page.evaluate(() => document.body?.innerText ?? '');

const findAppFrame = async (appId) => {
  const handles = await page.$$('iframe[src*="/v1/mcp/apps/"]');
  for (const handle of handles.reverse()) {
    const src = await handle.getAttribute('src');
    if (!src || !src.includes(`/v1/mcp/apps/${appId}/view`)) continue;
    const box = await handle.boundingBox();
    if (!box || box.height <= 0) continue;
    const frame = await handle.contentFrame();
    if (frame) return { handle, frame, box, src };
  }
  return null;
};

const waitForAppFrame = async (appId, timeout = 20000) => {
  const deadline = Date.now() + timeout;
  for (;;) {
    const found = await findAppFrame(appId);
    if (found) return found;
    if (Date.now() > deadline) throw new Error(`the app iframe for ${appId} did not mount`);
    await page.waitForTimeout(100);
  }
};

const waitForState = async (frame, predicate, timeout, label) => {
  try {
    await frame.waitForFunction(predicate, undefined, { timeout });
  } catch (error) {
    throw new Error(`${label} (within ${timeout}ms): ${String(error?.message ?? error)}`);
  }
  return frame.evaluate(() => ({ e2e: window.__e2e ?? null, broken: window.__brokenE2E ?? null, leaky: window.__leakyE2E ?? null }));
};

/** Open the session row (if the list is showing). */
const openSession = async () => {
  const row = await page.$('[aria-label^="打开会话"]');
  if (row) await row.click();
  await page.waitForSelector('[aria-label="工具调用组"], [aria-label="工具 mcp"]', {
    timeout: 20000,
    state: 'visible',
  });
};

/** Expand one Collapsible wrapper when its `aria-expanded` state says closed.
 * The wrapper is `role=group`; the trigger inside carries the state. */
const ensureOpen = async (selector) => {
  const wrappers = page.locator(selector);
  const count = await wrappers.count();
  for (let index = 0; index < count; index += 1) {
    const wrapper = wrappers.nth(index);
    const stateNode = wrapper.locator('[aria-expanded]').first();
    let expanded = null;
    if ((await stateNode.count()) > 0) {
      try {
        expanded = await stateNode.getAttribute('aria-expanded');
      } catch {
        expanded = null;
      }
    }
    if (expanded === 'true') continue;
    try {
      const button = wrapper.locator('button').first();
      if ((await button.count()) > 0) await button.click({ timeout: 3000 });
      else await wrapper.click({ timeout: 3000 });
    } catch {
      /* a row that disappeared between count() and click is not fatal */
    }
    await page.waitForTimeout(150);
  }
  return count;
};

/** Expand every layer of the tool transcript: the segment group, then each
 * tool family (two aggregate `mcp` calls are grouped by family), then each
 * call row. A call row's app slot is only in the tree while it is open. */
const expandToolRows = async () => {
  await ensureOpen('[role="group"][aria-label="工具调用组"]');
  await ensureOpen('[role="group"][aria-label^="工具家族"]');
  await ensureOpen('[role="group"][aria-label="工具 mcp"]');
  return page.locator('[role="group"][aria-label="工具 mcp"]').count();
};

/** The catalogue is the binding's single source; one GET also gives the
 * per-app CSP header and document URL for the evidence. */
const readCatalogue = async () => {
  const response = await page.request.get(`${BASE}/v1/mcp/apps`, { timeout: 20000 });
  const body = await response.json();
  report.catalogue = { status: response.status(), apps: body.apps ?? [] };
  return report.catalogue.apps;
};

const appByURI = (apps, uri) => apps.find((app) => app.resourceUri === uri) ?? null;

/** The row containing a resource URI; the app panel prints the URI in mono. */
const rowForURI = (uri) => page.locator('[aria-label="工具 mcp"]').filter({ hasText: uri }).first();

try {
  // ---- 0. login gate -----------------------------------------------------
  await page.goto(BASE, { waitUntil: 'domcontentloaded', timeout: 20000 });
  await page.waitForSelector('input[aria-label="用户名"], input[type="password"]', {
    timeout: 20000,
    state: 'visible',
  });
  await step('login-gate', { url: page.url() });

  const usernameInput = await page.$('input[aria-label="用户名"]');
  const passwordInput = await page.$('input[aria-label="密码"], input[type="password"]');
  if (!usernameInput || !passwordInput) throw new Error('login username/password fields not found');
  await usernameInput.fill(USERNAME);
  await passwordInput.fill(PASSWORD);
  const submit =
    (await page.$('[aria-label="进入 Keel"]')) ?? (await page.$('[aria-label="进入"]')) ?? (await page.$('text=进入'));
  if (!submit) throw new Error('login submit button not found');
  await submit.click();
  await page.waitForSelector('[aria-label^="打开会话"], [aria-label="工具调用组"], [aria-label="工具 mcp"]', {
    timeout: 20000,
    state: 'visible',
  });
  await step('login-submitted', { authenticated: true });

  // The P6A1 finding still holds on this build: detect() reads the deployment
  // capabilities before the login gate, and login() does not re-read them, so
  // can("mcp.apps") is false on the first page load after authentication. A
  // reload is the legitimate next user action; this lane does not change the
  // product. Record the known defect, then continue on the reloaded page.
  await page.reload({ waitUntil: 'domcontentloaded', timeout: 20000 });
  await page.waitForSelector('[aria-label^="打开会话"], [aria-label="工具调用组"], [aria-label="工具 mcp"]', {
    timeout: 20000,
    state: 'visible',
  });
  report.findings.push({
    id: 'login-capability-stale',
    summary:
      '登录后能力位未刷新：首屏 can("mcp.apps") 仍为 false（P6A1 已记录）；本次降级用例在登录后 reload 一次再验证。',
    impact: 'P2/P6 前端 store 已知缺陷；本 lane 不改产品代码，也不把该缺陷算作降级场景失败。',
  });
  await step('post-login-reload', { reloaded: true });

  // ---- 1. open the session and expand every tool row ---------------------
  await openSession();
  await step('session-open', { toolRow: '工具 mcp' });

  const expectedURIs = SCENARIO === 'handshake' ? ['ui://weather/dashboard', 'ui://weather/broken'] : ['ui://weather/leaky'];
  // Install the timing probe before the rows expand, so each iframe's own load
  // event is observed even when the document comes from the HTTP cache.
  if (SCENARIO === 'handshake') {
    const probe = await page.evaluate(() => {
      const state = (window.__p6a2a = {
        iframeSeen: {},
        iframeLoaded: {},
        brokenFallbackAt: null,
        observedAt: performance.now(),
      });
      const scan = () => {
        for (const frame of document.querySelectorAll('iframe[src*="/v1/mcp/apps/"]')) {
          const src = frame.src;
          if (!(src in state.iframeSeen)) {
            state.iframeSeen[src] = performance.now();
            frame.addEventListener('load', () => {
              state.iframeLoaded[src] = performance.now();
            });
          }
        }
        const fallback = document.querySelector('[aria-label="交互界面不可用"]');
        if (fallback && state.brokenFallbackAt === null) {
          const row = fallback.closest('[aria-label="工具 mcp"]');
          if (row && row.innerText.includes('ui://weather/broken')) state.brokenFallbackAt = performance.now();
        }
      };
      new MutationObserver(scan).observe(document.body, { childList: true, subtree: true });
      scan();
      return state;
    });
    report.timing = { probeInstalledAt: probe.observedAt };
  }
  await expandToolRows();
  for (const uri of expectedURIs) {
    await page.waitForFunction((expected) => document.body.innerText.includes(expected), uri, { timeout: 20000 });
  }
  await step('tool-rows-expanded', { expectedURIs });

  const apps = await readCatalogue();
  const dashboard = appByURI(apps, 'ui://weather/dashboard');
  const broken = appByURI(apps, 'ui://weather/broken');
  const leaky = appByURI(apps, 'ui://weather/leaky');
  check('catalogue-apps', apps.length === 3, apps.map((app) => app.resourceUri));
  check('catalogue-dashboard', Boolean(dashboard), dashboard);
  if (SCENARIO === 'handshake') {
    check('catalogue-broken', Boolean(broken), broken);
  } else {
    check('catalogue-leaky', Boolean(leaky), leaky);
  }
  const mcpRows = readJsonl(MCP_LOG);
  const readURIs = mcpRows.filter((row) => row.method === 'resources/read').map((row) => row.params?.uri);
  for (const uri of expectedURIs) {
    check(`mcp-resource-read-${uri.replace(/[^a-z]+/g, '-')}`, readURIs.includes(uri), readURIs);
  }
  report.catalogue.documentUrls = apps.map((app) => ({ resourceUri: app.resourceUri, documentUrl: app.documentUrl, csp: app.csp }));
  await step('catalogue', report.catalogue, { screenshot: false });

  const navigationBaseline = report.mainFrameNavigations.length;
  const urlBaseline = page.url();

  if (SCENARIO === 'handshake') {
    // ---- 2. the good app completes the handshake -------------------------
    const good = await waitForAppFrame(dashboard.appId, 20000);
    await waitForState(
      good.frame,
      () => Boolean(window.__e2e && window.__e2e.frames.some((f) => f.method === 'ui/notifications/tool-result')),
      20000,
      'the good app did not receive tool-result',
    );
    await waitForState(good.frame, () => Boolean(window.__e2e?.toolsCall), 20000, 'the good app tools/call did not settle');
    const goodState = await good.frame.evaluate(() => window.__e2e);
    check('good-handshake', goodState.host?.protocolVersion === '2026-01-26', goodState.host?.protocolVersion);
    check('good-tool-result', goodState.toolResult?.structuredContent?.temperatureC === 24, goodState.toolResult?.structuredContent);
    check('good-tools-call', Boolean(goodState.toolsCall?.result) && !goodState.toolsCall?.error, goodState.toolsCall);
    const goodBootedAt = goodState.bootedAt;
    check('good-phase-visible', Boolean(await findAppFrame(dashboard.appId)), 'the good iframe is visible');
    await step('good-app-ready', { bootedAt: goodBootedAt, toolsCall: goodState.toolsCall });

    // ---- 3. the broken app must fall back within the 5 s deadline --------
    const brokenFrame = await waitForAppFrame(broken.appId, 20000);
    const brokenSrc = brokenFrame.src;
    check('broken-iframe-mounted', Boolean(brokenSrc.includes(broken.appId)), brokenSrc);

    // The host destroys the instance after HANDSHAKE_TIMEOUT_MS (5 s from the
    // frame load). 15 s is the outer bound; the elapsed check below is the
    // actual deadline assertion.
    await page.waitForSelector('[aria-label="交互界面不可用"]', { state: 'visible', timeout: 15000 });
    await page.waitForFunction(
      (appId) => !document.querySelector(`iframe[src*="/v1/mcp/apps/${appId}/view"]`),
      broken.appId,
      { timeout: 10000 },
    );
    const afterFallback = await page.evaluate(() => window.__p6a2a);
    report.timing = {
      ...report.timing,
      iframeSeen: afterFallback.iframeSeen,
      iframeLoaded: afterFallback.iframeLoaded,
      brokenFallbackAt: afterFallback.brokenFallbackAt,
      brokenSrc,
    };
    const entriesFor = (record) =>
      Object.entries(record)
        .filter(([src]) => src.includes(broken.appId))
        .map(([, at]) => at);
    const seen = entriesFor(afterFallback.iframeSeen);
    const loaded = entriesFor(afterFallback.iframeLoaded);
    // The load event is the host's timer start; when the cached document beats
    // the MutationObserver, the first sighting is a safe upper bound for it.
    const starts = loaded.length ? loaded : seen;
    check('broken-load-observed', starts.length >= 1, { brokenSrc, seen, loaded });
    const fallbackAt = afterFallback.brokenFallbackAt;
    check('broken-fallback-observed', typeof fallbackAt === 'number', afterFallback.brokenFallbackAt);
    const elapsed = fallbackAt - Math.min(...starts);
    report.timing.elapsedMs = Math.round(elapsed);
    // The timer is 5000 ms from the frame's load. The measured span starts at
    // that same load event; 4 s lower bound proves it was the deadline, not an
    // instant failure, and 8 s allows local paint/polling jitter.
    check('broken-timeout-within-deadline', elapsed >= 4000 && elapsed <= 8000, {
      elapsedMs: Math.round(elapsed),
      deadlineMs: 5000,
    });

    const fallbackRow = await page.evaluate(() => {
      const fallbacks = Array.from(document.querySelectorAll('[aria-label="交互界面不可用"]'));
      for (const fallback of fallbacks) {
        const row = fallback.closest('[aria-label="工具 mcp"]');
        if (!row || !row.innerText.includes('ui://weather/broken')) continue;
        const retry = row.querySelector('[aria-label="重试交互界面"]');
        return {
          rowText: row.innerText,
          retryLabel: retry?.getAttribute('aria-label') ?? null,
          retryDisabled: retry?.hasAttribute('disabled') ?? null,
          fallbackText: fallback.innerText,
        };
      }
      return null;
    });
    report.app = { fallback: fallbackRow };
    check('broken-fallback-row', Boolean(fallbackRow), fallbackRow);
    check('broken-fallback-reason', String(fallbackRow?.fallbackText ?? '').includes('交互界面在 5 秒内未完成握手'), fallbackRow?.fallbackText);
    check('broken-fallback-readable', String(fallbackRow?.fallbackText ?? '').includes('文本结果仍可阅读'), fallbackRow?.fallbackText);
    check('broken-fallback-retry', fallbackRow?.retryLabel === '重试交互界面' && fallbackRow?.retryDisabled !== true, fallbackRow);
    const toolTextVisible = String(fallbackRow?.rowText ?? '').includes('坏面板的文本结果仍然可读');
    check('broken-text-result-still-visible', toolTextVisible, fallbackRow?.rowText?.slice(0, 400));
    await step('handshake-timeout-fallback', report.app.fallback);

    // ---- 4. the same session's good app is untouched ---------------------
    const goodAfter = await findAppFrame(dashboard.appId);
    check('good-still-mounted', Boolean(goodAfter), 'the good iframe is still mounted');
    const goodAfterState = await goodAfter.frame.evaluate(() => window.__e2e);
    check('good-same-instance', goodAfterState.bootedAt === goodBootedAt, {
      before: goodBootedAt,
      after: goodAfterState.bootedAt,
    });
    check('good-tools-call-survives', Boolean(goodAfterState.toolsCall?.result) && !goodAfterState.toolsCall?.error, goodAfterState.toolsCall);
    const goodRowFallback = await page.evaluate(() => {
      const rows = Array.from(document.querySelectorAll('[aria-label="工具 mcp"]'));
      const row = rows.find((node) => node.innerText.includes('ui://weather/dashboard'));
      return row ? Boolean(row.querySelector('[aria-label="交互界面不可用"]')) : null;
    });
    check('good-row-has-no-fallback', goodRowFallback === false, goodRowFallback);
    await step('good-app-unaffected', { bootedAt: goodAfterState.bootedAt, toolsCall: goodAfterState.toolsCall });

    // ---- 5. the retry button really starts a new handshake ----------------
    const retryRow = rowForURI('ui://weather/broken');
    await retryRow.locator('[aria-label="重试交互界面"]').click({ timeout: 10000 });
    await page.waitForSelector('[aria-label="交互界面不可用"]', { state: 'hidden', timeout: 10000 });
    const retryStartedAt = await page.evaluate(() => performance.now());
    await page.waitForSelector('[aria-label="交互界面不可用"]', { state: 'visible', timeout: 15000 });
    const retryElapsed = (await page.evaluate(() => performance.now())) - retryStartedAt;
    const retryText = await rowForURI('ui://weather/broken').innerText();
    check('retry-refalls-back', retryText.includes('交互界面在 5 秒内未完成握手'), retryText.slice(0, 400));
    check('retry-within-deadline', retryElapsed >= 4000 && retryElapsed <= 9000, Math.round(retryElapsed));
    report.timing.retryElapsedMs = Math.round(retryElapsed);
    await step('handshake-retry-fallback', { retryElapsedMs: Math.round(retryElapsed) });

    const goodFinal = await findAppFrame(dashboard.appId);
    const goodFinalState = goodFinal ? await goodFinal.frame.evaluate(() => window.__e2e) : null;
    check('good-survives-retry', Boolean(goodFinalState?.toolsCall?.result), goodFinalState?.toolsCall ?? null);

    check('host-no-navigation', page.url() === urlBaseline, { before: urlBaseline, after: page.url() });
    check('host-no-main-frame-navigation', report.mainFrameNavigations.length === navigationBaseline, {
      navigations: report.mainFrameNavigations.slice(navigationBaseline),
    });
    const unexpectedFailures = report.failedRequests.filter((request) => request.error !== 'net::ERR_ABORTED');
    check('no-page-errors', report.pageErrors.length === 0, report.pageErrors);
    check('no-failed-requests', unexpectedFailures.length === 0, unexpectedFailures);
  }

  if (SCENARIO === 'csp') {
    // ---- 2. the leaky app completes the handshake ------------------------
    const leakyFrame = await waitForAppFrame(leaky.appId, 20000);
    await waitForState(
      leakyFrame.frame,
      () => Boolean(window.__leakyE2E && window.__leakyE2E.host && window.__leakyE2E.toolResult),
      20000,
      'the leaky app did not complete the handshake',
    );
    await waitForState(leakyFrame.frame, () => Boolean(window.__leakyE2E?.leak?.done), 10000, 'the leaking fetch did not settle');
    const leakyState = await leakyFrame.frame.evaluate(() => window.__leakyE2E);
    report.app = {
      host: leakyState.host,
      toolResult: leakyState.toolResult,
      leak: leakyState.leak,
      violations: leakyState.violations,
      errors: leakyState.errors,
    };
    check('leaky-handshake', leakyState.host?.protocolVersion === '2026-01-26', leakyState.host?.protocolVersion);
    check('leaky-tool-result', leakyState.toolResult?.structuredContent?.fixture === 'leaky', leakyState.toolResult?.structuredContent);
    check('leak-attempted', leakyState.leak?.attemptedAt > 0, leakyState.leak);
    check('leak-rejected', Boolean(leakyState.leak?.error), leakyState.leak);

    // ---- 3. the browser enforced the document's connect-src --------------
    const violations = leakyState.violations ?? [];
    report.csp = { violations, console: [], document: null, trap: null, control: null };
    const connectViolation = violations.find(
      (violation) => String(violation.directive).includes('connect-src') && String(violation.blockedURI).includes(LEAK_HOST),
    );
    check('csp-in-frame-violation', Boolean(connectViolation), violations);
    report.csp.console = report.console.filter(
      (line) => /content security policy/i.test(line) && line.includes(LEAK_HOST),
    );
    const cspConsoleOrFailure =
      report.csp.console.length > 0 ||
      report.failedRequests.some((request) => request.url.includes(LEAK_HOST));
    check('csp-console-or-requestfailed', cspConsoleOrFailure, {
      console: report.csp.console,
      failedRequests: report.failedRequests.filter((request) => request.url.includes(LEAK_HOST)),
    });

    // The document's own header is the authority the browser enforced.
    const documentResponse = await page.request.get(`${BASE}${leaky.documentUrl}`, { timeout: 20000 });
    const headers = documentResponse.headers();
    const cspHeader = headers['content-security-policy'] ?? '';
    report.csp.document = {
      status: documentResponse.status(),
      csp: cspHeader,
      appMime: headers['x-mcp-app-mime'] ?? null,
    };
    check('csp-document-status', report.csp.document.status === 200, report.csp.document.status);
    check('csp-declared-domain-allowed', cspHeader.includes("connect-src 'self' https://api.fixture.test"), cspHeader);
    check('csp-undeclared-domain-absent', !cspHeader.includes(LEAK_HOST), cspHeader);
    check('csp-strict-default', cspHeader.includes("default-src 'none'") && cspHeader.includes('sandbox allow-scripts'), cspHeader);

    // ---- 4. the local trap proves the request never left the browser -----
    const trapRows = () =>
      readJsonl(LEAK_LOG).filter((row) => row.event !== 'listening');
    const duringApp = trapRows();
    report.csp.trap = { log: path.basename(LEAK_LOG), connectionsDuringApp: duringApp };
    check('leak-server-not-hit', duringApp.length === 0, duringApp);

    // Negative control: the same Chrome, same resolver map, navigates to the
    // fake host outside the app. If the trap sees this connection, the zero
    // above is a real block rather than a broken trap.
    const control = await context.newPage();
    try {
      await control.goto(`https://${LEAK_HOST}/leak`, { waitUntil: 'commit', timeout: 6000 });
    } catch {
      /* the fake host has no TLS; the connection attempt is the evidence */
    }
    let controlRows = [];
    const controlDeadline = Date.now() + 6000;
    while (Date.now() < controlDeadline && controlRows.length === 0) {
      controlRows = trapRows();
      if (controlRows.length === 0) await page.waitForTimeout(200);
    }
    await control.close();
    report.csp.control = { connections: controlRows };
    check('leak-trap-control', controlRows.length >= 1, { trapLog: LEAK_LOG, rows: controlRows });
    check('leak-app-attempt-blocked', duringApp.length === 0 && controlRows.length >= 1, {
      duringApp: duringApp.length,
      control: controlRows.length,
    });
    await step('csp-leak-blocked', report.csp);

    // ---- 5. the host page did not navigate and the View stayed up --------
    check('host-no-navigation', page.url() === urlBaseline, { before: urlBaseline, after: page.url() });
    check('host-no-main-frame-navigation', report.mainFrameNavigations.length === navigationBaseline, {
      navigations: report.mainFrameNavigations.slice(navigationBaseline),
    });
    const leakyRowFallback = await page.evaluate(() => {
      const rows = Array.from(document.querySelectorAll('[aria-label="工具 mcp"]'));
      const row = rows.find((node) => node.innerText.includes('ui://weather/leaky'));
      return row ? Boolean(row.querySelector('[aria-label="交互界面不可用"]')) : null;
    });
    check('leaky-row-has-no-fallback', leakyRowFallback === false, leakyRowFallback);
    check('leaky-iframe-still-mounted', Boolean(await findAppFrame(leaky.appId)), 'the leaky iframe is still mounted');
    const text = await bodyText();
    check('leaky-status-connected', text.includes('已连接'), { sample: text.slice(-400) });
    check('leaky-text-result-visible', text.includes('泄漏夹具的文本结果仍然可读'), { sample: text.slice(-600) });

    const otherFailures = report.failedRequests.filter(
      (request) => request.error !== 'net::ERR_ABORTED' && !request.url.includes(LEAK_HOST),
    );
    check('no-page-errors', report.pageErrors.length === 0, report.pageErrors);
    check('no-unrelated-failed-requests', otherFailures.length === 0, otherFailures);
  }

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
      url: location.href,
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
  const assertionRows = Object.entries(report.assertions)
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
  const html = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>P6A2a · ${escapeHtml(SCENARIO)} · MCP Apps 降级 E2E · ${escapeHtml(RUN)}</title>
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
<h1>P6A2a · ${escapeHtml(SCENARIO === 'handshake' ? '握手超时 → 文本兜底' : 'CSP 拒绝未声明域名')}</h1>
<div class="meta">
  run <strong>${escapeHtml(RUN)}</strong> · ${escapeHtml(report.startedAt)} → ${escapeHtml(report.finishedAt)} ·
  <span class="verdict ${report.pass ? 'pass' : 'fail'}">${report.pass ? 'PASS' : 'FAIL'}</span>
  ${report.error ? `<div><strong>error:</strong> <code>${escapeHtml(report.error)}</code></div>` : ''}
</div>
<p>链路：真实 <code>keel serve</code> → 真实 stdio MCP server → 真实 Chrome（playwright-core + 系统 Chrome）。
证据只记录断言、方法名、状态与大小；不含 cookie/token/DSN/Authorization。</p>
<h3>断言</h3><table><thead><tr><th>断言</th><th>结果</th><th>证据</th></tr></thead><tbody>${assertionRows}</tbody></table>
${report.findings.length ? `<h3>走查发现</h3>${report.findings.map((finding) => `<section class="step bad"><h2>${escapeHtml(finding.id)}</h2><p>${escapeHtml(finding.summary)}</p><pre>${escapeHtml(JSON.stringify(finding, null, 2))}</pre></section>`).join('\n')}` : ''}
<h3>逐步截图</h3>${stepBlocks}
</body></html>`;
  fs.writeFileSync(path.join(EVIDENCE, 'report.html'), html);
  await browser.close();
  console.log(
    JSON.stringify(
      {
        scenario: SCENARIO,
        run: RUN,
        pass: report.pass,
        error: report.error ?? null,
        assertions: Object.fromEntries(Object.entries(assertions).map(([key, value]) => [key, value.ok])),
        timing: report.timing,
      },
      null,
      2,
    ),
  );
  process.exit(report.pass ? 0 : 1);
}
