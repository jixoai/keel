# Manifest (`plugin.yaml`)

> **Implemented.** The package format, the validator and the store below ship in
> this repository; every rule carries the code that enforces it. The contract's
> prose lives in `openspec/specs/plugin-system/spec.md` (the plugin-package,
> discovery/install and package-storage sections), promoted there when the
> `keel-plugin-core` change was archived. One field is declaration-only: `permissions.fs` (see
> [Permissions](/harness/plugins/permissions)).

## 1. The package

A plugin **is a directory**:

```
<plugins-root>/<id>/
├─ plugin.yaml            the manifest (required)
├─ ui/                    webview resources (required for an iframe-contributing webview)
│   ├─ index.html
│   └─ assets/…
├─ bin/                   sidecar entry points (required for kind: sidecar) — any path the
│                         manifest's entry.command names
└─ extensibility.json     the machine-readable extension surface (required for official
                          plugins, optional for third-party ones)
```

The package is copied into a **managed directory** (`<plugins-root>/<id>/`) at
install time; the stored plugin never points at the source directory, and
installation records the tree hash (`internal/pluginpkg/package.go:74`,
`internal/harnessext/plugin_manager_ops.go:504`). The root itself is
`--plugins-root` / `KEEL_PLUGINS_ROOT` / `$KEEL_HOME/plugins`
(`cmd/keel/main.go:246`), and it is created on first install.

## 2. The manifest — full field list

```yaml
id: vendor.name                # ^[a-z][a-z0-9._-]{1,63}$; globally unique; one enabled version per id
version: 1.2.3                 # semver
kind: in-process | sidecar | webview
layer: kernel | harness | provider | ui | service   # optional; derived from kind when absent
description: one line
engines: { keel: ">=0.2.0 <0.3.0" }        # host version gate
provides:
  tools: [name, ...]           # model-facing tool names; globally unique (conflict = fail-closed)
  hooks: [event, ...]          # must belong to the closed hook set
  triggers: [id, ...]
  provider_types: [...]        # provider capability families this plugin declares
  provider: { ... }            # marks the plugin as a provider plugin (layer: provider)
  feature_ports: [port, ...]
  agents: [id, ...]
  agent_teams: [id, ...]
  cli: [{ path: [...], summary, schemas, permissions, danger, idempotency, callers }]
  ui: { entry: ui/index.html, title: "..." }   # required when a contribution is an iframe
  slots: [{ id, forms, arity, schema?, targets }]   # slots this plugin declares
  backends: [{ tool, action, impl }]                # existing aggregate-tool actions mapped onto this plugin
contributes:
  - { slot: <slot-id>, form: chunk|package|iframe|data, entry?, payload?, capabilities?, targets?, tool?, part? }
extensibility: { doc: EXTENSIBILITY.md, schema: extensibility.json }  # official plugins MUST; third-party MAY omit
requires: [plugin-or-service-id, ...]          # missing dependency ⇒ not loaded (no half-load)
permissions:
  fs: [workspace-write]        # file effect classes (declared; not mediated — see Permissions)
  net: [host, ...]             # allowed hosts
  secrets: [NAME, ...]         # allowed secret references
  tools: [name, ...]           # allowed tool calls
entry: { command: [...], args: [...], env: [...] }   # required for sidecar; may be empty for in-process
source: { type: builtin | local | git | npm, path?, url?, ref?, package? }
```

The struct is `internal/pluginpkg/manifest.go:142`; `permissions` `:381`,
`entry` `:398`, `source` `:413`; validation `:653`. Unknown fields are rejected,
so a typo is an error instead of a silent no-op.

Field-by-field semantics and enforcement points:

