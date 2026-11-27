#!/usr/bin/env node
// Real-service browser E2E for change keel-plugin-attachment (tasks 5.4, 5.5
// and the 3.6 viewport walk).
//
// One process owns the browser lane (playwright-core + the machine's Chrome)
// and the API lane (fetch with a cookie jar + CSRF). It proves the product
// surface, not a mock:
//
//   1. real uploads + a real input; the canonical AttachmentPart; the model
//      request carries a native image part for the PNG and a text fallback
//      (blob id, no bytes) for the PDF;
//   2. the browser's default attachment rows before any plugin exists;
//   3. install → enable a preview plugin through the durable approval: its
//      claim is visible in the approval prompt and the chunk renders above the
//      default facts;
//   4. disable: the preview disappears and the default row remains;
//   5. a throwing renderer: an explained failure, the default row and the
//      other attachment unaffected;
//   6. a reader without `file.read`: the no-permission face, no thumbnail or
//      download bytes;
//   7. an outsider without `session.read`: refusal, no attachment bytes;
//   8. tenant B against tenant A's URLs: 404 for file/thumbnail/part, including
//      a forged X-Tenant-ID;
//   9. 390/768/1024/1440: no horizontal overflow, one screenshot each.
//
// Every step writes checks and screenshots; steps.json feeds
// scripts/keel-identity-e2e/report-gen.mjs. No password, bootstrap token, DSN
// or blob byte ever reaches steps.json, a log line or a screenshot.
import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { createRequire } from 'node:module';
import os from 'node:os';

const BASE = (process.env.E2E_BASE || '').replace(/\/$/, '');
const BASE_B = (process.env.E2E_BASE_B || '').replace(/\/$/, '');
const EVIDENCE = process.env.E2E_EVIDENCE || '';
const RUN_DIR = process.env.E2E_RUN_DIR || EVIDENCE;
const SHOTS_DIR = 'screenshots';
const SHOTS = path.join(EVIDENCE, SHOTS_DIR);
const WORKTREE = process.env.E2E_WORKTREE || process.cwd();
const HEAD_COMMIT = process.env.E2E_HEAD_COMMIT || '';
const COVERAGE_BASE = process.env.E2E_COVERAGE_BASE || '8916393';
const ADMIN_USER = process.env.E2E_ADMIN_USERNAME || 'e2e-admin';
// Portability: an explicit path wins; otherwise resolve the installed package
// and let Playwright pick its browser (or require E2E_CHROME on a machine
// without one). No machine-specific cache path is the only default.
const nodeRequire = createRequire(import.meta.url);
const resolvePlaywright = () => {
  if (process.env.PLAYWRIGHT_CORE) return process.env.PLAYWRIGHT_CORE;
  try {
    return nodeRequire.resolve('playwright-core');
  } catch {
    /* not installed next to the worktree: try the machine's npx cache below */
  }
  const npx = path.join(os.homedir(), '.npm', '_npx');
  try {
    for (const entry of fs.readdirSync(npx)) {
      const candidate = path.join(npx, entry, 'node_modules', 'playwright-core', 'index.mjs');
      if (fs.existsSync(candidate)) return candidate;
    }
  } catch {
    /* no cache directory */
  }
  return '';
};
const PLAYWRIGHT = resolvePlaywright();
const CHROME = process.env.E2E_CHROME || (process.platform === 'darwin'
  ? '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'
  : '');
const HEADLESS = (process.env.E2E_HEADLESS || 'true') !== 'false';
const TENANT = process.env.E2E_TENANT || '';
const TENANT_B = process.env.E2E_TENANT_B || '';
const DSN_FILE = process.env.E2E_DSN_FILE || '';
const MODEL_LOG = process.env.E2E_MODEL_LOG || '';
const PACKAGE_ROOT = process.env.E2E_PLUGINS_ROOT || path.join(RUN_DIR, 'e2e-plugins');
const PREVIEW_PLUGIN = 'e2e.attachment-preview';
const BROKEN_PLUGIN = 'e2e.attachment-broken';

if (!BASE || !BASE_B || !EVIDENCE) {
  console.error('E2E_BASE, E2E_BASE_B and E2E_EVIDENCE are required');
  process.exit(2);
}
fs.mkdirSync(EVIDENCE, { recursive: true });
// A stale artifact is a lying artifact: this run owns the screenshots it writes.
fs.rmSync(SHOTS, { recursive: true, force: true });
fs.mkdirSync(SHOTS, { recursive: true });
fs.mkdirSync(PACKAGE_ROOT, { recursive: true });

const readSecret = (file) => {
  if (!file) throw new Error('secret file is not configured');
  return fs.readFileSync(file, 'utf8').trim();
};
const ADMIN_PASSWORD = readSecret(process.env.E2E_ADMIN_PASSWORD_FILE);
const READER_PASSWORD = readSecret(process.env.E2E_READER_PASSWORD_FILE);
const OUTSIDER_PASSWORD = readSecret(process.env.E2E_OUTSIDER_PASSWORD_FILE);
const TENANT_B_PASSWORD = readSecret(process.env.E2E_TENANT_B_PASSWORD_FILE);

const nowIso = () => new Date().toISOString();
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const log = (line) => console.log(`[e2e] ${line}`);

// ---- evidence -----------------------------------------------------------------

const steps = [];
const findings = [];
let shotSeq = 0;

function newStep(n, name, action, expect) {
  const step = { n, name, action, expect, result: 'RUNNING', startedAt: nowIso(), shots: [], checks: [], log: '' };
  steps.push(step);
  return step;
}

function check(step, name, ok, expected, actual) {
  step.checks.push({ name, ok: Boolean(ok), expected: String(expected ?? '—'), actual: String(actual ?? '—') });
  if (!ok) log(`FAIL ${name}: expected=${expected} actual=${actual}`);
  return Boolean(ok);
}

function logLine(step, text) {
  step.log = `${step.log}${step.log ? '\n' : ''}${text}`;
}

const addFinding = (id, severity, title, detail, evidence) =>
  findings.push({ id, severity, title, detail, ...(evidence ? { evidence: [evidence] } : {}) });

function finishStep(step) {
  step.finishedAt = nowIso();
  step.result = step.checks.every((entry) => entry.ok) ? 'PASS' : 'FAIL';
  console.log(`[step ${step.n}] ${step.result} ${step.name} (${step.checks.filter((c) => c.ok).length}/${step.checks.length})`);
}

async function runStep(n, name, action, expect, fn) {
  const step = newStep(n, name, action, expect);
  try {
    await fn(step);
  } catch (error) {
    check(step, `${name} 执行完成`, false, 'no throw', error?.message || String(error));
    addFinding(`step-${n}`, 'P0', `E2E 步骤 ${n} 失败：${name}`, String(error?.stack || error), null);
    logLine(step, String(error?.stack || error));
  }
  finishStep(step);
  return step;
}

async function shot(page, step, name, options = {}) {
  shotSeq += 1;
  const file = `${String(shotSeq).padStart(2, '0')}-${name}.png`;
  await page.waitForTimeout(250);
  await page.screenshot({ path: path.join(SHOTS, file), fullPage: options.fullPage !== false });
  step.shots.push(`${SHOTS_DIR}/${file}`);
  return `${SHOTS_DIR}/${file}`;
}

