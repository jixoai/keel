# Storage and runtime

Storage is where Keel's memory budget is won or lost, and where "the ledger is the only truth"
becomes concrete SQL. This page covers the PostgreSQL schema, the blob layer (chunked in-database
plus content-addressed overflow), admission pooling, the resource budget, and the measured numbers
that back them.

## 1. PostgreSQL: one database, logical isolation

Keel uses **one PostgreSQL 18 database** with `tenant_id` on every row and composite keys, not one
database or process per tenant (`README.md:70`). Row-level security is **on by default**: every
tenant table gets `ENABLE` + `FORCE ROW LEVEL SECURITY` and a `tenant_isolation` policy that
compares `tenant_id` with `current_setting('app.tenant_id', true)`, and the application assumes a
dedicated non-owner role instead of connecting as the table owner (`internal/store/rls.go:259`,
`internal/store/rls.go:57`). An unset setting yields NULL, so a query that forgot its tenant
predicate returns zero rows rather than another tenant's data; operator paths (provisioning,
pre-tenant auth lookups) opt out explicitly with `app.bypass_rls = 'on'`
(`internal/store/rls.go:292`).

`KEEL_RLS=off` is the explicit escape hatch for a managed cluster that forbids `CREATE ROLE`
(`cmd/keel/serve.go:313`); it is never auto-disabled. The isolation and quota layers around this
schema are on [Tenancy](/core/tenancy).

Why not a process or a SQLite file per tenant: 2GB ÷ 200 tenants = 10MB per tenant, and the Go
runtime alone needs 10–20MB — the arithmetic rules it out. 200 SQLite files would each carry a page
cache (2MB default → 400MB) and cannot share one transaction/backup/quota view
(`docs/ARCHITECTURE.md:32`).

The schema lives in two places:

- `internal/store/schema.sql` — the base tables;
- `internal/kernel/schema/0001…0005*.sql` — kernel migrations: fencing, tool-call index,
  external/handoff, pending input, loadout revision. Migrations run at startup, inside a
  transaction with `SET LOCAL statement_timeout=0`; lock waits are bounded by `lock_timeout=30s`
  with automatic retry on `55P03` (`docs/RUNBOOK.md:203`).

| Table | Purpose | Notable detail |
|---|---|---|
| `tenants` | tenant row + `config jsonb` + `config_revision` | the revision is the change identity; no ETag/mtime semantics on top |
| `sessions` | one row per session; `next_seq` allocates the event sequence | also carries the external marks: `state_owner`, `external_backend`, `external_ref`, `external_status`, `metering` |
| `events` | **the ledger**: `(tenant_id, session_id, seq)` primary key, `kind`, `payload jsonb`, `dedupe_key` | partial unique index `events_dedupe` on `(tenant_id, session_id, dedupe_key)` where not null |
| `checkpoints` | compaction summaries up to `upto_seq` | a projection, never a source of truth |
| `submissions` | request idempotency `(tenant_id, session_id, request_id)` | one submission per retried request |
| `tasks` | durable task rows (submissions/generation/nested tools, structured output) | the PG projection that exists today |
| `runs` | one run row per session: `status`, `lease_until`, `attempts`, `cursor_seq`, `last_error`, `pending_input`, `read_watermark`, `loadout_*` | claimed with `FOR UPDATE SKIP LOCKED`; a crashed process leaves the lease behind and any worker may reclaim it |
| `blobs` / `blob_chunks` / `blob_refs` | blob metadata + 64KiB chunk rows + refcount keyed by the owning event | `UNIQUE (tenant_id, sha256)` dedupes uploads; `STORAGE EXTERNAL` keeps compressed bytes out of TOAST |
| `usage_ledger` | one row per provider request, keyed by the runner-generated `request_id` | **no price columns** by design; `unaccounted` rows are upgraded in place when usage is recovered |
| `mcp_oauth_pending` / `mcp_oauth_credentials` | MCP OAuth state machine and encrypted credentials | AES-256-GCM with AAD = tenant+server |
| `model_settings` / `model_settings_history` / `tenant_secrets` | tenant model document + secret storage | secrets are encrypted; only references appear in configuration |
| `groups` / `session_placement` | placement containers and each session's lease (`epoch` + `lease_until`) | run slots are counted from `runs` per group and the tenant quota across groups, so two workers cannot oversell a group (`internal/store/schema.sql:894`, `internal/store/schema.sql:935`) |
| `file_manifest` / `session_snapshot` | the file plugin's path → hash projection and the tree hash recorded per event seq | both are projections: `fs.write` commits before the manifest changes, and a fork/snapshot materializes the prefix from the tree (`internal/store/schema.sql:855`, `internal/store/schema.sql:871`) |
| `task_cards` / `task_events` | the task panel's card rows and their event stream | a card is UI memory; a durable `tasks` row is scheduling bookkeeping (`internal/store/schema.sql:749`, `internal/store/schema.sql:773`) |
| `plugins` / `plugin_audit` | installed plugin inventory and its append-only audit | process-level on purpose: plugin enablement is not a tenant fact, so the tenant role gets no grant (`internal/store/schema.sql:582`, `internal/store/schema.sql:603`) |
| `notifications` | per-tenant notification rows | `push_attempted_at` is the compare-and-set that makes "push once" survive restarts (`internal/store/schema.sql:824`) |

