# Harness architecture

This page describes where every Harness capability lives, how the pieces are
assembled in one process, and which boundaries are load-bearing. It assumes the
Core concepts (ledger, kernel verbs, service graph, loadout) — see the Core
section for those.

## 1. Layers

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ Clients                                                                      │
│  web UI (NSH web build) · TUI (NSH tui target) · any HTTP client             │
│  shared projection: @keel/chat-surface (zero-DOM state machine)              │
└───────────────────────────────┬──────────────────────────────────────────────┘
                                │ HTTP + SSE (one origin; no CORS by construction)
┌───────────────────────────────▼──────────────────────────────────────────────┐
│ HTTP edge + AgentAPI (internal/httpapi, internal/agentapi)                    │
│  edge: tenants/sessions/input/retry/events/stream/files/runtime drain        │
│  product: capabilities, session list, cancel, decisions, plugins/triggers/   │
│           sub-agents, settings surface                                       │
└───────────────────────────────┬──────────────────────────────────────────────┘
                                │ seams: kernel.Committer/Reader/LeaseManager,
                                │ agentext primitives, service bindings
┌───────────────────────────────▼──────────────────────────────────────────────┐
│ Harness product primitives (internal/agentext)                               │
│  plugin runtime · hook bus · decision broker · triggers · sub-agents ·       │
│  run control · partial hub                                                   │
├──────────────────────────────────────────────────────────────────────────────┤
│ Host assembly (internal/host) + agent loop integration (internal/agent)       │
│  system prompt sections · skills · tool registry · MCP tools · telemetry     │
├──────────────────────────────────────────────────────────────────────────────┤
│ Service graph (internal/servicegraph + internal/loadout)                      │
│  descriptors, dependency resolution, lifecycle, generations, per-run leases  │
├──────────────────────────────────────────────────────────────────────────────┤
│ Contract kernel + ledger (internal/kernel) · loop (internal/loop) ·           │
│ content model (internal/agentcore) · tool execution (internal/agenttool)      │
│  event kinds, idempotency keys, leases/fencing, run rows, store projections  │
└───────────────────────────────┬──────────────────────────────────────────────┘
                                │
                       PostgreSQL (events, run rows, tasks, blobs) + local CAS
