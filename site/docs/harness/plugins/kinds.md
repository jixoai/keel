# Plugin kinds

> **Implemented**, with one named gap. All three kinds are carried by this
> build: `in-process` (bundled manifests), `sidecar` (external process over the
> gateway's stdio protocol) and `webview` (UI package hosted at `/plugins/<id>/`).
> What is **Deferred** is the `agent_backend` sidecar: the loader accepts
> `tool_provider` connections only, so the OAR adapter ships as a target
> manifest, not as a loadable package (`plugins/keel.oar/plugin.yaml:3`).

A plugin is exactly one of three kinds. The kind decides what `entry` means,
where the code runs, what isolation exists, and which failure modes the host
must handle.

| Kind | Runs | `entry` | Isolation | Typical use |
| --- | --- | --- | --- | --- |
| `in-process` | compiled into the host | empty (config only) | none (trusted) | official capabilities shipped in the binary |
| `sidecar` | external process | `command/args/env` | process boundary | anything that must not be able to take the host down |
| `webview` | UI in the host's WebView | `ui/` package (+ optional sidecar backend) | UI sandbox + bridge | dashboards, panels, creation-mode artifacts |

Which kinds this build can *load* is a second question, answered by the loaders
map. `keel serve` registers exactly one loader — the sidecar loader
(`cmd/keel/serve.go:1769`), and `loaderForKind` also routes a `webview` package
that carries an `entry.command` through it (`internal/harnessext/
plugin_manager_ops.go:409`). A `webview` package with no backend has no runtime
surface at all: enabling it gates its hosted UI and assembles nothing
(`internal/harnessext/plugin_manager_ops.go:447`). An `in-process` package
downloaded from disk has no loader — code of that kind must be compiled into the
binary, so enabling such a package answers `409` with "kind has no loader in
this build" (`internal/harnessext/plugin_manager.go:64`).

## 1. `in-process`

Official plugins that ship with the binary. The manifest controls enablement
and configuration; the code is compiled in. There is no sandbox and none is
claimed: an in-process plugin has full host privileges by construction. This is
the kind the current `harnessext` runtime implements for built-ins
(`internal/harnessext/builtin.go:38`, `:256`).

The bundled set is twelve manifests embedded in the binary
(`internal/pluginpkg/builtin/builtin.go:26`): `keel.file`, `keel.model`,
`keel.bot`, `keel.team-bot`, `keel.spill`, `keel.tasks`, `keel.shell`,
`keel.approval`, `keel.notify`, `keel.creation`, `keel.example` and
`keel.testkit`. All are enabled by default except `keel.testkit`, which is the
E2E grant-filter fixture (`cmd/keel/serve.go:1752`). Bundled plugins can be
disabled through the API but not uninstalled or updated from a package: their
rows say `bundled: true` and the action is refused
(`internal/harnessext/plugin_manager_ops.go:824`).

## 2. `sidecar`

An external process speaking **stdio, newline-delimited JSON-RPC 2.0** over the
gateway's existing contract — the loader re-invents nothing about the wire
(`internal/harnessext/plugin_sidecar.go:22`):

| Verb | How it appears on the wire | Semantics |
| --- | --- | --- |
| `initialize` | the handshake (`gateway.Conn.Connect`) | negotiate protocol version and provider kind; a `sidecar` must answer `provider{type: tool_provider}` with `capabilities.tools`; anything else is refused before a single tool runs |
| `tools/list` | `tools/list` (paged) | the tools the plugin provides |
| `tools/call` | `tools/call` | execute one call, with a request timeout, caller cancellation (propagated as `notifications/cancelled`) and definite errors on a dead connection |
| `health` | **not a wire method on this surface** | supervision-level only: the loader's own health report (connection state + process liveness + recorded failure) becomes the plugin's state in the inventory |
| `close` | a best-effort notification, then the shutdown ladder | stdin close → `SIGTERM` to the **process group** → `SIGKILL` to the process group |

The `agent_backend` provider kind (`session.open` / `message.send` /
`event.stream`) is deliberately not accepted: a `tool_provider` connection
rejects the agent surface's methods by construction, and inventing a second
protocol for plugins was rejected. That is why `keel.oar` is a **DEFERRED**
target manifest rather than an installable package.

What the plugin inherits from the same implementation (all **implemented**, in
`internal/gateway`):

- **env whitelist** — a spawned provider gets `MinimalEnv` plus explicitly
  declared names only; host secrets are never visible by default
  (`internal/gateway/process.go:106`);
- **clean close** — closing walks stdin close → grace period → SIGTERM to the
  **process group** → SIGKILL, and is idempotent
  (`internal/gateway/stdio.go:228`, `:379`);
- **process-group reclamation** — `KillTree` exists and the smoke suite asserts
  zero leftover grandchildren
  (`internal/gateway/process_unix.go:58`, `scripts/external-gateway-smoke.sh`);
- **per-call error surface** — a cancelled call, a timeout and a dead connection
  are all definite errors, never a silent hang.

The isolation promise, restated plainly:

> When a sidecar plugin's process is killed, its tool calls fail with a clear
> error and the plugin's state becomes `failed`; every other plugin and the
> Harness are unaffected; re-enabling the plugin reconnects it.

Two honest limits carry over from the external-gateway review and must be
restated for plugins (`docs/DSH-REFERENCE.md` §4):

1. **No resource budget yet.** Session count is capped (`maxSessions`), but CPU,
   memory and file descriptors are a deployment concern (cgroup/ulimit/nodes)
   — a per-backend budget is not implemented.
2. **No sweep after a host `SIGKILL`.** Normal reclamation runs only on the
   close path. If the host is killed, the contract is the protocol one — the
   parent's death closes stdin and the child must exit on EOF (verified for
   DSH: EOF → exit 0, `docs/DSH-REFERENCE.md` §3.2). A pidfile-based sweep or
   an external supervisor is not implemented.

