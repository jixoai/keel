# W12 UI slots + creation mode — first vertical slice

Date: 2026-11-22. Change: `keel-harness-v2`, W12 (`keel.ui` slot mechanism +
four forms + bridge + Native fixture E2E). This evidence covers the slice
delivered on `task/keel-w12-ui`; it does not claim the whole W12 board (42
items).

## 1. What is live

### 1.1 Slot registry (`web/src/plugins/`)

| Piece | File | Behaviour |
| --- | --- | --- |
| Declarations + strict validation | `slots.ts`, `validation.ts` | required fields and enums fail closed; unknown optional keys are warnings; slot ids must be namespaced by their declaring plugin |
| Registry | `registry.ts` | register/unregister a plugin's whole declaration atomically; duplicate slot/contribution/singleton handling; load/unload; target fallback; per-contribution failure state |
| First-party base slots | `base.ts` | `keel.ui.route/nav/action/tool-renderer/message-part/sidebar/settings-section/surface` plus `keel.ui.top-band`; all declared as the `keel.ui` plugin, not as harness built-ins |
| Four forms | `forms.ts`, `loader.ts` | `chunk` = NSH dynamic import in the host VM (bundled fixture and `/plugins/<id>/…` on web); `package` = explicit `ERR_UNSUPPORTED` (runner not wired) and `ERR_TARGET_UNAVAILABLE` in TUI; `iframe` = explicit browser frame, `ERR_TARGET_UNAVAILABLE` in TUI; `data` = JSON validated against the slot schema |
| Inventory sync | `sync.ts` | `GET /v1/plugins` is the only source; enabled plugins register, disabled/uninstalled ones unregister; `provides.slots` or top-level `slots`; `contributes` (plus an early `contributions` alias) |
| Bridge | `bridge.ts`, `web/host.js` | scoped `keel.plugin.scope` → `keel.plugin.call`; capability filter (`state/call/approve/subscribe`), tool allowlist, typed codes; SSE fan-out by plugin id |
| Harness projection | `internal/harnessext/plugin_extension.go`, `plugin_manager_view.go`, `internal/harnessapi/plugin_bridge.go` | tolerant reader for `layer` / `provides.slots` / `contributes` / `extensibility.json`; shape validation at install; declaration carried in `GET /v1/plugins` and the bridge state allowlist; server-side capability gate |

Duplicate policy (pinned by `registry.test.ts`):

- same provider, same slot id: replace;
- two providers, same slot id: `ERR_SLOT_CONFLICT` (the failed declaration
  leaves the old one untouched);
- same plugin, same contribution id: replace;
- another plugin, same contribution id: `ERR_CONTRIBUTION_CONFLICT`;
- a second plugin into a `single` slot: `ERR_SLOT_FULL`.

Unloading a slot provider removes the provider's slots and every
contribution targeting them, and records `ERR_SLOT_UNKNOWN` for the orphaned
contributor.

### 1.2 UI integration (no new top-bar pollution)

| Slot | Where it renders |
| --- | --- |
| `keel.ui.top-band` | the existing A4.4f band — the brand strip becomes the plugin row only when a contribution exists; `shell.tsx` reads `topBandHasContent()` |
| `keel.ui.sidebar` | a plugin section under the workspace session list |
| `keel.ui.nav` / `.action` | command palette rows and the settings page's plugin action buttons |
| `keel.ui.settings-section` | the settings page's 插件 section |
| `keel.ui.tool-renderer` | above the text result inside an existing tool row |
| `keel.ui.message-part` | before an assistant segment's text |
| `keel.ui.route` | the plugin page's native route (a chunk renders instead of the hosted webview) |
| `keel.ui.surface` | the plugin detail page (package = explicit unsupported face; iframe = browser frame) |

The shell's top band still has no chapter navigation, search, todo, account or
settings gear; the rail/bottom-tab contract is unchanged.

### 1.3 Fixture Native plugin E2E (existing manager path)

- Package: `internal/harnessext/testdata/w12-ui-fixture/` (`plugin.yaml`,
  `extensibility.json`, `EXTENSIBILITY.md`, `ui/`).
