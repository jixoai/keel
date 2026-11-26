#!/usr/bin/env node
// Live GitHub super-agent E2E driver (change keel-collab-superagent, 5.2).
//
// It drives an already-booted real `keel serve` whose Source/Channel is the
// configured GitHub repository. The lane is:
//
//   real issue -> Source poll -> Ticket + Assignment -> worker session
//   -> model `ask` tool -> Attention + Channel question comment
//   -> model `report waiting` -> blocked ticket
//   -> product API answer -> Channel answer comment -> worker turn
//   -> model `report ready` -> done + Channel completion comment
//   -> live marker dedupe probe (Go test) leaves the ticket count unchanged
//
// Every step writes assertions, log evidence and a PNG; report.mjs renders
// report.json into report.html. Nothing here chooses a ticket id: the ticket
// is found by the unique issue marker, exactly like an operator would.
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';

const BASE = (process.env.LIVE_BASE || '').replace(/\/$/, '');
const RUN = process.env.LIVE_RUN_DIR || '';
const REPO = process.env.LIVE_REPO || '';
const ISSUE = process.env.LIVE_ISSUE || '';
const ISSUE_URL = process.env.LIVE_ISSUE_URL || '';
const MARKER = process.env.LIVE_MARKER || '';
const PROVIDER_LOG = process.env.LIVE_PROVIDER_LOG || '';
const DSN_FILE = process.env.LIVE_DSN_FILE || '';
const GIT_COMMIT = process.env.LIVE_GIT_COMMIT || '';
const GIT_DIRTY = process.env.LIVE_GIT_DIRTY || '';
const BIN_SHA256 = process.env.LIVE_BIN_SHA256 || '';
const CHROME = process.env.LIVE_CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const PLAYWRIGHT = process.env.LIVE_PLAYWRIGHT || '';
const HEADLESS = process.env.LIVE_HEADLESS !== 'false';

if (!BASE || !RUN || !REPO || !ISSUE || !MARKER) {
  console.error('missing LIVE_BASE/LIVE_RUN_DIR/LIVE_REPO/LIVE_ISSUE/LIVE_MARKER');
  process.exit(2);
}
const SHOTS = path.join(RUN, 'shots');
const DSN = DSN_FILE && fs.existsSync(DSN_FILE) ? fs.readFileSync(DSN_FILE, 'utf8').trim() : '';
fs.mkdirSync(SHOTS, { recursive: true });
fs.mkdirSync(path.join(RUN, 'data'), { recursive: true });

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const brief = (value, max = 400) => {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  return text && text.length > max ? text.slice(0, max) + '…' : text;
};

const report = {
  overline: 'FDE2E · KEEL · SUPER-AGENT 5.2',
  title: '真实 GitHub 链路 E2E：Issue → 工单 → 员工 → 评论 → Attention → 完成',
  subtitle: `真实 \`keel serve\` + 临时 PostgreSQL + \`KEEL_SUPERAGENT_SOURCE=github\`，Source/Channel 是真实 GitHub 仓库 ${REPO}；员工模型走脚本化 OpenAI provider，通过 4.5 的 \`ask\`/\`report\` 工具驱动工单。`,
  meta: [
    `repo：${REPO}`,
    `issue：#${ISSUE}`,
    `marker：${MARKER}`,
    'PG 临时库',
    'local-only 产品鉴权',
    `commit：${GIT_COMMIT.slice(0, 12) || 'unknown'}（tracked dirty=${GIT_DIRTY || '?'}）`,
    `bin sha256：${BIN_SHA256.slice(0, 16) || 'unknown'}…`,
  ],
  conclusion: '',
  stats: [],
  env: [
    ['部署', '真实 keel serve（worktree 构建）+ 临时 PostgreSQL'],
    ['来源/通道', 'GitHub issue + issue comments（RFC3339 游标）'],
    ['两个 Bot', 'orchestrator / worker（worker 带 report、ask、task.write）'],
    ['允许 actor', 'Gaubee（gh api user）'],
    ['员工模型', 'loopback 脚本化 OpenAI provider（输出工具调用）'],
    ['轮询', '2s；槽位 1 + 人工保留 1'],
    ['token', '仅进程环境；必须是 user token（PAT / gh auth token），未写入文件/日志/提交'],
    ['代码版本', `git ${GIT_COMMIT || 'unknown'}（tracked dirty=${GIT_DIRTY || '?'}）；bin sha256 ${BIN_SHA256 || 'unknown'}`],
  ],
  steps: [],
  assertions: [],
  gaps: [],
  evidence: [],
  commands: [],
};

