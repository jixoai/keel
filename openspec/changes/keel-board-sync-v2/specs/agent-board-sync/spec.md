# agent-board-sync

## ADDED Requirements

### Requirement: Storage access is dialect-neutral

keel-board domain, server and sync layers MUST depend on storage interfaces, not PostgreSQL drivers. The PostgreSQL adapter MUST implement those interfaces; unsupported dialect capabilities MUST be explicit.

#### Scenario: PG adapter conformance
- **WHEN** the storage conformance suite runs against the PG adapter
- **THEN** append log, lease, sync entry and audit behavior pass without PostgreSQL types leaking into callers

#### Scenario: adapter capabilities are explicit
- **WHEN** a storage adapter declares its capabilities
- **THEN** every capability it does not declare is refused with a typed `storage.ErrUnsupported` rather than silently downgraded, and the conformance suite checks the declaration it was given

#### Scenario: domain packages stay driver-free
- **WHEN** `go list -deps` runs for store, syncstore, audit, automation, server, sync or log
- **THEN** no `github.com/jackc/pgx` package appears; the driver is reachable only through `pgadapter`

### Requirement: Sync status exposes real targets and ages

The board sync read API SHALL expose per-entry external targets, oldest pending age and 24h failure count when available. Missing fields MUST be omitted or null, never coerced to zero. The oldest pending age SHALL be reported as `lag.inbox` / `lag.outbox` (`outstanding`, `oldestAt`, `ageSeconds`); a null member means that queue has no outstanding row, not a failed measurement. A mount MAY derive a flat `oldestPendingAgeSeconds` from the oldest non-null member.

#### Scenario: backend has targets
- **WHEN** a sync entry has targets and pending rows
- **THEN** `targets`, measured `lag` (with `ageSeconds`) and `failures24h` are returned and the UI shows them

#### Scenario: backend lacks a field
- **WHEN** a field is unavailable
- **THEN** the API omits/null it and the UI shows one honest gap note, not a fabricated value

#### Scenario: no outstanding row is null
- **WHEN** both queues have no pending or leased row
- **THEN** `lag.inbox` and `lag.outbox` are null and the derived `oldestPendingAgeSeconds` is null, never 0

### Requirement: Board P2 states remain truthful

Watcher dead/lastError, delivery depth, provider errors, mobile copy-link and 390 tab/filter behavior MUST be surfaced or explicitly marked unavailable; the UI MUST NOT claim a retry or value that has no backend support.

#### Scenario: watcher has no dead letters
- **WHEN** the watcher DTO has no dead/lastError fields
- **THEN** the retry action is not rendered and the page does not promise it exists

#### Scenario: 390 selected tab
- **WHEN** the selected state tab is outside the initial scroll window
- **THEN** it is scrolled into view and remains reachable

#### Scenario: provider error disclosure
- **WHEN** an external copy carries a provider error
- **THEN** the first screen shows only the product category and next step, and the raw error stays behind an explicit "查看原始错误" disclosure
