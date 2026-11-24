# Plugin lifecycle

> **Implemented.** The lifecycle API — `installed → enabled → disabled →
> uninstalled`, plus `failed` and `invalid`, the install/update paths, the
> durable approval and the audit trail — ships in this repository
> (`internal/harnessext/plugin_manager.go:29`,
> `internal/harnessapi/plugins.go:56`). The assembly mechanics it builds on are
> the `harnessext` runtime's, cited below.

## 1. The state machine

```
  (absent) ──install──▶ installed ──enable──▶ enabled ──disable──▶ disabled
                            ▲                   │                   │
                            └──── update ───────┴───────────────────┘
                                     (same id, new version)

  enabled | installed | disabled ──(package stops validating)──▶ invalid
  installed | disabled ──(enable cannot load; host rolls back)──▶ failed
  failed ──enable (reconnect)──▶ enabled
  disabled ──uninstall──▶ uninstalled   (the row is deleted; audit only)
```

The six states are the closed set (`internal/harnessext/plugin_manager.go:29`,
values at `:33`; `LifecycleStates` at `:49`):

| State | Meaning |
| --- | --- |
| `installed` | a valid package in the managed root that is not enabled yet |
| `enabled` | the operator asked for it and the runtime (or, for a UI-only webview, the UI host) serves it |
| `disabled` | previously enabled, fully unloaded; still installed |
| `failed` | an enable that did not reach the active state; the runtime's own diagnostic is kept |
| `invalid` | a package that no longer validates on disk; the row keeps its state so the reason stays visible |
| `uninstalled` | terminal; rows in this state are deleted, so it appears only in the audit trail |

- `install` validates the manifest, the `engines.keel` gate and the package
  content tree, copies the package into the managed root and writes an audit
  row ([Manifest](/harness/plugins/manifest)).
- `disable` unloads and keeps the package; `uninstall` removes the managed
  directory (and any archived versions) and is refused while the plugin is
  `enabled` or while an enabled plugin depends on it
  (`internal/harnessext/plugin_manager_ops.go:817`). Bundled plugins can be
  disabled, never uninstalled (`:824`).
- `update` replaces the version of the same id — one enabled version per id at
  any time; the previous package is archived under `<root>/.versions/<id>/`
  and a failed enable rolls the old version back
  (`internal/harnessext/plugin_manager_ops.go:987`).
- `failed` is not a half-load: on a materialization failure or a runtime
  activation failure the host re-assembles **without** the plugin first, then
  records `failed` (or `disabled` when the plugin is merely waiting on a
  dependency) with the reason (`internal/harnessext/plugin_manager_ops.go:713`).

**Enable and disable are assembly operations**, not flag flips. Enable runs the
plugin through dependency-ordered activation; disable must remove *every*
side effect it registered:

| Promise | Mechanism (implemented) | Citation |
| --- | --- | --- |
| dependency-ordered activation | iterative activation loop; dependents activate once their requirements are provided | `internal/harnessext/plugin.go:237` |
| missing dependency ⇒ not loaded, visibly | state `PENDING` + `waiting for: <ids>` diagnostic | `internal/harnessext/plugin.go:291` |
| disable removes tools | the published tool set is recomputed after every assembly | `internal/harnessext/plugin.go:438` |
| disable removes subscriptions/triggers | `unsubs` and `triggerStops` are released in reverse order | `internal/harnessext/plugin.go:386` |
| disable runs declared cleanups exactly once | `effectsRun` bookkeeping prevents double execution on partial unload | `internal/harnessext/plugin.go:396` |
| a failed activation rolls back its own effects | failure path calls `cleanup` before returning | `internal/harnessext/plugin.go:317` |
| every change is visible | generation increments once per assembly; snapshot reports states + diagnostics | `internal/harnessext/plugin.go:209`, `:505` |

The spec's acceptance scenario is the same statement in product terms:
*enable a plugin that provides tool `x` and a webhook trigger, disable it, and
the model no longer has `x`, the trigger no longer fires, the inventory shows
`disabled`, and nothing errors* (`openspec/specs/plugin-system/spec.md`).

## 2. Persisted intent wins at boot

`Manager.Boot` reconciles the database with the disk and the bundled set
(`internal/harnessext/plugin_manager_ops.go:32`):

- a bundled plugin's row is refreshed, and its **persisted** state wins — a
  plugin an operator disabled stays disabled across restarts; only a first boot
  applies the default enablement;
- packages discovered under the plugins root are registered (highest version
  wins unless an installed version exists);