// ---- HTTP client (cookie jar + CSRF) --------------------------------------------

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
  constructor(label, base = BASE) {
    this.label = label;
    this.base = base;
    this.cookies = new Map();
    this.csrf = '';
    this.accountId = '';
  }

  cookieHeader() {
    return [...this.cookies.entries()].map(([k, v]) => `${k}=${v}`).join('; ');
  }

  async request(method, endpoint, options = {}) {
    const headers = { ...(options.headers || {}) };
    if (this.cookies.size > 0) headers.Cookie = this.cookieHeader();
    if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS' && this.csrf) {
      headers['X-Keel-CSRF'] = this.csrf;
    }
    let body;
    if (options.raw !== undefined) {
      headers['Content-Type'] = options.contentType || 'application/octet-stream';
      headers['Content-Length'] = String(options.raw.length);
      body = options.raw;
    } else if (options.body !== undefined) {
      headers['Content-Type'] = 'application/json';
      body = JSON.stringify(options.body);
    }
    const response = await fetch(this.base + endpoint, {
      method,
      headers,
      body,
      redirect: 'manual',
      signal: AbortSignal.timeout(options.timeout ?? 30000),
    });
    for (const [name, value] of parseSetCookie(response.headers)) this.cookies.set(name, value);
    const buffer = Buffer.from(await response.arrayBuffer());
    const text = buffer.toString('utf8');
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
    const account = parsed && typeof parsed === 'object' ? parsed.account : null;
    if (account && account.id) this.accountId = String(account.id);
    return { status: response.status, body: parsed ?? {}, headers: response.headers, buffer };
  }

  get(endpoint, options) {
    return this.request('GET', endpoint, options);
  }
  head(endpoint, options) {
    return this.request('HEAD', endpoint, options);
  }
  post(endpoint, body, options) {
    return this.request('POST', endpoint, { ...options, body });
  }
  upload(bytes, mime, options = {}) {
    return this.request('POST', '/v1/files', { ...options, raw: bytes, contentType: mime });
  }

  async session() {
    return this.get('/v1/auth/session');
  }

  async login(username, password) {
    await this.session();
    return this.post('/v1/auth/login', { username, password });
  }
}

async function approveAndRepeat(api, action, endpoint, body = {}) {
  const first = await api.post(endpoint, body);
  if (first.status !== 202) return { first, answer: null, final: first };
  const decisionId = first.body?.decision?.decisionId;
  if (!decisionId) throw new Error(`${action}: 202 without a decisionId`);
  const answer = await api.post(`/v1/plugin-approvals/${decisionId}`, { optionId: 'allow' });
  if (answer.status !== 200) throw new Error(`${action}: approval answer = ${answer.status}`);
  const final = await api.post(endpoint, { ...body, decisionId });
  return { first, answer, final };
}

// ---- DB / model log ------------------------------------------------------------

function psql(sql) {
  const dsn = fs.readFileSync(DSN_FILE, 'utf8').trim();
  return execFileSync('psql', [dsn, '-Atc', sql], { encoding: 'utf8', timeout: 20000 }).trim();
}

function modelRequests() {
  if (!MODEL_LOG || !fs.existsSync(MODEL_LOG)) return [];
  return fs
    .readFileSync(MODEL_LOG, 'utf8')
    .split('\n')
    .filter(Boolean)
    .map((line) => {
      try {
        return JSON.parse(line);
      } catch {
        return null;
      }
    })
    .filter(Boolean);
}

// ---- browser helpers ------------------------------------------------------------

let browser = null;

async function launchBrowser() {
  if (!PLAYWRIGHT) {
    throw new Error('playwright-core is not installed; set PLAYWRIGHT_CORE to its index.mjs (E2E is not run by the app build)');
  }
  const playwright = await import(PLAYWRIGHT);
  browser = await playwright.chromium.launch({
    ...(CHROME ? { executablePath: CHROME } : {}),
    headless: HEADLESS,
    args: ['--disable-dev-shm-usage'],
  });
}

async function newPage(width = 1440, height = 900, storageState = undefined) {
  const context = await browser.newContext({
    viewport: { width, height },
    deviceScaleFactor: 1,
    ...(storageState ? { storageState } : {}),
  });
  const page = await context.newPage();
  page.setDefaultTimeout(60000);
  page.setDefaultNavigationTimeout(60000);
  page.on('pageerror', (error) => log(`[browser] pageerror: ${error.message}`));
  page.on('requestfailed', (request) => {
    const url = request.url();
    if (!url.includes('/v1/events') && !url.includes('/v1/realtime') && !url.endsWith('/favicon.ico')) {
      log(`[browser] requestfailed: ${url} ${request.failure()?.errorText ?? ''}`);
    }
  });
  return { context, page };
}

const sel = (testId) => `[data-testid="${testId}"]`;

