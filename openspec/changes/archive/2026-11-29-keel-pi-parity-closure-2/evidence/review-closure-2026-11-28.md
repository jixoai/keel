# Review closure — P1/P2 fixes (2026-11-28, second pass)

> Review verdict under repair: **FAIL, P0=0, P1=5**. This file records the
> per-item closure evidence. It is **not** a self-approval: the change remains
> un-archived and awaits independent re-review.
>
> Code commit for the P1/P2 fixes and for the capacity/chaos binaries:
> `3c7bd7897237d790a25fbbfe136c9f08bc0f5d7b`
> (`task/keel-collab-pi-unified`). A follow-up commit
> `7948226f2cfd105e98f4afc2b255d27ca735ebad` only fixes the request-ledger row
> timestamp on a replay of an old seq; it is covered by kernel tests and is not
> exercised by the capacity/chaos workload (no manual compaction there).
> Capacity/chaos `subject.json` bind the measured `3c7bd789`. The change
> remains un-archived and awaits independent re-review.

## P1 items

### P1-1 MCP approval could replay an answered decision

- Fix: `cmd/keel/mcp_approval.go` — the decision request id is now
  `mcp-approval:v2:<tenant>:<session>:<attempt>:<toolCallId>:<sha256(arguments)>`.
  `attempt` comes from `agentcore.MCPAttemptFromContext`; the argument digest
  comes from the exact `toolsurface.MCPApprovalRequest.Arguments` bytes. A new
  attempt or a changed payload derives a different request id, so
  `decisions.Open` cannot return an already-answered decision from an earlier
  attempt.
- Tests: `TestMCPApprovalRequestIDBindsAttemptCallAndArguments` (same attempt
  idempotent, new attempt/different args distinct),
  `TestMCPApprovalDoesNotReplayAcrossAttempts` (attempt A answered; attempt B
  blocks and times out instead of inheriting the answer; broker holds two
  decisions).
- Verify: `go test -race ./cmd/keel/ -run 'TestMCPApproval'` → PASS.

### P1-2 Approval prompt/provenance lacked arguments

- Fix: `mcp_approval.go` — `mcpArgumentSummary` renders a bounded
  (`mcpApprovalSummaryLimit = 480` runes) JSON summary; sensitive keys
  (`token`, `secret`, `password`, `api_key`, `authorization`, `cookie`,
  `credential`, `private_key`, `access_key`, `bearer`, `session_key`,
  `client_secret`) become `[redacted]`; long strings/arrays are truncated and
  the `argumentsRedacted` flag is set. The prompt includes the summary; the
  provenance row carries `arguments`, `argumentsDigest` (SHA-256 of the exact
  payload), `argumentsRedacted`, `attemptId`, `toolCallId`. The raw payload is
  never logged.
- Test: `TestMCPApprovalPromptAndProvenanceCarryBoundedRedactedArguments`
  (benign path visible; `super-secret-value` absent from prompt and provenance;
  digest exact; attempt/call ids recorded).
- Verify: same `TestMCPApproval` command → PASS.

### P1-3 `compaction_tasks` had no RLS/readiness

- Fix: `internal/store/rls.go` adds `compaction_tasks` to `rlsTenantTables`;
  `internal/store/ready.go` adds it to `requiredTables`.
- Test: `internal/store` `TestCompactionTasksSchemaAndRLS` asserts
  `relrowsecurity` + `relforcerowsecurity`, exactly one `tenant_isolation`
  policy, membership in `rlsTenantTables`, and membership in `requiredTables`.
- Verify: `KEEL_TEST_DSN=... go test -race ./internal/store/ -run TestCompactionTasksSchemaAndRLS`
  → PASS.

### P1-4 `checkpoint_requests` had no TTL/row cap

- Fix: new independent retention seam
  `kernel.CheckpointRequestPruneRequest` / `CheckpointRequestPruner` and
  `Kernel.PruneCheckpointRequests`; `MemStore` and `PGStore` implement it.
  `Runner.pruneCheckpoints` calls it with `checkpointRequestMax = 1000` and
  `checkpointRequestTTL = 30 days`; projections/events are untouched. A pruned
  projection does not delete its request row, and the request prune does not
  touch projections.
