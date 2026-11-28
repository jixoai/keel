# Execution evidence — keel-pi-parity-closure-2 (2026-11-28)

> Branch: `task/keel-collab-pi-unified` (keel submodule), base
> `origin/task/keel-unified-integration` = `d4254284`.
> Worktree: `.worktrees/keel-collab-pi-unified` (keel repo).
> PostgreSQL: dedicated `postgres:18-alpine` container; PG tests use
> `KEEL_TEST_DSN`. No code outside `keel/` changed. No push to main.

## 1. CompactionTask / checkpoint

- Durable `compaction_tasks` state (`select` / `summarize` / `retry`) with
  `first_kept_seq`, `tail_seq`, `tokens_before`, `policy_version`, `attempt`,
  `retry_until`. The session kernel lease is the fencing authority (there is no
  independent task lease by design); automatic vs manual kind is encoded by the
  request-id namespace.
- Manual recovery: a committed compaction event with a lost projection is
  restored by requestId without a second model call; a failed/interrupted
  summary resumes the pinned range (`CompactAt`) instead of selecting a new cut.
- Automatic recovery: a committed event is projected on the next attempt; an
  uncommitted entry marker is discarded without touching the log; a stale
  marker is consumed.
- Crash matrix: `TestManualCompactRecoversCommittedEntry`,
  `TestManualCompactResumesPinnedSummarizePhase`,
  `TestRecoverAutomaticCompactionProjection`, and the new
  `TestCompactionResumeKeepsLogContiguousAndSingleAnswer` (interrupted
  compaction → normal attempt answers each pending input exactly once →
  contiguous 1..N seqs → retried compaction adds exactly one event; 0 lost
  events, 0 duplicate terminal answers). Existing run-level crash matrix
  (`TestCrashBeforeAssistantCommitReanswers`,
  `TestCrashAfterAssistantCommitDoesNotReanswer`, tool-turn/legacy cases) is
  unchanged and green.
- Kernel seam tests: `TestCompactionTaskCheckpoints`,
  `TestCompactionTaskValidation`.

## 2. Skills allowed-tools

- Execution-time intersection of the attempt's registry with the union of the
  active skills' declared allowed tools (`registry.Subset`); execution gate
  refuses out-of-set calls with code `skill_tool_not_allowed`, a typed,
  model-readable message and structured details; ordering is authorization →
  skill gate → guardrails/hooks.
- Absence of a declaration is unrestricted; a sibling unrestricted skill does
  not widen an active restricted set; `Bash(...)` scopes name the tool,
  `mcp__server__tool` names the aggregate `mcp` tool.
- Tests: `TestSkillToolPolicy` (7 cases), `TestSkillToolPolicyReadsLoadedPin`,
  `TestSkillToolPolicyIntersectsSurface`, `TestSkillToolGuardRefusesTyped`,
  `TestSkillToolGuardComposesAfterAuthorization`.

## 3. MCP lifecycle and destructive approval

- Pool: one slot per server; health probe on borrow when due and by the
  monitor; exponential backoff 1s…30s per server; concurrent borrowers share
  one reconnect.
- Fixed in this pass: `EnsureConnected` used to probe first, which
  re-scheduled the backoff and then refused the reconnect in the same call, so
  a crashed server could never be reconnected by a borrow or the monitor. It
  now checks the backoff before probing a disconnected slot; the monitor calls
  `EnsureConnected` only. Regression test:
  `TestEnsureConnectedReconnectsAfterBackoff`.
- Destructive approval: `annotations.destructiveHint` (or the small verb-family
  fallback) opens a durable `approval.confirm` decision (allow/deny, 5-minute
  timeout) via the same decision primitive as the `approve` tool; the call
  executes only after `allow`. IDs are scoped by attempt + provider
  `toolCallId`. `cmd/keel` binds the gate to the decision broker and approval
  provenance; `cmd/runtime` has no decision surface and stays fail-closed
  (`mcp_approval_required`). Tests: `TestMCPDestructiveToolFailsClosedWithoutApproval`,
  `TestMCPDestructiveToolRequiresAndHonoursApproval`,
  `TestMCPDestructiveClassification`, plus `cmd/keel` tests
  `TestMCPApprovalAllowsOnlyAfterHumanAllow`, `TestMCPApprovalDeniesWithoutAllow`,
  `TestMCPApprovalFailsClosedWithoutSeam`, `TestMCPApprovalRequestIDIsPerCall`.
- Crash: a server that exits during a call answers one typed `backend_failed`
  tool result; the run records a failed tool call and remains resumable
  (`TestMCPCallCrashIsTypedToolError`).

## 4. Usage ledger

- Wired the existing `usage_ledger` as the only provider-usage truth. The
  runner wraps every provider request (`NewUsageStream`): `unaccounted` before
  the call, `settled` with provider-reported input/output/cache/reasoning on
  the terminal event; no provider usage settles zeros rather than fabricating.
  Writes use a detached, 5s-bounded context so cancellation still settles.
- Production wiring: `internal/usage.Recorder` → `store.AppendUsage`, installed
  by both `cmd/keel` and `cmd/runtime` through `servicehost.Config.Usage`.
