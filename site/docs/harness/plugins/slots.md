---
title: Slots and contributions
---

# Slots and contributions

A slot is a named extension point a plugin **declares**; a contribution is another plugin putting
something into it. The Harness owns the mechanism, not the catalog: every slot id in a running system
belongs to the plugin that declared it, in that plugin's namespace
(`docs/PLUGIN-EXTENSIBILITY.md:9`).

That is what keeps third-party extension possible without a Harness release: a plugin can declare
`acme.analytics.panel` and another plugin can contribute to it, and the host never learns the name.

## 1. Vocabulary

Five closed sets, orthogonal to each other (`docs/PLUGIN-EXTENSIBILITY.md:16`):

| Term | Values | Answers |
|---|---|---|
| `kind` | `in-process` / `sidecar` / `webview` | how the plugin is hosted |
| `layer` | `kernel` / `harness` / `provider` / `ui` / `service` | where its contract lives |
| `form` | `chunk` / `package` / `iframe` / `data` | how a contribution runs |
| `arity` | `single` / `multi` / `ordered` | how many contributions a slot accepts |
| `target` | `web` / `native` / `tui` | which client can render it |

`layer` is optional and derived from `kind` when omitted: `webview → ui`, `sidecar → provider`,
`in-process → harness` (`docs/PLUGIN-EXTENSIBILITY.md:25`). An explicit layer may not contradict the
hosting constraint (§2.2 of the contract).

## 2. Declaring a slot

```yaml
id: acme.panel
version: 1.0.0
kind: webview
description: A review panel with one slot of its own.

provides:
  slots:
    - id: acme.panel.card          # must be prefixed by this plugin's id
      forms: [chunk, data]         # non-empty, closed set, no duplicates
      arity: multi                 # required: single | multi | ordered
      targets: [web, native, tui]  # non-empty
      schema:                      # required when forms contains data
        type: object
        required: [title]
        properties:
          title: { type: string }
```

Rules the validators enforce:

- **Namespace.** A slot id is `<plugin>.<name>`; the declaring plugin's id must be its prefix
  (`internal/pluginpkg/manifest.go:914`). A bare `route` is rejected; `acme.panel.card` is accepted.
- **Self-consistency.** Every declared target must have at least one form that can render there —
  "iframe only, `tui` only" is not a legal slot (`internal/pluginpkg/manifest.go:964`).
- **Data needs a schema.** A slot that accepts `data` must declare the JSON schema those payloads
  obey (`internal/pluginpkg/manifest.go:968`).
- **One owner.** A slot id may be declared by exactly one plugin; a second declaration is
  `ERR_SLOT_CONFLICT` and the whole registration is refused (`web/src/plugins/registry.ts:124`).

The contract freezes a first-party set — `keel.ui.route`, `keel.ui.nav`, `keel.ui.action`,
`keel.ui.tool-renderer`, `keel.ui.message-part`, `keel.ui.sidebar`, `keel.ui.settings-section`,
`keel.ui.surface`, `keel.tasks.widget` (`internal/pluginpkg/manifest.go:329`,
`docs/PLUGIN-EXTENSIBILITY.md:93`). They are still plugin declarations: uninstalling the declaring
plugin (`keel.ui`, `keel.tasks`) removes its slots and every contribution to them.

The web client also declares base slots of its own, including `keel.ui.top-band`, the system bar's
plugin row (`web/src/plugins/base.ts:15`, `:24`). Check the deployed client's base set before
targeting a slot you did not declare.

## 3. Contributing

```yaml
contributes:
  - { slot: acme.panel.card, form: chunk, entry: ui/card.js, title: Card }
  - { slot: acme.panel.card, form: data, payload: { title: Card } }
  - { slot: keel.ui.settings-section, form: chunk, entry: ui/settings.js }
```

| Form | Runs as | Isolation | Themed by host | web | native | tui |
|---|---|---|---|---|---|---|
| `chunk` | NSH dynamic import in the host tree | none (by design) | yes | ✅ | ✅ | ✅ |
| `package` | signed NSH package, own surface + HostLimits | VM | yes | ✅ | ✅ | ❌ |
| `iframe` | iframe / WebView page | browser | no | ✅ | ✅ | ❌ |
| `data` | JSON consumed by the host | none | yes | ✅ | ✅ | ✅ |

The matrix is the contract (`docs/PLUGIN-EXTENSIBILITY.md:112`) and the client's own table
(`web/src/plugins/forms.ts:32`).

Shape rules:

- `chunk` / `package` / `iframe` require `entry`, a package-relative path with no `..`, no backslash
  and no control characters; the file must exist in the package (`internal/pluginpkg/package.go:140`).
  A `package` entry must end in `.nshpkg`; an `iframe` entry must not be a `javascript:` URL
  (`internal/harnessext/plugin_extension.go:476`).
- `data` requires a non-empty `payload` and must not carry `entry`.
- A contribution to a slot **this manifest declares** is checked against that slot's forms. A
  contribution to an external slot is checked at assembly against the slots installed plugins
  declare (`Environment.KnownSlot`); with that resolver present, an unknown slot fails closed
  (`internal/pluginpkg/manifest.go:431`, `:1075`).
