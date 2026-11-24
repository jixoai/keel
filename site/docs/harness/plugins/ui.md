# Plugin UI

> **Implemented.** The hosting route `/plugins/<id>/` and the four-verb
> `keel.plugin.*` bridge both ship; the bridge also pushes the host theme tokens
> (`cmd/keel/plugins_ui.go:17`, `internal/harnessapi/plugin_bridge.go:1`). The
> template (webview + react + solid-glass) is in the repository and is what
> creation mode scaffolds (`plugins/templates/webview-react-solidglass/`).

## 1. How a plugin UI is hosted

```
<plugins-root>/<id>/ui/          host serves it at: /plugins/<id>/
├─ index.html                    entry document (provides.ui.entry)
└─ assets/**                     JS, CSS, fonts, images

WebView (web target)             Web deep link for plugin open
   │                                (TUI opens the URL instead of rendering)
   ▼
keel.plugin.* bridge             the ONLY capability channel:
                                 read own state · call own tools ·
                                 request approval · subscribe to events
```

The gate is the lifecycle state, and only `enabled` opens it
(`cmd/keel/plugins_ui.go:17`):

| State | Answer for `/plugins/<id>/…` |
| --- | --- |
| not installed | `404` — the id is not a fact of this deployment |
| `installed`, `disabled`, `failed`, `invalid` | `403` — the operator asked for it to be off |
| `enabled` | served, whether or not the plugin has a runtime surface (a UI-only webview is enabled but never assembled) |

Containment is part of the route: a `..` segment is refused, the resolved file
must stay inside the UI root **after** symlinks resolve, and `plugin.yaml`
itself is never served.

The same-origin design is not an accident: the web client is already served by
`keel serve` from the build directory (`cmd/keel/static.go:16`), so
`/plugins/<id>/` rides the same origin and needs no CORS — the property the
Harness API was designed around (`internal/harnessapi/api.go:1`).

## 2. The `keel.plugin.*` bridge

The bridge is the plugin UI's whole capability budget. Four verbs, no more
(`internal/harnessapi/plugin_bridge.go:75`):

| Bridge call | HTTP route | What it does |
| --- | --- | --- |
| read own state | `GET /v1/plugins/{id}/bridge/state` | the plugin's own inventory row (id, version, kind, state, provides, permissions, requires, tools, hooks, triggers, diagnostics, layer, slots, contributions, extension doc) plus the inventory revision/generation and the host **theme tokens** |
| call own tools | `POST /v1/plugins/{id}/bridge/call` | invoke a tool **this plugin** declares; another plugin's tool is out of the bridge's vocabulary, not a permission question |
| request approval | `POST /v1/plugins/{id}/bridge/approve`, then the `GET` / `POST` pair on `/v1/plugins/{id}/bridge/approvals/{decisionId}` | open a durable decision and wait for the answer |
| subscribe to events | `GET /v1/plugins/{id}/bridge/subscribe` (SSE, 15s heartbeat) | forwarded events relevant to the plugin |

Every verb requires the plugin to be **enabled** — the same gate as the hosting
route — and a verb the plugin's contributions did not declare is refused with a
typed `ERR_ACCESS_DENIED` (`internal/harnessapi/plugin_bridge.go:295`). A plugin
whose contributions declare no capabilities keeps the legacy full bridge (its
tools are still ownership-checked).

The state answer is an explicit projection: never the raw manifest document,
never the managed package path, never a provenance document, never a
credential. A plugin UI therefore cannot read a host secret through the bridge
even by accident.

Deliberately absent from the bridge: arbitrary HTTP, filesystem access, shell
execution, other plugins' tools, other sessions' events, and every secret. A
plugin that needs a network capability needs a sidecar with a declared
permission ([Permissions](/harness/plugins/permissions)).

## 3. The template: webview + react + solid-glass

One template is the standard — **webview + react + solid-glass** — for both
hand-written and creation-mode plugins. "Solid glass" is not a mood word here;
it is the theme the web client already ships:

- the token document is `web/theme.json` ("Keel Solid Glass"): OKLCH color
  variables for light/dark, radii (`radius`, `radius-card`, `radius-control`,
  `radius-pill`), `font-sans`/`font-mono` — consumed as CSS variables by the
  client, and handed to plugin UIs through the bridge's state verb
  (`internal/harnessapi/plugin_bridge.go:151`);
- a plugin UI must consume these host tokens instead of hard-coding values, so
  a plugin inherits the host's light/dark and spacing without shipping its own
  design system.

A minimal `ui/index.html` following the template:

```html
<!doctype html>
<html>
  <head>
    <meta charset="utf-8" />
    <title>example panel</title>
    <!-- host tokens: the same CSS variables the client consumes (web/theme.json) -->
  </head>
  <body>
    <div id="root"></div>
    <script type="module">
      // Bridge shell: the page is served under /plugins/<id>/ and the host
      // exposes the keel.plugin.* object to it.
      const state = await keel.plugin.state();          // own inventory row + theme
      const result = await keel.plugin.callTool("example_status", {});
      const answer = await keel.plugin.requestApproval({
        prompt: "Install this plugin?", options: [{ id: "allow" }, { id: "deny" }],
      });
      const unsubscribe = keel.plugin.onEvent((evt) => console.log(evt));
      document.querySelector("#root").textContent = JSON.stringify(result, null, 2);
    </script>
  </body>
</html>
```

> The four verbs are fixed; the exact JavaScript object shape is the host's
> bridge implementation, not a frozen wire contract.

## 4. TUI behavior

The TUI is a first-class client with a deliberate limitation:

- the TUI **must not render plugin UI** (no WebView, no HTML);
- a plugin row offers **Open**, which calls `openUrl` on the web deep link
  (`https://<host>/plugins/<id>/`);
- when the host has no URL opener (or the platform refuses it), the row keeps
  the copyable URL on screen instead — the plugin is still reachable, just not
  inside the terminal (`web/src/screens/plugins.tsx:13`, `:220`).

This mirrors how the TUI already treats the web: same data, different
rendering surface (`docs/WEBUI.md`). Note the packaging limit: `nsh package`
has no `tui` target, so the TUI runs from a development host
(`nsh run --target tui`) or an embedded `nsh-platform-tui`, and `KEEL_TUI_BIN`
points at a user-supplied binary (`web/README.md:167`).

## 5. What a plugin UI author should not do

- Do not fetch `/v1` directly except through the bridge's verbs — the bridge is
  what the permission model and the audit can see.
- Do not store credentials in the page: webview storage is per-host and not
  secret material.
- Do not reimplement host chrome; use the tokens and let the host own
  layout — the same rule the client itself follows (single projection, shared
  TSX across web and TUI).