Two database-side behaviours matter for the runtime:

- **Notify on append.** An `AFTER INSERT` trigger publishes `pg_notify('keel_events', tenant:session:seq)`
  (`internal/store/schema.sql:207`); subscribers re-read the log after their cursor, so a dropped
  notification costs one heartbeat interval, never data.
- **Readiness is schema-deep.** `GET /v1/ready` pings PG *and* checks the running schema (kernel
  migrations 0001/0003/0004/0005 columns, external session marks, handoff tables). An old database
  missing any of them answers 503 instead of pretending to be ready (`docs/RUNBOOK.md:290`).

## 2. Blobs: 64KiB chunks in PostgreSQL, overflow in a CAS

The storage decision was measured, not assumed (`docs/ARCHITECTURE.md:52`):

| Option | Result | Verdict |
|---|---|---|
| single-row whole-image `bytea` | 98.6 TPS (c16), whole image materialised | rejected — memory and throughput both fail at 1000 streams |
| **64KiB chunked `bytea`** (PK `(tenant_id, image_id, seq)`) | **1603 TPS (c16) / 1122 TPS (c32)** | adopted |
| Large Objects (`lo_*`) | one connection + transaction per stream | rejected — 1000 concurrent connections is not viable |
| local NVMe files | 3–10× faster than large PG blobs | adopted for large originals/overflow |

The rule in the uploader (`internal/blob/upload.go:39`, `:58`):

- **≤1MiB** — spooled to a temporary file (so memory stays constant), hashed with SHA-256, then
  written as 64KiB chunk rows into PostgreSQL.
- **>1MiB** — moved into the local **content-addressed store** (`CASPath`, two-level `sha256`
  directories, `internal/blob/upload.go:24`); PostgreSQL keeps only the metadata
  (`storage_kind` = `pg_chunked` | `local_cas`).
- Uploads are deduplicated per tenant by `sha256`; `ref_count` is maintained through `blob_refs`,
  which is keyed by the owning event so a double-increment is impossible; unreferenced blobs get a
  TTL via `expires_at`.

**Reading is streamed in both directions.** The base64 reader
(`internal/blob/stream.go:116`) walks the blob by chunk sequence, base64-encodes whole blocks
(4 base64 chars ≡ 3 source bytes, which is exactly what makes any wire offset resumable), and
writes with `io.Copy`. A whole image never enters memory; the read batch size is a
`BufferPool`-backed 64KiB×batch.

**Range resume** is client-driven: the client reports the wire offset it received
(`Range: bytes=<off>-`, must be a multiple of 4), the server maps it back to a source offset
(`srcOff = off/4*3`), answers `206 + Content-Range`, and `totalWire = 4*⌈n/3⌉` comes from metadata.
The base64 output carries no line breaks, or the 4:3 relationship would break
(`docs/BACKPRESSURE.md:102`).

### Lifecycle: TTL reaping, orphan scan, thumbnails

Reclamation is a database-first pipeline (`docs/BLOB-LIFECYCLE.md:73`):

- **Candidates** are rows with `ref_count = 0`, `complete = true`, an `expires_at` in the past and
  no parent blob naming them as `thumb_blob_id`; the select uses `FOR UPDATE SKIP LOCKED`, so
  several workers can sweep at once (`docs/BLOB-LIFECYCLE.md:86`).
- **Delete order is strict**: `blob_refs` → `blobs` → `blob_chunks` in one transaction, commit,
  then unlink the CAS file outside it after a cross-tenant `CASBlobExists` recheck — the same
  sha256 may be shared by another tenant (`docs/BLOB-LIFECYCLE.md:94`). Repeating a sweep is
  idempotent (`internal/blob/gc.go:86`).
