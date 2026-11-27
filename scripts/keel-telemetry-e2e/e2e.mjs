#!/usr/bin/env node
// Live E2E driver for change keel-plugin-telemetry (task 7.4).
//
// This lane talks HTTP/JSON only — no browser. It runs against the real stack
// started by run.sh: a throwaway PostgreSQL cluster, `keel serve --auth
// local-only`, a deterministic OpenAI-compatible provider and an independent
// OTLP/HTTP JSON collector.
//
// What it proves:
//   0. the stack is real (PostgreSQL version, local super admin, plugin ships
//      installed-but-not-enabled, telemetry hub disabled);
//   1. a real conversation with the plugin off sends zero collector requests;
//   2. a real admin API write stores the tenant telemetry document and secret;
//      the disabled plugin still sends nothing;
//   3. enabling `keel.telemetry` goes through the durable two-phase approval;
//      the question renders the sanitized config, never the header value;
//   4. the first telemetry-enabled conversation produces a real
//      `keel.agent.run` span with tenant/session, a metric batch (no identity
//      labels) and a bounded `turn.finished` log; the configured
//      Authorization header arrives but the secret never enters evidence;
//   5. a sampleRatio=0 rewrite (applied by disable→enable, the documented
//      re-read point) stops trace requests while metrics/logs still arrive;
//   6. disabling the plugin stops all new collector requests;
//   7. evidence hygiene: no raw secret in any collected artifact.
//
// Every assertion is PASS/FAIL; the driver exits non-zero when a step fails.
// run.sh owns process/database cleanup and the report; this driver writes
// steps.json only.
import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

const BASE = (process.env.E2E_BASE || '').replace(/\/$/, '');
const EVIDENCE = process.env.E2E_EVIDENCE || '';
const TENANT = process.env.E2E_TENANT || '';
const COLLECTOR_LOG = process.env.E2E_COLLECTOR_LOG || '';
const MODEL_LOG = process.env.E2E_MODEL_LOG || '';
const SECRET_FILE = process.env.E2E_OTLP_TOKEN_FILE || '';
const ADMIN_KEY_FILE = process.env.E2E_ADMIN_KEY_FILE || '';
const DSN_FILE = process.env.E2E_DSN_FILE || '';
const COLLECTOR_PORT = Number(process.env.E2E_COLLECTOR_PORT || 0);
const HEAD_COMMIT = process.env.E2E_HEAD_COMMIT || '';
const SERVICE_NAME = 'keel-telemetry-e2e';

if (!BASE || !EVIDENCE || !TENANT || !COLLECTOR_LOG || !MODEL_LOG || !SECRET_FILE || !ADMIN_KEY_FILE) {
  console.error('missing E2E_BASE/E2E_EVIDENCE/E2E_TENANT/E2E_COLLECTOR_LOG/E2E_MODEL_LOG/E2E_OTLP_TOKEN_FILE/E2E_ADMIN_KEY_FILE');
  process.exit(2);
}

fs.mkdirSync(EVIDENCE, { recursive: true });
const STEPS_FILE = path.join(EVIDENCE, 'steps.json');
const OTLP_TOKEN = fs.readFileSync(SECRET_FILE, 'utf8').trim();
const ADMIN_KEY = fs.readFileSync(ADMIN_KEY_FILE, 'utf8').trim();
const TOKEN_SHA256 = crypto.createHash('sha256').update(OTLP_TOKEN).digest('hex');

const nowIso = () => new Date().toISOString();
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const brief = (value, max = 500) => {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  if (text === undefined) return '';
  return text.length > max ? text.slice(0, max) + '…' : text;
};

// ---- evidence -----------------------------------------------------------------

const report = {
  lane: 'keel-telemetry-e2e',
  task: 'openspec/changes/keel-plugin-telemetry tasks 7.4',
  commit: HEAD_COMMIT,
  base: BASE,
  startedAt: nowIso(),
  finishedAt: null,
  environment: {
    tenant: TENANT,
    collector: `http://127.0.0.1:${COLLECTOR_PORT}`,
    serviceName: SERVICE_NAME,
    signals: 'traces,metrics,logs',
    database: DSN_FILE ? 'throwaway PostgreSQL cluster (keel db start)' : 'unknown',
    model: 'e2e-model via scripts/keel-telemetry-e2e/fake-provider.mjs',
    lane: 'HTTP/JSON only (no browser)',
  },
  steps: [],
  collector: null,
  model: null,
  pass: false,
  failures: 0,
  error: '',
};

let current = null;

function beginStep(id, title) {
  current = {
    id,
    title,
    startedAt: nowIso(),
    status: 'running',
    checks: [],
    details: {},
  };
  report.steps.push(current);
  console.log(`\n[step] ${id}: ${title}`);
  return current;
}