let current = null;
let failures = 0;
let counter = 0;

function beginStep(id, name, action, expect) {
  counter += 1;
  current = { n: String(counter).padStart(2, '0'), id, name, action, expect, result: 'PASS', log: '', shots: [], links: [] };
  report.steps.push(current);
  return current;
}

function check(id, group, name, ok, detail, opts = {}) {
  const result = ok ? 'PASS' : 'FAIL';
  if (!ok) {
    failures += 1;
    if (current) current.result = 'FAIL';
  }
  report.assertions.push({
    id,
    group,
    name,
    result,
    detail: typeof detail === 'string' ? detail : brief(detail),
    step: opts.step ?? (current ? current.n : null),
  });
  return ok;
}

function note(line) {
  if (current) current.log += (current.log ? '\n' : '') + line;
}

function finishStep() {
  if (current) current = null;
}

// ---- product API -----------------------------------------------------------

let cookie = '';
let csrf = '';
let authSession = null;

async function readAuth() {
  const response = await fetch(`${BASE}/v1/auth/session`);
  if (!response.ok) throw new Error(`auth session: HTTP ${response.status}`);
  authSession = await response.json();
  cookie = (response.headers.getSetCookie ? response.headers.getSetCookie() : [response.headers.get('set-cookie')])
    .map((entry) => String(entry).split(';')[0])
    .filter((part) => part.includes('='))
    .join('; ');
  csrf = authSession.csrfToken || '';
  return authSession;
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
    /* keep text */
  }
  return { status: response.status, body: parsed };
}

async function board() {
  const response = await api('GET', '/v1/superagent/tickets');
  return response.status === 200 ? response.body : { tickets: [], counts: {} };
}

async function inbox() {
  const response = await api('GET', '/v1/superagent/attention');
  return response.status === 200 ? response.body : { items: [] };
}

async function waitFor(label, fn, timeoutMs = 90000, intervalMs = 1000) {
  const deadline = Date.now() + timeoutMs;
  let last;
  while (Date.now() < deadline) {
    last = await fn();
    if (last) return last;
    await sleep(intervalMs);
  }
  throw new Error(`timeout waiting for ${label}`);
}

// ---- GitHub ----------------------------------------------------------------

function ghJSON(args) {
  let lastError = null;
  for (let attempt = 0; attempt < 5; attempt += 1) {
    try {
      const out = execFileSync('gh', args, { encoding: 'utf8', maxBuffer: 8 << 20 });
      return JSON.parse(out || 'null');
    } catch (error) {
      lastError = error;
      execFileSync('sleep', ['1']);
    }
  }
  throw lastError || new Error('gh api failed');
}

function issueComments() {
  return ghJSON(['api', `repos/${REPO}/issues/${ISSUE}/comments?per_page=100`, '--jq', '.']);
}

function commentBodies() {
  return issueComments().map((comment) => String(comment.body || ''));
}

function providerEntries() {
  if (!PROVIDER_LOG || !fs.existsSync(PROVIDER_LOG)) return [];
  return fs
    .readFileSync(PROVIDER_LOG, 'utf8')
    .split('\n')
    .filter(Boolean)
    .map((line) => {
      try {
        return JSON.parse(line);
      } catch {
        return { raw: line };
      }
    });
}

// assignmentIdOf reads the report tool result ("assignment":{...}) the model
// saw. The worker tools never take an assignment id as input, so an equal pair
// across the waiting and ready reports proves one Ticket/Assignment/session
// handled both.
function assignmentIdOf(entry) {
  const text = entry?.last?.text;
  if (typeof text !== 'string') return '';
  try {
    return JSON.parse(text)?.assignment?.id || '';
  } catch {
    return '';
  }
}

// ---- browser ---------------------------------------------------------------

let browser = null;
let page = null;

async function openBrowser() {
  let playwright;
  try {
    playwright = await import(PLAYWRIGHT);
  } catch (error) {
    note(`browser unavailable: ${error.message}`);
    return false;
  }
  browser = await playwright.chromium.launch({ executablePath: CHROME, headless: HEADLESS });
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, locale: 'zh-CN' });
  page = await context.newPage();
  page.setDefaultTimeout(20000);
  return true;
}