- Tests: `TestPruneCheckpointRequestsBoundsLedger` (mem+PG: cap keeps newest 2,
  TTL removes the remaining 4, `LoadCheckpoint` still returns seq 7, missing
  session = `ErrNotFound`, negative bound = `ErrInvalidRequest`);
  `TestMissingSessionParity` covers the ledger read/write paths.
- Verify: `KEEL_TEST_DSN=... go test -race ./internal/kernel/ -run 'TestPruneCheckpointRequests|TestMissingSessionParity'`
  → PASS.

### P1-5 CompactionTask attempts/retry/lease claimed but not implemented

- Fix: `internal/agent/manual_compact.go` increments `attempt` before each
  summary (`task.Attempt+1`, or 1 on the first try); a failed summary persists
  `phase=retry`, the incremented attempt and `retry_until = now + 30s`
  (`manualCompactRetryDelay`, zeroed in tests). An immediate replay returns
  `ErrManualCompactRetry` without a model call; after the deadline the pinned
  range resumes and increments the attempt again. `spec.md`/`design.md`/tasks
  now state the architectural truth: there is no independent task-lease row;
  the kernel session/run lease is the fencing authority (documented in
  `RUNBOOK.md` §1).
- Tests: `TestManualCompactRetryBackoffAndAttempts` (attempt=1, phase=retry,
  future deadline, no second model call before it, resume after deadline,
  task consumed), `TestManualCompactResumesPinnedSummarizePhase` (attempt=1,
  retry phase, resume keeps pinned range), `TestCompactionResumeKeepsLogContiguousAndSingleAnswer`.
- Verify: `KEEL_TEST_DSN=... go test -race ./internal/agent/ -run 'TestManualCompact(Retry|Resumes)|TestCompactionResume'`
  → PASS.

## Priority P2 items

| Item | Fix | Test |
|---|---|---|
| skill `mcp__server__tool` widened to all MCP | `internal/agent/skill_tools.go`: policy stores `mcpToolScope{server,tool}`; `allowsCall` parses the aggregate tool's `server`/`tool` arguments. `mcp__server__*` = server-wide; bare `mcp`/`mcp*` = all; per-tool grants do not permit `list`/`desc` or another server/tool. | `TestSkillToolPolicyMCPCallScope` |
| Assignment pending-bind race | `internal/collab/host.go`: idempotency-key lock acquired before the per-session binding guard, so two retries of one key with different worker sessions cannot both complete the open row. | `TestAssignConcurrentSameKeyBindsOnce` |
| Link UUID normalization | `normalizeLinkEndpoint` parses session/work_item/assignment UUIDs and stores `uuid.String()`; casing/hyphenation cannot bypass the natural key. | `TestAddLinkCanonicalizesUUIDEndpoints` |
| usage cancel/exception settled fake zero | `internal/agent/usage_stream.go`: `settle` is a no-op when no provider usage was observed; the marker stays `unaccounted` (possibly billed, unknown usage). An explicit empty usage object still settles zero (measured). | `TestUsageStreamDoesNotSettleWithoutUsage`, `TestUsageStreamSettlesExplicitZeroUsage` |
| Actions-only tasks checked without running Actions | `keel-pi-parity-closure-2/tasks.md` §5 both items are unchecked with the local-run evidence noted. | `openspec validate --strict` |
| capacity artifact not SHA-bound | `evidence/capacity-2026-11-28/subject.json` and `...-64/subject.json` record `sourceCommit=3c7bd789...`, build/container/provider/command/gate. | files present in evidence commit |
| mem/PG missing-session parity | `internal/kernel/pg.go`: `CheckpointByRequest`, `LoadCompactionTask`, `ListCompactionTasks` now answer `ErrNotFound` for a missing session, matching `MemStore`. | `TestMissingSessionParity` |

## Verification (this pass)