- a row whose package is gone or no longer validates becomes `invalid`, with
  the problem kept in diagnostics rather than the row disappearing.

This is why enablement is a durable decision, not a process-local flag.

## 3. Dependency rules

- `requires` names **plugins or services**. A requirement is satisfied when
  something active `provides` it; a plugin that cannot be satisfied is
  `disabled` with a named reason (or `PENDING` inside an assembly). It is never
  partially loaded (`internal/harnessext/plugin.go:291`;
  `internal/harnessext/plugin_manager_ops.go:690`).
- Tools are a global namespace: two enabled plugins may not provide the same
  tool name. The second one fails closed with a diagnostic, whichever order
  they are assembled in (`internal/harnessext/plugin.go:236`, `:454`).
- Hook events must belong to the closed set; an unknown event is an error at
  subscription time, never a silent no-op (`internal/harnessext/hooks.go:249`).
- Disable and uninstall refuse when an enabled plugin depends on the target, so
  a disable never leaves dependents pending (`internal/harnessext/
  plugin_manager_ops.go:788`).

## 4. What happens to work a plugin started

| Work item | On disable |
| --- | --- |
| tool provided by the plugin | removed from the model's tool set at the next assembly |
| hook subscription | released immediately (reverse order) |
| trigger | unregistered; no further fires; its existing audit rows remain |
| declared effect (timer, listener, connection) | `Stop(ctx)` runs with a 2s bounded context; failures are logged, never fatal |
| sidecar process | protocol `close` notification, then stdin close → grace → SIGTERM → SIGKILL on the process group |
| sub-agent sessions the plugin spawned | keep running — they are sessions in the ledger, not plugin state |

(`internal/harnessext/plugin.go:386` for effects; sidecar close path
`internal/gateway/stdio.go:228`.)

## 5. Hot update without disturbing in-flight work

Plugins are process-local registration, so an update is a **re-assembly**.
Two levels of protection keep it from disturbing work in progress:

1. **Product level (today).** `Runtime.Apply` unloads the previous assembly
   first, then activates the new one and increments the generation once. Hook
   callbacks and trigger fires that reference removed plugins fail visibly in
   their own counters; committed runs are untouched because the run's own
   registrations live in its transcript, not in the plugin table
   (`internal/harnessext/plugin.go:163`, `internal/harnessext/hooks.go:399`).
2. **Service level (where plugins become services).** The service graph adds
   generations with in-flight references: `RETIRING` instances accept no new
   runs and are unloaded in reverse dependency order once their references
   drain, or forcibly after the retire deadline — and a forced retire marks the
   holder's lease broken so the run fails with a `drain-expired` diagnostic
   instead of writing against a dead instance
   (`internal/servicegraph/lifecycle.go:21`, `internal/servicegraph/engine.go:270`,
   `:303`, `:650`).

## 6. Audit

Every lifecycle action — `install`, `enable`, `disable`, `uninstall`, `update`
— and every permission refusal records **time, operator, plugin id, version,
action, result**, and is queryable through `GET /v1/plugins/audit`
(`internal/harnessapi/plugins.go:256`). Permission refusals land in the same
table with action `deny`
(`internal/harnessext/plugin_permissions.go:19`). The audit row is written
before the HTTP response, `audit_id` is the keyset cursor, and approval
refusals are written even though the manager was never called
(`internal/harnessapi/plugin_approval.go:412`).

The table itself is `plugin_audit` (`internal/store/schema.sql:603`); the
inventory tables are `plugins` (`internal/store/schema.sql:582`).

## 7. Lifecycle in the UI — backend vs client

The backend surface is complete: `GET /v1/plugins` answers the inventory (root,
revision, generation, per-plugin view with `state`, `bundled`, `source`,
`path`, `contentHash`, slots/contributions, diagnostics), and the five write
endpoints plus the audit endpoint are mounted
(`internal/harnessapi/plugins.go:56`).

The client is behind that surface, and the gap is stated in the source:
`web/src/screens/plugins.tsx` renders each card from `PluginStatusView` and
still shows an Empty where the install/update card belongs — "the entry that
owns the install surface and the plugin UI route" (`web/src/screens/plugins.tsx:1`).
So: **API implemented, client install/update cards not wired**. Opening a
plugin's UI from the inventory is wired: the web target embeds `/plugins/<id>`,
the terminal target calls `openUrl` and keeps the URL copyable
([Plugin UI](/harness/plugins/ui)).