- `iframe` needs `provides.ui.entry` (the `/plugins/<id>/` page). A `webview` plugin may be a pure
  chunk package and omit `provides.ui` entirely (`docs/PLUGIN-EXTENSIBILITY.md:74`,
  `:75`).

## 4. Targets and degradation

The client asks for a slot **per target** (`web`, `native`, `tui`). Unavailability is explicit, never a
blank hole:

- A form that cannot run on the target comes back `unavailable` with a code and a sentence
  (`web/src/plugins/forms.ts:47`). In the terminal, the sentence says the contribution needs a
  graphical surface and offers to open it in a browser.
- A slot that does not declare the target is `ERR_TARGET_UNAVAILABLE`
  (`web/src/plugins/forms.ts:75`).
- `package` currently reports `ERR_UNSUPPORTED`: the independent-surface runner is not wired in this
  client, and the page says so instead of faking a panel (`web/src/plugins/forms.ts:62`).

## 5. Arity

| Arity | Behaviour |
|---|---|
| `single` | one contribution; a second plugin's contribution is `ERR_SLOT_FULL` and is refused (`web/src/plugins/registry.ts:185`) |
| `multi` | every valid contribution renders; the host draws them in declaration order |
| `ordered` | validated by the manifest and the client; the client applies no extra sort key, so declaration order is the order |

Contributions are mounted per `(pluginId, contributionId)`; a replaced contribution's late chunk
cannot overwrite its successor because each load carries a generation
(`web/src/plugins/registry.ts:54`).

## 6. Failure isolation

A broken plugin must not take the host down, and the code is written to that rule:

- **A bad declaration never fails the manager.** `DecodeExtension` reports field problems in
  `Problems`; it does not error (`internal/harnessext/plugin_extension.go:185`). The plugin may install;
  its UI surface simply does not load.
- **Registration is atomic.** A field error anywhere leaves the plugin's previous declaration
  untouched: no half-contribution is ever visible (`web/src/plugins/registry.ts:83`).
- **A failed chunk is a face, not a hole.** The contribution is marked `failed` with its code
  (`web/src/plugins/registry.ts:495`) and offers a retry (`web/src/plugins/registry.ts:421`); the rest
  of the slot keeps rendering.
- **Slots and contributions disappear together.** Disabling or uninstalling a plugin unregisters its
  slots *and* every contribution other plugins made to them, with a warning
  (`web/src/plugins/registry.ts:308`).
- **Intent and assembly are separate.** The manager owns what the database says, the runtime owns what
  is loaded; an enabled plugin that failed to load is reported with runtime diagnostics, not pretended
  healthy (`internal/harnessext/plugin_manager.go:25`).

In the browser, the plugin's extension surface is `GET /v1/plugins`: each row carries `layer`, `slots`,
`contributes` and the `extensibility` pointer (`internal/harnessext/plugin_manager_view.go:66`). The
declarations travel even while the plugin is disabled; the client decides what to load.

## 7. `extensibility.json` and the catalog

`extensibility.json` is the machine-readable companion of `EXTENSIBILITY.md`
(`docs/PLUGIN-EXTENSIBILITY.md:135`). Its `schema` must be `keel.plugin-extensibility/1`, and its
`plugin` / `version` / `slots` / `contributes` must match `plugin.yaml` exactly; drift is refused at
assembly (`docs/PLUGIN-EXTENSIBILITY.md:182`).

- Official plugins **must** declare `extensibility.doc` + `extensibility.schema` and ship both files
  (`docs/PLUGIN-EXTENSIBILITY.md:177`).
- Third-party plugins may omit it; the install succeeds and the plugin is marked "not usable by creation
  mode".
- Creation mode aggregates every installed plugin's document into one machine-readable catalog
  (`docs/PLUGIN-EXTENSIBILITY.md:184`).

Do not confuse that with `plugins/keel.plugin-market/catalog.json`, which is the **coverage matrix**
(schema `keel.plugin-market.catalog/1`, `plugins/keel.plugin-market/catalog.json:2`) — which upstream
capability is covered by which keel plugin, at what status. It is not a slot list: slot existence is
decided by declarations in `GET /v1/plugins`. Its human projection is `plugins/COVERAGE.md`, and the
single source is enforced by `docs/PLUGIN-CATALOG.md:3`.

## 8. Validating an extension surface

```bash
go test ./internal/pluginpkg                 # field-level validator + official-doc gate
go run ./plugins/tools/verifypkg             # manifest + real sidecar handshake + docs
node scripts/plugin-catalog.mjs --check      # catalog truth and COVERAGE reconciliation
(cd web && nsh test --target tui)            # client registry + slot rendering tests
```

These are the same entries the contract lists (`docs/PLUGIN-EXTENSIBILITY.md:262`), plus the client
suite. A field error names the path the author wrote, e.g. `provides.slots[0].arity` or
`contributes[2].form`.

## 9. Related

- [Plugins](/harness/plugins/) — the package, lifecycle and permissions around these declarations.
- [Manifest](/harness/plugins/manifest) — every `plugin.yaml` field.
- [Plugin UI](/harness/plugins/ui) — `/plugins/<id>/` and the `keel.plugin.*` bridge.
- [Creation mode](/harness/creation-mode) — the consumer of `extensibility.json`.
