---
# The glossary is a single page: a sidebar rail holding one entry would just be
# an empty column, so this page runs without a sidebar.
sidebar: false
---

# Glossary

> Naming rule of this project: the product layer is called **Harness** (formerly
> "Agent"). The protocol names **AgentAPI** and **AgentBackend** are contracts
> and are kept as-is — renaming them would break clients and adapters. Every
> page on this site follows this rule, and a scan script
> (`scripts/naming-scan.sh`) checks it.

## Product vocabulary

| Term | Definition | First-hand source |
| --- | --- | --- |
| **Keel** | The project: a stateless, multi-tenant agent runtime base. | `README.md` |
| **Core** | The contract kernel + event ledger + service graph + loadout + storage + handoff. The part that cannot be swapped. | `docs/SERVICE-GRAPH.md`, `docs/KERNEL.md` |
| **Harness** (formerly "Agent") | The product layer over the Core: sessions, turns, tools, approvals, plugins, triggers, sub-agents, and the client surfaces. The process is `keel serve`. | `internal/agentext/doc.go`, `docs/HARNESS.md` |
| **AgentAPI** | The `/v1` product API contract (alias: Harness API). Name kept; it is a wire contract consumed by the web UI and TUI. | `internal/agentapi/api.go:49` |
| **AgentBackend** | The protocol a foreign agent runtime (for example DSH) implements to be driven as an external backend. Name kept. | `internal/gateway/protocol.go`, `docs/DSH-REFERENCE.md` |
| **DSH** | DeepSeek Harness — an external reference for the plugin model and an external-agent target. A reference, not this project's source of truth. | `docs/DSH-REFERENCE.md` |

## Session and run vocabulary

| Term | Definition | Source |
| --- | --- | --- |
| **Ledger** | The append-only event log; the only truth. Projections and checkpoints are derived from it. | `docs/KERNEL.md` |
| **Event kind** | The closed vocabulary of durable facts: `user.message`, `assistant.message`, `tool.result`, `compaction`, `run.started`/`run.failed` (reserved), `run.finished`, `external.event`. | `internal/kernel/validate.go:36` |
| **seq** | The per-session event sequence number; the cursor clients resume from (`id: <seq>`). | `internal/httpapi/server.go:628` |
| **Session** | A conversation: one event log, at most one writer at a time. | `docs/KERNEL.md` |
| **Run / attempt** | One execution of a session's pending work. A run can be attempted several times (retry); each attempt is claimed, fenced and settled. | `internal/kernel/lease.go`, `internal/agent/worker.go:223` |
| **Claim / lease / epoch** | The kernel's ownership mechanism: an owner receives a lease with a fencing token `owner@epoch`; stale owners are refused. | `internal/kernel/lease.go:16` |
| **Turn** | One model request/response cycle inside a run; a turn may include one tool batch. | `internal/loop/loop.go` |
| **`inputSeqs`** | `assistant.message.payload.inputSeqs` — the user-message sequence numbers a request presented to the model. The consumption fact: an input is answered iff a completed assistant message references it. | `internal/agent/runner.go:707` |
| **Read watermark** | The highest committed seq an attempt loaded; input above it re-queues the session at settle. | `internal/agent/worker.go:399` |
| **`delivery: queue` / `guide`** | How a committed input is consumed: `queue` waits for the next claim; `guide` is injected into the running turn at a drain point (and degrades to `queue` when no attempt is live, visibly in the event). | `docs/KERNEL.md` §2 |
| **Partial** | A token-level delta streamed beside the log; never stored, never replayed, carries no cursor. | `internal/httpapi/partials.go:5` |
| **Compaction** | Replacing the oldest transcript span with a committed summary event; history is never edited. | `internal/compaction/compact.go` |
| **Handoff** | Cross-process succession: new runtime, drain the old one, new epoch, continue from the ledger. | `docs/HANDOFF.md` |
| **Decision** | The approval/question primitive: durable row, idempotent answer, bounded timeout. Not a kernel event kind. | `internal/agentext/decisions.go:233` |
| **Trigger** | A scheduled or webhook-started turn (`interval`/`cron`/`webhook`) with idempotency keys and an audit trail. | `internal/agentext/triggers.go:30` |
| **Sub-agent** | A child session spawned by a tool or API call, with a budget and a structured result. | `internal/agentext/subagent.go:44` |

## Assembly vocabulary

| Term | Definition | Source |
| --- | --- | --- |
| **Service graph** | The assembly of first-party services: capability-based dependency resolution, ordered activation, reverse-order teardown. | `internal/servicegraph/lifecycle.go:21` |
| **Loadout** | An immutable snapshot of the assembled service set `{id, revision, version, summary, services}`, pinned per run. | `docs/LOADOUT.md` §1 |
| **Generation** | A counter incremented once per reassembly; instances from an old generation serve in-flight runs and are retired afterwards. | `internal/servicegraph/engine.go:270`, `docs/LOADOUT.md` §4 |
| **Selection** | The model/provider choice recorded with a run; validated against the current generation's provider configuration. | `internal/modelcfg`, `internal/agent/runner.go:321` |
| **Plugin** | An extension of the Harness. Today: an in-process Go value (`agentext.Plugin`). Planned: a `plugin.yaml` package of one of three kinds. | `internal/agentext/plugin.go:67` |
| **`plugin.yaml`** | The planned plugin manifest (id/version/kind/engines/provides/requires/permissions/entry/source). | planned: `keel-plugin-core` spec |
| **in-process / sidecar / webview** | The three planned plugin kinds: compiled-in / external process over stdio JSON-RPC / hosted UI package. | planned: `keel-plugin-core` spec |
| **`keel.plugin.*`** | The planned bridge a webview plugin uses to read its own state, call its own tools, request an approval and subscribe to events — and nothing else. | planned: `keel-plugin-core` spec |
| **Creation mode** | The planned built-in plugin `keel.creation`: natural language → generated webview plugin → install → enable → deep link, with approval and rollback. | planned: `keel-plugin-core` spec |

## DSH vocabulary mapped (reference only)

| DSH term | Meaning there | Mapping here |
| --- | --- | --- |
| `apply(ctx)` | a plugin's entry point receiving injected services | `Plugin.Start(ctx, *PluginEnv)` |
| inject / DI | services resolved and injected by the runtime | service graph bindings + `PluginEnv` |
| effect | a reversible side effect owned by a plugin | `Plugin.Effects` released in reverse order |
| bundle | a patch layer (config + plugins) | planned enablement list; today the default loadout |
| profile | an ordered stack of bundles | loadout (immutable snapshot, pinned per run) |
| Cordis | the TS plugin runtime | **not ported** — `agentext` + `servicegraph` express the same semantics in Go |