- **Orphans are retried, not guessed**: `SweepOrphanCAS(grace)` walks only the two-level hex layout,
  ignores fresh files, and deletes a file only when no `blobs` row (`storage_kind = 2`) names its
  sha256; the `layerfs/` subtree is out of scope by construction (`docs/BLOB-LIFECYCLE.md:111`,
  `internal/blob/gc.go:142`). Defaults: TTL sweep every `1m`, orphan scan every `30m` with a fixed
  1h grace (`docs/BLOB-LIFECYCLE.md:125`, `docs/BLOB-LIFECYCLE.md:126`).
- **Thumbnails are their own blobs**: a re-encoded `image/png` row (`complete`, `ref_count = 0`,
  longest edge 320px) tracked by `thumb_status` (`pending` → `ready`/`failed`); the parent row is
  never rewritten (`docs/BLOB-LIFECYCLE.md:141`). `GET /v1/files/{id}/thumbnail` answers `200`,
  `202` (pending), `404` (none) or `409` (failed) (`docs/BLOB-LIFECYCLE.md:161`).

The file tree is a projection too: `file_manifest` maps path → hash, `session_snapshot` records the
layerfs tree hash at an event seq, and `MaterializeSnapshot` rebuilds a directory from that tree
(`internal/fileplugin/service.go:849`). A write commits the `fs.write` event before the manifest
moves, so the ledger stays the truth (`internal/fileplugin/service.go:353`). The HTTP surface for
upload, download, ranges and thumbnails is on [Files](/core/files).

**No dedicated storage engine.** The research conclusion is explicit: Haystack/SeaweedFS/LMDB/bbolt/
Badger all have RSS ≈ data size or a 4MB buffer per read, with no benefit at 1GbE; local CAS + page
cache + PostgreSQL metadata is the ceiling (`docs/ARCHITECTURE.md:152`). The full trade-off study is
`docs/BLOB-STORAGE-RESEARCH.md`.

## 3. Admission: pools, not queues

Requests are pooled by size class — Small <256KiB, Medium <2MiB, Large ≥2MiB — with soft
reservations (640/300/60) and hard caps (900/400/80). Admission requires the class to be under its
hard cap *and* the reservation arithmetic for the other classes to still fit. Queue capacity is 256;
overflow answers `429 + Retry-After` immediately and the client retries with jitter
(`docs/BACKPRESSURE.md:57`).

The HTTP server configuration that makes SSE and JSON coexist in one process
(`docs/BACKPRESSURE.md:76`):

| Field | Value | Reason |
|---|---|---|
| `ReadHeaderTimeout` | 5s | slowloris |
| `ReadTimeout` | 0 | SSE would be cut in half; JSON routes set their own read deadline (30s) |
| `WriteTimeout` | 0 | it is an absolute deadline for the whole response — SSE cannot survive it; write deadlines are per-write instead |
| `IdleTimeout` | 75s | keep-alive |
| `MaxHeaderBytes` | 16KiB | |
| kernel sndbuf | `SetWriteBuffer(64KiB)` | 1000 connections → ~128MB of kernel memory instead of the multi-MB autotune default |

Slow-client detection is two-level: a single write may block at most 4s, and a 15-second window
with under 180KB of progress (12KB/s) for two consecutive windows marks the client slow; total
duration is only a backstop (`docs/BACKPRESSURE.md:90`). After a deadline the connection is
aborted (`panic(http.ErrAbortHandler)`) rather than writing an error page into a half-written
chunked frame.

## 4. The resource budget

Per-stream memory, with 1000 streams (`docs/BACKPRESSURE.md:108`):

| Item | Bytes |
|---|---|
| read batch (batch=2×64KiB) | 131,072 |
| pgx row decode (transient) | 65,536 |
| base64 output block | 87,384 |
| net/http bufw | 2,048 |
| goroutine stack | 8,192 |
| handler/header/timer | ~4,096 |
| TLS record buffer | ~17,408 |
| **per plaintext stream** | **~227KiB** |
| **per TLS stream** | **~245KiB** |

1000 live streams ≈ **227–245MB userspace + 128–160MB kernel** ≈ **~390MB** — and `GOMEMLIMIT`
does not cover kernel socket buffers, so the two must be accounted separately. The runtime flags
that shape the budget (`cmd/runtime/main.go:43`):

```bash
GOMEMLIMIT=1610612736 ./keel-runtime \
  -addr 127.0.0.1:13740 -cas /tmp/keel-cas \
  -streams 2200 -chunk-batch 1 -admit-rate 1200 -runs 64
```