async function shot(name, caption, target = page) {
  if (!target) return;
  const file = path.join(SHOTS, name);
  await target.screenshot({ path: file, fullPage: false });
  if (current) current.shots.push({ src: `shots/${name}`, caption });
  return `shots/${name}`;
}

async function gotoProduct(url, waitText) {
  await page.goto(`${BASE}${url}`, { waitUntil: 'domcontentloaded' });
  if (waitText) await page.waitForFunction((text) => document.body.innerText.includes(text), waitText, { timeout: 30000 });
  await sleep(1200);
}

async function gotoIssue() {
  await page.goto(ISSUE_URL, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction((marker) => document.body.innerText.includes(marker), MARKER, { timeout: 30000 });
  await sleep(1500);
}

// GitHub serves anonymous issue pages from a short cache, so a reload can
// briefly predate the comment the API already returns. Retry until the text is
// on the page; the screenshot must show the comment it claims to show.
async function gotoIssueWaiting(expected) {
  for (let attempt = 0; attempt < 8; attempt += 1) {
    await gotoIssue();
    try {
      await page.waitForFunction((text) => document.body.innerText.includes(text), expected, { timeout: 4000 });
      await sleep(500);
      return;
    } catch {
      await sleep(3000);
    }
  }
  throw new Error(`issue page never showed: ${expected}`);
}

// capture runs one navigation+screenshot and only records a note on failure:
// a slow GitHub render must not turn a product assertion red.
async function capture(fn, name, caption) {
  if (!page) return;
  try {
    await fn();
    await shot(name, caption);
  } catch (error) {
    note(`screenshot ${name} failed: ${error.message}`);
  }
}

// ---- lane ------------------------------------------------------------------

async function main() {
  const startedAt = new Date().toISOString();

  // 0. deployment facts
  beginStep('boot', '部署就绪与外部凭据', '核对真实 serve 健康、super-agent 队列启用、测试仓库/issue 可读。', '健康 200；日志出现 super-agent queue enabled（source=github）；issue 存在且公开可截图。');
  const health = await api('GET', '/v1/health');
  check('B1', 'boot', 'keel serve 健康', health.status === 200 && health.body?.ok === true, `HTTP ${health.status}`);
  const auth = await readAuth();
  check('B2', 'boot', 'local-only 超管会话', auth.authenticated === true && auth.localAdmin === true, {
    account: auth.account?.username,
    tenantId: auth.tenantId,
  });
  const issue = ghJSON(['api', `repos/${REPO}/issues/${ISSUE}`, '--jq', '.']);
  check('B3', 'boot', '真实 issue 存在且公开可读', Boolean(issue?.number) && issue?.state === 'open', {
    number: issue?.number,
    state: issue?.state,
    url: issue?.html_url,
  });
  note(`serve=${BASE}\nissue=${ISSUE_URL}\naccount=${auth.account?.username} tenant=${auth.tenantId}`);
  note(`commit=${GIT_COMMIT} trackedDirty=${GIT_DIRTY} binSha256=${BIN_SHA256}`);
  finishStep();

  // 1. Source -> Ticket -> Assignment
  beginStep('ticket', 'Issue → Ticket/Assignment', `在真实 issue #${ISSUE} 上等待 Source 轮询接单。`, '看板出现唯一工单，线程 ID 指向该 issue，且派出 1 个执行 Assignment。');
  // The queue creates the Ticket and its first Assignment in one locked pass,
  // but the board read can interleave with the dispatch; wait for the settled
  // shape the assertions describe instead of the transient accept row.
  const ticket = await waitFor('ticket', async () => {
    const body = await board();
    const found = (body.tickets || []).find((item) => JSON.stringify(item).includes(MARKER));
    return found && found.assignmentCount >= 1 ? found : null;
  }, 120000);
  const threadId = ticket.thread?.id || '';
  check('T1', 'ticket', '工单由真实 issue 接单', threadId === `${REPO}#${ISSUE}`, threadId);
  check('T2', 'ticket', '工单带账本 Mandate（requester=Gaubee）', ticket.mandate?.requester?.login === 'Gaubee' && ticket.mandate?.strength === 'comment', {
    requester: ticket.mandate?.requester?.login,
    strength: ticket.mandate?.strength,
  });
  check('T3', 'ticket', '创建了一个执行 Assignment', ticket.assignmentCount >= 1, `assignmentCount=${ticket.assignmentCount}`);
  const accepted = await waitFor('acceptance comment', async () => {
    const bodies = commentBodies();
    return bodies.find((body) => body.includes(`Ticket ${ticket.id.slice(0, 8)} accepted`)) || null;
  }, 45000);
  check('T4', 'ticket', 'Channel 在 issue 上写回接单评论', Boolean(accepted), accepted ? brief(accepted, 200) : 'missing');
  check('T5', 'ticket', '评论带 keel-message marker', Boolean(accepted && accepted.includes('<!-- keel-message:')), accepted ? 'marker present' : 'no marker');
  note(`ticketId=${ticket.id}\nassignmentCount=${ticket.assignmentCount}\nstatus=${ticket.status}\nthread=${threadId}`);
  current.links.push({ label: 'GitHub issue', href: ISSUE_URL });
  finishStep();

  // 2. worker ask + report waiting
  beginStep('ask', '员工提问（模型 ask 工具）', '脚本化员工模型调用 `ask`，随后 `report waiting`。', '真实 Attention 打开、工单转 blocked、issue 出现问题评论。');
  const attention = await waitFor('attention', async () => {
    const body = await inbox();
    return (body.items || []).find((item) => item.workItemId === ticket.id);
  }, 120000);
  check('A1', 'ask', '模型 ask 工具创建了真实 Attention', attention.status === 'open', {
    attentionId: attention.id,
    requestId: attention.requestId,
  });
  check('A2', 'ask', 'Attention 路由到 issue 线程', attention.channel === `${REPO}#${ISSUE}` && attention.route === 'channel', {
    route: attention.route,
    channel: attention.channel,
  });
  const questionComment = await waitFor('question comment', async () => {
    const bodies = commentBodies();
    return bodies.find((body) => body.includes(attention.prompt) && body.includes('<!-- keel-message:')) || null;
  }, 45000);
  check('A3', 'ask', '问题评论真实写回 GitHub', Boolean(questionComment && questionComment.includes('<!-- keel-message:')), questionComment ? brief(questionComment, 220) : 'missing');
  const blockedTicket = await waitFor('blocked ticket', async () => {
    const body = await board();
    const found = (body.tickets || []).find((item) => item.id === ticket.id);
    return found?.status === 'blocked' ? found : null;
  }, 60000);
  check('A4', 'ask', 'report waiting 后工单为 blocked', blockedTicket.status === 'blocked', blockedTicket.status);
  const askEntry = providerEntries().find((entry) => entry.choice?.tool === 'ask');
  const waitEntry = providerEntries().find((entry) => entry.choice?.tool === 'report' && entry.choice?.args?.state === 'waiting');
  check('A5', 'ask', 'provider 记录了 ask 与 report waiting', Boolean(askEntry && waitEntry), {
    ask: Boolean(askEntry),
    reportWaiting: Boolean(waitEntry),
  });
  note(`attentionId=${attention.id}\nprompt=${attention.prompt}\nstatus=${blockedTicket.status}`);
  await openBrowser();
  await capture(() => gotoIssueWaiting(attention.prompt), '01-github-question.png', 'GitHub issue：接单 + 员工问题评论');
  await capture(() => gotoProduct('/superagent', '工单看板'), '02-product-board-attention.png', '产品 /superagent 看板：待人工收件箱显示员工问题');
  finishStep();

  // 3. marker dedupe and self-echo while the ticket is parked on the human
  beginStep(
    'dedupe',
    'marker 去重与自身回声（真实 GitHub，待人工窗口）',
    '在同一 messageId 上连续 Send 两次；再写入去重探针评论（owner + 行尾 marker），让 Source 至少轮询三个周期；同时跑复审回归用例（正文编辑、仅标题编辑、owner+行尾 marker、跨端点游标取 max、PG 重启快照）。',
    '重试不产生第二条评论；探针评论不被读回：assignment 数、模型 turn 数不增，工单仍 blocked；回归用例全 PASS。',
  );
  const beforeBoard = await board();
  const beforeTicket = (beforeBoard.tickets || []).find((item) => item.id === ticket.id);
  const turnCount = () => providerEntries().filter((entry) => typeof entry.roles === 'string').length;
  const beforeTurns = turnCount();
  let dedupeOutput = '';
  let dedupeExit = 1;
  try {
    dedupeOutput = execFileSync(
      'go',
      ['test', './internal/superagent', '-run', 'TestGitHubChannelLiveMarkerDedupe', '-v', '-count=1'],
      {
        encoding: 'utf8',
        maxBuffer: 8 << 20,
        env: {
          ...process.env,
          KEEL_LIVE_GITHUB_REPO: REPO,
          KEEL_LIVE_GITHUB_ISSUE: ISSUE,
          KEEL_LIVE_GITHUB_TOKEN: process.env.LIVE_GITHUB_TOKEN || '',
        },
      },
    );
    dedupeExit = 0;
  } catch (error) {
    dedupeOutput = String(error.stdout || '') + String(error.stderr || '');
    dedupeExit = typeof error.status === 'number' ? error.status : 1;
  }
  fs.writeFileSync(path.join(RUN, 'data', 'dedupe.log'), dedupeOutput);
  check('M1', 'dedupe', 'Go live dedupe 用例通过', dedupeExit === 0, `exit=${dedupeExit}`);

  // The review-driven regressions run inside the same lane so the report
  // carries their real output: durable body/title snapshots (with real PG when
  // the lane DSN is available), dispatch-failure replay, the no-op sink,
  // title-only edits, owner+trailing-marker filtering, human marker quotes,
  // the cross-endpoint cursor max over skipped self comments, and Link
  // pagination.
  const regressionNames = [
    'TestUnchangedIssueUpdateIsNotAFollowUp',
    'TestIssueUpdateSnapshotIsDurableAcrossRestart',
    'TestFollowUpDispatchFailureKeepsSnapshotForRetry',
    'TestNoopInputSinkDoesNotAdvanceDeliveredSnapshot',
    'TestGitHubSourceIgnoresOwnChannelComments',
    'TestGitHubSourceFailsClosedWhenOwnerIsUnknown',
    'TestGitHubSourceFollowsPagination',
    'TestGitHubSourceRefusesOffHostPagination',
  ];
  let regressionOutput = '';
  let regressionExit = 1;
  try {
    regressionOutput = execFileSync(
      'go',
      ['test', './internal/superagent', '-run', regressionNames.join('|'), '-v', '-count=1'],
      { encoding: 'utf8', maxBuffer: 8 << 20, env: { ...process.env, KEEL_TEST_DSN: DSN } },
    );
    regressionExit = 0;
  } catch (error) {
    regressionOutput = String(error.stdout || '') + String(error.stderr || '');
    regressionExit = typeof error.status === 'number' ? error.status : 1;
  }
  fs.writeFileSync(path.join(RUN, 'data', 'regression.log'), regressionOutput);
  check('R1', 'dedupe', '复审回归用例集 exit=0', regressionExit === 0, `exit=${regressionExit}`);
  const regressed = regressionNames.filter((name) => !regressionOutput.includes('--- PASS: ' + name));
  check(
    'R2',
    'dedupe',
    'P1 快照 / 派工失败不推进 / Noop 不推进 / 仅标题编辑 / owner+行尾 marker / 游标取 max / 分页 / PG 重启 全部 PASS',
    regressed.length === 0,
    regressed.length === 0 ? `${regressionNames.length} cases PASS` : 'missing PASS: ' + regressed.join(','),
  );
  check('M2', 'dedupe', '日志断言重试后仍只有 1 条 marker 评论', dedupeOutput.includes('live marker dedupe:') && !dedupeOutput.includes('marker dedupe failed'), brief(dedupeOutput.split('\n').filter((line) => line.includes('marker') || line.includes('FAIL')).join(' | '), 300));
  const markerProbe = commentBodies().filter((body) => body.includes('live E2E marker dedupe probe'));
  check('M3', 'dedupe', '去重探针在 issue 上恰好 1 条', markerProbe.length === 1, `probe comments=${markerProbe.length}`);
  // Three poll intervals (2s each) plus slack: the probe comment's updated_at
  // bump and its marker comment must both have been read by the Source.
  await sleep(9000);
  const afterProbe = await board();
  const afterTicket = (afterProbe.tickets || []).find((item) => item.id === ticket.id);
  const afterTurns = turnCount();
  check(
    'M4',
    'dedupe',
    '自身评论不会被读回（待人工窗口内 assignment 数不增）',
    afterTicket && afterTicket.assignmentCount === beforeTicket.assignmentCount,
    { before: beforeTicket?.assignmentCount, after: afterTicket?.assignmentCount },
  );
  check(
    'M5',
    'dedupe',
    '自身评论不会被读回（模型 turn 数不增）',
    afterTurns === beforeTurns,
    { before: beforeTurns, after: afterTurns },
  );
  check('M6', 'dedupe', '探针后工单仍为 blocked', afterTicket?.status === 'blocked', afterTicket?.status);
  check('M7', 'dedupe', '探针后工单数不增', (afterProbe.tickets || []).length === 1, `tickets=${(afterProbe.tickets || []).length}`);
  note(dedupeOutput.trim().split('\n').slice(-4).join('\n'));
  note(regressionOutput.split('\n').filter((line) => line.includes('--- PASS') || line.includes('--- SKIP')).join('\n'));
  finishStep();

  // 4. answer via product API + channel writeback
  beginStep('answer', '产品 API 回答 Attention', `POST /v1/superagent/attention/${attention.id}/answer {optionId:"approve"}。`, '200；收件箱不再列该问题；issue 出现答复评论；答复进入同一 worker 会话。');
  const answered = await api('POST', `/v1/superagent/attention/${attention.id}/answer`, { optionId: 'approve', text: '' });
  check('N1', 'answer', '产品 answer 路由 200（operator seam 已装配）', answered.status === 200, `HTTP ${answered.status} ${brief(answered.body, 200)}`);
  const answeredBody = answered.body?.attention || answered.body || {};
  check('N2', 'answer', 'Attention answered，记录答复 actor', answeredBody.status === 'answered' && (answeredBody.answer?.optionId === 'approve' || answeredBody.answer?.actor), {
    status: answeredBody.status,
    answer: answeredBody.answer,
  });
  const answerComment = await waitFor('answer comment', async () => {
    const bodies = commentBodies();
    return bodies.find((body) => body.includes('Human selected: approve') && body.includes('<!-- keel-message:')) || null;
  }, 60000);
  check('N3', 'answer', '答复评论写回 GitHub（含 marker）', Boolean(answerComment), brief(answerComment, 200));
  const liveTicket = await waitFor('ticket after answer', async () => {
    const body = await board();
    return (body.tickets || []).find((item) => item.id === ticket.id && item.status !== 'blocked') || null;
  }, 60000);
  check('N4', 'answer', '答复唤醒同一工单（active 或已完成）', ['active', 'done'].includes(liveTicket.status), liveTicket.status);
  note(`answerStatus=${answered.status}\ncomment=${brief(answerComment, 240)}`);
  await capture(() => gotoIssueWaiting('Human selected: approve'), '03-github-answer.png', 'GitHub issue：产品答复写回评论');
  finishStep();

  // 4. worker ready -> done
  beginStep('done', '员工完成（模型 report ready）', '答复投递给该 Ticket 的 worker（会话仍在则同会话，已结束则派人工 follow-up）；模型调用 `report ready`。', '同一 Ticket；工单 done；issue 出现完成评论；assignment done。');
  const doneTicket = await waitFor('done ticket', async () => {
    const body = await board();
    const found = (body.tickets || []).find((item) => item.id === ticket.id);
    return found?.status === 'done' ? found : null;
  }, 120000);
  const readyEntry = providerEntries().find((entry) => entry.choice?.tool === 'report' && entry.choice?.args?.state === 'ready');
  check('D1', 'done', 'provider 记录了 report ready', Boolean(readyEntry), readyEntry ? brief(readyEntry.choice.args, 200) : 'missing');
  const reportResults = providerEntries().filter((entry) => typeof entry.last?.text === 'string' && entry.last.text.includes('"assignment"'));
  const ticketIdOf = (entry) => {
    try {
      return JSON.parse(entry?.last?.text)?.ticket?.id || '';
    } catch {
      return '';
    }
  };
  const waitingTicket = ticketIdOf(reportResults.find((entry) => entry.last.text.includes('"state":"waiting"')));
  const readyTicket = ticketIdOf(reportResults.find((entry) => entry.last.text.includes('"state":"ready"')));
  check(
    'D5',
    'done',
    '等待与完成报告属于同一 Ticket',
    Boolean(waitingTicket) && waitingTicket === readyTicket && readyTicket === ticket.id,
    { waitingTicket, readyTicket, ticketId: ticket.id },
  );
  check('D2', 'done', '工单最终 done', doneTicket.status === 'done', doneTicket.status);
  const completion = await waitFor('completion comment', async () => {
    const bodies = commentBodies();
    return bodies.find((body) => body.includes(`Ticket ${ticket.id.slice(0, 8)} completed`) && body.includes('<!-- keel-message:')) || null;
  }, 60000);
  check('D3', 'done', '完成评论写回 GitHub（含 marker）', Boolean(completion), brief(completion, 220));
  const answerTurn = providerEntries().find((entry) => (entry.last?.text || '').includes('Human selected: approve'));
  check('D4', 'done', '答复真实进入员工模型（模型看到 Human selected: approve）', Boolean(answerTurn), answerTurn ? `role=${answerTurn.last.role} ${brief(answerTurn.last.text, 200)}` : 'missing');
  note(`status=${doneTicket.status}\ncompletion=${brief(completion, 240)}`);
  await capture(() => gotoIssueWaiting(`Ticket ${ticket.id.slice(0, 8)} completed`), '04-github-complete.png', 'GitHub issue：完成评论');
  await capture(() => gotoProduct('/superagent', '工单看板'), '05-product-board-done.png', '产品 /superagent 看板：完成态与四类计数');
  finishStep();

  // 6. product UI + GitHub screenshots
  beginStep('evidence', '产品详情页与看板收口', '打开真实产品 /superagent/<ticketId> 详情，并复核看板数据。', '详情显示该工单的 Mandate 与执行 Assignment；看板显示 1 张已完成工单。');
  if (!page) await openBrowser();
  const browserOK = Boolean(page);
  check('U1', 'evidence', '浏览器可用', browserOK, browserOK ? 'chromium launched' : 'playwright unavailable');
  if (browserOK) {
    try {
      await gotoProduct('/superagent', '工单看板');
      const boardText = await page.evaluate(() => document.body.innerText);
      check('U2', 'evidence', '产品看板渲染真实工单', boardText.includes(MARKER) || boardText.includes(ticket.id.slice(0, 8)), brief(boardText.replace(/\s+/g, ' ').slice(0, 240)));
      await gotoProduct(`/superagent/${ticket.id}`, '工单');
      await shot('06-product-ticket-detail.png', '产品工单详情（Mandate / Assignment / 时间线）');
      const detailText = await page.evaluate(() => document.body.innerText);
      check(
        'U3',
        'evidence',
        '详情页含该工单的 Mandate 与 Worker 区块',
        detailText.includes(MARKER) && detailText.includes('Mandate') && /Worker|执行员工/.test(detailText),
        brief(detailText.replace(/\s+/g, ' ').slice(0, 300)),
      );
    } catch (error) {
      check('U4', 'evidence', '详情页断言未报错', false, error.message);
      note(`detail error: ${error.message}`);
    } finally {
      if (browser) await browser.close();
    }
  }
  finishStep();

  // final board snapshot for the report
  const finalBoard = await board();
  const finalComments = commentBodies();
  report.stats = [
    { value: String((finalBoard.tickets || []).length), label: '工单总数' },
    { value: String(finalBoard.counts?.done ?? 0), label: '完成' },
    { value: String(finalBoard.counts?.blocked ?? 0), label: '待人工' },
    { value: String(finalComments.length), label: 'issue 评论' },
    { value: String(providerEntries().filter((entry) => entry.choice?.tool).length), label: '模型工具调用' },
    { value: String(report.assertions.filter((item) => item.result === 'PASS').length), label: '断言通过' },
  ];
  report.conclusion =
    failures === 0
      ? `5.2 真实 GitHub 链路跑通：真实 issue 被 Source 接单为工单，员工通过模型 \`ask\`/\`report\` 工具提问并等待，产品 API 回答后 Channel 把问题/答复/完成评论写回 issue（marker 单条、重试去重），工单最终 done。${report.assertions.length} 条断言全部通过。`
      : `本次 5.2 真实 GitHub 链路未全绿：${failures} 条断言失败（见下表），其余步骤证据已保留，未做任何伪造。`;
  report.gaps = [
    {
      severity: 'P2',
      title: '真实 repo 的 allowlist 只区分 actor，不区分部署自身','detail':
        'Channel 用 token owner 身份写评论，GitHub 无法给出 Bot 身份；本次以评论里的隐藏 marker 过滤自身回声。若运维更换 token 身份或允许跨账号写评论，仍需该过滤。',
    },
    {
      severity: 'P2',
      title: 'issue_updated 按持久化「已投递标题/正文」快照去重',
      detail: 'GitHub 每次评论都会 bump issue.updated_at；Source 会产出 issue_updated。工单文档保存最近一次真正送达 worker 的标题+正文，同内容的事件被丢弃；正文或标题真实变化投递一次并推进快照，重启后规则不变。',
    },
    {
      severity: 'P2',
      title: 'worker 模型是脚本化 provider，不是真实 LLM',
      detail: 'report/ask 的调用、参数校验、幂等与审计都是真实产品路径；但“模型如何决定提问/完成”由确定性脚本给出，不评测模型判断质量。',
    },
    {
      severity: 'P2',
      title: '本轮线程是 issue；PR 线程未实跑',
      detail: 'GitHub Source/Channel 对 issue 与 pull_request 共用同一适配器路径（thread kind 不同）；本轮只在真实 issue 上验证评论回写，PR 会话的线程 id 形态未跑。',
    },
    {
      severity: 'P2',
      title: 'live 只有单账号（Gaubee），未用第二个人类账号实测 marker 引用',
      detail: 'owner+行尾 marker 的精确过滤由单测覆盖（人类引用 marker 仍投递、owner 行中 marker 仍投递）；本轮 live 的评论都由同一 token owner 发出，无法在真实 GitHub 上证明“别的人类账号引用 marker 不被误杀”。',
    },
    {
      severity: 'P2',
      title: '仅支持 user token；GitHub App installation token 不支持',
      detail: 'Source 通过 GET /user 解析 token owner 以识别自身评论；App installation token 没有 user login，Poll 会失败关闭。部署必须用 classic PAT 或 gh auth token（repo scope）。后续若要支持 App，需要按 bot 身份（评论 author 为 <app>[bot]）设计过滤。',
    },
  ];
  report.evidence = [
    { path: 'runtime/serve.log', note: '真实 keel serve 启动、super-agent queue enabled、Recover/Poll 日志' },
    { path: 'runtime/provider.log', note: '脚本化员工模型的每次调用与工具选择（含 ask / report waiting / report ready）' },
    { path: 'runtime/bootstrap.log', note: '角色/Profile/两个 Bot/授权组的创建结果' },
    { path: 'runtime/worker-tools.log', note: '启用部署后为 worker Bot 补 report/ask 工具授权' },
    { path: 'data/dedupe.log', note: '真实 GitHub marker 去重用例输出' },
    { path: 'data/regression.log', note: '复审回归：P1 持久化快照（真 PG 重启）、仅标题编辑、owner+行尾 marker、人类引用 marker、跨端点游标取 max' },
    { path: 'shots/', note: 'GitHub issue 与产品看板/详情截图' },
  ];
  report.commands = [
    `KEEL_LIVE_GITHUB_REPO=${REPO} KEEL_LIVE_GITHUB_ISSUE=${ISSUE} KEEL_LIVE_GITHUB_TOKEN=<token> \\\n  go test ./internal/superagent -run LiveMarker -v`,
    `scripts/superagent-live-github/run.sh`,
  ];
  report.meta.push(`started：${startedAt}`);
  report.meta.push(`finished：${new Date().toISOString()}`);
  report.footer = failures === 0 ? 'live GitHub lane: PASS' : `live GitHub lane: FAIL (${failures})`;

  fs.writeFileSync(path.join(RUN, 'report.json'), JSON.stringify(report, null, 2) + '\n');
  execFileSync(process.execPath, [path.join(process.cwd(), 'scripts/superagent-acceptance/report.mjs'), RUN], { stdio: 'inherit' });
  console.log(failures === 0 ? 'LIVE E2E PASS' : `LIVE E2E FAIL (${failures})`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((error) => {
  console.error('live e2e crashed:', error.stack || error.message);
  if (current) {
    current.result = 'FAIL';
    current.log += `\ncrash: ${error.message}`;
  }
  fs.writeFileSync(path.join(RUN, 'report-crash.json'), JSON.stringify({ error: String(error.stack || error.message), steps: report.steps, assertions: report.assertions }, null, 2));
  process.exit(1);
});
