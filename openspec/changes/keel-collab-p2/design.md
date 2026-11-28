# Design: keel-collab-p2

- Assignment rebind: validate source/target WorkItem, reject cross-tenant/closed; return typed conflict/not-found.
- Link: validate endpoint kinds, dedupe by (tenant, from, to, kind), keep relationship across WorkItems explicit.
- Tenant registry: request-scoped lookup keyed by tenant; no package-global mutable registry.
- Checkpoint: canonical summary hash; requestId dedupe table survives prune; TTL cleanup is lazy and bounded.
- All behavior covered by unit/PG tests; no API breaking change.
