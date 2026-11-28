# PI parity closure — per-capability runbook

> Change: `keel-pi-parity-closure-2`. Scope: Keel core (`internal/agent`,
> `internal/kernel`, `internal/mcp`, `internal/toolsurface`, `internal/usage`).
> Every command below is run from the repository root. `KEEL_TEST_DSN` points at
> a dedicated PostgreSQL 18 database.

## 1. CompactionTask / checkpoint recovery

**What is durable**

| Table | Meaning | Never |
|---|---|---|
| `events` | the transcript and compaction facts | never pruned by checkpoint policy |
| `checkpoints` | projection of a **finished** compaction (`summary`, retained tail, `summary_hash`, accounting) | never the source of truth |
| `checkpoint_requests` | manual-compaction idempotency ledger (`requestId -> upto_seq`, result snapshot) | deleted by `PruneCheckpoints` |
| `compaction_tasks` | in-flight task checkpoint (`select` / `summarize` / `retry`, pinned range, attempts, `retry_until`) | never carries a summary; deleted once event + projection are durable |

**Recovery rules**

- Manual request whose compaction **event** committed but projection save failed:
  the next call with the same `requestId` restores the projection from the event
  (`recovered: true`), without a second model call.
- Manual request killed during the summary call: the task row pinned the range
  (`first_kept_seq`); the retry resumes `phase=summarize` and re-runs the
  summary on that exact range (`CompactAt`). A pin that no longer names a
  committed child event falls back to a fresh cut and logs a warning.
- Automatic compaction: `compaction_start` writes a `select` row keyed
  `keel-auto-compaction:<cursor>`; a committed event is projected on the next
  attempt; an uncommitted marker is discarded (the context was never changed).

**Inspect**

```sql
SELECT tenant_id, session_id, request_id, phase, first_kept_seq, tail_seq,
       tokens_before, policy_version, attempt, retry_until, updated_at
  FROM compaction_tasks
 WHERE session_id = $1
 ORDER BY updated_at DESC;
SELECT upto_seq, kept_from_seq, summary_hash, request_id, policy_version
  FROM checkpoints
 WHERE tenant_id = $1 AND session_id = $2
 ORDER BY upto_seq DESC;
SELECT request_id, upto_seq, summary_hash
  FROM checkpoint_requests
 WHERE tenant_id = $1 AND session_id = $2;
```

A row that stays in `summarize`/`retry` after the process is healthy means the
task never reached a terminal outcome; do not delete it by hand — replay the
same request (the pin is the recovery fact). `retry_until` is advisory state
for the retry phase, not a scheduler.