- Flow: `Manager.Install` → `Enable` → `Snapshot` exposes `layer: ui` and six
  contributions → `Disable` → `Uninstall` → no inventory row and no managed
  directory (`TestManagerInstallCarriesContributionAndUninstallLeavesNone`).
- Rendering: the same contribution shape is shipped as
  `web/src/plugins/fixtures/hello-chunk.tsx` and entered as
  `fixture:w12-hello`. It is a real NSH chunk: `nsh build --split` emits
  `hello-chunk.js`; the TUI host and the browser host both render it.
- The fixture plugin is opt-in in the development transport
  (`?fixture=1&plugins=1` / `host.params.plugins === true`), so existing
  product-shaped fixture suites are untouched.

### 1.4 Bridge capability filtering

- `web/host.js` issues a per-plugin scope token; the app passes only that
  token to a contribution's scoped bridge.
- Undeclared verbs, tools outside `provides.tools`, unknown tokens and expired
  tokens fail with `{ok:false, code:"ERR_ACCESS_DENIED", …}` before any HTTP
  request (pinned by `host.selftest.mjs`).
- The server refuses `call`/`approve`/`subscribe` when the plugin's
  contributions did not declare the capability, and still enforces tool
  ownership (`plugin_bridge_capability_test.go`).
- `chunk` shares the host VM (spec: no isolation). This is the documented
  trust boundary; the bridge is the capability boundary, not a sandbox against
  malicious same-VM code.

## 2. Deferred / explicit limitations

| Item | State |
| --- | --- |
| `package` runner (independent VM + `HostLimits`, `<Window>`) | deferred; every target shows `ERR_UNSUPPORTED` with the reason; TUI shows `ERR_TARGET_UNAVAILABLE` |
| Runtime-installed chunk in TUI/native | bundled chunks render in TUI (fixture proves it); a package-supplied chunk name not in the bundle is an explicit `ERR_TARGET_UNAVAILABLE` (QuickJS only accepts plain chunk names from the bundle/chunk dir) |
| `extensibility.json` aggregation in `keel.creation` | still W0.5-6 (`creation.catalog` unchanged); this slice reads the document for UI declarations |
| `layer` / `provides.slots` / `contributes` in `internal/pluginpkg.Manifest` | owned by W0.5; this slice does not edit `pluginpkg` and reads the documented shape from the parsed manifest when present, the raw `plugin.yaml`, and `extensibility.json` |
| `macos` packaged target / NSH 0.5.0 | not exercised in this slice (this host runs nsh 0.4.0; `nsh build` has no `--target`); web + TUI evidence only |
| `iframe` theming | explicitly labelled `iframe · 非原生主题`; never presented as native |

## 3. Commands and results

```text
cd web
nsh check                         # pass (no output)
nsh build --split                 # built App.tsx ... 1 chunk ... hello-chunk.js emitted
nsh build                         # built App.tsx ... 1 asset
nsh package --target web -o /tmp/w12-web
                                  # packaged /tmp/w12-web/Keel, 10.7 MB
nsh test --target tui src/plugins/registry.test.ts
                                  # 15 tests: 15 passed
nsh test --target tui src/plugins/slots.tui.test.tsx
                                  # 2 tests: 2 passed
nsh test --target tui App.tui.test.tsx
                                  # 12 tests: 12 passed
nsh test --target tui             # 297 tests: 297 passed (6m 5s, gated build)
                                  # targeted reruns after the final registry/App
                                  # edits: registry 15/15, slots 2/2, App 12/12
node --test web/host.selftest.mjs # 24 tests: 24 passed (8 plugin-bridge cases)
go test ./internal/harnessext     # pass (incl. extension decode/validate/install/uninstall)
go test ./internal/harnessapi -run TestPluginBridge
                                  # pass (state allowlist + capability gate + typed codes)
go build ./...                    # pass
```

`nsh build --target web` is not a flag this CLI version accepts (`nsh 0.4.0`
nightly: `build` has `--split`/`--release`, not `--target`); the web target is
built with `nsh package --target web` above.