async function waitTestId(page, testId, timeout = 30000) {
  await page.waitForSelector(sel(testId), { timeout, state: 'visible' });
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

async function loginUI(page, base, username, password) {
  await navigate(page, base);
  await waitTestId(page, 'login-gate');
  await page.fill(`${sel('login-username')} input`, username);
  await page.fill(`${sel('login-password')} input`, password);
  await page.click(sel('login-submit'));
  await waitTestId(page, 'books-shell', 40000);
}

async function openSession(page, base, sessionId) {
  await navigate(page, base);
  await waitTestId(page, 'books-shell', 40000);
  await waitTestId(page, `session-row-${sessionId}`, 40000);
  await page.click(sel(`session-row-${sessionId}`));
  await waitTestId(page, 'conversation-panel', 40000);
}

async function bodyText(page) {
  return page.locator('body').innerText();
}

async function overflow(page) {
  return page.evaluate(() => ({
    inner: window.innerWidth,
    doc: document.documentElement.scrollWidth,
    body: document.body ? document.body.scrollWidth : null,
  }));
}

async function waitFor(fn, timeoutMs = 30000, intervalMs = 400) {
  const started = Date.now();
  let last = null;
  while (Date.now() - started < timeoutMs) {
    try {
      last = await fn();
      if (last) return last;
    } catch (error) {
      last = error;
    }
    await sleep(intervalMs);
  }
  throw new Error(`timed out after ${timeoutMs}ms (last=${last})`);
}

async function waitRun(api, sessionId, timeoutMs = 90000) {
  return waitFor(async () => {
    const run = await api.get(`/v1/sessions/${sessionId}/run`);
    const status = run.body?.status;
    if (status === 'done' || status === 'failed' || status === 'completed') return status;
    return null;
  }, timeoutMs);
}

async function waitThumbReady(api, fileId, timeoutMs = 45000) {
  return waitFor(async () => {
    const meta = await api.get(`/v1/files/${fileId}/meta`);
    return meta.body?.thumbStatus === 'ready' ? meta.body : null;
  }, timeoutMs);
}

async function waitInventory(api, pluginId, state = 'enabled', timeoutMs = 30000) {
  return waitFor(async () => {
    const res = await api.get('/v1/plugins');
    const row = (res.body?.plugins || []).find((plugin) => plugin.id === pluginId);
    if (!row) return null;
    if (state && row.state !== state) return null;
    return row;
  }, timeoutMs);
}

// ---- fixtures ---------------------------------------------------------------

const PNG = fs.readFileSync(path.join(WORKTREE, 'web/assets/demo-shot.png'));
const PDF = Buffer.from(
  '%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n' +
    '3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]/Contents 4 0 R>>endobj\n' +
    '4 0 obj<</Length 44>>stream\nBT /F1 12 Tf 20 100 Td (attachment e2e) Tj ET\nendstream\nendobj\n' +
    'trailer<</Root 1 0 R>>\n%%EOF\n',
);
const PREVIEW_CHUNK = `// E2E attachment preview chunk (served from /plugins/<id>/ui/preview.js).
const realm = typeof globalThis === "object" ? globalThis.__nsh_browser_realm : undefined;
const modules = realm && realm.modules;
const nsh = (modules && typeof modules.get === "function" && modules.get("nsh")) || {};
const Text = nsh.Text;
const View = nsh.View;
export function render(context) {
  if (typeof Text !== "function" || typeof View !== "function") return null;
  const part = (context && context.props && context.props.part) || {};
  return View({
    testID: "e2e-attachment-preview",
    style: { padding: 8, gap: 2, flexDirection: "column", borderRadius: 8 },
    children: [
      Text({ style: { fontSize: 13, fontWeight: 600 }, children: "E2E 插件附件预览" }),
      Text({ style: { fontSize: 12 }, children: (part.name || "?") + " · " + (part.mime || "?") + " · " + (part.size || 0) + " bytes" }),
    ],
  });
}
`;
const THROWING_CHUNK = `// E2E broken attachment preview: the host must fall back, not blank the row.
export function render() {
  throw new Error("E2E-ATTACHMENT-PREVIEW-EXPLODED");
}
`;

/** Write one plugin package and return its directory. */
function writePackage(id, { chunk, mime = ['image/png'], title }) {
  const dir = path.join(PACKAGE_ROOT, id);
  fs.rmSync(dir, { recursive: true, force: true });
  fs.mkdirSync(path.join(dir, 'ui'), { recursive: true });
  fs.writeFileSync(
    path.join(dir, 'plugin.yaml'),
    [
      `id: ${id}`,
      'version: 0.1.0',
      'kind: webview',
      `description: ${title}`,
      'engines:',
      '  keel: ">=0.1.0 <1.0.0"',
      'provides:',
      '  ui:',
      '    entry: ui/index.html',
      `    title: ${title}`,
      'contributes:',
      '  - slot: keel.ui.message-part',
      '    form: chunk',
      '    entry: ui/preview.js',
      'extensibility:',
      '  doc: EXTENSIBILITY.md',
      '  schema: extensibility.json',
      'source:',
      '  type: local',
      '',
    ].join('\n'),
  );
  fs.writeFileSync(path.join(dir, 'ui', 'index.html'), `<!doctype html><title>${title}</title><p>${id}</p>\n`);
  fs.writeFileSync(path.join(dir, 'ui', 'preview.js'), chunk);
  fs.writeFileSync(path.join(dir, 'EXTENSIBILITY.md'), `# ${title}\n\nE2E attachment preview fixture.\n`);
  fs.writeFileSync(
    path.join(dir, 'extensibility.json'),
    JSON.stringify(
      {
        schema: 'keel.plugin-extensibility/1',
        plugin: id,
        version: '0.1.0',
        summary: 'E2E attachment preview fixture',
        slots: [],
        contributes: [
          {
            slot: 'keel.ui.message-part',
            form: 'chunk',
            entry: 'ui/preview.js',
            part: 'attachment',
            mime,
          },
        ],
        configuration: [],
        hooks: [],
        providers: [],
        examples: [],
        limits: [],
      },
      null,
      2,
    ) + '\n',
  );
  return dir;
}

// ---- change coverage ------------------------------------------------------------

function changedFiles() {
  const tracked = execFileSync('git', ['diff', '--name-only', COVERAGE_BASE], { cwd: WORKTREE, encoding: 'utf8' })
    .split('\n')
    .filter(Boolean);
  const untracked = execFileSync('git', ['ls-files', '--others', '--exclude-standard'], {
    cwd: WORKTREE,
    encoding: 'utf8',
  })
    .split('\n')
    .filter(Boolean);
  return [...new Set([...tracked, ...untracked])].sort();
}

const COVERAGE_MAP = [
  [/^web\/src\/screens\/messages\.tsx$/, '附件行 / 五态 / 插件槽位', '步骤 2、3、4、6'],
  [/^web\/src\/screens\/attachment-row\.tsx$/, '附件行五态 / 缩略图 / 无权限', '步骤 2、6 + TUI harness'],
  [/^web\/src\/screens\/attachment-row\.harness\.tsx$/, '附件行 harness', 'TUI harness'],
  [/^web\/src\/screens\/attachment-row\.tui\.test\.tsx$/, '五态与回退渲染单测', 'nsh test --target tui'],
  [/^web\/src\/plugins\/slots\.tsx$/, '预览槽位与回退面', '步骤 3、4、5'],
  [/^web\/src\/plugins\/registry\.ts$/, '声明顺序 + MIME 选择 + bridge 生命周期', 'TUI registry/bridge tests'],
  [/^web\/src\/plugins\/registry\.test\.ts$/, 'MIME/顺序/iframe 契约单测', 'nsh test --target tui'],
  [/^web\/src\/plugins\/bridge\.ts$/, '订阅/退订生命周期', 'TUI bridge tests'],
  [/^web\/src\/plugins\/bridge\.test\.ts$/, '订阅清理单测', 'nsh test --target tui'],
  [/^web\/src\/plugins\/sync\.ts$/, 'inventory 顺序同步', 'TUI registry tests'],
  [/^web\/src\/plugins\/validation\.ts$/, 'mime 声明 / iframe 契约校验', 'TUI registry tests'],
  [/^web\/src\/plugins\/mime\.ts$/, 'MIME 语法与匹配', 'TUI registry tests'],
  [/^web\/src\/plugins\/mime\.test\.ts$/, 'MIME 单测', 'nsh test --target tui'],
  [/^web\/src\/plugins\/types\.ts$/, '槽位契约', '编译 + TUI 测试'],
  [/^web\/src\/plugins\/bundled\.ts$/, '测试 chunk 入口', 'TUI harness'],
  [/^web\/src\/plugins\/fixtures\//, '测试 chunk', 'TUI harness'],
  [/^web\/src\/plugins\/slots\.tui\.test\.tsx$/, 'TUI 附件测试', 'nsh test --target tui'],
  [/^web\/src\/data\/attachments\.ts$/, 'AttachmentPart 读取/缓存/权限', '步骤 1、6'],
  [/^web\/src\/data\/attachments\.test\.ts$/, '附件投影状态/刷新/去重单测', 'nsh test --target tui'],
  [/^web\/src\/data\/attachment-link\.ts$/, '同源下载/打开入口', '步骤 2、8'],
  [/^web\/src\/data\/attachment-link\.test\.ts$/, '同源策略单测', 'nsh test --target tui'],
  [/^web\/src\/data\/contract\.ts$/, 'AttachmentPart 契约', '步骤 1、2'],
  [/^web\/src\/data\/http\.ts$/, 'part 路由客户端', '步骤 1、2'],
  [/^web\/src\/data\/store\.ts$/, 'part 读取入口 / 403 权限面', '步骤 2、6、7'],
  [/^web\/src\/data\/fixture\.ts$/, '演示数据（插件/附件）', 'TUI 测试'],
];

/** Only documentation/evidence and the E2E harness itself may be exempt. A
 * product or test file outside COVERAGE_MAP is UNCOVERED, never silently
 * exempt. */
const EXEMPT_ALLOWLIST = [/^openspec\//, /^scripts\/keel-attachment-e2e\//];

function coverageRows() {
  return changedFiles().map((file) => {
    const match = COVERAGE_MAP.find(([pattern]) => pattern.test(file));
    if (match) {
      return { file, change: 'changed', status: 'COVERED', surface: match[1], evidence: match[2] };
    }
    if (EXEMPT_ALLOWLIST.some((pattern) => pattern.test(file))) {
      return {
        file,
        change: 'changed',
        status: 'EXEMPT',
        surface: file.startsWith('openspec/') ? 'change 文档/证据' : 'E2E 运行器（证据生产者）',
        evidence: 'EXEMPT：显式白名单',
      };
    }
    return {
      file,
      change: 'changed',
      status: 'UNCOVERED',
      surface: '未映射产品/测试文件',
      evidence: 'UNCOVERED：必须映射或补证据',
    };
  });
}

// ---- steps ---------------------------------------------------------------------

async function prepareBot(admin, { roleName, botId, accountId, instruction }) {
  const role = await admin.post('/v1/roles', { name: roleName, instruction });
  const profile = await admin.post('/v1/profiles', {
    accountId,
    roleId: role.body?.role?.roleId,
  });
  const bot = await admin.post('/v1/bots', {
    botId,
    name: roleName,
    profileId: profile.body?.profile?.profileId,
    modelKind: 'text',
    defaultModel: { providerId: 'e2e-scripted', modelId: 'e2e-attachment' },
    harness: { tools: [], context: { instruction, permissions: [] } },
  });
  return { role, profile, bot };
}

async function step1(step, ctx) {
  const admin = ctx.admin;
  const health = await admin.get('/v1/health');
  check(step, 'tenant A 服务健康', health.status === 200, 200, health.status);

  // A run needs a Bot-owned session (the selection binding). The admin owns
  // this Bot; its effective grants come from the caller's account, so the
  // admin joins the built-in bot-operator group.
  const groups = await admin.get('/v1/permission-groups?limit=200');
  const operatorGroup = (groups.body?.groups || []).find((row) => row.name === 'bot-operator');
  check(step, 'bot-operator 预设存在', !!operatorGroup, 'present', groups.body?.groups?.length);
  const member = await admin.post(`/v1/permission-groups/${operatorGroup.groupId}/members`, {
    accountId: ctx.adminAccountId,
  });
  check(step, '管理员进入 bot-operator', member.status === 200, 200, member.status);
  const made = await prepareBot(admin, {
    roleName: 'E2E Attachment Operator',
    botId: 'e2e-attachment-operator',
    accountId: ctx.adminAccountId,
    instruction: '检查用户提交的附件。',
  });
  check(step, '创建附件会话 Bot', made.bot.status === 201, 201, made.bot.status);

  const png = await admin.upload(PNG, 'image/png');
  check(step, '上传 PNG 返回 fileId', png.status === 201 && !!png.body.fileId, '201 + fileId', `${png.status} ${png.body.fileId || ''}`);
  const pngId = png.body.fileId;
  const pdf = await admin.upload(PDF, 'application/pdf');
  check(step, '上传 PDF 返回 fileId', pdf.status === 201 && !!pdf.body.fileId, '201 + fileId', `${pdf.status} ${pdf.body.fileId || ''}`);
  const pdfId = pdf.body.fileId;

  const session = await admin.post('/v1/sessions', { botId: 'e2e-attachment-operator' });
  check(step, '创建 Bot 所有的会话', session.status === 201 && !!session.body.sessionId, '201 + sessionId', session.status);
  const sessionId = session.body.sessionId;

  const input = await admin.post(`/v1/sessions/${sessionId}/input`, {
    text: '请检查这两个附件：一张截图和一份说明。',
    requestId: `e2e-attachment-${Date.now()}`,
    delivery: 'queue',
    attachments: [
      { fileId: pngId, name: 'demo-shot.png', mime: 'image/png', size: png.body.size },
      { fileId: pdfId, name: '发布说明.pdf', mime: 'application/pdf', size: pdf.body.size },
    ],
  });
  check(step, '提交带附件的输入', input.status === 201 || input.status === 202, '201/202', input.status);
  const runStatus = await waitRun(admin, sessionId);
  check(step, '运行结束', runStatus === 'done' || runStatus === 'completed', 'done', runStatus);

  const events = await admin.get(`/v1/sessions/${sessionId}/events?after=0&limit=200`);
  const userEvent = (events.body?.events || []).find((event) => event.kind === 'user.message');
  check(step, '事件流出现 user.message', !!userEvent, 'user.message', userEvent?.kind || 'missing');
  const seq = userEvent?.seq;
  const payload = userEvent?.payload || {};
  check(step, '事件只存 fileId 与展示元数据', payload.attachments?.length === 2 && !JSON.stringify(payload).includes('base64'), '2 refs, no bytes', payload.attachments?.length);

  const part = await admin.get(`/v1/sessions/${sessionId}/attachments/${pngId}/part?seq=${seq}`);
  const partBody = part.body || {};
  check(step, 'canonical AttachmentPart 可读', part.status === 200, 200, part.status);
  check(step, 'part 字段完整', partBody.fileId === pngId && partBody.mime === 'image/png' && partBody.kind === 'image' && partBody.size > 0 && !!partBody.downloadUrl, 'fileId/mime/kind/size/downloadUrl', JSON.stringify({ fileId: partBody.fileId, mime: partBody.mime, kind: partBody.kind, size: partBody.size }));
  check(step, 'part 含 thumbStatus 与 delivery', ['none', 'pending', 'ready', 'failed'].includes(partBody.thumbStatus) && partBody.delivery?.native === false && !!partBody.delivery?.reason, 'thumbStatus + delivery(native=false, reason)', `${partBody.thumbStatus} / ${partBody.delivery?.native}`);
  const partKeys = Object.keys(partBody);
  check(step, 'part 不含字节/base64/CAS 路径', !partKeys.some((key) => ['content', 'data', 'bytes', 'base64', 'path'].includes(key)) && !JSON.stringify(partBody).includes('base64'), 'metadata only', partKeys.join(','));

  await waitThumbReady(admin, pngId);
  const meta = await admin.get(`/v1/files/${pngId}/meta`);
  check(step, '缩略图终态 ready', meta.body?.thumbStatus === 'ready', 'ready', meta.body?.thumbStatus);
  const thumb = await admin.get(`/v1/files/${pngId}/thumbnail`);
  check(step, '缩略图可读（owner）', thumb.status === 200 && thumb.buffer.length > 0, '200 + bytes', `${thumb.status} ${thumb.buffer.length}`);
  const file = await admin.get(`/v1/files/${pngId}`);
  check(step, '附件字节可下载（owner）', file.status === 200 && file.buffer.length > 1000, '200 + bytes', `${file.status} ${file.buffer.length}`);

  const requests = modelRequests();
  // A native image is inlined as a data URI, so the blob id is not in that
  // request; select by the wire shape instead of by the id.
  const withNativeImage = requests.filter((entry) => {
    const body = JSON.stringify(entry.body || {});
    return body.includes('"image_url"') && body.includes('data:image/png;base64');
  });
  const withPdf = requests.filter((entry) => JSON.stringify(entry.body || {}).includes(pdfId));
  check(step, '模型请求带 PNG 原生 image_url', withNativeImage.length > 0, 'image_url data URI', withNativeImage.length);
  const pdfWire = withPdf.map((entry) => JSON.stringify(entry.body)).join('\n');
  check(step, 'PDF 走文本兜底（含 blobId 与 MIME）', withPdf.length > 0 && pdfWire.includes(pdfId) && pdfWire.includes('application/pdf'), 'blobId + application/pdf', withPdf.length);
  check(step, 'PDF 兜底不带字节', !pdfWire.includes('data:application/pdf;base64') && !pdfWire.includes('"file_data"'), 'no file bytes', pdfWire.includes('file_data') ? 'file_data present' : 'no file_data');

  ctx.sessionId = sessionId;
  ctx.seq = seq;
  ctx.pngId = pngId;
  ctx.pdfId = pdfId;
  logLine(step, `session=${sessionId} seq=${seq} png=${pngId} pdf=${pdfId}`);
}

async function step2(step, ctx) {
  const { page, context } = await newPage(1440, 900);
  ctx.adminPage = page;
  ctx.adminContext = context;
  await loginUI(page, BASE, ADMIN_USER, ADMIN_PASSWORD);
  await openSession(page, BASE, ctx.sessionId);
  await waitTestId(page, `attachment-${ctx.pngId}`, 40000);
  await waitTestId(page, `attachment-${ctx.pdfId}`, 40000);
  const text = await bodyText(page);
  check(step, '默认行显示 PNG 名称与大小', text.includes('demo-shot.png') && /KB|MB/.test(text), 'name + size', text.includes('demo-shot.png'));
  check(step, '默认行显示 PDF 名称与 MIME', text.includes('发布说明.pdf') && text.includes('application/pdf'), 'name + mime', text.includes('发布说明.pdf'));
  check(step, '没有插件预览时只显示默认行', !text.includes('E2E 插件附件预览'), 'no plugin preview', text.includes('E2E 插件附件预览'));
  check(step, '投递形态如实标注未记录', text.includes('本次投递形态未记录'), 'delivery note', text.includes('本次投递形态未记录'));
  const thumbImg = page.locator(sel(`attachment-${ctx.pngId}`)).locator('img[src*="/thumbnail"]').first();
  const drawn = await thumbImg
    .evaluate((img) => ({ complete: img.complete, naturalWidth: img.naturalWidth, src: img.currentSrc }))
    .catch(() => ({ complete: false, naturalWidth: 0, src: '' }));
  check(step, '图片缩略图已绘制（naturalWidth > 0，无空白占位）', drawn.complete && drawn.naturalWidth > 0, 'complete + naturalWidth > 0', JSON.stringify(drawn));
  const size = await overflow(page);
  check(step, '1440 无横向溢出', size.doc <= size.inner, 'doc <= inner', JSON.stringify(size));
  await shot(page, step, 'default-attachment-row', { fullPage: true });
}

async function step3(step, ctx) {
  const admin = ctx.admin;
  const pluginDir = writePackage(PREVIEW_PLUGIN, { chunk: PREVIEW_CHUNK, title: 'E2E Attachment Preview' });
  const install = await approveAndRepeat(admin, 'install preview plugin', '/v1/plugins/install', { path: pluginDir });
  check(step, '安装返回 202 审批并获批', install.first.status === 202 && install.answer?.status === 200 && install.final.status === 201, '202 → 200 → 201', `${install.first.status}/${install.answer?.status}/${install.final.status}`);
  const prompt = String(install.first.body?.decision?.prompt || '');
  check(step, '审批文本展示附件认领', prompt.includes('keel.ui.message-part') && prompt.includes('attachment') && prompt.includes('image/png'), 'slot + part + mime', prompt.slice(0, 200));
  const enable = await approveAndRepeat(admin, 'enable preview plugin', `/v1/plugins/${PREVIEW_PLUGIN}/enable`, {});
  check(step, '启用返回 200', enable.final.status === 200, 200, enable.final.status);
  const inventory = await waitInventory(admin, PREVIEW_PLUGIN, 'enabled');
  const contribution = (inventory.contributes || []).find((entry) => entry.part === 'attachment');
  check(step, 'inventory 投影带 part/mime', contribution?.part === 'attachment' && (contribution?.mime || []).includes('image/png'), 'part=attachment, mime=image/png', JSON.stringify(contribution || null));

  await pageReload(ctx.adminPage);
  await openSession(ctx.adminPage, BASE, ctx.sessionId);
  await waitTestId(ctx.adminPage, `plugin-attachment-${ctx.pngId}`, 40000);
  await waitTestId(ctx.adminPage, 'e2e-attachment-preview', 40000);
  const text = await bodyText(ctx.adminPage);
  check(step, '插件预览在 PNG 行渲染', text.includes('E2E 插件附件预览') && text.includes('demo-shot.png · image/png'), 'preview text', text.includes('E2E 插件附件预览'));
  check(step, '默认事实与下载入口仍可见', text.includes('demo-shot.png') && text.includes('下载'), 'name + download', text.includes('下载'));
  const pdfRow = await ctx.adminPage.locator(sel(`attachment-${ctx.pdfId}`)).innerText();
  check(step, 'PDF 行不受 PNG 插件影响', !pdfRow.includes('E2E 插件附件预览'), 'no preview on PDF', pdfRow.includes('E2E 插件附件预览'));
  await shot(ctx.adminPage, step, 'plugin-preview');
}

async function pageReload(page) {
  await navigate(page, BASE);
  await waitTestId(page, 'books-shell', 40000);
}

async function step4(step, ctx) {
  const admin = ctx.admin;
  const disable = await admin.post(`/v1/plugins/${PREVIEW_PLUGIN}/disable`, {});
  check(step, '停用返回 200', disable.status === 200, 200, disable.status);
  const inventory = await waitInventory(admin, PREVIEW_PLUGIN, 'disabled');
  check(step, 'inventory 状态 disabled', inventory.state === 'disabled', 'disabled', inventory.state);
  await pageReload(ctx.adminPage);
  await openSession(ctx.adminPage, BASE, ctx.sessionId);
  await waitTestId(ctx.adminPage, `attachment-${ctx.pngId}`, 40000);
  const text = await bodyText(ctx.adminPage);
  check(step, '预览随停用撤销', !text.includes('E2E 插件附件预览'), 'no preview', text.includes('E2E 插件附件预览'));
  check(step, '默认行恢复', text.includes('demo-shot.png') && text.includes('下载'), 'name + download', text.includes('下载'));
  check(step, '没有残留插件失败面', (await ctx.adminPage.locator('[data-testid^="plugin-failure-"]').count()) === 0, '0 failures', await ctx.adminPage.locator('[data-testid^="plugin-failure-"]').count());
  await shot(ctx.adminPage, step, 'disabled-default-row');
  const uninstall = await admin.post(`/v1/plugins/${PREVIEW_PLUGIN}/uninstall`, {});
  check(step, '卸载返回 200', uninstall.status === 200, 200, uninstall.status);
}

async function step5(step, ctx) {
  const admin = ctx.admin;
  const brokenDir = writePackage(BROKEN_PLUGIN, { chunk: THROWING_CHUNK, title: 'E2E Broken Attachment Preview' });
  const install = await approveAndRepeat(admin, 'install broken plugin', '/v1/plugins/install', { path: brokenDir });
  check(step, '坏渲染器安装成功', install.final.status === 201, 201, install.final.status);
  const enable = await approveAndRepeat(admin, 'enable broken plugin', `/v1/plugins/${BROKEN_PLUGIN}/enable`, {});
  check(step, '坏渲染器启用成功', enable.final.status === 200, 200, enable.final.status);
  await pageReload(ctx.adminPage);
  await openSession(ctx.adminPage, BASE, ctx.sessionId);
  await waitTestId(ctx.adminPage, `attachment-${ctx.pngId}`, 40000);
  await ctx.adminPage.waitForSelector(sel('attachment-preview-failure'), { timeout: 40000 });
  const text = await bodyText(ctx.adminPage);
  const compact = text.replace(/\s+/g, '');
  check(step, '渲染抛错显示中文失败面', text.includes('附件预览失败') && compact.includes('E2E-ATTACHMENT-PREVIEW-EXPLODED'), '附件预览失败 + reason', compact.includes('E2E-ATTACHMENT-PREVIEW-EXPLODED'));
  check(step, '不把技术码当主文案', !text.includes('ERR_PLUGIN'), 'no ERR_PLUGIN', text.includes('ERR_PLUGIN'));
  check(step, '默认行保留', text.includes('demo-shot.png') && text.includes('下载'), 'name + download', text.includes('下载'));
  const pdfRow = await ctx.adminPage.locator(sel(`attachment-${ctx.pdfId}`)).innerText();
  check(step, '单个失败不影响其他附件', !pdfRow.includes('附件预览失败') && !pdfRow.includes('E2E-ATTACHMENT-PREVIEW-EXPLODED'), 'PDF row clean', pdfRow.slice(0, 80));
  await shot(ctx.adminPage, step, 'preview-failure-fallback');
  await admin.post(`/v1/plugins/${BROKEN_PLUGIN}/disable`, {});
  await admin.post(`/v1/plugins/${BROKEN_PLUGIN}/uninstall`, {});
}

async function step6(step, ctx) {
  const admin = ctx.admin;
  const group = await admin.post('/v1/permission-groups', { name: 'e2e-attachment-readers' });
  check(step, '创建 reader 权限组', group.status === 201 && !!group.body?.group?.groupId, '201 + groupId', group.status);
  const groupId = group.body.group.groupId;
  const grants = [
    ['session.read', 'own'],
    ['session.write', 'own'],
    ['app.read', 'tenant'],
    ['model.read', 'tenant'],
    ['plugin.read', 'tenant'],
    ['bot.read', 'own'],
    ['bot.manage', 'own'],
    ['bot.invoke', 'own'],
  ];
  for (const [key, scope] of grants) {
    const res = await admin.post(`/v1/permission-groups/${groupId}/grants`, { key, scope });
    check(step, `授予 ${key}@${scope}`, res.status === 200, 200, res.status);
  }
  const account = await admin.post('/v1/accounts', {
    username: 'e2e-reader',
    displayName: 'E2E Reader',
    kind: 'human',
    password: READER_PASSWORD,
  });
  check(step, '创建 reader 账号', account.status === 201 && !!account.body?.account?.accountId, '201 + accountId', account.status);
  const member = await admin.post(`/v1/permission-groups/${groupId}/members`, { accountId: account.body.account.accountId });
  check(step, 'reader 入组', member.status === 200, 200, member.status);

  const readerBot = await prepareBot(admin, {
    roleName: 'E2E Attachment Reader',
    botId: 'e2e-reader-bot',
    accountId: account.body.account.accountId,
    instruction: '查看用户提交的附件。',
  });
  check(step, '创建 reader 的 Bot', readerBot.bot.status === 201, 201, readerBot.bot.status);

  const reader = new Api('reader');
  const login = await reader.login('e2e-reader', READER_PASSWORD);
  check(step, 'reader 登录', login.status === 200 && login.body.authenticated === true, 200, login.status);
  const who = await reader.get('/v1/auth/whoami');
  const perms = who.body?.permissions || {};
  check(step, 'reader 有 session.read、无 file.read', perms['session.read'] === 'own' && !perms['file.read'], 'session.read=own, file.read absent', JSON.stringify({ session: perms['session.read'], file: perms['file.read'] }));

  const rSession = await reader.post('/v1/sessions', { botId: 'e2e-reader-bot' });
  check(step, 'reader 创建自己的 Bot 会话', rSession.status === 201, 201, rSession.status);
  const readerSession = rSession.body.sessionId;
  const sent = await admin.post(`/v1/sessions/${readerSession}/input`, {
    text: '这张截图给你看。',
    requestId: `e2e-reader-${Date.now()}`,
    delivery: 'queue',
    attachments: [{ fileId: ctx.pngId, name: 'demo-shot.png', mime: 'image/png', size: PNG.length }],
  });
  check(step, '管理员向 reader 会话提交附件', sent.status === 201 || sent.status === 202, '201/202', sent.status);
  await waitRun(admin, readerSession);
  const events = await admin.get(`/v1/sessions/${readerSession}/events?after=0&limit=100`);
  const seq = (events.body?.events || []).find((event) => event.kind === 'user.message')?.seq;
  check(step, 'reader 会话有 user.message', !!seq, 'seq', seq);

  const part = await reader.get(`/v1/sessions/${readerSession}/attachments/${ctx.pngId}/part?seq=${seq}`);
  check(step, 'reader 可读附件元数据（session.read）', part.status === 200 && part.body?.mime === 'image/png', 200, part.status);
  check(step, '元数据不含字节', !JSON.stringify(part.body).includes('base64') && !Object.keys(part.body || {}).some((key) => ['content', 'data', 'bytes'].includes(key)), 'metadata only', Object.keys(part.body || {}).join(','));
  const file = await reader.get(`/v1/files/${ctx.pngId}`);
  check(step, 'reader 下载被拒（缺 file.read）', file.status === 403, 403, file.status);
  check(step, '拒绝响应不含 PNG 字节', !file.buffer.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])) && !String(file.headers.get('content-type') || '').includes('image/png'), 'no bytes', file.headers.get('content-type'));
  const thumb = await reader.get(`/v1/files/${ctx.pngId}/thumbnail`);
  check(step, 'reader 缩略图被拒', thumb.status === 403, 403, thumb.status);

  const { page, context } = await newPage(1440, 900);
  ctx.readerPage = page;
  ctx.readerContext = context;
  await loginUI(page, BASE, 'e2e-reader', READER_PASSWORD);
  await openSession(page, BASE, readerSession);
  await waitTestId(page, `attachment-${ctx.pngId}`, 40000);
  await page.waitForSelector(sel('attachment-state-file-forbidden'), { timeout: 40000 });
  const text = await bodyText(page);
  check(step, 'UI 显示附件无权限态', text.includes('无权限查看附件内容'), 'no-permission alert', text.includes('无权限查看附件内容'));
  check(step, '无权限时仍可见名称与大小', text.includes('demo-shot.png') && /KB|MB/.test(text), 'name + size', text.includes('demo-shot.png'));
  check(step, '下载入口显示无权限且禁用', text.includes('无权限下载'), 'disabled download', text.includes('无权限下载'));
  check(step, '页面没有缩略图字节', (await page.locator(sel(`attachment-${ctx.pngId}`)).locator('img[src*="thumbnail"]').count()) === 0, 'no thumbnail img', await page.locator(sel(`attachment-${ctx.pngId}`)).locator('img[src*="thumbnail"]').count());
  await shot(page, step, 'reader-no-file-read');
  ctx.readerAccountId = account.body.account.accountId;
  ctx.readerSession = readerSession;
}

