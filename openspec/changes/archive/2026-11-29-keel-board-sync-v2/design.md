# Design: keel-board-sync-v2

## Storage boundary (board v0.1.3 → v0.1.4)

- Interfaces live in `store`, `syncstore`, `audit` and `automation`; no `pgx`/`database/sql` types cross the boundary.
- `pgadapter` owns syncstore/audit/automation SQL, transactions and advisory locks; `pgadapter/logstore` owns the log; `pgadapter/schema` owns the embedded migrations and their schema-derived advisory lock.
- `storage.Capabilities` declares `Durable`, `CrossProcessLeases`, `Migrations` and `ListenNotify`; an absent capability is a typed `storage.ErrUnsupported`, never a silent downgrade. PostgreSQL declares the first three; `ListenNotify` is false because `Wait` polls the head (LISTEN/NOTIFY would live in `pgadapter/logstore` if added).
- `conformance.RunStorage` runs the append log, sync entry/lease/health, automation lease and audit contracts through interfaces only. PostgreSQL opts in from `pgadapter`; the in-memory adapters run the same suite.
- `TestStorageBoundary` discovers every module package dynamically (so new subpackages cannot be missed), skips only the adapter/cmd/scripts, and asserts `pgadapter` still owns pgx.
- The deployment assemblies (`cmd/board`, keel's `internal/boardintegration`) call `Capabilities.Require(Durable, CrossProcessLeases, Migrations)` before serving; a weaker adapter fails at startup with typed `storage.ErrUnsupported`.
- The conformance suite proves cross-process leases with two handles, not just by comparing declarations.

## Sync status contract

- board v0.1.2 exposes read-only fields keyed by sync entry: `targets[]`, `lag.{inbox,outbox}` with `outstanding`, `oldestAt` and `ageSeconds`, and `failures24h`.
- `lag` members are `null` when that queue has no outstanding row, never a zero-second timestamp; missing fields are omitted/null, never fabricated.
- The design's flat `oldestPendingAgeSeconds` is derived at the keel mount from the oldest of `lag.inbox.ageSeconds` / `lag.outbox.ageSeconds`; it is null when neither queue has an outstanding row or the board sent no measurement.
- keel mount maps the fields without changing permissions; UI renders `unknown` only when the API explicitly omits.
- Existing retry/credential/dead-letter behavior is unchanged.

## UI

- Sync page: real targets/lag/24h failures; one honest backend-gap note only when all fields are absent; no repeated "unknown" metrics.
- 390: selected state tab is revealed inside the horizontal tab strip (deep links included); filter sheet body scrolls; footer wraps so actions stay reachable on 390×667.
- Task: provider error copy stays product-level; each external copy offers an explicit "查看原始错误" disclosure for the raw error code, while the first screen never shows it. Mobile copy-link remains reachable.
- Automation: next run shows a date, cancellation uses the destructive weight and an AlertDialog; watcher dead/lastError are real facts, and retry is rendered only when dead > 0.
