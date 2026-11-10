# Creation mode

> **Planned.** Creation mode is specified in
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md` ("creation mode (shipped as
> the built-in plugin `keel.creation`)") and tracked in `plugins/COVERAGE.md`. Nothing on
> this page exists in the current checkout; every mechanism it builds on is
> implemented and cited.

Creation mode is the answer to a simple question: *if everything is a plugin,
who writes the plugins?* The answer is the agent itself — under the same
approval and audit rules as everything else.

## 1. What it is

A **built-in plugin** named `keel.creation`. It is not a privileged core
feature: it can be disabled like any other plugin, and it is subject to its own
permission set (`spec: creation mode`). What it does:

```
user describes a capability in natural language
        │
        ▼
keel.creation: generate a plugin package
        │   webview + react + solid-glass template
        │   plugin.yaml (id/version/kind/provides/permissions)
        │   ui/index.html + assets
        ▼
validate (manifest · version gate · content hash · template lint)
        │
        ▼
install → enable  (durable approval required)
        │
        ▼
return a UI deep link:  /plugins/<id>/
```

The spec's acceptance scenario is the shortest possible statement of the
feature: *say "make a small panel showing this session's token usage", and a
webview plugin package is generated, installed and enabled; the plugin appears
in the inventory; opening its UI shows the data; the audit has `install` and
`enable` records* (`spec: one sentence generates a plugin`).

## 2. Why it is a plugin, not a core mode

Three consequences fall out of making creation mode a plugin, and all three are
the point:

1. **Disable-able.** A deployment that does not want self-extension disables
   `keel.creation`; nothing else changes.
2. **Uniform review.** Generated plugins enter through exactly the same
   pipeline as hand-written ones: manifest validation, approval, permission
   enforcement, audit ([Permissions](/harness/plugins/permissions),
   [Lifecycle](/harness/plugins/lifecycle)). There is no "generated" shortcut
   around the rules.
3. **Uniform artifacts.** A generated plugin is a normal package in the
   managed directory: listable, disable-able, deletable, auditable
   (`spec: artifacts MUST be listable, disableable and deletable`).

## 3. Failure is a first-class outcome

Generation is a model-driven activity; it will fail sometimes, and the failure
path is specified as strictly as the success path (`spec: a failed generation rolls back`):

- a failed generation or validation produces **no half-installed state** — the
  install step is atomic at the package level;
- the error is returned to the user verbatim (not re-wrapped into a generic
  "something went wrong");
- temporary artifacts produced during generation are cleaned up;
- generation and installation both write audit rows, including the failure.

The rollback rule is the same discipline the service graph applies to
activation: *a failed activation rolls back its own effects; it never leaves a
half-registered listener* (`internal/servicegraph/lifecycle.go:36`,
`internal/agentext/plugin.go:317`).

## 4. How creation mode talks to the model

Creation mode is an ordinary participant in the Harness loop:

- it holds a conversation with the user (it is an extension of the session, not
  a separate wizard);
- generation happens as a tool-using turn: the model writes the package files
  in a workspace, the plugin's tools scaffold and validate them;
- the install/enable step **opens a decision** and waits — the approval is a
  durable row, not a modal dialog ([Approvals and
  questions](/harness/approvals-and-questions));
- every step lands in the ledger as normal events, so the whole act of
  creation is replayable and auditable like any other run.

## 5. What the generated package must satisfy

Generated plugins are held to the same standard as hand-written ones — no
"generated" exemption:

| Requirement | Where specified |
| --- | --- |
| `plugin.yaml` valid, `engines.keel` satisfied | [Manifest](/harness/plugins/manifest) |
| kind: `webview` (the template) with an optional sidecar | [Kinds](/harness/plugins/kinds) |
| permissions declared narrowly; bridge-only capability access | [Permissions](/harness/plugins/permissions) |
| theme from host tokens (solid glass), no hard-coded design system | [Plugin UI](/harness/plugins/ui) |
| deterministic build; assets under `ui/` | [Plugin UI](/harness/plugins/ui) |
| audit rows for generate/install/enable | §3 |

## 6. Relationship to the official suite

Creation mode and the official plugin suite are complementary halves of the
same thesis: the official suite (`plugins/COVERAGE.md`, waves W1–W3) brings
keel's existing capability surface *in* as plugins; creation mode lets a user
grow *new* ones without waiting for a release. Both end up as rows in the same
inventory, under the same permissions and the same audit.
