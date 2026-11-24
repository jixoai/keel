# Harness

> **Harness** is keel's product layer. It turns the Core's event ledger and loop into a
> usable agent product: sessions, turns, tools, approvals, plugins, triggers and the
> client surfaces that speak to them. Its product entities are **Account**, **Role**,
> **Profile**, **Bot** and **TeamBot**: a Bot is a runnable AI employee
> (`Profile + ModelKind + Harness`), and a TeamBot is a mission plus a roster of Bots.
> The old "Agent"/`TeamAgent` naming is deleted (alpha zero compatibility); the protocol
> names **AgentAPI** and **AgentBackend** are contracts and are kept
> (see the [glossary](/glossary)).

The Harness is not a new runtime. It is a **host process** (`keel serve`) assembled
on top of the Core, plus a set of product primitives that never leak into the
kernel. Everything in this section describes the same binary you get from
`cmd/keel`.

## 1. What the Harness owns — and what it does not

The Core packages (`internal/kernel`, `internal/loop`, `internal/agentcore`,
`internal/agenttool`, `internal/agent`) know only events, leases, tools and the
transcript. Everything that is *policy* lives in the Harness and is layered on
top: plugin assembly, hooks, approvals, triggers, sub-agents and token-level
partials (`internal/harnessext/doc.go:1`).

| Question | Answered by | Citation |
| --- | --- | --- |
| May this event be appended? | Core (contract kernel) | `internal/kernel/validate.go:46` |
| Who owns the session right now? | Core (leases + fencing epoch) | `internal/kernel/lease.go:16` |
| What does the model see? | Harness host assembly + the loop | `internal/agent/runner.go:280` |
| Which tools exist for this run? | Harness (host registry + plugin tools) | `internal/agenttool/registry.go` |
| May this tool run? What did it see? | Harness (hooks, approvals, policy) | `internal/harnessext/hooks.go:18` |
| Which plugins are active? | Harness (plugin runtime) | `internal/harnessext/plugin.go:163` |
| What does the client display? | Harness projection (`@keel/chat-surface`) | `packages/chat-surface/src/types.ts` |

The dependency direction is one-way: **Harness → Core**. A kernel package is
never allowed to import a product package, and a test enforces it
(`internal/architecture/boundary_test.go`).

## 2. Relation to Core

The Core section of this site covers the ledger, the kernel verbs, the service
graph and loadout, storage and handoff. For the Harness the essential Core
facts are:

- **The event log is the only truth.** Every state change is an append-only
  event; checkpoints and projections are derived (`docs/KERNEL.md`).
- **One writer per session.** A run may only commit under a live lease and the
  current fencing epoch (`internal/kernel/lease.go:71`, `:89`).
- **The service graph assembles, the ledger decides.** Services declare
  `provides`/`requires` and cleanup effects; they never write the event table
  (`docs/SERVICE-GRAPH.md` §2).
- **Runs pin a loadout.** Each run records `loadout_id`/`revision`/`generation`,
  so a configuration change never disturbs an in-flight run
  (`internal/agent/worker.go:70`, `docs/LOADOUT.md` §4).

The Harness consumes the Core through narrow seams — `kernel.Committer`,
`kernel.Reader`, `kernel.LeaseManager`, `kernel.ExternalSessions`
(`internal/kernel/seams.go`), plus the per-run binding supplied by the service
graph (`internal/agent/worker.go:73`). No Harness component holds a raw write
handle to the events table (`docs/LOADOUT.md` §7).

## 3. A process view

`keel serve` is the single process that hosts the Harness. It boots the store,
the contract kernel, the service graph (default loadout) and the product layer
in that order, and it owns every shutdown order (`cmd/keel/serve.go:75`,
`:213`, `:430`).