- `-streams` — total concurrent stream cap (1000 SSE sessions plus image streams need 1600–2200).
- `-chunk-batch` — chunks per PG query: `1` ≈150KB/stream, `2` is the 1Gbps mode (halves query count).
- `-bandwidth` — global outbound bytes/s (0 = unlimited; production sets it to 85–90% of the link).
- `-runs` — concurrent agent runs; `-no-worker` serves the API only.

**PostgreSQL tuning for the 2GB profile** (measured stable): `shared_buffers=384MB`,
`max_connections=60`, `work_mem=4MB`, `maintenance_work_mem=64MB`, `wal_buffers=16MB`,
`max_wal_size=1GB`, and `pgxpool` with 24–32 connections (`docs/ARCHITECTURE.md:90`). The pool
settings are part of the design: `MaxConns=24` (the naive default `max(4,GOMAXPROCS)` is too small,
while c32 measured 30% *worse*), `MinConns=8`, `MaxConnLifetime=30m`, `MaxConnIdleTime=5m`,
`HealthCheckPeriod=30s`, plus `statement_timeout=5s` and `idle_in_transaction_session_timeout=3s`
on connect (`docs/BACKPRESSURE.md:154`). Acquiring a connection has a 300ms context timeout →
`503 + Retry-After`, so network backpressure never leaks into a slow-query cascade.

## 5. Measured results

The 2GB / 1000-session baseline, as reported in `README.md:26` and `docs/RUNBOOK.md:152`
(2026-11-06 and the 2026-11-08 final round; different load mixes, so both are listed):

| Metric | 2026-11-06 baseline | 2026-11-08 final round |
|---|---|---|
| Concurrent sessions (SSE) | 1000, 0 hard errors | 1000, 0 hard errors |
| Images | 800×1MiB completed (1.12GB base64 out; 200 returned 429 by pool and were retried) | 998/1000 completed (1853 × 429 retried) |
| Throughput | 18.6 MB/s (client-limited) | 23.2 MB/s |
| Latency | p50 28.9s / p95 31.4s (= 1.4MB ÷ 50KB/s, server adds ≈0) | — |
| 2GB cgroup peak | 544MB | **640MB (29.8%)**, gate ≤1.5GB |
| Double-process handoff peak | — | 656MB (32.0%) |

`429` responses are expected backpressure from the size-class pools, not stream errors; the load
tool retries per `Retry-After`. `docs/MATURITY.md:23` defines the GA-scale condition (2GB cgroup,
1000 SSE, 64 concurrent runs, periodic 1MiB images, RSS peak < 1.8GB, 0 OOM).

The stateless E2E suite is **8/8**: `requestId` idempotency across restart, SSE cursor across
restart, blob wire-offset resume across restart, agent reply, retry append, `SIGKILL` during a model
call, `SIGKILL` during tool execution, and 100 concurrent sessions × 2000 events with no gap
(`cmd/e2e/main.go:86`, `docs/LOADOUT.md:168`).

## 6. Operating the runtime

| Surface | Where |
|---|---|
| liveness | `GET /v1/health` (always 200; use for k8s liveness) |
| readiness | `GET /v1/ready` (PG + schema; 503 while draining; use for k8s readiness) |
| metrics | `GET /metrics` (Prometheus text 0.0.4) and `GET /v1/metrics` (JSON) |
| drain / takeover | `POST /v1/runtime/drain`, `GET /v1/runtime/status` |
| graceful stop | SIGTERM: stop claiming, 503 new API requests, flush SSE, send `event: drain`, close at a bounded rate; SIGINT cancels immediately |

Metric families are fixed-cardinality on purpose — **never** label by tenant/session/run
(`docs/RUNBOOK.md:248`): `keel_runs_started_total`, `keel_runs_finished_total{status}`,
`keel_run_attempts_total{outcome}`, `keel_run_attempt_duration_seconds`,
`keel_runs_pending`, `keel_streams_active`, `keel_draining`, `keel_leases_expired_total`,
`keel_compactions_total`, plus Go runtime and `pgxpool` gauges. A zombie attempt (lease taken over
or terminal state already committed) is refused by the `status='running'` + `attempts` guard on
finish (`ErrStaleRun`), counted as `abandoned`, and does **not** write a terminal state.

`/metrics`, `/v1/health` and `/v1/ready` are **unauthenticated**: bind them to loopback or restrict
them at the proxy. The full runbook (test suites, load tests, chaos, the 2GB cgroup replay, the
image and compose flows) is `docs/RUNBOOK.md`.
