# W2/W12 recovery + guardrails slice — evidence

> Slice: automatic recovery/supervisor, resume policy, guardrails and the
> escalation ladder. Not the whole W12 catalog.
> Branch: `task/keel-w2-recovery`; commits `8d1db3a` (policy/store seam),
> `1198e8a` (recovery/guardrails wiring), plus the test/evidence commit.
> Date: 2026-11-22.

## 1. What changed

| Area | File | What |
|---|---|---|
| Policy | `internal/supervisor/policy.go` | `resume.mode=auto\|manual\|never`, `maxAttempts`, `backoff` (exponential, capped), `onExhausted=escalate\|stop`, ordered `providerFallback`; `[guardrails]` budgets. Parses a `config.toml` subset (`[resume]`, `[guardrails]`), inline JSON (`KEEL_SUPERVISOR_POLICY`) or a file (`KEEL_SUPERVISOR_POLICY_FILE`), then applies `KEEL_RESUME_*` / `KEEL_GUARDRAIL_*` overrides. Unknown keys inside the two supervised sections are refused (fail closed). |
| Orchestration | `internal/supervisor/supervisor.go` | Startup + periodic scan; auto-resume failed runs with budget, backoff and provider fallback; park `manual`/`never` runs as resumable failed rows; crash-loop budget; escalation ladder; `resume.attempt` / `supervision.escalated` facts. |
| Store seam | `internal/store/supervision.go` | Cross-tenant run scan (keyset-paginated by `(updated_at, tenant_id, session_id)` so every non-terminal run is reached), `ParkRun`, resume/escalation counting, latest selection read. Additive methods only; **no schema/table change** and no change to `RequeueRun`. |
| Provider quota | `internal/durable/generation.go` | `IsProviderExhausted` classification for `providerFallback`. |
| Parent link | `internal/harnessext/fork.go` | `DurableParentSession` reads the child's first input, so escalation survives a restart. |
| Guardrails | `internal/agent/guardrails.go` | Duplicate-call reminder/block, per-call timeout, consecutive-failure limit, token/cost budget, heap-pressure signal. Each trip commits `guardrail.tripped` and has a deterministic outcome. |
| Recovery fact | `internal/agent/runner.go` | A reclaimed lease commits a counted `resume.attempt` adoption fact (owner, epoch, reason, config hash/revision) before any new work. |
| Wiring | `internal/servicehost/{host,services}.go`, `cmd/keel/{serve,supervisor}.go` | Runner captures the guardrail policy once per attempt; `keel serve` runs a pre-boot `ParkDisabled` pass for `manual|never`, then the full supervisor startup scan before the listener, and keeps reconciling. |

Deliberate choices:

- **No second resume endpoint.** The supervisor calls `store.RequeueRun`, the
  same method the `POST /v1/sessions/{id}/resume` adapter
  (`cmd/keel/serve.go: runRequeuer`) uses. The endpoint remains the only
  external resume surface.
- **Park is a resumable failed row.** `resume.mode=manual|never` moves a
  pending/expired-running row to `status=failed` with a
  `resume.mode=…` reason. The worker claim query therefore never picks it up,
  and the explicit endpoint (`RequeueRun` accepts failed) continues it. This
  avoids a new run status and a kernel claim change.
- **The ledger is the truth.** No recovery-state table was added. The retry
  budget is counted from `resume.attempt` facts (`actor=supervisor`,
  `counted=true`); backoff uses the newest counted fact's `created_at`;
  escalation dedupe uses `supervision.escalated` facts at/after the failure
  epoch.
- **One policy document, two readers.** `supervisorPolicyReader` is created
  before `servicehost.Boot`; the runner reads `[guardrails]` per attempt and
  the supervisor reads `[resume]` per scan. A rotation changes later attempts
  only; committed facts keep their recorded config hash/revision.
- **Guardrail cancellation is run-local.** A memory/quota/cost trip cancels
  only that attempt's context; other runs and the process are untouched.
- **`bash` and `agent` are exempt from the per-call timeout by default.**
  `bash` already has the timeout-to-background contract (W2.2) and `agent`
  legitimately waits for a sub-agent; a generic timeout would break both.

## 2. Exact recovery scenarios proven

All names are test functions in the committed tree.

