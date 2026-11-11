# Plugins

> **Status legend used throughout this section**
>
> - **Implemented** — ships in this repository today; every claim carries a
>   `path:line` citation that can be opened.
> - **Planned** — specified in the in-development change `keel-plugin-core`
>   (`openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`, coverage
>   matrix `plugins/COVERAGE.md`). Neither file exists in the current checkout;
>   both arrive with the change.
>
> The plugin *standard* below is the planned one. What exists today is a
> compiled-in, in-process plugin host — described honestly first.

## 1. The thesis: everything above the ledger is assembly

The design statement keel shares with DeepSeek Harness is "everything is a
plugin" — with keel's own correction: **the contract kernel is not**. The
ledger, event order, idempotency keys and fencing are the invariants that make
every other capability replaceable; they are implemented as a service graph
(general assembly concerns) plus a small in-process plugin runtime (product
extensions).

Two mechanisms express the same semantics in Go:

| Mechanism | What it assembles | Citation |
| --- | --- | --- |
| **Service graph** (`internal/servicegraph` + `internal/loadout`) | first-party runtime services: store, llm, prompt, tools, loop, compaction, projection, telemetry, external gateways | `internal/loadout/loadout.go:63` |
| **Plugin runtime** (`internal/agentext`) | product extensions: tools, hooks, triggers, and their cleanup | `internal/agentext/plugin.go:163` |

Both use the same vocabulary — `provides`/`requires`/`optional`, ordered
activation, **reverse-order reversal** of every registered side effect, a
generation counter, and observable diagnostics. A service whose assembly
identity is unchanged is shared across generations
(`docs/LOADOUT.md` §4); a plugin is re-assembled on every `Apply`
(`internal/agentext/plugin.go:209`).

The correlation with the DSH plugin model is deliberate but semantic, not
mechanical — see §4.

## 2. What is implemented today

The `agentext` plugin contract is a Go value:

```go
type Plugin struct {
    ID, Version string
    Provides    []string      // capabilities this plugin makes available
    Requires    []string      // capabilities it needs (else it stays PENDING)
    Tools       []ToolSpec    // model-facing tools
    Hooks       []HookSpec    // subscriptions to the closed event set
    Triggers    []TriggerSpec // interval / cron / webhook
    Effects     []Effect      // named cleanups, run in reverse order
    Start       func(ctx context.Context, env *PluginEnv) error
    Stop        func(ctx context.Context) error
}
```

(`internal/agentext/plugin.go:67`; `Start` `:78`, `Stop` `:80`, `Effect` `:51`,
`PluginEnv` `:57`.)

Guarantees the host provides, each with a test in
`internal/agentext/agentext_test.go` / `e2e_test.go`:

- **dependency topology** — activation proceeds in dependency order; a plugin
  whose requirement is missing stays `PENDING` with a `waiting for: …`
  diagnostic instead of half-loading (`internal/agentext/plugin.go:291`);
- **reversible registration** — hook subscriptions, trigger registrations and
  declared effects are released in exact reverse order; an unload cannot leave
  a listener, timer or tool behind (`internal/agentext/plugin.go:386`,
  `:417`);
- **fail-closed tool namespace** — two plugins declaring the same tool name
  make the second plugin `FAILED`; the model never sees an ambiguous name
  (`internal/agentext/plugin.go:236`);
- **generation** — every `Apply` increments the generation once; the snapshot
  reports it (`internal/agentext/plugin.go:213`, `:505`);
- **tool publishing** — after every assembly the active tool set is pushed into
  the host registry through one sink (in `keel serve`:
  `rt.RefreshProductTools`, `cmd/keel/serve.go:464`).

Out of the box, `keel serve` assembles two built-in plugins — `keel.example`
(a complete demo: two tools, three hook subscriptions, two triggers, two
effects; `internal/agentext/builtin.go:38`) and `keel.subagents` (exposes the
`spawn_subagent` tool; `internal/agentext/builtin.go:256`). The settings screen
renders the assembled surface — generation, per-plugin state, provided tools,
hooks, triggers, diagnostics (`web/src/screens/settings.tsx:1148`).

**What this is not:** there is no manifest file, no install step, no
permission model and no UI hosting. A "plugin" today is code compiled into the
binary, trusted by construction. Everything below is the planned standard that
turns this runtime into a package system.

## 3. The planned standard, at a glance

The `keel-plugin-core` change specifies a plugin **package**: a directory
`<plugins-root>/<id>/` with a `plugin.yaml` manifest, one of three kinds, a
lifecycle state machine, an enforced permission set and an audit trail
(`openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`). The
specification's key decisions:

