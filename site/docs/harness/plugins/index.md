# Plugins

> **Status legend used throughout this section**
>
> - **Implemented** — ships in this repository today; every claim carries a
>   `path:line` citation that can be opened.
> - **Partly implemented** — the mechanism ships, but a named part is missing or
>   narrower than the contract; the gap is stated where it appears.
> - **Deferred** — specified and registered, not in this build. The live
>   contract is `openspec/specs/plugin-system/spec.md` (the `keel-plugin-core`
>   change that introduced it is archived under
>   `openspec/changes/archive/2026-11-11-keel-plugin-core/`); the coverage
>   matrix is `plugins/COVERAGE.md`, whose machine-readable source is
>   `plugins/keel.plugin-market/catalog.json` (`scripts/plugin-catalog.mjs:13`).
>
> The plugin package standard, its lifecycle API, permission enforcement at the
> tool and process boundary, the webview host and the creation-mode plugin are
> all shipped. What remains narrower than the contract is named in §2.3, not
> implied by a "planned" banner.

## 1. The thesis: everything above the ledger is assembly

The design statement keel shares with DeepSeek Harness is "everything is a
plugin" — with keel's own correction: **the contract kernel is not**. The
ledger, event order, idempotency keys and fencing are the invariants that make
every other capability replaceable; they are implemented as a service graph
(general assembly concerns) plus a plugin host (product extensions) in front of
a package layer.

Three mechanisms express the same semantics in Go:

| Mechanism | What it assembles | Citation |
| --- | --- | --- |
| **Service graph** (`internal/servicegraph` + `internal/loadout`) | first-party runtime services: store, llm, prompt, tools, loop, compaction, projection, telemetry, external gateways | `internal/loadout/loadout.go:63` |
| **Plugin runtime** (`internal/harnessext`) | product extensions: tools, hooks, triggers, prompt sections, and their cleanup | `internal/harnessext/plugin.go:163` |
| **Plugin packages** (`internal/pluginpkg` + `internal/harnessext` manager) | installed `plugin.yaml` packages: manifest validation, managed root, lifecycle, permissions, audit | `internal/pluginpkg/manifest.go:142`, `internal/harnessext/plugin_manager_ops.go:504` |

All three use the same vocabulary — `provides`/`requires`/`optional`, ordered
activation, **reverse-order reversal** of every registered side effect, a
generation counter, and observable diagnostics. A service whose assembly
identity is unchanged is shared across generations (`docs/LOADOUT.md` §4); a
plugin is re-assembled on every `Apply` (`internal/harnessext/plugin.go:209`).

The correlation with the DSH plugin model is deliberate but semantic, not
mechanical — see §4.

## 2. What ships today

### 2.1 The in-process contract

The `harnessext` plugin contract is a Go value:

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

(`internal/harnessext/plugin.go:67`; `Start` `:78`, `Stop` `:80`, `Effect` `:51`,
`PluginEnv` `:57`.)

Guarantees the host provides, each with a test in
`internal/harnessext/harnessext_test.go` / `e2e_test.go`:

- **dependency topology** — activation proceeds in dependency order; a plugin
  whose requirement is missing stays `PENDING` with a `waiting for: …`
  diagnostic instead of half-loading (`internal/harnessext/plugin.go:291`);
- **reversible registration** — hook subscriptions, trigger registrations and
  declared effects are released in exact reverse order; an unload cannot leave
  a listener, timer or tool behind (`internal/harnessext/plugin.go:386`,
  `:417`);
- **fail-closed tool namespace** — two plugins declaring the same tool name
  make the second plugin `FAILED`; the model never sees an ambiguous name
  (`internal/harnessext/plugin.go:236`);
- **generation** — every `Apply` increments the generation once; the snapshot
  reports it (`internal/harnessext/plugin.go:213`, `:505`);
- **tool publishing** — after every assembly the active tool set is pushed into
  the host registry through one sink (in `keel serve`:
  `rt.RefreshProductTools`, `cmd/keel/serve.go:464`).

### 2.2 The package layer

The package contract is implemented end to end:

- **Manifest** — `plugin.yaml` is parsed strictly (unknown fields rejected) and
  validated with field-level errors: `id/version/kind/layer/description/
  engines/provides/contributes/extensibility/requires/permissions/entry/source`
  (`internal/pluginpkg/manifest.go:142`, validation at `:653`). The package
  loader validates the tree (UI entry, contribution entries, sidecar command)
  and hashes it (`internal/pluginpkg/package.go:74`).
- **Managed root** — packages live in `<plugins-root>/<id>/`, are copied in and
  content-hashed at install; discovery runs at boot. The root defaults to
  `$KEEL_HOME/plugins` and is overridden by `--plugins-root` /
  `KEEL_PLUGINS_ROOT` (`cmd/keel/main.go:175`, `:246`).
