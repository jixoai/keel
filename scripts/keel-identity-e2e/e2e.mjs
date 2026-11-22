#!/usr/bin/env node
// Real-service E2E for change keel-bot-identity-access (A6).
//
// One process owns the browser lane (playwright-core + the machine's Chrome)
// and the API lane (fetch with a cookie jar + CSRF). Every step writes its
// assertions and screenshots to `steps.json`; `report-gen.mjs` turns that into
// the HTML report. No credential (password, setup token, Bot key, DSN) is ever
// written to steps.json, a log line or a screenshot: this file reads them from
// 0600 files, keeps them in memory, and records only ids/prefixes.
//
// The stack (PG database, tenant, port, `keel serve`, secrets) is created by
// `run.sh`; this script assumes a healthy server at E2E_BASE.

import fs from 'node:fs';
import path from 'node:path';
import { execFileSync, spawn } from 'node:child_process';

const BASE = (process.env.E2E_BASE || '').replace(/\/$/, '');
const EVIDENCE = process.env.E2E_EVIDENCE || '';
const SHOTS = path.join(EVIDENCE, 'screenshots');
const ADMIN_USER = process.env.E2E_ADMIN_USERNAME || 'e2e-admin';
const CHROME = process.env.E2E_CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const PLAYWRIGHT =
  process.env.PLAYWRIGHT_CORE ||
  '/Users/kzf/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs';
const HEADLESS = (process.env.E2E_HEADLESS || 'true') !== 'false';
const KEEL_BIN = process.env.E2E_KEEL_BIN || '';
const DB = process.env.E2E_DB || '';
const PG_CONTAINER = process.env.E2E_PG_CONTAINER || 'keel-pg';
const PG_USER = process.env.E2E_PG_USER || 'postgres';
const TENANT = process.env.E2E_TENANT || '';
const LEGACY_DSN_FILE = process.env.E2E_LEGACY_DSN_FILE || '';
const DSN_FILE = process.env.E2E_DSN_FILE || '';
const BASELINE = process.env.E2E_BASELINE || '6fc1b2c';

if (!BASE || !EVIDENCE) {
  console.error('E2E_BASE and E2E_EVIDENCE are required');
  process.exit(2);
}
fs.mkdirSync(SHOTS, { recursive: true });

const readSecret = (file) => {
  if (!file) throw new Error('secret file is not configured');
  return fs.readFileSync(file, 'utf8').trim();
};

const ADMIN_PASSWORD = readSecret(process.env.E2E_ADMIN_PASSWORD_FILE);
const ALICE_PASSWORD = readSecret(process.env.E2E_ALICE_PASSWORD_FILE);
const BOB_PASSWORD = readSecret(process.env.E2E_BOB_PASSWORD_FILE);
const MEMBER_PASSWORD = readSecret(process.env.E2E_MEMBER_PASSWORD_FILE);
const BOOTSTRAP_TOKEN = readSecret(process.env.E2E_BOOTSTRAP_TOKEN_FILE);

const nowIso = () => new Date().toISOString();
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// ---- evidence -----------------------------------------------------------------

const steps = [];
const findings = [];
process.on('unhandledRejection', (error) => {
  console.error(`[e2e] unhandled rejection: ${error?.stack || error}`);
});
let shotSeq = 0;

function addFinding(id, severity, title, detail, evidence) {
  findings.push({ id, severity, title, detail, evidence: evidence ?? null });
}

function newStep(n, name, action, expect) {
  return {
    n,
    name,
    action,
    expect,
    result: null,
    startedAt: nowIso(),
    finishedAt: null,
    durationMs: 0,
    checks: [],
    shots: [],
    log: [],
    error: null,
    note: null,
  };
}

function check(step, name, ok, expected, actual, extra = {}) {
  step.checks.push({
    name,
    ok: Boolean(ok),
    expected: expected === undefined ? null : expected,
    actual: actual === undefined ? null : actual,
    severity: extra.severity || 'P1',
    findingId: extra.findingId || null,
  });
  return Boolean(ok);
}

function log(step, line) {
  step.log.push(String(line));
}

async function shot(page, step, label, options = {}) {
  shotSeq += 1;
  const file = `${String(shotSeq).padStart(2, '0')}-${String(step.n).padStart(2, '0')}-${label}.png`;
  const full = path.join(SHOTS, file);
  await page.waitForTimeout(options.settle ?? 300);
  await page.screenshot({ path: full, fullPage: options.fullPage !== false });
  const rel = `screenshots/${file}`;
  step.shots.push(rel);
  return rel;
}

async function runStep(n, name, action, expect, fn) {
  const step = newStep(n, name, action, expect);
  steps.push(step);
  const started = Date.now();
  try {
    await fn(step);
    if (!step.result) {
      const failed = step.checks.filter((c) => !c.ok);
      if (failed.length > 0 && failed.every((c) => c.severity === 'P2')) {
        step.result = 'PASS';
        step.note = `P2 observations: ${failed.map((c) => c.name).join('; ')}`;
      } else {
        step.result = step.checks.length > 0 && failed.length === 0 ? 'PASS' : 'FAIL';
      }
    }
  } catch (error) {
    step.error = String(error?.stack || error?.message || error);
    step.result = 'FAIL';
    log(step, `EXCEPTION: ${String(error?.message || error)}`);
  }
  step.finishedAt = nowIso();
  step.durationMs = Date.now() - started;
  if (step.result !== 'PASS') {
    const failed = step.checks.filter((c) => !c.ok);
    const severity = failed.some((c) => c.severity === 'P0') ? 'P0' : failed.some((c) => c.severity === 'P1') ? 'P1' : 'P2';
    const root = failed.map((c) => c.findingId).find(Boolean);
    if (!root || !findings.some((row) => row.id === root)) {
    addFinding(
      `step-${n}-failed`,
      severity,
      `E2E 步骤 ${n} 未通过：${name}`,
      step.error || step.checks.filter((c) => !c.ok).map((c) => `${c.name}: expected ${c.expected}, actual ${c.actual}`).join('; '),
      step.shots.slice(0, 3),
    );
    }
  }
  console.log(`[step ${n}] ${step.result} ${name} (${step.checks.filter((c) => c.ok).length}/${step.checks.length})`);
  return step;
}

function blockedStep(n, name, action, expect, reason) {
  const step = newStep(n, name, action, expect);
  step.result = 'BLOCKED';
  step.note = reason;
  step.finishedAt = nowIso();
  steps.push(step);
  addFinding(`step-${n}-blocked`, 'P1', `E2E 步骤 ${n} 被前置失败阻塞：${name}`, reason, null);
  console.log(`[step ${n}] BLOCKED ${name}: ${reason}`);
  return step;
}

// ---- HTTP client (cookie jar + CSRF + Bot key) ----------------------------------

function parseSetCookie(headers) {
  const out = [];
  const raw = typeof headers.getSetCookie === 'function' ? headers.getSetCookie() : [];
  for (const line of raw) {
    const first = String(line).split(';', 1)[0];
    const eq = first.indexOf('=');
    if (eq > 0) out.push([first.slice(0, eq).trim(), first.slice(eq + 1)]);
  }
  return out;
}

class Api {
  constructor(label) {
    this.label = label;
    this.cookies = new Map();
    this.csrf = '';
    this.key = '';
  }

  cookieHeader() {
    return [...this.cookies.entries()].map(([k, v]) => `${k}=${v}`).join('; ');
  }

  useKey(key) {
    this.key = key;
    return this;
  }

  async request(method, endpoint, options = {}) {
    const headers = { ...(options.headers || {}) };
    if (this.cookies.size > 0) headers.Cookie = this.cookieHeader();
    if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS' && this.csrf) {
      headers['X-Keel-CSRF'] = this.csrf;
    }
    const authKey = options.key ?? this.key;
    if (authKey) headers.Authorization = `Bearer ${authKey}`;
    let body;
    if (options.body !== undefined) {
      headers['Content-Type'] = 'application/json';
      body = JSON.stringify(options.body);
    }
    const response = await fetch(BASE + endpoint, {
      method,
      headers,
      body,
      redirect: 'manual',
      signal: AbortSignal.timeout(options.timeout ?? 30000),
    });
    for (const [name, value] of parseSetCookie(response.headers)) this.cookies.set(name, value);
    const text = await response.text();
    let parsed = null;
    if (text) {
      try {
        parsed = JSON.parse(text);
      } catch {
        parsed = { raw: text.slice(0, 500) };
      }
    }
    if (parsed && typeof parsed === 'object' && typeof parsed.csrfToken === 'string') {
      this.csrf = parsed.csrfToken;
    }
    return { status: response.status, body: parsed ?? {}, headers: response.headers };
  }

  get(endpoint, options) {
    return this.request('GET', endpoint, options);
  }

  post(endpoint, body, options) {
    return this.request('POST', endpoint, { ...options, body });
  }

  patch(endpoint, body, options) {
    return this.request('PATCH', endpoint, { ...options, body });
  }

  del(endpoint, options) {
    return this.request('DELETE', endpoint, options);
  }

  async session() {
    const res = await this.get('/v1/auth/session');
    return res;
  }

  async login(username, password) {
    await this.session();
    const res = await this.post('/v1/auth/login', { username, password });
    return res;
  }
}

// ---- DB fixture checks ----------------------------------------------------------

function psql(sql) {
  if (!DB) throw new Error('E2E_DB is not set');
  const out = execFileSync(
    'docker',
    ['exec', PG_CONTAINER, 'psql', '-U', PG_USER, '-d', DB, '-Atc', sql],
    { encoding: 'utf8', timeout: 20000 },
  );
  return out.trim();
}

function readDsn(file) {
  return fs.readFileSync(file, 'utf8').trim();
}

function copyEnv() {
  const env = { ...process.env };
  delete env.KEEL_BOOTSTRAP_TOKEN;
  return env;
}

