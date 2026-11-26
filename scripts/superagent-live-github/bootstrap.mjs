#!/usr/bin/env node
// Bootstraps the live-GitHub super-agent deployment through the product HTTP
// API (local-only auth): a role, one profile owned by the local admin, the
// orchestrator and worker Bots, and a permission group that gives the Bot
// owner account the grants the orchestrator principal needs (bot.invoke, ...).
//
// It is idempotent enough for a throwaway database: every resource is created
// fresh on a fresh DSN.
//
//   node bootstrap.mjs <base-url> <out-json>
import fs from 'node:fs';

const BASE = (process.argv[2] || '').replace(/\/$/, '');
const OUT = process.argv[3] || '';
if (!BASE) {
  console.error('usage: bootstrap.mjs <base-url> <out-json>');
  process.exit(2);
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

let cookie = '';
let csrf = '';

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
  cookie = cookieHeader(
    response.headers.getSetCookie ? response.headers.getSetCookie() : [response.headers.get('set-cookie')],
  );
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
    /* keep text */
  }
  return { status: response.status, body: parsed };
}

function must(label, result, want) {
  const accepted = Array.isArray(want) ? want : [want];
  if (!accepted.includes(result.status)) {
    throw new Error(`${label}: HTTP ${result.status} ${JSON.stringify(result.body).slice(0, 600)}`);
  }
  return result.body;
}

const auth = await readAuthSession();
if (!auth.authenticated || !auth.account?.id) throw new Error('local admin session is missing');
const accountId = auth.account.id;

const role = must(
  'create role',
  await api('POST', '/v1/roles', {
    name: 'Live GitHub E2E Role',
    instruction: 'You are a super-agent worker. Work only inside the ticket bound to your session.',
  }),
  201,
);
const profile = must('create profile', await api('POST', '/v1/profiles', { accountId, roleId: role.role.roleId }), 201);

// The `report`/`ask` tools are registered only when KEEL_SUPERAGENT_SOURCE
// assembles the queue, and the assembly needs both Bots to exist first. The
// bootstrap server therefore runs without the switch; a second pass patches the
// worker harness once the enabled server is up (see set-worker-tools.mjs).
const workerHarness = {
  tools: process.env.LIVE_E2E_WORKER_TOOLS === '1' ? ['report', 'ask'] : [],
  context: {
    instruction: 'E2E super-agent worker: use report/ask for the bound ticket.',
    permissions: ['task.read', 'task.write', 'session.read', 'model.invoke'],
  },
};
const worker = must(
  'create worker bot',
  await api('POST', '/v1/bots', {
    botId: 'worker',
    name: 'E2E Worker',
    profileId: profile.profile.profileId,
    modelKind: 'text',
    defaultModel: { providerId: 'e2e', modelId: 'live-e2e-model' },
    harness: workerHarness,
  }),
  201,
);
const orchestrator = must(
  'create orchestrator bot',
  await api('POST', '/v1/bots', {
    botId: 'orchestrator',
    name: 'E2E Orchestrator',
    profileId: profile.profile.profileId,
    modelKind: 'text',
    defaultModel: { providerId: 'e2e', modelId: 'live-e2e-model' },
    harness: {
      tools: [],
      context: {
        instruction: 'Super-agent orchestrator Bot: the queue principal, never a model turn.',
        permissions: ['bot.invoke', 'task.read', 'task.write', 'session.read'],
      },
    },
  }),
  201,
);

// The orchestrator principal is the owner account + Bot harness. The authorizer
// has no super-admin short circuit for that constructed principal, so the
// owner account needs explicit grants (a group is the product's path).
const group = must('create permission group', await api('POST', '/v1/permission-groups', { name: 'E2E Superagent Grants' }), 201);
const groupId = group.group.groupId;
const grantKeys = ['bot.invoke', 'task.read', 'task.write', 'session.read', 'session.write', 'model.invoke'];
for (const key of grantKeys) {
  must(
    `grant ${key}`,
    await api('POST', `/v1/permission-groups/${groupId}/grants`, { key, scope: 'tenant' }),
    [200, 201],
  );
}
must('add member', await api('POST', `/v1/permission-groups/${groupId}/members`, { accountId }), [200, 201]);

const out = {
  accountId,
  tenantId: auth.tenantId,
  roleId: role.role.roleId,
  profileId: profile.profile.profileId,
  workerBotId: worker.bot?.botId,
  orchestratorBotId: orchestrator.bot?.botId,
  groupId,
  grantKeys,
};
if (OUT) fs.writeFileSync(OUT, JSON.stringify(out, null, 2) + '\n');
console.log(JSON.stringify(out));

// Give the store a moment to settle before the caller restarts the server with
// the super-agent assembly enabled.
await sleep(200);
