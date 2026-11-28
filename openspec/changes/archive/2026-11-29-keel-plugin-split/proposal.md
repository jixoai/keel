# Proposal: keel-plugin-split

## Why

Board and superagent are currently coupled through `internal/superagentboard` and a shared mount path. The desired architecture is two plugins: board owns the task/channel/thread substrate, superagent owns tickets/assignments/attention/outbox and depends on board. This is a breaking change; no compatibility bridge or legacy data path is kept.

Only board and superagent need their own domain storage. The plugin platform needs shared plugin state; attachment/telemetry use blob/telemetry/platform services; commands and UI/config plugins do not get independent databases.

## What Changes

- Define a plugin storage contract: platform `plugin_state`/config plus per-plugin domain store interfaces; PostgreSQL is the only implementation in this change.
- Extract board into a plugin that exposes task/channel/thread/append/query services and owns board storage.
- Extract superagent into a plugin that owns ticket/assignment/attention/outbox/subscription/cursor/report/lease storage and declares `requires: [board]`.
- Remove `internal/superagentboard` bridge/mirror and all board ⇄ superagent imports; board MUST NOT import superagent.
- Load order: board before superagent; if board is disabled/absent, superagent is `PENDING`, not half-loaded.
- Breaking data model: rebuild board/superagent tables under the new ownership; do not keep legacy compatibility or dual-write.
- No SQLite implementation in this change; the storage interface leaves room for a later adapter.
