# W2 placement first vertical slice (lane H2A2)

- generated: 2026-11-20T13:34Z, refreshed 2026-11-20T13:47Z (evidence directory name requested as `2026-11-21`)
- base: `889c01b` (main) + WIP `990e586` (schema/rls + placement types)
- worktree: `.worktrees/keel-w2-placement`
- commits: `d7c799f`, `af35f3d`, `9d1fc6e`, `6e79f83`, `8958e6b`, `3f3774a`, `7e7ab9e`
  (+ final evidence commit)

## Scope delivered

### 1. Schema + RLS + migration test

- `internal/store/schema.sql:919` `groups` (tenant FK, status CHECK active/draining/retired,
  unique `(tenant_id, name)`, `max_concurrent_runs`, `memory_limit_bytes`).
- `internal/store/schema.sql:960` `session_placement` (PK `(tenant_id, session_id)`,
  `epoch > 0`, `lease_until NOT NULL`, group FK).
- `internal/store/schema.sql:990` `runs.group_id` + tenant-consistent FK
  (`runs_group_fk`) + active partial indexes `runs_active_group` / `runs_active_tenant`.
  The runs row remains the only active-run account; there is no second slot ledger.
  The column add is catalog-probed before `ALTER TABLE`, so replaying schema.sql on
  every boot/test does not take ACCESS EXCLUSIVE on the hot runs table.
- `internal/store/rls.go:87-88` both new tables are in `rlsTenantTables`, so they get
  `ENABLE + FORCE ROW LEVEL SECURITY` and `tenant_isolation` from the existing migration.
- Tests: `internal/store/placement_schema_test.go:13` (tables, FORCE RLS, policy, FK,
  indexes, cross-tenant FK refusal) and `:101` (tenant-scoped read/write isolation).
  Result: PASS (`test-store-placement.txt`).

### 2. Strategies (pack / user / pin)

- `internal/placement/service.go:359` `resolveStrategyGroup`:
  `pack` -> `pack-default` (single group; memory-budget multi-group packing deferred),
  `user` -> `user` group, `pin` -> a request group or `Options.PinGroupID`
  (configuration); neither present is `ReasonPinRequiresGroup`, mapped to 409.
- `EnsureGroup` is idempotent on `(tenant_id, name)` so concurrent strategists converge
  (`internal/placement/pg.go` / `mem.go`).
- Test: `internal/placement/service_test.go:49` PASS.

### 3. Lease claim: epoch monotonic + lease_until conditional write

- Every write path runs inside `inTx`, which retries only the transient
  `40P01/40001/55P03` failures. This was a self-review finding: `CreateGroup` as a bare
  statement deadlocked against a concurrent schema/RLS migration when placement and store
  tests ran in parallel; the focused combined run now passes three rounds in a row.
- `internal/placement/pg.go:208` `ClaimPlacement` and `:337` `AdoptPlacement` run one
  transaction that locks the placement row `FOR UPDATE`, compares the stored epoch with
  `expectedEpoch`, checks `lease_until > now()`, then writes with both fences:
  `AND epoch = $expected AND (lease_until <= now() OR worker_id = $worker)` (and a
  unique-PK insert for the create case).
- Outcomes (typed `*Conflict`, edge maps to 409):
  - stale epoch -> `epoch_mismatch` (carries expected/current epoch, holder, lease);
  - live lease held by another worker -> `lease_held`;
  - move to another group while the old lease is live -> `lease_held` (claim) /
    `already_placed` (explicit join);
  - draining/retired/missing group -> `group_draining` / `group_unavailable`.
- `Seek` (`service.go:224`) returns a plan only: `DrainRequired` from the live lease,
  `NextEpoch = epoch+1`, `CopyProcessState=false`, rebuild from seq 0.
- Evidence:
  - `TestPGConcurrentClaimsSingleWinner` (`pg_test.go:135`): 24 concurrent claims for one
    session split across two groups -> exactly 1 winner, 23 typed conflicts, exactly
    1 `session_placement` row and 1 `runs` row; a second `AdmitRun` is idempotent
    (`AlreadyActive=true`, still 1 run). This is the "no second concurrent run" proof.
  - `TestPGClaimEpochAndLiveLease` (`pg_test.go:67`): expected 0 vs stored 1 -> conflict;
    live takeover -> `lease_held`; same holder renewal -> epoch 2; stale epoch 1 ->
    conflict current 2; 120 ms TTL + expiry -> new worker takes over at epoch 2; leave
    by a non-holder is a conflict and a successful leave removes the row.
  - `TestLeaveEpochGuard` (`service_test.go`): leave checks epoch and holder, removes the
    placement fact, and a second leave is `ErrPlacementNotFound`.

### 4. Group slots + tenant quota (DB side, cross-group)

- Admission choice: **reject with 429 + Retry-After**, not queue. Written down in the
  package doc (`internal/placement/types.go`), `Admission` doc, and tasks.md. Rationale:
  the spec allows either; a typed refusal is the smaller surface and the client/UI can
  show the reason. The slot is reserved by the pending `runs` row, so a refusal never
  silently drops work.
