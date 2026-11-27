# Phase 0 independent review — round 1

Captured from the independent reviewer's result on `6688dc2c64a523d4773c19f5778a33ee62be4b90`.
Formatting normalized; findings unchanged. This round blocked the `v0.1.0` tag.

Verdict: **FAIL**. Worktree clean; no local or remote tag; nothing merged or pushed by the reviewer.

## Gates

| Gate | Result |
| --- | --- |
| `gofmt -l .` | empty, exit 0 |
| `go vet ./...` | exit 0 |
| `go test ./...` without DSN | exit 0 |
| `go test ./...` with DSN | exit 0 |
| `go test -race ./...` with DSN | exit 0 |
| `go run ./scripts/e2e` | PASS: 53 steps (51 pass, 0 fail, 2 skip), assertions 232/232 |
| `go test -count=10 ...` | first run **exit 1**, rerun exit 0 |

First `-count=10` failure:

```text
--- FAIL: TestPostgresRestartRecoversCursorAndLease (1.25s)
    store_test.go:687: recovered ack: automation: the lease is not live
FAIL	github.com/jixoai/keel-board/automation	176.927s
```

E2E report: `/tmp/keel-review-gates/e2e-20261127T044825Z/report.html`. Two skips:
1. `source read / join`: the server did not mount the routes.
2. the real `cmd/board` mirror carrier: no platform-endpoint injection seam; harness hosted the server.

## P0

1. **Core board tables lacked RLS; a non-superuser could read across tenants.**
   `store/migrations/0001_board_log.sql` created `board_heads/board_events/board_requests`
   without `ENABLE/FORCE ROW LEVEL SECURITY`; `0003` covered only `board_sync_*`, `0006`
   automation. With a non-superuser role and no `keel.tenant_id`, two tenants'
   `board_events` (message text and raw payload) were visible; sync tables returned 0.
   Design §4.4 and task 2.1 require forced RLS on all `board_*` tables.

## P1

2. **`0005_sync_thread_identity.sql` could not deduplicate as a non-superuser.** The
   unscoped `DELETE` matched zero rows under the already-forced sync RLS, then the unique
   index failed with `DETAIL: Duplicate keys exist.` A fresh empty schema passed; an
   upgrade with duplicates failed.
3. **`-count=10` was flaky.** `TestPostgresRestartRecoversCursorAndLease` used
   `LeaseTTL = 150ms`; under parallel package load the recovered lease expired before its
   ack. The first real run failed; a rerun passed.
4. **Source read/join API missing.** Routes stopped at webhooks/bridge/entries/status/
   inbox/retry/outbox/threads. No `/v1/board/sources/{key}/read` or `/join`; E2E recorded
   the skip itself. Design §14.10 and task 4.6 were unmet.
5. **Audit/metrics missing.** No metrics, Prometheus, expvar, or independent audit write.
   Design §18.2's `board_events_total`, append duration, sync depths, dead letters, and
   webhook duration had no code.
6. **README/PORTING/SYNC contradicted the code.** `docs/SYNC.md:16` claimed no durable
   store/HTTP server/worker; `docs/PORTING.md:173` claimed typing/schedules/watchers
   missing; `docs/PORTING.md:310` and `docs/SYNC.md:1077` claimed subscription/backfill/
   files/typing remained the server's next phase; `docs/PORTING.md:23` claimed a SQLite
   store; `:41` claimed content-addressed IDs; README/`:303` still said adapter skeletons.
7. **Phase 0 evidence/tag/review records missing.** No `evidence/` directory; tasks
   1.1–9.4 unchecked; no review findings; no `v0.1.0` tag locally or remotely.

## P2

- `server/asProtocol` fell back to `403` + `err.Error()`, and `server.fail` logged raw
  errors; a custom Tenant/Actor/Authorizer error could leak DSN/URL/credential context.
- Bridge unknown key returned `404`; native webhook unknown key returned `200` (key
  enumeration difference, low risk).
- Bridge nonce is claimed before `StoreRecords`; a same-nonce retry gets `409`. Documented,
  acceptable; sidecars must use a new nonce.
- `FileURLSafe` rejects literal IPs but does not check resolved private IPs; the boundary
  relies on the exact `FileHosts` allowlist.
- `handleUploadFinish` did not check `ticket.ExpiresAt`.
- `cmd/keel-board` did not exist (actual `cmd/board`); `server.Version` was
  `0.1.0-phase1`.
- `writePlaceDigest` covers only channel/re; a keyed retry with different text replays the
  first result. Documented in PORTING as deferred.
- `handleEvents` ignored `board.Read` errors on the `visibleViews`/`last` path, which could
  turn a store failure into an empty stream instead of 503.
- `discovery` always returned `"sync":[]`.

## Cross-module and security checks

- Route registration was clean with sync+automation enabled.
- A real `cmd/board` smoke (PG + FS blobs + automation + ChatSDK bridge) served discovery,
  entries, watchers, bridge 401, upload 201/read 200, and exited 0 on SIGTERM.
- Tenant tests in server/access/syncstore/automation passed; no direct cross-tenant HTTP
  path was found. Core RLS was the exception (P0).
- Webhook signatures were verified before parsing for Slack/GitHub/Linear/ChatSDK.
- SSRF/file host checks, secret redaction, ActionCall boundaries, approval guards, and SQL
  parameterization held; the DNS-private-IP gap remains P2.
- Six mutation checks were all caught: core tenant predicate removal, lease token/liveness
  removal, Slack signature always-pass, bridge nonce replay, file allowlist accept-all,
  constant projection digest.

## Task checkboxes

Could be checked with evidence: 1.2–1.4, 2.3–2.10 (2.2/2.3 with qualifiers), 3.1–3.3, 3.5,
4.1–4.5, 4.7, 5.1–5.4, 6.1–6.4, 7.1–7.4, 8.1–8.4, 9.2 (PASS with two explicit skips).

Blocked: 1.1 (`cmd/keel-board` naming), 2.1 (no core RLS; projection tables),
2.2 (in-process projection vs design), 2.3 (digest semantics), 3.4 (audit/metrics),
4.6 (source read/join and entry CRUD), 9.1 (flaky stress, no recorded output),
9.3 (no findings record), 9.4 (no tag/evidence/pin).

## Merge history

`HEAD = 6688dc2c64a523d4773c19f5778a33ee62be4b90`, parent `1093fb3`; no merges; main
linear; `origin/main` equal; no tags.

## Minimum fix list

1. Force RLS on `board_heads/events/requests`; set `keel.tenant_id` in the core store;
   add an A/B non-superuser test.
2. Fix `0005` for a non-superuser owner; add an upgrade test with duplicates.
3. Stabilize the restart lease test; make `-count=10` a recorded release gate.
4. Add `/v1/board/sources/{key}/read` and `/join` (and the entry status route), or revise
   task/design with a recorded deferral.
5. Add audit/metrics or revise task 3.4.
6. Correct README/PORTING/SYNC status.
7. Generate `evidence/phase-0-*.md` (script), record review findings and the pin, then tag
   a new immutable commit `v0.1.0`.