async function step7(step, ctx) {
  const admin = ctx.admin;
  const account = await admin.post('/v1/accounts', {
    username: 'e2e-outsider',
    displayName: 'E2E Outsider',
    kind: 'human',
    password: OUTSIDER_PASSWORD,
  });
  check(step, '创建无权限账号', account.status === 201, 201, account.status);
  const outsider = new Api('outsider');
  const login = await outsider.login('e2e-outsider', OUTSIDER_PASSWORD);
  check(step, 'outsider 登录', login.status === 200, 200, login.status);
  const list = await outsider.get('/v1/sessions');
  check(step, '无 session.read 不能列会话', list.status === 403, 403, list.status);
  check(step, '拒绝原因说明 session.read', String(list.body?.requiredKey || '') === 'session.read', 'session.read', list.body?.requiredKey || list.body?.code);
  const events = await outsider.get(`/v1/sessions/${ctx.sessionId}/events?after=0&limit=10`);
  check(step, '无 session.read 不能读事件', events.status === 403, 403, events.status);
  const part = await outsider.get(`/v1/sessions/${ctx.sessionId}/attachments/${ctx.pngId}/part?seq=${ctx.seq}`);
  check(step, '无 session.read 不能读附件投影', part.status === 403, 403, part.status);
  const file = await outsider.get(`/v1/files/${ctx.pngId}`);
  check(step, '无 session.read 不能读字节', file.status === 403, 403, file.status);
  check(step, '拒绝响应不含附件名/字节', !JSON.stringify(file.body).includes('demo-shot.png') && !file.buffer.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])), 'no name/bytes', file.status);

  const { page, context } = await newPage(1440, 900);
  ctx.outsiderPage = page;
  ctx.outsiderContext = context;
  await navigate(page, BASE);
  await waitTestId(page, 'login-gate', 30000);
  await page.fill(`${sel('login-username')} input`, 'e2e-outsider');
  await page.fill(`${sel('login-password')} input`, OUTSIDER_PASSWORD);
  await page.click(sel('login-submit'));
  await page.waitForTimeout(1500);
  await page.waitForTimeout(1500);
  const text = await bodyText(page);
  check(step, 'UI 不展示会话/附件内容', !text.includes('demo-shot.png') && !text.includes('发布说明.pdf'), 'no attachment text', text.includes('demo-shot.png'));
  const gateVisible = (await page.locator(sel('login-gate')).count()) > 0;
  check(step, 'UI 显示真实权限态而非登录失败', !gateVisible && /无权限读取会话|session\.read/.test(text), 'permission face, no login gate', text.slice(-200));
  await shot(page, step, 'outsider-no-session-read');
}