| # | Scenario | Proof |
|---|---|---|
| R1 | **`kill -9` after the answer committed, before settle.** A helper process claims the run, runs the real `Runner` (answer + read watermark committed), then sleeps. The parent SIGKILLs it. A second process starts the real `Worker`, waits for the dead lease, reclaims the **same run** with a new epoch and settles it. | `TestCrashThenRestartResumesWithoutADuplicateFinal` (`internal/agent/recovery_process_test.go`). Asserts: one `assistant.message` (no duplicate final), exactly one `run.finished`, at least one counted `resume.attempt` adoption fact (`reason=lease_expired`, `result=adopted`, `owner=recovering-worker`; a recovery attempt that itself loses its lease may legitimately add another adoption fact), contiguous `seq` 1..N, final status `done`. Log: `w2-w12-recovery-logs/agent-guardrails-crash.txt`. |
| R2 | **Worker reclaims an expired lease and records who/why/config.** Short first lease, successor claim reports `Reclaimed`, `RunAttempt` commits the adoption fact before reading/answering. | `TestReclaimedLeaseRecordsTheRecoveryFact` (`internal/agent/guardrails_pg_test.go`). Asserts one `resume.attempt` fact with `reason=lease_expired`, `result=adopted`, `counted=true`, owner `successor-runtime`, config hash/revision, and one answer. |
| R3 | **Startup/periodic scan auto-resumes a failed run, idempotently.** Failed run → `RequeueRun` → pending; fact committed; second scan does not add an attempt. The scan pages by keyset, so a backlog larger than one page is still fully visited. | `TestPGSupervisorResumesFailedRunAndRecordsTheFact` (`internal/supervisor/pg_test.go`), `TestScanIsIdempotentOnceRequeued`, `TestScanAutoResumesFailedRun`, `TestScanPaginatesEveryRun`, `TestPGSupervisorPaginatesEveryRun`. |
| R4 | **`resume.mode=manual` parks the run; explicit resume continues it.** A pre-boot pass parks pending/expired-running rows before the worker starts claiming; the parked row is `failed` with a policy reason; `ClaimScope` returns `ErrNoRunnable`; `RequeueRun` (the `/resume` store path) makes it claimable. | `TestPGManualPolicyParksAndExplicitResumeContinues`; unit: `TestScanManualParksPendingRun`, `TestParkDisabledRunsBeforeBoot`. |
| R5 | **`resume.mode=never` never auto-recovers.** The pre-boot and normal scans park pending/expired-running rows with `policy_never`; an already-failed row is not retried; a live lease is never touched. | `TestScanNeverParksAndDoesNotResumeFailures`, `TestParkDisabledRunsBeforeBoot`, `TestParkDisabledIsANoopForAuto`. |
| R6 | **Budget, backoff and crash loops.** Backoff skips a scan before the next deadline; `maxAttempts` exhaustion escalates; a crash loop at the budget is parked/escalated instead of being reclaimed forever. | `TestScanRespectsBackoff`, `TestScanCrashLoopExhaustsInsteadOfReclaiming`, `TestBackoffDelay`. |
| R7 | **Escalation ladder.** Exhaustion: parent via a task.note on the parent card; no parent → owner via `notify.Service`; `onExhausted=stop` records a stop fact; one escalation per failure epoch; super-agent is an optional L3 seam only when the parent rung did not take the hand-off. | `TestScanEscalatesToParentAtExhaustion`, `TestScanEscalatesToOwnerWithoutParent`, `TestScanOnExhaustedStop`, `TestScanEscalatesOncePerFailureEpoch`, `TestPGEscalationIsDurable`. |
| R8 | **Provider fallback.** A quota-exhausted failure with `providerFallback` writes the fallback `selection` into the `resume.attempt` fact; the runner already resolves the newest `selection` fact, so the next attempt uses it. An invalid fallback is recorded (`fallbackError`) but does not block the resume. | `TestScanProviderFallbackSelection`, `TestScanProviderFallbackInvalidStillResumes`; selection resolution is the existing `internal/agent/model.go: runSelection`. |
| R9 | **Config rotation affects later attempts only.** Two failures under different policy/config versions record different `configRevision` values; the first fact is unchanged after rotation. | `TestConfigRotationAffectsOnlyLaterAttempts`; policy reader wiring in `cmd/keel/supervisor.go`. |
| R10 | **Operator cancellation is not auto-resumed.** A failed run whose last error is `context canceled` is counted as cancelled and skipped. | `TestScanSkipsOperatorCancelledRun`. |