```
keel serve (one process)
├─ HTTP edge            /v1/tenants, /v1/sessions/{id}/input, /v1/sessions/{id}/events[/stream],
│                       /v1/files, /v1/runtime/{drain,status}      internal/httpapi/server.go:146
├─ Product API (Harness API)
│   ├─ discovery        GET /v1/capabilities                        internal/harnessapi/api.go:257
│   ├─ sessions         GET /v1/sessions, POST …/{id}/cancel        internal/harnessapi/api.go:171
│   ├─ decisions        GET/POST /v1/sessions/{id}/decisions[/…]    internal/harnessapi/api.go:173
│   ├─ plugins          GET /v1/plugins, POST …/install|{id}/enable|disable|uninstall|update,
│   │                   GET …/audit, POST /v1/plugin-approvals/{id} internal/harnessapi/plugins.go:56
│   ├─ plugin UI        GET /plugins/{id}/*, /v1/plugins/{id}/bridge/* cmd/keel/plugins_ui.go:17
│   ├─ triggers         GET /v1/triggers, POST …/{id}/fire          internal/harnessapi/ext.go:39
│   └─ sub-agents       GET /v1/subagents, POST …/{id}/subagents    internal/harnessapi/ext.go:128
├─ Extension surface (harnessext)
│   ├─ plugin runtime   declarative plugins, generation, reverse unload   internal/harnessext/plugin.go:163
│   ├─ plugin manager   manifest packages, lifecycle, permissions, audit  internal/harnessext/plugin_manager_ops.go:504
│   ├─ hook bus         closed event set, async, bounded queue            internal/harnessext/hooks.go:298
│   ├─ decisions        approval/clarify primitive with durable rows      internal/harnessext/decisions.go:233
│   ├─ triggers         interval/cron/webhook + idempotency + audit       internal/harnessext/triggers.go:305
│   ├─ sub-agents       child sessions with budgets and structured result internal/harnessext/subagent.go:175
│   ├─ run control      cancel registration for the executing attempt     internal/harnessext/turns.go:223
│   └─ partials         process-local token deltas (never in the log)     internal/harnessext/turns.go:360
├─ Host assembly        system prompt, skills, tool registry, MCP         internal/host/host.go
├─ Service graph        store/llm/prompt/mcp/tools/pi-loop/…              internal/loadout/loadout.go:63
├─ Worker               claim → execute → settle                          internal/agent/worker.go:170
└─ Static UI host       web/dist (NSH web build), /plugins/<id>/          cmd/keel/static.go:16
```

## 4. Lifecycle of one session

The diagram below is the whole truth path for one session, from the client's
first input to a settled run — including what happens when the process dies.

```
client                     Harness host (keel serve)                 Core (kernel + ledger)
  │                              │                                        │
  │ POST /v1/sessions            │                                        │
  ├─────────────────────────────▶│ create session row + session        ───▶│
  │                              │                                        │
  │ POST /v1/sessions/{id}/input │                                        │
  ├─────────────────────────────▶│ commitUserInput (edge class)        ───▶│ validate → seq → queue run (one tx)
  │   201 {seq, requestId}       │◀───────────────────────────────────────│  (200 = duplicate requestId;
  │   202 = committed but queued │                                        │   202 + queued{position,etaSeconds})
  │                              │                                        │
  │                              │ worker poll loop                       │
  │                              ├─ claim() ─────────────────────────────▶│ lease + fencing epoch
  │                              │  runner.RunAttempt():                  │
  │                              │   1 load events ──────────────────────▶│
  │                              │   2 record read watermark ────────────▶│
  │                              │   3 resolve model / session selection  │
  │                              │   4 fill missing tool results          │
  │                              │   5 loop (model stream, tool batches)  │
  │                              │        commit assistant.message ──────▶│ dedupe by generationId
  │                              │        commit tool.result ────────────▶│ paired by toolCallId
  │                              │   6 commit run.finished (summary) ────▶│
  │                              │   7 finish() ─────────────────────────▶│ settle / re-queue unread input
  │ GET /…/events/stream         │                                        │
  ├─────────────────────────────▶│ SSE: committed events (id: = seq)     ◀─│ LISTEN/NOTIFY wake
  │   ?partials=1                │ SSE: partial frames (no id)            │  (process-local hub)
  │                              │                                        │
  │ (crash: kill -9)             │                     lease expires ─────▶│ next claim takes over (epoch+1)
  │                              │                     successor rebuilds from the ledger
```

Two properties are load-bearing and should be read in order:

