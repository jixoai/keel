# Identity realtime invalidation stream — evidence (2026-11-22)

Object: commits `590fcfa` (server) and `cf776d7` (web), branch
`task/keel-identity-realtime`, rebased onto main `67648f7` (originally based on
`548e3ae`). This evidence closes the two
remaining realtime items of the archived change
`openspec/changes/archive/2026-11-22-keel-bot-identity-access/`:

- §3 UI/UX: 「实时性：账号/权限/Bot 状态变化在 2s 内反映（SSE/事件），不要求手动刷新。」
- §4 B: 「实时性：权限撤销/账号禁用/Bot 归档在 2s 内反映到 UI 与下一次调用。」

Owner classified the gap as P1. The delivered A6 E2E did not exercise it.

## 1. Delivered shape

### Server route

`GET /v1/identity/events` (SSE) in `internal/harnessapi/identity_events.go`:

- authenticated; tenant taken from the edge principal (`X-Tenant-ID` must match
  the session/bot-key tenant);
- authorized with the existing tenant-level `app.read` key (no new registry
  key);
- frames are invalidation facts only: `event: identity` +
  `{"kind":"accounts"|"permissions"|"bots"|"teams"|"sessions","at":"<RFC3339>"}`
  — no ids, names, grants, keys or other PII/credentials;
- `retry: 2000` reconnect hint; `: connected` / `: heartbeat <time>` comment
  keepalives every 15s (test override 40ms);
- bounded: 256 subscribers deployment-wide, 32 per tenant, 64-fact buffer per
  subscriber; overflow is a 503, never a silent no-stream; a full buffer drops
  the oldest fact (bounded memory);
- disconnect cleanup: `defer subscription.Close()` + request context; the hub
  count returns to zero (asserted).

`internal/accessgate/matrix.go` declares `GET /v1/identity/events` as
`ModeKey` with `app.read`; `IdentityEndpoints()` includes the same route, so the
coverage floor test and the startup `CheckRoutes` gate both see it.

### Publish points (after the write **and** its mandatory audit row)

| Projection | Mutations |
|---|---|
| `accounts` | account create, disable/enable, password reset |
| `permissions` | account↔group add/remove; group create/rename/delete; group member add/remove; group grant add/remove; role create/update/delete; profile create/update/delete |
| `bots` | Bot create/update/archive; key issue/revoke (role update/delete and profile writes also publish `bots` because they change the persona/owner shown beside a Bot) |
| `teams` | TeamBot create/update (including the dedicated roster route)/archive |
| `sessions` | session share add/revoke; Bot invoke; TeamBot start; session archive |

A handler publishes immediately before its success response. Every 4xx/5xx
path returns before that call, so a refused permission check, a failed store
write and a failed audit append publish nothing.

### Web subscriber

`web/src/data/identity-stream.ts` is the single client:

- fetch-based SSE (the same transport seam as the existing session stream);
- auth-keyed: `BooksChrome`'s effect calls `identityStream.sync(store.auth())`;
  `store.loadIdentity()` also calls the registered starter, so a login on
  `/settings` or `/plugins` starts it too;
- 40ms coalescing window: a burst of the same kind is one re-read; multiple
  kinds in one window share the same flush;
- permissions are re-read first (`whoami`), then the projections: `accounts` /
  `permissions` → `identityAccess` (accounts, groups, current detail);
  `bots`/`teams` → `botAccess` (lists, current detail, keys); `sessions` →
  `store.refreshSessions()`; the open share dialog re-reads its tuples;
- reconnect: 500ms × 1.7 exponential backoff capped at 8s; after a dropped
  connection the next open re-reads every loaded projection once (SSE facts are
  not replayed);
- 401 closes the stream and returns to the login gate; logout/account switch
  closes it through `store.clearSessionState()`'s closer, so an old cookie is
  never replayed; 403/404/405/501 (no key/route) stop without retry;
- data-layer guards read `can(...)` before re-reading a projection, so a
  revoked capability does not produce a background 403.

No `web/App.tsx` change: integration is `web/src/data/store.ts` +
`web/src/screens/books/shell.tsx`.

## 2. Exact 2s proof

Command (fresh PostgreSQL database `keel_identity_realtime_test`, recreated
before the run):

```
KEEL_TEST_DSN='postgres://postgres:runtime@127.0.0.1:13741/keel_identity_realtime_test?sslmode=disable' \
  go test ./internal/harnessapi -run 'TestIdentityEvent' -count=1 -v
```

Result (log line, verbatim):

```
identity event latency: 121.975167ms (kind=permissions)
```

The measurement starts immediately before a real
`POST /v1/permission-groups` (handler + store + audit) and stops when the
httptest SSE reader receives the `identity` frame. The test fails if that is
> 2s (`TestIdentityEventStreamDeliversMutationWithinTwoSeconds`). An earlier
race-enabled run measured 55.077959ms.

Web-side latency assertions (same 2s bound) are in
`web/src/data/identity-stream.test.ts`:

```
✓ identity stream: an accounts fact re-reads accounts within 2s, once per burst (220ms)
✓ identity stream: a permissions fact re-reads whoami and revokes the local capability (53ms)
✓ identity stream: a sessions fact refreshes the session list; heartbeats do nothing (371ms)
```

`waitFor` returns the measured elapsed time and each case asserts
`< 2000ms`; the permissions case also asserts that a whoami answer without
`session.read` flips `can("session.read")` to false while `app.read` keeps the
stream open.

