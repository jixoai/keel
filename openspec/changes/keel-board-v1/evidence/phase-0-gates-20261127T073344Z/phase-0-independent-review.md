# Phase 0 final independent review

Captured from the independent reviewer's final result on `d5a9e0a6d983685994b5df49c266350d80ab4dbd`.
Formatting normalized; findings unchanged. Verdict: **PASS**.

- P0: 0
- P1: 0
- P2: recorded below; none block `v0.1.0`.
- Repository: clean; local and remote main at `d5a9e0a`; no tag existed before the review.
- CI: https://github.com/jixoai/keel-board/actions/runs/45956949204 (same commit) — overall success.
  - stress job `140365527104`: success 07:26:23Z, `-count=10 -timeout=30m` over store/log/syncstore/automation/conformance.
  - test job `140365527514`: success 07:31:37Z, gofmt/vet/test/E2E/race.

## Round-1 P0/P1 closure

| Round-1 item | Status | Evidence |
| --- | --- | --- |
| P0 core RLS missing | closed | `store/migrations/0008_core_rls.sql` ENABLE+FORCE on `board_heads/events/requests`; `store/postgres.go` sets `keel.tenant_id` per transaction; `store/rls_core_test.go` restricted-role A/B, unset tenant sees zero rows, cross-tenant insert refused. Mutation removing the tenant predicate turns it red. |
| P1 0005 restricted-owner upgrade | closed | `0005_sync_thread_identity.sql` lifts FORCE only for the owner migration transaction and restores it; `rls_core_test.go` applies 0001–0005 as a restricted owner, deduplicates, and verifies FORCE. |
| P1 `-count=10` flaky | closed | CI stress run above: store 455.324s, log 175.604s, syncstore 678.337s, automation 460.511s, conformance 59.337s, all ok. Lease tests use seconds. |
| P1 source read/join missing | closed | `server/sources.go`, routes in `server/server.go`, GitHub `SourceReader`, `cmd/board` `JOIN_CHANNELS`, disabled 409, status; E2E sources PASS. |
| P1 audit/metrics missing | closed | 14 design metrics in `metrics/`; durable `audit` + `0009_audit.sql` RLS; unauth paths use fixed `unknown` labels and a per-key 10/min failure-audit bound. Mutations for both turn red. |
| P1 README/PORTING/SYNC stale | closed | Docs corrected for SQLite, content-addressed IDs, adapter skeletons, and next-phase claims. |
| P1 evidence/tag | expected gap | Phase-0 evidence is generated after the tag by `scripts/board-phase0-evidence.sh`; task 9.4 is checked only after that. |

## Final gates

| Gate | Result | Evidence |
| --- | --- | --- |
| `gofmt -l .` | exit 0, empty | 0.142s |
| `go vet ./...` | exit 0 | 18.03s |
| `go test ./... -count=1` with DSN | exit 0 | 138.77s, all packages ok |
| `go test -race ./...` | exit 0 | 128.82s, zero DATA RACE |
| `go run ./scripts/e2e` | PASS | 59 steps (58 pass, 0 fail, 1 documented skip), 321/321 assertions, 32.32s |
| CI stress + test | success | run 45956949204, jobs above |
| Repository | clean | HEAD = origin/main = d5a9e0a; `go mod verify` passes |

The one E2E skip is the real `cmd/board` mirror carrier; its assembly difference is documented in `scripts/e2e/README.md`.

## Mutations (6/6 red)

Removing the core RLS predicate, lease token/liveness check, bridge key ownership guard, audit flood bound, source disabled check, or GitHub repo allowlist each made its targeted test fail.

## Design/tasks consistency

- In-process deterministic projection is now the documented design (§4.2); no PG projection tables.
- Source reads are read-only, `board.manage`, and write no board event; E2E checks the head does not move.
- Entry POST/PATCH is explicitly Phase 1 (10.6); Phase 0 builds the registry from deployment configuration.

## P2 (non-blocking)

1. Design/API path drift to reconcile before Phase 1: webhook path, `/me`, SSE path, typing route, place-only idempotency digest, `tenant_id` text vs uuid.
2. `keel.bypass_rls` is a SQL-settable GUC; only `Tenants` uses it and no HTTP path reaches it, but the “operator-only” wording is not enforced. Use a privileged connection/role or narrow the document.
3. Audit covers webhook, bridge, source read/join, dead letters, and retries; per-attempt outbound success/inbound processed are documented Phase 1 scope.
4. Bridge unknown key returns 404 while a disabled configured key returns 200; webhook distinguishes neither. Low-risk enumeration.
5. `server.fail` still logs raw unexpected errors; responses are generic.
6. No regression test for the SSE history-failure 503 path (the code is fixed).
7. `FileURLSafe` does not resolve DNS; the exact `FileHosts` allowlist is the boundary.
8. Phase 0 CLI requires a mirror per sync key, so a source-only connection needs a dummy mirror; `UpsertEntry` does not re-enable a disabled entry on restart (Phase 0 has no PATCH).

## Taggability

The reviewer approved tagging `d5a9e0a6d983685994b5df49c266350d80ab4dbd` as `v0.1.0`: P0/P1 clear, CI test+stress green on the same commit, local gates green, clean tree. Evidence and the pin record are produced after the tag; task 9.4 follows them.
