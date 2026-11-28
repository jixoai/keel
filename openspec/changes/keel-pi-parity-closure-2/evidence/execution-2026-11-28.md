# Execution evidence — keel-pi-parity-closure-2 (2026-11-28)

> Branch: `task/keel-collab-pi-unified` (keel submodule), base
> `origin/task/keel-unified-integration` = `d4254284`.
> Worktree: `.worktrees/keel-collab-pi-unified` (keel repo).
> **Code commit measured by capacity/chaos:**
> `3c7bd7897237d790a25fbbfe136c9f08bc0f5d7b`.
> Follow-up `7948226f2cfd105e98f4afc2b255d27ca735ebad` fixes only the
> request-ledger row timestamp on a replay of an old seq (kernel tests; no
> manual compaction in the capacity/chaos workload). Capacity artifacts bind
> `3c7bd789` in their `subject.json`.
> PostgreSQL: dedicated `postgres:18`/`postgres:18-alpine` containers;
> PG tests use `KEEL_TEST_DSN`. No code outside `keel/` changed; no push to main.

## 0. Review closure

The independent review returned **FAIL, P0=0, P1=5**. Every P1 and the priority
P2 items are fixed at `3c7bd789`; the per-item evidence, files and tests are in
`evidence/review-closure-2026-11-28.md`. This file records the implementation
and the run results. It is not a self-approval: the change remains un-archived
and awaits independent re-review.

## 1. CompactionTask / checkpoint

- Durable `compaction_tasks` state (`select` / `summarize` / `retry`) with
  `first_kept_seq`, `tail_seq`, `tokens_before`, `policy_version`, `attempt`,
  `retry_until`. The session kernel lease is the fencing authority; there is no
  independent task-lease row (spec/design/tasks now say this explicitly).
- A failed summary increments `attempt`, persists `phase=retry` and
  `retry_until`; an immediate replay is refused with `ErrManualCompactRetry`
  without a model call; after the deadline the pinned range resumes and the
  attempt increments again.
- Manual recovery: a committed compaction event with a lost projection is
  restored by requestId without a second model call; an interrupted summary
  resumes the pinned range (`CompactAt`) instead of selecting a new cut.
- Automatic recovery: a committed event is projected on the next attempt; an
  uncommitted entry marker is discarded without touching the log.
- `checkpoint_requests` is bounded independently: `PruneCheckpointRequests`
  keeps at most 1000 newest request rows and expires rows older than 30 days on
  the run read path; projections and events are untouched.
- Tests: `TestManualCompactRecoversCommittedEntry`,
  `TestManualCompactResumesPinnedSummarizePhase`,
  `TestManualCompactRetryBackoffAndAttempts`,
  `TestRecoverAutomaticCompactionProjection`,
  `TestCompactionResumeKeepsLogContiguousAndSingleAnswer`,
  `TestPruneCheckpointRequestsBoundsLedger`, `TestMissingSessionParity`,
  `TestCompactionTaskCheckpoints`, `TestCompactionTaskValidation`.

## 2. Skills allowed-tools

- Execution-time intersection of the attempt's registry with the union of the
  active skills' declared allowed tools (`registry.Subset`); the execution gate
  refuses out-of-set calls with `skill_tool_not_allowed`, a typed,
  model-readable message and structured details.
- MCP spellings are scoped, not widened: `mcp__server__tool` permits only that
  call through the aggregate `mcp` tool; `mcp__server__*`/`mcp__server` permits
  that server; bare `mcp`/`mcp*` permits all. A per-tool grant does not permit
  `list`/`desc` or another server/tool.
- Absence of a declaration is unrestricted; a sibling unrestricted skill does
  not widen an active restricted set; `Bash(...)` scopes name the tool.
- Tests: `TestSkillToolPolicy` (7 cases), `TestSkillToolPolicyMCPCallScope`,
  `TestSkillToolPolicyReadsLoadedPin`, `TestSkillToolPolicyIntersectsSurface`,
  `TestSkillToolGuardRefusesTyped`,
  `TestSkillToolGuardComposesAfterAuthorization`.

## 3. MCP lifecycle and destructive approval

- Pool: one slot per server; health probe on borrow when due and by the
  monitor; exponential backoff 1s…30s per server; concurrent borrowers share
  one reconnect.
- `EnsureConnected` checks the backoff **before** probing a disconnected slot;
  the monitor calls `EnsureConnected` only. This fixes the earlier
  probe-reschedules-backoff starvation (`TestEnsureConnectedReconnectsAfterBackoff`).
- Destructive approval: `annotations.destructiveHint` (or the small verb-family
  fallback) opens a durable `approval.confirm` decision (allow/deny, 5-minute
  timeout); execution only after `allow`. The decision id binds attempt +
  provider `toolCallId` + the SHA-256 argument digest, so an answered decision
  cannot replay into a later attempt or a changed payload. The prompt and
  provenance carry a bounded (~480-rune) redacted argument summary and the
  exact-payload digest; raw arguments are never logged.
- `cmd/keel` binds the gate to the decision broker and approval provenance;
  `cmd/runtime` has no decision surface and stays fail-closed
  (`mcp_approval_required`).