- `internal/placement/pg.go:487` `AdmitRun`:
  - one `pg_advisory_xact_lock` per tenant serialises every admission of that tenant,
    including admissions into different groups;
  - counts group slots and tenant runs from `runs WHERE status IN ('pending','running')`
    in the same transaction that upserts the pending run row (kernel claim flips it to
    running without changing the count; kernel finish frees it);
  - capacity refusal is `*CapacityError` with `Reason=group_slots_full|tenant_quota_exceeded`
    and `RetryAfter` = nearest running lease in the refused scope, clamped to >= 1s;
  - a session with an already-active run is admitted idempotently (`AlreadyActive=true`)
    and does not add a row.
- Evidence:
  - `TestPGRunSlotGroupLimit` (`pg_test.go:206`): group limit 1 -> second session gets
    `group_slots_full`, `Limit=1 Active=1`, `RetryAfter >= 1s`; freeing the run admits it.
  - `TestPGTenantQuotaAcrossGroupsNoOversell` (`pg_test.go:251`): limit 4 across two
    groups -> 4 admitted, 5th `tenant_quota_exceeded`, DB count 4.
  - `TestPGConcurrentAdmissionsNoOversell` (`pg_test.go:290`): 30 concurrent admissions
    across two groups with tenant limit 5 -> exactly 5 admitted / 25 capacity errors and
    DB count 5; then 20 concurrent admissions into a group with 3 slots -> exactly
    3 admitted / 17 capacity errors and DB count 3.
  - `TestPGDrainingGroup` (`pg_test.go:383`): existing placement renews, new
    placement/run into a draining group is refused, retired is terminal.
  - Mem implementation mirrors the same semantics; `-race` suite PASS (`test-race.txt`).

## Gates run

| gate | result | log |
|---|---|---|
| `go build ./...` | PASS | `build.txt` |
| `go vet ./internal/placement ./internal/store ./internal/architecture` | PASS | `vet.txt` |
| `go test ./internal/placement/ -count=1 -v` (DSN set) | PASS, 17 tests | `test-placement.txt` |
| `go test ./internal/store/ -run TestPlacement` (DSN set) | PASS, 2 tests | `test-store-placement.txt` |
| `go test -race ./internal/placement/` (DSN set) | PASS | `test-race.txt` |
| `go test -p 1 ./...` (DSN set, only KEEL_TEST_DSN exported) | PASS, exit 0 | `test-full.txt` |
| `openspec validate keel-harness-v2 --strict` | valid | `openspec-validate.txt` |

The first full run exported the whole `.env.local`, which leaked the real
`KEEL_SERVE_PASSWORD`/`KEEL_BASE_URL` into `cmd/keel` tests and made their fake server reject
login; that run is not a code gate. The second full run, with only `KEEL_TEST_DSN`, had three
timing-sensitive failures under the default cross-package parallelism
(`kernel/TestClaimDoesNotResurrectSettledRun`, `agent/TestSecondCompactionAnchorsFromCheckpoint`,
`agentapi/TestMCPAppRejectionAuditSampling`); each passes when run alone (`test-full-parallel.txt`
records the run). The serial run (`-p 1`) is the clean full gate: 56 packages ok, 0 failed,
exit 0, run at the final code (`7e7ab9e`).

## Not done (honest gaps)

- **Queue / ETA**: not implemented by choice; the slice uses 429 + Retry-After. No queue
  position or ETA exists.
- **Runtime wiring**: `internal/agent` worker and `internal/quota.Limits` still use the
  process-local semaphore/counter; they do not call `placement.AdmitRun` yet. Parent and
  child sub-agent runs therefore still occupy only the process-wide worker semaphore.
  Wiring is a separate integration step (the service API and DB semantics are ready).
- **pack memory-budget multi-group packing**: pack currently resolves one `pack-default`
  group; `memory_limit_bytes` is stored but not used to spread sessions across groups.
- **Group OOM / process isolation**: explicitly out of this lane; drain/SIGTERM/process
  supervision remain as previously recorded in tasks.md.
- **HTTP edge**: `Conflict` -> 409 and `CapacityError` -> 429 are documented on the types
  and exercised at the service layer; no HTTP handler was added in this lane.

## Self-review notes

- Found and fixed: `CreateGroup`/`SetGroupStatus`/`ReleasePlacement`/`ReleaseRun` were
  bare statements with no transient-error retry; concurrent schema/RLS migration
  produced a real `40P01` deadlock in a combined test run. All writes now go through
  `inTx`; the combined placement+store+architecture suite passed three consecutive runs.
- Mem and PG implementations were walked against each other for: expected-epoch 0
  create, stale epoch, live holder, expired takeover/group move, draining renewal vs new
  placement, idempotent re-admission, release only of a pending run, capacity Retry-After
  scope (group vs tenant). The pg/mem test suites assert the same outcomes.
- The admission count is `pending+running`, deliberately conservative: a reserved but not
  yet claimed run holds the slot. A crash between admission and kernel claim leaks a
  pending row; `ReleaseRun` frees it on the graceful abandon path. Crash recovery of
  pending admission rows is NOT implemented and is recorded as a gap.
- The tenant quota is read outside the DB transaction (`Options.TenantRunLimit`, fail
  closed). The count is inside the transaction, so a config change can only change the
  limit, never the atomicity of the count.
