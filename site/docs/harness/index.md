# Harness

> **Harness** is keel's product layer. It turns the Core's event ledger and loop into a
> usable agent product: sessions, turns, tools, approvals, plugins, triggers and the
> client surfaces that speak to them. The name replaces the earlier "Agent" naming;
> the protocol names **AgentAPI** and **AgentBackend** are contracts and are kept
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
partials (`internal/agentext/doc.go:1`).

| Question | Answered by | Citation |
| --- | --- | --- |
| May this event be appended? | Core (contract kernel) | `internal/kernel/validate.go:36` |
| Who owns the session right now? | Core (leases + fencing epoch) | `internal/kernel/lease.go:16` |
| What does the model see? | Harness host assembly + the loop | `internal/agent/runner.go:280` |
| Which tools exist for this run? | Harness (host registry + plugin tools) | `internal/agenttool/registry.go` |
| May this tool run? What did it see? | Harness (hooks, approvals, policy) | `internal/agentext/hooks.go:18` |
| Which plugins are active? | Harness (plugin runtime) | `internal/agentext/plugin.go:163` |
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
  (`internal/agent/worker.go:242`, `docs/LOADOUT.md` §4).

The Harness consumes the Core through narrow seams — `kernel.Committer`,
`kernel.Reader`, `kernel.LeaseManager`, `kernel.ExternalSessions`
(`internal/kernel/seams.go`), plus the per-run binding supplied by the service
graph (`internal/agent/worker.go:72`). No Harness component holds a raw write
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
├─ Product API (AgentAPI)
│   ├─ discovery        GET /v1/capabilities                        internal/agentapi/api.go:257
│   ├─ sessions         GET /v1/sessions, POST …/{id}/cancel        internal/agentapi/api.go:171
│   ├─ decisions        GET/POST /v1/sessions/{id}/decisions[/…]    internal/agentapi/api.go:173
│   ├─ plugins          GET /v1/plugins                             internal/agentapi/ext.go:18
│   ├─ triggers         GET /v1/triggers, POST …/{id}/fire          internal/agentapi/ext.go:39
│   └─ sub-agents       GET /v1/subagents, POST …/{id}/subagents    internal/agentapi/ext.go:128
├─ Extension surface (agentext)
│   ├─ plugin runtime   declarative plugins, generation, reverse unload   internal/agentext/plugin.go:163
│   ├─ hook bus         closed event set, async, bounded queue            internal/agentext/hooks.go:298
│   ├─ decisions        approval/clarify primitive with durable rows      internal/agentext/decisions.go:233
│   ├─ triggers         interval/cron/webhook + idempotency + audit       internal/agentext/triggers.go:305
│   ├─ sub-agents       child sessions with budgets and structured result internal/agentext/subagent.go:175
│   ├─ run control      cancel registration for the executing attempt     internal/agentext/turns.go:222
│   └─ partials         process-local token deltas (never in the log)     internal/agentext/turns.go:360
├─ Host assembly        system prompt, skills, tool registry, MCP         internal/host/host.go
├─ Service graph        store/llm/prompt/mcp/tools/pi-loop/…              internal/loadout/loadout.go:63
├─ Worker               claim → execute → settle                          internal/agent/worker.go:170
└─ Static UI host       web/dist (NSH web build), /plugins/<id>/ (planned) cmd/keel/static.go
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
  │   202 {seq, requestId}       │◀───────────────────────────────────────│
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

## 5. Implemented today vs planned

This site distinguishes **implemented** (with a `path:line` citation) from
**planned** (pointing at the in-development `keel-plugin-core` change). The
distinction is deliberate: the plugin standard below is a specification, not a
description of shipped code.

| Area | Status | Where |
| --- | --- | --- |
| Ledger, kernel verbs, recovery, handoff | Implemented | [Anatomy of a turn](/harness/anatomy-of-a-turn), [Recovery and compaction](/harness/recovery-and-compaction) |
| Streaming, partials, drain frame | Implemented | [Streaming and partials](/harness/streaming-and-partial) |
| Decisions (approvals/questions) | Implemented | [Approvals and questions](/harness/approvals-and-questions) |
| In-process declarative plugins (`agentext.Plugin`) | Implemented | [Plugin host](/harness/plugins/) |
| `plugin.yaml` packages, install, sidecar/webview kinds | Planned (`keel-plugin-core` W1) | [Manifest](/harness/plugins/manifest), [Kinds](/harness/plugins/kinds) |
| Plugin permissions, sandbox boundary, audit endpoint | Planned (`keel-plugin-core` W1) | [Permissions](/harness/plugins/permissions) |
| Plugin UI bridge (`keel.plugin.*`), plugin hosting | Planned (`keel-plugin-core` W1) | [Plugin UI](/harness/plugins/ui) |
| Creation mode (`keel.creation`) | Planned (`keel-plugin-core` W1) | [Creation mode](/harness/creation-mode) |
| Official plugin suite coverage matrix | Planned (W1/W2/W3 waves) | [Plugins overview](/harness/plugins/) |

> **Planned** in this site means: specified in
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md` and tracked in
> `plugins/COVERAGE.md`. Neither file is present in the current checkout — both
> arrive with the `keel-plugin-core` change. Anything not marked planned is
> implemented in this repository today.

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
- [Plugins](/harness/plugins/) — the plugin host, the standard, and the
  DSH comparison.
- [Creation mode](/harness/creation-mode) — natural language to a running
  plugin (planned).
- [Operations](/harness/operations) — serve, TUI, diagnostics, upgrades.