**Lease / kind**: there is no independent task lease column by design. Manual
compaction runs under the session's kernel lease (`CompactNow` receives the
`kernel.Lease`), and automatic compaction runs inside the run's lease; that
lease is the fencing authority, so a task row cannot be executed by a stale
owner. `kind` is encoded by the request-id namespace (`keel-auto-compaction:` =
automatic; the caller's requestId = manual), and the phase column is the state
machine (`select → summarize → retry`). `attempt` is the durable retry count.

**Evidence / tests**

- `go test -race ./internal/kernel/ -run 'TestCompactionTask|TestCheckpoint'`
- `go test -race ./internal/agent/ -run 'TestManualCompact|TestRecoverAutomatic|TestCorruptCheckpoint|TestRunLazily'`
- Crash matrix: `TestManualCompactRecoversCommittedEntry`,
  `TestManualCompactResumesPinnedSummarizePhase`,
  `TestRecoverAutomaticCompactionProjection`.

## 2. Skills `allowed-tools`

- Absence of a declaration = no restriction. A declaration restricts the
  **attempt's** surface to the union of the active skills' entries, intersected
  with the actually registered toolset (`registry.Subset`).
- Entry mapping: case-insensitive tool names; `Bash(git:*)` names `bash`;
  `mcp__<server>__<tool>` and `mcp*` name the aggregate `mcp` tool.
- Execution gate: a call outside the set is refused with code
  `skill_tool_not_allowed`, a model-readable message naming the skill and the
  allowed tools, and structured `details{code,tool,skills,allowed}`. Ordering:
  principal authorization → skill gate → guardrails/hooks.
- Verify: `go test -race ./internal/agent/ -run TestSkillTool` plus a live
  `/skill:<name>` turn; the model-visible manifest excludes denied tools.

## 3. MCP pool, health and destructive approval

**Pool**

- One slot per server; a borrow (`CallToolWithOptions`/`HealthCheck`) validates
  it. Health probes run every 30s (configurable via
  `mcp.ConnectOptions.HealthCheckInterval`) and on a borrow when due.
- Reconnect backoff: 1s, 2s, 4s … capped at 30s per server; a successful
  reconnect resets it and increments `Reconnects`. `EnsureConnected` checks the
  backoff **before** probing a disconnected slot (a failed probe schedules the
  next attempt; the following borrow performs the reconnect), and shares one
  in-flight attempt among concurrent borrowers. The health monitor calls
  `EnsureConnected` only for the same reason.
- A crashed server answers a typed `ErrServerUnavailable` /
  `ReconnectBackoffError`; the run stays resumable. `Manager.PoolStatus()`
  exposes `connected/healthy/failures/reconnects/nextRetry/checkError`.

**Destructive approval**

- `annotations.destructiveHint` wins; otherwise a small verb-family fallback
  (`delete`, `write`, `execute`, …) marks the tool.
- `cmd/keel` wires `toolsurface.MCPApproval` to a durable decision of kind
  `approval.confirm` (options `allow`/`deny`, 5-minute timeout). The call runs
  only after `allow`; the answer is recorded by the normal `/v1/decisions`
  provenance path.
- No decision broker (e.g. `cmd/runtime`) ⇒ fail closed: the tool answers
  `mcp_approval_required` and never executes.
- The decision request id is derived from the attempt scope + provider
  `toolCallId`, so a retried call re-addresses one question; a distinct call
  asks again.

**Verify**

- `go test -race ./internal/mcp/ ./internal/toolsurface/ -run 'TestServerSlot|TestHealthCheck|TestEnsureConnected|TestMCP'`
- `go test -race ./cmd/keel/ -run 'TestMCPApproval'`

## 4. Usage ledger

- One source of truth: the existing `usage_ledger` table. Both `cmd/keel` and
  `cmd/runtime` install `internal/usage.Recorder` on the runner; every provider
  request writes an `unaccounted` marker before the call and upgrades it to
  `settled` with the provider-reported numbers on the terminal event.
- Request id: `u<promptSeq>-a<ordinal>-r<requestNo>` for a turn;
  `...-compact` for a manual-compaction summary. A retried attempt derives the
  same ids, so `AppendUsage` upgrades in place instead of double-billing.
- A provider that reports no usage settles zeros (not measurable), never a
  fabricated number. Accounting is best-effort and bounded (5s) on a detached
  context; it never fails the model request.
- The attempt summary sums the same provider usage events.

**Inspect**

```sql
SELECT request_id, seq, provider, model, input_tokens, output_tokens,
       cache_read_tokens, cache_write_tokens, reasoning_tokens, status
  FROM usage_ledger
 WHERE tenant_id = $1 AND session_id = $2
 ORDER BY seq DESC, request_id;
```

`status='unaccounted'` after a crash is the honest crash marker; it remains
zero-token until a replay settles it. `SUM` excludes no row: use
`unaccounted` counts to reconcile.

**Verify**

- `go test -race ./internal/agent/ -run TestUsage`
- `KEEL_TEST_DSN=... go test -race ./internal/usage/`

## 5. Nightly chaos and PI conformance golden

**Nightly chaos (10 minutes, SIGKILL every 30s)**

- CI: `.github/workflows/ci.yml` job `chaos-nightly` (schedule or
  `workflow_dispatch`). It starts a dedicated `keel-pg` container and runs
  `scripts/acceptance-g5-chaos.sh`.
- Local: `KEEL_TEST_DSN=... KEEL_PG_CTR=keel-pg scripts/acceptance-g5-chaos.sh <evidence-dir>`.
  The wrapper owns a dedicated `<db>_chaos` database and ports 13760/13761.
- Invariants asserted: contiguous seq, one user.message per posted requestId,
  at most one final assistant reply per turn, no stuck run, duration/fault
  cadence and 20 ± 1 kills. Evidence: `chaos-kill.json`, `result.json`,
  `runtime.log`, `cleanup.txt`.

**PI conformance golden**

- CI: `.github/workflows/conformance.yml` on PI pin/doc/conformance changes and
  on demand. `docs/conformance-golden.json` is the floor: every package's pass
  count and the total must not shrink.
- Local:
  `KEEL_TEST_DSN=... KEEL_CONFORMANCE_GOLDEN=docs/conformance-golden.json scripts/conformance.sh`.
- Regenerate the golden only with a deliberate coverage change:
  `python3 scripts/summarize-conformance.py /tmp/keel-conformance.json --out /tmp/summary.json`,
  then copy the compacted package/total counts into `docs/conformance-golden.json`.

## 6. Capacity profile (2 GB / 1000 sessions / 64 runs)

```bash
KEEL_PG_CTR=keel-pg \
KEEL_PERF_PREFILL_RUNS=1000 KEEL_PERF_MIN_ACTIVE_RUNS=64 \
KEEL_PERF_DIR=<evidence-dir> \
scripts/acceptance-perf.sh 1000 60s
```

The runtime runs under `--memory=2g`, `GOMEMLIMIT=1.5GiB`, 2000 stream slots and
the default 64 run slots. The gate fails on an unreadable cgroup peak, peak >
1536 MB, sessions ≠ 1000, no text load, a short run, non-429 stream errors, or
`runsActiveMax < 64` when the slot floor is set. Evidence: `acceptance.json`,
`bench.json`, `loadtest.log`.

## 7. Known limits

- The MCP health monitor reconnects a crashed server, but a call that was
  in-flight at the crash still fails once with a typed error; the run resumes.
- The nightly chaos job is scheduled on GitHub-hosted runners. It is not part
  of per-push CI by design.
- The conformance golden is a coverage floor, not a behavioral snapshot:
  behavior changes are caught by the tests themselves (a `fail` count).
