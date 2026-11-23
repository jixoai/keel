# W2 review follow-ups — P2 closure + web P1 (2026-11-23)

Lane: `task/keel-w2-followups` (worktree `.worktrees/keel-w2`), based on the W2
queue/ETA/memory-packing slice; final rebase on `a549e87` + W11 + W3. Scope: the
six follow-ups from the W2 review that are implementable in this lane, plus the
web queued-state P1 (§7). Group OOM / process isolation and draining-group
eviction remain out of scope (unchanged).

## 1. FIFO fairness in a crash/reconfigure window

Before: `AdmitRun` counted only active (`pending`/`running`) rows. A queued head
left behind by a crashed worker (or a raised `max_concurrent_runs`) did not count,
so a new session could be inserted `pending` into the free slot before promotion
ran — the newcomer silently jumped the queue.

After (`internal/placement/pg.go`, `internal/placement/mem.go`):

- when the target group has a free slot, the admission runs the same guarded
  promotion pass (`ReleaseRun`/settlement use it) under the same per-tenant
  advisory lock **before** it may reserve a slot;
- the self run row is re-read (promotion may have promoted this very session);
- if a queued head still remains after promotion (memory-budget head-of-line, or
  a concurrent writer), a **new** admission is written to the queue tail instead
  of taking the slot (`groupHasQueuedTx` in PG, `queueHeadLocked` in MemStore);
- a full target group skips the promotion pass: the admission is going to the
  queue anyway, and the shorter lock hold keeps a burst of queued admissions
  from exhausting the 1s `lock_timeout` budget.

`Created`/`AlreadyActive` now describe what existed before the call
(`wasActive`/`wasQueued`), because promotion inside `AdmitRun` can change the
row's status.

Tests: `TestQueuedHeadGetsAFreeSlotBeforeANewAdmission` (Mem),
`TestPGQueuedHeadGetsAFreeSlotBeforeANewAdmission` (real PG).

## 2. Tenant quota vs. a concurrent kernel `user.message` wake

Two holes were closed:

1. **The wake cannot create an active placement row.** `kernel/pg.go` now writes
   a `user.message` wake for a run that is `done`/`failed` **and belongs to a
   placement group** as `status='queued'` with a fresh `queue_seq` (instead of
   `pending`). Promotion — quota-guarded, under the placement lock — is the only
   door to a slot. This also closes the drain bypass where a direct wake used to
   resurrect a run into a draining group. A session that never had a run row
   keeps the M1 bare-commit wake (`pending`) because the kernel cannot invent a
   placement for it (see Limitations). `kernel/mem.go` now preserves `queued`
   like PG already did.
2. **Promotion re-checks the DB aggregate every time.** `promoteQueuedTx`
   re-counts `runs(status in pending,running)` for the tenant before each head
   (READ COMMITTED sees a wake that committed meanwhile), guards the
   `queued -> pending` UPDATE with the same count in-statement, and ends with a
   fail-closed check: if a concurrent wake pushed the tenant over quota after
   `startingActive`, the transaction returns the retryable
   `errPromotionContention`, rolls back, and `inTx` retries against the wake's
   committed row instead of committing an oversell.

Tests (real PG): `TestPGUserMessageWakeOfATerminalRunJoinsTheQueue` (a terminal
grouped run wakes queued; promotion stays blocked while the quota is spent and
promotes it when the quota frees) and
`TestPGPromotionRacingAUserMessageWakeDoesNotOversell` (8 iterations racing
`PromoteQueued` against the keyed kernel commit; at most one active run, the
earlier queue entry wins, the wake stays queued). The kernel contract test
`TestEdgeCommitKeepsAPlacementQueuedRunQueued` runs on both Mem and PG and pins
"a wake never turns queued into claimable pending". Existing
`TestPGConcurrentAdmissionsNoOversell` / `TestPGGateConcurrentAdmissionsNoOversell`
remain green.

## 3. ETA samples: completed observations only

Rule: `run_duration_samples` is written **only** for a completed attempt
(`kernel.RunSummaryDone`, `"done"`). `abandoned`, `finish_error`, `failed`,
`cancelled` and `superseded` attempts still free the slot and trigger promotion,
but their duration never enters the group/tenant average. Rationale: those
durations measure a shutdown, a lost lease, a configuration failure or a
deliberate stop, not the work a queued run is waiting for; averaging them made
the advertised wait drift away from the actual one.

`Gate.RunSettled` owns the filter (`internal/placement/gate.go`); the
`agent.SettlementObserver` comments now state the rule.

Tests: `TestRunSettledOnlySamplesCompletedAttempts` (Mem) and
`TestPGRunSettledOnlySamplesCompletedAttempts` (real `run_duration_samples`
table).

## 4. `/run` 202 uses the input/retry helper

Before: `triggerRun` built its own queued map and omitted `etaSamples`/`etaBasis`.
After: it builds a `placement.QueueState` from the admission and calls
`queuedAdmissionBody` + `setQueuedRetryAfter`, the same functions input and
retry use. The now-unused `queueETASeconds` helper was removed.

Test: `TestPlacementHTTPTriggerRunGated` inserts one 4s observation and asserts
the `/run` 202 body carries `position/active/limit/groupId/etaKnown/etaSeconds/
etaSamples/etaBasis` and `Retry-After: 4`.

## 5. Memory refusal diagnostics

Before: when the pack strategy found no group with headroom, the typed refusal
set `Active: len(candidates)` (a **group** count) and left `UsedBytes` at zero.