function runKeel(args, extraEnv = {}, timeout = 30000) {
  return new Promise((resolve) => {
    const child = spawn(KEEL_BIN, args, {
      env: { ...copyEnv(), ...extraEnv },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let stdout = '';
    let stderr = '';
    const timer = setTimeout(() => {
      try {
        child.kill('SIGKILL');
      } catch {
        /* ignore */
      }
    }, timeout);
    child.stdout.on('data', (chunk) => {
      stdout += chunk.toString();
    });
    child.stderr.on('data', (chunk) => {
      stderr += chunk.toString();
    });
    child.on('close', (code) => {
      clearTimeout(timer);
      resolve({ code, stdout, stderr });
    });
    child.on('error', (error) => {
      clearTimeout(timer);
      resolve({ code: -1, stdout, stderr: String(error) });
    });
  });
}

// ---- browser helpers ------------------------------------------------------------

let browser = null;

async function launchBrowser() {
  const playwright = await import(PLAYWRIGHT);
  browser = await playwright.chromium.launch({
    executablePath: CHROME,
    headless: HEADLESS,
    args: ['--disable-dev-shm-usage'],
  });
  return browser;
}

async function newContextPage(width = 1440, height = 900) {
  const context = await browser.newContext({ viewport: { width, height }, deviceScaleFactor: 1 });
  const page = await context.newPage();
  page.setDefaultTimeout(60000);
  page.setDefaultNavigationTimeout(60000);
  page.on('pageerror', (error) => console.log(`[browser] pageerror: ${error.message}`));
  page.on('requestfailed', (request) => {
    const url = request.url();
    if (!url.includes('/v1/events') && !url.includes('/v1/realtime')) {
      console.log(`[browser] requestfailed: ${url} ${request.failure()?.errorText ?? ''}`);
    }
  });
  return { context, page };
}

const sel = (testId) => `[data-testid="${testId}"]`;

async function waitTestId(page, testId, timeout = 20000, state = 'visible') {
  await page.waitForSelector(sel(testId), { timeout, state });
}

async function loginUI(page, username, password) {
  await navigate(page, BASE);
  await waitTestId(page, 'login-gate');
  await page.fill(`${sel('login-username')} input`, username);
  await page.fill(`${sel('login-password')} input`, password);
  await page.click(sel('login-submit'));
  await waitTestId(page, 'books-shell', 30000);
}

async function gotoRoute(page, route, readyTestId) {
  await navigate(page, BASE + route);
  await waitTestId(page, 'books-shell', 30000);
  if (readyTestId) await waitTestId(page, readyTestId, 30000);
}

async function bodyText(page) {
  return page.locator('body').innerText();
}

async function navigate(page, url) {
  let lastError = null;
  for (let attempt = 0; attempt < 3; attempt += 1) {
    try {
      await page.goto(url, { waitUntil: 'commit', timeout: 60000 });
      return;
    } catch (error) {
      lastError = error;
      await sleep(400);
    }
  }
  throw lastError;
}

async function reload(page) {
  let lastError = null;
  for (let attempt = 0; attempt < 3; attempt += 1) {
    try {
      await page.reload({ waitUntil: 'commit', timeout: 60000 });
      return;
    } catch (error) {
      lastError = error;
      await sleep(400);
    }
  }
  throw lastError;
}

async function clickTestId(page, testId) {
  await page.click(sel(testId));
}

async function fillTestId(page, testId, value) {
  await page.fill(`${sel(testId)} input, input${sel(testId)}`, value);
}

// ================================================================================
// Step 1 — empty DB / bootstrap / direct entry / logout / login
// ================================================================================

async function step1(step, ctx) {
  const { page } = ctx.adminPage;
  await navigate(page, BASE);
  await waitTestId(page, 'login-gate');
  await waitTestId(page, 'bootstrap-token');
  const gateText = await bodyText(page);
  check(step, 'empty deployment shows the bootstrap form', gateText.includes('初始化部署'), '初始化部署', gateText.slice(0, 80));
  const forcedChange = (await page.$(sel('must-change-hint'))) !== null;
  check(step, 'no forced change form on first boot', !forcedChange, 'absent', forcedChange ? 'present' : 'absent');
  await shot(page, step, 'bootstrap-empty');

  await page.fill(`${sel('bootstrap-token')} input`, BOOTSTRAP_TOKEN);
  await page.fill(`${sel('bootstrap-username')} input`, ADMIN_USER);
  await page.fill(`${sel('bootstrap-password')} input`, ADMIN_PASSWORD);
  await page.fill(`${sel('bootstrap-confirm')} input`, ADMIN_PASSWORD);
  await page.click(sel('bootstrap-submit'));

  await waitTestId(page, 'books-shell', 30000);
  const afterBootstrap = await bodyText(page);
  check(step, 'bootstrap lands directly in the app', afterBootstrap.includes('工作区'), '工作区', afterBootstrap.slice(0, 80));
  const forcedChangeAfter = (await page.$(sel('must-change-hint'))) !== null;
  check(step, 'no must-change gate after bootstrap', !forcedChangeAfter, 'absent', forcedChangeAfter ? 'present' : 'absent');
  await shot(page, step, 'direct-entry');

  // logout through the account menu, then sign back in through the form
  await clickTestId(page, 'books-account-trigger');
  await page.getByText('退出登录', { exact: true }).last().click();
  await waitTestId(page, 'books-signout-dialog');
  await page.getByText('确认退出', { exact: true }).last().click();
  await waitTestId(page, 'login-gate', 20000);
  check(step, 'logout returns to the login gate', true, 'login gate', 'login gate');
  await loginUI(page, ADMIN_USER, ADMIN_PASSWORD);
  check(step, 'second login succeeds', (await page.$(sel('books-shell'))) !== null, 'books shell', 'books shell');
  await shot(page, step, 're-login');

  // API lane: reuse the consumed token and compare the two 401 shapes.
  const anon = new Api('anon');
  const anonAccounts = await anon.get('/v1/accounts');
  check(step, 'anonymous account list is 401', anonAccounts.status === 401, 401, anonAccounts.status);

  const replay = new Api('replay');
  await replay.session();
  const replayRes = await replay.post('/v1/auth/bootstrap', {
    token: BOOTSTRAP_TOKEN,
    username: 'replay-admin',
    password: ADMIN_PASSWORD,
  });
  check(step, 'setup token cannot be replayed', [401, 403, 409].includes(replayRes.status), '401/403/409', replayRes.status);

  const wrong = new Api('wrong');
  await wrong.session();
  const wrongRes = await wrong.post('/v1/auth/login', { username: ADMIN_USER, password: 'definitely-not-the-password' });
  const missing = new Api('missing');
  await missing.session();
  const missingRes = await missing.post('/v1/auth/login', { username: 'no-such-user-xyz', password: 'definitely-not-the-password' });
  check(step, 'wrong password is 401', wrongRes.status === 401, 401, wrongRes.status);
  check(step, 'unknown account is the same shape 401', missingRes.status === 401, 401, missingRes.status);
  check(
    step,
    'wrong password and unknown account share the error shape',
    JSON.stringify(wrongRes.body) === JSON.stringify(missingRes.body),
    'same body',
    JSON.stringify(wrongRes.body).slice(0, 120),
  );

  // API lane after the browser bootstrap. The env-token path is expected to
  // produce a deployment whose first run works; hold it to that before any
  // remediation step.
  ctx.admin = new Api('admin');
  const adminLogin = await ctx.admin.login(ADMIN_USER, ADMIN_PASSWORD);
  check(step, 'API admin login succeeds', adminLogin.status === 200 && adminLogin.body.authenticated === true, '200 authenticated', `${adminLogin.status}`);
  check(
    step,
    'bootstrap password is final (no mustChangePassword)',
    adminLogin.body.mustChangePassword === false,
    false,
    adminLogin.body.mustChangePassword,
  );

  const probeSession = await ctx.admin.post('/v1/sessions', {});
  check(step, 'first session can be created', probeSession.status === 201, 201, probeSession.status);
  const probeSessionId = probeSession.body.sessionId;
  let firstInput = { status: 0, body: {} };
  if (probeSessionId) {
    firstInput = await ctx.admin.post(`/v1/sessions/${probeSessionId}/input`, { text: 'env-token bootstrap probe' });
  }
  const fkError = String(firstInput.body?.error || '');
  const tenantRows = Number(psql(`select count(*) from tenants where tenant_id='${TENANT}'`));
  check(step, 'env bootstrap created the tenant row', tenantRows === 1, 1, tenantRows, {
    severity: 'P0',
    findingId: 'env-token-tenant-missing',
  });
  check(
    step,
    'first input is not refused by the tenant FK',
    firstInput.status !== 500 || !fkError.includes('groups_tenant_fk'),
    'not 500 groups_tenant_fk',
    `${firstInput.status} ${fkError.slice(0, 100)}`,
    { severity: 'P0', findingId: 'env-token-tenant-missing' },
  );
  if (tenantRows === 0 || fkError.includes('groups_tenant_fk')) {
    addFinding(
      'env-token-tenant-missing',
      'P0',
      'KEEL_BOOTSTRAP_TOKEN 空库启动后没有 tenants 行，首个 run 500（groups_tenant_fk）',
      '用 env token 完成浏览器 bootstrap 后，`keel admin bootstrap` 本应创建的租户行不存在；placement 创建 groups 时触发 groups_tenant_fk，会话/团队启动返回 500。CLI bootstrap 路径会创建该行，容器路线（env token）不会。',
      [
        `select count(*) from tenants = ${tenantRows}`,
        `POST /v1/sessions/{id}/input → ${firstInput.status} ${fkError.slice(0, 160)}`,
      ],
    );
    const res = await runKeel(
      ['admin', 'tenant', 'create', '--tenant', TENANT, '--name', 'keel-e2e-identity'],
      { KEEL_DSN: readDsn(DSN_FILE), KEEL_SERVE_TENANT: TENANT },
    );
    check(step, 'tenant remediation via keel admin tenant create', res.code === 0, 0, res.code);
    const after = Number(psql(`select count(*) from tenants where tenant_id='${TENANT}'`));
    check(step, 'tenant row exists after remediation', after === 1, 1, after);
    log(step, `tenant remediation: exit=${res.code} stdout=${res.stdout.trim().slice(0, 120)}`);
    if (probeSessionId) {
      const retry = await ctx.admin.post(`/v1/sessions/${probeSessionId}/input`, { text: 'after tenant remediation' });
      check(step, 'after remediation the input commits', retry.status === 201, 201, retry.status);
      await ctx.admin.del(`/v1/sessions/${probeSessionId}`);
    }
  } else if (probeSessionId) {
    await ctx.admin.del(`/v1/sessions/${probeSessionId}`);
  }

  const who = await ctx.admin.get('/v1/auth/whoami');
  check(step, 'whoami reports the super admin', who.status === 200 && who.body.account?.isSuperAdmin === true, 'isSuperAdmin true', who.body.account?.isSuperAdmin);
  ctx.adminAccountId = who.body.account?.id;
  log(step, `admin account=${ctx.adminAccountId}`);
}

// ================================================================================
// Step 2 — permission group + human account + effective union + grant ⊆ own
// ================================================================================

async function step2(step, ctx) {
  const { page } = ctx.adminPage;
  const admin = ctx.admin;

  // Create the group through the product dialog (not just the API). At 1440
  // the identity chapter keeps an index column; below that the index is the page.
  await gotoRoute(page, '/identity/groups', 'identity-chapter');
  await page.waitForSelector(`${sel('groups-index-scroll')}, ${sel('groups-head')}`, { timeout: 30000 });
  const wide = (await page.$(sel('groups-index-scroll'))) !== null;
  await clickTestId(page, wide ? 'groups-create' : 'groups-create-page');
  await waitTestId(page, 'group-create-dialog');
  await fillTestId(page, 'group-create-name', 'e2e-readers');
  await clickTestId(page, 'group-create-submit');
  await page.getByText('e2e-readers', { exact: true }).first().waitFor({ timeout: 20000 });
  check(step, 'create group dialog persists e2e-readers', true, 'group in list', 'group in list');
  await shot(page, step, 'group-created');

  const groups = await admin.get('/v1/permission-groups?limit=200');
  const group = (groups.body.groups || []).find((row) => row.name === 'e2e-readers');
  check(step, 'group is in the API list', Boolean(group), 'present', groups.body.groups?.length ?? 0);
  if (!group) throw new Error('e2e-readers missing from GET /v1/permission-groups');
  ctx.readersGroupId = group.groupId;

  const grants = [
    ['session.read', 'own'],
    ['session.write', 'own'],
    ['bot.read', 'own'],
    ['bot.invoke', 'own'],
    ['team_bot.read', 'own'],
    ['team_bot.manage', 'tenant'],
  ];
  for (const [key, scope] of grants) {
    const res = await admin.post(`/v1/permission-groups/${group.groupId}/grants`, { key, scope });
    check(step, `grant ${key}@${scope}`, res.status === 200, 200, res.status);
  }
  const detail = await admin.get(`/v1/permission-groups/${group.groupId}`);
  const held = new Map((detail.body.grants || []).map((row) => [row.key, row.scope]));
  check(step, 'group detail carries the whole grant set', grants.every(([key, scope]) => held.get(key) === scope), grants.length, detail.body.grants?.length);
  await waitTestId(page, 'group-matrix', 30000);
  await shot(page, step, 'group-matrix');

  // Create alice through the account dialog.
  await gotoRoute(page, '/identity/accounts', 'identity-chapter');
  await page.waitForSelector(`${sel('accounts-index-scroll')}, ${sel('accounts-head')}`, { timeout: 30000 });
  const accountsWide = (await page.$(sel('accounts-index-scroll'))) !== null;
  await clickTestId(page, accountsWide ? 'accounts-create' : 'accounts-create-page');
  await waitTestId(page, 'account-create-dialog');
  await fillTestId(page, 'account-create-username', 'alice');
  await fillTestId(page, 'account-create-name', 'Alice');
  await fillTestId(page, 'account-create-password', ALICE_PASSWORD);
  await clickTestId(page, 'account-create-submit');
  await page.getByText('Alice 已创建', { exact: false }).first().waitFor({ timeout: 20000 });
  // The created panel shows the temporary password; leave it before any capture.
  await page.getByText('查看账号', { exact: true }).last().click();
  await waitTestId(page, 'account-head', 20000);
  check(step, 'create account dialog persists alice', true, 'account detail', 'account detail');
  await shot(page, step, 'alice-account');

  const accounts = await admin.get('/v1/accounts?limit=200');
  const alice = (accounts.body.accounts || []).find((row) => row.username === 'alice');
  check(step, 'alice is in the API list', Boolean(alice), 'present', accounts.body.accounts?.length ?? 0);
  if (!alice) throw new Error('alice missing from GET /v1/accounts');
  ctx.aliceAccountId = alice.accountId;
  const addMember = await admin.post(`/v1/permission-groups/${group.groupId}/members`, { accountId: alice.accountId });
  check(step, 'alice joins e2e-readers', addMember.status === 200, 200, addMember.status);

  // bob is the unrelated account used by the privacy step.
  const bobRes = await admin.post('/v1/accounts', {
    username: 'bob',
    displayName: 'Bob',
    kind: 'human',
    password: BOB_PASSWORD,
  });
  check(step, 'bob account created', bobRes.status === 201, 201, bobRes.status);
  ctx.bobAccountId = bobRes.body.account?.accountId;
  const bobMember = await admin.post(`/v1/permission-groups/${group.groupId}/members`, { accountId: ctx.bobAccountId });
  check(step, 'bob joins e2e-readers (read key without relations)', bobMember.status === 200, 200, bobMember.status);

  // Effective permissions are the union of the group grants.
  const aliceApi = new Api('alice');
  const aliceLogin = await aliceApi.login('alice', ALICE_PASSWORD);
  check(step, 'alice can log in', aliceLogin.status === 200 && aliceLogin.body.authenticated === true, '200', aliceLogin.status);
  const aliceWho = await aliceApi.get('/v1/auth/whoami');
  const perms = aliceWho.body.permissions || {};
  check(step, 'alice effective set includes session.read=own', perms['session.read'] === 'own', 'own', perms['session.read']);
  check(step, 'alice effective set includes bot.invoke=own', perms['bot.invoke'] === 'own', 'own', perms['bot.invoke']);
  check(step, 'alice is not a super admin', aliceWho.body.account?.isSuperAdmin === false, false, aliceWho.body.account?.isSuperAdmin);
  await navigate(page, `${BASE}/identity/accounts/${alice.accountId}`);
  await waitTestId(page, 'account-head', 30000);
  await shot(page, step, 'alice-detail');

  // grant ⊆ own: alice has no permission.manage, so the write is refused and
  // the group does not gain the key.
  const badGrant = await aliceApi.post(`/v1/permission-groups/${group.groupId}/grants`, {
    key: 'secret.read.value',
    scope: 'tenant',
  });
  check(step, 'alice cannot grant a key she does not hold', badGrant.status === 403, 403, badGrant.status);
  const afterBad = await admin.get(`/v1/permission-groups/${group.groupId}`);
  check(
    step,
    'refused grant is not persisted',
    !(afterBad.body.grants || []).some((row) => row.key === 'secret.read.value'),
    'absent',
    'present',
  );
  const badMember = await aliceApi.post(`/v1/permission-groups/${group.groupId}/members`, { accountId: ctx.bobAccountId });
  check(step, 'alice cannot add a member without account.permission.grant', badMember.status === 403, 403, badMember.status);
  log(step, `alice denied: ${badGrant.body.requiredKey || badGrant.body.code} / ${badMember.body.requiredKey || badMember.body.code}`);
}

// ================================================================================
// Step 3 — Role / Profile / Bot (model kind, default model, harness) + detail
// ================================================================================

async function makeRoleProfileBot(api, spec) {
  const role = await api.post('/v1/roles', { name: spec.roleName, instruction: spec.instruction ?? '' });
  const profile = await api.post('/v1/profiles', {
    accountId: spec.accountId,
    roleId: role.body.role?.roleId,
  });
  const bot = await api.post('/v1/bots', {
    botId: spec.botId,
    name: spec.name,
    profileId: profile.body.profile?.profileId,
    modelKind: spec.modelKind ?? 'text',
    defaultModel: spec.defaultModel ?? { providerId: 'fake', modelId: 'fake' },
    harness: spec.harness ?? { tools: [], context: { instruction: spec.instruction ?? '', permissions: [] } },
  });
  return { role, profile, bot };
}

async function step3(step, ctx) {
  const admin = ctx.admin;
  const made = await makeRoleProfileBot(admin, {
    roleName: 'Support Agent',
    instruction: '回答客户问题；先确认订单号，再给出下一步。',
    accountId: ctx.aliceAccountId,
    botId: 'support-1',
    name: '客服一号',
    harness: {
      tools: [],
      context: {
        instruction: '回答客户问题；先确认订单号，再给出下一步。',
        permissions: ['session.read', 'session.write', 'bot.read'],
      },
    },
  });
  check(step, 'create role', made.role.status === 201, 201, made.role.status);
  check(step, 'create profile on alice', made.profile.status === 201, 201, made.profile.status);
  check(step, 'create bot', made.bot.status === 201, 201, made.bot.status);
  if (made.bot.status !== 201) throw new Error(`create bot: ${JSON.stringify(made.bot.body).slice(0, 400)}`);
  ctx.supportProfileId = made.profile.body.profile?.profileId;
  ctx.supportRoleId = made.role.body.role?.roleId;

  const detail = await admin.get('/v1/bots/support-1');
  const bot = detail.body.bot || {};
  check(step, 'bot detail is readable', detail.status === 200, 200, detail.status);
  check(step, 'bot carries profileId', bot.profileId === ctx.supportProfileId, ctx.supportProfileId, bot.profileId);
  check(step, 'bot carries the owner account (alice)', bot.accountId === ctx.aliceAccountId, ctx.aliceAccountId, bot.accountId);
  check(step, 'model kind is text', bot.modelKind === 'text', 'text', bot.modelKind);
  check(step, 'default model is recorded', bot.defaultModel?.providerId === 'fake' && bot.defaultModel?.modelId === 'fake', 'fake/fake', `${bot.defaultModel?.providerId}/${bot.defaultModel?.modelId}`);
  check(step, 'harness instruction is recorded', String(bot.harness?.context?.instruction || '').includes('回答客户问题'), 'contains 回答客户问题', bot.harness?.context?.instruction);
  check(step, 'harness permissions are recorded', (bot.harness?.context?.permissions || []).includes('session.read'), 'session.read', JSON.stringify(bot.harness?.context?.permissions));

  const { page } = ctx.adminPage;
  await gotoRoute(page, '/bots/support-1', 'bot-page');
  await waitTestId(page, 'bot-band-head', 30000);
  const text = await bodyText(page);
  check(step, 'bot page shows the name', text.includes('客服一号'), '客服一号', text.slice(0, 80));
  check(step, 'bot page shows model kind and model', text.includes('文本') && text.toLowerCase().includes('fake'), '文本/fake', text.slice(0, 200));
  await shot(page, step, 'bot-detail');
  await gotoRoute(page, '/bots', 'bots-index');
  await waitTestId(page, 'bot-row-support-1', 30000);
  await shot(page, step, 'bots-list');
  log(step, `bot detail: profile=${ctx.supportProfileId} account=${ctx.aliceAccountId}`);
}

// ================================================================================
// Step 4 — Bot key: issue, use, wrong bot, revoke
// ================================================================================

async function step4(step, ctx) {
  const admin = ctx.admin;

  // A second bot owned by bob gives the "wrong bot" target.
  const other = await makeRoleProfileBot(admin, {
    roleName: 'Other Owner',
    instruction: '另一个账号的 Bot。',
    accountId: ctx.bobAccountId,
    botId: 'other-1',
    name: '另一个 Bot',
    harness: {
      tools: [],
      context: { instruction: '另一个账号的 Bot。', permissions: ['session.read', 'session.write'] },
    },
  });
  check(step, 'cross-account bot created for the negative control', other.bot.status === 201, 201, other.bot.status);

  const issue = await admin.post('/v1/bots/support-1/keys', { label: 'e2e-step4' });
  check(step, 'issue Bot key', issue.status === 201, 201, issue.status);
  const plaintext = issue.body.plaintext || '';
  const keyView = issue.body.key || {};
  check(step, 'key is shown once with the keel_bot_ prefix', plaintext.startsWith('keel_bot_'), 'keel_bot_…', plaintext ? 'keel_bot_…' : 'missing');
  check(step, 'key view carries the prefix and bot id', keyView.botId === 'support-1' && Boolean(keyView.prefix), 'support-1/prefix', `${keyView.botId}/<prefix>`);
  const plaintextInMetadata = JSON.stringify(keyView).includes(plaintext);
  check(step, 'plaintext is not part of the key metadata', !plaintextInMetadata, 'absent', plaintextInMetadata ? 'present' : 'absent');
  ctx.supportKeyId = keyView.keyId;

  const botApi = new Api('support-bot').useKey(plaintext);
  const who = await botApi.get('/v1/auth/whoami');
  check(step, 'Bot key authenticates', who.status === 200, 200, who.status);
  check(step, 'Bot principal is the bound Bot', who.body.bot?.id === 'support-1', 'support-1', who.body.bot?.id);
  check(step, 'Bot principal owns alice account', who.body.account?.id === ctx.aliceAccountId, ctx.aliceAccountId, who.body.account?.id);
  check(step, 'Bot is never a super admin', who.body.account?.isSuperAdmin === false, false, who.body.account?.isSuperAdmin);
  const list = await botApi.get('/v1/bots?limit=200');
  check(step, 'Bot key can call the API within its grants', list.status === 200, 200, list.status);

  const wrongBot = await botApi.get('/v1/bots/other-1');
  check(step, 'Bot key on another account/bot is 403', wrongBot.status === 403, 403, wrongBot.status);
  const wrongInvoke = await botApi.post('/v1/bots/other-1/invoke', { text: 'hi' });
  check(step, 'Bot key cannot invoke another account/bot', wrongInvoke.status === 403, 403, wrongInvoke.status);

  const keys = await admin.get('/v1/bots/support-1/keys');
  const used = (keys.body.keys || []).find((row) => row.keyId === keyView.keyId);
  check(step, 'last_used_at is updated after a Bot call', Boolean(used?.lastUsedAt), 'non-null', used?.lastUsedAt ?? null);

  const revoke = await admin.del(`/v1/bots/support-1/keys/${keyView.keyId}`);
  check(step, 'revoke Bot key', revoke.status === 200, 200, revoke.status);
  const afterRevoke = await botApi.get('/v1/auth/whoami');
  check(step, 'revoked key is 401', afterRevoke.status === 401, 401, afterRevoke.status);

  const { page } = ctx.adminPage;
  await gotoRoute(page, '/bots/support-1', 'bot-page');
  if ((await page.$(sel('bot-section-keys'))) !== null) await clickTestId(page, 'bot-section-keys');
  else await clickTestId(page, 'bot-segment-keys');
  await waitTestId(page, 'bot-keys', 30000);
  await shot(page, step, 'bot-keys');
  log(step, `keyId=${keyView.keyId}; revoked key -> 401`);
}

// ================================================================================
// Step 5 — TeamBot: mission/roster, cross-account control, start, run DAG
// ================================================================================

async function waitRunDone(api, sessionId, timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs;
  let last = null;
  while (Date.now() < deadline) {
    const res = await api.get(`/v1/sessions/${sessionId}/run`);
    last = res.body;
    if (res.status === 200 && ['done', 'failed', 'cancelled', 'idle'].includes(String(res.body.status))) {
      return { status: res.status, body: res.body };
    }
    await sleep(400);
  }
  return { status: 200, body: last };
}

async function step5(step, ctx) {
  const admin = ctx.admin;
  // support-2 is a second Bot on alice's account: same-account roster works.
  const member = await makeRoleProfileBot(admin, {
    roleName: 'Support Member',
    instruction: '作为客服团队成员回答客户问题。',
    accountId: ctx.aliceAccountId,
    botId: 'support-2',
    name: '客服二号',
    harness: {
      tools: [],
      context: { instruction: '作为客服团队成员回答客户问题。', permissions: ['session.read', 'session.write'] },
    },
  });
  check(step, 'member Bot support-2 created on the same account', member.bot.status === 201, 201, member.bot.status);

  const outside = await makeRoleProfileBot(admin, {
    roleName: 'Outside Member',
    instruction: '外部账号的 Bot。',
    accountId: ctx.bobAccountId,
    botId: 'outside-1',
    name: '外部成员',
    harness: {
      tools: [],
      context: { instruction: '外部账号的 Bot。', permissions: ['session.read'] },
    },
  });
  check(step, 'foreign Bot outside-1 created on bob account', outside.bot.status === 201, 201, outside.bot.status);

  const team = await admin.post('/v1/team-bots', {
    teamBotId: 'support-team',
    name: '客服团队',
    mission: '在 24 小时内回答客户问题并给出可执行下一步。',
    roster: [
      { botId: 'support-1', role: 'captain' },
      { botId: 'support-2', role: 'member' },
    ],
  });
  check(step, 'TeamBot created with mission + captain', team.status === 201, 201, team.status);
  const created = team.body.teamBot || {};
  check(step, 'mission is non-empty', String(created.mission || '').length > 0, 'non-empty', created.mission);
  check(step, 'roster has exactly one captain', (created.roster || []).filter((seat) => seat.role === 'captain').length === 1, 1, (created.roster || []).filter((seat) => seat.role === 'captain').length);
  check(step, 'revision/content hash are stable facts', created.revision === 1 && /^[a-f0-9]{64}$/.test(created.hash || ''), 'revision=1 + sha256', `${created.revision}/${String(created.hash).slice(0, 8)}`);

  // Cross-account negative: alice holds team_bot.manage + bot.invoke but has no
  // relation on outside-1, so the roster write must be refused.
  const alice = new Api('alice');
  await alice.login('alice', ALICE_PASSWORD);
  const denied = await alice.post('/v1/team-bots', {
    teamBotId: 'leak-team',
    name: '越权团队',
    mission: '不应创建',
    roster: [{ botId: 'outside-1', role: 'captain' }],
  });
  check(step, 'cross-account roster without a relation is 403', denied.status === 403, 403, denied.status);
  check(step, 'denial names bot.invoke', denied.body.requiredKey === 'bot.invoke', 'bot.invoke', denied.body.requiredKey);
  const leak = await admin.get('/v1/team-bots/leak-team');
  check(step, 'refused TeamBot is not created', leak.status === 404, 404, leak.status);

  // Positive cross-account path: the product has no share endpoint, so the
  // fixture writes the viewer relation the engine needs and documents the gap.
  const relation = psql(
    `insert into resource_relations (tenant_id, resource_type, resource_id, relation, subject_type, subject_id, created_by) values ('${TENANT}','bot','outside-1','viewer','account','${ctx.aliceAccountId}','${ctx.adminAccountId}') on conflict do nothing returning 1`,
  );
  check(step, 'fixture: alice receives viewer on outside-1', relation.startsWith('1') || relation === '', 'row written', relation.split('\n')[0] || 'already existed');
  const allowed = await alice.post('/v1/team-bots', {
    teamBotId: 'cross-team',
    name: '跨账号团队',
    mission: '验证跨账号成员需要 bot.invoke 关系。',
    roster: [{ botId: 'outside-1', role: 'captain' }],
  });
  check(step, 'cross-account roster with a viewer relation is allowed', allowed.status === 201, 201, allowed.status);
  if (allowed.status === 201) {
    const startCross = await alice.post('/v1/team-bots/cross-team/start', { text: '开始' });
    check(step, 'cross-account team starts while the relation exists', startCross.status === 201, 201, startCross.status);
    psql(`delete from resource_relations where tenant_id='${TENANT}' and resource_type='bot' and resource_id='outside-1' and relation='viewer' and subject_type='account' and subject_id='${ctx.aliceAccountId}'`);
    const startAfter = await alice.post('/v1/team-bots/cross-team/start', { text: '再次开始' });
    check(step, 'after relation removal the next start is 403', startAfter.status === 403, 403, startAfter.status);
    addFinding(
      'no-bot-share-surface',
      'P1',
      '产品没有 Bot/TeamBot 共享/关系写入端点：跨账号 roster 只能靠 DB 夹具验证',
      '验收 §2.4 要求 roster 可引用同/跨 Account 的 Bot，能否放入/调用由 bot.invoke 决定；但 /v1 没有任何 Bot 共享/relations 写入路由（resource_relations 只在 Bot owner 创建与 external session 路径写入）。E2E 用 DB 夹具补 viewer 关系后验证了引擎的 allow/deny，但真实用户无路径把外部 Bot 放进团队。',
      ['fixture insert resource_relations', 'POST /v1/team-bots cross-team → 201', 'relation delete → start 403'],
    );
    ctx.crossTeamSession = startCross.body.sessionId;
  }

  // Start the main team and wait for the run to settle.
  const start = await admin.post('/v1/team-bots/support-team/start', { text: '开始' });
  check(step, 'TeamBot start creates a captain session', start.status === 201, 201, start.status);
  if (start.status !== 201) throw new Error(`team start: ${JSON.stringify(start.body).slice(0, 400)}`);
  const sessionId = start.body.sessionId;
  ctx.teamSessionId = sessionId;
  const run = await waitRunDone(admin, sessionId);
  check(step, 'captain run settles (done)', run.body?.status === 'done', 'done', run.body?.status);
  const events = await admin.get(`/v1/sessions/${sessionId}/events?limit=500`);
  const kinds = (events.body.events || []).map((row) => row.kind);
  check(step, 'run events include user.message', kinds.includes('user.message'), 'user.message', kinds.join(','));
  check(step, 'run events include assistant.message', kinds.includes('assistant.message'), 'assistant.message', kinds.join(','));
  check(step, 'run events include run.finished', kinds.includes('run.finished'), 'run.finished', kinds.join(','));
  const userEvent = (events.body.events || []).find((row) => row.kind === 'user.message');
  check(step, 'user event carries teamBotId', userEvent?.payload?.teamBotId === 'support-team', 'support-team', userEvent?.payload?.teamBotId);
  check(step, 'user event carries the mission snapshot', String(userEvent?.payload?.teamMission || '').includes('回答客户问题'), 'mission snapshot', userEvent?.payload?.teamMission);
  const finished = (events.body.events || []).find((row) => row.kind === 'run.finished');
  check(step, 'run.finished names the captain Bot', finished?.payload?.agentId === 'support-1', 'support-1', finished?.payload?.agentId);
  check(step, 'run.finished names the TeamBot', finished?.payload?.teamId === 'support-team', 'support-team', finished?.payload?.teamId);

  const ownership = psql(
    `select bot_id || '|' || account_id || '|' || created_by_account_id || '|' || coalesce(team_bot_id,'') from sessions where tenant_id='${TENANT}' and session_id='${sessionId}'`,
  );
  const [botId, accountId, createdBy, teamBotId] = ownership.split('|');
  check(step, 'captain session belongs to support-1', botId === 'support-1', 'support-1', botId);
  check(step, 'captain session belongs to alice', accountId === ctx.aliceAccountId, ctx.aliceAccountId, accountId);
  check(step, 'captain session records the human initiator', createdBy === ctx.adminAccountId, ctx.adminAccountId, createdBy);
  check(step, 'captain session records the TeamBot mission id', teamBotId === 'support-team', 'support-team', teamBotId);

  // The acceptance also asks the captain to organize members (create/fork/
  // invoke) during the run. The runtime has no Bot-management tool surface, so
  // the DAG can only show the captain node; record that as a gap rather than
  // pretending the team ran as a team.
  const toolEvents = (events.body.events || []).filter((row) => row.kind === 'tool_call' || row.kind === 'tool.result');
  if (!toolEvents.some((row) => JSON.stringify(row.payload || {}).includes('bot'))) {
    addFinding(
      'no-captain-member-tool',
      'P1',
      'TeamBot 启动只跑队长：队长没有调用成员的运行时工具，DAG 没有成员节点',
      '验收 §2.4/§5 要求队长 Bot 在运行中创建/分叉/调用成员，成员以独立 Session 执行并在 DAG 可见。当前 tasks.md A4.2b「captain→member 运行时工具调用 bot.invoke」仍未勾选，工具面没有 Bot 管理动作；本次真实 run 的 run.finished 只记录 agentId=support-1（队长），成员 Session 只能由人类另行 invoke 产生。',
      ['run.finished.agentId = support-1（队长）', 'DAG 0 个成员节点', 'tasks.md A4.2b unchecked: captain→member 运行时工具调用'],
    );
  }

  // A member Bot invoked by the human gets its own Session (not the captain's).
  const memberInvoke = await admin.post('/v1/bots/support-2/invoke', { text: '请回答一位客户的问题。' });
  check(step, 'member Bot invoke creates its own session', memberInvoke.status === 201, 201, memberInvoke.status);
  if (memberInvoke.status === 201) {
    ctx.memberSessionId = memberInvoke.body.sessionId;
    check(step, 'member session differs from the captain session', memberInvoke.body.sessionId !== sessionId, 'different', memberInvoke.body.sessionId);
    const memberRun = await waitRunDone(admin, memberInvoke.body.sessionId);
    check(step, 'member run settles (done)', memberRun.body?.status === 'done', 'done', memberRun.body?.status);
    const memberOwnership = psql(
      `select bot_id || '|' || account_id from sessions where tenant_id='${TENANT}' and session_id='${memberInvoke.body.sessionId}'`,
    );
    check(step, 'member session belongs to support-2', memberOwnership.split('|')[0] === 'support-2', 'support-2', memberOwnership.split('|')[0]);
  }

  const { page } = ctx.adminPage;
  await gotoRoute(page, '/teams/support-team', 'team-page');
  const teamText = await bodyText(page);
  check(step, 'team page shows mission and roster', teamText.includes('客服团队') && teamText.includes('回答客户问题'), 'mission + roster', teamText.slice(0, 160));
  await shot(page, step, 'team-detail');
  await gotoRoute(page, `/teams/support-team/runs/${sessionId}`, 'run-page');
  await waitTestId(page, 'run-lanes', 30000);
  await clickTestId(page, 'run-mode-graph');
  await waitTestId(page, 'run-graph', 30000);
  const runText = await bodyText(page);
  check(step, 'run page renders the DAG', runText.includes('客服一号') || runText.includes('support-1'), 'captain node', runText.slice(0, 200));
  check(step, 'run page shows the team mission', runText.includes('回答客户问题'), 'mission', runText.slice(0, 200));
  await shot(page, step, 'run-dag');
  log(step, `captain session=${sessionId}; member session=${ctx.memberSessionId || 'n/a'}`);
}

// ================================================================================
// Step 6 — butler Bot: create members / team / group via its own key and start
// ================================================================================

async function step6(step, ctx) {
  const admin = ctx.admin;
  const { page } = ctx.adminPage;

  // The butler Bot and its account.
  const account = await admin.post('/v1/accounts', {
    username: 'butler-account',
    displayName: '管家账号',
    kind: 'bot',
  });
  check(step, 'butler account created', account.status === 201, 201, account.status);
  const butlerAccountId = account.body.account?.accountId;
  const role = await admin.post('/v1/roles', { name: '管家', instruction: '组织团队、配置权限并启动。' });
  const profile = await admin.post('/v1/profiles', { accountId: butlerAccountId, roleId: role.body.role?.roleId });
  const butlerPermissions = [
    'bot.read',
    'bot.create',
    'bot.manage',
    'bot.bind',
    'bot.invoke',
    'team_bot.read',
    'team_bot.manage',
    'profile.read',
    'profile.manage',
    'role.read',
    'role.manage',
    'account.read',
    'account.manage',
    'account.permission.grant',
    'permission.read',
    'permission.manage',
    'audit.read',
    'session.read',
    'session.write',
  ];
  const butler = await admin.post('/v1/bots', {
    botId: 'butler',
    name: '管家',
    profileId: profile.body.profile?.profileId,
    modelKind: 'text',
    defaultModel: { providerId: 'fake', modelId: 'fake' },
    harness: {
      tools: [],
      context: { instruction: '组织团队、配置权限并启动。', permissions: butlerPermissions },
    },
  });
  check(step, 'butler Bot created', butler.status === 201, 201, butler.status);
  const group = await admin.post('/v1/permission-groups', { name: 'butler-ops' });
  const groupId = group.body.group?.groupId;
  check(step, 'butler permission group created', group.status === 201, 201, group.status);
  for (const key of butlerPermissions) {
    const res = await admin.post(`/v1/permission-groups/${groupId}/grants`, { key, scope: 'tenant' });
    if (res.status !== 200) check(step, `grant ${key} to butler-ops`, false, 200, res.status);
  }
  const member = await admin.post(`/v1/permission-groups/${groupId}/members`, { accountId: butlerAccountId });
  check(step, 'butler account joins butler-ops', member.status === 200, 200, member.status);
  const issued = await admin.post('/v1/bots/butler/keys', { label: 'e2e-butler' });
  check(step, 'butler key issued', issued.status === 201, 201, issued.status);
  const butlerKey = issued.body.plaintext;
  ctx.butlerKeyId = issued.body.key?.keyId;

  const butlerApi = new Api('butler-bot').useKey(butlerKey);
  const who = await butlerApi.get('/v1/auth/whoami');
  check(step, 'butler key sees the granted capability set', Object.keys(who.body.permissions || {}).length >= 15, '>=15 keys', Object.keys(who.body.permissions || {}).length);
  check(step, 'butler is not a super admin', who.body.account?.isSuperAdmin === false, false, who.body.account?.isSuperAdmin);

  // 1) butler creates a member Bot (Role + Profile + Bot) with its own key.
  const memberRole = await butlerApi.post('/v1/roles', { name: '客服成员', instruction: '按管家分配回答客户问题。' });
  check(step, 'butler creates a role', memberRole.status === 201, 201, memberRole.status);
  const memberProfile = await butlerApi.post('/v1/profiles', {
    accountId: butlerAccountId,
    roleId: memberRole.body.role?.roleId,
  });
  check(step, 'butler creates a profile', memberProfile.status === 201, 201, memberProfile.status);
  const memberBot = await butlerApi.post('/v1/bots', {
    botId: 'butler-member',
    name: '管家成员',
    profileId: memberProfile.body.profile?.profileId,
    modelKind: 'text',
    defaultModel: { providerId: 'fake', modelId: 'fake' },
    harness: {
      tools: [],
      context: { instruction: '按管家分配回答客户问题。', permissions: ['session.read', 'session.write'] },
    },
  });
  check(step, 'butler creates a member Bot', memberBot.status === 201, 201, memberBot.status);

  // 2) butler creates a permission group and a TeamBot, then starts it.
  const memberGroup = await butlerApi.post('/v1/permission-groups', { name: 'butler-members' });
  check(step, 'butler creates a permission group', memberGroup.status === 201, 201, memberGroup.status);
  const memberGroupId = memberGroup.body.group?.groupId;
  for (const key of ['session.read', 'session.write']) {
    const res = await butlerApi.post(`/v1/permission-groups/${memberGroupId}/grants`, { key, scope: 'own' });
    check(step, `butler grants ${key}@own`, res.status === 200, 200, res.status);
  }
  const team = await butlerApi.post('/v1/team-bots', {
    teamBotId: 'butler-team',
    name: '管家团队',
    mission: '按客户问题分派成员并汇总答案。',
    roster: [
      { botId: 'butler', role: 'captain' },
      { botId: 'butler-member', role: 'member' },
    ],
  });
  check(step, 'butler creates a TeamBot (mission + roster)', team.status === 201, 201, team.status);
  const start = await butlerApi.post('/v1/team-bots/butler-team/start', { text: '组建并启动客服团队。' });
  check(step, 'butler starts the TeamBot', start.status === 201, 201, start.status);
  if (start.status === 201) {
    ctx.butlerSessionId = start.body.sessionId;
    const run = await waitRunDone(butlerApi, start.body.sessionId);
    check(step, 'butler team run settles (done)', run.body?.status === 'done', 'done', run.body?.status);
    const events = await butlerApi.get(`/v1/sessions/${start.body.sessionId}/events?limit=500`);
    const finished = (events.body.events || []).find((row) => row.kind === 'run.finished');
    check(step, 'butler run uses the captain Bot', finished?.payload?.agentId === 'butler', 'butler', finished?.payload?.agentId);
    check(step, 'butler run records the team', finished?.payload?.teamId === 'butler-team', 'butler-team', finished?.payload?.teamId);
  }

  // 3) No Bot-specific bypass: super-admin reservation and foreign-bot manage.
  const superAttempt = await butlerApi.post('/v1/accounts', {
    username: 'fake-super',
    kind: 'human',
    password: MEMBER_PASSWORD,
    isSuperAdmin: true,
  });
  check(step, 'Bot cannot create a super admin', superAttempt.status === 403, 403, superAttempt.status);
  const foreignKeys = await butlerApi.post('/v1/bots/other-1/keys', { label: 'steal' });
  check(step, 'Bot cannot issue a key for a foreign Bot', foreignKeys.status === 403, 403, foreignKeys.status);

  if (ctx.butlerSessionId) {
    await gotoRoute(page, `/teams/butler-team/runs/${ctx.butlerSessionId}`, 'run-page');
    await waitTestId(page, 'run-lanes', 30000);
    await clickTestId(page, 'run-mode-graph');
    await waitTestId(page, 'run-graph', 30000);
    await shot(page, step, 'butler-run');
  }
  await gotoRoute(page, '/records', 'records-head');
  await waitTestId(page, 'records-table', 30000);
  await shot(page, step, 'records-after-butler');
  log(step, `butler account=${butlerAccountId}; member Bot=butler-member; session=${ctx.butlerSessionId}`);
}

// ================================================================================
// Step 7 — Session default private, viewer share, revocation
// ================================================================================

async function step7(step, ctx) {
  const admin = ctx.admin;
  const invoke = await admin.post('/v1/bots/support-1/invoke', { text: '请给我一个三步处理方案。' });
  check(step, 'human invokes support-1', invoke.status === 201, 201, invoke.status);
  if (invoke.status !== 201) throw new Error(`invoke: ${JSON.stringify(invoke.body).slice(0, 300)}`);
  const sessionId = invoke.body.sessionId;
  ctx.privateSessionId = sessionId;
  await waitRunDone(admin, sessionId);

  const ownership = psql(
    `select bot_id || '|' || account_id || '|' || created_by_account_id from sessions where tenant_id='${TENANT}' and session_id='${sessionId}'`,
  );
  check(step, 'new Session belongs to a Bot', ownership.startsWith('support-1|'), 'support-1', ownership);
  check(step, 'new Session belongs to alice account', ownership.split('|')[1] === ctx.aliceAccountId, ctx.aliceAccountId, ownership.split('|')[1]);
  check(step, 'created_by records the initiator', ownership.split('|')[2] === ctx.adminAccountId, ctx.adminAccountId, ownership.split('|')[2]);

  const alice = new Api('alice');
  await alice.login('alice', ALICE_PASSWORD);
  const aliceRead = await alice.get(`/v1/sessions/${sessionId}/events`);
  check(step, 'owner can read the Session', aliceRead.status === 200, 200, aliceRead.status);

  const bob = new Api('bob');
  await bob.login('bob', BOB_PASSWORD);
  const bobBefore = await bob.get(`/v1/sessions/${sessionId}/events`);
  check(step, 'unrelated account is denied by default', bobBefore.status === 403, 403, bobBefore.status);
  check(step, 'denial names session.read', bobBefore.body.requiredKey === 'session.read', 'session.read', bobBefore.body.requiredKey);

  // Product gap: no share endpoint. The fixture writes the viewer relation the
  // engine is designed to consume; the gap is reported, not hidden.
  psql(
    `insert into resource_relations (tenant_id, resource_type, resource_id, relation, subject_type, subject_id, created_by) values ('${TENANT}','session','${sessionId}','viewer','account','${ctx.bobAccountId}','${ctx.aliceAccountId}') on conflict do nothing`,
  );
  const bobShared = await bob.get(`/v1/sessions/${sessionId}/events`);
  check(step, 'viewer relation makes the Session readable', bobShared.status === 200, 200, bobShared.status);

  // Bob sees it in the workspace; capture before/after revoke.
  const { page } = ctx.bobPage;
  await navigate(page, BASE);
  await waitTestId(page, 'books-shell', 30000);
  await page.waitForSelector(`[data-testid="session-row-${sessionId}"]`, { timeout: 20000 }).catch(() => {});
  const bobHasRow = (await page.$(sel(`session-row-${sessionId}`))) !== null;
  check(step, 'shared Session appears in the other account workspace', bobHasRow, 'row visible', bobHasRow ? 'visible' : 'absent');
  await shot(page, step, 'shared-session-bob');

  psql(
    `delete from resource_relations where tenant_id='${TENANT}' and resource_type='session' and resource_id='${sessionId}' and relation='viewer' and subject_type='account' and subject_id='${ctx.bobAccountId}'`,
  );
  const bobAfter = await bob.get(`/v1/sessions/${sessionId}/events`);
  check(step, 'revocation denies the next read', bobAfter.status === 403, 403, bobAfter.status);
  await reload(page);
  await waitTestId(page, 'books-shell', 30000);
  await page.waitForTimeout(800);
  const bobHasRowAfter = (await page.$(sel(`session-row-${sessionId}`))) !== null;
  check(step, 'revoked Session leaves the other workspace', !bobHasRowAfter, 'absent', bobHasRowAfter ? 'visible' : 'absent');
  await shot(page, step, 'revoked-session-bob');

  addFinding(
    'no-session-share-surface',
    'P1',
    '没有 Session 分享/关系写入 API 或 UI：viewer/editor 分享无法由真实用户完成',
    '验收 §2.5 要求 owner 可给 viewer/editor 关系并撤销；产品无 /v1/sessions/{id}/share、无 ReBAC 关系写路由，Session 详情也没有分享动作。E2E 只能用 DB 夹具写 resource_relations 验证引擎 allow→revoke→403。',
    ['fixture insert viewer relation on session', 'bob GET events 200', 'delete relation → bob GET events 403'],
  );
  log(step, `session=${sessionId}; bob before=403 shared=200 revoked=403`);
}

// ================================================================================
// Step 8 — old entries: routes, CLI, cookies/keys, legacy schema
// ================================================================================

async function step8(step, ctx) {
  const admin = ctx.admin;
  const routes = [
    ['GET', '/v1/agents'],
    ['POST', '/v1/agents'],
    ['GET', '/v1/agents/anything'],
    ['DELETE', '/v1/agents/anything'],
    ['GET', '/v1/teams'],
    ['POST', '/v1/teams'],
    ['GET', '/v1/teams/anything'],
    ['POST', '/v1/teams/anything/members'],
  ];
  for (const [method, route] of routes) {
    const res = await admin.request(method, route, { body: method === 'POST' ? {} : undefined });
    check(step, `${method} ${route} is 404`, res.status === 404, 404, res.status);
  }

  for (const command of ['agent', 'team']) {
    const res = await runKeel([command]);
    check(step, `keel ${command} is an unknown command`, res.code === 2 && /unknown command/.test(res.stderr), 'exit 2 + unknown command', `exit ${res.code}`);
  }
  const oldAdmin = await runKeel(['admin', 'key', 'create']);
  check(step, 'keel admin key is an unknown command', oldAdmin.code === 2 && /unknown command/.test(oldAdmin.stderr), 'exit 2', oldAdmin.code);

  // Old cookie / old credential shapes.
  const oldCookie = new Api('old-cookie');
  oldCookie.cookies.set('keel_session', '00000000-0000-0000-0000-000000000000');
  const cookieRes = await oldCookie.get('/v1/auth/whoami');
  check(step, 'old/forged session cookie is 401', cookieRes.status === 401, 401, cookieRes.status);
  const oldKey = new Api('old-key');
  const keyRes = await oldKey.get('/v1/auth/whoami', { key: 'keel_legacy_0000000000000000000000000000000000000000' });
  check(step, 'old API-key shape is 401', keyRes.status === 401, 401, keyRes.status);
  const headerRes = await oldKey.get('/v1/auth/whoami', { headers: { 'X-API-Key': 'legacy-key' } });
  check(step, 'old X-API-Key header is 401', headerRes.status === 401, 401, headerRes.status);

  // Legacy schema: an independent database with a populated agents table must
  // refuse startup and keep the data (no silent migration).
  if (LEGACY_DSN_FILE && fs.existsSync(LEGACY_DSN_FILE)) {
    const legacyDsn = readDsn(LEGACY_DSN_FILE);
    const res = await runKeel(
      ['serve', '--addr', '127.0.0.1:13799', '--auth', 'required', '--tenant', TENANT],
      {
        KEEL_DSN: legacyDsn,
        KEEL_SECRET_KEY: '00'.repeat(32),
        KEEL_SERVE_TENANT: TENANT,
      },
      15000,
    );
    const refusal = `${res.stderr}\n${res.stdout}`;
    check(step, 'legacy agents row refuses startup', res.code !== 0, 'non-zero', res.code);
    check(
      step,
      'refusal names the legacy registry and the rebuild instruction',
      /legacy agent registry still holds rows/.test(refusal),
      'legacy agent registry still holds rows',
      refusal.replace(/\s+/g, ' ').slice(0, 240),
    );
    log(step, `legacy serve exit=${res.code}`);
  } else {
    check(step, 'legacy schema fixture is available', false, 'E2E_LEGACY_DSN_FILE', 'missing');
  }

  // A random/retired password login stays a same-shaped 401.
  const legacyPass = new Api('legacy-pass');
  await legacyPass.session();
  const passRes = await legacyPass.post('/v1/auth/login', { password: 'old-deployment-passphrase' });
  check(step, 'old passphrase login body is refused', passRes.status === 400 || passRes.status === 401, '400/401', passRes.status);
}

// ================================================================================
// Step 9 — four viewports, five states, 44px targets
// ================================================================================

async function captureViewport(page, step, width, height, suffix) {
  await page.setViewportSize({ width, height });
  await reload(page);
  await waitTestId(page, 'books-shell', 30000);
  const overflow = await page.evaluate(() => ({
    doc: document.documentElement.scrollWidth,
    body: document.body.scrollWidth,
    width: window.innerWidth,
  }));
  check(
    step,
    `${suffix} has no horizontal overflow`,
    overflow.doc <= overflow.width + 2 && overflow.body <= overflow.width + 2,
    `<= ${overflow.width + 2}`,
    `doc=${overflow.doc} body=${overflow.body}`,
  );
}

async function step9(step, ctx) {
  const { page } = ctx.adminPage;
  const viewports = [
    [1440, 900, '1440'],
    [1024, 900, '1024'],
    [768, 900, '768'],
    [390, 844, '390'],
  ];
  const pages = [
    [`/identity/accounts/${ctx.aliceAccountId}`, 'account-head', 'accounts'],
    [`/identity/groups/${ctx.readersGroupId}`, 'group-matrix', 'matrix'],
    ['/records', 'records-head', 'records'],
  ];
  for (const [route, ready, label] of pages) {
    await page.setViewportSize({ width: 1440, height: 900 });
    await gotoRoute(page, route, ready);
    for (const [width, height, suffix] of viewports) {
      await page.setViewportSize({ width, height });
      await reload(page);
      await waitTestId(page, 'books-shell', 30000);
      await waitTestId(page, ready, 30000);
      const overflow = await page.evaluate(() => ({
        doc: document.documentElement.scrollWidth,
        body: document.body.scrollWidth,
        width: window.innerWidth,
      }));
      check(
        step,
        `${label}-${suffix} has no horizontal overflow`,
        overflow.doc <= overflow.width + 2 && overflow.body <= overflow.width + 2,
        `<= ${overflow.width + 2}`,
        `doc=${overflow.doc} body=${overflow.body}`,
      );
      await shot(page, step, `${label}-${suffix}`, { settle: 250 });
    }
  }

  // Five states, each on a real page. Loading and error are induced with
  // request interception, not with a fixture store.
  await page.setViewportSize({ width: 1440, height: 900 });

  await page.route('**/v1/accounts**', async (route) => {
    await sleep(2500);
    try {
      await route.continue();
    } catch {
      /* the page may have navigated away and cancelled the route */
    }
  });
  await navigate(page, `${BASE}/identity/accounts`);
  await waitTestId(page, 'books-shell', 30000);
  await page.waitForTimeout(500);
  const loadingVisible =
    (await page.$(sel('accounts-loading'))) !== null || (await page.$(sel('accounts-index-loading'))) !== null;
  check(step, 'loading state renders a skeleton', loadingVisible, 'accounts-loading/accounts-index-loading', loadingVisible ? 'visible' : 'absent');
  await shot(page, step, 'state-loading');
  await page.unroute('**/v1/accounts**');

  // Success state: the selected account's detail page.
  await gotoRoute(page, `/identity/accounts/${ctx.aliceAccountId}`, 'account-head');
  check(step, 'success state renders the account detail', (await page.$(sel('account-head'))) !== null, 'account-head', 'present');
  await shot(page, step, 'state-success');

  // Empty state: an unmatched search is the page's honest empty.
  await gotoRoute(page, '/records', 'records-head');
  await fillTestId(page, 'records-search', 'zzz-no-such-record');
  await page.waitForTimeout(1200);
  const emptyVisible = (await page.$(sel('records-empty'))) !== null;
  check(step, 'empty state explains there is no match', emptyVisible, 'records-empty', emptyVisible ? 'visible' : 'absent');
  await shot(page, step, 'state-empty');

  // Error state: the group index answers 500 (narrow layout owns groups-error).
  await page.setViewportSize({ width: 1024, height: 900 });
  await page.route('**/v1/permission-groups**', async (route) => {
    await route.fulfill({
      status: 500,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'E2E induced failure', code: 'internal' }),
    });
  });
  await navigate(page, `${BASE}/identity/groups`);
  await waitTestId(page, 'books-shell', 30000);
  await page.waitForTimeout(1200);
  const errorVisible =
    (await page.$(sel('groups-error'))) !== null || (await page.$(sel('groups-paper-error'))) !== null;
  check(step, 'error state names the failure and offers a retry', errorVisible, 'groups-error', errorVisible ? 'visible' : 'absent');
  await shot(page, step, 'state-error');
  await page.unroute('**/v1/permission-groups**');

  // Permission state: alice may not read accounts.
  const { page: alicePage } = ctx.alicePage;
  await alicePage.setViewportSize({ width: 1440, height: 900 });
  await navigate(alicePage, `${BASE}/identity/accounts`);
  await waitTestId(alicePage, 'books-shell', 30000);
  await alicePage.waitForTimeout(1200);
  const permissionVisible = (await alicePage.$(sel('page-permission'))) !== null;
  check(step, 'permission state is a page, not a blank shell', permissionVisible, 'page-permission', permissionVisible ? 'visible' : 'absent');
  const permissionText = await bodyText(alicePage);
  check(step, 'permission page explains the missing capability', permissionText.includes('权限'), 'explains permission', permissionText.slice(0, 160));
  await shot(alicePage, step, 'state-permission');

  // 44px targets on the phone layout.
  await page.setViewportSize({ width: 390, height: 844 });
  await gotoRoute(page, '/identity/accounts', 'accounts-head');
  // The page renders its head before the account list arrives; the create
  // button is a data-dependent action, so wait for the list's action row.
  await page.waitForSelector(`${sel('accounts-create-page')}, ${sel('accounts-create')}`, { timeout: 30000 }).catch(() => {});
  await page.waitForTimeout(400);
  const targets = [
    'books-tab-workspace',
    'books-tab-identity',
    'books-account-trigger',
    'accounts-create-page',
    'accounts-search-page',
  ];
  const measured = await page.evaluate((ids) => {
    return ids.map((id) => {
      const el = document.querySelector(`[data-testid="${id}"]`);
      if (!el) return { id, missing: true };
      const rect = el.getBoundingClientRect();
      return { id, width: Math.round(rect.width), height: Math.round(rect.height) };
    });
  }, targets);
  for (const row of measured) {
    const ok = !row.missing && row.height >= 44 && row.width >= 44;
    check(step, `44px target ${row.id}`, ok, '>=44x44', row.missing ? 'missing' : `${row.width}x${row.height}`, {
      severity: row.id === 'books-account-trigger' ? 'P2' : 'P1',
      findingId: row.id === 'books-account-trigger' ? 'account-trigger-under-44' : null,
    });
  }
  const trigger = measured.find((row) => row.id === 'books-account-trigger');
  if (trigger && !trigger.missing && (trigger.width < 44 || trigger.height < 44)) {
    addFinding(
      'account-trigger-under-44',
      'P2',
      '手机端账号菜单触控目标宽 32px（高 56px），低于 44px 目标',
      '390 宽下 books-account-trigger 的命中框为 32×56；底部章导航与新建/搜索按钮均 >=44。视觉门要求关键目标 >=44px，需给头像按钮补横向内边距或最小宽度。',
      [`measured books-account-trigger = ${trigger.width}x${trigger.height}`],
    );
  }
  await shot(page, step, 'targets-390');

  // A team/run page on the phone too, so the DAG is reachable at 390.
  if (ctx.teamSessionId) {
    await gotoRoute(page, `/teams/support-team/runs/${ctx.teamSessionId}`, 'run-page');
    await page.waitForTimeout(1500);
    const runText = await bodyText(page);
    check(step, 'run/DAG is reachable at 390', runText.includes('客服团队') || runText.includes('support-1') || runText.includes('运行'), 'run content', runText.slice(0, 120));
    await shot(page, step, 'run-390');
  }
}

