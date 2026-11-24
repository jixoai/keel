# Creation mode

> **Implemented**: `keel.creation` is a bundled plugin with six actions, enabled
> by default like every builtin except `keel.testkit`
> (`internal/harnessext/creation.go:91`, `cmd/keel/serve.go:1752`), and it
> scaffolds the webview + react + solid-glass template
> (`plugins/templates/webview-react-solidglass/`).
> **Deferred**: the end-to-end gate G8 — "one sentence → a running plugin" — has
> not passed (`openspec/changes/archive/2026-11-24-keel-harness-v2/DISPOSITION.md:20`).
> This page describes what ships and states the gap; it does not claim a fully
> accepted feature.

Creation mode is the answer to a simple question: *if everything is a plugin,
who writes the plugins?* The answer is the agent itself — under the same
approval and audit rules as everything else.

## 1. What it is

A **built-in plugin** named `keel.creation`. It is not a privileged core
feature: it can be disabled like any other plugin, and it is subject to its own
permission set. The manifest is
`internal/pluginpkg/builtin/keel.creation.yaml`; the implementation lives in
`internal/harnessext/creation.go`. What it does:

```
user describes a capability in natural language
        │
        ▼
keel.creation: scaffold a plugin package
        │   webview + react + solid-glass template
        │   plugin.yaml (id/version/kind/provides/permissions)
        │   ui/index.html + assets
        ▼
validate (manifest · version gate · content tree · template lint)
        │
        ▼
install → enable  (durable approval required)
        │
        ▼
return a UI deep link:  /plugins/<id>/
```

The six actions are the plugin's whole surface (`internal/harnessext/creation.go:91`):

| Action | What it does |
| --- | --- |
| `catalog` | read-only aggregate of installed plugins and their surface |
| `scaffold` | render the template into `<plugins-root>/.creation/<id>`, then validate the **output** with the real package validator |
| `validate` | field-level problems of a staged package, plus structural/host-prerequisite warnings |
| `install` | durable approval → manager `Install` → manager `Enable` → deep link |
| `list` | what this session generated and where it stands |
| `remove` | durable approval → disable → uninstall → delete the staged artifact |

The model reaches them through the harness-owned `creation` aggregate tool; the
`keel.creation.*` names are internal handlers and are never published to the
model (`internal/harnessext/creation.go:88`). Creation mode does **not** call a
model from inside a plugin: the main session's agent writes the package, and
these tools are its hands on the filesystem and on the plugin manager.

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
3. **Uniform artifacts.** A generated plugin is a normal package in the managed
   directory: listable, disable-able, deletable, auditable.

## 3. Failure is a first-class outcome

Generation is a model-driven activity; it will fail sometimes, and the failure
path is implemented as strictly as the success path:

- a scaffold renders into a temporary directory and only moves into the staging
  path after the **real** validator accepts it, so a failed generation leaves
  nothing behind (`internal/harnessext/creation.go:28`);
- a failed install removes the staged package;
- the error is returned to the user verbatim (not re-wrapped into a generic
  "something went wrong");
- every step writes audit rows, including the failure.

The rollback rule is the same discipline the service graph applies to
activation: *a failed activation rolls back its own effects; it never leaves a
half-registered listener* (`internal/servicegraph/lifecycle.go:36`,
`internal/harnessext/plugin.go:317`).

## 4. How creation mode talks to the model

Creation mode is an ordinary participant in the Harness loop:

- it holds a conversation with the user (it is an extension of the session, not
  a separate wizard);
- generation happens as a tool-using turn: the model writes the package files
  in a workspace, the plugin's tools scaffold and validate them;
- the install/enable step **opens a decision** and waits — the approval is a
  durable row, not a modal dialog ([Approvals and
  questions](/harness/approvals-and-questions)). It is two-phase because a
  human answer does not fit in one tool call: call `install` to get
  `approval_required` + `decisionId` + `answerUrl`, have it answered, then call
  `install` again with that `decisionId`. A refusal changes nothing.
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

## 7. Acceptance status — what has not passed

Two parts of the standard around creation mode are **not** accepted, and the
documentation says so instead of implying completion:

| Item | State | Evidence |
| --- | --- | --- |
| G8 end-to-end: AI writes a native plugin in a live session, start to deep link | **Deferred** — the end-to-end run has not gone through; the webview E2E path exists | `openspec/changes/archive/2026-11-24-keel-harness-v2/DISPOSITION.md:20` |
| v2 extensibility aggregation and the native chunk main path | **partial** — the declaration format is frozen and validated, but aggregating every installed plugin's `extensibility.json` into the creation surface is unfinished | `plugins/COVERAGE.md` (`keel.creation` → `partial`) |

So the honest summary is: the six actions, the template, the approval path and
the rollback discipline ship; the "say one sentence, get a running plugin"
acceptance gate is open. Until it closes, treat creation mode as implemented but
**not** as a fully accepted feature.
