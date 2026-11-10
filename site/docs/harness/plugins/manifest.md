# Manifest (`plugin.yaml`)

> **Planned.** The package format and every rule on this page come from
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md` ("插件包与清单"
> and "发现、安装与包存储"). Nothing here is implemented in the current
> checkout; the implemented in-process contract is described at the end of this
> page for contrast.

## 1. The package

A plugin **is a directory**:

```
<plugins-root>/<id>/
├─ plugin.yaml            the manifest (required)
├─ ui/                    webview resources (required for kind: webview)
│   ├─ index.html
│   └─ assets/…
└─ bin/                   sidecar entry points (required for kind: sidecar)
```

The package contents are stored in a **managed directory**; a manifest may not
point at arbitrary external paths, and installation records a content hash
(`spec: 发现、安装与包存储`).

## 2. The manifest — full field list

```yaml
id: vendor.name                # ^[a-z][a-z0-9._-]{1,63}$; globally unique; one enabled version per id
version: 1.2.3                 # semver
kind: in-process | sidecar | webview
description: one line
engines: { keel: ">=0.2.0 <0.3.0" }        # host version gate
provides:
  tools: [name, ...]           # model-facing tool names; globally unique (conflict = fail-closed)
  hooks: [event, ...]          # must belong to the closed hook set
  triggers: [id, ...]
  ui: { entry: ui/index.html, title: "..." }   # required for webview
requires: [plugin-or-service-id, ...]          # missing dependency ⇒ not loaded (no half-load)
permissions:
  fs: [workspace-write]        # file effect classes
  net: [host, ...]             # allowed hosts
  secrets: [NAME, ...]         # allowed secret references
  tools: [name, ...]           # allowed tool calls
entry: { command: [...], args: [...], env: [...] }   # required for sidecar; may be empty for in-process
source: { type: builtin | local | git | npm, ... }
```

Field-by-field semantics and enforcement points:

| Field | Meaning | Enforced where |
| --- | --- | --- |
| `id` | global identity; the directory name; the audit subject | install validation |
| `version` | semver; a second enabled version of the same id is refused | install/upgrade |
| `kind` | one of three; drives which `entry`/`ui` shape is valid | install validation, [Kinds](/harness/plugins/kinds) |
| `engines.keel` | semver range the plugin supports; mismatch refuses install **and** enable with the mismatched range named | version gate |
| `provides.tools` | tool names the plugin contributes; a duplicate across enabled plugins is a fail-closed conflict | tool dispatch, assembly |
| `provides.hooks` | subscriptions to the **closed** event set (today: `subagent.*`, `trigger.*`, `turn.*`, `decision.*`; `internal/agentext/hooks.go:18`) | subscription registration |
| `provides.triggers` | trigger ids the plugin owns (`interval`/`cron`/`webhook`) | trigger engine |
| `provides.ui` | webview entry document + title; required for `kind: webview` | UI hosting |
| `requires` | plugin or service ids that must be present; missing ⇒ `disabled` with a reason, never a half-load | lifecycle assembly |
| `permissions` | the capability budget; see [Permissions](/harness/plugins/permissions) | dispatch, env construction, audit |
| `entry` | the sidecar process command/args/env (env only from allowlisted names) | process supervision |
| `source` | provenance of the package (`builtin`, `local`, `git`, `npm`, …) | install + audit |

Rules that are *not* field-level but are part of the same contract:

- **invalid manifest → refusal with a field-level reason**; the plugin
  directory is not adopted (`spec: 清单非法即拒绝`);
- **version gate** — `engines.keel` mismatch refuses install and enable, naming
  the supported range (`spec: 版本门`);
- **content hash + audit** — installation validates manifest, version gate and
  package content hash, and writes an audit record (`spec: 发现、安装与包存储`);
- **install is immediately visible** — after install, `GET /v1/plugins` lists
  the plugin (state `installed`/`enabled`) and the UI inventory refreshes
  (`spec: 安装即出现`).

## 3. Discovery, install and storage (planned API surface)

| Operation | Surface | Semantics |
| --- | --- | --- |
| discover | scan `plugins-root` at boot | plugins found on disk appear in the inventory |
| install | install API (local dir / git / package) | validate manifest + gate + hash; write audit |
| list | `GET /v1/plugins` (extended by the change) | states `installed`/`enabled`/`disabled`, diagnostics |
| uninstall | uninstall API | managed-directory removal + audit; cannot remove while providing to live runs |
| upgrade | same id, new version | version replacement; enable path re-runs dependency-ordered assembly |

Today's `GET /v1/plugins` already exists and answers the assembled surface —
generation, plugin states, hooks summary, tool names
(`internal/agentapi/ext.go:18`, `internal/agentext/plugin.go:505`). The change
extends that response with installed/disabled packages rather than inventing a
second endpoint.

## 4. What exists today instead of this manifest

The implemented in-process contract carries the same *concepts* without a file:
`ID`, `Version`, `Provides`, `Requires`, `Tools`, `Hooks`, `Triggers`,
`Effects`, `Start`, `Stop` (`internal/agentext/plugin.go:67`). The mapping is
one-way — the planned manifest is a serialization of the same declaration plus
everything the Go type deliberately left out (permissions, source, UI).

| `plugin.yaml` concept | Today's Go equivalent | Note |
| --- | --- | --- |
| `id` / `version` | `Plugin.ID` / `Plugin.Version` | reported by the snapshot |
| `provides` / `requires` | `Plugin.Provides` / `Plugin.Requires` | same dependency-topological assembly |
| `provides.tools` | `Plugin.Tools` | duplicate names fail-closed |
| `provides.hooks` | `Plugin.Hooks` | closed set, enforced by `Subscribe` |
| `provides.triggers` | `Plugin.Triggers` | owner is filled from the plugin id |
| `effects` (reversal contract) | `Plugin.Effects` | reverse-order cleanup with a partial-unload guard |
| `kind` / `permissions` / `source` | — | **no equivalent today**; in-process code is trusted by construction |