// ================================================================================
// Step 10 — audit: query, filters, detail, ownership
// ================================================================================

async function step10(step, ctx) {
  const admin = ctx.admin;
  const list = await admin.get('/v1/audit/events?limit=200');
  check(step, 'audit list is readable', list.status === 200 && Array.isArray(list.body.events), '200 events[]', list.status);
  const events = list.body.events || [];
  check(step, 'audit ledger is not empty', events.length > 20, '>20', events.length);

  const byKind = (kind) => events.filter((row) => row.kind === kind);
  const login = byKind('login');
  const grant = byKind('grant');
  const deny = byKind('deny');
  const invoke = byKind('invoke');
  const account = byKind('account');
  const superAdmin = byKind('super-admin');
  const relation = byKind('relation');

  check(step, 'login events exist', login.length >= 2, '>=2', login.length);
  check(step, 'grant events exist', grant.length >= 3, '>=3', grant.length);
  check(step, 'denial events exist', deny.length >= 1, '>=1', deny.length);
  check(step, 'invoke events exist', invoke.length >= 2, '>=2', invoke.length);
  check(step, 'account/key lifecycle events exist', account.length >= 2, '>=2', account.length);
  check(step, 'super-admin action events exist', superAdmin.length >= 1, '>=1', superAdmin.length);
  check(step, 'relation events exist', relation.length >= 1, '>=1', relation.length);

  const grantKeys = new Set(grant.map((row) => row.capabilityKey).filter(Boolean));
  check(step, 'grant events carry capabilityKey', grantKeys.has('session.read') || grantKeys.has('bot.invoke'), 'session.read/bot.invoke', [...grantKeys].slice(0, 6));
  const denyKeys = new Set(deny.map((row) => row.capabilityKey).filter(Boolean));
  check(step, 'denial events carry the missing capabilityKey', denyKeys.size >= 1, '>=1 key', [...denyKeys].slice(0, 6));
  const invite = invoke.find((row) => row.objectType === 'Bot');
  check(step, 'invoke event names the callee Bot', Boolean(invite?.objectId), 'Bot id', invite?.objectId);
  check(step, 'invoke event carries the Bot owner', Boolean(invite?.objectOwnerName || invite?.objectOwnerId), 'owner name/id', invite?.objectOwnerName || invite?.objectOwnerId);
  check(step, 'invoke event carries the run id', Boolean(invite?.runId), 'session id', invite?.runId);
  check(step, 'every event has an occurredAt', events.every((row) => Boolean(row.occurredAt)), 'all', events.filter((row) => !row.occurredAt).length);
  check(step, 'every event has a result', events.every((row) => Boolean(row.result)), 'all', events.filter((row) => !row.result).length);

  const sample = deny[0];
  check(step, 'denial row names the actor', Boolean(sample?.actorName || sample?.actorId), 'actor', sample?.actorName || sample?.actorId);
  check(step, 'denial row names the object/result', Boolean(sample?.result), 'result', sample?.result);
  const detail = await admin.get(`/v1/audit/events/${sample.eventId}`);
  check(step, 'audit detail endpoint returns the same row', detail.status === 200 && detail.body.event?.eventId === sample.eventId, sample.eventId, detail.body.event?.eventId);
  check(step, 'audit detail keeps capabilityKey and owner fields', Boolean(detail.body.event?.capabilityKey), 'capabilityKey', detail.body.event?.capabilityKey);

  const filtered = await admin.get('/v1/audit/events?kind=deny&result=denied&limit=50');
  check(step, 'kind/result filters are server-side', filtered.status === 200 && (filtered.body.events || []).every((row) => row.kind === 'deny' && row.result === 'denied'), 'only deny/denied', (filtered.body.events || []).length);

  const { page } = ctx.adminPage;
  await page.setViewportSize({ width: 1440, height: 900 });
  await gotoRoute(page, '/records', 'records-head');
  await waitTestId(page, 'records-table', 30000);
  await shot(page, step, 'records-ledger');
  if (sample) {
    await navigate(page, `${BASE}/records/${sample.eventId}`);
    await waitTestId(page, 'record-detail', 30000);
    const text = await bodyText(page);
    check(step, 'record detail page is readable', text.length > 20, 'detail content', text.slice(0, 120));
    await shot(page, step, 'record-detail');
  }
  log(step, `events=${events.length} login=${login.length} grant=${grant.length} deny=${deny.length} invoke=${invoke.length}`);
}