| Field | Meaning | Enforced where |
| --- | --- | --- |
| `id` | global identity; the directory name; the audit subject | install validation |
| `version` | semver; a second enabled version of the same id is refused | install/update |
| `kind` | one of three; drives which `entry`/`ui` shape is valid | install validation, [Kinds](/harness/plugins/kinds) |
| `layer` | architectural role, orthogonal to `kind`; absent ⇒ derived (`webview`→`ui`, `sidecar`→`provider`, `in-process`→`harness`) | install validation, published on the inventory |
| `engines.keel` | semver range the plugin supports; mismatch refuses install **and** enable with the mismatched range named | version gate |
| `provides.tools` | tool names the plugin contributes; a duplicate across enabled plugins is a fail-closed conflict; published names are normalized to `^[A-Za-z0-9_-]{1,64}$` | install validation, tool dispatch, assembly |
| `provides.hooks` | subscriptions to the **closed** event set (today: `subagent.*`, `trigger.*`, `turn.*`, `decision.*`; `internal/harnessext/hooks.go:18`) | subscription registration |
| `provides.triggers` | trigger ids the plugin owns (`interval`/`cron`/`webhook`) | trigger engine |
| `provides.provider_types` | provider capability families with display name, capabilities and config/auth/model/parameter schemas the settings UI renders | settings surface, provider registry |
| `provides.provider` | marks a provider plugin; requires `layer: provider` | install validation |
| `provides.feature_ports` | feature ports this plugin itself serves | install validation, inventory |
| `provides.agents` / `agent_teams` | agent and agent-team ids the plugin contributes | assembly, inventory |
| `provides.cli` | the plugin's command tree: summary, schemas, permissions, danger, idempotency, callers — one source for `--help`, settings and creation mode | install validation; **not yet executed** as a `keel <cmd>` surface |
| `provides.ui` | webview entry document + title; required for an iframe-contributing webview, optional for a pure chunk package | UI hosting |
| `provides.slots` | slots this plugin declares (extension points others contribute to); slot ids carry the declaring plugin's namespace | extension validation, inventory |
| `provides.backends` | maps existing aggregate-tool actions onto this plugin; a backend cannot add an action name | install validation, tool dispatch |
| `contributes` | this plugin's contributions: slot id, form (`chunk`/`package`/`iframe`/`data`), entry/payload and target set | extension validation, client slot registry |
| `extensibility` | `EXTENSIBILITY.md` + `extensibility.json`; official plugins must declare, third-party may omit (missing = "not usable by creation mode") | package validation (`internal/pluginpkg/extensibility.go:127`) |
| `requires` | plugin or service ids that must be present; missing ⇒ not loaded, never half-loaded | lifecycle assembly |
| `permissions` | the capability budget; see [Permissions](/harness/plugins/permissions) | tool dispatch and sidecar env enforced; `net`/`secrets` gates exist without a routed host verb; `fs` declaration-only; every refusal audited |
| `entry` | the sidecar process command/args/env (env only from allowlisted names, values always from the host) | process supervision |
| `source` | provenance of the package (`builtin`, `local`, `git`, `npm` + path/url/ref/package) | install + audit |

Rules that are *not* field-level but are part of the same contract:

- **invalid manifest → refusal with a field-level reason**; the plugin directory
  is not adopted and no row is written (`internal/pluginpkg/manifest.go:653`);
- **version gate** — `engines.keel` mismatch refuses install and enable, naming
  the supported range (`internal/harnessext/plugin_manager_ops.go:577`, `:687`);
- **content hash** — installation validates manifest, version gate and package
  content tree, then copies the package and records the SHA-256 of the tree
  (`internal/pluginpkg/package.go:74`, `internal/harnessext/plugin_manager_ops.go:504`);
- **install is immediately visible** — after install, `GET /v1/plugins` lists
  the plugin (`installed` or `enabled`) and the inventory revision advances.

## 3. Discovery, install and storage

| Operation | Surface | Semantics |
| --- | --- | --- |
| discover | scan `plugins-root` at boot (`internal/pluginpkg.Discover`) | packages found on disk appear in the inventory; a row whose package is missing or invalid keeps its state so the reason stays visible |
| install | `POST /v1/plugins/install` `{path}` | validate manifest + gate + content hash; copy into the managed root; write an audit row |
| list | `GET /v1/plugins` | root, revision, generation, per-plugin views, tools, prompt sections, hooks, triggers, limits |
| uninstall | `POST /v1/plugins/{id}/uninstall` | disable first; managed-directory removal (plus archived versions) + audit; bundled plugins are disabled, not uninstalled |
| update | `POST /v1/plugins/{id}/update` `{path?}` | same id, new version; an empty path picks the highest other version under the root; the previous package is archived under `<root>/.versions/<id>/`; a failed enable rolls the old version back |
| audit | `GET /v1/plugins/audit` | the lifecycle trail, keyset-paginated by `after` |

The install API is **two-phase** because adopting third-party code is a
decision, not a default (`internal/harnessapi/plugin_approval.go:412`):

```
POST /v1/plugins/install {"path": "..."}                      → 202 {"approvalRequired": true, "decision": …, "answerUrl"}
POST /v1/plugin-approvals/{decisionId} {"optionId": "allow"}   (the human answers)
POST /v1/plugins/install {"path": "...", "decisionId": "..."}  → 201 (or 409 refused)
```

Status codes, precisely:

| Code | Meaning here |
| --- | --- |
| `202` | install/enable/update opened a durable approval question; nothing was adopted yet |
| `201` | the install ran: the package is in the managed root and a row exists |
| `200` | enable/disable/uninstall/update completed |
| `409` | the state machine refused (already enabled, not enabled, enabled dependents, version gate, kind without a loader) or the approval was denied / timed out |
| `400` | the request or package is unusable (bad path, invalid manifest) with field-level detail |
| `404` | no such installed id |
| `503` | no plugin manager is wired, or lifecycle writes have no decision primitive to ask through (fail closed) |

The decision id is derived from `(tenant, action, requestId-or-target)`, so a
question opened for one action or one package cannot authorize another; a
denied or timed-out answer never reaches the manager and is recorded in the
audit as `refused`.