## 3. `webview`

A UI package under `ui/`, hosted by the host and rendered in a WebView:

```
<plugins-root>/<id>/ui/
├─ index.html          entry document (manifest: provides.ui.entry)
└─ assets/**           JS/CSS/images — served from /plugins/<id>/
```

- The host serves the package at **`/plugins/<id>/`**, and only for an
  **enabled** plugin; the manifest itself is never served and `..` traversal is
  refused (`cmd/keel/plugins_ui.go:17`).
- The UI talks to the host **only** through the bridge `keel.plugin.*`
  (read own state, call own tools, request an approval, subscribe to events)
  and never receives host secrets. Details in [Plugin UI](/harness/plugins/ui).
- A webview plugin **may carry a sidecar backend** (`entry.command`); the UI
  then reaches its own process through its own tools, never by direct IPC. A
  UI-only webview is enabled but never assembled.
- A webview package may also contribute a **chunk** instead of an iframe; a
  pure chunk package may omit `provides.ui` entirely and renders inside the host
  tree through the client slot registry ([Slots](/harness/plugins/slots)).
- **TUI behavior**: the TUI must not render plugin UI. A plugin row offers
  "Open", which calls `openUrl` on the web deep link; when the platform refuses
  the hand-off, the row keeps the copyable URL on screen instead
  (`web/src/screens/plugins.tsx:13`, `:220`).

## 4. Choosing a kind

| If you need… | Choose | Because |
| --- | --- | --- |
| a dashboard or panel | `webview` | the UI is the deliverable; no process to supervise |
| a tool that talks to an external system with its own runtime/toolchain | `sidecar` | crash isolation and a protocol boundary |
| a capability that is part of the product itself | `in-process` | zero IPC overhead; official code is trusted |
| anything in a language that is not Go, with UI | `webview` + sidecar | UI and logic both stay out of the host process |

The decision is recorded in `plugin.yaml` (`kind`) and is not switchable at
runtime: changing the kind means shipping a different package. And a kind only
works if this build has a loader for it — `in-process` means "compiled in", not
"dropped into the plugins root".