Recovery facts:

- `resume.attempt` payload: `actor`, `reason`, `result`, `counted`, `attempt`,
  `statusBefore`, `lastError`, `configHash`, `configRevision`,
  `configGeneration`, optional `selection` / `providerFallback` /
  `fallbackError`, `at`.
- `supervision.escalated` payload: `actor`, `level` (`parent|owner|stop|super-agent`),
  `reason`, `result`, `attempts`, `maxAttempts`, `statusBefore`, `lastError`,
  `parentSessionId`/`parentResult`/`parentError`,
  `notificationId`/`notificationStatus`/`notificationError`,
  `superAgentResult`/`superAgentError`, config identity, `at`.
- Both use the registered system lane (`CommitRequest.System`) and distinct
  `supervisor:<session>:resume:<n>` / `supervisor:<session>:escalate:<n>`
  dedupe keys (no `tool:` prefix).

## 3. Guardrails proven

| Guardrail | Outcome | Audit + test |
|---|---|---|
| Duplicate tool call (`duplicate_call`) | The call over the limit is **blocked** with an explanatory error result; the model can change course. | `TestGuardrailDuplicateCallBlocksAndAudits`, `TestGuardrailDuplicateResetsOnDifferentCall`, `TestGuardrailDuplicateWhitespaceIsTheSameCall`, `TestGuardrailAccessRefusalIsNotCountedAsDuplicate`. |
| Per-call timeout (`call_timeout`) | The single call is aborted with a timeout error result; the run continues. `bash`/`agent` exempt by default. | `TestGuardrailCallTimeoutAbortsTheCallAndAudits`, `TestGuardrailTimeoutExemptionKeepsRegistry`. |
| Consecutive failures (`consecutive_failures`) | The attempt is **aborted** after the limit; a success resets the streak. | `TestGuardrailConsecutiveFailuresAbortsRun`, `TestGuardrailSuccessResetsFailureStreak`. |
| Token budget (`token_budget`) | The attempt is aborted after the committed assistant message is recorded. | `TestGuardrailTokenAndCostBudgets/tokens`. |
| Cost budget (`cost_budget`) | The attempt is aborted using the configured USD/MTok rates. | `TestGuardrailTokenAndCostBudgets/cost`. |
| Memory pressure (`memory_pressure`) | Only the tripping run is aborted; a second guard proves unrelated runs keep running. | `TestGuardrailMemoryPressureAbortsOnlyItsRun`, `TestGuardrailMemoryPressureIsAuditedInTheLedger` (real kernel/store, asserts a `guardrail.tripped` event and a non-done run). |
| Disabled/zero policy | The guard is nil; the run path is unchanged. | `TestGuardrailDisabledPolicyKeepsRunPathUnchanged`. |
| Audit write failure | The deterministic outcome (block) still applies; the failure is logged, not swallowed into an open surface. | `TestGuardrailCommitFailureDoesNotPanic`. |

Guardrail fact payload: `actor`, `trigger`, `outcome`
(`blocked|tool_aborted|run_aborted`), `detail`, `attempt` (lease epoch),
`configHash`, `configRevision`, optional tool/call/count/limit/tokens/cost/heap,
`at`. Keys are `supervisor:<session>:<trigger>:<epoch>:<n>`.

## 4. Verification commands and results

Local test database: Docker `keel-pg` (PG 18) on `127.0.0.1:13741`; each
package run used a disposable database (`keel_w2_all` / `keel_agent_test`).
The supervisor package creates and drops its own database in `TestMain`, because
its scan is cross-tenant by design and must not observe a developer database's
leftover runs.

