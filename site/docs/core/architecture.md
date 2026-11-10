# Architecture

This page is the structural map of the Core: the ledger, the kernel around it, the service graph,
loadout, handoff and backpressure. It follows `docs/SERVICE-GRAPH.md` and `docs/KERNEL.md`, with
code anchors for the parts the repository pins with tests.

## The one rule

> **The ledger cannot be swapped; the workbench can. Swapping the workbench is a shift change, not
> a hot-plug.**
>
> — `docs/SERVICE-GRAPH.md:30`

Everything below is a consequence of that sentence.

```
client / SSE
     │
     ▼
┌──────────────────────────────────────────────────────┐
│ contract kernel (not pluggable)                       │
│ event order · turn/step · tool pairing                │
│ idempotency · lease/epoch · tenant/quota · crash truth│
└───────────────┬──────────────────────────────────────┘
                │ Commit / Claim / Renew / Finish
                ▼
        PostgreSQL event ledger (the only truth)
                ▲
                │ services submit proposals; the kernel decides
┌───────────────┴──────────────────────────────────────┐
│ service graph (loadout, replaceable)                  │
│ pi-loop · prompt · tools · llm · session-projection   │
│ compaction · mcp · telemetry · sandbox                │
└──────────────────────────────────────────────────────┘
```

## 1. The ledger: an append-only event log per session

A session's state is a strictly ordered sequence of events. `sessions.next_seq` allocates the next
`seq` inside the same transaction that appends the row, so `(tenant_id, session_id, seq)` is the
primary key and the order is the log's, not a timestamp's (`internal/store/schema.sql:52`).

Two consequences show up everywhere else in the design:

- **Checkpoints and transcripts are projections.** The `checkpoints` table stores a summary up to
  a `upto_seq`; transcript rebuild (`internal/agent/messages.go`) is a function of the event log.
  Neither is allowed to become a second source of truth.
- **Recovery is replay.** A new process does not need any in-memory continuation state; it reads
  the log and continues. The only mutable coordination left is the lease row (below).

The ledger also drives live delivery: an `AFTER INSERT` trigger emits `pg_notify('keel_events', …)`
so SSE subscribers wake up instead of polling (`internal/store/schema.sql:207`). A dropped
notification costs one heartbeat interval, never data.

## 2. The kernel: four jobs, and nothing else

The kernel does not think, call models or execute tools. It does four things
(`docs/SERVICE-GRAPH.md:61`):

1. **Accept**: validate that an event is legal, not a duplicate, and correctly paired.
2. **Book**: inside a transaction, allocate `seq`, append the event, and reject stale owners.
3. **Hand out keys**: on claim, give the owner an identity plus an epoch; an expired lease can be
   reclaimed.
4. **Guard boundaries**: tenant, quota, event kinds, session ownership.

Services cannot write the `events` table. They hand results to the kernel's `Commit`, and the
kernel decides whether they become facts. The package-level boundary is machine-checked:
`internal/architecture/boundary_test.go:14` lists the kernel packages
(`internal/loop`, `internal/agentcore`, `internal/agenttool`, `internal/kernel`, `internal/agent`,
`internal/store`) and asserts they never import product packages.

