---
title: Tenancy, RLS and quotas
description: How tenants are isolated and limited — one database with tenant_id, row-level security on by default, two quota families with one vocabulary per dimension, and the tenant management API.
---

# Tenancy, RLS and quotas

Keel is multi-tenant in **one PostgreSQL 18 database**: `tenant_id` on every row, composite keys,
no database or process per tenant (`README.md:70`). Three layers keep tenants apart — predicate
discipline in the schema, row-level security as the second line of defence, and two independent
quota vocabularies enforced at three points.

## 1. The tenant row

`tenants` holds the identity and the edge limits (`internal/store/schema.sql:5`), plus the
configuration document and the revision that changes on every accepted write
(`internal/store/schema.sql:16`). `quota_storage_bytes` is added with an `ALTER` so an existing
database converges instead of being recreated (`internal/store/schema.sql:703`).

## 2. Row-level security is on by default

- The application does not connect as the table owner: it `SET ROLE`s into a dedicated
  non-owner role (`keel_app`, created `NOLOGIN NOSUPERUSER NOBYPASSRLS`), because owners and
  superusers bypass RLS (`internal/store/rls.go:45`, `internal/store/rls.go:259`).
- Every tenant table gets `ENABLE` + `FORCE ROW LEVEL SECURITY` and one `tenant_isolation` policy
  whose `USING`/`WITH CHECK` compare `tenant_id` with `current_setting('app.tenant_id', true)`.
  The missing-ok form makes an unset setting NULL, so a forgotten predicate returns **zero rows**
  rather than another tenant's data, and the operator path opts out explicitly with
  `app.bypass_rls = 'on'` (`internal/store/rls.go:292`).
- The protected table list is explicit in one place (`internal/store/rls.go:57`). `plugins` is
  excluded on purpose: plugin enablement is a process fact, not a tenant fact, and the tenant role
  gets no grant on it at all (`internal/store/rls.go:54`).
- The migration is idempotent, serialised by an advisory lock and retried on concurrent-catalog
  errors; a cluster that cannot create the role fails loudly instead of leaving `FORCE` RLS with no
  role to serve (`internal/store/rls.go:120`).
- `KEEL_RLS=off` is the explicit escape hatch for a managed cluster that forbids `CREATE ROLE`
  (`cmd/keel/serve.go:313`); it is never inferred.

## 3. Quotas: two vocabularies, three enforcement points

Run and event budgets live in the tenant configuration document; stream, bandwidth and storage
budgets live in the `tenants` row. The split is deliberate and the code states the rule: "exactly
one quota vocabulary per dimension" (`internal/store/byte_usage.go:181`).

| Vocabulary | Dimensions | Enforced by | Refusal |
|---|---|---|---|
| Tenant document (`host.QuotaConfig`) | `runConcurrency`, `eventsPerMinute` | run concurrency: the durable placement gate; event rate: the kernel's in-process bucket | `429` (`internal/httpapi/server.go:838`) |
| `tenants` quota columns | `quota_streams`, `quota_bytes_per_sec`, `quota_storage_bytes` | the process edge, before any ledger row exists | `429` / `413` (`internal/quota/edge.go:9`) |

Run concurrency is admitted durably by the placement gate while the kernel keeps only the
event-rate bucket; the split is deliberate so one run is never counted twice
(`cmd/keel/serve.go:412`, `internal/quota/limits.go:227`). The stored tenant document wins over the
deployment default and is re-read on a 2-second cache (`internal/quota/limits.go:58`,
`internal/quota/limits.go:244`); a read or parse failure refuses the admission instead of guessing
(`internal/quota/limits.go:20`). Storage is a live measurement against the blobs table, so releasing
a blob really releases quota (`internal/quota/edge.go:24`). Placement details are on
[Placement and groups](/core/placement).

## 4. Tenant management API

Tenant-wide operations are operator operations, and there is no tenant-wide API credential: human
accounts log in with passwords and Bots use bot-bound `keel_bot_…` keys
(`internal/harnessapi/tenancy.go:7`). Provisioning requires the deployment's operator key as a
Bearer header (`internal/harnessapi/tenancy.go:158`, `docs/HOST.md:172`).

| Method / path | Purpose | Notes |
|---|---|---|
| `POST /v1/admin/tenants` | create a tenant and seed its built-in permission groups | `201`; idempotent by id (`internal/harnessapi/tenancy.go:58`) |
| `GET /v1/quota` | the tenant's own limits and current usage | `streams`, `bytesPerSec`, `storageBytes`; counts only, no price (`internal/harnessapi/tenancy.go:141`) |
| `GET /v1/auth/whoami` | resolved tenant, account, Bot and permission set | what the presented credential actually is (`internal/harnessapi/tenancy.go:56`) |
| `PUT /v1/tenants/{id}/config` | replace the tenant configuration document | body is the document; `400` invalid, `413` too large, `404` unknown tenant (`docs/HOST.md:178`) |
| `GET /v1/tenants/{id}/config` | `{tenantId, revision, config}` | `config_revision` is the change identity |
| `GET /v1/tenants/{id}/usage` | token counts grouped by session/provider/model | counts only, `unaccountedEntries` included |
| `POST /v1/tenants` | create a tenant with an explicit id | `X-Tenant-ID` is the row id, not a credential (`docs/HOST.md:177`) |

```bash
# create a tenant; the operator credential is the deployment's KEEL_ADMIN_KEY
curl -s -X POST http://127.0.0.1:13750/v1/admin/tenants \
  -H "Authorization: Bearer $KEEL_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"acme"}'

# what the tenant may use right now; machine clients present a bot key
curl -s http://127.0.0.1:13750/v1/quota \
  -H "Authorization: Bearer $KEEL_BOT_KEY"
```

`/v1/quota` requires a key with app-read authority; operator routes are marked `ModeOperator` in
the access matrix, so an ordinary tenant credential cannot provision
(`internal/accessgate/matrix.go:154`, `internal/accessgate/matrix.go:383`).

## 5. Single-tenant `keel serve` vs multi-tenant `cmd/runtime`

`keel serve` is **one tenant's** product process (`KEEL_SERVE_TENANT`): its extension read surface
(plugins, triggers) reports that process's global assembly state, and multi-tenant reuse of that
surface needs a separate change (`docs/HARNESS.md:39`). Process-level resources cannot pretend to
be tenant-scoped inside one process — the MCP catalogue is the cited example — so a multi-tenant
deployment runs **one process per tenant** (`docs/HOST.md:262`).

`cmd/runtime` is the bare runtime rather than the product shell: it keeps `X-Tenant-ID` as its
runtime tenant interface, while everywhere else the auth middleware injects the header and deletes
a client-supplied one (`docs/HOST.md:270`). Multi-tenancy is therefore a deployment shape (one
database, many tenants, one product process each) and not a per-request mode switch in one
`keel serve`.
