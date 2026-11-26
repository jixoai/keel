#!/usr/bin/env node
// Browser + API evidence for change keel-checkpoint.
//
// The lane drives the packaged web build against a real `keel serve` on a
// throwaway PostgreSQL database:
//
//   empty → create a Bot-owned session → tenant/effective/override view →
//   UI save a narrowing → automatic compaction (threshold) → manual policy
//   toggle (disabled 409 then enabled 201 + replay) → busy/loading faces →
//   over-wide request clamped by the tenant ceiling → kill -9 → error face →
//   restart → recovery metric + policy survives → reset → permission face.
//
// Every step writes assertions and PNGs; report.mjs renders them. Secrets and
// the DSN stay in files the report never reads.
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
const MODEL_PORT = Number(process.env.E2E_MODEL_PORT || 0);
const MODELS_CONFIG = process.env.E2E_MODELS_CONFIG || '';
const CONFIG_FILE = process.env.E2E_CONFIG_FILE || '';
const HOME = process.env.E2E_HOME || '';
const CAS_DIR = process.env.E2E_CAS_DIR || '';
const CHROME = process.env.E2E_CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const PLAYWRIGHT = process.env.PLAYWRIGHT_CORE || '';
const HEADLESS = process.env.E2E_HEADLESS !== 'false';
const PUBLIC_URL = process.env.E2E_PUBLIC_URL || '';

if (!BASE || !EVIDENCE || !BIN || !DSN_FILE || !SECRET_FILE || !DB) {
  console.error('missing E2E_BASE/E2E_EVIDENCE/E2E_KEEL_BIN/E2E_DSN_FILE/E2E_SECRET_FILE/E2E_DB');
  process.exit(2);
}
fs.mkdirSync(SHOTS, { recursive: true });
fs.mkdirSync(DATA, { recursive: true });
fs.mkdirSync(path.join(EVIDENCE, 'runtime'), { recursive: true });

const DSN = fs.readFileSync(DSN_FILE, 'utf8').trim();
const SECRET = fs.readFileSync(SECRET_FILE, 'utf8').trim();
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const brief = (value, max = 320) => {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  return text && text.length > max ? text.slice(0, max) + '…' : text;
};

const report = {
  lane: 'keel-checkpoint-e2e',
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
    models: 'e2e-model (contextWindow 30000)',
    tenantPolicy: 'config.toml: compaction.enabled=true, thresholdTokens=1000, maxCheckpoints=3, ttlHours=168',
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
  if (result.status !== 0) {
    throw new Error(`psql failed: ${brief(result.stderr || '')}`);
  }
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
  report.steps.push(current);
  console.log(`\n[step] ${id}: ${title}`);
  return current;
}

function finishStep(status = 'passed') {
  if (!current) return;
  current.status = status;
  current.durationMs = Date.now() - new Date(current.startedAt).getTime();
  current = null;
}

