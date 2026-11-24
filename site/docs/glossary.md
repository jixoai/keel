---
# The glossary is a single page: a sidebar rail holding one entry would just be
# an empty column, so this page runs without a sidebar.
sidebar: false
---

# Glossary

> Naming rule of this project: the product layer is called **Harness**; its
> product entities are **Bot**, **TeamBot**, **Account**, **Role** and
> **Profile**. The old product word "Agent" and `TeamAgent` are deleted (alpha
> zero compatibility), not aliases. The protocol names **AgentAPI** and
> **AgentBackend** are contracts and are kept as-is — renaming them would break
> clients and adapters. PI's technical packages (`agentcore`, `agentloop`,
> `agenttool`, `internal/agent`) keep their names too. Every page on this site
> follows this rule, and a scan script (`scripts/naming-scan.sh`) checks it.

## Product vocabulary

| Term | Definition | First-hand source |
| --- | --- | --- |
| **Keel** | The project: a stateless, multi-tenant agent runtime base. | `README.md` |
| **Core** | The contract kernel + event ledger + service graph + loadout + storage + handoff. The part that cannot be swapped. | `docs/SERVICE-GRAPH.md`, `docs/KERNEL.md` |
| **Harness** | The product layer over the Core: sessions, turns, tools, approvals, plugins, triggers, sub-agents, and the client surfaces. The process is `keel serve`. | `internal/harnessext/doc.go`, `docs/HARNESS.md` |
| **Account** | The login and permission subject; human, bot and service accounts share one model. | `accounts` table, `/v1/accounts`, `internal/botteam` |
| **Role** | Identity and soul: name, instruction, voice, default behavior. It grants **no** permission. | `roles` table, `/v1/roles` |
| **Profile** | The `Account + Role` binding a Bot runs under. | `profiles` table, `/v1/profiles` |
| **Bot** | A runnable AI employee: `Profile + ModelKind + Harness`; a Bot is a permission subject and owns Sessions. | `bots` table, `/v1/bots`, `keel bot` |
| **TeamBot** | Mission + roster of Bots. It is a resource, not a permission subject; it owns no Session/Account/Profile. | `team_bots` table, `/v1/team-bots`, `keel team-bot` |
| **AgentAPI** | The `/v1` product API contract (alias: Harness API). Name kept; it is a wire contract consumed by the web UI and TUI. | `internal/harnessapi/api.go` |
| **AgentBackend** | The protocol a foreign agent runtime (for example DSH) implements to be driven as an external backend. Name kept. | `internal/gateway/protocol.go`, `docs/DSH-REFERENCE.md` |
| **DSH** | DeepSeek Harness — an external reference for the plugin model and an external-agent target. A reference, not this project's source of truth. | `docs/DSH-REFERENCE.md` |

## Session and run vocabulary

| Term | Definition | Source |
| --- | --- | --- |
| **Ledger** | The append-only event log; the only truth. Projections and checkpoints are derived from it. | `docs/KERNEL.md` |
| **Event kind** | The closed vocabulary of durable facts — **25 registered kinds**: `user.message`, `assistant.message`, `tool.result`, `compaction`, `run.started`/`run.failed` (reserved), `run.finished` (carries a `run` key), `external.event`, the six `task.*` kinds, `fs.write`/`fs.capture`/`fs.capture_failed`, and the system kinds `config.rotated`, `placement.joined`/`placement.left`, `supervision.escalated`, `guardrail.tripped`, `resume.attempt`, `notification.ack`/`notification.push_sent`. | `internal/kernel/validate.go:46` |
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
| **Decision** | The approval/question primitive: durable row, idempotent answer, bounded timeout. Not a kernel event kind. | `internal/harnessext/decisions.go:233` |
| **Trigger** | A scheduled or webhook-started turn (`interval`/`cron`/`webhook`) with idempotency keys and an audit trail. | `internal/harnessext/triggers.go:30` |
| **Sub-agent** | A child session spawned by a tool or API call, with a budget and a structured result. | `internal/harnessext/subagent.go:21` |

## Assembly vocabulary

| Term | Definition | Source |
| --- | --- | --- |
| **Service graph** | The assembly of first-party services: capability-based dependency resolution, ordered activation, reverse-order teardown. | `internal/servicegraph/lifecycle.go:21` |
| **Loadout** | An immutable snapshot of the assembled service set `{id, revision, version, summary, services}`, pinned per run. | `docs/LOADOUT.md` §1 |
| **Generation** | A counter incremented once per reassembly; instances from an old generation serve in-flight runs and are retired afterwards. | `internal/servicegraph/engine.go:270`, `docs/LOADOUT.md` §4 |
| **Selection** | The model/provider choice recorded with a run; validated against the current generation's provider configuration. | `internal/modelcfg`, `internal/agent/runner.go:321` |
| **Assembly / row** | The configuration stack above the service graph: base → mode → preset → `config.toml`, overlaid by row id (`<plugin-id>#<row>`), last write wins; only base may add rows. | `docs/ASSEMBLY.md:41`, `docs/ASSEMBLY.md:53` |
| **Plugin** | A Harness extension packaged as a `plugin.yaml` of one of three kinds (`in-process`/`sidecar`/`webview`), installed into a managed root and enabled through the plugin manager; lifecycle states `installed/enabled/disabled/failed/invalid` (`uninstalled` survives only in the audit trail). | `internal/pluginpkg/manifest.go:142`, `internal/harnessext/plugin_manager.go:31` |
| **`plugin.yaml`** | The plugin package manifest: id/version/kind/layer/description/engines/provides/contributes/extensibility/requires/permissions/entry/source, validated strictly on load (an unknown field is an error, not a no-op). | `internal/pluginpkg/manifest.go:142` |
| **in-process / sidecar / webview** | The three plugin kinds, all loadable: compiled into the host / an external process speaking the gateway's stdio line-JSON-RPC as a `tool_provider` / a UI package served on the product origin at `/plugins/<id>/` for enabled plugins only. | `internal/harnessext/plugin_sidecar.go:22`, `cmd/keel/plugins_ui.go:17` |
| **`keel.plugin.*`** | The bridge a webview plugin uses to read its own state, call its own tools, request an approval and subscribe to events — and nothing else (`state`/`call`/`approve`/`subscribe`, plus theme tokens). | `internal/harnessapi/plugin_bridge.go:75` |
| **Creation mode** | The built-in plugin `keel.creation`, enabled by default: natural language → generated webview plugin → install → enable → deep link, with approval and rollback. Six actions — `catalog`/`scaffold`/`validate`/`install`/`list`/`remove`; the G8 end-to-end acceptance is still deferred and is not claimed as done. | `internal/harnessext/creation.go:91`, `internal/pluginpkg/builtin/keel.creation.yaml:1` |

## DSH vocabulary mapped (reference only)

| DSH term | Meaning there | Mapping here |
| --- | --- | --- |
| `apply(ctx)` | a plugin's entry point receiving injected services | `Plugin.Start(ctx, *PluginEnv)` |
| inject / DI | services resolved and injected by the runtime | service graph bindings + `PluginEnv` |
| effect | a reversible side effect owned by a plugin | `Plugin.Effects` released in reverse order |
| bundle | a patch layer (config + plugins) | an assembly row: a `<plugin-id>#<row>` impl+params override, last write wins (`internal/assembly`, `docs/ASSEMBLY.md:53`) |
| profile | an ordered stack of bundles | loadout (immutable snapshot, pinned per run) |
| Cordis | the TS plugin runtime | **not ported** — `harnessext` + `servicegraph` express the same semantics in Go |
