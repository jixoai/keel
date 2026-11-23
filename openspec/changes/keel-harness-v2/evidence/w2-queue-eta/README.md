# W2 queue + ETA + memory packing (evidence)

Change: `openspec/changes/keel-harness-v2` — tasks.md line 120 (组内并发槽位
`max_concurrent_runs`: 满时排队 + ETA；用户配额在 DB 侧聚合).

Lane: `task/keel-w2-placement` (worktree `.worktrees/keel-w2-placement`).
Base: `1297837`. Group OOM / process isolation is **not** part of this lane and is
not simulated.

## What changed

### 1. A full group queues instead of answering 429

`runs.status` gains `queued`: a durable FIFO entry written by the admission
transaction when `max_concurrent_runs` is reached.

- `runs_queue_seq` (sequence) → `runs.queue_seq`: the arrival order. Positions are
  compared inside one `(tenant, group)`, so a global sequence preserves per-group
  FIFO. A repeated input keeps the existing `queue_seq` (COALESCE), so it cannot
  jump the line.
- A queued row holds **no** slot: group/tenant counts are `status IN
  ('pending','running')`, exactly as before. It is therefore not a hidden
  oversell channel.
- A queued row is **not claimable**: the kernel's `claimablePredicate` matches
  only `pending` / expired `running`. Promotion is the only transition that makes
  a queued run runnable. `TestPGQueuedRunIsNeverClaimable` proves a claim of a
  queued session returns `ErrNoRunnable` and succeeds after promotion.
- The two writers that used to force `pending` now preserve `queued`:
  - kernel `user.message` wake (`internal/kernel/pg.go` Append);
  - `store.EnqueueRun` (the HTTP commit repair path).
  `store.RequeueRun` treats `queued` as "already enqueued" (returns `pending`,
  never bypasses the queue).

### 2. Promotion: FIFO, exactly once, four triggers

One guarded transition (`UPDATE … SET status='pending' WHERE status='queued'`)
takes the head under a per-tenant transaction advisory lock, using
`ORDER BY queue_seq LIMIT 1 FOR UPDATE` (no `SKIP LOCKED`, so a locked head
cannot be skipped in favour of a later entry). It re-checks group slots, the
group memory budget and the tenant quota before each promotion, and stops at the
head if any of them blocks (head-of-line, never out of order).

Triggers:

1. `ReleaseRun` — same transaction as the release (abandoned reservation).
2. Terminal attempt settlement — new `agent.SettlementObserver` seam
   (`internal/agent/worker.go`) → `placement.Gate.RunSettled`
   (`ObserveRunDuration` + `PromoteQueued`), wired through
   `servicehost.Config.Settlements` in `cmd/runtime` and `cmd/keel serve`.
3. Crash recovery — `placementwiring.RunQueueSweeper` (started by both cmds):
   one pass immediately, then every `QueueInterval` (default 5s,
   `KEEL_PLACEMENT_QUEUE_INTERVAL_MS`). It promotes tenants with queued rows even
   when no in-process settlement will ever fire.
4. Admission of a session in a group with free capacity also runs the sweeper
   (but never promotes a queued row directly: the FIFO head wins;
   `TestPGQueuedReAdmissionCannotJumpTheQueue`).

Concurrency: admissions and promotions of one tenant share the advisory lock;
the lock wait is bounded by `SET LOCAL lock_timeout = '1s'` so a burst queues as
a transient 55P03 (retried by `inTx`) instead of dying on the 5s statement
ceiling. `promoteQueuedTx` first probes `EXISTS(queued)` for the tenant, keeping
the common no-queue admission path cheap.

### 3. ETA from observed durations only

New `run_duration_samples` table (tenant, group, duration_ms, observed_at;
RLS/fenced + FK to groups). Every settled attempt writes one sample (worker's
settlement hook), the store retains the newest `ETASampleRetention = 50` per
group and averages the newest `ETASampleWindow = 20`:

- group window first, tenant window as documented fallback (`etaBasis`);
- `wait = avg * ceil(position / max_concurrent_runs)` (`etcWait`);
- **no history → `QueueETA{Known:false}`**; HTTP renders `etaSeconds: null` and
  sends no `Retry-After`. No fabricated 0 anywhere.

### 4. Tenant quota stays a hard refusal

- `!selfActive && tenantLimit > 0 && activeTenant >= limit` → typed
  `*CapacityError{CapacityTenant}` (429 + Retry-After) **before** any queueing,
  so a spent quota never becomes a queue entry (`TestPGQueueNeverBypassesTheTenantQuota`).
- Promotion re-checks the quota and stops when it is spent; lowering the quota
  under a queue blocks promotion until room returns
  (`TestQueuePromotionStopsAtALoweredTenantQuota`).

### 5. Pack by memory budget across groups

- `Options.MemoryEstimator` is the only source of a session's estimate; the old
  invented default (8 MiB) is removed.
