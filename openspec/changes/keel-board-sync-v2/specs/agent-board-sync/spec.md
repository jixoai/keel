# agent-board-sync

## ADDED Requirements

### Requirement: Storage access is dialect-neutral

keel-board domain, server and sync layers MUST depend on storage interfaces, not PostgreSQL drivers. The PostgreSQL adapter MUST implement those interfaces; unsupported dialect capabilities MUST be explicit.

#### Scenario: PG adapter conformance
- **WHEN** the storage conformance suite runs against the PG adapter
- **THEN** append log, lease, sync entry and audit behavior pass without PostgreSQL types leaking into callers

### Requirement: Sync status exposes real targets and ages

The board sync read API SHALL expose per-entry external targets, oldest pending age and 24h failure count when available. Missing fields MUST be omitted or null, never coerced to zero.

#### Scenario: backend has targets
- **WHEN** a sync entry has targets and pending rows
- **THEN** `targets`, `oldestPendingAgeSeconds` and `failures24h` are returned and the UI shows them

#### Scenario: backend lacks a field
- **WHEN** a field is unavailable
- **THEN** the API omits/null it and the UI shows one honest gap note, not a fabricated value

### Requirement: Board P2 states remain truthful

Watcher dead/lastError, delivery depth, provider errors, mobile copy-link and 390 tab/filter behavior MUST be surfaced or explicitly marked unavailable; the UI MUST NOT claim a retry or value that has no backend support.

#### Scenario: watcher has no dead letters
- **WHEN** the watcher DTO has no dead/lastError fields
- **THEN** the retry action is not rendered and the page does not promise it exists

#### Scenario: 390 selected tab
- **WHEN** the selected state tab is outside the initial scroll window
- **THEN** it is scrolled into view and remains reachable