function check(label, ok, actual, expected) {
  const entry = {
    label,
    ok: Boolean(ok),
    actual: actual === undefined || actual === null ? null : brief(actual),
    expected: expected === undefined ? null : expected,
  };
  if (current) current.checks.push(entry);
  if (!ok) report.failures += 1;
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${label}${ok ? '' : ` (actual: ${brief(actual, 240)} want: ${brief(expected, 160)})`}`);
  return Boolean(ok);
}

function record(details) {
  if (current) Object.assign(current.details, details);
}

function saveData(name, value) {
  fs.writeFileSync(path.join(EVIDENCE, name), JSON.stringify(value, null, 2) + '\n');
}

function finishStep(status) {
  if (!current) return;
  const failed = current.checks.filter((entry) => !entry.ok);
  current.status = status || (failed.length === 0 && current.checks.length > 0 ? 'passed' : 'failed');
  current.finishedAt = nowIso();
  current.durationMs = Date.now() - new Date(current.startedAt).getTime();
  console.log(`[step] ${current.id}: ${current.status.toUpperCase()} (${current.checks.length - failed.length}/${current.checks.length})`);
  current = null;
}

function fail(message) {
  throw new Error(message);
}

// ---- HTTP ---------------------------------------------------------------------

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
    signal: AbortSignal.timeout(60000),
  });
  const text = await response.text();
  let parsed = text;
  try {
    parsed = JSON.parse(text);
  } catch {
    /* non-JSON bodies stay text */
  }
  return { status: response.status, body: parsed, text };
}

async function adminApi(method, endpoint, body) {
  const headers = { Authorization: `Bearer ${ADMIN_KEY}` };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const response = await fetch(`${BASE}${endpoint}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(30000),
  });
  const text = await response.text();
  let parsed = text;
  try {
    parsed = JSON.parse(text);
  } catch {
    /* non-JSON bodies stay text */
  }
  return { status: response.status, body: parsed, text };
}

// ---- collector/model journals --------------------------------------------------

function readJsonl(file) {
  if (!fs.existsSync(file)) return [];
  const lines = fs.readFileSync(file, 'utf8').split('\n');
  const out = [];
  for (const line of lines) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    try {
      out.push(JSON.parse(trimmed));
    } catch {
      /* ignore a torn final line while the producer appends */
    }
  }
  return out;
}

const collectorRequests = () => readJsonl(COLLECTOR_LOG);
const modelRequests = () => readJsonl(MODEL_LOG);
const requestsSince = (records, iso) => records.filter((entry) => entry.receivedAt >= iso);
const countPaths = (records) => records.reduce((sum, entry) => {
  sum[entry.path] = (sum[entry.path] || 0) + 1;
  return sum;
}, {});

async function waitFor(predicate, timeoutMs, label, intervalMs = 250) {
  const deadline = Date.now() + timeoutMs;
  let last = null;
  while (Date.now() < deadline) {
    last = await predicate();
    if (last) return last;
    await sleep(intervalMs);
  }
  throw new Error(`timed out waiting for ${label}`);
}

async function waitCollector(predicate, timeoutMs, label) {
  return waitFor(() => {
    const records = collectorRequests();
    const found = predicate(records);
    return found || null;
  }, timeoutMs, label);
}

/** Waits until no new collector request arrives for `quietMs` (bounded). */
async function waitCollectorQuiet(quietMs = 6000, maxMs = 30000) {
  const deadline = Date.now() + maxMs;
  let lastCount = collectorRequests().length;
  let quietSince = Date.now();
  while (Date.now() < deadline) {
    await sleep(250);
    const count = collectorRequests().length;
    if (count !== lastCount) {
      lastCount = count;
      quietSince = Date.now();
    } else if (Date.now() - quietSince >= quietMs) {
      return count;
    }
  }
  return collectorRequests().length;
}

// ---- conversation ---------------------------------------------------------------

async function events(sessionId) {
  const response = await api('GET', `/v1/sessions/${sessionId}/events`);
  if (response.status !== 200) throw new Error(`events: HTTP ${response.status}`);
  return Array.isArray(response.body?.events) ? response.body.events : [];
}

async function waitEvents(sessionId, predicate, timeoutMs = 90000) {
  const deadline = Date.now() + timeoutMs;
  let last = [];
  while (Date.now() < deadline) {
    last = await events(sessionId);
    if (predicate(last)) return last;
    await sleep(300);
  }
  throw new Error(`events predicate timed out; kinds: ${brief(last.map((event) => event.kind), 300)}`);
}

async function createDeployment() {
  const accountId = sessionDocument?.account?.id || '';
  if (!accountId) throw new Error('local admin account id is missing from /v1/auth/session');
  const role = await api('POST', '/v1/roles', { name: 'E2E Telemetry Role', instruction: 'E2E telemetry live lane' });
  if (role.status !== 201) throw new Error(`create role: ${role.status} ${brief(role.body)}`);
  const roleId = role.body.role.roleId;
  const profile = await api('POST', '/v1/profiles', { accountId, roleId });
  if (profile.status !== 201) throw new Error(`create profile: ${profile.status} ${brief(profile.body)}`);
  const profileId = profile.body.profile.profileId;
  const bot = await api('POST', '/v1/bots', {
    name: 'E2E Telemetry Bot',
    profileId,
    modelKind: 'text',
    defaultModel: { providerId: 'e2e', modelId: 'e2e-model' },
    harness: { tools: [], context: { instruction: 'E2E telemetry bot', permissions: [] } },
  });
  if (bot.status !== 201) throw new Error(`create bot: ${bot.status} ${brief(bot.body)}`);
  const botId = bot.body.bot.botId;
  const session = await api('POST', '/v1/sessions', { botId });
  if (session.status !== 201) throw new Error(`create session: ${session.status} ${brief(session.body)}`);
  return { accountId, roleId, profileId, botId, sessionId: session.body.sessionId };
}

