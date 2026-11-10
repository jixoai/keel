# Recovery and compaction

Two mechanisms keep a long session honest: **recovery**, which makes `kill -9`
a non-event, and **compaction**, which keeps the context bounded without
rewriting history. Both follow the same rule — the ledger is append-only and
the result of either mechanism is a committed fact.

## 1. Recovery in one sentence

> A crashed process is never resumed; it is **replaced**. The successor claims
> the session, rebuilds the transcript from the ledger and re-executes only
> what the ledger says is missing.

That single sentence explains every design decision in this section: the read
watermark, the consumption fact (`inputSeqs`), the at-least-once tool contract
and the derived generation id all exist so that "what is missing" is a fact,
not a guess.

## 2. What a crash leaves behind

The ledger is the durable state — every fact up to the last commit survives a
`SIGKILL`. What does *not* survive is process-local state: an in-flight model
stream, a partially executed tool batch, the token-level partials in the hub.
None of that is ever treated as a fact.

Recovery is a normal claim path, not a special mode
(`internal/agent/worker.go:170`):

1. the dead attempt stops renewing its lease; the lease expires (default TTL
   30s, `KEEL_LEASE_MS` overrides — `internal/agent/worker.go:151`);
2. the next claim takes the session over: the kernel issues a **new epoch**, and
   the claim reports `Reclaimed` so the lease-expiry metric stays honest
   (`internal/kernel/kernel.go:123`, `internal/agent/worker.go:200`);
3. any write from the old owner is refused by the fence — the successor is the
   only writer (`stale-epoch` / `lease-lost`, `internal/kernel/kernel.go:355`).

### What the successor knows

| Question | Answer mechanism | Citation |
| --- | --- | --- |
| Which input did the dead attempt never read? | the **read watermark** persisted before any other work | `internal/agent/runner.go:295` |
| Was this input already answered? | the **consumption fact**: a completed assistant message's `inputSeqs` | `internal/agent/runner.go:707` |
| Did the dead attempt answer only partially? | unresolved tool calls after the newest assistant message | `internal/agent/runner.go:829` |
| Must this exact answer be written twice? | no — the **generation id** is derived from the log, so a retried answer dedupes | `internal/agent/runner.go:634` |
| Did input arrive while the attempt was running? | `FinishRequest.ReadThrough` re-queues everything above the watermark | `internal/agent/worker.go:399` |

### At-least-once tools

Tool execution across a crash is **at-least-once**: a call committed without
its result is executed again by the successor, and the kernel keeps exactly one
result per `toolCallId` (`internal/agent/runner.go:829`, `:937`). This is a
deliberate product contract — the framework never pretends to be exactly-once:

> Idempotent effects: tools must carry their own idempotency keys
> (replay-safe ≠ exactly-once; the framework does not fake it).
> — `docs/ARCHITECTURE.md` §3

### Clean shutdown is different

A process that shuts down *while executing* does not settle the run as failed;
it **gives the claim back** (`Release`), so the next process claims the run
immediately and the tenant's admission slot is returned instead of leaking
(`internal/agent/worker.go:368`, `internal/kernel/lease.go:112`).

### Retrying input by hand

If an attempt failed terminally (provider error), the input stays owed and the
next claim retries it. An operator or client can also re-ask a committed input
explicitly: `POST /v1/sessions/{id}/retry` commits the same text under a new
request id and records `retryOf` (the seq it re-asks) in the payload
(`internal/httpapi/server.go:156`, `internal/httpapi/input_commit.go:41`).

## 3. Handoff: recovering across processes

Rolling an upgrade is the same problem at process scale. The Harness never
hot-swaps a running process: **handoff** starts a new runtime, drains the old
one and lets the new epoch take over (`docs/HANDOFF.md`).