After: `Store.GroupUsage` returns `{Runs, MemoryBytes}` per group (one query,
count + sum of active rows). The pack refusal aggregates the candidate pool:
`Active` = active-run count, `UsedBytes` = summed estimates, `LimitBytes` =
summed group limits. The admission-time `memoryErrorTx` / `memoryCapacityError`
already had per-group used/limit bytes; they keep them. HTTP already rendered
`active` and `usedBytes` (when `limitBytes > 0`), so no wire change was needed —
only honest values.

Tests: `TestPackMemoryBudgetChoosesHeadroom` now asserts 4 active runs and
170/200 bytes; `TestPackChoosesGroupByHeadroomAtClaim` asserts 1 run and 90/100;
`TestPlacementHTTPMemoryRefusalDiagnostics` pins the 429 body and `Retry-After`.

## 6. Web: a real 排队中 state

Before: `sessionState("queued")` fell through to `"ready"` (就绪) — the list and
the status pane said the run was idle while its row was a durable FIFO entry.

After:

- `web/src/ui/run-state.ts` adds `queued` / 排队中 (neutral tone) and
  `queueDetail(queue)` → `排队中 · 第 N 位 · 约 M 秒`; an unknown ETA renders
  `排队中 · 第 N 位` (never `0 秒`), and a queued row without a position still
  says `排队中`.
- `SessionSummary.queue` carries the 202 queue block; `HttpCommands.send*`
  parses it with `parseQueueFacts` (`web/src/data/queue.ts`) and the store
  patches `runStatus: "queued"` + the fact. `run.finished` / a started run clear
  it.
- The session row shows the queue badge even on the active row; the status pane
  header prefers the active session's queue fact over the conversation
  projection (which has no queue fact and would say 就绪).

Tests: `web/src/ui/run-state.test.ts` (4), `web/src/data/queue.test.ts` (4),
`nsh check`. The full `nsh test --target tui` suite was run while other lanes'
Go suites were hammering the machine: 330/334 passed; the four failures are the
settings model page (30s timeout) and three books-screen flows (three 30s
timeouts plus a records-page interaction), none of which touch the changed
files. `nsh test --target tui src/screens/books/books.tui.test.tsx --filter
'记录——默认打开首条'` passes alone (8.0s), and `App.tui.test.tsx` passes
11/12 (the remaining one is the same settings timeout). See `tests.txt`.

## 7. Web P1: the queue fact cannot outlive the server fact

Review found a drift: the client queue fact (from a 202) could survive after the
server row left `queued` (promotion/finish/cancel), and a historical `attach()`
replay or a live input racing its 202 could overwrite a queued row.

After (`web/src/data/store.ts`, `web/src/data/http.ts`,
`web/src/screens/session-list.tsx`, `web/src/screens/session-status-pane.tsx`):

- `mergeSessions` drops the client queue fact whenever the incoming server
  `runStatus` is not `queued`; a still-queued row keeps the admission fact;
- `rowStateOf` and the status header only render 排队中 while the row's
  `runStatus` is still queued (a stale fact cannot speak for the server);
- `onLiveEvent` ignores `attach()`'s historical user/assistant/run.finished
  replay and a racing input for a queued row;
- `retry` parses a queued 202 through `parseQueueFacts` and patches
  `queued` + the queue fact instead of optimistically saying running.

Tests on the final rebase (Go `internal/placement`/`httpapi`/`kernel` green;
web queue suites green): `web/src/data/store.queue.test.ts` (5),
`web/src/data/queue.test.ts` (4), `web/src/ui/run-state.test.ts` (4). See
`web-queue-p1.txt` and `tests-rerun-2026-11-23.txt`.

## Known follow-ups (P3, not blocking)

The independent review (2026-11-23) left three observations, recorded here
rather than silently dropped:

1. The admission's three-false combination (promotion promoted the caller's own
   queued row, so `Created`/`AlreadyActive`/`Queued` are all false) has no
   direct unit test; its consumers are exercised through the gate/HTTP paths.
2. `errPromotionContention` can consume the transaction's retry budget after
   four consecutive races and surface as a generic 500; mapping it to
   503 + `Retry-After` is a follow-up (probability is low).
3. The send path's 202 → queued patch has no store-level integration test (the
   retry path does); the unit and gate paths are covered.

None of these is a correctness gap in the covered paths; they are backlog for
the next W2 touch.

## Limitations (explicit)

- A kernel `user.message` commit for a session with **no run row** still inserts
  the M1 bare-commit wake as `pending`. The kernel cannot know a placement for
  an unplaced session, and the M1 contract pins "commit alone queues a run".
  This path is unreachable from the product input paths (HTTP, retry, MCP app
  and `TurnStarter` all admit through the placement gate before the commit), so
  it cannot oversell a placement-managed tenant quota; it is called out rather
  than simulated away.
- The wake rule for a terminal grouped row makes a **direct** (ungated) commit
  wait for promotion (settlement/sweeper, default 5s) instead of starting
  immediately. Gated input remains immediately pending because admission runs
  first. This is the price of keeping the quota invariant; no fake immediate
  claim is reported.
- Draining/retired groups still keep their queued rows (no eviction/migration),
  as documented by the original W2 lane.
- The final over-quota check can only see a wake that commits **before** the
  check; a wake committing in the last instant before our COMMIT is outside any
  lock the kernel takes. The guarded UPDATE + per-promotion re-count + final
  check make the window as small as it can be without making the kernel wake
  quota-aware (it has no quota source) or taking the placement lock in the
  kernel.