| Topic | Decision (planned) | Page |
| --- | --- | --- |
| Package & manifest | `plugin.yaml` with id/version/kind/engines/provides/requires/permissions/entry/source; invalid manifest → install refused with a field-level reason | [Manifest](/harness/plugins/manifest) |
| Three kinds | `in-process` (official, compiled), `sidecar` (external process, stdio newline JSON-RPC), `webview` (UI package hosted by the host) | [Kinds](/harness/plugins/kinds) |
| Lifecycle | `installed → enabled → disabled → uninstalled`; enable goes through `agentext`'s dependency-ordered assembly and reverse unload; disable must leave zero residue | [Lifecycle](/harness/plugins/lifecycle) |
| Permissions | tools allowlist checked at dispatch; `net` hosts; `secrets` references; sidecar env whitelist; install/enable/upgrade require a durable approval; violations are audited | [Permissions](/harness/plugins/permissions) |
| UI | webview resources under `/plugins/<id>/`, bridged via `keel.plugin.*`, theme from host tokens; TUI never renders plugin UI (opens the URL instead) | [Plugin UI](/harness/plugins/ui) |
| Testing | unit + integration + conformance + negative controls (crash isolation, permission denial, rollback) | [Testing](/harness/plugins/testing) |
| Creation mode | `keel.creation`, itself a plugin: NL → generate → validate → install → enable → deep link, with approval and rollback | [Creation mode](/harness/creation-mode) |
| Coverage | the official suite is tracked against DSH's capability groups and bundles, in waves W1/W2/W3 | `plugins/COVERAGE.md` (planned) |

## 4. DSH comparison — mapping, not porting

DeepSeek Harness is a **reference**, not the source of truth for keel. Its
plugin model, as documented for this comparison, maps onto keel semantics like
this:

| DSH plugin model | keel counterpart | Status |
| --- | --- | --- |
| plugin = `apply(ctx)` + injected dependencies + reversible side effects | `Plugin.Start(ctx, *PluginEnv)` with `Provides`/`Requires` and `Effects` released in reverse order (`internal/agentext/plugin.go:57`, `:78`, `:51`, `:387`) | **Implemented** (in-process) |
| capability triad: Definition / Provider / Consumer | service graph: a descriptor `provides` a capability, dependents declare `requires`/`optional`, bindings are asserted narrow interfaces (`internal/servicegraph/descriptor.go`, `docs/LOADOUT.md` §7) | **Implemented** |
| bundle = a patch layer of config/plugins | the planned enablement list + `plugin.yaml` (`keel-plugin-core`); today the closest analogue is the default loadout blueprint (`internal/loadout/loadout.go:63`) | **Planned** |
| profile = an ordered stack of bundles | loadout: an immutable snapshot `{id, revision, version, summary, services}` pinned per run (`docs/LOADOUT.md` §1) | **Implemented** (services), planned (plugins in the set) |
| plugin can provide tool / hook / command / UI slot / provider / storage | tools, hooks, triggers implemented (`internal/agentext/plugin.go:67`); commands, UI slots, providers-as-plugins and storage are on the W1/W2 waves (`plugins/COVERAGE.md`) | **Partly implemented / planned** |
| installing third-party code executes it; explicit permission is required | today: code is compiled in, so trust is implicit; planned: permission enforcement + durable install approval (`keel-plugin-core` "permissions and sandbox") | **Planned** |
| Cordis runtime (in-process object graph, synchronous `get/provide`, effect system, event bus) | **not ported.** keel expresses the same semantics with Go `agentext` + `servicegraph`; introducing a second plugin runtime would violate the single-source-of-truth rule (`plugins/COVERAGE.md` §D, `vendor/cordis` = absent with reason) | **Deliberate difference** |

Two more differences worth knowing before reading the plugin pages:

1. **State ownership.** A DSH runtime can be connected as an *external*
   AgentBackend, where the backend owns its session state and keel only records
   the compatibility log (`docs/DSH-REFERENCE.md`). That path is about
   *foreign agents*, not about plugins, and it never claims native recovery.
2. **Cross-process seams are explicit.** DSH's in-process object graph has no
   lossless network equivalent; keel's service graph therefore binds services
   to narrow Go interfaces, and anything that needs to cross a process boundary
   does so as an explicit protocol (sidecar JSON-RPC, MCP, AgentBackend),
   never as a transparent proxy of an object graph (`docs/SERVICE-GRAPH.md` §4).

## 5. Reading order

1. [Manifest](/harness/plugins/manifest) — the package and the full
   `plugin.yaml` schema.
2. [Kinds](/harness/plugins/kinds) — in-process, sidecar and webview, with the
   sidecar protocol and the webview resource layout.
3. [Lifecycle](/harness/plugins/lifecycle) — states, dependency rules,
   reversible unload, hot update.
4. [Permissions](/harness/plugins/permissions) — allowlists, approvals, audit,
   and where the sandbox boundary really is.
5. [Plugin UI](/harness/plugins/ui) — webview + react + solid-glass, the
   `keel.plugin.*` bridge, theme tokens.
6. [Testing](/harness/plugins/testing) — unit, integration, conformance and
   negative controls.