- `Options.DefaultMemoryLimitBytes > 0` with no estimator fails the **boot**
  (`FromEnv`, names `KEEL_GROUP_MEMORY_LIMIT_BYTES` and the missing estimator).
  A memory-bounded group reached at runtime without an estimate is a typed
  `*UnsupportedError{memory_estimate_unavailable}` → HTTP 501.
- Pack strategy: among active `pack-*` groups, best fit = smallest leftover after
  placing the estimate (ties by name); an unbounded group is the fallback when no
  bounded group fits. Refusal when nothing fits: `CapacityGroupMemory` 429 with
  `limitBytes` / `usedBytes`.
- Admission records the estimate on `runs.memory_bytes` and enforces the group
  ceiling (`used + estimate <= memory_limit_bytes`), so packing cannot be
  bypassed by a stale placement decision.
- Promotion re-checks the budget; a head that no longer fits waits without
  letting a later, smaller entry jump (`TestQueueMemoryBlocksPromotionWithoutJumpingFIFO`).

### 6. HTTP contract

- `POST /v1/sessions/{id}/input`, `POST /v1/sessions/{id}/retry`,
  `POST /v1/sessions/{id}/run`: a queued run answers **202** with
  ```json
  "queued": {"position": 1, "groupId": "…", "active": 1, "limit": 2,
             "etaKnown": true, "etaSeconds": 12, "etaSamples": 4, "etaBasis": "group"}
  ```
  and `Retry-After` only when the ETA is known.
- 429 stays: tenant quota, group memory budget. 409 stays: draining/retired
  group, lease conflicts. 501 is new: typed unsupported/misconfigured.
- The queue state is read after the commit (a settlement may have promoted the
  run in between); a read failure never fails a committed input.

## Schema (guarded, re-runnable)

See `internal/store/schema.sql`: `runs.status` comment, `runs_queue_seq`,
`runs.queue_seq`, `runs.memory_bytes`, `runs_queued_fifo`, `run_duration_samples`
(+ index), and `run_duration_samples` added to `rlsTenantTables`.

## Tests

`tests.txt` in this directory is the full targeted run (sequential, isolated
throwaway PostgreSQL on `127.0.0.1:13742`). Coverage:

- MemStore: FIFO positions, promotion on release, no-oversell under concurrency,
  exactly-once promotion race, ETA unknown/known (group + tenant fallback),
  tenant hard refusal + promotion blocked by a lowered quota, re-admission of a
  queued run surviving lowered ceilings, pack headroom / unbounded fallback /
  unsupported without estimate / memory ceiling / memory head-of-line,
  re-admission cannot jump the queue, settlement hook, crash sweep.
- PG: the same queue/FIFO/no-jump/promotion paths on the real `runs` table,
  queued-not-claimable through `kernel.ClaimScope`, queued re-admission after
  lowered tenant/memory ceilings, ETA samples + pruning, tenant hard refusal,
  pack memory across groups, unsupported estimate, user.message and `EnqueueRun`
  preserving queued, placement move re-homing a queued row to the new group tail.
- Review P1 regression (2026-11-23): a queued row re-homed from an unbounded
  group into a memory-bounded one now refreshes `runs.memory_bytes` from the
  target group's estimate, so promotion cannot stall forever at the head.
  `TestQueuedRunMoveToMemoryBoundedGroupRefreshesEstimate` (MemStore) and
  `TestPGQueuedRunMoveToMemoryBoundedGroupRefreshesEstimate` (real PG) pin it.
  The previously existing move tests only targeted unbounded groups.
- HTTP e2e (real PG + production wiring): 202 + position + known ETA +
  Retry-After, concurrent inputs (3 slots / 12 inputs → 3 pending + 9 queued,
  all 12 inputs durable), crash-pending convergence with promotion, `/run`
  queueing, 501 memory-unsupported, 409 draining, tenant 429; retry answers
  202 with the same queue block when its run waits.
- Existing suites re-run green: placement, wiring, httpapi, agent, store, kernel,
  servicehost, harnessext, supervisor (sequential run in `tests.txt`).

Packages are run one `go test` at a time in `tests.txt`: several packages in one
invocation race the store package's legacy-schema fixtures against every other
package's migration guard on the shared database (pre-existing test isolation,
not this lane).

## Limitations (explicit, not simulated)

- Group OOM / process isolation remains out of scope (no cgroup enforcement, no
  fabricated `memory.peak`). Memory estimates come only from the configured
  estimator; the production wiring has none, so a memory-bounded deployment
  fails the boot until one is supplied.
- Queued entries in a **draining/retired** group are not evicted or migrated in
  this lane: they stay `queued` (promotion lists active groups only) and wait for
  an operator decision. No fake success is reported.
- The queue has no depth cap: it is bounded by clients' inputs and the durable
  ledger, not by a separate queue table.
- Test DB note: port 13741 was avoided because two long-lived processes from
  other worktrees (`/tmp/keel-app`, `/tmp/keel-identity-app`) reconnected to it
  and claimed/settled runs from the shared database. The isolated container
  `keel-w2q-pg` on 13742 was used and removed after the run.
