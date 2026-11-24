---
title: Assembly
description: The four-layer assembly behind a loadout — base → mode → preset → config.toml, row ids, overlay rules, the strict config.toml subset and the capability matrix.
---

# Assembly

A loadout is an immutable snapshot pinned per run ([Architecture §4](/core/architecture)). This
page is about the input side: how the effective row set is stacked from four layers, and what is
(and is not) wired in production.

> **Status.** The pure model (`internal/assembly`), the disk loader (`internal/configfile`) and the
> `serve` wiring ship. Three parts are explicitly deferred: the `headless`/`tui`/`sdk` mode
> bundles, real local/remote/SSH seam providers, and `config.toml` rows entering the production
> service graph (`docs/ASSEMBLY.md:3`, `docs/ASSEMBLY.md:35`, `docs/ASSEMBLY.md:36`).

## 1. The four layers

```text
base bundle      defaults: kernel seams, memory hygiene, product services
   ↓ row id      (the only layer allowed to add rows)
mode bundle      serve / headless / tui / sdk
   ↓
preset           behaviour overlays for tools, prompts and skills
   ↓
config.toml      user file: immutable versions + a ledger anchor
```

`base` is the only layer that may introduce a row; the others are pure overlays
(`docs/ASSEMBLY.md:44`, `docs/ASSEMBLY.md:70`). A stack must contain exactly one `base` document;
`mode`, `preset` and `config` are optional and, when present, must appear in that canonical order,
each at most once, with a `mode` name in `serve`/`headless`/`tui`/`sdk` (`docs/ASSEMBLY.md:75`).

## 2. Row identity, not position

```text
<plugin-id>#<row>        keel.shell#bash-local
```

The id is a stable identity: `plugin-id` follows the plugin namespace and the row half has exactly
one `#` (`docs/ASSEMBLY.md:53`). Document order only decides base declaration order and the
tie-break in activation; it never decides what an overlay hits (`docs/ASSEMBLY.md:61`).

| Rule | Behaviour |
|---|---|
| Match | row id only; last write wins (mode over base, preset over both, config over all three) (`docs/ASSEMBLY.md:68`) |
| Granularity | the whole row is replaced — `impl` + `params`; params are never merged key by key |
| Add | only `base` may add rows; an overlay naming an undeclared row is `row-unknown` (`docs/ASSEMBLY.md:70`) |
| Order | effective rows keep base order; overlay rows must follow it, otherwise `row-order` |
| Structure | an overlay cannot change `layer` (`row-layer`) or `requires` (`row-requires`) (`docs/ASSEMBLY.md:72`) |
| Absent | `absent = true` keeps the row in the effective set, removes it from activation, and dependents become `Blocked` |
| Anonymous | base may carry id-less rows (kept, never overridable); an id-less overlay row is rejected (`row-id`) (`docs/ASSEMBLY.md:74`) |

## 3. Activation

`requires` is declared only in base. `Assembly.Activation()` derives the order with a Kahn
topological sort: availability activates a row, ties break by effective position, and a missing or
absent dependency does not degrade the dependent — it enters `Assembly.Blocked()` with
`Reason=absent|dependency` and the missing row id (`docs/ASSEMBLY.md:80`). The service adapter
treats `Blocked` as a loadout construction failure, not as something to ignore
(`docs/ASSEMBLY.md:91`).

## 4. config.toml is a strict subset

```toml
version = 1

[filesystem.capture]
mode = "checkpoint"        # per-tool-call | checkpoint | manual

[[rows]]
id = "keel.shell#bash-local"
impl = "ssh"
params.host = "devbox"
params.port = '22'

[[rows]]
id = "keel.ui#shell"
absent = true
```

Only `version = 1`, `[[rows]]` and `[filesystem.capture]` are allowed; a row may carry `id`,
`impl`, `absent` and `params.<key>`; values are strings (`"..."` or `'...'`), and arrays, inline
tables and multi-line strings are rejected with a `config.toml:<line>` path instead of being
half-applied (`docs/ASSEMBLY.md:151`). Validation is one pass over the whole stack and the error
codes are stable: `missing-base`, `stage-order`, `stage-duplicate`, `row-unknown`, `row-order`,
`row-cycle`, `config-value`, `config-anchor`, and friends (`docs/ASSEMBLY.md:117`).

## 5. Versions and the ledger anchor

`Config.Hash()` is a normalized content digest (`sha256:<64 hex>`): comments, whitespace, key order
and row order do not change it; any change to `impl`, `params`, `absent` or
`filesystem.capture.mode` does (`docs/ASSEMBLY.md:178`). Rotation advances `revision` even when the
bytes are identical, and the anchor must strictly increase (`config-anchor`); `Pin()` is a deep
copy, so a rotating file never changes an in-flight run (`docs/ASSEMBLY.md:186`).

The only disk adapter is `internal/configfile.Loader`. `keel serve --config-file` /
`KEEL_CONFIG_FILE` (default `$KEEL_HOME/config.toml`) loads at startup only: a missing file is an
empty configuration, an unreadable or invalid file fails startup, and a rotation is
parse → write `.new` → `fsync(file)` → `rename` → `fsync(dir)` (`docs/ASSEMBLY.md:203`). `Rotate`
and `Reload` are library APIs today — no production caller and no HTTP entry point
(`docs/ASSEMBLY.md:228`). The active version is visible in `GET /v1/capabilities` under
`configFile` (hash, revision, anchor, capture mode; never the local path)
(`internal/harnessapi/api.go:525`).

## 6. Capability seams

Five seams are a closed set — `fs`, `shell`, `subprocess`, `exec-surface`, `terminal` — and each
seam has a closed capability set a provider may declare (`docs/ASSEMBLY.md:261`). `Prepare` checks
the requirements against the registry before the assembly is returned; unsupported combinations are
typed `unsupported_capability` / `invalid_capability` and there is no fallback chain
(`docs/ASSEMBLY.md:291`).

## 7. What production wires today

| Function | Effect | Source |
|---|---|---|
| `ServiceRowID` | `keel.<service>#service` — the stable row id for a service | `internal/loadout/assembly.go:38` |
| `AssemblyBase` | maps `DefaultServices()` to base rows; `Requires` derived from the service graph | `docs/ASSEMBLY.md:300` |
| `AssemblyMode` | wires `serve` only; `headless`/`tui`/`sdk` return an error | `docs/ASSEMBLY.md:302` |
| `AssemblyConfig` | maps the tenant document to the `config` stage (disabled services become absent rows) | `docs/ASSEMBLY.md:303` |
| `AssembledServices` | `base → serve → config`, then reads the service set from the active rows; `Blocked` fails the loadout | `docs/ASSEMBLY.md:304` |
| `configfile.Open` / `OverlayTenant` | startup load; settings overlay onto the boot tenant document | `docs/ASSEMBLY.md:306` |

So "which services exist" is decided by the overlay, not by a second set of if/else branches —
`servicegraph` remains the only loader/activator. What is not wired: non-service `config.toml`
rows do not reach the production service graph, there is no production rotation caller, and the
other mode bundles plus real seam-provider registration are still open work (`docs/ASSEMBLY.md:315`).

## 8. Verify

```bash
go test ./internal/assembly
go test ./internal/configfile
go test ./internal/loadout
go test ./internal/servicehost
go test ./internal/architecture
go test ./cmd/keel
```

(`docs/ASSEMBLY.md:320`)
