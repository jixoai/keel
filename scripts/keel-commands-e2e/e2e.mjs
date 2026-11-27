#!/usr/bin/env node
// Real-service browser evidence for change keel-plugin-commands (task 5.4).
//
// The lane drives the packaged web build against a real `keel serve` on a
// throwaway PostgreSQL database:
//
//   1. open the `/` catalog (builtin command) and assert the server order;
//   2. run the builtin `/compact` from the Composer and read its command row
//      beside the existing compaction divider;
//   3. install + approve the E2E plugin through the real two-phase lifecycle;
//   4. open the catalog again: the plugin commands are there, same order as
//      the API;
//   5. run the argument command through the generated form;
//   6. replay the same requestId through the API: same result, one execution;
//   7. run the destructive command: deny (refused row), then approve (result);
//   8. disable + uninstall the plugin and re-read the catalog: gone;
//   9. 390/768/1024/1440 screenshots, no horizontal overflow.
//
// Every step writes assertions and PNGs; report.mjs renders report.html.
// Secrets and the DSN stay in files the report never reads.
import { execFileSync, spawn } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';

const BASE = (process.env.E2E_BASE || '').replace(/\/$/, '');
const EVIDENCE = process.env.E2E_EVIDENCE || '';
const SHOTS = path.join(EVIDENCE, 'shots');
const DATA = path.join(EVIDENCE, 'data');
const BIN = process.env.E2E_KEEL_BIN || '';
const WEB_DIR = process.env.E2E_WEB_DIR || '';
const DSN_FILE = process.env.E2E_DSN_FILE || '';
const SECRET_FILE = process.env.E2E_SECRET_FILE || '';
const DB = process.env.E2E_DB || '';
const TENANT = process.env.E2E_TENANT || '';
const PORT = Number(process.env.E2E_PORT || 0);
const MODELS_CONFIG = process.env.E2E_MODELS_CONFIG || '';
const CONFIG_FILE = process.env.E2E_CONFIG_FILE || '';
const HOME = process.env.E2E_HOME || '';
const CAS_DIR = process.env.E2E_CAS_DIR || '';
const PLUGINS_ROOT = process.env.E2E_PLUGINS_ROOT || '';
const PLUGIN_SRC = process.env.E2E_PLUGIN_SRC || '';
const PLUGIN_STATE = process.env.E2E_PLUGIN_STATE || '';
const CHROME = process.env.E2E_CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const PLAYWRIGHT = process.env.PLAYWRIGHT_CORE || '';
const HEADLESS = process.env.E2E_HEADLESS !== 'false';
const PUBLIC_URL = process.env.E2E_PUBLIC_URL || '';

if (!BASE || !EVIDENCE || !BIN || !DSN_FILE || !SECRET_FILE || !DB || !PLUGIN_SRC) {
  console.error('missing E2E_BASE/E2E_EVIDENCE/E2E_KEEL_BIN/E2E_DSN_FILE/E2E_SECRET_FILE/E2E_DB/E2E_PLUGIN_SRC');
  process.exit(2);
}
fs.mkdirSync(SHOTS, { recursive: true });
fs.mkdirSync(DATA, { recursive: true });
fs.mkdirSync(path.join(EVIDENCE, 'runtime'), { recursive: true });

const DSN = fs.readFileSync(DSN_FILE, 'utf8').trim();
const SECRET = fs.readFileSync(SECRET_FILE, 'utf8').trim();
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const brief = (value, max = 420) => {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  return text && text.length > max ? text.slice(0, max) + '…' : text;
};

const report = {
  lane: 'keel-plugin-commands-e2e',
  base: BASE,
  publicUrl: PUBLIC_URL,
  startedAt: new Date().toISOString(),
  finishedAt: null,
  commit: process.env.E2E_HEAD_COMMIT || '',
  environment: {
    database: DB,
    postgres: psqlVersion(),
    node: process.version,
    chrome: CHROME,
    models: 'e2e-model (deterministic loopback provider; /compact summary included)',
    plugin: PLUGIN_SRC,
    databaseRowProbe: 'command_invocations (tenant, session, request_id)',
    publicDeployment: PUBLIC_URL ? PUBLIC_URL : '未部署（仅本地 127.0.0.1 证据）',
  },
  steps: [],
  console: [],
  pageErrors: [],
  failedRequests: [],
  failures: 0,
  pass: false,
  error: '',
};

let current = null;
let browser = null;
let context = null;
let page = null;

// A hung browser (or a step that never settles) must not leave the stack
// running forever: the watchdog exits and run.sh's cleanup stops the server.
const watchdog = setTimeout(() => {
  console.error('e2e watchdog: 8 minutes without completion');
  process.exit(2);
}, 8 * 60 * 1000);
watchdog.unref?.();
let screenshotSeq = 0;
const started = Date.now();