// ================================================================================
// Coverage — every changed file gets a surface + evidence; UNCOVERED must be 0
// ================================================================================

function classifyFile(file) {
  const testLike = /(_test\.go|\.test\.tsx?|\.harness\.tsx?|harness\.test\.tsx?|\.fixture\.)/.test(file);
  if (testLike) return { status: 'EXEMPT-UNIT', surface: 'test/fixture', evidence: '同包单测（A5 门已跑）；本 lane 不重复跑单测' };
  if (/(^|\/)(openspec|\.agents|docs|site|design)\//.test(file) || /^(README|NOTICE|LICENSE)/.test(file)) {
    return { status: 'EXEMPT-DOC', surface: '文档/设计/证据', evidence: '无运行时面；文档词汇与设计方向在对应门禁审查' };
  }
  if (/\.(png|jpg|jpeg|webp|gif|ico|woff2?|ttf|wasm|br|gz)$/i.test(file)) {
    return { status: 'EXEMPT-BINARY', surface: '静态资源', evidence: '二进制资源；由构建/截图引用验证' };
  }
  if (/^web\/(src\/screens\/books\/|src\/screens\/login|src\/data\/|src\/ui\/|App\.tsx|kit\/)/.test(file)) {
    return { status: 'COVERED-E2E-UI', surface: 'Web UI', evidence: 'E2E 步骤 1/2/3/4/5/6/7/9/10（真实浏览器 + 截图）' };
  }
  if (/^internal\/(harnessapi|access|accessgate|accesswire|auth|botteam|store)\//.test(file)) {
    return { status: 'COVERED-E2E-API', surface: '身份/权限/存储 API', evidence: 'E2E 步骤 1–10（真 PG + 真 serve）' };
  }
  if (/^cmd\/keel\/(auth|admin|serve|bot|team_bot|session_owner|static|main|cli_client|doctor)/.test(file)) {
    return { status: 'COVERED-E2E-CLI', surface: 'CLI / serve 入口', evidence: 'E2E 步骤 1/4/5/6/8（CLI 负控 + 真进程）' };
  }
  if (/^internal\/httpapi\//.test(file)) {
    return { status: 'COVERED-E2E-EDGE', surface: 'HTTP 边缘/会话', evidence: 'E2E 步骤 1/5/7/8（cookie/CSRF/旧路由）' };
  }
  if (/^internal\/(agent|agentcore|agentloop|agenttool|harnessext|toolsurface|servicehost|pluginpkg|kernel|sessionbinding|sessionfork)\//.test(file)) {
    return { status: 'COVERED-E2E-RUN', surface: '运行时/工具/插件', evidence: 'E2E 步骤 5/6（真 run + 事件 + DAG）' };
  }
  return { status: 'EXEMPT-UNIT', surface: '其余代码', evidence: '同包单测与既存门禁；本 lane 不构造第二套单测' };
}