async function step8(step, ctx) {
  const tenantB = new Api('tenant-b', BASE_B);
  const login = await tenantB.login('tenant-b-admin', TENANT_B_PASSWORD);
  check(step, '租户 B 登录', login.status === 200 && login.body.tenantId === TENANT_B, `200 tenantId=${TENANT_B}`, `${login.status} ${login.body.tenantId}`);
  const own = await tenantB.upload(PNG, 'image/png');
  check(step, '租户 B 上传自己的文件', own.status === 201 && !!own.body.fileId, '201', own.status);
  const ownRead = await tenantB.get(`/v1/files/${own.body.fileId}`);
  check(step, '租户 B 读自己的文件', ownRead.status === 200 && ownRead.buffer.length > 1000, '200 + bytes', `${ownRead.status} ${ownRead.buffer.length}`);

  const fileA = await tenantB.get(`/v1/files/${ctx.pngId}`);
  check(step, '租户 B 读租户 A 附件 → 404', fileA.status === 404, 404, fileA.status);
  const thumbA = await tenantB.get(`/v1/files/${ctx.pngId}/thumbnail`);
  check(step, '租户 B 读租户 A 缩略图 → 404', thumbA.status === 404, 404, thumbA.status);
  const partA = await tenantB.get(`/v1/sessions/${ctx.sessionId}/attachments/${ctx.pngId}/part?seq=${ctx.seq}`);
  check(step, '租户 B 读租户 A 附件 part → 404', partA.status === 404, 404, partA.status);
  const forged = await tenantB.get(`/v1/files/${ctx.pngId}`, { headers: { 'X-Tenant-ID': TENANT } });
  check(step, '伪造 X-Tenant-ID 仍 404', forged.status === 404, 404, forged.status);
  const aToB = await ctx.admin.get(`/v1/files/${own.body.fileId}`);
  check(step, '租户 A 读租户 B 文件 → 404（对称）', aToB.status === 404, 404, aToB.status);

  check(step, '跨租户响应无字节', !fileA.buffer.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])) && !String(fileA.headers.get('content-type') || '').includes('image/png'), 'no bytes', fileA.headers.get('content-type'));

  const { page, context } = await newPage(1440, 900);
  ctx.tenantBPage = page;
  ctx.tenantBContext = context;
  await loginUI(page, BASE_B, 'tenant-b-admin', TENANT_B_PASSWORD);
  // Stay on tenant B's own server: the path names tenant A's blob, and the
  // server's principal (never the path) decides which tenant may read it.
  const response = await page.goto(`${BASE_B}/v1/files/${ctx.pngId}`, { waitUntil: 'commit' });
  const status = response?.status() ?? 0;
  check(step, '浏览器（租户 B 会话）打开租户 A 附件 → 404', status === 404, 404, status);
  const text = await bodyText(page);
  check(step, '浏览器 404 页不泄露字节', !/^\x89PNG/.test(text), 'error page', text.slice(0, 120));
  await shot(page, step, 'tenant-b-file-404');
  const thumbResponse = await page.goto(`${BASE_B}/v1/files/${ctx.pngId}/thumbnail`, { waitUntil: 'commit' });
  const thumbStatus = thumbResponse?.status() ?? 0;
  check(step, '浏览器租户 B 缩略图 → 404', thumbStatus === 404, 404, thumbStatus);
  const thumbText = await bodyText(page);
  check(step, '浏览器租户 B 缩略图 404 页不泄露字节', !thumbText.includes('base64') && !thumbText.includes('\x89PNG'), 'no bytes', thumbText.slice(0, 120));
  await shot(page, step, 'tenant-b-thumbnail-404');
}

