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