1. **Commit before display.** Nothing a client shows is invented locally; the
   projection consumes committed events (`packages/chat-surface/src/events.ts`),
   and optimistic input is reconciled by `requestId`
   (`packages/chat-surface/src/engine.ts`). See
   [Anatomy of a turn](/harness/anatomy-of-a-turn).
2. **Recover, don't resume.** A killed process is replaced by a new claim that
   rebuilds the transcript from the ledger and re-executes only what is
   missing (tools are at-least-once). See
   [Recovery and compaction](/harness/recovery-and-compaction).

## 5. Implemented, partly implemented, deferred

This site uses three tiers, and every row below names the tier it is in:

- **Implemented** — ships in this repository; the claim carries a `path:line`
  citation.
- **Partly implemented** — the mechanism ships, but a named part is missing or
  narrower than the contract (the gap is stated).
- **Deferred** — specified and registered, not in this build; tracked in
  `plugins/COVERAGE.md` and, where a gate was deferred, in the change's
  `DISPOSITION.md`.

| Area | Status | Where |
| --- | --- | --- |
| Ledger, kernel verbs, recovery, handoff | Implemented | [Anatomy of a turn](/harness/anatomy-of-a-turn), [Recovery and compaction](/harness/recovery-and-compaction) |
| Streaming, partials, drain frame; queued admission (`202` + `queued{position,etaSeconds}`, `429` only for tenant quota / memory cap) | Implemented | [Streaming and partials](/harness/streaming-and-partial) |
| Decisions (approvals/questions) | Implemented | [Approvals and questions](/harness/approvals-and-questions) |
| In-process declarative plugins (`harnessext.Plugin`) | Implemented | [Plugin host](/harness/plugins/) |
| MCP Apps host (catalogue, sandbox, bridge, events/era) | Implemented (P1–P5; P6 acceptance in progress) | [MCP Apps](/harness/mcp-apps) |
| `plugin.yaml` packages, install/lifecycle API, sidecar and webview kinds | Implemented; `agent_backend` sidecars Deferred | [Manifest](/harness/plugins/manifest), [Kinds](/harness/plugins/kinds), [Lifecycle](/harness/plugins/lifecycle) |
| Plugin permissions and audit endpoint | Partly implemented: tools + sidecar env enforced; `net`/`secrets` gates not routed; `fs` declaration-only | [Permissions](/harness/plugins/permissions) |
| Plugin UI bridge (`keel.plugin.*`), `/plugins/<id>/` hosting | Implemented (backend + client open path); client install/update cards not wired | [Plugin UI](/harness/plugins/ui) |
| Creation mode (`keel.creation`) | Implemented actions/template; end-to-end gate G8 Deferred | [Creation mode](/harness/creation-mode) |
| Official plugin suite coverage matrix | Implemented (catalog + CI gate); per-plugin status is the matrix's own vocabulary | [Plugins overview](/harness/plugins/) |

`internal/httpapi/server.go:497` is the status-code truth for input: `201` when
the submission is created, `200` when the same `requestId` is replayed, `202`
when the input is durable but its run waits for a group slot, and `429` only for
tenant quota or the in-memory cap (`internal/httpapi/server.go:924`).

## 6. Where to go next

- [Architecture](/harness/architecture) — the layers, the service graph, and
  where each primitive lives.
- [Anatomy of a turn](/harness/anatomy-of-a-turn) — a single input, step by
  step, with the event sequence.
- [Streaming and partials](/harness/streaming-and-partial) — SSE, resume
  cursors, backpressure and the token-level bypass.
- [Approvals and questions](/harness/approvals-and-questions) — the decision
  primitive.
- [Recovery and compaction](/harness/recovery-and-compaction) — crashes,
  leases, handoff, context compaction.
- [Plugins](/harness/plugins/) — the plugin host, the package standard, and
  the DSH comparison.
- [MCP Apps](/harness/mcp-apps) — interactive tool results: app declaration,
  sandboxing, bridge actions and degradation.
- [Creation mode](/harness/creation-mode) — natural language to a running
  plugin (actions shipped; the end-to-end gate is Deferred).
- [Operations](/harness/operations) — serve, TUI, diagnostics, upgrades.