async function step9(step, ctx) {
  const state = await ctx.adminContext.storageState();
  // The review's P2: the viewport walk must show the plugin preview in place,
  // not only the fallback after the uninstall. Reinstall it for this step and
  // withdraw it afterwards.
  const pluginDir = writePackage(PREVIEW_PLUGIN, { chunk: PREVIEW_CHUNK, title: 'E2E Attachment Preview' });
  const install = await approveAndRepeat(ctx.admin, 'install preview plugin (layout)', '/v1/plugins/install', { path: pluginDir });
  check(step, '布局走查前重新安装预览插件', install.final.status === 201, 201, install.final.status);
  const enable = await approveAndRepeat(ctx.admin, 'enable preview plugin (layout)', `/v1/plugins/${PREVIEW_PLUGIN}/enable`, {});
  check(step, '布局走查前重新启用预览插件', enable.final.status === 200, 200, enable.final.status);
  const inventory = await waitInventory(ctx.admin, PREVIEW_PLUGIN, 'enabled');
  const contribution = (inventory.contributes || []).find((entry) => entry.part === 'attachment');
  check(step, '布局走查前 inventory 带附件贡献', contribution?.part === 'attachment', 'part=attachment', JSON.stringify(contribution || null));
  const viewports = [
    { width: 390, height: 844 },
    { width: 768, height: 1024 },
    { width: 1024, height: 800 },
    { width: 1440, height: 900 },
  ];
  for (const viewport of viewports) {
    const { page, context } = await newPage(viewport.width, viewport.height, state);
    await openSession(page, BASE, ctx.sessionId);
    await waitTestId(page, `attachment-${ctx.pngId}`, 40000);
    await waitTestId(page, `attachment-${ctx.pdfId}`, 40000);
    await waitTestId(page, `plugin-attachment-${ctx.pngId}`, 40000);
    try {
      await waitTestId(page, 'e2e-attachment-preview', 40000);
    } catch (error) {
      logLine(step, `${viewport.width}: preview did not render; body=${(await bodyText(page)).slice(0, 600)}`);
      throw error;
    }
    const text = await bodyText(page);
    check(step, `${viewport.width}: 插件预览在场`, text.includes('E2E 插件附件预览'), 'preview text', text.includes('E2E 插件附件预览'));
    const size = await overflow(page);
    check(step, `${viewport.width}: 无横向溢出`, size.doc <= size.inner, 'doc <= inner', JSON.stringify(size));
    const box = await page.locator(sel(`attachment-${ctx.pngId}`)).boundingBox();
    check(
      step,
      `${viewport.width}: 附件行在视口内`,
      !!box && box.x >= -0.5 && box.x + box.width <= viewport.width + 0.5,
      'inside viewport',
      JSON.stringify(box),
    );
    const image = await page
      .locator(sel(`attachment-${ctx.pngId}`))
      .locator('img[src*="/thumbnail"]')
      .first()
      .evaluate((img) => ({ complete: img.complete, naturalWidth: img.naturalWidth }))
      .catch(() => ({ complete: false, naturalWidth: 0 }));
    check(step, `${viewport.width}: 缩略图已绘制`, image.complete && image.naturalWidth > 0, 'complete + naturalWidth > 0', JSON.stringify(image));
    await shot(page, step, `layout-${viewport.width}`);
    await context.close();
  }
  const disabled = await ctx.admin.post(`/v1/plugins/${PREVIEW_PLUGIN}/disable`, {});
  check(step, '布局走查后停用预览插件', disabled.status === 200, 200, disabled.status);
  const removed = await ctx.admin.post(`/v1/plugins/${PREVIEW_PLUGIN}/uninstall`, {});
  check(step, '布局走查后卸载预览插件', removed.status === 200, 200, removed.status);
}