function psqlVersion() {
  try {
    return execFileSync('psql', ['--version'], { encoding: 'utf8' }).trim();
  } catch {
    return 'unavailable';
  }
}

/** psql without ever printing the DSN. */
function psql(sql) {
  const result = spawnSyncPsql(sql);
  if (result.status !== 0) throw new Error(`psql failed: ${brief(result.stderr || '')}`);
  return (result.stdout || '').trim();
}

function spawnSyncPsql(sql) {
  try {
    const out = execFileSync('psql', [DSN, '-Atc', sql], { encoding: 'utf8' });
    return { status: 0, stdout: out, stderr: '' };
  } catch (error) {
    return {
      status: typeof error.status === 'number' ? error.status : 1,
      stdout: typeof error.stdout === 'string' ? error.stdout : '',
      stderr: typeof error.stderr === 'string' ? error.stderr : String(error.message || error),
    };
  }
}

function beginStep(id, title) {
  current = {
    id,
    title,
    startedAt: new Date().toISOString(),
    status: 'running',
    checks: [],
    screenshots: [],
    details: {},
  };
  console.log(`\n== ${id} · ${title}`);
}

function finishStep(status) {
  if (!current) return;
  const failed = current.checks.some((entry) => !entry.ok);
  current.status = status || (failed ? 'failed' : 'passed');
  current.durationMs = Date.now() - new Date(current.startedAt).getTime();
  report.steps.push(current);
  console.log(`-- ${current.id}: ${current.status} (${current.checks.filter((c) => c.ok).length}/${current.checks.length})`);
  current = null;
}

function check(label, ok, actual, expected) {
  const entry = { label, ok: Boolean(ok), actual, expected };
  current?.checks.push(entry);
  if (!ok) {
    report.failures += 1;
    console.log(`  FAIL ${label} (actual: ${brief(actual)} want: ${brief(expected)})`);
  } else {
    console.log(`  ok   ${label}`);
  }
  return Boolean(ok);
}

function record(details) {
  if (current) Object.assign(current.details, details);
}

function saveData(name, value) {
  fs.writeFileSync(path.join(DATA, name), JSON.stringify(value, null, 2) + '\n');
}

// ---- server lifecycle ------------------------------------------------------

let server = null;
let serverLog = null;

function serverEnv() {
  return {
    ...process.env,
    KEEL_DSN: DSN,
    KEEL_SECRET_KEY: SECRET,
    KEEL_SERVE_TENANT: TENANT,
    KEEL_MODELS_CONFIG: MODELS_CONFIG,
    KEEL_CONFIG_FILE: CONFIG_FILE,
    KEEL_HOME: HOME,
    KEEL_SKILLS_ROOT: path.join(HOME, 'skills'),
    KEEL_CAS_DIR: CAS_DIR,
    KEEL_PLUGINS_ROOT: PLUGINS_ROOT,
    KEEL_COMMANDS_E2E_STATE: PLUGIN_STATE,
    KEEL_SERVE_WEB_DIR: WEB_DIR,
    E2E_PROVIDER_KEY: 'e2e-key',
  };
}

async function waitHealth(timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const response = await fetch(`${BASE}/v1/health`);
      if (response.ok) return await response.json();
    } catch {
      /* not up yet */
    }
    await sleep(200);
  }
  throw new Error('keel serve did not become healthy');
}

async function startServer() {
  const logPath = path.join(EVIDENCE, 'runtime', 'serve.log');
  fs.appendFileSync(logPath, `\n--- start ${new Date().toISOString()} ---\n`);
  serverLog = fs.openSync(logPath, 'a');
  server = spawn(BIN, ['serve', '--addr', `127.0.0.1:${PORT}`, '--auth', 'local-only', '--tenant', TENANT], {
    env: serverEnv(),
    stdio: ['ignore', serverLog, serverLog],
  });
  fs.writeFileSync(path.join(EVIDENCE, 'runtime', 'server.pid'), String(server.pid));
  return await waitHealth();
}

async function stopServer(signal = 'SIGKILL') {
  const child = server;
  server = null;
  if (!child) return { exited: true, signal, alreadyStopped: true };
  const exited = new Promise((resolve) => {
    child.once('exit', (code, sig) => resolve({ code, sig }));
  });
  try {
    child.kill(signal);
  } catch {
    /* already gone */
  }
  const result = await Promise.race([exited, sleep(5000).then(() => ({ timeout: true }))]);
  if (serverLog !== null) {
    try {
      fs.closeSync(serverLog);
    } catch {
      /* ignore */
    }
    serverLog = null;
  }
  try {
    fs.rmSync(path.join(EVIDENCE, 'runtime', 'server.pid'), { force: true });
  } catch {
    /* ignore */
  }
  return result;
}

