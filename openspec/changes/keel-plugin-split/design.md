# Design: keel-plugin-split

## Storage ownership

- Plugin kernel: tenant-scoped `plugin_state`, config, install/version and dependency status.
- Board plugin: append log, tasks, channels, threads, sync entries, board audit; own domain store.
- Superagent plugin: tickets, assignments, attentions, outbox, subscriptions, source cursors, reports, writer lease; own domain store.
- Attachment: blob service + metadata/plugin_data; no independent domain database.
- Telemetry: bounded telemetry store/config; no business relational store.
- Commands/UI/MCP/skills/hooks/triggers: platform state or core services; no per-plugin DB.
- Collaboration/core/checkpoint remain core services; they do not access board tables.

## Plugin dependency

- Plugin manifests declare `requires`; host performs topological load.
- `superagent` requires `board`; missing board produces `PENDING` with a reason.
- Unload is reverse order; board cannot unload while superagent is active.
- Board plugin exposes a narrow service interface; superagent never imports board store/PostgreSQL types.

## Breaking migration

- Drop/recreate board and superagent domain tables under the new owners.
- Do not keep old `/superagent` bridge, mirror or dual-write path.
- Data export/reset is an explicit alpha operation; no compatibility guarantee.

## As built (2026-11-28)

- Service-plugin kernel: `internal/pluginruntime`. It owns `requires`/`provides`,
  topological activation, `PENDING`, reverse unload and the dependent guard.
  The agent plugin runtime (`harnessext`) is unchanged: board/superagent are host
  services, not model extensions.
- Platform storage: `internal/pluginstore` (contract + memory + conformance) and
  `internal/pluginstore/pg` (PostgreSQL adapter over `plugins`, `plugin_state`,
  `plugin_config`, `plugin_dependency`). No SQLite adapter.
- Board plugin: `internal/boardplugin` owns `LogBoard` (moved out of the deleted
  `superagentboard`) and publishes `Service`. The interface gained only the
  `Tasks`/`Channels`/`Thread` query methods the import path needs.
  `boardintegration.Surface` implements `pluginstore.DomainStore`.
- Superagent plugin: `internal/superagentplugin` publishes the supervisor and the
  board writer/importer; `internal/superagentstore` owns all `superagent_*` SQL
  and `superagent_board_links`. `requires: [board]`.
- The HTTP command bridge was removed rather than ported: the change only
  requires the board service boundary, and forwarding board actions back to the
  supervisor would preserve the coupling the split removes. Board actions no
  longer mutate tickets; ticket→board projection and board-task→ticket import
  remain.
- Residual (documented, not BLOCKED): board HTTP mounting and superagent runtime
  construction remain in `cmd/keel`; the plugin kernel owns their lifecycle,
  dependency status and service publication. A dynamic HTTP mount is a later
  increment, not a storage or dependency violation.
