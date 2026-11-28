# Design: keel-board-sync-v2

## Storage boundary

- Interfaces live in keel-board domain packages; no `pgx`/`database/sql` types cross the boundary.
- PostgreSQL adapter owns SQL, transactions, advisory locks, LISTEN/NOTIFY and migrations.
- Dialect capability flags make unsupported behavior explicit (e.g. `notify`, `advisoryLock`).
- A shared conformance suite runs against the PG adapter; future adapters opt in.
- No SQLite implementation in this change; only compile-time seams and tests.

## Sync status contract

- keel-board exposes read-only fields keyed by sync entry: `targets[]`, `oldestPendingAgeSeconds`, `failures24h`.
- Absence is `null`/omitted, never a fabricated zero.
- keel mount maps the fields without changing permissions; UI renders `unknown` only when the API explicitly omits.
- Existing retry/credential/dead-letter behavior is unchanged.

## UI

- Sync page: one honest backend-gap note only when all fields are absent; no repeated “unknown” metrics.
- 390: selected state tab scrolls into view; filter sheet body scrolls; footer can wrap on short screens.
- Board task: provider error copy stays product-level, with an optional disclosure for raw text; mobile copy-link remains reachable.