- **Lifecycle** — `install`, `enable`, `disable`, `uninstall`, `update` over
  six states (`installed`, `enabled`, `disabled`, `failed`, `invalid`,
  `uninstalled`; `internal/harnessext/plugin_manager.go:29`). The API is
  `GET /v1/plugins`, `POST /v1/plugins/install`,
  `POST /v1/plugins/{id}/enable|disable|uninstall|update`,
  `GET /v1/plugins/audit` (`internal/harnessapi/plugins.go:56`). Install,
  enable and update are gated by a durable approval
  (`internal/harnessapi/plugin_approval.go:412`).
- **Permissions** — `permissions.tools` is checked at tool dispatch and the
  sidecar environment is built from the declared names; every refusal writes one
  `plugin_audit` row (`internal/harnessext/plugin_permissions.go:19`).
  `permissions.net` and `permissions.secrets` have a policy, a gate and an audit
  path, but no shipped host verb routes through them yet; `permissions.fs` is
  declared and published but not mediated — stated in
  [Permissions](/harness/plugins/permissions).
- **Kinds** — bundled `in-process` plugins; `sidecar` processes over the
  gateway's stdio JSON-RPC (`internal/harnessext/plugin_sidecar.go:22`); every
  other kind is resolved by the loaders map, which in `keel serve` contains
  exactly the sidecar loader (`cmd/keel/serve.go:1769`). A `webview` package is
  hosted at `/plugins/<id>/` and bridged through `keel.plugin.*`
  (`cmd/keel/plugins_ui.go:17`, `internal/harnessapi/plugin_bridge.go:75`).
- **Slot contributions** — `provides.slots` and `contributes[]` are decoded,
  validated and published on the inventory (`internal/harnessext/
  plugin_extension.go:64`, `internal/harnessext/plugin_manager_view.go:198`),
  and the client renders `chunk`, `iframe` and `data` contributions through one
  slot registry; the signed `package` form is an explicit unsupported face in
  the client today (`web/src/plugins/slots.tsx:1`, `:590`).
- **Creation mode** — `keel.creation` is a bundled plugin with six actions
  (`catalog`, `scaffold`, `validate`, `install`, `list`, `remove`;
  `internal/harnessext/creation.go:91`), enabled by default like every builtin
  except `keel.testkit` (`cmd/keel/serve.go:1752`).

Out of the box, `keel serve` assembles the bundled set — `keel.example`
(`internal/harnessext/builtin.go:38`), `keel.file`, `keel.model`, `keel.bot`,
`keel.team-bot`, `keel.spill`, `keel.tasks`, `keel.shell`, `keel.approval`,
`keel.notify`, `keel.creation` — plus the disabled-by-default `keel.testkit`
(`internal/pluginpkg/builtin/`, `cmd/keel/serve.go:1660`). Official installable
packages under `plugins/` add `keel.plugin-market`, `keel.web-search` and
`keel.context-memory`; their status is tracked in `plugins/COVERAGE.md`. The
sub-agent primitive is the harness-owned `agent` tool, not a plugin tool
(`internal/toolsurface`). The settings screen renders the assembled surface —
generation, per-plugin state, provided tools, hooks, triggers, diagnostics
(`web/src/screens/settings.tsx:1148`).

### 2.3 What is deferred

| Deferred item | Why | Where it is recorded |
| --- | --- | --- |
| `agent_backend` sidecars (the OAR adapter) | the sidecar loader accepts `tool_provider` connections only; `plugins/keel.oar/plugin.yaml` is kept as a **DEFERRED** target manifest | `plugins/keel.oar/plugin.yaml:3` |
| Creation mode end-to-end (G8) | the action backend, templates and webview E2E exist; the "one sentence → running plugin" end-to-end gate has not passed | `openspec/changes/archive/2026-11-24-keel-harness-v2/DISPOSITION.md:20` |
| Creation-mode v2 extensibility aggregation, native chunk path | the declaration format is frozen and validated; aggregating every installed plugin's `extensibility.json` into a creation surface is not finished | `plugins/COVERAGE.md` (status `partial`) |
| Mediated fetch / secret-read verbs for the `net` and `secrets` gates | the policy, gate and audit path exist (`PermissionedBroker.Fetch` / `ReadSecret`), but no shipped host verb routes through them, so nothing is refused by them yet | `internal/harnessext/plugin_permissions.go:647` |
| Per-plugin CPU/memory budget, pidfile sweep after a host `SIGKILL` | the process boundary and process-group reaping exist; the resource budget and the survivor sweep do not | [Permissions](/harness/plugins/permissions) §5 |
| Client install/update cards | the backend install API and lifecycle endpoints ship; the web plugin screen still renders an Empty where the card belongs | `web/src/screens/plugins.tsx:1` |

## 3. The standard, at a glance

A plugin **is a package**: a directory `<plugins-root>/<id>/` with a
`plugin.yaml` manifest, one of three kinds, a lifecycle state machine, an
enforced permission set and an audit trail
(`openspec/specs/plugin-system/spec.md`).