// ---- API ------------------------------------------------------------------

let cookie = '';
let csrf = '';
let sessionDocument = null;

function cookieHeader(setCookie) {
  return (setCookie || [])
    .map((entry) => String(entry).split(';')[0])
    .filter((part) => part.includes('='))
    .join('; ');
}

async function readAuthSession() {
  const response = await fetch(`${BASE}/v1/auth/session`);
  if (!response.ok) throw new Error(`auth session: HTTP ${response.status}`);
  const body = await response.json();
  sessionDocument = body;
  cookie = cookieHeader(response.headers.getSetCookie ? response.headers.getSetCookie() : [response.headers.get('set-cookie')]);
  csrf = body.csrfToken || '';
  return body;
}

async function api(method, endpoint, body) {
  const headers = {};
  if (cookie) headers.Cookie = cookie;
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET' && method !== 'HEAD') headers['X-Keel-CSRF'] = csrf;
  const response = await fetch(`${BASE}${endpoint}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  let parsed = text;
  try {
    parsed = JSON.parse(text);
  } catch {
    /* a non-JSON body is kept as text */
  }
  return { status: response.status, body: parsed, headers: response.headers };
}

/** A Bot-owned session: only a bot-owned session can run a turn, and /compact
 * needs one settled turn (field: "session is not owned by a bot and cannot run"). */
async function createDeployment() {
  const accountId = sessionDocument?.account?.id || '';
  if (!accountId) throw new Error('local admin account id is missing from /v1/auth/session');
  const role = await api('POST', '/v1/roles', { name: 'E2E Commands Role', instruction: 'E2E commands' });
  if (role.status !== 201) throw new Error(`create role: HTTP ${role.status} ${brief(role.body)}`);
  const roleId = role.body.role.roleId;
  const profile = await api('POST', '/v1/profiles', { accountId, roleId });
  if (profile.status !== 201) throw new Error(`create profile: HTTP ${profile.status} ${brief(profile.body)}`);
  const profileId = profile.body.profile.profileId;
  const bot = await api('POST', '/v1/bots', {
    name: 'E2E Commands Bot',
    profileId,
    modelKind: 'text',
    defaultModel: { providerId: 'e2e', modelId: 'e2e-model' },
    harness: { tools: [], context: { instruction: 'E2E commands bot', permissions: [] } },
  });
  if (bot.status !== 201) throw new Error(`create bot: HTTP ${bot.status} ${brief(bot.body)}`);
  const botId = bot.body.bot.botId;
  const session = await api('POST', '/v1/sessions', { botId });
  if (session.status !== 201) throw new Error(`create session: HTTP ${session.status} ${brief(session.body)}`);
  return { accountId, roleId, profileId, botId, sessionId: session.body.sessionId };
}

async function sendInput(sessionId, text, requestId) {
  const response = await api('POST', `/v1/sessions/${sessionId}/input`, { text, requestId });
  if (response.status !== 201) throw new Error(`input: HTTP ${response.status} ${brief(response.body)}`);
  return response.body;
}

async function events(sessionId) {
  const response = await api('GET', `/v1/sessions/${sessionId}/events`);
  if (response.status !== 200) throw new Error(`events: HTTP ${response.status}`);
  return Array.isArray(response.body?.events) ? response.body.events : [];
}

async function waitEvents(sessionId, predicate, timeoutMs = 60000) {
  const deadline = Date.now() + timeoutMs;
  let last = [];
  while (Date.now() < deadline) {
    last = await events(sessionId);
    if (predicate(last)) return last;
    await sleep(400);
  }
  throw new Error(`events predicate timed out; last kinds: ${brief(last.map((event) => event.kind))}`);
}

async function catalog(sessionId, target = 'web') {
  const response = await api('GET', `/v1/sessions/${sessionId}/commands?target=${target}`);
  if (response.status !== 200) throw new Error(`catalog: HTTP ${response.status} ${brief(response.body)}`);
  return response.body;
}

async function approve(decision) {
  const decisionId = decision?.decisionId || decision?.id;
  if (!decisionId) throw new Error(`approval: no decision id in ${brief(decision)}`);
  const answer = await api('POST', `/v1/plugin-approvals/${decisionId}`, { optionId: 'allow' });
  if (answer.status !== 200) throw new Error(`approval answer: HTTP ${answer.status} ${brief(answer.body)}`);
  return decisionId;
}

async function installPlugin() {
  const opener = 'e2e-install-commands';
  const asked = await api('POST', '/v1/plugins/install', { path: PLUGIN_SRC, requestId: opener });
  if (asked.status !== 202) throw new Error(`install ask: HTTP ${asked.status} ${brief(asked.body)}`);
  const decisionId = await approve(asked.body.decision);
  const installed = await api('POST', '/v1/plugins/install', {
    path: PLUGIN_SRC,
    requestId: opener,
    decisionId,
  });
  if (installed.status !== 201 && installed.status !== 200) {
    throw new Error(`install: HTTP ${installed.status} ${brief(installed.body)}`);
  }
  const enableOpener = 'e2e-enable-commands';
  const enableAsked = await api('POST', '/v1/plugins/e2e.commands/enable', { requestId: enableOpener });
  if (enableAsked.status !== 202) {
    throw new Error(`enable ask: HTTP ${enableAsked.status} ${brief(enableAsked.body)}`);
  }
  const enableDecision = await approve(enableAsked.body.decision);
  const enabled = await api('POST', '/v1/plugins/e2e.commands/enable', {
    requestId: enableOpener,
    decisionId: enableDecision,
  });
  if (enabled.status !== 200) throw new Error(`enable: HTTP ${enabled.status} ${brief(enabled.body)}`);
  return { installed: installed.body, enabled: enabled.body };
}

async function disablePlugin() {
  const disabled = await api('POST', '/v1/plugins/e2e.commands/disable');
  if (disabled.status !== 200) throw new Error(`disable: HTTP ${disabled.status} ${brief(disabled.body)}`);
  return disabled.body;
}

async function uninstallPlugin() {
  const uninstalled = await api('POST', '/v1/plugins/e2e.commands/uninstall');
  if (uninstalled.status !== 200) {
    throw new Error(`uninstall: HTTP ${uninstalled.status} ${brief(uninstalled.body)}`);
  }
  return uninstalled.body;
}

function sidecarCalls() {
  const file = path.join(PLUGIN_STATE, 'calls.ndjson');
  if (!fs.existsSync(file)) return [];
  return fs
    .readFileSync(file, 'utf8')
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => {
      try {
        return JSON.parse(line);
      } catch {
        return { tool: 'unparseable', line };
      }
    });
}

// ---- browser ---------------------------------------------------------------

const shot = async (page, file, label) => {
  await page.waitForTimeout(250);
  const relative = `shots/${String(++screenshotSeq).padStart(2, '0')}-${file}`;
  await page.screenshot({ path: path.join(EVIDENCE, relative), fullPage: true });
  current?.screenshots.push(relative);
  console.log(`  shot ${relative} · ${label}`);
};

const overflow = async (page, label) => {
  const value = await page.evaluate(() => ({
    innerWidth: window.innerWidth,
    docScrollWidth: document.documentElement.scrollWidth,
    bodyScrollWidth: document.body ? document.body.scrollWidth : null,
  }));
  record({ [`overflow-${label}`]: value });
  return check(`${label}: no horizontal overflow`, value.docScrollWidth <= value.innerWidth, value, { docScrollWidth: '<= innerWidth' });
};

const bodyText = async (page) => (await page.evaluate(() => document.body.innerText || '')).replace(/\s+/g, ' ');

/** The conversation scrolls inside the list, so a target below the internal
 * viewport would be absent from a full-page screenshot. Bring it into view
 * before the shot (best effort: the following wait/screenshot still asserts). */
const scrollToText = async (page, text) => {
  await page.getByText(text, { exact: false }).last().scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(350);
};

const waitText = async (page, text, timeout = 15000) => {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const textNow = await bodyText(page);
    if (textNow.includes(text)) return true;
    await page.waitForTimeout(200);
  }
  throw new Error(`text never appeared: ${text}`);
};

const openSession = async (page, sessionId) => {
  await page.goto(`${BASE}/`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('[data-testid="keel-system-bar"]', { timeout: 20000 });
  await page.waitForSelector(`[data-testid="session-row-${sessionId}"]`, { timeout: 20000 });
  await page.click(`[data-testid="session-row-${sessionId}"]`);
  await page.waitForSelector('[data-testid="composer-input"]', { timeout: 20000 });
  await page.waitForTimeout(400);
};

const clearComposer = async (page) => {
  await page.click('[data-testid="composer-input"]');
  await page.keyboard.press('Meta+A');
  await page.keyboard.press('Backspace');
};

const typeDraft = async (page, text) => {
  await clearComposer(page);
  await page.keyboard.type(text, { delay: 20 });
};

/** The kit renders a form control's testID on its wrapper; the native input
 * (or textarea) is inside it. */
const fillField = async (page, id, value) => {
  const wrapper = page.locator(`[data-testid="${id}"]`);
  await wrapper.waitFor({ state: 'visible', timeout: 10000 });
  const input = wrapper.locator('input, textarea').first();
  if (await input.count()) {
    await input.fill(String(value));
    return;
  }
  await wrapper.click();
  await page.keyboard.type(String(value));
};

const menuRows = async (page) =>
  page.evaluate(() =>
    [...document.querySelectorAll('[data-testid^="composer-commands-row-"]')].map((node) =>
      (node.getAttribute('data-testid') || '').replace('composer-commands-row-', ''),
    ),
  );

// ---- main ------------------------------------------------------------------

async function main() {
  console.log('starting keel serve…');
  const health = await startServer();
  check('keel serve healthy', Boolean(health), health, { status: 'ok' });
  const auth = await readAuthSession();
  check('local-only super admin session', auth.authenticated === true && auth.localAdmin === true, {
    authenticated: auth.authenticated,
    localAdmin: auth.localAdmin,
    mode: auth.mode,
  });

  // A session with one settled turn: /compact needs something to summarize.
  const created = await createDeployment();
  const sessionId = created.sessionId;
  check('bot-owned session created', typeof sessionId === 'string' && sessionId.length > 0, { sessionId, botId: created.botId });
  // Three settled turns: manual compaction needs a safe cut point, and the
  // configured retain budget (100 tokens) leaves earlier turns outside it.
  for (let turn = 1; turn <= 3; turn += 1) {
    await sendInput(sessionId, `E2E 命令：第 ${turn} 轮正常对话，保留足够的历史以便手动压缩。`, `e2e-commands-warmup-${turn}`);
    await waitEvents(
      sessionId,
      (all) => all.filter((event) => event.kind === 'run.finished').length >= turn,
      60000,
    );
  }
  const warmupEvents = await events(sessionId);
  check(
    'three settled turns before compact',
    warmupEvents.filter((event) => event.kind === 'run.finished').length >= 3,
    warmupEvents.filter((event) => event.kind === 'run.finished').length,
    3,
  );
  // Narrow the retained tail so a manual compaction has a safe cut (the
  // tenant policy is 100 tokens; a session may narrow, never widen).
  const policy = await api('PUT', `/v1/sessions/${sessionId}/checkpoint-policy`, { retainTokens: 1 });
  check('session retain budget narrowed to 1', policy.status === 200, policy.status, 200);
  console.log(`session ${sessionId} warmed up`);
  record({ sessionId, botId: created.botId });

  const playwright = await import(PLAYWRIGHT);
  const { chromium } = playwright;
  browser = await chromium.launch({ executablePath: CHROME, headless: HEADLESS });
  context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  page = await context.newPage();
  const pageErrors = [];
  const consoleErrors = [];
  const failedRequests = [];
  let step = 'boot';
  page.on('console', (message) => {
    if (message.type() !== 'error') return;
    const text = brief(message.text());
    // The destructive-denial step EXPECTS the API to answer 403: the browser
    // logs that failed fetch as a console error. It is the tested behaviour,
    // not an app error; keep it out of the error list but record it.
    if (step === 'destructive-denied' && text.includes('status of 403')) {
      record({ expectedConsole: [text] });
      return;
    }
    consoleErrors.push({ step, text });
  });
  page.on('pageerror', (error) => pageErrors.push({ step, error: brief(error.stack || String(error)) }));
  page.on('requestfailed', (request) => {
    if (request.url().includes('favicon')) return;
    failedRequests.push({ step, url: brief(request.url()), error: brief(request.failure()?.errorText) });
  });

  // ---- 1. catalog before the plugin --------------------------------------
  step = 'catalog-builtin';
  beginStep(step, '打开目录：内置命令可发现，顺序来自服务端');
  await openSession(page, sessionId);
  await typeDraft(page, '/');
  await page.waitForSelector('[data-testid="composer-commands"]', { timeout: 15000 });
  // The menu mounts before its first catalog read settles; wait for the row
  // (the assertion below then compares the rendered order, not a loading face).
  await page.waitForSelector('[data-testid="composer-commands-row-compact"]', { timeout: 15000 });
  const before = await catalog(sessionId, 'web');
  check('builtin catalog has compact', before.commands.some((row) => row.name === 'compact' && row.source === 'builtin'), before.commands.map((row) => row.name), ['compact']);
  check('no plugin command before install', !before.commands.some((row) => row.source === 'plugin'), before.commands.filter((row) => row.source === 'plugin').map((row) => row.name), []);
  const beforeRows = await menuRows(page);
  check('rendered rows match the server order', beforeRows.join(',') === before.commands.map((row) => row.name).join(','), beforeRows, before.commands.map((row) => row.name));
  await shot(page, 'catalog-builtin.png', '内置命令目录');
  await overflow(page, 'catalog-builtin');
  finishStep();

  // ---- 2. builtin /compact ----------------------------------------------
  step = 'compact';
  beginStep(step, '执行内置 /compact：命令行 + 既有压缩分隔线');
  await page.click('[data-testid="composer-commands-pick-compact"]');
  const compactEvents = await waitEvents(
    sessionId,
    (all) => all.some((event) => event.kind === 'command.invoked' && event.payload?.command === 'compact' && event.payload?.status === 'ok'),
    30000,
  );
  const compactRow = compactEvents.find(
    (event) => event.kind === 'command.invoked' && event.payload?.command === 'compact' && event.payload?.status === 'ok',
  );
  check('durable command.invoked ok for compact', Boolean(compactRow), compactRow?.payload?.status, 'ok');
  check(
    'compact performed (a real compaction, not a no-op)',
    compactRow?.payload?.data?.performed === true,
    compactRow?.payload?.data,
    { performed: true },
  );
  await waitText(page, '上下文已压缩', 30000);
  await waitText(page, '/compact', 15000);
  check('compaction divider rendered', (await bodyText(page)).includes('上下文已压缩'), 'present', 'present');
  check('no user/assistant message pretends to be the command', !(await bodyText(page)).includes('已发送沉淀请求'), 'absent', 'absent');
  await shot(page, 'compact-result.png', '/compact 命令行与压缩分隔线');
  finishStep();

  // ---- 3. install the plugin through the real lifecycle ------------------
  step = 'install-plugin';
  beginStep(step, '安装插件命令：真实两阶段审批（install + enable）');
  const plugin = await installPlugin();
  check('plugin installed', Boolean(plugin.installed?.installed !== false), brief(plugin.installed), 'installed');
  check('plugin enabled', Boolean(plugin.enabled?.id === 'e2e.commands' || plugin.enabled?.plugin?.id === 'e2e.commands' || plugin.enabled), brief(plugin.enabled), 'e2e.commands enabled');
  const after = await catalog(sessionId, 'web');
  const pluginRows = after.commands.filter((row) => row.source === 'plugin');
  check('plugin commands are in the catalog', pluginRows.map((row) => row.name).join(',') === 'e2e.commands:refresh,e2e.commands:note,e2e.commands:purge', pluginRows.map((row) => row.name), ['e2e.commands:refresh', 'e2e.commands:note', 'e2e.commands:purge']);
  check('catalog order is builtin → plugin', after.commands.slice(0, 4).map((row) => row.name).join(',') === 'compact,e2e.commands:refresh,e2e.commands:note,e2e.commands:purge', after.commands.map((row) => row.name).slice(0, 4), ['compact', 'e2e.commands:refresh', 'e2e.commands:note', 'e2e.commands:purge']);
  // Re-open the menu: the opening reads the catalog again.
  await clearComposer(page);
  await page.keyboard.type('/');
  await page.waitForSelector('[data-testid="composer-commands-row-e2e.commands:refresh"]', { timeout: 15000 });
  const installedRows = await menuRows(page);
  check('UI rows match the fresh catalog', installedRows.join(',') === after.commands.map((row) => row.name).join(','), installedRows, after.commands.map((row) => row.name));
  await shot(page, 'catalog-with-plugin.png', '安装后目录（内置 → 插件）');
  await overflow(page, 'catalog-with-plugin');
  finishStep();

  // ---- 4. run the argument command through the form ----------------------
  step = 'plugin-form';
  beginStep(step, '执行插件命令：args_schema 生成表单 → 结构化结果');
  await page.click('[data-testid="composer-commands-pick-e2e.commands:refresh"]');
  await page.waitForSelector('[data-testid="composer-command-form"]', { timeout: 15000 });
  await fillField(page, 'command-field-path', 'docs');
  await page.click('[data-testid="composer-command-execute"]');
  await waitText(page, '"indexed":7', 30000);
  await waitText(page, '/e2e.commands:refresh', 10000);
  await scrollToText(page, '"indexed":7');
  const refreshEvents = await waitEvents(
    sessionId,
    (all) => all.some((event) => event.kind === 'command.invoked' && event.payload?.command === 'e2e.commands:refresh' && event.payload?.status === 'ok'),
    30000,
  );
  const refreshEvent = refreshEvents.find(
    (event) => event.kind === 'command.invoked' && event.payload?.command === 'e2e.commands:refresh' && event.payload?.status === 'ok',
  );
  check('plugin command result row', Boolean(refreshEvent), refreshEvent?.payload?.command, 'e2e.commands:refresh');
  const refreshRequestId = refreshEvent?.payload?.requestId;
  check('result row carries a requestId', typeof refreshRequestId === 'string' && refreshRequestId.length > 0, refreshRequestId);
  check('sidecar executed refresh once', sidecarCalls().filter((call) => call.tool === 'refresh').length === 1, sidecarCalls().map((call) => call.tool), ['refresh']);
  await shot(page, 'plugin-result.png', '插件命令结果行');
  finishStep();

  // ---- 5. replay the same requestId --------------------------------------
  step = 'replay';
  beginStep(step, '重放同 requestId：同结果、不二次执行');
  const replay = await api('POST', `/v1/sessions/${sessionId}/commands/e2e.commands:refresh`, {
    args: { path: 'docs' },
    requestId: refreshRequestId,
  });
  check('replay answers 200', replay.status === 200, replay.status, 200);
  check(
    'replay result is the first result',
    replay.body?.data?.structuredContent?.indexed === 7 && replay.body?.requestId === refreshRequestId,
    { indexed: replay.body?.data?.structuredContent?.indexed, requestId: replay.body?.requestId },
    { indexed: 7, requestId: refreshRequestId },
  );
  check('replay did not execute the tool again', sidecarCalls().filter((call) => call.tool === 'refresh').length === 1, sidecarCalls().filter((call) => call.tool === 'refresh').length, 1);
  const rowCount = Number(
    psql(`select count(*) from command_invocations where tenant_id='${TENANT}' and session_id='${sessionId}' and request_id='${refreshRequestId.replace(/'/g, "''")}'`),
  );
  check('one durable invocation row for the requestId', rowCount === 1, rowCount, 1);
  await scrollToText(page, '/e2e.commands:refresh');
  await shot(page, 'replay-row.png', '重放后的对话（仍只有一行）');
  finishStep();

  // ---- 6. destructive: deny then approve ---------------------------------
  step = 'destructive-denied';
  beginStep(step, '破坏性命令：审批拒绝 → 已拒绝行，无副作用');
  await typeDraft(page, '/e2e.commands:purge');
  await page.waitForSelector('[data-testid="composer-commands-pick-e2e.commands:purge"]', { timeout: 15000 });
  await page.click('[data-testid="composer-commands-pick-e2e.commands:purge"]');
  await waitText(page, '是否执行破坏性命令', 20000);
  await waitText(page, '等待审批', 20000);
  await scrollToText(page, '是否执行破坏性命令');
  await shot(page, 'destructive-pending.png', '破坏性命令等待审批');
  await page.getByText('取消', { exact: true }).click();
  await waitText(page, '已拒绝', 30000);
  await waitText(page, 'decision_refused', 15000);
  await scrollToText(page, '已拒绝');
  const refusedEvents = await waitEvents(
    sessionId,
    (all) => all.some((event) => event.kind === 'command.invoked' && event.payload?.command === 'e2e.commands:purge' && event.payload?.status === 'refused'),
    30000,
  );
  check('refusal is a durable command row', refusedEvents.some((event) => event.kind === 'command.invoked' && event.payload?.status === 'refused'), 'refused', 'refused');
  check('denied command never ran the tool', sidecarCalls().filter((call) => call.tool === 'purge').length === 0, sidecarCalls().filter((call) => call.tool === 'purge').length, 0);
  await shot(page, 'destructive-refused.png', '审批拒绝后的命令行');
  finishStep();

  step = 'destructive-approved';
  beginStep(step, '破坏性命令：审批允许 → 只执行一次并给出结果');
  await typeDraft(page, '/e2e.commands:purge');
  await page.waitForSelector('[data-testid="composer-commands-pick-e2e.commands:purge"]', { timeout: 15000 });
  await page.click('[data-testid="composer-commands-pick-e2e.commands:purge"]');
  await waitText(page, '是否执行破坏性命令', 20000);
  await page.getByText('执行', { exact: true }).click();
  await waitText(page, '"purged":7', 30000);
  await waitText(page, '/e2e.commands:purge', 10000);
  await scrollToText(page, '"purged":7');
  check('approved command executed exactly once', sidecarCalls().filter((call) => call.tool === 'purge').length === 1, sidecarCalls().filter((call) => call.tool === 'purge').length, 1);
  await shot(page, 'destructive-result.png', '审批允许后的结果行');
  finishStep();

  // ---- 7. four viewports ------------------------------------------------
  step = 'viewports';
  beginStep(step, '4.5 视觉：390/768/1024/1440 目录与表单无横向溢出');
  for (const viewport of [
    { width: 390, height: 844 },
    { width: 768, height: 1024 },
    { width: 1024, height: 800 },
    { width: 1440, height: 900 },
  ]) {
    await page.setViewportSize(viewport);
    await page.waitForTimeout(400);
    await typeDraft(page, '/');
    await page.waitForSelector('[data-testid="composer-commands"]', { timeout: 15000 });
    await shot(page, `catalog-${viewport.width}.png`, `${viewport.width} 命令目录`);
    await overflow(page, `catalog-${viewport.width}`);
    await typeDraft(page, '/e2e.commands:refresh');
    await page.waitForSelector('[data-testid="composer-commands-pick-e2e.commands:refresh"]', { timeout: 15000 });
    await page.click('[data-testid="composer-commands-pick-e2e.commands:refresh"]');
    await page.waitForSelector('[data-testid="composer-command-form"]', { timeout: 15000 });
    await shot(page, `form-${viewport.width}.png`, `${viewport.width} 参数表单`);
    await overflow(page, `form-${viewport.width}`);
    await page.click('[data-testid="composer-command-cancel"]');
  }
  await page.setViewportSize({ width: 1440, height: 900 });
  await shot(page, 'workspace-final.png', '最终工作区');
  finishStep();

  // ---- 8. disable + uninstall revokes the catalog ------------------------
  step = 'disable-uninstall';
  beginStep(step, '停用/卸载插件：目录与执行入口一起撤销');
  await disablePlugin();
  const disabledCatalog = await catalog(sessionId, 'web');
  check('disabled plugin commands are gone', !disabledCatalog.commands.some((row) => row.source === 'plugin'), disabledCatalog.commands.filter((row) => row.source === 'plugin').map((row) => row.name), []);
  await typeDraft(page, '/');
  await page.waitForSelector('[data-testid="composer-commands"]', { timeout: 15000 });
  await page.waitForTimeout(500);
  const disabledRows = await menuRows(page);
  check('UI no longer draws the disabled commands', !disabledRows.some((name) => name.startsWith('e2e.commands:')), disabledRows, []);
  await shot(page, 'catalog-after-disable.png', '停用后的目录');
  const executeAfterDisable = await api('POST', `/v1/sessions/${sessionId}/commands/e2e.commands:refresh`, {
    args: { path: 'docs' },
    requestId: 'e2e-after-disable',
  });
  check('execution entry is 404 after disable', executeAfterDisable.status === 404, executeAfterDisable.status, 404);
  await uninstallPlugin();
  const uninstalledCatalog = await catalog(sessionId, 'web');
  check('uninstall keeps the command gone', !uninstalledCatalog.commands.some((row) => row.source === 'plugin'), uninstalledCatalog.commands.map((row) => row.name), ['compact']);
  await shot(page, 'after-uninstall.png', '卸载后的工作区');
  finishStep();

  // ---- report ----------------------------------------------------------
  report.finishedAt = new Date().toISOString();
  report.console = consoleErrors;
  report.pageErrors = pageErrors;
  report.failedRequests = failedRequests;
  report.pass = report.failures === 0 && report.steps.every((entry) => entry.status === 'passed');
  report.environment.durationSeconds = Math.round((Date.now() - started) / 1000);
  saveData('catalog-before.json', before);
  saveData('catalog-after.json', after);
  saveData('sidecar-calls.json', sidecarCalls());
  fs.writeFileSync(path.join(EVIDENCE, 'steps.json'), JSON.stringify(report, null, 2) + '\n');

  await context.close();
  await browser.close();
  await stopServer('SIGTERM');
  clearTimeout(watchdog);
  console.log(`\nresult: ${report.pass ? 'PASS' : 'FAIL'} · failures=${report.failures}`);
  process.exit(report.pass ? 0 : 1);
}

try {
  await main();
} catch (error) {
  report.error = String(error?.stack || error);
  if (current) {
    current.status = 'failed';
    finishStep('failed');
  }
  try {
    await context?.close();
  } catch {
    /* ignore */
  }
  try {
    await browser?.close();
  } catch {
    /* ignore */
  }
  try {
    await stopServer('SIGKILL');
  } catch {
    /* ignore */
  }
  if (!report.finishedAt) report.finishedAt = new Date().toISOString();
  report.pass = false;
  fs.writeFileSync(path.join(EVIDENCE, 'steps.json'), JSON.stringify(report, null, 2) + '\n');
  console.error(report.error);
  process.exit(1);
}