Browser evidence (packaged build served on 127.0.0.1:13681,
`?fixture=1&plugins=1`, headless Chrome via playwright-core):

| Viewport | `documentElement.scrollWidth` | `innerWidth` | Horizontal overflow |
| --- | --- | --- | --- |
| 1440 | 1440 | 1440 | none |
| 1440 settings | 1440 | 1440 | none |
| 1440 plugin route | 1440 | 1440 | none |
| 390 | 390 | 390 | none |

Screenshots (relative to this file):

- `w12-ui/workspace-1440.png` — top band chunk + sidebar chunk.
- `w12-ui/workspace-390.png` — phone top band and bottom tabs, no overflow.
- `w12-ui/settings-plugins-1440.png` — settings page with action slot.
- `w12-ui/settings-plugins-bottom-1440.png` — settings-section slot.
- `w12-ui/plugin-route-1440.png` — native route chunk (no webview).
- `w12-ui/report.json` — assertions, overflow numbers, console errors
  (one favicon 404 from the static server; no app errors).

## 4. Negative controls (all currently failing closed)

| Control | Expected |
| --- | --- |
| slot id outside the declaring namespace | `ERR_SLOT_INVALID` |
| empty forms / unknown target / unknown form | `ERR_SLOT_INVALID` / `ERR_FORM_NOT_ALLOWED` |
| `iframe` into a chunk-only slot | `ERR_FORM_NOT_ALLOWED`, no half entry |
| chunk with no entry / `../` escape | `ERR_CONTRIBUTION_INVALID` |
| `data` nav without its required label | `ERR_CONTRIBUTION_INVALID` |
| duplicate contribution id across plugins | `ERR_CONTRIBUTION_CONFLICT` |
| second contribution into a singleton | `ERR_SLOT_FULL` |
| package/iframe on TUI | `ERR_TARGET_UNAVAILABLE`, explicit sentence |
| bridge verb not declared (`call`/`approve`/`subscribe`) | `ERR_ACCESS_DENIED`, no HTTP request |
| tool not in `provides.tools` | `ERR_ACCESS_DENIED` (client and server) |
| unknown bridge verb (`keel.admin.*`) | `ERR_ACCESS_DENIED` |
| install a bad extension declaration | field-level install refusal, no row |

## 5. Known test-environment flake

`go test ./internal/harnessapi` also runs the pre-existing
`TestMCPAppRejectionAuditSampling`, which samples its audit window with the
real clock and fails when the 1000-request overflow loop takes longer than one
second (`rate audit rows ... = 3/4, want 2`). It fails independently of this
slice (no file under `internal/harnessapi/mcp_app_*` was touched) and is
reported rather than silently retried. The targeted bridge tests
(`go test ./internal/harnessapi -run TestPluginBridge`) pass.

## 6. Files owned by this slice

- New: `web/src/plugins/{base,types,errors,validation,forms,loader,bundled,registry,sync,bridge,index,slots.tsx}`, `web/src/plugins/fixtures/hello-chunk.tsx`, `web/src/plugins/{registry.test.ts,slots.tui.test.tsx}`, `internal/harnessext/plugin_extension.go`, `internal/harnessext/plugin_extension_test.go`, `internal/harnessext/testdata/w12-ui-fixture/`, `internal/harnessapi/plugin_bridge_capability_test.go`.
- Touched for integration: `web/App.tsx`, `web/host.js`, `web/host.selftest.mjs`, `web/src/data/{contract,fixture,store}.ts`, `web/src/screens/{session-list,settings,messages,plugins,command-palette}.tsx`, `web/src/screens/books/{shell,plugin-top-panels}.tsx`, `internal/harnessext/{plugin_manager,plugin_manager_ops,plugin_manager_view}.go`, `internal/harnessapi/plugin_bridge.go`.
- Not touched: `openspec/changes/keel-harness-v2/tasks.md`, `internal/pluginpkg/**`, `plugins/keel.plugin-market/catalog.json`, `cmd/r1diff`, `internal/store/schema.sql`, `cmd/keel/serve.go`, `plugins/keel.oar/**`, `plugins/keel.supervisor/**`.