/** Sends one user input and waits for that turn's run.finished(done). */
async function runTurn(sessionId, text, requestId) {
  const before = (await events(sessionId)).filter((event) => event.kind === 'run.finished').length;
  const startedAt = nowIso();
  const input = await api('POST', `/v1/sessions/${sessionId}/input`, { text, requestId });
  if (input.status !== 201) throw new Error(`input: HTTP ${input.status} ${brief(input.body)}`);
  const all = await waitEvents(sessionId, (list) => {
    const finished = list.filter((event) => event.kind === 'run.finished');
    return finished.length >= before + 1 && finished[finished.length - 1]?.payload?.status === 'done';
  });
  const finished = all.filter((event) => event.kind === 'run.finished');
  return {
    startedAt,
    finishedAt: nowIso(),
    finishedCount: finished.length,
    lastStatus: finished[finished.length - 1]?.payload?.status,
  };
}

// ---- plugins / telemetry ---------------------------------------------------------

function pluginView(inventory, id) {
  return (inventory?.plugins || []).find((plugin) => plugin.id === id) || null;
}

async function inventory() {
  const response = await api('GET', '/v1/plugins');
  if (response.status !== 200) throw new Error(`plugins: HTTP ${response.status}`);
  return response.body;
}

async function telemetryStatus() {
  const response = await api('GET', '/v1/telemetry/status');
  return response;
}

function telemetryConfigDocument(sampleRatio) {
  return {
    telemetry: {
      adapter: 'otlp',
      endpoint: `http://127.0.0.1:${COLLECTOR_PORT}`,
      headers: { Authorization: 'secret:OTLP_TOKEN' },
      serviceName: SERVICE_NAME,
      signals: ['traces', 'metrics', 'logs'],
      sampleRatio,
      logMinLevel: 'info',
    },
  };
}

async function putTelemetryConfig(sampleRatio) {
  const document = telemetryConfigDocument(sampleRatio);
  const response = await adminApi('PUT', `/v1/tenants/${TENANT}/config`, document);
  if (response.status !== 200) throw new Error(`tenant config: HTTP ${response.status} ${brief(response.body)}`);
  return { document, response };
}

async function enableTelemetry(requestId, expectedSampleRatio) {
  const first = await api('POST', '/v1/plugins/keel.telemetry/enable', { requestId });
  if (first.status !== 202) throw new Error(`enable open: HTTP ${first.status} ${brief(first.body)}`);
  const decision = first.body?.decision;
  if (!decision?.decisionId) throw new Error('enable open: 202 without a decisionId');
  const prompt = String(decision.prompt || '');
  const answer = await api('POST', `/v1/plugin-approvals/${decision.decisionId}`, { optionId: 'allow' });
  const repeat = await api('POST', '/v1/plugins/keel.telemetry/enable', { requestId, decisionId: decision.decisionId });
  if (repeat.status !== 200) throw new Error(`enable repeat: HTTP ${repeat.status} ${brief(repeat.body)}`);
  return {
    firstStatus: first.status,
    decision,
    prompt,
    expectedSampleRatio,
    answerStatus: answer.status,
    answer: answer.body,
    repeatStatus: repeat.status,
    repeat: repeat.body,
    state: repeat.body?.plugin?.state,
  };
}

async function disableTelemetry() {
  const response = await api('POST', '/v1/plugins/keel.telemetry/disable', {});
  return response;
}

// ---- psql -----------------------------------------------------------------------

const DSN = DSN_FILE ? fs.readFileSync(DSN_FILE, 'utf8').trim() : '';

function psql(sql) {
  if (!DSN) throw new Error('E2E_DSN_FILE is not set');
  return execFileSync('psql', [DSN, '-Atc', sql], { encoding: 'utf8', timeout: 20000 }).trim();
}

// ---- collector summaries --------------------------------------------------------

function summarizeCollector(records) {
  const paths = {};
  const spanNames = new Set();
  const metricNames = new Set();
  const logEvents = new Set();
  const auth = new Set();
  for (const entry of records) {
    paths[entry.path] = (paths[entry.path] || 0) + 1;
    if (entry.headers?.authorization?.sha256) auth.add(entry.headers.authorization.sha256);
    const summary = entry.summary || {};
    for (const span of summary.spans || []) spanNames.add(span.name);
    for (const metric of summary.metrics || []) metricNames.add(metric.name);
    for (const record of summary.records || []) logEvents.add(record.body);
  }
  return {
    requests: records.length,
    paths,
    spanNames: [...spanNames].sort(),
    metricNames: [...metricNames].sort(),
    logEvents: [...logEvents].sort(),
    authorizationFingerprints: [...auth],
  };
}

function findRunSpan(records, sinceIso) {
  for (const entry of requestsSince(records, sinceIso)) {
    if (entry.path !== '/v1/traces') continue;
    for (const span of entry.summary?.spans || []) {
      if (span.name === 'keel.agent.run') return { entry, span };
    }
  }
  return null;
}

function findMetric(records, sinceIso, name) {
  for (const entry of requestsSince(records, sinceIso)) {
    if (entry.path !== '/v1/metrics') continue;
    for (const metric of entry.summary?.metrics || []) {
      if (metric.name === name) return { entry, metric };
    }
  }
  return null;
}

function findLog(records, sinceIso, event) {
  for (const entry of requestsSince(records, sinceIso)) {
    if (entry.path !== '/v1/logs') continue;
    for (const record of entry.summary?.records || []) {
      if (record.body === event) return { entry, record };
    }
  }
  return null;
}