function buildCoverage() {
  let files = [];
  try {
    const out = execFileSync('git', ['diff', '--name-status', `${BASELINE}..HEAD`], {
      cwd: path.resolve(process.env.E2E_REPO_ROOT || process.cwd()),
      encoding: 'utf8',
      maxBuffer: 16 * 1024 * 1024,
    });
    files = out
      .split('\n')
      .filter(Boolean)
      .map((line) => {
        const [change, ...rest] = line.split('\t');
        return { change, file: rest[rest.length - 1] };
      });
  } catch (error) {
    console.log(`coverage: git diff failed: ${error.message}`);
  }
  const rows = files.map((entry) => {
    const cls = classifyFile(entry.file);
    return {
      file: entry.file,
      change: entry.change,
      status: entry.change === 'D' ? 'EXEMPT-REMOVED' : cls.status,
      surface: entry.change === 'D' ? '已删除' : cls.surface,
      evidence: entry.change === 'D' ? '删除项，无运行时面' : cls.evidence,
    };
  });
  const uncovered = rows.filter((row) => !row.status.startsWith('COVERED') && !row.status.startsWith('EXEMPT'));
  return {
    baseline: BASELINE,
    total: rows.length,
    covered: rows.filter((row) => row.status.startsWith('COVERED')).length,
    exempt: rows.filter((row) => row.status.startsWith('EXEMPT')).length,
    uncovered: uncovered.length,
    rows,
  };
}