```

The order matters: each layer may use only the ones below it. The kernel-facing
rule is enforced mechanically — product packages must never become a dependency
of kernel packages (`internal/architecture/boundary_test.go`).

## 2. The host process and the service graph

`keel serve` is the only place that knows every layer. It boots in a fixed
order, and the order *is* the safety contract (`cmd/keel/serve.go:75`):

1. open the store and apply kernel migrations;
2. build the seam set (`Committer`/`Reader`/`Leases`/`External`, read-only run
   mirror, tenant quota, audit sink);
3. build the default tenant document from flags/environment
   (`cmd/keel/serve.go:181`), including the stored model-settings override;
4. `servicehost.Boot` → `loadout.FromTenantConfig` → `Engine.Apply` → assert
   every runtime-path service is `ACTIVE` (`cmd/keel/serve.go:213`);
5. start the SSE hub only after the runtime is up, so a boot failure cannot
   park a pool connection (`cmd/keel/serve.go:275`);
6. build the product layer (decisions, triggers, sub-agents, plugins) and mount
   the AgentAPI next to the HTTP edge (`cmd/keel/serve.go:282`, `:294`).

The default loadout is the assembly of record
(`internal/loadout/loadout.go:63`; documented in `docs/LOADOUT.md` §2):

| Service | Provides | Requires | Optional | Default |
| --- | --- | --- | --- | --- |
| `store` | `ctx.store` | — | — | always |
| `llm` | `ctx.llm` | — | — | always (empty model = fake provider) |
| `prompt` | `ctx.prompt` | — | `ctx.mcp`, `ctx.sandbox`, `ctx.external-tools` | always |
| `mcp` | `ctx.mcp` | — | — | only when `mcp.json` is configured |
| `sandbox` | `ctx.sandbox` | — | — | never (no factory; degradation is diagnosed) |
| `tools` | `ctx.tools` | `ctx.prompt` | — | always (registry view of the same host) |
| `pi-loop` | `ctx.loop` | `ctx.llm`, `ctx.tools` | `ctx.compaction` | always |
| `compaction` | `ctx.compaction` | `ctx.llm` | `ctx.store` | `compaction.enabled=true` |
| `session-projection` | `ctx.session-projection` | `ctx.store` | — | always |
| `telemetry` | `ctx.telemetry` | — | — | `telemetry.adapter` non-empty |
| `worker` | `ctx.worker` | store/loop/prompt/tools | `ctx.telemetry` | process-level `-no-worker` |
| `external-tools` | `ctx.external-tools` | — | — | `external.tools` non-empty |
| `external-agent` | `ctx.external-agent` | `ctx.store` | — | `external.backends` non-empty |

Two deliberate shapes: `prompt` and `tools` share one `host.Host` object (there
is no second registry for MCP/tool refresh to fork), and `sandbox` has no stub
factory — an unimplemented service is an optional provider that does not exist,
so dependents degrade with a diagnostic instead of half-starting
(`docs/LOADOUT.md` §2).

### Lifecycle and generations

Service instances move through
`PENDING → LOADING → ACTIVE → RETIRING → UNLOADING → PENDING`; a failed
activation lands in `FAILED` and can be retried by a later `Apply`
(`internal/servicegraph/lifecycle.go:21`). Every `Apply` increments the
generation exactly once; a service whose assembly identity
`(name, version, provides, requires, optional, role)` is unchanged is **shared
across generations** instead of rebuilt (`docs/LOADOUT.md` §4).

Each claimed run *acquires* a lease from the engine
(`internal/servicegraph/engine.go:270`) and pins the generation: the run keeps
the loop instance it started with even if the host reassembles services
later. A retiring generation accepts no new runs and is unloaded in reverse
dependency order once its references drain, or forcibly after the retire
deadline — its leases are then marked broken and the holder must fail the run
with a `drain-expired` diagnostic (`internal/agent/worker.go:242`,
`internal/servicegraph/engine.go:303`, `:650`). A host ticker calls
`CollectExpired` periodically so an idle process never accumulates old
generations (`docs/LOADOUT.md` §4).

## 3. The plugin host

The plugin host is the `Harness`'s extension runtime. Today it is
`agentext.Runtime`: an **in-process, declarative** contract.

```
agentext.Runtime
├─ assemble(): unload previous assembly → generation+1 → activate in dependency order
│    ├─ duplicate tool names across plugins → fail-closed diagnostic
│    ├─ requires not satisfied → PENDING with "waiting for: …"
│    └─ Start(ctx, env) → hooks Subscribe → triggers Register → tool adaptation
├─ publishTools(): every active plugin's tools go to the host registry via ToolSink
│    └─ in keel serve: sink = rt.RefreshProductTools     cmd/keel/serve.go:464
├─ Snapshot(): generation, plugin states, hooks stats, tool names   plugin.go:505
└─ Close(): unload everything in reverse order, then publish an empty tool set
```

Citations: plugin contract `internal/agentext/plugin.go:67`; assembly
`internal/agentext/plugin.go:209`; activation `:317`; reverse cleanup `:387`;
unload `:417`; snapshot `:505`. The current plugin set in `keel serve` is two
built-ins: `keel.example` (`internal/agentext/builtin.go:38`) and
`keel.subagents` (`internal/agentext/builtin.go:256`).

What this host gives you today: dependency-ordered activation, reverse-order
reversal of every declared side effect (hooks subscriptions, trigger
registrations, effects), generation counting, observable states and
diagnostics, and a fail-closed rule for duplicate tool names. What it does
*not* have: manifests, installation, sandboxing, permissions or UI hosting —
those are the planned `plugin.yaml` layer described in
[Plugins](/harness/plugins/). Until then, "a plugin" means a Go value compiled
into the binary.

## 4. Tools and approvals

Tools are the only way the model touches the world, so their path is narrow and
auditable.

```
model stream (provider)
   │ assistant.message with toolCalls
   ▼
