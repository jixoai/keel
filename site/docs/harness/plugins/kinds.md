# Plugin kinds

> **Planned.** The three kinds, their `entry` shapes and the isolation promises
> come from `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`
> ("the three kinds kind"). The supervision machinery a sidecar reuses already exists in
> `internal/gateway` — that part is implemented and is cited below.

A plugin is exactly one of three kinds. The kind decides what `entry` means,
where the code runs, what isolation exists, and which failure modes the host
must handle.

| Kind | Runs | `entry` | Isolation | Typical use |
| --- | --- | --- | --- | --- |
| `in-process` | compiled into the host | empty (config only) | none (trusted) | official capabilities: `keel.core`, `keel.llm`, … |
| `sidecar` | external process | `command/args/env` | process boundary | anything that must not be able to take the host down |
| `webview` | UI in the host's WebView | `ui/` package (+ optional sidecar backend) | UI sandbox + bridge | dashboards, panels, creation-mode artifacts |

## 1. `in-process`

Official plugins that ship with the binary. The manifest controls enablement
and configuration; the code is compiled in. There is no sandbox and none is
claimed: an in-process plugin has full host privileges by construction. This is
the kind the current `agentext` runtime already implements for built-ins
(`internal/agentext/builtin.go:38`, `:256`).

## 2. `sidecar`

An external process speaking **stdio, newline-delimited JSON-RPC**
(`spec: the three kinds kind`). The method set is the gateway's existing one — the plugin
spec explicitly reuses it:

| Method | Direction | Semantics |
| --- | --- | --- |
| `initialize` | request | negotiate protocol version, provider kind, capabilities; an incompatible version refuses activation |
| `tools/list` | request | the tools the plugin provides |
| `tools/call` | request | execute one tool call |
| `health` | request | `{status: ok|degraded|unavailable}` |
| `close` | request | close a session, or ask the backend to shut down gracefully when it carries no session |

The constants are implemented today for the external gateway:
`internal/gateway/protocol.go:86` (`initialize`), `:100` (`health`), `:103`
(`close`), `:106` (`tools/list`), `:108` (`tools/call`).

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
- **restart policy** — a crashed backend is restarted with backoff per
  `gateway.RestartPolicy` (`internal/gateway/process.go:26`).

The isolation promise, restated plainly (`spec: sidecar crash isolation`):

> When a sidecar plugin's process is killed, its tool calls fail with a clear
> error and the plugin's state becomes `failed`; every other plugin and the
> Harness are unaffected; re-enabling the plugin can restore it.

Two honest limits carry over from the external-gateway review and must be
restated for plugins (`docs/DSH-REFERENCE.md` §4):

1. **No resource budget yet.** Session count is capped (`maxSessions`), but CPU,
   memory and file descriptors are a deployment concern (cgroup/ulimit/nodes)
   — a planned per-backend budget is the fix.
2. **No sweep after a host `SIGKILL`.** Normal reclamation runs only on the
   close path. If the host is killed, the contract is the protocol one — the
   parent's death closes stdin and the child must exit on EOF (verified for
   DSH: EOF → exit 0, `docs/DSH-REFERENCE.md` §3.2). A pidfile-based sweep or
   an external supervisor is the planned hardening.

## 3. `webview`

A UI package under `ui/`, hosted by the host and rendered in a WebView
(`spec: the three kinds kind` and "UI surfaces and bridging"):

```
<plugins-root>/<id>/ui/
├─ index.html          entry document (manifest: provides.ui.entry)
└─ assets/**           JS/CSS/images — served from /plugins/<id>/
```

- The host serves the package at **`/plugins/<id>/`**; opening the plugin from
  the inventory loads that path (`spec: UI surfaces and bridging`).
- The UI talks to the host **only** through the bridge `keel.plugin.*`
  (read own state, call own tools, request an approval, subscribe to events)
  and never receives host secrets (`spec: UI surfaces and bridging`). Details in
  [Plugin UI](/harness/plugins/ui).
- A webview plugin **may carry a sidecar backend**; the UI then reaches its own
  process through its own tools, never by direct IPC.
- **TUI behavior** (`spec: TUI behavior`, planned): the TUI must not render plugin
  UI. A plugin row offers "Open", which calls `openUrl` on the web deep link;
  when the host has no URL opener, it must display the copyable URL instead.

## 4. Choosing a kind

| If you need… | Choose | Because |
| --- | --- | --- |
| a dashboard or panel | `webview` | the UI is the deliverable; no process to supervise |
| a tool that talks to an external system with its own runtime/toolchain | `sidecar` | crash isolation and a protocol boundary |
| a capability that is part of the product itself | `in-process` | zero IPC overhead; official code is trusted |
| anything in a language that is not Go, with UI | `webview` + sidecar | UI and logic both stay out of the host process |

The decision is recorded in `plugin.yaml` (`kind`) and is not switchable at
runtime: changing the kind means shipping a different package.
