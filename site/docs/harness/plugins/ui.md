# Plugin UI

> **Planned.** The webview hosting path, the `keel.plugin.*` bridge and the
> template requirement come from
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md` ("UI surfaces and bridging",
> "TUI behavior", "creation mode"). The theme vocabulary and the client stack the template
> targets are **implemented** in this repository and are cited below.

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

Rules the spec fixes:

- the page loads from **`/plugins/<id>/`** and can display its own state and
  call its own tools — verification for this is part of the acceptance
  scenario (`spec: plugin UI works`);
- the bridge is the only channel: **the UI must never receive host secrets**
  (`spec: UI surfaces and bridging`);
- the theme must use **host tokens**, i.e. the webview + react + solid-glass
  template (§3).

The same-origin design is not an accident: the web client is already served by
`keel serve` from the build directory (`cmd/keel/static.go:16`), so
`/plugins/<id>/` rides the same origin and needs no CORS — the property the
AgentAPI was designed around (`internal/agentapi/api.go:1`).

## 2. The `keel.plugin.*` bridge

The bridge is the plugin UI's whole capability budget. Four verbs, no more:

| Bridge call | What it does | Backed by (today's analogue) |
| --- | --- | --- |
| read own state | the plugin's row from the inventory: id, version, state, provides, diagnostics | `GET /v1/plugins` snapshot (`internal/agentapi/ext.go:18`) |
| call own tools | invoke a tool **this plugin** declared (the host resolves the owning plugin from the session) | tool dispatch through the same registry the model uses |
| request approval | open a decision and wait for the answer | the decision primitive ([Approvals](/harness/approvals-and-questions)) |
| subscribe to events | receive forwarded events relevant to the plugin (its own trigger/decision/subagent lifecycle) | the hook bus and its closed event set (`internal/agentext/hooks.go:18`) |

Deliberately absent from the bridge: arbitrary HTTP, filesystem access, shell
execution, other plugins' tools, other sessions' events, and every secret. A
plugin that needs a network capability needs a sidecar with a declared
permission ([Permissions](/harness/plugins/permissions)).

## 3. The template: webview + react + solid-glass

The spec requires exactly one template — **webview + react + solid-glass** —
for both hand-written and creation-mode plugins (`spec: creation mode`, "the generated plugin package
(webview+react+solid-glass template)"). "Solid glass" is not a mood word here; it
is the theme the web client already ships:

- the token document is `web/theme.json` ("Keel Solid Glass"): OKLCH color
  variables for light/dark, radii (`radius`, `radius-card`, `radius-control`,
  `radius-pill`), `font-sans`/`font-mono` — consumed as CSS variables by the
  client;
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
      // Bridge shell: provided by the host page when served under /plugins/<id>/
      const state = await keel.plugin.state();          // own inventory row
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

> The exact bridge object shape is part of the planned change; the snippet
> shows the four verbs §2 fixes, not a frozen API.

## 4. TUI behavior

The TUI is a first-class client with a deliberate limitation
(`spec: TUI behavior`):

- the TUI **must not render plugin UI** (no WebView, no HTML);
- a plugin row offers **Open**, which calls `openUrl` on the web deep link
  (`https://<host>/plugins/<id>/`);
- when the host has no URL opener, the TUI shows the copyable URL text
  instead — the plugin is still reachable, just not inside the terminal.

This mirrors how the TUI already treats the web: same data, different
rendering surface (`web/README.md`, `docs/WEBUI.md`).

## 5. What a plugin UI author should not do

- Do not fetch `/v1` directly except through the bridge's verbs — the bridge is
  what the permission model and the audit can see.
- Do not store credentials in the page: webview storage is per-host and not
  secret material.
- Do not reimplement host chrome; use the tokens and let the host own
  layout — the same rule the client itself follows (single projection, shared
  TSX across web and TUI).
