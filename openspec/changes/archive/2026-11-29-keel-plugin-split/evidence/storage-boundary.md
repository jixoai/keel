# keel-plugin-split — storage and plugin boundary

Date: 2026-11-28
Branch: `task/keel-plugin-fix`
Base: `77cea0bc` (board P2 v0.1.4 + plugin split)
Code commits: `6d75fab8` (P1), `03569a74` (P2 batch), `5a9cd668` (pluginstore owner), `01912296` (worker-session E2E)

## Ownership

| Owner | Tables / surface | Adapter |
|---|---|---|
| Plugin platform | `plugin_state`, `plugin_config`, `plugin_dependency` | `internal/pluginstore` contract; `internal/pluginstore/pg` PostgreSQL adapter; `Memory` test adapter; shared `conformance` suite |
| Core plugin inventory | `plugins` (install/version/state) | `internal/store` + `harnessext.Manager`; `pluginstore` has no install writer (single owner) |
| Board plugin | board log/task/channel/thread tables (owned and migrated by `keel-board` v0.1.4 `pgadapter`), `board_tenant_sync_entries`, `board_tenant_blob_keys` | `boardintegration.Surface` (implements `pluginstore.DomainStore`); `boardplugin.LogBoard` over the board log API |
| Superagent plugin | `superagent_work_items`, `superagent_assignments`, `superagent_worker_reports`, `superagent_attentions`, `superagent_links`, `superagent_subscriptions`, `superagent_source_cursors`, `superagent_source_events`, `superagent_outbox`, `superagent_session_index`, `superagent_board_links` | `internal/superagentstore` PostgreSQL adapter (implements `superagent.Store` + `collab.SessionIndex`) |
| Attachment | blob + platform metadata; no independent domain schema | existing blob/attachment services |
| Telemetry | bounded telemetry store/config; no business relational store | existing telemetry plugin/platform |
| Commands / UI / MCP / skills / hooks / triggers | platform state or core services; no per-plugin DB | existing platform/core |

No SQLite adapter was added. `pluginstore.Store` is the adapter boundary; a second
dialect would implement the same interface and pass the same conformance suite.

## Plugin dependency

- `internal/pluginruntime` is the service-plugin kernel: manifest-like `Spec`
  (`requires`/`provides`), Kahn topological load with declaration-order tie
  break, `PENDING` for a missing/disabled/failed requirement, reverse-order
  unload, and a refusal to stop a plugin while an active dependent requires it.
- `keel.board` provides `board`; `keel.superagent` requires `board` and provides
  `superagent`. Dependency status is persisted tenant-scoped in
  `plugin_dependency`.
- Tests: `internal/pluginruntime/runtime_test.go` (order, PENDING for
  absent/disabled/failed board, reverse unload, dependent guard, persisted
  status, cycle/duplicate-capability refusal) and
  `internal/superagentplugin/plugin_test.go` (real board/superagent plugins:
  start order board→superagent, unload order superagent→board).

## Board service extension (minimal, recorded)

The board dependency is consumed through `boardplugin.Service` only. The change
added three query methods to that interface because the old bridge reached into
the board log directly:

- `Tasks(ctx, tenantID, TaskQuery) ([]TaskInfo, error)` — task read for the
  board→ticket import.
- `Channels(ctx, tenantID) ([]ChannelInfo, error)` — channel read model.
- `Thread(ctx, tenantID, taskID, after, limit) ([]protocol.MessageView, error)` —
  thread/reply read.

No board storage API, SQL or table was changed for the split, and the board
module is pinned at v0.1.4 (read-only use). This is the minimum extension; a
future board service can add write-side bulk operations without changing the
ownership boundary.

## Breaking changes (no compatibility layer)

- Deleted `internal/superagentboard` (bridge, mirror, commands, token/config
  copies) and `cmd/board-sync-e2e`.
- Deleted the board HTTP command bridge: a board claim/complete/fail/block/
  unblock/assign/cancel no longer calls the supervisor. The board projection is
  one-way (ticket → board); the supervisor remains the ticket truth.
- Schema migration drops `collab_*` and `board_links` outright and creates
  `superagent_*` / `superagent_board_links`. No views, dual writes or legacy
  bridge remain. Board-owned tables are migrated by the board module's own
  adapter, not by keel SQL.
- `internal/store` no longer implements `superagent.Store` or
  `collab.SessionIndex`; those moved to `internal/superagentstore`.
- Board augmentation no longer emits `mirror{source,readOnly,bridge}` or
  bridge-move filtering; `archived` and `moves` remain board semantics.

## E2E

`internal/superagentplugin/e2e_pg_test.go` (requires `KEEL_TEST_DSN`):

- `TestBoardTaskToTicketToBoardWriteback`: board task → board service import →
  ticket in real PostgreSQL (the importer writes no assignment) → ticket
  completion → writer claim/complete through the board service → board task
  `completed`.
- `TestImportedBoardTaskDispatchesWorkerSession`: board task → import with
  `WorkerBot` → the normal guarded Host dispatch binds an ACTIVE assignment with
  a worker session and Bot `worker-bot` → two production `Writer.SyncOnce`
  passes (import + projection) mirror one claim and do not re-import the
  projection.
- `TestWriterImportDoesNotReimportItsOwnProjection`: two/three production
  `SyncOnce` passes over a non-imported ticket's projection stay stable (one
  ticket, one board task, link points at the original ticket).

No board table is read by the superagent side.

Import decisions are unit-pinned in `internal/superagentplugin/importer_test.go`
(projected/terminal/archived tasks skipped, `WorkerBot` and owner session
stamped, dispatch called, no sessionless assignment) and in
`internal/superagent/orchestrator_test.go`
(`TestDispatchTicketBindsImportedWorkerSession`).