| Command | Result |
|---|---|
| `go build ./...`, `go vet` on touched packages | PASS |
| `KEEL_TEST_DSN=... go test -race -p 1 ./internal/{collab,kernel,agent,mcp,toolsurface,usage,servicehost,store}/...` | PASS |
| `KEEL_TEST_DSN=... go test -race -p 1 ./cmd/keel/... ./cmd/runtime/...` | PASS |
| conformance golden | PASS — `1318 pass / 0 fail / 3 skip`, `GOLDEN: PASS` (floor refreshed to 1318) |
| 2GB/1000 capacity | PASS — peak 595.7 MB, 1000 sessions, 0 hard SSE errors |
| 2GB/1000/64 capacity | PASS — `runsActiveMax=64`, peak 545.9 MB, 0 hard SSE errors |
| 600s chaos (20 SIGKILLs) | PASS — 24699 events, 0 lost events, 0 duplicate final replies; kill interval 29.990–30.030s |
| `openspec validate --strict` both changes | PASS |

## Second re-review batch (P1-A, P1-B, key P2)

> Code commit: `f00b36f275e63eefc66418528f5ad25833d84175`
> (on top of the evidence commit `7e8d663e`; the earlier capacity/chaos
> artifacts remain bound to `3c7bd789` and were not rerun for this batch).

### P1-A — answer path replaced the open provenance Context

- `internal/approval/approval.go`: `RecordAnswer` now merges
  `existing.Context` with the answer context (non-empty answer keys override,
  open keys survive); `RecordOutcome` does the same for timeout/cancel.
  Repeated same-answer calls persist newly added answer context without
  changing the answer identity.
- Tests: `TestAnswerMergesOpenContext` (unit),
  `TestApprovalAnswerKeepsOpenContext` (real harnessapi HTTP answer path),
  `TestPostgresProvenanceRoundTrip` (PG: open args/digest/redacted/attempt/call
  survive the answer alongside `remoteAddr`).
- Verify: `KEEL_TEST_DSN=... go test -race ./internal/approval/ ./internal/harnessapi/`
  → PASS.

### P1-B — `mcp__*__<tool>` widened to every MCP call

- `internal/agent/skill_tools.go`: `parseMCPAllowedTool` keeps
  `mcp__*__read_file` as `{server:"*", tool:"read_file"}`; `allowsCall` matches
  any server for that exact/glob tool. `mcp__server__` / `mcp__` / `mcp__*__`
  are malformed and grant nothing. Only bare `mcp`/`mcp*` is `mcpAll`.
- Tests: `TestSkillToolPolicyMCPCallScope` — read_file passes on multiple
  servers; delete_file is refused on every server; malformed spellings leave
  the aggregate tool hidden.
- Verify: `go test -race ./internal/agent/ -run TestSkillTool` → PASS.

### Key P2

| Item | Fix | Test |
|---|---|---|
| P2-1 | `CompactNow` defers the detached request/task ledger prune on every exit (success, failure, replay) | `TestManualCompactPrunesRequestLedgerOnManualPaths` |
| P2-2 | retry row written via `saveManualCompactRetry` on `WithoutCancel` + 5s | `TestManualCompactCancellationPersistsRetryTask` |
| P2-4 | `mapManualCompactError` maps `ErrManualCompactRetry` → `ErrCompactBusy` (HTTP 409/command busy); `compaction_tasks` 100-row cap via `PruneCompactionTasks` (mem+PG) | `TestMapManualCompactErrorRetryIsTransient`, `TestPruneCompactionTasksCapsRows` |
| P2-5 | map keys sorted before the 32-field bound; same prompt on retry | `TestMCPArgumentSummaryDeterministicLargeMap`, `TestMCPApprovalSameAttemptLargeArgsDoesNotConflict` |
| P2-8 | request `created_at` is first-seen in mem and PG; replay does not reset | `TestCheckpointRequestCreatedAtIsFirstSeen` |
| P2-9 | comment/RUNBOOK state summary and ledger read the same provider values | `TestUsageStreamRecordsRequestLifecycle` |

### Verification for this batch

| Command | Result |
|---|---|
| `go build ./...`, `go vet` on touched packages | PASS |
| `KEEL_TEST_DSN=... go test -race -p 1 ./internal/approval/... ./internal/harnessapi/... ./internal/kernel/... ./internal/agent/... ./cmd/keel/...` | PASS |
| `openspec validate --strict` both changes | PASS |

## Not claimed

- Independent re-review / P0/P1=0 and archive are **not** done here.
- The two GitHub Actions jobs were wired but not executed on GitHub from this
  environment; their local equivalents (conformance golden, 600s chaos) were
  run and recorded above.
