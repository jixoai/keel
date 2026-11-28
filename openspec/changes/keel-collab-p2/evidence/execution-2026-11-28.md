# Execution evidence — keel-collab-p2 (2026-11-28)

> Branch: `task/keel-collab-pi-unified` (keel submodule), base
> `origin/task/keel-unified-integration` = `d4254284` (lane code included).
> Worktree: `.worktrees/keel-collab-pi-unified` (keel repo).
> PostgreSQL: dedicated `postgres:18-alpine` container; `KEEL_TEST_DSN`
> pointed at it. No code outside `keel/` changed. No push to main.

## 1. Core P2

### P2-4 Assignment cross-WorkItem rebind and typed errors

- `WorkStore.ListAssignmentsBySession` added to the port and implemented by
  `MemoryWorkStore` and the PG store (`internal/store/superagent.go`); the PG
  query is tenant-scoped and ordered by `(created_at, assignment_id)`.
- `Coordinator.Assign` serialises the binding guard per `(tenant, session)` and
  scans durable assignments, so a stale session index cannot hide a binding:
  - already bound to another WorkItem → `session_already_assigned`
    (`ErrSessionAssigned`);
  - already open on the same WorkItem → `assignment_already_bound`
    (`ErrAssignmentBound`);
  - terminal assignment reuse → `assignment_terminal`
    (`ErrAssignmentTerminal`, also on `CreateSession`);
  - idempotency key with changed Bot/session → `assignment_conflict`.
- Tests: `TestAssignRejectsBoundSessionAcrossWorkItems`,
  `TestAssignSameSessionTwiceInWorkItemRejected`,
  `TestAssignIdempotentParameterConflicts`,
  `TestAssignConcurrentCrossWorkItemBindsOnce` (added here: two concurrent
  Assign calls, exactly one winner, one row),
  `TestCreateSessionRejectsBoundAndTerminalAssignment`,
  `TestCollabAssignmentBySessionAcrossWorkItems` (PG, tenant isolation).

### P2-5 Link endpoints, natural-key dedupe, cross-WorkItem read

- `Coordinator.AddLink` normalises endpoints, validates the closed endpoint
  vocabulary, rejects self-links, and proves session/work_item/assignment
  endpoints exist in the request tenant before insert.
- Deterministic id `linkID(tenant, scope, kind, from, to)`; memory and PG stores
  dedupe by the natural relation. PG adds the `collab_links_natural` unique
  index; an explicit id naming a different relation returns `link_conflict`.
- `ListLinks` returns the relations hosted by the WorkItem plus relations whose
  either endpoint is that WorkItem (readable from both sides, one row).
- Tests: `TestAddLinkIdempotentAcrossWorkItems`,
  `TestAddLinkRejectsUnknownEndpoints`, `TestCollabLinkNaturalDedupe` (PG).

### P2-6 Per-request tenant registry

- `ModeRegistrySource` / `TenantRegistries`: lazy per-tenant build, one cached
  registry per tenant, factory tenant-mismatch refusal, unknown tenant →
  `tenant_unknown`; the mutex is held during the factory (documented
  non-reentrant contract).
- `HostOptions` accepts either static `Modes` or `Registries` (mutually
  exclusive); every entry resolves `registryFor(ctx, tenantID)` and stamps the
  request tenant on the context so refusal audits carry it.
- Production assembly (`cmd/keel`): `newCollabRegistries` factory,
  `newCollabHost` injects `TenantRegistries`; the deployment tenant's registry
  is built eagerly for the superagent/capability projection and Host calls
  resolve per request.
- Tests: `TestTenantRegistriesBuildOncePerTenant`,
  `TestTenantRegistriesRejectUnknownAndMismatchedTenants`,
  `TestTenantRegistriesConcurrentFirstUseBuildsOnce`,
  `TestHostResolvesPerRequestTenantRegistry`,
  `TestHostRejectsUnknownRequestTenant`.

## 2. Checkpoint P2

- `CheckpointSummaryHash = sha256hex(summary)`; kernel (`SaveCheckpoint`) and
  both stores compute/verify it, schema carries `summary_hash`; recovery
  (`corruptCheckpointReason`) rejects a row hash mismatch and an event/row hash
  mismatch, while a pre-hash row/event is structurally validated but not
  pretended verified.
- `checkpoint_requests` is an independent idempotency ledger. `SaveCheckpoint`
  writes it in the same PG transaction for manual requests; `CheckpointByRequest`
  reads the ledger, not the prunable projection. `PruneCheckpoints` never
  touches it. Same requestId + different `upto_seq` is refused; two different
  requests may legitimately select the same cut (mem/PG parity asserted).
- TTL/Max cleanup is lazy on the run read path (`Runner.pruneCheckpoints`
  before building the model view), best-effort, newest-anchored row protected;
  events and the idempotency ledger are untouched.
- Tests: `TestCheckpointSummaryHash`, `TestCheckpointRequestSurvivesPrune`,
  `TestCheckpointRecordRoundTrip` (mem/PG, including same-seq second request),
  `TestCheckpointSkipsUnanchoredRows`, `TestCheckpointTenantIsolation`,
  `TestCorruptCheckpointSummaryHash`, `TestCompactionCarriesSummaryHash`,
  `TestManualCompactionCarriesSummaryHash`,
  `TestRunLazilyPrunesExpiredCheckpoints`.

## 3. Commands and results

Run from the keel worktree; `KEEL_TEST_DSN` was set to a dedicated local PG 18
container for the PG rows.

| Command | Result |
|---|---|
| `go build ./...` | PASS |
| `go test -race ./internal/collab/... ./internal/kernel/... ./internal/agent/...` (no DSN; PG tests skip) | PASS |
| `KEEL_TEST_DSN=... go test -race -p 1 -count=1 ./internal/collab/... ./internal/kernel/... ./internal/agent/...` | PASS |
| `KEEL_TEST_DSN=... go test -race -p 1 -count=1 ./internal/store/...` | PASS (after updating `ready_test.go` for the new `summary_hash` column contract) |
| `go test -race ./internal/collab/ -run 'TestAssignConcurrent|TestAssign|TestAddLink|TestTenantReg'` | PASS |
| `openspec validate --strict keel-collab-p2` | valid |

The earlier `internal/store` run failed once because
`TestCheckReadyRequiresEveryKernelColumn` did not create the new
`checkpoints.summary_hash` column in its isolated schema; the test was updated
to mirror the binary's contract and now covers dropping `summary_hash` as a
readiness failure.

## 4. Remaining (not claimed)

- Independent sub-agent review (P0/P1=0) is not performed by this closing pass;
  the change is deliberately left un-archived.
- Archive/disposition of the old archived change is not updated here.