// ---- steps ----------------------------------------------------------------------

async function main() {
  // ---- 0. stack -------------------------------------------------------------
  beginStep('stack', '真实栈：PostgreSQL + keel serve + 独立 collector，插件默认关闭');
  const healthResponse = await fetch(`${BASE}/v1/health`);
  const health = healthResponse.ok ? await healthResponse.json() : null;
  check('keel serve /v1/health 200', healthResponse.status === 200 && Boolean(health), healthResponse.status, 200);
  const auth = await readAuthSession();
  check('local-only 超管会话', auth.authenticated === true && auth.localAdmin === true && auth.account?.isSuperAdmin === true, {
    authenticated: auth.authenticated,
    localAdmin: auth.localAdmin,
    isSuperAdmin: auth.account?.isSuperAdmin,
  }, true);
  check('会话租户 = E2E 租户', auth.tenantId === TENANT, auth.tenantId, TENANT);
  const version = psql('select version()');
  check('真实 PostgreSQL 可达', /PostgreSQL/i.test(version), version.split(',')[0], 'PostgreSQL');
  const bootInventory = await inventory();
  const telemetryPlugin = pluginView(bootInventory, 'keel.telemetry');
  check('keel.telemetry 随二进制提供', Boolean(telemetryPlugin), telemetryPlugin?.id || null, 'keel.telemetry');
  check('keel.telemetry 默认未启用', telemetryPlugin?.state === 'installed' || telemetryPlugin?.state === 'disabled', telemetryPlugin?.state, 'installed|disabled');
  check('keel.telemetry 是 bundled 插件', telemetryPlugin?.bundled === true, telemetryPlugin?.bundled, true);
  check('清单 enabled 不含 keel.telemetry', !(bootInventory.enabled || []).includes('keel.telemetry'), bootInventory.enabled, 'no keel.telemetry');
  const bootStatus = await telemetryStatus();
  check('GET /v1/telemetry/status 200', bootStatus.status === 200, bootStatus.status, 200);
  check('telemetry hub 默认 disabled', bootStatus.body?.enabled === false && bootStatus.body?.state !== 'active', {
    enabled: bootStatus.body?.enabled,
    state: bootStatus.body?.state,
  }, { enabled: false });
  check('collector 起步 0 请求', collectorRequests().length === 0, collectorRequests().length, 0);
  record({ health, account: auth.account?.username, postgres: version.split(',')[0], bootPluginState: telemetryPlugin?.state, bootTelemetry: bootStatus.body });
  finishStep();

  // ---- 1. default off, real run --------------------------------------------
  beginStep('default-off', '默认关闭：未启用插件时一轮真实对话，collector 收到 0 请求');
  const created = await createDeployment();
  record({ deployment: created });
  check('真实会话已创建', Boolean(created.sessionId), created.sessionId, 'sessionId');
  const beforeOff = collectorRequests().length;
  const modelBeforeOff = modelRequests().length;
  const offTurn = await runTurn(created.sessionId, 'E2E telemetry default-off probe: 请回答这一轮。', `e2e-off-${Date.now()}`);
  check('默认关闭轮次 run.finished=done', offTurn.lastStatus === 'done', offTurn.lastStatus, 'done');
  const modelAfterOff = modelRequests().length;
  check('脚本化模型确实被调用', modelAfterOff > modelBeforeOff, `${modelBeforeOff} -> ${modelAfterOff}`, 'increases');
  await sleep(7000); // exporter FlushInterval is 5s
  const afterOff = collectorRequests().length;
  check('关闭状态一轮对话后 collector 仍为 0 请求', afterOff === beforeOff, afterOff, beforeOff);
  check('关闭状态无 trace/metric/log 请求', Object.keys(countPaths(collectorRequests())).length === 0, countPaths(collectorRequests()), {});
  record({ turn: offTurn, collectorRequests: afterOff });
  finishStep();

  // ---- 2. admin config write, still disabled --------------------------------
  beginStep('configured-disabled', '真实 admin API 写入 tenant telemetry 文档与 secret，插件未启用仍零请求');
  const secretWrite = await api('PUT', '/v1/settings/secrets/OTLP_TOKEN', { value: OTLP_TOKEN });
  check('secret 写入 200', secretWrite.status === 200, secretWrite.status, 200);
  // The masked preview is itself a partial value; evidence only records that a
  // masked, non-empty preview was returned.
  check('secret 响应只回 masked preview', typeof secretWrite.body?.preview === 'string' && secretWrite.body.preview.length > 0 && secretWrite.body.preview !== OTLP_TOKEN, 'masked non-empty preview', 'masked non-empty preview');
  check('secret 响应不含明文值', !JSON.stringify(secretWrite.body).includes(OTLP_TOKEN), 'no raw token', 'no raw token');
  const write = await putTelemetryConfig(1);
  check('PUT tenant config 200', write.response.status === 200, write.response.status, 200);
  check('config revision >= 1', Number(write.response.body?.revision) >= 1, write.response.body?.revision, '>= 1');
  const read = await adminApi('GET', `/v1/tenants/${TENANT}/config`);
  check('GET tenant config 200', read.status === 200, read.status, 200);
  const stored = read.body?.config || {};
  check('stored adapter=otlp', stored?.telemetry?.adapter === 'otlp', stored?.telemetry?.adapter, 'otlp');
  check('stored endpoint 指向 collector', stored?.telemetry?.endpoint === `http://127.0.0.1:${COLLECTOR_PORT}`, stored?.telemetry?.endpoint, `http://127.0.0.1:${COLLECTOR_PORT}`);
  check('stored header 是 secret 引用', stored?.telemetry?.headers?.Authorization === 'secret:OTLP_TOKEN', stored?.telemetry?.headers?.Authorization, 'secret:OTLP_TOKEN');
  check('stored sampleRatio=1', Number(stored?.telemetry?.sampleRatio) === 1, stored?.telemetry?.sampleRatio, 1);
  check('config 读回不含明文 secret', !read.text.includes(OTLP_TOKEN), 'no raw token', 'no raw token');
  const dbConfig = psql(`select config->'telemetry'->>'adapter', config->'telemetry'->>'endpoint', config_revision from tenants where tenant_id='${TENANT}'`);
  check('PostgreSQL tenants.config 已落盘', dbConfig.startsWith('otlp|') && dbConfig.includes(`127.0.0.1:${COLLECTOR_PORT}`), dbConfig, 'otlp|…endpoint…|revision');
  const statusBeforeEnable = await telemetryStatus();
  check('写入配置但未启用：status enabled=false', statusBeforeEnable.body?.enabled === false, {
    enabled: statusBeforeEnable.body?.enabled,
    state: statusBeforeEnable.body?.state,
  }, { enabled: false });
  const beforeConfiguredRun = collectorRequests().length;
  const configuredTurn = await runTurn(created.sessionId, 'E2E telemetry configured-but-disabled probe: 再答一轮。', `e2e-disabled-${Date.now()}`);
  check('配置已写但插件未启用：轮次完成', configuredTurn.lastStatus === 'done', configuredTurn.lastStatus, 'done');
  await sleep(7000);
  check('配置已写但插件未启用：collector 仍 0 新请求', collectorRequests().length === beforeConfiguredRun, collectorRequests().length, beforeConfiguredRun);
  record({ config: stored, storedRow: dbConfig, revision: read.body?.revision, turn: configuredTurn });
  finishStep();

  // ---- 3. two-phase enable --------------------------------------------------
  beginStep('enable-approval', '两阶段审批启用 keel.telemetry：202 → allow → 200 enabled');
  const enable = await enableTelemetry('e2e-enable-initial', 1);
  check('首次 enable 返回 202 审批问题', enable.firstStatus === 202, enable.firstStatus, 202);
  check('审批类型 plugin.enable', enable.decision.kind === 'plugin.enable', enable.decision.kind, 'plugin.enable');
  check('审批问题为 open', enable.decision.status === 'open', enable.decision.status, 'open');
  check('审批问题渲染 exporter=otlp', enable.prompt.includes('exporter=otlp'), brief(enable.prompt, 300), 'exporter=otlp');
  check('审批问题渲染 endpoint host', enable.prompt.includes(`endpointHost=127.0.0.1:${COLLECTOR_PORT}`), brief(enable.prompt, 300), `endpointHost=127.0.0.1:${COLLECTOR_PORT}`);
  check('审批问题渲染 sampleRatio=1', enable.prompt.includes('sampleRatio=1'), brief(enable.prompt, 300), 'sampleRatio=1');
  check('审批问题只渲染 header 名', enable.prompt.includes('headers=Authorization'), brief(enable.prompt, 300), 'headers=Authorization');
  check('审批问题不含明文 secret', !enable.prompt.includes(OTLP_TOKEN), 'no raw token', 'no raw token');
  check('审批问题不含 secret 引用值', !enable.prompt.includes('secret:OTLP_TOKEN'), 'no secret ref', 'no secret ref');
  check('allow 回答 200', enable.answerStatus === 200, enable.answerStatus, 200);
  check('审批已回答 allow', enable.answer?.decision?.status === 'answered' && enable.answer?.decision?.answer?.optionId === 'allow', enable.answer?.decision?.status, 'answered/allow');
  check('重复 enable 200', enable.repeatStatus === 200, enable.repeatStatus, 200);
  check('插件状态 enabled', enable.state === 'enabled', enable.state, 'enabled');
  const enabledInventory = await inventory();
  check('清单显示 keel.telemetry enabled', pluginView(enabledInventory, 'keel.telemetry')?.state === 'enabled', pluginView(enabledInventory, 'keel.telemetry')?.state, 'enabled');
  const enabledStatus = await telemetryStatus();
  check('telemetry status active+enabled', enabledStatus.body?.enabled === true && enabledStatus.body?.state === 'active', {
    enabled: enabledStatus.body?.enabled,
    state: enabledStatus.body?.state,
  }, { enabled: true, state: 'active' });
  const statusSignals = [...(enabledStatus.body?.signals || [])].sort();
  check('status signals = traces/metrics/logs', JSON.stringify(statusSignals) === JSON.stringify(['logs', 'metrics', 'traces']), statusSignals, ['logs', 'metrics', 'traces']);
  check('status endpointHost 来自配置', enabledStatus.body?.endpointHost === `127.0.0.1:${COLLECTOR_PORT}`, enabledStatus.body?.endpointHost, `127.0.0.1:${COLLECTOR_PORT}`);
  check('status sampleRatio=1', Number(enabledStatus.body?.sampleRatio) === 1, enabledStatus.body?.sampleRatio, 1);
  check('status headerNames=[Authorization]', JSON.stringify(enabledStatus.body?.headerNames) === JSON.stringify(['Authorization']), enabledStatus.body?.headerNames, ['Authorization']);
  check('status 不含明文 secret', !JSON.stringify(enabledStatus.body).includes(OTLP_TOKEN), 'no raw token', 'no raw token');
  const audit = await api('GET', '/v1/plugins/audit?plugin=keel.telemetry&limit=50');
  const auditText = JSON.stringify(audit.body || {});
  check('环境审计可见 enable 成功', audit.status === 200 && (audit.body?.entries || []).some((entry) => entry.action === 'enable' && entry.result === 'ok'), (audit.body?.entries || []).map((entry) => `${entry.action}:${entry.result}`), 'enable:ok');
  check('审计不含明文 secret', !auditText.includes(OTLP_TOKEN), 'no raw token', 'no raw token');
  record({
    decisionId: enable.decision.decisionId,
    prompt: enable.prompt,
    status: enabledStatus.body,
    auditActions: (audit.body?.entries || []).map((entry) => ({ action: entry.action, result: entry.result, actor: entry.actor })),
    auditDetailSample: (audit.body?.entries || []).find((entry) => entry.action === 'enable')?.detail || null,
  });
  finishStep();

  // ---- 4. enabled run: span + metric + log ---------------------------------
  beginStep('export-enabled', '启用后一轮真实对话：collector 收到 keel.agent.run span、metric 与 turn.finished log');
  const beforeEnabledRun = collectorRequests().length;
  check('enable 本身不产生 collector 请求', beforeEnabledRun === 0, beforeEnabledRun, 0);
  const run = await runTurn(created.sessionId, 'E2E telemetry enabled probe: 请回答并触发一次导出。', `e2e-enabled-${Date.now()}`);
  check('启用轮次 run.finished=done', run.lastStatus === 'done', run.lastStatus, 'done');
  const runSpan = await waitCollector(
    (records) => findRunSpan(records, run.startedAt),
    30000,
    'keel.agent.run trace span',
  );
  const metric = await waitCollector(
    (records) => findMetric(records, run.startedAt, 'keel.agent.runs'),
    30000,
    'keel.agent.runs metric',
  );
  const logRecord = await waitCollector(
    (records) => findLog(records, run.startedAt, 'turn.finished'),
    30000,
    'turn.finished log record',
  );
  const afterRunRequests = collectorRequests();
  const newRequests = requestsSince(afterRunRequests, run.startedAt);
  const newPaths = countPaths(newRequests);
  check('收到 /v1/traces 请求', (newPaths['/v1/traces'] || 0) >= 1, newPaths, 'traces>=1');
  check('收到 /v1/metrics 请求', (newPaths['/v1/metrics'] || 0) >= 1, newPaths, 'metrics>=1');
  check('收到 /v1/logs 请求', (newPaths['/v1/logs'] || 0) >= 1, newPaths, 'logs>=1');
  check('span 名 keel.agent.run', runSpan?.span?.name === 'keel.agent.run', runSpan?.span?.name, 'keel.agent.run');
  check('span tenant = E2E 租户', runSpan?.span?.attributes?.tenant === TENANT, runSpan?.span?.attributes?.tenant, TENANT);
  check('span session = 真实 sessionId', runSpan?.span?.attributes?.session === created.sessionId, runSpan?.span?.attributes?.session, created.sessionId);
  check('span model 来自本轮解析', runSpan?.span?.attributes?.model === 'e2e-model', runSpan?.span?.attributes?.model, 'e2e-model');
  check('span provider 非空', Boolean(runSpan?.span?.attributes?.provider), runSpan?.span?.attributes?.provider, 'non-empty');
  check('span status OK', String(runSpan?.span?.status) === '1' || runSpan?.span?.status === 1, runSpan?.span?.status, 1);
  check('metric keel.agent.runs 存在', metric?.metric?.name === 'keel.agent.runs', metric?.metric?.name, 'keel.agent.runs');
  const metricPoint = metric?.metric?.points?.[0] || {};
  check('metric outcome=ok', metricPoint?.attributes?.outcome === 'ok', metricPoint?.attributes, { outcome: 'ok' });
  check('metric model 标签存在', metricPoint?.attributes?.model === 'e2e-model', metricPoint?.attributes?.model, 'e2e-model');
  const metricIdentityKeys = (metric?.metric?.points || []).flatMap((point) => Object.keys(point.attributes || {})).filter((key) => key === 'tenant' || key === 'session');
  check('metric 不带 tenant/session 身份标签', metricIdentityKeys.length === 0, [...new Set(metricIdentityKeys)], []);
  check('log event turn.finished', logRecord?.record?.body === 'turn.finished', logRecord?.record?.body, 'turn.finished');
  check('log 带 tenant/session', logRecord?.record?.attributes?.tenant === TENANT && logRecord?.record?.attributes?.session === created.sessionId, {
    tenant: logRecord?.record?.attributes?.tenant,
    session: logRecord?.record?.attributes?.session,
  }, { tenant: TENANT, session: created.sessionId });
  check('log status=ok', logRecord?.record?.attributes?.status === 'ok', logRecord?.record?.attributes?.status, 'ok');
  const resource = runSpan?.entry?.summary?.resource || {};
  check('resource service.name 来自配置', resource['service.name'] === SERVICE_NAME, resource['service.name'], SERVICE_NAME);
  check('resource tenant.id = 租户', resource['tenant.id'] === TENANT, resource['tenant.id'], TENANT);
  const authHeaders = newRequests.filter((entry) => entry.headers?.authorization?.sha256 === TOKEN_SHA256);
  check('每个请求都带配置的 Authorization', authHeaders.length === newRequests.length && newRequests.length > 0, `${authHeaders.length}/${newRequests.length}`, 'all');
  check('Authorization 指纹来自 secret 值', newRequests.every((entry) => entry.headers?.authorization?.sha256 === TOKEN_SHA256), newRequests.map((entry) => entry.headers?.authorization?.sha256?.slice(0, 12)), TOKEN_SHA256.slice(0, 12));
  check('collector 记录中无明文 secret', !JSON.stringify(afterRunRequests).includes(OTLP_TOKEN), 'no raw token', 'no raw token');
  check('请求 Content-Type 为 JSON', newRequests.every((entry) => String(entry.contentType).includes('application/json')), newRequests.map((entry) => entry.contentType), 'application/json');
  check('只请求 OTLP 信号路径', newRequests.every((entry) => ['/v1/traces', '/v1/metrics', '/v1/logs'].includes(entry.path)), newPaths, ['/v1/traces', '/v1/metrics', '/v1/logs']);
  record({
    turn: run,
    runSpan: { name: runSpan?.span?.name, attributes: runSpan?.span?.attributes, status: runSpan?.span?.status },
    metric: { name: metric?.metric?.name, points: metric?.metric?.points?.slice(0, 2) },
    log: logRecord?.record,
    paths: newPaths,
    authorizationFingerprint: TOKEN_SHA256,
    resource,
    childSpans: (runSpan?.entry?.summary?.spans || []).map((span) => span.name),
  });
  finishStep();

  // ---- 5. sampleRatio 0 ----------------------------------------------------
  beginStep('sampling-zero', '采样率改为 0：重读配置后无 trace 请求，metric/log 仍到达');
  const zeroWrite = await putTelemetryConfig(0);
  check('PUT sampleRatio=0 200', zeroWrite.response.status === 200, zeroWrite.response.status, 200);
  const disabledForReload = await disableTelemetry();
  check('改采样前停用插件 200', disabledForReload.status === 200 && disabledForReload.body?.plugin?.state === 'disabled', {
    status: disabledForReload.status,
    state: disabledForReload.body?.plugin?.state,
  }, { status: 200, state: 'disabled' });
  await waitCollectorQuiet(6000);
  const enableZero = await enableTelemetry('e2e-enable-sample-0', 0);
  check('重新启用 202→allow→200', enableZero.firstStatus === 202 && enableZero.answerStatus === 200 && enableZero.repeatStatus === 200, {
    first: enableZero.firstStatus,
    answer: enableZero.answerStatus,
    repeat: enableZero.repeatStatus,
  }, '202/200/200');
  check('审批问题渲染 sampleRatio=0', enableZero.prompt.includes('sampleRatio=0'), brief(enableZero.prompt, 300), 'sampleRatio=0');
  check('重新启用后插件 enabled', enableZero.state === 'enabled', enableZero.state, 'enabled');
  const zeroStatus = await telemetryStatus();
  check('status sampleRatio=0', Number(zeroStatus.body?.sampleRatio) === 0, zeroStatus.body?.sampleRatio, 0);
  check('status 仍 active', zeroStatus.body?.state === 'active' && zeroStatus.body?.enabled === true, {
    state: zeroStatus.body?.state,
    enabled: zeroStatus.body?.enabled,
  }, { state: 'active', enabled: true });
  await waitCollectorQuiet(6000);
  const zeroBaseline = collectorRequests().length;
  const zeroRun = await runTurn(created.sessionId, 'E2E telemetry sample-zero probe: 这一轮不应产生 trace。', `e2e-sample0-${Date.now()}`);
  check('采样 0 轮次 run.finished=done', zeroRun.lastStatus === 'done', zeroRun.lastStatus, 'done');
  const zeroMetric = await waitCollector(
    (records) => findMetric(records, zeroRun.startedAt, 'keel.agent.runs'),
    30000,
    'metric at sampleRatio=0',
  );
  const zeroLog = await waitCollector(
    (records) => findLog(records, zeroRun.startedAt, 'turn.finished'),
    30000,
    'log at sampleRatio=0',
  );
  check('采样 0：metric 仍到达', Boolean(zeroMetric), zeroMetric?.metric?.name, 'keel.agent.runs');
  check('采样 0：log 仍到达', Boolean(zeroLog), zeroLog?.record?.body, 'turn.finished');
  await sleep(7000); // a full exporter flush interval after the sampled run
  const zeroAfter = collectorRequests();
  const zeroNew = requestsSince(zeroAfter, zeroRun.startedAt);
  const zeroNewPaths = countPaths(zeroNew);
  const zeroTraces = zeroNew.filter((entry) => entry.path === '/v1/traces');
  check('采样 0：没有新的 trace 请求', zeroTraces.length === 0, zeroNewPaths, { traces: 0 });
  check('采样 0：没有 keel.agent.run span', findRunSpan(zeroAfter, zeroRun.startedAt) === null, findRunSpan(zeroAfter, zeroRun.startedAt)?.span?.name || null, null);
  check('采样 0：本轮只有 metric/log 请求', (zeroNewPaths['/v1/metrics'] || 0) >= 1 && (zeroNewPaths['/v1/logs'] || 0) >= 1, zeroNewPaths, 'metrics+logs');
  record({ turn: zeroRun, baseline: zeroBaseline, newPaths: zeroNewPaths, metric: zeroMetric?.metric?.name, log: zeroLog?.record?.body, status: zeroStatus.body });
  finishStep();

  // ---- 6. disable ----------------------------------------------------------
  beginStep('disable', '停用插件：再发一轮真实对话，collector 不再收到新请求');
  const disabled = await disableTelemetry();
  check('disable 200', disabled.status === 200, disabled.status, 200);
  check('插件状态 disabled', disabled.body?.plugin?.state === 'disabled', disabled.body?.plugin?.state, 'disabled');
  const disabledStatus = await telemetryStatus();
  check('status enabled=false', disabledStatus.body?.enabled === false, {
    enabled: disabledStatus.body?.enabled,
    state: disabledStatus.body?.state,
  }, { enabled: false });
  const disabledInventory = await inventory();
  check('清单 keel.telemetry disabled', pluginView(disabledInventory, 'keel.telemetry')?.state === 'disabled', pluginView(disabledInventory, 'keel.telemetry')?.state, 'disabled');
  await waitCollectorQuiet(7000);
  const disableBaseline = collectorRequests().length;
  const modelBeforeDisabledRun = modelRequests().length;
  const disabledRun = await runTurn(created.sessionId, 'E2E telemetry disabled probe: 停用后这一轮不应导出。', `e2e-postdisable-${Date.now()}`);
  check('停用后轮次 run.finished=done', disabledRun.lastStatus === 'done', disabledRun.lastStatus, 'done');
  check('脚本化模型仍被调用', modelRequests().length > modelBeforeDisabledRun, `${modelBeforeDisabledRun} -> ${modelRequests().length}`, 'increases');
  await sleep(8000);
  const disableAfter = collectorRequests();
  check('停用后无任何新 collector 请求', disableAfter.length === disableBaseline, disableAfter.length, disableBaseline);
  check('停用后无新 span/metric/log', requestsSince(disableAfter, disabledRun.startedAt).length === 0, countPaths(requestsSince(disableAfter, disabledRun.startedAt)), {});
  record({ turn: disabledRun, baseline: disableBaseline, status: disabledStatus.body, inventoryState: pluginView(disabledInventory, 'keel.telemetry')?.state });
  finishStep();

  // ---- 7. evidence hygiene -------------------------------------------------
  beginStep('evidence-hygiene', '证据卫生：secret 值不进入 collector/模型/serve 日志');
  const collectorText = fs.readFileSync(COLLECTOR_LOG, 'utf8');
  const modelText = fs.existsSync(MODEL_LOG) ? fs.readFileSync(MODEL_LOG, 'utf8') : '';
  const serveLog = path.join(EVIDENCE, 'serve.log');
  const serveText = fs.existsSync(serveLog) ? fs.readFileSync(serveLog, 'utf8') : '';
  check('collector-requests.jsonl 不含明文 secret', !collectorText.includes(OTLP_TOKEN), 'no raw token', 'no raw token');
  check('model-requests.jsonl 不含明文 secret', !modelText.includes(OTLP_TOKEN), 'no raw token', 'no raw token');
  check('serve.log 不含明文 secret', !serveText.includes(OTLP_TOKEN), 'no raw token', 'no raw token');
  check('collector 只记录 Authorization 指纹', !/"authorization"\s*:\s*"/i.test(collectorText), 'fingerprint only', 'fingerprint only');
  const finalRequests = collectorRequests();
  check('interaction 全程 secret 未泄漏', !JSON.stringify(report.steps).includes(OTLP_TOKEN), 'no raw token', 'no raw token');
  record({
    collectorSummary: summarizeCollector(finalRequests),
    modelSummary: {
      requests: modelRequests().length,
      models: [...new Set(modelRequests().map((entry) => entry.model).filter(Boolean))],
      streams: [...new Set(modelRequests().map((entry) => entry.stream))],
    },
    collectorBytes: collectorText.length,
    serveLogBytes: serveText.length,
  });
  finishStep();
}

// ---- entry ----------------------------------------------------------------------

(async () => {
  let error = null;
  try {
    await main();
  } catch (caught) {
    error = String(caught?.stack || caught?.message || caught);
    console.error(`\n[e2e] FAILED: ${error}`);
    if (current) {
      check('driver exception', false, String(caught?.message || caught), 'no exception');
      finishStep('failed');
    }
    report.failures += 1;
  }
  report.error = error || '';
  report.finishedAt = nowIso();
  report.pass = !error && report.steps.length > 0 && report.steps.every((step) => step.status === 'passed');
  report.collector = summarizeCollector(collectorRequests());
  report.model = {
    requests: modelRequests().length,
    models: [...new Set(modelRequests().map((entry) => entry.model).filter(Boolean))],
  };
  saveData('steps.json', report);
  saveData('collector-summary.json', report.collector);
  console.log(`\n[e2e] ${report.pass ? 'PASS' : 'FAIL'} steps=${report.steps.length} failedChecks=${report.failures} collectorRequests=${report.collector.requests}`);
  process.exit(report.pass ? 0 : 1);
})();