```
old runtime                                    new runtime
  │ claim loop                                     │
  │◀── POST /v1/runtime/drain {"mode":"handoff",   │
  │        "deadlineMs":30000}                     │
  ├─ stops claiming immediately                     │
  ├─ keeps accepting edge input (still committed)   │
  ├─ in-flight runs continue to their commit        │
  │     boundary; uncommitted partials are dropped  │
  ├─ releases leases ──────────────────────────────▶│ claims with epoch+1
  │                                                 ├─ rebuilds from the ledger
  │ SSE clients get `event: drain`                  │ clients resume from Last-Event-ID
  │ {retry_after_ms} then reconnect ───────────────▶│ first frame = cursor + 1
```

- Two drain modes: `shutdown` (SIGTERM path — in-flight runs are abandoned and
  their leases expire) and `handoff` (in-flight runs settle at their commit
  boundary and the lease is cleared) (`docs/HANDOFF.md`, `POST
  /v1/runtime/drain` in `internal/httpapi/runtime.go:178`).
- Audit: `runtime_instances` and `handoffs` record old/new runtime, loadout
  revision, epoch and in-flight counts; `GET /v1/runtime/status` exposes
  `runtimeId`, drain mode, `claiming` and `inFlightRuns` (`docs/HANDOFF.md`).
- **Upgrade discipline (mandatory):** never let two writers coexist — *drain
  the old process first, then start the new binary*. An old binary's write path
  ignores the fence, so coexistence breaks the exactly-one-writer invariant
  (documented with the cutover test `internal/kernel/cutover_test.go` and the
  rollback runbook in `docs/HANDOFF.md`).
- What the user sees is a continued session, not a preserved process: 0 lost
  events, 0 duplicated final replies, a continuous SSE cursor (the W4
  acceptance in `docs/SERVICE-GRAPH.md` §6).

## 4. Compaction

Long sessions outgrow any context window. Compaction summarizes the oldest part
of the transcript and commits that fact — it never edits or deletes history.

```
transcript: [m1 … m40][m41 … m60][m61 … m80]      ← threshold crossed
                     │
    compaction.cutpoint / summary / tokens          internal/compaction/compact.go
                     ▼
commit event kind=compaction                        internal/agent/runner.go:478
  {summary, firstKept, summarized, timestamp}
                     ▼
next request presents: [system, summary, kept messages…]
```

- **Trigger**: the compaction service supplies settings from the tenant
  document — `compaction.enabled`, `threshold`, `keepRecentTokens`
  (`docs/LOADOUT.md` §3). The model's own context window is preferred over the
  tenant default when the model declares one
  (`internal/agent/runner.go:489`).
- **Cut point**: chosen by the compaction package's policy, pinned at a turn
  boundary, never inside a tool call/result pair
  (`internal/compaction/cutpoint.go`).
- **Event shape**: `{summary, firstKept, summarized, timestamp}`
  (`internal/agent/runner.go:467`); `firstKept`/`summarized` are index facts so
  a rebuild can reconstruct exactly what the request presented.
- **Projection**: the client renders compaction as a maintenance fact
  (`compaction` in `packages/chat-surface/src/projection.ts`); an active
  compaction marks the session busy and blocks queue draining
  (`docs/CHAT-SURFACE.md` §5).
- **Conformance**: the compaction core is ported from PI's test suite and is
  part of the conformance matrix (`docs/PI-CONFORMANCE.md`,
  `internal/compaction/conformance_test.go`).

## 5. Acceptance evidence

The recovery guarantees are tested, not promised:

| Guarantee | Evidence |
| --- | --- |
| requestId idempotency, SSE cursor, blob resume, 2000-event sequence with no gap | `cmd/e2e` (7/7 in `README.md`) |
| `SIGKILL` during a model call and during tool execution, then continue | `internal/agent/crash_matrix_test.go`, `internal/durable/*_test.go` |
| abandoned attempts, lease takeover, drain gate | `internal/agent/worker_abandon_test.go`, `worker_drain_gate_test.go` |
| handoff safety points (5 classes) and dual-process SSE continuation | `internal/handoff/*_test.go`, `docs/HANDOFF.md` |
| cutover discipline (old writer vs fence) | `internal/kernel/cutover_test.go` |
| compaction cut points and policy conformance | `internal/compaction/conformance_test.go`, `internal/loop/loop_compaction_test.go` |
| full gate | `scripts/acceptance-all.sh` |