- Request ids are generation-derived (`u<promptSeq>-a<ordinal>-r<n>`; manual
  compaction adds `-compact-<requestId>`), so a retried generation upgrades the
  same row instead of double-billing, and two compactions at one generation do
  not share a row.
- Tests: `TestUsageStreamRecordsRequestLifecycle` (marker → settlement, same
  numbers as `RunSummary.Tokens`), `TestUsageRecordOutlivesCancellation`,
  `TestRecorderWritesTheExistingLedger` (real PG: 1 entry, 0 unaccounted,
  idempotent replay).

## 5. Nightly chaos and conformance golden

- CI: `chaos-nightly` in `.github/workflows/ci.yml` (schedule /
  `workflow_dispatch`) starts a dedicated `keel-pg` and runs
  `scripts/acceptance-g5-chaos.sh` for 600s with a kill every 30s, uploading
  the evidence directory. The placeholder TODO is removed.
- CI: new `.github/workflows/conformance.yml` runs on PI pin/doc/conformance
  changes and on demand; `docs/conformance-golden.json` is a per-package pass
  floor (adding tests is allowed; losing/skipping them fails).
- Local conformance on this branch (serial `-p 1`, matching the integration
  database rule; `scripts/conformance.sh` now summarizes even when `go test`
  exits non-zero):
  `KEEL_TEST_DSN=... KEEL_CONFORMANCE_GOLDEN=docs/conformance-golden.json scripts/conformance.sh`
  → `TOTAL 1314 pass / 0 fail / 3 skip`, `GOLDEN: PASS`.
- Local formal chaos (same command as CI): see §7.

## 6. Capacity and runbook

- Per-capability runbook: `openspec/changes/keel-pi-parity-closure-2/RUNBOOK.md`.
- 2GB / 1000 sessions (normal load, fake provider):
  `evidence/capacity-2026-11-28/acceptance.json` — peak 618.5 MB / 2048 MB,
  1000 sessions, 0 hard SSE errors, PASS.
- 2GB / 1000 sessions / 64 concurrent runs (slow provider, 1000 prefilled runs,
  observed `runsActiveMax`):
  `evidence/capacity-2026-11-28-64/acceptance.json` — `runsActiveMax=64`,
  `runsPendingMax=985`, peak 647.7 MB, 999 images, 4168 texts, 0 hard SSE
  errors, PASS.
- `scripts/acceptance-perf.sh` gained `KEEL_PERF_PREFILL_RUNS` and
  `KEEL_PERF_MIN_ACTIVE_RUNS`, so the 64-slot profile is an asserted gate, not
  an observation.

## 7. Chaos soak

Formal 600s / SIGKILL every 30s run, same command as the CI job:

```
KEEL_TEST_DSN=... KEEL_PG_CTR=keel-pg \
  scripts/acceptance-g5-chaos.sh \
  openspec/changes/keel-pi-parity-closure-2/evidence/chaos-2026-11-28
```

`result.json`: **PASS** — `faults=20`, `restarts=20`, `durationSec=655.5`,
`eventCount=22852`, `sessionsChecked=16`, kill interval mean/min/max
30.002/29.997/30.013s, and all invariant counters zero:

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

Evidence: `evidence/chaos-2026-11-28/{result.json,chaos-kill.json,chaos.log,runtime.log.gz,runtime-tail.log,cleanup.txt}`.
The wrapper compressed the 6.9 MB runtime log to `runtime.log.gz`.

## 8. Commands

| Command | Result |
|---|---|
| `go build ./...` | PASS |
| `go vet ./internal/{collab,kernel,agent,mcp,toolsurface,usage,servicehost}/... ./cmd/{keel,runtime}/...` | PASS |
| `KEEL_TEST_DSN=... go test -race -p 1 -count=1 ./internal/collab/... ./internal/kernel/... ./internal/agent/... ./internal/mcp/... ./internal/toolsurface/... ./internal/usage/... ./internal/servicehost/...` | PASS (collab 1.5s, kernel 37.2s, agent 55.4s, mcp 4.6s+oauth 1.9s, toolsurface 1.7s, usage 1.7s, servicehost 16.5s) |
| `KEEL_TEST_DSN=... go test -race -p 1 -count=1 ./internal/store/...` | PASS (39.0s; schema natural-key dedupe migration) |
| `KEEL_TEST_DSN=... go test -race -p 1 -count=1 ./cmd/keel/... ./cmd/runtime/...` | PASS (keel 78.4s, runtime 1.5s) |
| `KEEL_TEST_DSN=... KEEL_CONFORMANCE_GOLDEN=docs/conformance-golden.json scripts/conformance.sh` | PASS — 1314/0/3, golden pass |
| `openspec validate --strict keel-collab-p2` / `keel-pi-parity-closure-2` | both valid |

## 9. Remaining / not claimed

- GitHub Actions execution of the two CI jobs is not observable from this
  environment; the identical chaos and conformance harnesses were run locally.
- Independent sub-agent review (P0/P1=0) and archive are intentionally left
  unchecked for the separate review.