- Tests: `TestMCPDestructiveToolFailsClosedWithoutApproval`,
  `TestMCPDestructiveToolRequiresAndHonoursApproval`,
  `TestMCPDestructiveClassification`, `TestMCPCallCrashIsTypedToolError`; in
  `cmd/keel`: `TestMCPApprovalAllowsOnlyAfterHumanAllow`,
  `TestMCPApprovalDeniesWithoutAllow`, `TestMCPApprovalFailsClosedWithoutSeam`,
  `TestMCPApprovalRequestIDBindsAttemptCallAndArguments`,
  `TestMCPApprovalDoesNotReplayAcrossAttempts`,
  `TestMCPApprovalPromptAndProvenanceCarryBoundedRedactedArguments`.

## 4. Usage ledger

- The existing `usage_ledger` is the only provider-usage truth. The runner
  wraps every provider request (`NewUsageStream`): `unaccounted` before the
  call, `settled` only with a provider usage report. No report (cancellation,
  provider error, abnormal close) leaves the row unaccounted — never a false
  settled zero; an explicit empty usage object is a measurement and settles 0.
- Production wiring: `internal/usage.Recorder` → `store.AppendUsage`, installed
  by both `cmd/keel` and `cmd/runtime` through `servicehost.Config.Usage`.
- Request ids: `u<promptSeq>-a<ordinal>-r<n>` for turns,
  `...-compact-<requestId>` for manual compaction summaries.
- Tests: `TestUsageStreamRecordsRequestLifecycle`,
  `TestUsageStreamDoesNotSettleWithoutUsage`,
  `TestUsageStreamSettlesExplicitZeroUsage`,
  `TestUsageRecordOutlivesCancellation`, `TestRecorderWritesTheExistingLedger`.

## 5. Nightly chaos and conformance golden

- CI: `chaos-nightly` in `.github/workflows/ci.yml` and
  `.github/workflows/conformance.yml` with `docs/conformance-golden.json`.
  These workflow jobs are **not executed on GitHub from this environment**;
  the official task boxes stay unchecked for that reason. The identical local
  harness runs are recorded in §7/§8.
- `scripts/conformance.sh` runs `-p 1` (the integration database rule) and
  summarizes even when `go test` exits non-zero.

## 6. Capacity and runbook

- Per-capability runbook: `RUNBOOK.md`.
- 2GB / 1000 sessions: `evidence/capacity-2026-11-28/acceptance.json` + `subject.json`
  (sourceCommit `3c7bd789`) — peak 595.7 MB, 1000 sessions, 0 hard SSE errors,
  verdict PASS.
- 2GB / 1000 sessions / 64 concurrent runs: `evidence/capacity-2026-11-28-64/acceptance.json`
  + `subject.json` (sourceCommit `3c7bd789`) — `runsActiveMax=64`,
  `runsPendingMax=988`, peak 545.9 MB, 690 images, 2265 texts, 0 hard SSE
  errors, verdict PASS.
- `scripts/acceptance-perf.sh` supports `KEEL_PERF_PREFILL_RUNS` and
  `KEEL_PERF_MIN_ACTIVE_RUNS`, so the 64-slot profile is an asserted gate.

## 7. Chaos soak

Formal 600s / SIGKILL every 30s rerun on `subject.json`'s `sourceCommit`,
ports 13794/13795 (13760/13761 were held by another process):

```
KEEL_TEST_DSN=... KEEL_PG_CTR=keel-pg KEEL_G5_CHAOS_PORT=13794 \
  scripts/acceptance-g5-chaos.sh evidence/chaos-2026-11-28
```

`result.json`: **PASS** — `faults=20`, `restarts=20`, `durationSec=702.7`,
`eventCount=24699`, `sessionsChecked=16`, kill interval mean/min/max
30.003/29.990/30.030s, and all invariant counters zero:

| Counter | Value |
|---|---|
| seqGaps | 0 |
| duplicateRequestIDs | 0 |
| duplicateFinalReplies | 0 |
| unansweredNewest | 0 |
| orphanToolResults | 0 |
| unreadAfterSettle | 0 |
| stuckRuns | 0 |
| runStatusErrors | 0 |
| eventLoadErrors | 0 |

Evidence: `evidence/chaos-2026-11-28/{result.json,chaos-kill.json,chaos.log,runtime.log.gz,runtime-tail.log,cleanup.txt,subject.json}`.

## 8. Commands

| Command | Result |
|---|---|
| `go build ./...`, `go vet` on touched packages | PASS |
| `KEEL_TEST_DSN=... go test -race -p 1 ./internal/{collab,kernel,agent,mcp,toolsurface,usage,servicehost,store}/...` | PASS |
| `KEEL_TEST_DSN=... go test -race -p 1 ./cmd/keel/... ./cmd/runtime/...` | PASS (keel 106.3s, runtime 1.3s) |
| `KEEL_TEST_DSN=... KEEL_CONFORMANCE_GOLDEN=docs/conformance-golden.json scripts/conformance.sh` | PASS — 1318 pass / 0 fail / 3 skip, golden PASS |
| `KEEL_TEST_DSN=... KEEL_PG_CTR=keel-pg KEEL_G5_CHAOS_PORT=13794 scripts/acceptance-g5-chaos.sh ...` | PASS — 20 SIGKILLs, 0 lost events, 0 duplicate final replies |
| `openspec validate --strict keel-collab-p2` / `keel-pi-parity-closure-2` | PASS |

The conformance run was executed after `7948226f` (the request-ledger timestamp
fix); the capacity/chaos runs were executed from `3c7bd789` as recorded in
their `subject.json`.

## 9. Remaining / not claimed

- Independent re-review (P0/P1=0) and archive are not done here.
- GitHub Actions execution of the two CI jobs is not observable from this
  environment; local equivalents were run.