## 3. Negative controls and lifecycle

Go tests (`internal/harnessapi/identity_events_test.go`), all passing:

| Test | Fact proven |
|---|---|
| `TestIdentityEventsHubIsTenantScopedAndBounded` | tenant A's fact never reaches tenant B; unknown kind dropped; 256/32/64 bounds and close cleanup |
| `TestIdentityEventStreamIsTenantScopedAtTheRoute` | two httptest SSE servers on one hub: tenant B's live connection receives no tenant A fact |
| `TestIdentityEventStreamDeliversMutationWithinTwoSeconds` | real mutation → frame in 121.98ms |
| `TestIdentityEventStreamPublishesOnlySucceededWrites` | 403 (no `bot.create`), 400 (missing password), 409 (duplicate) publish nothing; a later valid write still publishes |
| `TestIdentityEventStreamHeartbeatCleanupAndReconnect` | heartbeat frame within 1s; subscriber count returns to 0 after disconnect; a fresh subscriber receives the next fact |
| `TestIdentityEventStreamPublishesEveryProjection` | `permissions`, `bots`, `teams`, `sessions` each publish their own kind after a real HTTP write |
| `TestIdentityEventStreamRequiresAuthenticationAndAppRead` | no principal → 401; principal without `app.read` → 403 and zero subscriptions |

Web tests (`web/src/data/identity-stream.test.ts`), 6/6:

- accounts fact re-reads accounts and three duplicate frames coalesce to **one**
  request (no duplicate requests);
- permissions fact re-reads whoami/groups/accounts and revokes `can()`;
- sessions fact re-reads the session list; a heartbeat comment causes **no**
  request;
- reconnect re-reads loaded projections (facts are not replayed);
- 401 closes and never reconnects (900ms later still one request) and returns
  to the login gate;
- logout closes the transport.

## 4. Gates run

```
go build ./...                                        exit 0
go vet ./internal/harnessapi ./internal/accessgate ./internal/httpapi   exit 0
KEEL_TEST_DSN=... go test -p 1 ./internal/harnessapi ./internal/accessgate ./internal/httpapi -count=1
  ok internal/harnessapi   267.013s
  ok internal/accessgate     0.657s
  ok internal/httpapi      185.027s
  exit=0
cd web && nsh check                                   exit 0
cd web && nsh test src/data/identity-stream.test.ts --target tui
  6 tests: 6 passed, 0 failed (2.3s)
cd web && nsh test --target tui
  287 tests: 287 passed, 0 failed (7m 10s)
```

`-p 1` is deliberate: `internal/httpapi/access_pg_test.go` installs a temporary
`auth_events` failure trigger in the shared test database, so package-parallel
runs can make an unrelated identity test see `audit store is down`. With the
project's serial test discipline the touched packages are green.

The branch was rebased concurrently onto main `67648f7` (548 files changed by
other lanes; none under `web/`, `internal/harnessapi/`, `internal/accessgate/`
or `internal/httpapi/`). After the rebase, `go build ./...`, `go vet` on the
three touched packages, `go test -p 1 ./internal/harnessapi -run
'TestIdentityEvent'`, `nsh check` and the targeted TUI stream test were rerun
green; the numbers above are from the identical package sources.

`TestEveryV1RouteHasPermissionDeclaration` in `internal/httpapi` passes with the
new route, and `accessgate.UnusedRules` reports no drift.

## 5. Review / walk-through

- Event body is exactly `{kind, at}`; tenant isolation is checked at the hub and
  again at the route (the tenant comes from the authenticated principal, not a
  client field).
- A fact is published only after the business write and the audit append
  succeeded; denied/failed writes are covered by tests.
- Subscriber memory is bounded per connection and per tenant; publish never
  blocks on a slow client; close is idempotent and locked against publish.
- The SSE handler flushes headers before entering the loop, survives client
  disconnect via the request context, and stops on write/flush errors.
- The web client never invents rows: every fact triggers the ordinary,
  authorized read path; a failed re-read is left to the next fact or the page's
  explicit retry.
- The stream carries no resource identity, so a tenant member with `app.read`
  learns only that a projection changed.

## 6. Known limits (not regressions)

- The hub is process-local by design (`keel serve` is one process). A
  multi-replica deployment needs a shared fan-out in front of it; this slice
  does not claim cross-process delivery.
- Facts are not replayable. A reconnecting client re-reads every loaded
  projection once, which covers the gap; a client that never loaded a
  projection does not fetch it until its page asks.

## 7. Files

Server: `internal/harnessapi/identity_events.go` (new),
`internal/harnessapi/identity_events_test.go` (new),
`internal/harnessapi/identity.go`, `internal/harnessapi/api.go`,
`internal/harnessapi/accounts.go`, `internal/harnessapi/permission_groups.go`,
`internal/harnessapi/roles_profiles.go`, `internal/harnessapi/bots.go`,
`internal/harnessapi/team_bots.go`, `internal/harnessapi/shares.go`,
`internal/harnessapi/sessions.go`, `internal/accessgate/matrix.go`,
`internal/harnessapi/identity_pg_helpers_test.go`.

Web: `web/src/data/identity-stream.ts` (new),
`web/src/data/identity-stream.test.ts` (new), `web/src/data/store.ts`,
`web/src/data/identity-access.ts`, `web/src/data/bots.ts`,
`web/src/data/shares.ts`, `web/src/screens/books/shell.tsx`.