```bash
# Guardrails + crash/restart
KEEL_TEST_DSN=<local test DSN> go test ./internal/agent \
  -run 'TestGuardrail|TestReclaimedLeaseRecords|TestCrashThenRestart' -count=1 -v
# => PASS; TestCrashThenRestartResumesWithoutADuplicateFinal (7.58s)

# Supervisor unit + PG
KEEL_TEST_DSN=<local test DSN> go test ./internal/supervisor \
  -run 'TestScan|TestConfigRotation|TestPG|TestParsePolicy|TestLoadPolicy|TestBackoff|TestPolicy' -count=1 -v
# => PASS

# Full affected suite (serialized: package tests share the DSN)
KEEL_TEST_DSN=<local test DSN> go test \
  ./internal/agent ./internal/supervisor ./internal/servicehost ./cmd/keel \
  ./internal/store ./internal/harnessext ./internal/durable -count=1 -p 1
# => ok agent 82.895s; supervisor 2.405s; servicehost 5.505s; cmd/keel 12.113s;
#    store 21.537s; harnessext 26.133s; durable 0.413s

go vet ./internal/supervisor ./internal/agent ./internal/store \
  ./internal/durable ./internal/harnessext ./internal/servicehost ./cmd/keel
# => clean
```

Raw logs committed under
`openspec/changes/keel-harness-v2/evidence/w2-w12-recovery-logs/`:
`supervisor-unit-pg.txt`, `agent-guardrails-crash.txt`, `full-suite.txt`.

Negative controls exercised:

- Duplicate final is caught by the crash fixture's exact-one
  `assistant.message`/`run.finished` assertion.
- `resume.mode=never`/`manual` are caught by the park tests: a worker claim on a
  parked row returns `ErrNoRunnable`.
- Rotation rewriting history is caught by re-reading the first fact after the
  config/version change.
- Killing unrelated runs is caught by the two-guard memory test.
- Invalid policy is fail-closed to `manual` (`supervisorPolicyReader.policy`);
  unknown keys in `[resume]`/`[guardrails]` are rejected by
  `TestParsePolicyRejectsInvalidValues`.

## 5. Walkthrough / review notes

- Startup order: `servicehost.Boot` starts the worker, so a `manual|never`
  policy needs to win earlier. `parkDisabledBeforeBoot` runs before Boot (a
  no-op for `auto`) and parks only pending/expired-running rows without touching
  a live lease. After Boot, `newRecoverySupervisor` runs the full startup `Scan`
  before the HTTP listener opens. A queued run under `auto` may be claimed in
  the milliseconds before that full scan; that is harmless (the worker's claim
  is the execution, and a reclaimed lease records the recovery fact). Failed-run
  reconciliation and escalation run in the explicit startup `Scan`.
- The supervisor never claims/renews/finishes a lease. It only reads rows,
  calls the shared `RequeueRun`, parks policy-disabled rows, and commits facts.
- `ParkRun` never touches a live lease: a policy flag must not kill work another
  process is executing.
- Operator cancellation (`run.finished.status=cancelled` leaves
  `last_error=context canceled`) is excluded from auto-resume.
- `onExhausted=escalate` tries parent, then owner; L3 super-agent is an optional
  injected seam, not a new kernel path. The first three rungs are wired in
  `cmd/keel/supervisor.go`; no `keel.super-agent` plugin entity was added.
- No changes to `openspec/changes/keel-harness-v2/tasks.md`, `web/**`,
  `internal/pluginpkg/**`, the plugin catalog, `cmd/r1diff`,
  `plugins/keel.oar/**`, or `internal/harnessapi/plugin_bridge.go`.
- No new endpoint and no route-matrix change; `POST /v1/sessions/{id}/resume`
  keeps its existing contract.
- Resource hygiene: test processes created by the crash fixture were SIGKILLed
  or their worker process was killed after the run settled; the disposable
  PostgreSQL databases are dropped after the final verification run.

## 6. Remaining work (registered, not done in this slice)

W2 (as required by the slice brief):

- Group-level run queue with ETA (current contract remains explicit 429 +
  `Retry-After`; no queue).
- Memory-budget-aware packing across groups (`pack` currently one group /
  process-level concurrency).
- layerfs gRPC service (ADR 0001 Phase B); in-process Go CAS remains the base.
- Group OOM/process isolation and per-group memory ceiling enforcement.
- `keel.supervisor` as a plugin-catalog entity (this slice delivers the
  equivalent package/policy surface, not the marketplace row).
- Pending-recovery UI listing (`resume.mode=never` sessions are durable and
  queryable, but no `web/**` panel was added in this slice).
- `keel.super-agent` plugin entity (only the L3 capability seam exists).
- Full `config.toml` parser: this slice parses only the `[resume]` and
  `[guardrails]` subset and ignores other tables; the agent's complete
  config.toml vocabulary remains a separate W0/W2 item.