// ---- main ----------------------------------------------------------------------

async function main() {
  const admin = new Api('admin');
  const login = await admin.login(ADMIN_USER, ADMIN_PASSWORD);
  if (login.status !== 200) throw new Error(`admin login failed (${login.status})`);
  const ctx = { admin, adminAccountId: admin.accountId || login.body?.account?.id };

  await runStep(1, '上传 → 输入 → canonical AttachmentPart 与模型投递', '真实 HTTP 上传 PNG/PDF，提交带附件输入，读取 part 与模型请求', '原生 image_url + PDF 文本兜底，元数据无字节', (step) => step1(step, ctx));
  await runStep(2, '浏览器默认附件行（无插件）', '登录 → 打开会话，检查默认行、缩略图与下载入口', '名称/大小/MIME/缩略图/下载可见，无插件预览', (step) => step2(step, ctx));
  await runStep(3, '安装预览插件并渲染', '安装 + durabile 审批 + 启用，重载页面打开会话', '审批展示认领；chunk 渲染且默认事实仍在', (step) => step3(step, ctx));
  await runStep(4, '停用插件 → 回退默认行', '停用 + 重载，检查预览撤销与默认行恢复', '无预览、无失败面，默认行可用', (step) => step4(step, ctx));
  await runStep(5, '预览抛错 → 回退且隔离', '安装抛错插件，重载检查失败提示与默认行', '可解释失败；PDF 行不受影响', (step) => step5(step, ctx));
  await runStep(6, 'reader 无 file.read → 无权限态', '建组/账号/会话，reader 打开会话并请求附件', '元数据可见；缩略图/下载 403；UI 无权限态', (step) => step6(step, ctx));
  await runStep(7, 'outsider 无 session.read → 不泄露', '登录无权限账号，请求会话/附件/字节', '403；UI 不展示会话或附件内容', (step) => step7(step, ctx));
  await runStep(8, '跨租户：租户 B 读租户 A 附件', '租户 B 登录，读 A 的文件/缩略图/part，含伪造租户头', '全部 404；对称检查 A 读 B 也 404', (step) => step8(step, ctx));
  await runStep(9, '390/768/1024/1440 布局走查', '四个视口打开附件会话，测量溢出与行框', '无横向溢出，附件行在视口内', (step) => step9(step, ctx));

  const rows = coverageRows();
  const coverage = {
    baseline: COVERAGE_BASE,
    total: rows.length,
    covered: rows.filter((row) => row.status.startsWith('COVERED')).length,
    exempt: rows.filter((row) => row.status.startsWith('EXEMPT')).length,
    uncovered: rows.filter((row) => !row.status.startsWith('COVERED') && !row.status.startsWith('EXEMPT')).length,
    rows,
  };
  const totalChecks = steps.reduce((sum, step) => sum + step.checks.length, 0);
  const passedChecks = steps.reduce((sum, step) => sum + step.checks.filter((c) => c.ok).length, 0);
  const failedSteps = steps.filter((step) => step.result !== 'PASS');
  if (coverage.uncovered > 0) {
    addFinding(
      'coverage-uncovered',
      'P0',
      `改动文件覆盖表 UNCOVERED=${coverage.uncovered}`,
      '产品/测试文件没有映射到证据。EXEMPT 只允许文档/证据和 E2E 运行器；其余必须 COVERED。',
      coverage.rows
        .filter((row) => row.status === 'UNCOVERED')
        .map((row) => row.file)
        .join(', '),
    );
  }
  const report = {
    title: 'keel-plugin-attachment · 真实浏览器 E2E',
    change: 'keel-plugin-attachment（tasks 3.6 / 5.4 / 5.5）',
    date: new Date().toISOString().slice(0, 10),
    frozenBase: { repo: WORKTREE, commit: HEAD_COMMIT },
    conclusion:
      failedSteps.length === 0 && coverage.uncovered === 0
        ? `${steps.length}/${steps.length} 步通过，${passedChecks}/${totalChecks} 条断言通过；P0/P1 = 0；UNCOVERED=0。`
        : `${steps.length - failedSteps.length}/${steps.length} 步通过；${passedChecks}/${totalChecks} 条断言通过；UNCOVERED=${coverage.uncovered}。`,
    assertions: { total: totalChecks, passed: passedChecks },
    steps,
    findings,
    coverage,
  };
  fs.writeFileSync(path.join(EVIDENCE, 'steps.json'), JSON.stringify(report, null, 2) + '\n');
  console.log(`[e2e] report data written: ${path.join(EVIDENCE, 'steps.json')} (${passedChecks}/${totalChecks} checks, UNCOVERED=${coverage.uncovered})`);
  if (failedSteps.length > 0 || coverage.uncovered > 0) process.exitCode = 1;
}

(async () => {
  await launchBrowser();
  try {
    await main();
  } finally {
    if (browser) await browser.close().catch(() => {});
  }
})().catch((error) => {
  console.error(`[e2e] fatal: ${error?.stack || error}`);
  process.exit(1);
});
