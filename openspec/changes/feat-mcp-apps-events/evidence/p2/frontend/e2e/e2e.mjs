import fs from 'node:fs';

const playwright = await import(
  process.env.PLAYWRIGHT_CORE || '/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs',
);
const { chromium } = playwright;

const REPORT = '/tmp/p2fe-e2e/report.json';
const CONSOLE = '/tmp/p2fe-e2e/console.log';
fs.writeFileSync(CONSOLE, '');
const report = { url: 'http://127.0.0.1:13679/?fixture=1', steps: [], console: [], pageErrors: [], failedRequests: [] };
const logConsole = (m) => {
  const line = `${m.type()}: ${m.text()}`;
  report.console.push(line);
  fs.appendFileSync(CONSOLE, line + '\n');
};
const browser = await chromium.launch({ executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: true });
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
page.on('console', logConsole);
page.on('pageerror', (e) => { report.pageErrors.push(e.message); });
page.on('requestfailed', (r) => { report.failedRequests.push({ url: r.url(), error: r.failure()?.errorText }); });

const step = (name, detail) => { report.steps.push({ name, detail, at: new Date().toISOString() }); };

try {
  await page.goto(report.url, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('[aria-label="打开会话 天气面板"]', { timeout: 15000 });
  step('session-list', 'loaded');

  await page.click('[aria-label="打开会话 天气面板"]');
  await page.waitForSelector('text=上海现在 24°C', { timeout: 15000 });
  step('session-open', 'weather session rendered');

  // The tool row's content slot is built with the row; the View mounts after
  // the catalogue fetch. Expand the row only if the app area is not revealed.
  const frameSelector = 'iframe[src*="/v1/mcp/apps/mcpapp_0123456789abcdef/view"]';
  try {
    await page.waitForSelector(frameSelector, { timeout: 3000, state: 'attached' });
  } catch {
    await page.locator('[aria-label="工具 mcp"]').last().click();
    await page.waitForSelector('text=ui://weather/dashboard', { timeout: 5000 });
  }
  await page.waitForSelector(frameSelector, { timeout: 20000, state: 'attached' });
  step('frame-attached', 'sandboxed frame mounted');
  // The row's content is built before it is revealed; open it if the frame is
  // still clipped so the size assertion measures the visible box.
  if (((await page.locator(frameSelector).boundingBox())?.height ?? 0) === 0) {
    await page.locator('[aria-label="工具 mcp"]').last().click();
    await page.waitForTimeout(300);
  }
  const frameHandle = await page.$(frameSelector);
  const sandbox = await frameHandle.getAttribute('sandbox');
  const src = await frameHandle.getAttribute('src');
  step('frame-mounted', { src, sandbox });

  // Wait for the full sequence inside the app document.
  const appFrame = page.frames().find((f) => f.url().includes('/v1/mcp/apps/mcpapp_0123456789abcdef/view'));
  if (!appFrame) throw new Error('app frame not found in page.frames()');
  await appFrame.waitForFunction(() => {
    const s = window.__e2e;
    if (!s) return false;
    const methods = s.frames.map((f) => f.method);
    return methods.includes('ui/notifications/tool-input') && methods.includes('ui/notifications/tool-result');
  }, { timeout: 20000 });

  const state = await appFrame.evaluate(() => window.__e2e);
  step('app-state', state);

  // The host consumes size-changed asynchronously; wait for the 360px box.
  await page.waitForFunction(() => {
    const el = document.querySelector('iframe[src*="/v1/mcp/apps/mcpapp_0123456789abcdef/view"]');
    return el && el.getBoundingClientRect().height >= 359;
  }, { timeout: 10000 });
  const box = await frameHandle.boundingBox();
  step('frame-box', box);

  // Host UI shows the bridge reached "ready".
  const hostText = await page.locator('body').innerText();
  const hostReady = hostText.includes('已连接');
  step('host-badge', { ready: hostReady, snippet: hostText.slice(0, 240) });

  // Wait for the isolation probe's fetch to settle.
  await appFrame.waitForFunction(() => window.__e2e && window.__e2e.cookieCheck !== null, { timeout: 10000 });
  const state2 = await appFrame.evaluate(() => window.__e2e);
  step('isolation', { isolation: state2.isolation, cookieCheck: state2.cookieCheck });

  await page.screenshot({ path: '/tmp/p2fe-e2e/after.png', fullPage: true });

  // P1 navigation defense: navigate the View to a third-party document. The
  // host must discard the authorized instance, reload its own app URL, and the
  // third-party page must not inherit tool-input/tool-result.
  await appFrame.evaluate(() => { location.href = "/evil.html"; });
  await page.waitForFunction(() => Array.from(document.querySelectorAll("iframe")).some((f) => (f.src || "").length === 0) === false, { timeout: 1000 }).catch(() => {});
  await page.waitForTimeout(700);
  const evilFrame = page.frames().find((f) => f.url().includes("/evil.html"));
  const evil = evilFrame ? await evilFrame.evaluate(() => window.__evil) : null;
  report.evil = evil;

  // The host reloads the app document (reload generation > 0) and re-handshakes.
  await page.waitForFunction(() => {
    const el = document.querySelector('iframe[src*="/v1/mcp/apps/mcpapp_0123456789abcdef/view"]');
    return el && /[?&]reload=\d+/.test(el.src);
  }, { timeout: 15000 });
  const reloadedSrc = await page.locator('iframe[src*="/v1/mcp/apps/"]').first().getAttribute('src');
  const reloadedFrame = page.frames().find((f) => f.url().includes('/v1/mcp/apps/mcpapp_0123456789abcdef/view'));
  if (!reloadedFrame) throw new Error('reloaded app frame not found');
  await reloadedFrame.waitForFunction(() => {
    const s = window.__e2e;
    const methods = s?.frames?.map((f) => f.method) ?? [];
    return methods.includes('ui/notifications/tool-input') && methods.includes('ui/notifications/tool-result');
  }, { timeout: 15000 });
  const reloadedState = await reloadedFrame.evaluate(() => window.__e2e);
  const evilReports = fs.readFileSync('/tmp/p2fe-e2e/server-headers.log', 'utf8')
    .split('\n')
    .filter((line) => line.startsWith('EVIL-REPORT '))
    .map((line) => { try { return JSON.parse(line.slice('EVIL-REPORT '.length)); } catch { return null; } })
    .filter(Boolean);
  report.navigation = {
    reloadedSrc,
    evilReports,
    reloadedFrames: reloadedState.frames.map((f) => f.method),
    evilMethods: evil ? evil.got.map((f) => f.method || "response") : null,
  };
  step('navigation-defense', report.navigation);

  // Direct header assertion from the served document (curl-equivalent in-page).
  const response = await page.request.get(src);
  report.documentHeaders = {
    status: response.status(),
    csp: response.headers()['content-security-policy'] ?? null,
    nosniff: response.headers()['x-content-type-options'] ?? null,
    referrer: response.headers()['referrer-policy'] ?? null,
    mime: response.headers()['x-mcp-app-mime'] ?? null,
    hash: response.headers()['x-mcp-app-hash'] ?? null,
    cache: response.headers()['cache-control'] ?? null,
  };
  step('document-headers', report.documentHeaders);

  report.pass = Boolean(
    sandbox && sandbox.split(/\s+/).includes('allow-scripts') && !sandbox.split(/\s+/).includes('allow-same-origin') &&
    state?.host?.protocolVersion === '2026-01-26' &&
    state.host?.hostInfo?.name === 'Keel' &&
    state.frames.some((f) => f.method === 'ui/notifications/tool-input') &&
    state.frames.some((f) => f.method === 'ui/notifications/tool-result') &&
    box && Math.abs(box.height - 360) < 2 &&
    hostReady &&
    state2.isolation && !String(state2.isolation.cookie).includes('keel_host_secret') &&
    String(state2.isolation.parentDom).startsWith('blocked') &&
    String(state2.isolation.localStorage).startsWith('blocked') &&
    state2.isolation.popupBlocked === true &&
    !String(state2.cookieCheck?.cookie ?? '').includes('keel_host_secret') &&
    report.documentHeaders.csp?.includes("frame-ancestors 'self'") &&
    report.documentHeaders.csp?.includes('sandbox allow-scripts') &&
    report.documentHeaders.nosniff === 'nosniff' &&
    report.documentHeaders.referrer === 'no-referrer' &&
    report.navigation && /[?&]reload=\d+/.test(report.navigation.reloadedSrc) &&
    report.navigation.reloadedFrames.includes('ui/notifications/tool-input') &&
    report.navigation.reloadedFrames.includes('ui/notifications/tool-result') &&
    (report.navigation.evilMethods ?? []).every((m) => !String(m).startsWith('ui/notifications/tool-')) &&
    report.navigation.evilReports.every((r) => (r.got ?? []).every((f) => !String(f.method ?? '').startsWith('ui/notifications/tool-')))
  );
} catch (error) {
  report.error = String(error?.stack ?? error);
  try { report.debug = await page.evaluate(() => ({ text: document.body.innerText.slice(-2500), iframes: Array.from(document.querySelectorAll('iframe')).map((i) => ({ src: i.src, sandbox: i.getAttribute('sandbox') })) })); } catch {}
  try { await page.screenshot({ path: '/tmp/p2fe-e2e/failure.png', fullPage: true }); } catch {}
} finally {
  fs.writeFileSync(REPORT, JSON.stringify(report, null, 2));
  await browser.close();
  console.log(JSON.stringify({ pass: report.pass, error: report.error, steps: report.steps.map((s) => s.name) }, null, 2));
  process.exit(report.pass ? 0 : 1);
}
