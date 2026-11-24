---
title: Placement and groups
description: Where a session's runs execute — groups, leases with epoch fencing, the durable FIFO queue with 202 + position + ETA, and the 429 boundary that the queue never crosses.
---

# Placement and groups

[Assembly](/core/assembly) decides what a process is made of; placement decides where a session's
runs execute. The model is frozen in the harness spec
(`openspec/specs/harness-core/spec.md:24`) and implemented in `internal/placement`, whose package
contract is the header of `internal/placement/types.go:1`.

## 1. Groups and leases

A **group** is the placement and collaboration unit: it owns its concurrent-run slots
(`max_concurrent_runs`), a memory budget, a plugin assembly set and a health state, and in this
change one worker process carries one group (`internal/placement/types.go:5`,
`openspec/specs/harness-core/spec.md:26`). Group status is `active` / `draining` / `retired`; a
draining group takes no new placements (`internal/placement/types.go:64`).

A session's membership is a **lease, not a static binding**: `(epoch, lease_until)`. A live lease
cannot be adopted, an expired one can — which is what keeps the kernel's stateless contract intact
(`internal/placement/types.go:6`). Moving a session is "drain the old group, rebuild in the new one
from the ledger"; no process state is copied.

## 2. Strategies

| Strategy | Meaning | Implementation |
|---|---|---|
| `pack` (default) | density: fills the tenant's pack groups (`pack-default`) by memory budget | `internal/placement/types.go:51` |
| `user` | one group per user — with user == tenant today, one group per tenant (the isolation SKU) | `internal/placement/types.go:54` |
| `pin` | the group named by the caller, or the configured pin target | `internal/placement/types.go:57` |

## 3. Counting happens in the database

Both limits are counted in the database, so two groups cannot sell the same tenant slot and two
workers cannot oversell a group (`internal/placement/types.go:10`). The `runs` table stays the only
active-run account — placement adds `groups` and `session_placement`, never a second ledger
(`internal/placement/types.go:21`, `internal/store/schema.sql:894`, `internal/store/schema.sql:935`).

## 4. A full group queues; it does not fail

When the only limit involved is the group's slot count, the run becomes a durable FIFO entry:
status `queued`, one per session, ordered by a database sequence (`internal/placement/types.go:12`).
The input commit already succeeded — the message is durable either way — so the HTTP edge reports
the wait instead of pretending the run started:

| Response | When |
|---|---|
| `201` | new submission, run admitted immediately (`internal/httpapi/server.go:497`) |
| `200` | the same `requestId` was already committed (idempotent repeat) |
| `202` + `queued{position, active, limit, etaKnown, etaSeconds, etaSamples, etaBasis}` | the run waits in a full group (`internal/httpapi/server.go:927`) |
| `429` + `Retry-After` | the tenant run quota is spent, or the group's memory budget is full (`internal/httpapi/server.go:866`) |
| `409` + `reason` | placement conflict: `epoch_mismatch`, `lease_held`, `group_draining`, `group_unavailable`, `pin_requires_group`, `already_placed` (`internal/placement/types.go:105`) |
| `501` | a memory-bounded group without an estimate — retrying cannot help (`internal/placement/types.go:384`) |

The queue position is 1-based and the ETA is derived only from observed attempts
(`avgDuration × ceil(position/limit)`, averaged over a 20-sample window, 50 retained per group)
(`internal/placement/types.go:463`). With no history, `etaSeconds` is `null` and no `Retry-After` is
sent — an unknown wait is never reported as zero (`internal/httpapi/server.go:952`).

Promotion is one guarded transition, `queued → pending`, running FIFO on `ReleaseRun`, on a terminal
attempt settlement, and from a recovery sweep, so a crash converges the queue
(`internal/placement/types.go:16`). A queued row is never claimable by the kernel.

## 5. The 429 boundary

A full group is **not** a 429 any more: it is a queued admission, and the run keeps its place. The
tenant quota stays a hard refusal (`429` + `Retry-After`) so that the queue can never become an
oversell channel (`internal/placement/types.go:14`). A group's **memory** budget behaves the same
way: a run whose estimate does not fit is refused at admission instead of being queued behind a
number nothing can honour, and promotion re-checks it (`internal/placement/types.go:167`).

## 6. Wiring and knobs

`keel serve` and `cmd/runtime` mount the gate as the admission gate; a misconfigured gate fails the
boot instead of silently falling back to the process-local worker semaphore
(`cmd/keel/serve.go:417`). Run concurrency is therefore admitted durably, and the kernel keeps only
the event-rate dimension via `quota.Limits.EventsOnly`, so one run is never counted twice
(`cmd/keel/serve.go:409`, `internal/quota/limits.go:227`).

| Environment | Default | Meaning |
|---|---|---|
| `KEEL_PLACEMENT_STRATEGY` | `pack` | default policy for an unplaced session (`internal/placement/wiring/fromenv.go:21`) |
| `KEEL_PLACEMENT_GROUP_ID` | — | the pin strategy's target group (`internal/placement/wiring/fromenv.go:22`) |
| `KEEL_GROUP_MAX_CONCURRENT_RUNS` | `0` (unlimited) | slot count for groups the strategies create on first use (`internal/placement/wiring/fromenv.go:27`) |
| `KEEL_GROUP_MEMORY_LIMIT_BYTES` | `0` (unbounded) | the pack strategy's memory budget (`internal/placement/wiring/fromenv.go:30`) |
| `KEEL_PLACEMENT_LEASE_MS` | package default (30s) | placement lease TTL (`internal/placement/wiring/fromenv.go:33`, `internal/placement/types.go:468`) |
| `KEEL_PLACEMENT_QUEUE_INTERVAL_MS` | package default | crash-recovery queue sweep interval (`internal/placement/wiring/fromenv.go:36`) |

## 7. Verify

```bash
go test ./internal/placement/...
```

The package tests cover the mem and PG stores, the queue (FIFO order, promotion exactly once,
no oversell) and the gate; the end-to-end placement path is exercised through the HTTP edge
(`internal/httpapi/placement_e2e_test.go`).