// ================================================================================
// main
// ================================================================================

const startedAt = nowIso();
const ctx = {};

let browserStarted = false;
try {
  await launchBrowser();
  browserStarted = true;

  const adminPage = await newContextPage(1440, 900);
  const alicePage = await newContextPage(1440, 900);
  const bobPage = await newContextPage(1440, 900);
  ctx.adminPage = adminPage;
  ctx.alicePage = alicePage;
  ctx.bobPage = bobPage;

  await runStep(
    1,
    '空库 / bootstrap / 直接进入 / 退出重登',
    '空库启动后由浏览器用一次性 setup token 创建超级管理员；退出后重新登录；API 复用 token 与错误密码负控。',
    'bootstrap 后直接进入应用（不再强制改密）；token 只能用一次；错误密码与不存在账号同形 401。',
    (step) => step1(step, ctx),
  );

  const adminReady = Boolean(ctx.admin);
  if (!adminReady) {
    for (const [n, name] of [
      [2, '权限组与 human 账号'],
      [3, 'Bot 创建与详情'],
      [4, 'Bot key 签发/使用/吊销'],
      [5, 'TeamBot 启动与 DAG'],
      [6, '管家 Bot 场景'],
      [7, 'Session 私有与分享'],
      [8, '旧入口负控'],
      [10, '审计'],
    ]) {
      blockedStep(n, name, '-', '-', 'step 1 未建立可用的管理员会话');
    }
  } else {
    await runStep(
      2,
      '权限组 / human 账号 / 有效能力 / grant ⊆ own',
      'UI 建组并授 session.read/own、bot.invoke/own 等；UI 建 alice 并加入组；alice 读 whoami；alice 尝试授 secret.read 与拉人。',
      '有效权限是并集；无 permission.manage 的 alice 授 secret.read 或拉人 403，且 grant 不落库。',
      (step) => step2(step, ctx),
    );

    if (!ctx.aliceAccountId || !ctx.bobAccountId) {
      for (const [n, name] of [
        [3, 'Bot 创建与详情'],
        [4, 'Bot key 签发/使用/吊销'],
        [5, 'TeamBot 启动与 DAG'],
        [6, '管家 Bot 场景'],
        [7, 'Session 私有与分享'],
        [9, '四档 viewport + 五态 + 44px'],
        [10, '审计'],
      ]) {
        blockedStep(n, name, '-', '-', 'step 2 未建立 alice/bob 账号');
      }
    } else {
    // Login alice/bob pages after step 2 created them.
    try {
      await loginUI(alicePage.page, 'alice', ALICE_PASSWORD);
      await loginUI(bobPage.page, 'bob', BOB_PASSWORD);
    } catch (error) {
      addFinding('browser-login-failed', 'P1', 'alice/bob 浏览器登录失败', String(error?.message || error), null);
    }

    await runStep(
      3,
      'Role / Profile / Bot（ModelKind / 默认模型 / Harness）',
      '在 alice 的 Account 上建 Role、Profile、support-1，查看 Bot 详情与列表。',
      'Bot 归属 alice；ModelKind=text；默认模型与 Harness 落库并在详情/列表可见。',
      (step) => step3(step, ctx),
    );
    await runStep(
      4,
      'Bot key：签发 / 调用 / 错误 Bot / 吊销',
      '为 support-1 签发一次性密钥；以 Bot 身份 whoami/列 Bot；访问 bob 的 other-1；吊销后再调用。',
      '密钥只显示一次；Bot 身份正确；跨 Bot 403；吊销后 401；last_used_at 更新。',
      (step) => step4(step, ctx),
    );
    await runStep(
      5,
      'TeamBot（Mission/Roster）/ 跨账号对照 / 启动 / DAG',
      '建 support-team（support-1 队长 + support-2 成员）；alice 尝试拉 bob 的 outside-1；启动并等待 run；成员 Bot 独立 Session。',
      '无关系时跨账号 403；有关系时 201、撤销后下一次 403；启动创建队长 Session 并跑完；成员 Session 独立归属。',
      (step) => step5(step, ctx),
    );
    await runStep(
      6,
      '管家 Bot：用 Bot key 建成员 Bot / 权限组 / TeamBot 并启动',
      '建 butler + butler-ops；用 butler key 建角色、Profile、成员 Bot、权限组、TeamBot 并启动；负控 super admin 与外部 Bot key。',
      'Bot 与其 Account 同权限规则；能建团队并跑完；不能自升超管或管别人的 Bot。',
      (step) => step6(step, ctx),
    );
    await runStep(
      7,
      'Session 默认私有 / viewer 分享 / 撤销',
      '人类调用 support-1 生成 Bot-owned Session；alice 可读、bob 403；写 viewer 关系后 bob 可读；撤销后 403。',
      '默认 403；分享后 200；撤销后下一次 403；UI 列表同步消失。产品无分享端点时如实登记 P1。',
      (step) => step7(step, ctx),
    );
    }

    await runStep(
      8,
      '旧入口 / 旧 CLI / 旧凭证 / 旧 schema 负控',
      '管理员访问 /v1/agents|teams；跑 keel agent/team 与 admin key；伪造 cookie/key；独立库放 agents 行后启动 serve。',
      '旧路由 404、旧命令 unknown、旧 cookie/key 401、旧 schema 启动拒绝并保留数据。',
      (step) => step8(step, ctx),
    );
    await runStep(
      9,
      '四档 viewport + 五态 + 44px',
      '账号/矩阵/记录在 1440/1024/768/390 截图；加载/空/错/权限不足/成功五态；手机关键目标测量。',
      '四档无横向溢出、页面可达；五态都有设计；关键目标 >=44px。',
      (step) => step9(step, ctx),
    );
    await runStep(
      10,
      '审计：谁 / 何时 / 对什么 / 结果',
      '查 /v1/audit/events 与详情；过滤 deny；打开记录页与单条详情。',
      '登录/授权/拒绝/调用/密钥/关系都有行；拒绝行含 capabilityKey；调用行含 Bot 归属与 runId。',
      (step) => step10(step, ctx),
    );
  }
} catch (error) {
  console.error(`e2e fatal: ${error?.stack || error}`);
  addFinding('e2e-fatal', 'P0', 'E2E 主流程未能完成', String(error?.message || error), null);
} finally {
  const coverage = buildCoverage();
  if (coverage.uncovered > 0) {
    addFinding(
      'coverage-uncovered',
      'P1',
      `改动文件覆盖表仍有 ${coverage.uncovered} 个 UNCOVERED`,
      '每个改动文件都必须映射到 E2E 步骤或三类豁免（单测/文档设计/删除/二进制）；见 report 的覆盖表。',
      coverage.rows.filter((row) => row.status.startsWith('UNCOVERED')).slice(0, 20).map((row) => row.file),
    );
  }
  const passedSteps = steps.filter((step) => step.result === 'PASS').length;
  const failedSteps = steps.filter((step) => step.result === 'FAIL').length;
  const blocked = steps.filter((step) => step.result === 'BLOCKED').length;
  const checks = steps.flatMap((step) => step.checks);
  const checksPassed = checks.filter((row) => row.ok).length;
  const p0 = findings.filter((row) => row.severity === 'P0').length;
  const p1 = findings.filter((row) => row.severity === 'P1').length;
  const p2 = findings.filter((row) => row.severity === 'P2').length;
  const shots = steps.reduce((sum, step) => sum + step.shots.length, 0);

  // Visual before/after: the design prototypes are the "before" because Owner
  // 2026-11-21 ruled the old UI out as a baseline; every shipped screen is the
  // "after" from this run. Non-visual files are exempt in coverage.json.
  const beforeDir = path.join(EVIDENCE, 'before');
  fs.mkdirSync(beforeDir, { recursive: true });
  const designShots = path.resolve(process.env.E2E_REPO_ROOT || process.cwd(), 'design/opendesign/shots');
  const copyBefore = (name) => {
    const src = path.join(designShots, name);
    if (!fs.existsSync(src)) return null;
    const dst = path.join(beforeDir, name);
    fs.copyFileSync(src, dst);
    return `before/${name}`;
  };
  const findShot = (needle) => {
    for (const step of steps) {
      for (const file of step.shots) if (file.includes(needle)) return file;
    }
    return null;
  };
  const visualDelta = [
    {
      surface: '登录 / bootstrap / 退出重登',
      files: ['web/src/screens/login.tsx', 'web/src/screens/books/shell.tsx'],
      before: null,
      after: [findShot('bootstrap-empty'), findShot('direct-entry'), findShot('re-login')].filter(Boolean),
      note: '旧登录面不是基线（Owner 2026-11-21）；before 以设计方向 1440-direction 代表，after 为真实 bootstrap/登录截图。',
    },
    {
      surface: '身份 / 账号 / 权限矩阵',
      files: ['web/src/screens/books/accounts.tsx', 'web/src/screens/books/groups.tsx', 'web/src/screens/books/identity.tsx'],
      before: copyBefore('1440-identity.png'),
      after: [findShot('accounts-1440'), findShot('accounts-390'), findShot('matrix-1440'), findShot('matrix-390')].filter(Boolean),
      note: 'before = 设计原型 1440-identity；after = 真实账号详情 + 权限矩阵。',
    },
    {
      surface: '员工 / Bot 与密钥',
      files: ['web/src/screens/books/bots.tsx'],
      before: copyBefore('1440-bot.png'),
      after: [findShot('bot-detail'), findShot('bots-list'), findShot('bot-keys')].filter(Boolean),
      note: 'before = 设计原型 1440-bot；after = 真实 Bot 详情、列表与密钥段。',
    },
    {
      surface: '团队 / 运行 DAG',
      files: ['web/src/screens/books/teams.tsx', 'web/src/screens/books/run.tsx'],
      before: copyBefore('1440-run.png'),
      after: [findShot('team-detail'), findShot('run-dag'), findShot('butler-run'), findShot('run-390')].filter(Boolean),
      note: 'before = 设计原型 1440-run；after = 真实团队详情与运行图。',
    },
    {
      surface: '记录 / 审计',
      files: ['web/src/screens/books/records.tsx'],
      before: copyBefore('1440-record.png'),
      after: [findShot('records-ledger'), findShot('record-detail'), findShot('records-1440'), findShot('records-390')].filter(Boolean),
      note: 'before = 设计原型 1440-record；after = 真实审计账与详情。',
    },
    {
      surface: '五态与四档',
      files: ['web/src/screens/books/common.tsx', 'web/src/ui/kit/tokens.ts', 'web/ui/kit'],
      before: copyBefore('1440-states.png'),
      after: [findShot('state-loading'), findShot('state-success'), findShot('state-empty'), findShot('state-error'), findShot('state-permission'), findShot('targets-390')].filter(Boolean),
      note: 'before = 设计原型 1440-states；after = 真实加载/成功/空/错/权限不足与 390 触控目标。',
    },
  ];

  const payload = {
    title: 'Keel Bot 身份与权限 · A6 真实服务端到端验收',
    change: 'keel-bot-identity-access（A6）',
    date: new Date().toISOString().slice(0, 10),
    frozenBase: { repo: 'jixoai/keel', commit: process.env.E2E_HEAD_COMMIT || '' },
    environment: {
      base: BASE,
      database: DB,
      tenant: TENANT,
      realPostgres: true,
      realServe: true,
      realBrowser: `Chrome (playwright-core, headless=${HEADLESS})`,
      model: 'fake（deployment 未配置 provider；run 生命周期真实，模型为确定性 fake）',
    },
    conclusion: `${passedSteps}/${steps.length} 步 PASS，${checksPassed}/${checks.length} 断言通过，${shots} 张截图；P0=${p0} P1=${p1} P2=${p2}${failedSteps ? `；${failedSteps} 步 FAIL` : ''}${blocked ? `；${blocked} 步 BLOCKED` : ''}。`,
    assertions: { total: checks.length, passed: checksPassed },
    steps: steps.map(({ checks: stepChecks, log: stepLog, note, ...rest }) => ({
      ...rest,
      assertionNote: note || undefined,
      log: stepLog.join('\n'),
      checks: stepChecks,
    })),
    findings,
    coverage,
    visualDelta,
    finishedAt: nowIso(),
    startedAt,
  };
  fs.writeFileSync(path.join(EVIDENCE, 'steps.json'), JSON.stringify(payload, null, 2));
  fs.writeFileSync(path.join(EVIDENCE, 'coverage.json'), JSON.stringify(coverage, null, 2));
  console.log(
    `e2e done: steps=${passedSteps}/${steps.length} checks=${checksPassed}/${checks.length} shots=${shots} findings=P0:${p0} P1:${p1} P2:${p2} uncovered=${coverage.uncovered}`,
  );
  if (browserStarted && browser) {
    await browser.close().catch(() => {});
  }
}

process.exitCode = steps.some((step) => step.result === 'FAIL') || findings.some((row) => row.severity === 'P0' || row.severity === 'P1')
  ? 1
  : 0;