| Package | Responsibility | Not responsible for |
|---|---|---|
| `internal/agentcore` | message/content/event types (PI's message & event semantics) | reading the log, calling models |
| `internal/agenttool` | tool execution and batches (parallel calls, result pairing) | the business meaning of any tool |
| `internal/loop` | one loop execution: turns, tool batches, the three drain points (steering/follow-up/…), compaction, settle | writing the ledger |
| `internal/agentloop` | the replaceable loop contract + PI conformance manifest | binding to one implementation |
| `internal/kernel` | the ledger: commit/dedupe, lease/fencing, run rows, external session channel, delivery-mode and run-summary validation | deciding who runs or what they run |
| `internal/agent` | runner (rebuild → fill holes → loop → commit) + worker (claim/renew/settle) | auth, listing, approvals, plugins |

Services get **narrow seams**, not store handles: `kernel.Committer`, `kernel.Reader`,
`kernel.LeaseManager`, `kernel.ExternalSessions` (`internal/kernel/seams.go`), plus per-service
interfaces such as `agent.Assembly`, `agent.RunMirror` and `agent.RunBinding`. No service ever
receives an event-table write handle, a `seq` allocator, or a bypass around tenant/lease checks
(`docs/LOADOUT.md:147`).

## 3. The service graph: capabilities, descriptors, lifecycle

The service graph is the replaceable half. Each service is described statically by a
`ServiceDescriptor` (`internal/servicegraph/descriptor.go:26`):

| Field | Meaning |
|---|---|
| `Name` | unique within a loadout (`"pi-loop"`) |
| `Version` | implementation identity — bump it whenever the implementation changes |
| `Provides` / `Requires` / `Optional` | capability names (`ctx.llm`); a missing required capability keeps the service `PENDING`, a missing optional one activates it degraded and is recorded in diagnostics |
| `Role` | `primary` or `fallback` in a capability's provider chain |
| `Effects` | names of the cleanups the service registers while starting (listeners, timers, registrations) |
| `Factory` | `func(ctx, servicegraph.Resolved) (servicegraph.Instance, error)` (`internal/servicegraph/lifecycle.go:49`) — attached by the loadout builder |

Lifecycle states (`docs/SERVICE-GRAPH.md:170`):

```
PENDING ──deps ready──▶ LOADING ──ok──▶ ACTIVE
   ▲                       │              │
   │                       └──fail──▶ FAILED
   └────missing dep──── UNLOADING ◀── RETIRING ──┘
```

Rules the tests pin:

- A missing **required** dependency is `PENDING`, not a startup failure; diagnostics name who it is
  waiting for. A missing **optional** dependency still allows `ACTIVE` with a degraded diagnostic.
  `sandbox` is declared with *no factory at all*: dependents degrade instead of blocking on a
  service that cannot exist (`docs/LOADOUT.md:48`).
- Startup failure rolls back only the effects that service registered; already-active independent
  services are untouched.
- Replacing a provider unloads dependents first, then the provider; every reassembly increments
  `generation` and old effects must not leak into the new generation.
- Service identity for reassembly is `(Name, Version, Provides, Requires, Optional, Role)`. If the
  configuration changed but the service did not, the **instance is shared across generations**
  instead of being rebuilt.

## 4. Loadout: an immutable snapshot, pinned per run

A loadout is `{id, revision, version, summary, service descriptors}` — an immutable snapshot
resolved from the tenant configuration document (`internal/host.TenantConfig`). There is no second
configuration vocabulary: `internal/loadout/tenant.go` only maps the document, it does not define
its own (`docs/LOADOUT.md:51`).

The default blueprint is `loadout.DefaultServices()` (`internal/loadout/loadout.go:83`):

| Service | Provides | Requires | Optional | Runtime implementation | Default |
|---|---|---|---|---|---|
| `store` | `ctx.store` | — | — | read-only run mirror `agent.RunMirror` | always on |
| `llm` | `ctx.llm` | — | — | `provider.ResolveProvider` → `StreamFn` | always (empty model = fake) |
| `prompt` | `ctx.prompt` | — | `ctx.mcp`, `ctx.sandbox`, `ctx.external-tools` | `host.New` (prompt/skills/registry) | always on |
| `mcp` | `ctx.mcp` | — | — | `mcp.Manager` (connect/reconnect/close) | only with an mcp.json |
| `sandbox` | `ctx.sandbox` | — | — | **no implementation**: no factory | never (degraded diagnostic) |
| `tools` | `ctx.tools` | `ctx.prompt` | — | registry view of the same `host.Host` | always on |
| `pi-loop` | `ctx.loop` | `ctx.llm`, `ctx.tools` | `ctx.compaction` | `agent.Runner` (loop + kernel commit path) | always on |
| `compaction` | `ctx.compaction` | `ctx.llm` | `ctx.store` | `agent.CompactionOptions` | only with `compaction.enabled=true` |
| `session-projection` | `ctx.session-projection` | `ctx.store` | — | `kernel.Reader` (tenant-bounded reads) | always on |
| `telemetry` | `ctx.telemetry` | — | — | `telemetry.Memory` | only when `telemetry.adapter` is set |
| `worker` | `ctx.worker` | `ctx.store`, `ctx.loop`, `ctx.prompt`, `ctx.tools` | `ctx.telemetry` | `agent.Worker` claim loop | off with `-no-worker` |
| `external-tools` | `ctx.external-tools` | — | — | `externalgw.ToolService` | only when `external.tools` is set |
| `external-agent` | `ctx.external-agent` | `ctx.store` | — | `externalgw.AgentService` | only when `external.backends` is set |

Two deliberate structures (`docs/LOADOUT.md:42`):

- **`prompt` and `tools` share one host object.** Skills, built-in tools, MCP tools and external
  ToolProvider tools all register in the `host.Host` registry; `tools` is a view of it, not a
  second registry (otherwise `RefreshMCP` would fork the registries). Hence `tools` requires
  `prompt`, and `external-tools` is an optional dependency of `prompt`.
- **`sandbox` does not fabricate an empty factory.** An unimplemented service is simply an optional
  provider in the blueprint; dependents record a degraded diagnostic instead of half-starting.

**Pinning.** Every claimed run calls `Engine.Acquire()` and gets a lease carrying
generation/revision; `pi-loop` is resolved from that lease (not from a global lookup), and
`FinishRequest.Loadout` records `loadout_id`/`revision`/`generation`/`owner_epoch` in the run row
(`internal/kernel/schema/0005-loadout-revision.sql`, `docs/LOADOUT.md:99`). The read-only query
`GET /v1/sessions/{id}/run/loadout` returns the pin.

**Reload.** `Runtime.Reload` applies a new document: new runs use the new generation; in-flight
runs keep the old instances; the old generation enters `RETIRING` (accepts no new runs) and is
unloaded in reverse order once references drop or the retire deadline passes. Past the deadline,
`CollectExpired` force-unloads and marks still-held leases as broken — the holder must fail its run
with a `drain-expired` diagnostic (`internal/servicehost/host.go:691`, `docs/LOADOUT.md:109`).
A host ticker calls `CollectExpired` periodically so an idle process does not keep a dead
generation's effects around (`docs/LOADOUT.md:114`).

**Conformance gate (fail-closed).** Any service providing `ctx.loop` must present a PI conformance
manifest whose canonical hash equals the service's pin; an undeclared, unpinned or drifted loop is
rejected before the loadout exists (`internal/loadout/conformance.go:55`). The default `pi-loop`
pins `PinnedPiLoopManifestHash`; changing the suite requires deliberately re-pinning and bumping
the loop service version.

## 5. Handoff: how the workbench is swapped

Go services are compiled into the binary — there is no in-process hot-plug. The upgrade primitive
is the shift change, and it is defined step by step in `docs/HANDOFF.md:75`:

1. The new runtime starts with the new loadout and reaches `ready`.
2. The old runtime enters **handoff drain**: it stops claiming new runs, but keeps accepting
   edge-scoped input (`user.message` events still commit and wake runs).
3. In-flight runs are drained to a safe point: submitted facts are kept, uncommitted partials
   (a half-streamed message, a running tool) are dropped at the deadline; the lease is released.
4. The new runtime claims with `epoch+1` and rebuilds from the ledger.
5. SSE clients reconnect with `Last-Event-ID`; the first frame is `cursor+1`.

The five safety points (`docs/HANDOFF.md:100`) and the invariants (`docs/HANDOFF.md:110`) are the
acceptance criteria: user message committed → continue from that seq; assistant tool call committed
but result missing → the tool is re-executed (at-least-once, exactly one final result); tool result
committed → continue without re-executing; mid-model-stream → drop the partial, no half message;
mid-tool-execution → retry by idempotency key until it converges. Invariants: 0 lost events, 0
duplicated final replies (one assistant per `generationId`), 0 double tool results, a monotonic SSE
cursor, and every run converges to `done` or `failed`.

Fencing is what makes this safe: an old owner's `Commit` carries the epoch it claimed under; after
the new owner claims, the epoch is higher and old-epoch commits are refused. An owner that gets
refused must stop retrying and exit — it must not repackage local results as a "new commit"
(`docs/HANDOFF.md:93`). The kernel rejects stale writers by re-reading owner+epoch under the
session row lock at the linearisation point (`docs/SERVICE-GRAPH.md:199`).

**Upgrade discipline:** drain the old process first, then start the new binary. Old binaries do not
pass through the fence (`store.AppendEvent` does not look at the epoch), so a vanished writer can
add unfenced rows while a new owner holds the lease. Shutdown drain (SIGTERM) differs from handoff
drain: shutdown keeps the existing semantics of refusing new writes and abandons in-flight runs
(no finish, no release), so their lease expires after a bounded TTL before the new process can take
over (`docs/HANDOFF.md:24`).

## 6. Backpressure: bandwidth is the explicit ceiling

Three mechanisms stack, each owning one layer (`docs/BACKPRESSURE.md:16`):

| Mechanism | Backpressure source | Governs | Does not govern | Cost |
|---|---|---|---|---|
| (a) blocking `Write` (`io.Copy`) | kernel sndbuf + peer receive window | per-stream resident memory (naturally bounded) | total bandwidth, fairness, dead clients | 0 |
| (b) global/tenant token bucket | userspace accounting | total bandwidth cap, fairness, observable queueing | a single slow client | one `WaitN` per chunk |
| (c) `SetWriteDeadline` | timeout | dead/stalled clients, goroutine leaks | bandwidth total | one syscall per write |

The recommended order on the data path is **(b) bucket → (c) deadline → (a) Write**; the bucket
charges post-base64 wire bytes, and its burst must be at least the largest single write (87,384
bytes for a 64KiB chunk → burst 176KiB, rate ≈ link × 0.88).

**Lane pooling by size** (`docs/BACKPRESSURE.md:57`): three classes — Small <256KiB,
Medium <2MiB, Large ≥2MiB — with soft reservations (640/300/60) and hard caps (900/400/80). A
request is admitted iff its class is below its hard cap *and* reserving the other classes' unmet
soft quotas still fits. This avoids both starvation (a single priority queue with shortest-job-first
starves large requests) and wasted capacity (a class below its soft quota can borrow up to its hard
cap). Queue capacity 256; overflow answers **429 + `Retry-After`**.

**Streaming pipeline**: blobs are read in 64KiB chunks, base64-encoded as whole blocks
(4 base64 chars ≡ 3 source bytes, which is what makes any wire offset resumable), and written with
`io.Copy`; the per-stream budget is ~227KiB plaintext / ~245KiB TLS, so 1000 live streams cost
~390MB of userspace plus ~128MB of kernel socket buffers (`docs/BACKPRESSURE.md:108`). The details,
the PostgreSQL query discipline (keyset batches, no long-lived cursors) and the measured numbers are
on [Storage and runtime](/core/storage).

## 7. What is replaceable, and what is not

| Not replaceable (contract kernel) | Replaceable (service graph) |
|---|---|
| event order, `seq`, one writer per session | agent loop (default `pi-loop`, conformance-gated) |
| tool call ↔ result pairing | prompt assembly |
| `requestId` / `generationId` / `toolCallId` idempotency | tool registration and execution |
| lease / ownership / fencing epoch | LLM provider |
| tenant isolation, quota enforcement | session reads/projection (appends still go through the kernel) |
| crash-recovery truth | compaction, MCP, telemetry, sandbox |
| the default loop's PI behaviour contract | external AgentBackend / ToolProvider |

**Boundary rule: a service may be replaced, but a service may not change the ledger.**