function check(label, ok, actual, expected) {
  const entry = { label, ok: Boolean(ok), actual: actual === undefined ? undefined : brief(actual), expected };
  if (current) current.checks.push(entry);
  if (!ok) report.failures += 1;
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${label}${ok ? '' : ` (actual: ${brief(actual)} want: ${brief(expected)})`}`);
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
  server = spawn(
    BIN,
    ['serve', '--addr', `127.0.0.1:${PORT}`, '--auth', 'local-only', '--tenant', TENANT],
    { env: serverEnv(), stdio: ['ignore', serverLog, serverLog] },
  );
  fs.writeFileSync(path.join(EVIDENCE, 'runtime', 'server.pid'), String(server.pid));
  const health = await waitHealth();
  return health;
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

async function policy(sessionId) {
  const response = await api('GET', `/v1/sessions/${sessionId}/checkpoint-policy`);
  if (response.status !== 200) throw new Error(`GET policy: HTTP ${response.status} ${brief(response.body)}`);
  return response.body;
}

async function putPolicy(sessionId, update) {
  const response = await api('PUT', `/v1/sessions/${sessionId}/checkpoint-policy`, update);
  if (response.status !== 200) throw new Error(`PUT policy: HTTP ${response.status} ${brief(response.body)}`);
  return response.body;
}

/** A compact request can land while the previous turn's lease is still
 * settling; `busy` is the documented transient (the policy refusal is decided
 * after the lease). Retry it so the assertion sees the durable answer. */
async function compactSettled(sessionId, requestId, timeoutMs = 20000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const response = await api('POST', `/v1/sessions/${sessionId}/compact`, { requestId });
    if (response.body?.status !== 'busy' || Date.now() > deadline) return response;
    await sleep(250);
  }
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

async function metrics() {
  const response = await fetch(`${BASE}/metrics`);
  const text = await response.text();
  const out = {};
  for (const line of text.split('\n')) {
    const match = line.match(/^(keel_checkpoint_[a-z_]+)(?:\{[^}]*\})?\s+([0-9.eE+-]+)$/);
    if (match) out[match[1]] = (out[match[1]] || 0) + Number(match[2]);
  }
  return out;
}

// ---- browser --------------------------------------------------------------

let playwright = null;
let browser = null;
let page = null;
let context = null;

const sel = (id) => `[data-testid="${id}"]`;

async function bodyText() {
  return (await page.evaluate(() => document.body.innerText || '')).replace(/\s+/g, ' ').trim();
}

async function waitText(regex, timeoutMs = 20000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const text = await bodyText();
    if (regex.test(text)) return text;
    await sleep(200);
  }
  throw new Error(`text not found: ${regex}; body: ${brief(await bodyText())}`);
}

async function chooseSelect(selectId, optionText) {
  await page.click(sel(selectId));
  await sleep(250);
  const option = page.getByText(optionText, { exact: true }).last();
  await option.waitFor({ state: 'visible', timeout: 5000 });
  await option.click();
  await sleep(250);
}

async function resize(width) {
  await page.setViewportSize({ width, height: width <= 430 ? 844 : 1000 });
  await sleep(350);
}

async function shot(name) {
  screenshotSeq += 1;
  const file = `${String(screenshotSeq).padStart(2, '0')}-${name}.png`;
  await page.screenshot({ path: path.join(SHOTS, file), fullPage: true });
  if (current) current.screenshots.push(`shots/${file}`);
  return `shots/${file}`;
}

async function overflow(label) {
  const value = await page.evaluate(() => ({
    innerWidth: window.innerWidth,
    docScrollWidth: document.documentElement.scrollWidth,
    bodyScrollWidth: document.body.scrollWidth,
  }));
  check(`${label}: no horizontal overflow`, value.docScrollWidth <= value.innerWidth + 1 && value.bodyScrollWidth <= value.innerWidth + 1, value, {
    innerWidth: value.innerWidth,
  });
  return value;
}

async function dismissNotice() {
  const close = page.getByLabel('关闭提示');
  if (await close.count()) {
    await close.first().click().catch(() => {});
    await sleep(150);
  }
}

async function openCheckpoints({ fresh = false } = {}) {
  if (fresh) {
    await page.goto(`${BASE}/settings`, { waitUntil: 'domcontentloaded' });
  }
  await page.waitForSelector(sel('settings-section-checkpoints'), { timeout: 30000 });
  await page.click(sel('settings-section-checkpoints'));
  await page.waitForSelector(sel('settings-checkpoint-section'), { timeout: 20000 });
}

async function fillField(id, value) {
  const locator = page.locator(`${sel(id)} input, input${sel(id)}`).first();
  await locator.waitFor({ state: 'visible', timeout: 10000 });
  await locator.fill(String(value));
}

async function clickAndWaitSaved() {
  await dismissNotice();
  await page.click(sel('settings-checkpoint-save'));
  await page.waitForSelector(sel('settings-checkpoint-saving'), { state: 'visible', timeout: 5000 }).catch(() => {});
  await page.waitForSelector(sel('settings-checkpoint-saving'), { state: 'detached', timeout: 30000 }).catch(() => {});
  await waitText(/来源：会话覆盖/, 30000);
}

// ---- steps ----------------------------------------------------------------

async function createDeployment() {
  const accountId = sessionDocument?.account?.id || '';
  if (!accountId) throw new Error('local admin account id is missing from /v1/auth/session');

  const role = await api('POST', '/v1/roles', { name: 'E2E Checkpoint Role', instruction: 'E2E checkpoint policy' });
  if (role.status !== 201) throw new Error(`create role: ${role.status} ${brief(role.body)}`);
  const roleId = role.body.role.roleId;

  const profile = await api('POST', '/v1/profiles', { accountId, roleId });
  if (profile.status !== 201) throw new Error(`create profile: ${profile.status} ${brief(profile.body)}`);
  const profileId = profile.body.profile.profileId;

  const bot = await api('POST', '/v1/bots', {
    name: 'E2E Checkpoint Bot',
    profileId,
    modelKind: 'text',
    defaultModel: { providerId: 'e2e', modelId: 'e2e-model' },
    harness: { tools: [], context: { instruction: 'E2E checkpoint bot', permissions: [] } },
  });
  if (bot.status !== 201) throw new Error(`create bot: ${bot.status} ${brief(bot.body)}`);
  const botId = bot.body.bot.botId;

  const session = await api('POST', '/v1/sessions', { botId });
  if (session.status !== 201) throw new Error(`create session: ${session.status} ${brief(session.body)}`);
  const sessionId = session.body.sessionId;

  return { accountId, roleId, profileId, botId, sessionId };
}

async function runAll() {
  // ---- 0. stack + empty face ---------------------------------------------
  beginStep('empty', '启动真实栈并进入检查点分区（空态）');
  const health = await startServer();
  record({ health, port: PORT, modelPort: MODEL_PORT, webDir: WEB_DIR });
  check('keel serve healthy', Boolean(health), health, true);
  const auth = await readAuthSession();
  check('local-only super admin session', auth.authenticated === true && auth.localAdmin === true && auth.account?.isSuperAdmin === true, {
    authenticated: auth.authenticated,
    localAdmin: auth.localAdmin,
    isSuperAdmin: auth.account?.isSuperAdmin,
  });
  record({ tenantId: auth.tenantId, account: auth.account?.username });

  playwright = await import(PLAYWRIGHT);
  browser = await playwright.chromium.launch({ executablePath: CHROME, headless: HEADLESS });
  context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, locale: 'zh-CN' });
  page = await context.newPage();
  page.setDefaultTimeout(20000);
  page.on('console', (message) => {
    if (message.type() === 'error') report.console.push({ step: current?.id || 'boot', text: message.text() });
  });
  page.on('pageerror', (error) => report.pageErrors.push({ step: current?.id || 'boot', error: String(error?.stack || error) }));
  page.on('requestfailed', (request) => {
    if (request.url().endsWith('/favicon.ico')) return;
    report.failedRequests.push({ step: current?.id || 'boot', url: request.url(), error: request.failure()?.errorText });
  });

  await page.goto(`${BASE}/settings`, { waitUntil: 'domcontentloaded' });
  await openCheckpoints();
  await page.waitForSelector(sel('settings-checkpoint-empty'), { timeout: 30000 });
  const emptyText = await bodyText();
  check('empty face text', emptyText.includes('还没有可配置的会话'), emptyText.includes('还没有可配置的会话'), true);
  await shot('empty-1440');
  await resize(390);
  await shot('empty-390');
  await overflow('empty 390');
  await resize(1440);
  finishStep();

  // ---- 1. create session + tenant policy ---------------------------------
  beginStep('tenant', '创建 Bot 会话：读取中骨架 → 租户/生效策略');
  const created = await createDeployment();
  record(created);
  // The first projection has no previous view, so it is the real loading face:
  // hold the GET open and capture the skeleton before letting it land.
  let holdLoad = 0;
  await page.route('**/v1/sessions/*/checkpoint-policy', async (route) => {
    if (route.request().method() === 'GET' && holdLoad > 0) {
      holdLoad -= 1;
      await sleep(2500);
    }
    await route.continue();
  });
  holdLoad = 1;
  await page.getByText('刷新会话列表', { exact: true }).click();
  await page.waitForSelector(sel('settings-checkpoint-loading'), { state: 'visible', timeout: 10000 });
  await shot('loading-1440');
  check('skeleton visible on the first read', await page.locator(sel('settings-checkpoint-loading')).isVisible(), true, true);
  await page.waitForSelector(sel('settings-checkpoint-loading'), { state: 'detached', timeout: 20000 });
  await page.waitForSelector(sel('settings-checkpoint-effective'), { timeout: 30000 });
  const tenantView = await policy(created.sessionId);
  saveData('policy-tenant.json', tenantView);
  record({ tenantView });
  check('source starts at tenant', tenantView.source === 'tenant', tenantView.source, 'tenant');
  check('tenant threshold 1000', tenantView.tenant.thresholdTokens === 1000, tenantView.tenant.thresholdTokens, 1000);
  check('tenant maxCheckpoints 3', tenantView.tenant.maxCheckpoints === 3, tenantView.tenant.maxCheckpoints, 3);
  check('tenant ttlHours 168', tenantView.tenant.ttlHours === 168, tenantView.tenant.ttlHours, 168);
  check('effective equals tenant before any override', JSON.stringify(tenantView.effective) === JSON.stringify(tenantView.tenant), tenantView.effective, tenantView.tenant);
  const tenantText = await bodyText();
  check('UI shows tenant/default source', tenantText.includes('来源：租户默认'), tenantText.includes('来源：租户默认'), true);
  check('UI shows tenant line', tenantText.includes('租户默认：自动压缩 开'), tenantText.includes('租户默认：自动压缩 开'), true);
  check('UI shows no override yet', tenantText.includes('会话覆盖：未设置（完全继承租户策略）'), true, true);
  await shot('tenant-1440');
  await resize(390);
  await shot('tenant-390');
  await overflow('tenant 390');
  await resize(1440);
  finishStep();

  // ---- 2. UI save (auto compaction knobs) ---------------------------------
  beginStep('save', 'UI 保存会话收窄：threshold/retain/max/TTL');
  await fillField('settings-checkpoint-threshold', 1500);
  await fillField('settings-checkpoint-retain', 200);
  await fillField('settings-checkpoint-max', 2);
  await fillField('settings-checkpoint-ttl', 24);
  await clickAndWaitSaved();
  const savedView = await policy(created.sessionId);
  saveData('policy-saved.json', savedView);
  record({ savedView });
  check('source is session', savedView.source === 'session', savedView.source, 'session');
  check('effective threshold 1500', savedView.effective.thresholdTokens === 1500, savedView.effective.thresholdTokens, 1500);
  check('effective retain 200', savedView.effective.retainTokens === 200, savedView.effective.retainTokens, 200);
  check('effective max 2', savedView.effective.maxCheckpoints === 2, savedView.effective.maxCheckpoints, 2);
  check('effective ttl 24', savedView.effective.ttlHours === 24, savedView.effective.ttlHours, 24);
  check('no clamp in the valid narrowing', savedView.narrowed.length === 0, savedView.narrowed, []);
  const savedText = await bodyText();
  check('UI shows the stored override', savedText.includes('会话覆盖：阈值 1500 tokens · 保留 200 tokens · 最多 2 个 · TTL 24 小时'), savedText.includes('会话覆盖：阈值 1500 tokens'), true);
  await shot('saved-1440');
  await resize(390);
  await shot('saved-390');
  await overflow('saved 390');
  await resize(1440);
  finishStep();

  // ---- 3. automatic compaction --------------------------------------------
  beginStep('auto', '自动压缩：阈值触发、compaction 事件与 checkpoint 投影');
  await sendInput(created.sessionId, '预热一轮：先建立一次正常回合。', 'e2e-warmup-1');
  await waitEvents(created.sessionId, (all) => {
    const finished = all.filter((event) => event.kind === 'run.finished');
    return finished.length >= 1 && finished[finished.length - 1].payload?.status === 'done';
  });
  const longInput = 'E2E 压缩验证段落。'.repeat(700);
  await sendInput(created.sessionId, longInput, 'e2e-auto-compact-1');
  const autoEvents = await waitEvents(created.sessionId, (all) => {
    const compactions = all.filter((event) => event.kind === 'compaction');
    const finished = all.filter((event) => event.kind === 'run.finished');
    return compactions.length >= 1 && finished.some((event) => event.payload?.status === 'done' && event.seq > compactions[0].seq);
  });
  saveData('events-auto.json', autoEvents);
  const compaction = autoEvents.filter((event) => event.kind === 'compaction')[0];
  check('automatic compaction event reason', compaction?.payload?.reason === 'threshold', compaction?.payload?.reason, 'threshold');
  check('summary carries the E2E marker', String(compaction?.payload?.summary || '').includes('CHECKPOINT-E2E'), String(compaction?.payload?.summary || '').slice(0, 120), 'CHECKPOINT-E2E');
  check('compaction kept a transcript tail', Number(compaction?.payload?.firstKeptTranscript) > 0, compaction?.payload?.firstKeptTranscript, '> 0');
  const rowsAfterAuto = psql(`select upto_seq, kept_from_seq, request_id from checkpoints where tenant_id='${TENANT}' and session_id='${created.sessionId}' order by upto_seq`);
  record({ checkpointRows: rowsAfterAuto });
  const autoRow = rowsAfterAuto.split('\n').filter(Boolean).pop() || '';
  const [autoUpto, autoKept] = autoRow.split('|');
  check('checkpoint row names the compaction seq', Number(autoUpto) === compaction?.seq, autoUpto, compaction?.seq);
  check('checkpoint row is anchored before the compaction', Number(autoKept) > 0 && Number(autoKept) < Number(autoUpto), autoKept, '< upto_seq');
  const autoMetrics = await metrics();
  check('checkpoint created metric', (autoMetrics.keel_checkpoint_created_total || 0) >= 1, autoMetrics.keel_checkpoint_created_total, '>= 1');
  record({ metrics: autoMetrics });

  await page.goto(`${BASE}/`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector(`[data-testid="session-row-${created.sessionId}"]`, { timeout: 20000 });
  await page.click(`[data-testid="session-row-${created.sessionId}"]`);
  await waitText(/上下文已压缩/, 20000);
  await shot('auto-conversation-1440');
  await resize(390);
  await waitText(/上下文已压缩/, 10000);
  await shot('auto-conversation-390');
  await overflow('auto conversation 390');
  await resize(1440);
  finishStep();

  // ---- 4. manual policy toggle + manual compaction ------------------------
  beginStep('manual', '手动压缩开关：关闭 → 409，开启 → 201 与幂等回放');
  await openCheckpoints({ fresh: true });
  await page.waitForSelector(sel('settings-checkpoint-effective'), { timeout: 20000 });
  await fillField('settings-checkpoint-threshold', 50000);
  await fillField('settings-checkpoint-retain', 200);
  await fillField('settings-checkpoint-max', 2);
  await fillField('settings-checkpoint-ttl', 24);
  await chooseSelect('settings-checkpoint-manual', '关闭');
  await clickAndWaitSaved();
  const manualOff = await policy(created.sessionId);
  check('manualCompact off in the effective policy', manualOff.effective.manualCompact === false, manualOff.effective.manualCompact, false);

  const secondLong = '第二次长输入，手动压缩验证。'.repeat(600);
  await sendInput(created.sessionId, secondLong, 'e2e-manual-input-1');
  await waitEvents(created.sessionId, (all) => {
    const finished = all.filter((event) => event.kind === 'run.finished');
    return finished.length >= 2 && finished[finished.length - 1].payload?.status === 'done';
  });
  const beforeDisabled = await events(created.sessionId);
  const compactionsBefore = beforeDisabled.filter((event) => event.kind === 'compaction').length;
  const disabled = await compactSettled(created.sessionId, 'e2e-manual-disabled');
  saveData('manual-disabled.json', disabled.body);
  check('manual compaction refused with 409', disabled.status === 409, disabled.status, 409);
  check('refusal reason names the policy', disabled.body?.status === 'disabled', disabled.body?.status, 'disabled');

  await page.waitForSelector(sel('settings-checkpoint-effective'), { timeout: 10000 });
  await chooseSelect('settings-checkpoint-manual', '开启');
  await clickAndWaitSaved();
  const manualOn = await policy(created.sessionId);
  check('manualCompact on in the effective policy', manualOn.effective.manualCompact === true, manualOn.effective.manualCompact, true);
  const compacted = await api('POST', `/v1/sessions/${created.sessionId}/compact`, { requestId: 'e2e-manual-1' });
  saveData('manual-compact.json', compacted.body);
  check('manual compaction performed (201)', compacted.status === 201 && compacted.body?.performed === true, compacted.status, 201);
  check('manual checkpoint names the policy version', String(compacted.body?.policyVersion || '').startsWith('cp1-'), compacted.body?.policyVersion, 'cp1-*');
  const replay = await api('POST', `/v1/sessions/${created.sessionId}/compact`, { requestId: 'e2e-manual-1' });
  saveData('manual-replay.json', replay.body);
  check('repeated requestId replays (200)', replay.status === 200 && replay.body?.replayed === true, replay.status, 200);
  const afterManualEvents = await events(created.sessionId);
  check('no second compaction event on replay', afterManualEvents.filter((event) => event.kind === 'compaction').length === compactionsBefore + 1, afterManualEvents.filter((event) => event.kind === 'compaction').length, compactionsBefore + 1);
  const rowsAfterManual = psql(`select upto_seq, kept_from_seq, request_id from checkpoints where tenant_id='${TENANT}' and session_id='${created.sessionId}' order by upto_seq`);
  record({ checkpointRows: rowsAfterManual });
  check('two checkpoint projections exist', rowsAfterManual.split('\n').filter(Boolean).length >= 2, rowsAfterManual.split('\n').filter(Boolean).length, '>= 2');
  check('manual row records its requestId', rowsAfterManual.includes('e2e-manual-1'), rowsAfterManual, 'e2e-manual-1');

  await page.goto(`${BASE}/`, { waitUntil: 'domcontentloaded' });
  await page.click(`[data-testid="session-row-${created.sessionId}"]`);
  await waitText(/上下文已压缩/, 20000);
  await shot('manual-conversation-1440');
  finishStep();

  // ---- 5. busy face --------------------------------------------------------
  beginStep('busy', '保存中：表单禁用与进度文案');
  await openCheckpoints({ fresh: true });
  await page.waitForSelector(sel('settings-checkpoint-effective'), { timeout: 20000 });
  let holdSave = 0;
  await page.route('**/v1/sessions/*/checkpoint-policy', async (route) => {
    if (route.request().method() === 'PUT' && holdSave > 0) {
      holdSave -= 1;
      await sleep(2500);
    }
    await route.continue();
  });
  await fillField('settings-checkpoint-threshold', 40000);
  holdSave = 1;
  await page.click(sel('settings-checkpoint-save'));
  await page.waitForSelector(sel('settings-checkpoint-saving'), { state: 'visible', timeout: 5000 });
  await shot('busy-1440');
  check('save button disabled while saving', await page.locator(sel('settings-checkpoint-save')).isDisabled().catch(() => false), true, true);
  await page.waitForSelector(sel('settings-checkpoint-saving'), { state: 'detached', timeout: 20000 });
  await page.waitForSelector(sel('settings-checkpoint-effective'), { timeout: 20000 });
  const busyView = await policy(created.sessionId);
  check('delayed save still stored the value', busyView.effective.thresholdTokens === 40000, busyView.effective.thresholdTokens, 40000);
  finishStep();

  // ---- 6. over-wide request narrowed by the tenant ------------------------
  beginStep('narrowed', '策略收窄：超宽请求被租户上限逐项压回');
  await fillField('settings-checkpoint-threshold', 100);
  await fillField('settings-checkpoint-retain', 999999);
  await fillField('settings-checkpoint-max', 0);
  await fillField('settings-checkpoint-ttl', 9999);
  await clickAndWaitSaved();
  await page.waitForSelector(sel('settings-checkpoint-narrowed'), { timeout: 20000 });
  const narrowedView = await policy(created.sessionId);
  saveData('policy-narrowed.json', narrowedView);
  record({ narrowedView });
  const fields = (narrowedView.narrowed || []).map((row) => row.field).sort();
  check('four fields clamped', fields.length === 4, fields, 4);
  check('threshold clamped', narrowedView.effective.thresholdTokens === 1000, narrowedView.effective.thresholdTokens, 1000);
  check('retain clamped', narrowedView.effective.retainTokens === 20000, narrowedView.effective.retainTokens, 20000);
  check('max clamped', narrowedView.effective.maxCheckpoints === 3, narrowedView.effective.maxCheckpoints, 3);
  check('ttl clamped', narrowedView.effective.ttlHours === 168, narrowedView.effective.ttlHours, 168);
  check('ttl reason uses the server field name', fields.includes('ttl'), fields, 'ttl');
  const narrowedText = await bodyText();
  check('UI lists every clamp', narrowedText.includes('被租户上限收窄') && narrowedText.includes('最大检查点数'), narrowedText.includes('被租户上限收窄'), true);
  const overrideLine = await page.locator(sel('settings-checkpoint-override')).innerText();
  // The override line also carries enabled/manual when they were part of the
  // stored document, so assert the fields themselves, not a fixed prefix.
  const overrideShown =
    overrideLine.includes('阈值 100 tokens') &&
    overrideLine.includes('保留 999999 tokens') &&
    overrideLine.includes('最多 不限') &&
    overrideLine.includes('TTL 9999 小时');
  check('UI shows the requested override, not the clamped values', overrideShown, overrideLine, '阈值 100 / 999999 / 不限 / 9999');
  await shot('narrowed-1440');
  await resize(390);
  await shot('narrowed-390');
  await overflow('narrowed 390');
  await resize(1440);
  finishStep();

  // ---- 7. kill -9: error face, restart, recovery --------------------------
  beginStep('restart', 'kill -9 → 错误态 → 重启恢复与策略持久化');
  const beforeRestart = await events(created.sessionId);
  const killed = await stopServer('SIGKILL');
  check('server killed with SIGKILL', Boolean(killed.sig || killed.code !== 0), killed, 'SIGKILL');
  await page.click(sel('settings-checkpoint-refresh'));
  await page.waitForSelector(sel('settings-checkpoint-error'), { timeout: 20000 });
  await shot('error-1440');
  const errorText = await bodyText();
  check('error face names the read failure', errorText.includes('检查点策略读取失败'), errorText.includes('检查点策略读取失败'), true);

  const health2 = await startServer();
  record({ healthAfterRestart: health2 });
  await readAuthSession();
  await page.click(sel('settings-checkpoint-retry'));
  await page.waitForSelector(sel('settings-checkpoint-effective'), { timeout: 30000 });
  const afterRestartPolicy = await policy(created.sessionId);
  check('session override survived the restart', afterRestartPolicy.source === 'session' && afterRestartPolicy.narrowed.length === 4, {
    source: afterRestartPolicy.source,
    narrowed: afterRestartPolicy.narrowed.length,
  }, { source: 'session', narrowed: 4 });
  const rowsAfterRestart = psql(`select upto_seq, kept_from_seq from checkpoints where tenant_id='${TENANT}' and session_id='${created.sessionId}' order by upto_seq`);
  check('projections survived the restart', rowsAfterRestart.split('\n').filter(Boolean).length >= 2, rowsAfterRestart, '>= 2');
  await shot('restart-1440');

  const beforeRestartFinished = (await events(created.sessionId)).filter((event) => event.kind === 'run.finished').length;
  await sendInput(created.sessionId, '重启后继续：验证 checkpoint 恢复。', 'e2e-after-restart-1');
  await waitEvents(created.sessionId, (all) => {
    const finished = all.filter((event) => event.kind === 'run.finished');
    return finished.length >= beforeRestartFinished + 1 && finished[finished.length - 1].payload?.status === 'done';
  });
  const recoveryMetrics = await metrics();
  check('checkpoint recovery metric advanced', (recoveryMetrics.keel_checkpoint_recovered_total || 0) >= 1, recoveryMetrics.keel_checkpoint_recovered_total, '>= 1');
  const afterRestartEvents = await events(created.sessionId);
  const seqs = afterRestartEvents.map((event) => event.seq);
  check('event seqs stay strictly increasing', seqs.every((seq, index) => index === 0 || seq > seqs[index - 1]), seqs.slice(-6), 'strictly increasing');
  check('no event was lost', afterRestartEvents.length >= beforeRestart.length, { before: beforeRestart.length, after: afterRestartEvents.length }, '>= before');
  record({ metricsAfterRestart: recoveryMetrics });
  finishStep();

  // ---- 8. permission face (UI gate; server 403 is covered below) ----------
  beginStep('permission', '无权限：缺少 session.read 的只读身份');
  const permissionContext = await browser.newContext({ viewport: { width: 1440, height: 1000 }, locale: 'zh-CN' });
  const permissionPage = await permissionContext.newPage();
  await permissionPage.route('**/v1/auth/whoami', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        tenantId: TENANT,
        accountId: sessionDocument.account?.id || '',
        username: 'read-only-e2e',
        displayName: 'Read Only E2E',
        kind: 'human',
        disabled: false,
        isSuperAdmin: false,
        permissions: {},
      }),
    }),
  );
  await permissionPage.goto(`${BASE}/settings`, { waitUntil: 'domcontentloaded' });
  await permissionPage.waitForSelector(sel('settings-section-checkpoints'), { timeout: 30000 });
  await permissionPage.click(sel('settings-section-checkpoints'));
  await permissionPage.waitForSelector(sel('settings-checkpoint-permission'), { timeout: 20000 });
  const permissionText = (await permissionPage.evaluate(() => document.body.innerText || '')).replace(/\s+/g, ' ');
  check('permission face names the missing key', permissionText.includes('缺少 session.read'), permissionText.includes('缺少 session.read'), true);
  screenshotSeq += 1;
  const permissionFile = `${String(screenshotSeq).padStart(2, '0')}-permission-1440.png`;
  await permissionPage.screenshot({ path: path.join(SHOTS, permissionFile), fullPage: true });
  current.screenshots.push(`shots/${permissionFile}`);
  await permissionPage.setViewportSize({ width: 390, height: 844 });
  await sleep(300);
  screenshotSeq += 1;
  const permissionPhone = `${String(screenshotSeq).padStart(2, '0')}-permission-390.png`;
  await permissionPage.screenshot({ path: path.join(SHOTS, permissionPhone), fullPage: true });
  current.screenshots.push(`shots/${permissionPhone}`);
  await permissionContext.close();
  finishStep();

  // A real server-side 403: a bot key with no harness permissions reading the
  // session policy. The key never enters the report.
  beginStep('server-403', '服务端 403：无 session.read 的 Bot key');
  const keyResponse = await api('POST', `/v1/bots/${created.botId}/keys`, { label: 'e2e-checkpoint-readonly' });
  saveData('bot-key-status.json', { status: keyResponse.status });
  check('bot key minted', keyResponse.status === 201 || keyResponse.status === 200, keyResponse.status, '201');
  const botKey = keyResponse.body?.plaintext || '';
  const keyId = keyResponse.body?.key?.keyId || '';
  if (botKey) {
    const forbidden = await fetch(`${BASE}/v1/sessions/${created.sessionId}/checkpoint-policy`, {
      headers: { Authorization: `Bearer ${botKey}`, 'X-Bot-ID': created.botId },
    });
    check('bot key without session.read is refused', forbidden.status === 403, forbidden.status, 403);
  } else {
    check('bot key value returned once', false, Object.keys(keyResponse.body || {}), 'key');
  }
  if (keyId) {
    const deleted = await api('DELETE', `/v1/bots/${created.botId}/keys/${keyId}`);
    check('temporary bot key revoked', deleted.status === 200 || deleted.status === 204, deleted.status, '200/204');
  }
  finishStep();

  // ---- 9. reset to the tenant default ------------------------------------
  beginStep('reset', '恢复租户默认：覆盖删除、来源回到 tenant');
  await dismissNotice();
  await page.click(sel('settings-checkpoint-reset'));
  await waitText(/来源：租户默认/, 30000);
  await page.waitForSelector(sel('settings-checkpoint-saving'), { state: 'detached', timeout: 15000 }).catch(() => {});
  const resetView = await policy(created.sessionId);
  saveData('policy-reset.json', resetView);
  check('override cleared', resetView.source === 'tenant' && !resetView.override, { source: resetView.source, override: resetView.override }, { source: 'tenant' });
  check('effective is the tenant policy again', JSON.stringify(resetView.effective) === JSON.stringify(resetView.tenant), resetView.effective, resetView.tenant);
  const rowsAfterReset = psql(`select count(*) from session_checkpoint_policies where tenant_id='${TENANT}' and session_id='${created.sessionId}'`);
  check('policy row removed', rowsAfterReset === '0', rowsAfterReset, '0');
  await shot('reset-1440');
  finishStep();

  // ---- 10. final facts -----------------------------------------------------
  beginStep('final', '收尾：事件连续、无重复、清理前事实');
  const finalEvents = await events(created.sessionId);
  const compactionCount = finalEvents.filter((event) => event.kind === 'compaction').length;
  check('two compactions remain in the log', compactionCount === 2, compactionCount, 2);
  const checkpointRows = psql(`select count(*) from checkpoints where tenant_id='${TENANT}' and session_id='${created.sessionId}'`);
  check('two checkpoint projections remain', Number(checkpointRows) >= 2, checkpointRows, '>= 2');
  const eventCount = finalEvents.length;
  record({ eventCount, compactionCount, checkpointRows, durationMs: Date.now() - started });
  await shot('final-settings-1440');
  finishStep();
}

// ---- run / write evidence --------------------------------------------------

async function main() {
  let exitCode = 0;
  try {
    await runAll();
  } catch (error) {
    report.error = String(error?.stack || error);
    report.failures += 1;
    if (current) {
      current.status = 'failed';
      current.error = report.error;
      current = null;
    }
    exitCode = 1;
    console.error(report.error);
  } finally {
    try {
      if (context) await context.close();
    } catch {
      /* ignore */
    }
    try {
      if (browser) await browser.close();
    } catch {
      /* ignore */
    }
    await stopServer('SIGKILL').catch(() => {});
    report.finishedAt = new Date().toISOString();
    report.pass = report.failures === 0 && !report.error;
    fs.writeFileSync(path.join(EVIDENCE, 'steps.json'), JSON.stringify(report, null, 2) + '\n');
    if (!report.pass) exitCode = 1;
    console.log(`\nsteps: ${report.pass ? 'PASS' : 'FAIL'} (${report.failures} failed check(s))`);
  }
  process.exit(exitCode);
}

await main();
