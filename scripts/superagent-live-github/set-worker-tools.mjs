#!/usr/bin/env node
// Second bootstrap pass: once the enabled deployment serves the super-agent
// product tools, grant the worker Bot `report`/`ask` in its Harness. The
// deployment validates harness tools against what it actually serves, so this
// cannot be part of the first pass (the queue needs both Bots to assemble).
//
//   node set-worker-tools.mjs <base-url> <worker-bot-id>
const BASE = (process.argv[2] || '').replace(/\/$/, '');
const BOT = process.argv[3] || 'worker';
if (!BASE) {
  console.error('usage: set-worker-tools.mjs <base-url> <worker-bot-id>');
  process.exit(2);
}

let cookie = '';
let csrf = '';
const session = await fetch(`${BASE}/v1/auth/session`);
if (!session.ok) throw new Error(`auth session: HTTP ${session.status}`);
const auth = await session.json();
cookie = (session.headers.getSetCookie ? session.headers.getSetCookie() : [session.headers.get('set-cookie')])
  .map((entry) => String(entry).split(';')[0])
  .filter((part) => part.includes('='))
  .join('; ');
csrf = auth.csrfToken || '';

async function api(method, endpoint, body) {
  const headers = {};
  if (cookie) headers.Cookie = cookie;
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET') headers['X-Keel-CSRF'] = csrf;
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

const before = await api('GET', `/v1/bots/${BOT}`);
if (before.status !== 200) {
  throw new Error(`read worker bot: HTTP ${before.status} ${JSON.stringify(before.body).slice(0, 400)}`);
}
const harness = before.body.bot?.harness || { tools: [], context: { permissions: [] } };
const permissions = Array.from(
  new Set([...(harness.context?.permissions || []), 'task.read', 'task.write', 'session.read', 'model.invoke']),
);
const updated = await api('PATCH', `/v1/bots/${BOT}`, {
  harness: {
    tools: Array.from(new Set([...(harness.tools || []), 'report', 'ask'])),
    context: { ...(harness.context || {}), permissions },
  },
});
if (updated.status !== 200 && updated.status !== 201) {
  throw new Error(`patch worker harness: HTTP ${updated.status} ${JSON.stringify(updated.body).slice(0, 600)}`);
}
const after = await api('GET', `/v1/bots/${BOT}`);
console.log(JSON.stringify({ tools: after.body.bot?.harness?.tools, permissions: after.body.bot?.harness?.context?.permissions }));
