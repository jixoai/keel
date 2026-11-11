# Plugin lifecycle

> **Planned** lifecycle API (`installed → enabled → disabled → uninstalled`,
> install/enable/disable/uninstall endpoints, version replacement) per
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md` ("lifecycle").
> The *assembly mechanics* it builds on are **implemented** in
> `internal/agentext` and `internal/servicegraph` and are cited below.

## 1. The state machine

```
            install                enable                  disable
  (absent) ─────────▶ installed ─────────▶ enabled ◀────────────┐
                        │   ▲                │                   │
                        │   │   upgrade      │  disable          │
                        │   └──── (same id,   ▼                   │
                        │        new version) disabled ──────────┘
                        │                        │
                        ▼                        ▼
                   uninstalled ◀───────────────  uninstall
```

- `install` validates the manifest, the `engines.keel` gate and the package
  hash, stores the package in the managed directory and writes an audit row
  ([Manifest](/harness/plugins/manifest)).
- `uninstalled` removes the managed directory; it is refused while the plugin
  is enabled and in use.
- `upgrade` replaces the version of the same id — one enabled version per id at
  any time.

**Enable and disable are assembly operations**, not flag flips. Enable runs the
plugin through dependency-ordered activation; disable must remove *every*
side effect it registered. The implemented host does exactly this for its
in-process plugins, and the planned lifecycle reuses it:

| Promise | Mechanism (implemented) | Citation |
| --- | --- | --- |
| dependency-ordered activation | iterative activation loop; dependents activate once their requirements are provided | `internal/agentext/plugin.go:237` |
| missing dependency ⇒ not loaded, visibly | state `PENDING` + `waiting for: <ids>` diagnostic | `internal/agentext/plugin.go:291` |
| disable removes tools | the published tool set is recomputed after every assembly | `internal/agentext/plugin.go:438` |
| disable removes subscriptions/triggers | `unsubs` and `triggerStops` are released in reverse order | `internal/agentext/plugin.go:386` |
| disable runs declared cleanups exactly once | `effectsRun` bookkeeping prevents double execution on partial unload | `internal/agentext/plugin.go:396` |
| a failed activation rolls back its own effects | failure path calls `cleanup` before returning | `internal/agentext/plugin.go:317` |
| every change is visible | generation increments once per assembly; snapshot reports states + diagnostics | `internal/agentext/plugin.go:209`, `:505` |

The spec's acceptance scenario is the same statement in product terms:
*enable a plugin that provides tool `x` and a webhook trigger, disable it, and
the model no longer has `x`, the trigger no longer fires, the inventory shows
`disabled`, and nothing errors* (`spec: disabling revokes immediately`).

## 2. Dependency rules

- `requires` names **plugins or services**. A requirement is satisfied when
  something active `provides` it; a plugin that cannot be satisfied stays
  `disabled` with a named reason. It is never partially loaded
  (`spec: a missing dependency blocks load`; mechanism: `internal/agentext/plugin.go:291`).
- Tools are a global namespace: two enabled plugins may not provide the same
  tool name. The second one fails closed with a diagnostic, whichever order
  they are assembled in (`internal/agentext/plugin.go:236`, `:454`).
- Hook events must belong to the closed set; an unknown event is an error at
  subscription time, never a silent no-op (`internal/agentext/hooks.go:249`).

## 3. Hot update without disturbing in-flight work

Plugins are process-local registration, so an update is a **re-assembly**.
Two levels of protection keep it from disturbing work in progress:

1. **Product level (today).** `Runtime.Apply` unloads the previous assembly
   first, then activates the new one and increments the generation once. Hook
   callbacks and trigger fires that reference removed plugins fail visibly in
   their own counters; committed runs are untouched because the run's own
   registrations live in its transcript, not in the plugin table
   (`internal/agentext/plugin.go:163`, `internal/agentext/hooks.go:399`).
2. **Service level (where plugins become services).** The planned unification
   routes official plugins through the service graph, which adds generations
   with in-flight references: `RETIRING` instances accept no new runs and are
   unloaded in reverse dependency order once their references drain, or
   forcibly after the retire deadline — and a forced retire marks the holder's
   lease broken so the run fails with a `drain-expired` diagnostic instead of
   writing against a dead instance
   (`internal/servicegraph/lifecycle.go:21`, `internal/servicegraph/engine.go:270`,
   `:303`, `:650`).

## 4. What happens to work a plugin started

| Work item | On disable |
| --- | --- |
| tool provided by the plugin | removed from the model's tool set at the next assembly |
| hook subscription | released immediately (reverse order) |
| trigger | unregistered; no further fires; its existing audit rows remain |
| declared effect (timer, listener, connection) | `Stop(ctx)` runs with a 2s bounded context; failures are logged, never fatal | 
| sidecar process | `close` on the protocol, then stdin close → grace → SIGTERM → SIGKILL on the process group |
| sub-agent sessions the plugin spawned | keep running — they are sessions in the ledger, not plugin state |

(`internal/agentext/plugin.go:386` for effects; sidecar close path
`internal/gateway/stdio.go:228`.)

## 5. Audit

Every lifecycle action — `install`, `enable`, `disable`, `uninstall`, `update`
— and every permission refusal must record **time, operator, plugin id,
version, action, result**, and be queryable through `GET /v1/plugins/audit`
(`spec: audit`). The audit lane reuses the same discipline as the trigger audit
today: bounded records, ordered by time, with the actor and the outcome
(`internal/agentext/triggers.go:86` for the existing audit-entry shape).

## 6. Lifecycle in the UI

The current settings screen already renders the assembled surface — generation,
per-plugin state chips (`active`/`pending`/`failed`/`stopped`), provided tools,
hooks, triggers and diagnostics (`web/src/screens/settings.tsx:1005`, `:1148`).
The planned lifecycle adds rows for `installed`/`disabled` packages plus
enable/disable/uninstall/update actions and an audit view — same table, more
states.