| Topic | State | What ships | Page |
| --- | --- | --- | --- |
| Package & manifest | **Implemented** | `plugin.yaml` with id/version/kind/layer/engines/provides/contributes/extensibility/requires/permissions/entry/source; invalid manifest → install refused with a field-level reason | [Manifest](/harness/plugins/manifest) |
| Three kinds | **Implemented** for `in-process` (bundled) and `sidecar` (tool-provider); **Deferred** for `agent_backend` sidecars | `sidecar` = external process, stdio newline JSON-RPC; `webview` = UI package hosted by the host (+ optional sidecar backend) | [Kinds](/harness/plugins/kinds) |
| Lifecycle | **Implemented** | `installed/enabled/disabled/failed/invalid/uninstalled`; enable goes through `harnessext`'s dependency-ordered assembly and reverse unload; disable leaves zero residue | [Lifecycle](/harness/plugins/lifecycle) |
| Permissions | **Partly implemented**: tools and sidecar env enforced; `net`/`secrets` gates exist with no routed host verb; `fs` declaration-only | tools allowlist checked at dispatch; sidecar env whitelist; install/enable/upgrade require a durable approval; every refusal audited | [Permissions](/harness/plugins/permissions) |
| UI | **Implemented** | webview resources under `/plugins/<id>/`, bridged via `keel.plugin.*`, theme from host tokens; the TUI never renders plugin UI (it opens the URL) | [Plugin UI](/harness/plugins/ui) |
| Slots & contributions | **Implemented** (declaration → inventory → client slot registry); `package` form and creation-mode aggregation are **Deferred** | `provides.slots[]`, `contributes[]`, `extensibility.json`; chunk/iframe/data render today, package is an explicit unsupported face | [Slots](/harness/plugins/slots) |
| Testing | **Implemented** requirements, coverage matrix and catalog gate | unit + integration + conformance + negative controls; `plugins/COVERAGE.md`; `scripts/plugin-catalog.mjs --check` | [Testing](/harness/plugins/testing) |
| Creation mode | **Implemented** actions and templates; **Deferred** end-to-end (G8) | `keel.creation`, itself a plugin: NL → generate → validate → install → enable → deep link, with approval and rollback | [Creation mode](/harness/creation-mode) |

## 4. DSH comparison — mapping, not porting

DeepSeek Harness is a **reference**, not the source of truth for keel. Its
plugin model, as documented for this comparison, maps onto keel semantics like
this:

| DSH plugin model | keel counterpart | Status |
| --- | --- | --- |
| plugin = `apply(ctx)` + injected dependencies + reversible side effects | `Plugin.Start(ctx, *PluginEnv)` with `Provides`/`Requires` and `Effects` released in reverse order (`internal/harnessext/plugin.go:57`, `:78`, `:51`, `:387`) | **Implemented** (in-process) |
| capability triad: Definition / Provider / Consumer | service graph: a descriptor `provides` a capability, dependents declare `requires`/`optional`, bindings are asserted narrow interfaces (`internal/servicegraph/descriptor.go`, `docs/LOADOUT.md` §7) | **Implemented** |
| bundle = a patch layer of config/plugins | assembly: four ordered layers (base → mode → preset → `config.toml`) plus row-id overlays (`docs/ASSEMBLY.md` §2) ; a *bundle of plugins* is still a coverage-matrix row, not a shipped artefact | **Implemented** (assembly), **Deferred** (plugin bundles) |
| profile = an ordered stack of bundles | loadout: an immutable snapshot `{id, revision, version, summary, services}` pinned per run (`docs/LOADOUT.md` §1); assembly linearizes the config layers | **Implemented** |
| plugin can provide tool / hook / command / UI slot / provider / storage | tools, hooks, triggers, prompt sections, slots/contributions and provider types are declared and validated (`internal/pluginpkg/manifest.go:210`); the `provides.cli` tree is declared and validated but not executed as a `keel <cmd>` surface; storage domains are registered rows, not plugins | **Partly implemented** |
| installing third-party code executes it; explicit permission is required | shipped: install/enable/update are durable approvals, permissions are enforced at host seams, refusals are audited (`internal/harnessext/plugin_permissions.go:19`) | **Implemented** |
| Cordis runtime (in-process object graph, synchronous `get/provide`, effect system, event bus) | **not ported.** keel expresses the same semantics with Go `harnessext` + `servicegraph`; introducing a second plugin runtime would violate the single-source-of-truth rule (`plugins/COVERAGE.md` §D, `vendor/cordis` = absent with reason) | **Deliberate difference** |

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
3. [Lifecycle](/harness/plugins/lifecycle) — states, transitions, the install
   API, reversible unload, hot update, audit.
4. [Permissions](/harness/plugins/permissions) — allowlists, approvals, audit,
   and where the sandbox boundary really is.
5. [Plugin UI](/harness/plugins/ui) — webview + react + solid-glass, the
   `keel.plugin.*` bridge, theme tokens, TUI behavior.
6. [Slots](/harness/plugins/slots) — slots, contributions and the client slot
   registry.
7. [Testing](/harness/plugins/testing) — unit, integration, conformance and
   negative controls.