internal/loop            turn structure, one tool batch per assistant message
   ▼
internal/agenttool      batch execution (parallel by default, sequential when declared),
   │                     result pairing by toolCallId, result trimming
   ▼
agenttool.ToolRegistry  lookup by name (host registry: builtin + skills + MCP + plugin tools)
   ▼
tool.result commit      kernel pairs it with the call; duplicates deduped by toolCallId
```

- The registry is owned by the host assembly (`internal/host`), and the
  `tools` service is a view of it, not a copy (`docs/LOADOUT.md` §2).
- Execution is **at-least-once** across crashes: a call committed without its
  result is re-executed by the next attempt, and the kernel keeps exactly one
  result per `toolCallId` (`internal/agent/runner.go:829`, `:937`). Tools that
  have side effects are expected to carry their own idempotency keys
  (`docs/ARCHITECTURE.md` §3).
- Tool results can be large; the executor trims them and the transcript stays
  bounded (`internal/agenttool/utf8trim.go`).

**Approvals** are not a property of the tool executor; they are the decision
primitive of the product layer (`internal/agentext/decisions.go:233`). A run
that needs a human answer opens a decision, waits with a bounded timeout
(default 5 minutes, `internal/agentext/decisions.go:162`), and continues or
fails on the answer. Decisions are durable rows in the tasks bookkeeping table,
**never a new event kind** — the kernel's vocabulary is unchanged
(`internal/kernel/validate.go:36`, `internal/agentext/doc.go`). See
[Approvals and questions](/harness/approvals-and-questions).

## 5. Projection

Clients never read the ledger directly and never assemble state from scratch:

```
committed events (GET /events, SSE /events/stream)
        │
        ▼
@keel/chat-surface   wire payload parsing   packages/chat-surface/src/events.ts
  │                  pure projection        packages/chat-surface/src/projection.ts
  │                  facade + reconnect     packages/chat-surface/src/engine.ts
  ▼
ConversationView     turns, work segments, queue, decisions, run summary,
                     connection state — the frozen contract in
                     packages/chat-surface/src/types.ts
        │
        ▼
NSH renderer         web screen / TUI target (same TSX)
```

The projection is deliberately *not* authoritative. It mirrors the log and
never invents facts: unread input is derived from `assistant.message.inputSeqs`
(see [Anatomy of a turn](/harness/anatomy-of-a-turn)), queue/guide state is
event-driven, and a stop is only confirmed by `run.finished`
(`packages/chat-surface/src/projection.ts`). Partials are the one exception
that proves the rule: token deltas travel beside the log, are never committed,
and are dropped the moment the committed message arrives
(`internal/agentext/streamtee.go:24`, `packages/chat-surface/src/engine.ts`).

The same projection powers both surfaces, which is why the web UI and the TUI
cannot drift apart: `keel tui` runs the same TSX against the same AgentAPI
(`docs/WEBUI.md`, `web/README.md`).

## 6. What can be replaced, what cannot

| Replaceable (assembly) | Not replaceable (contract) |
| --- | --- |
| agent loop (must pass PI conformance) | event order, seq, one writer per session |
| prompt assembly | tool-call ↔ result pairing |
| tool registry and execution | `requestId` / `generationId` / `toolCallId` idempotency |
| LLM provider | lease ownership and fencing epoch |
| session read projection | tenant isolation and quota enforcement |
| compaction, MCP, telemetry, sandbox | crash-recovery truth |
| external AgentBackend / ToolProvider | the default loop's PI behavior contract |

This table is the Harness counterpart of the Core's service-graph rule
(`docs/SERVICE-GRAPH.md` §3): *services may be swapped; services may never
change the ledger*. When a plugin wants to influence the ledger it must go
through the kernel's `Commit` like everyone else
(`internal/kernel/kernel.go:599`).
