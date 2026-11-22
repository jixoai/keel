# W0.5 Contract Freeze — Evidence (2026-11-22)

> Slice: `keel-harness-v2` **W0.5 contract freeze** plus the W0 items that unblock
> W9/W12. Worktree: `.worktrees/keel-w05-contract`, branch `task/keel-w05-contract`.
> This file is the slice's evidence **and backlog**: every item deliberately not
> implemented is listed in §6 with a reason, so a later wave does not mistake a
> frozen vocabulary for a shipped feature.
>
> Contract documents: `docs/PLUGIN-EXTENSIBILITY.md` (slots/forms/layers/assembly),
> `docs/PLUGIN-CATALOG.md` (catalog schema + reconciliation). Schemas:
> `plugins/keel.plugin-market/catalog.schema.json`,
> `docs/schemas/extensibility.schema.json`.
>
> Commits: `5c8097a` (manifest/extensibility contract), `259f25e` (catalog
> single source), `026d7d0` (layer boundary tests), `d105fb5` (R1 re-pin +
> this evidence).

## 1. Delivered

| # | Deliverable | Files | Gate |
|---|---|---|---|
| 1 | Catalog single source: `wave` + `dshGroups` + reconciled ids/status, generated/checked COVERAGE | `plugins/keel.plugin-market/catalog.json` (+ `catalog.schema.json`), `plugins/COVERAGE.md`, `scripts/plugin-catalog.mjs`, `.github/workflows/ci.yml`, `docs/PLUGIN-CATALOG.md` | `node scripts/plugin-catalog.mjs --check` |
| 2 | Manifest extension: five layers, `provides.slots`, `contributes`, `provides.backends`, `extensibility` | `internal/pluginpkg/manifest.go`, `package.go`, `extensibility.go`; tests `extensibility_test.go`, `official_plugins_test.go` | `go test ./internal/pluginpkg` |
| 3 | Frozen extension declaration format + official docs | `docs/PLUGIN-EXTENSIBILITY.md`, `docs/schemas/extensibility.schema.json`, `plugins/keel.web-search|context-memory|plugin-market/{EXTENSIBILITY.md,extensibility.json}`, manifests | `verifypkg` + `official_plugins_test.go` |
| 4 | Layer boundaries + four-layer assembly/row-id overlay documented | `internal/architecture/v2_layers_test.go`; `docs/PLUGIN-EXTENSIBILITY.md` §6–§7 | `go test ./internal/architecture` |
| 5 | R1 re-pin: v2-kind scenario/probes, 77-reason allowlist, baseline pin | `cmd/r1diff/{main.go,bookkeeping.go,scenario.go,pin.go,baseline.json,allow-v2.json}`, `scripts/build-r1-baseline.sh`, `scripts/acceptance-all.sh` | R1 live + negative control (§4) |
| 6 | Explicit deferrals/backlog | this file §6 | review |

## 2. Contract decisions (frozen)

1. **Catalog field location.** `keelPluginId`/`status` stay under `coverage` (the
   existing sidecar contract); `wave` and `dshGroups` are entry-level fields.
   The check enforces all four on every entry; `docs/PLUGIN-CATALOG.md` names the
   canonical paths. This avoids a second copy and keeps `keel.plugin-market`
   sidecar code unchanged.
2. **Status enum is extended with `merged`.** The v2 decisions need an explicit
   disposition for `goal/todo/plan → keel.tasks`, `guard → keel.supervisor`,
   `workspace → Group`, `team absent`, `skill→skills`, `preset→presets`,
   `tools→tool-executor`. Merged/renamed ids live in `catalog.retiredIds` with a
   reason; `COVERAGE.md` rows resolve through them.
3. **Wave enum**: `W0.5 | W1 | W2 | W3 | never`. `never` is only for explicit
   `absent` rows.
4. **Layer derivation**: `webview→ui`, `sidecar→provider`, `in-process→harness`;
   explicit `layer` overrides. Carry constraints: kernel→in-process,
   service→sidecar, provider→sidecar/webview, ui→webview.
5. **Slot ids are namespaced** (`<plugin>.<name>`); the first-party set is
   `keel.ui.route|nav|action|tool-renderer|message-part|sidebar|settings-section|surface`
   plus `keel.tasks.widget`. A bare `route` is refused with a field error.
6. **Form rules**: `chunk|package|iframe|data`; `data` requires a slot schema and
   a non-empty payload; `chunk/package/iframe` require a package-relative entry;
   `iframe` also requires `provides.ui.entry`; `package/iframe` are explicitly
   unavailable on `tui`, so a TUI-only slot accepting only those forms is
   refused.
7. **Official gate**: `extensibility.doc` + `extensibility.schema` are mandatory
   for official packages (`Environment.RequireExtensibility`); the document is
   parsed with unknown-field rejection, validated, and must agree with the
   manifest's plugin id/version/slots/contributes. Third parties may omit it.
8. **Layer boundary reality check.** `internal/provider` is the in-process LLM
   adapter used by the kernel loop, so it is a kernel seam, not the plugin
   provider layer; the Go-side provider boundary is `internal/gateway`.
   `layerfs` is Phase A in-process CAS, so the harness imports it; the gRPC-only
   service boundary is deferred (§6). The architecture tests assert the
   enforceable direction today, not a fictional future graph.

## 3. Verification (run in this worktree)

```text
$ go test -count=1 ./internal/pluginpkg ./internal/architecture ./cmd/r1diff ./internal/r1diff
ok  	github.com/jixoai/keel/internal/pluginpkg	0.223s
ok  	github.com/jixoai/keel/internal/architecture	4.194s
ok  	github.com/jixoai/keel/cmd/r1diff	0.102s
ok  	github.com/jixoai/keel/internal/r1diff	0.077s

$ go vet ./internal/pluginpkg ./internal/architecture ./cmd/r1diff ./internal/r1diff ./plugins/tools/verifypkg
vet: OK

$ node scripts/plugin-catalog.mjs --check
plugin-catalog: OK (102 entries, §A/§B/§C mapped, §H current)

$ go run ./plugins/tools/verifypkg
3 package(s) checked, 0 failure(s)
```

Catalog composition after the freeze: 53 DSH official groups + 33 community +
16 keel official (3 delivered + 13 planned) = **102 entries**; status counts
`delivered=4, partial=15, planned=59, registered=16, merged=5, absent=3`.
The market sidecar smoke (`search status=["delivered"]`) returns the same four
delivered entries and reports `catalog 2026-11-22, 102 entries`.

Targeted tests cover: valid UI contribution; rejected bare/foreign slot ids,
unknown forms/targets/arity, data-without-schema, TUI-only iframe, unknown
contribution slot, form not allowed by slot, data/chunk payload shape, duplicate
contributions, layer/kind constraints, unknown backend action, official
extensibility gate, document/manifest drift, every v2 kind classified in R1, and
a pinned load/negative path for the baseline pin.

## 4. R1 re-pin and differential

- Pin: `cmd/r1diff/baseline.json` — `ref e7361f8` (last commit before
  keel-service-graph, therefore the pre-v2 runtime), scenario `r1-basic-v2`,
  allowlist `cmd/r1diff/allow-v2.json`, **77 rules**, `repinnedAt 2026-11-22`.
- `scripts/build-r1-baseline.sh` now reads `ref` from the pin (no duplicated
  hardcoded commit); `scripts/acceptance-all.sh` builds from the pin, runs the
  capture with `-pin`/`-allow`, and runs the negative control with the same
  files.
- Scenario additions: one negative write probe per v2 family
  (`task.created`, `fs.write`, `config.rotated`, `placement.joined`,
  `supervision.escalated`, `guardrail.tripped`, `resume.attempt`,
  `notification.ack`) through the user edge, plus run-snapshot fields
  (`configHashBefore/After`, `anchorEventSeq`, `toolManifestHash`) in the
  bookkeeping section.

Live capture (real PostgreSQL, baseline built from the pin, candidate from the
working tree; ports 13764/13765 because unrelated report servers hold
13778/13779 on this machine):

```text
R1 pin: ref e7361f8, scenario r1-basic-v2, allowlist cmd/r1diff/allow-v2.json (77 rules)
R1 differential: 152 leaf paths compared, 103 difference(s), 103 allowed, 0 unexpected
  allow rules: 77/77 matched
verdict: PASS (no difference outside the allowlist; every allow rule fired)
```

Negative control (same dump both sides, same pin/allowlist):

```text
77 stale allow rules
verdict: FAIL (stale allow rules: registered differences did not happen)
```

What the v2 probes proved, and why each delta has a reason:

- The pre-v2 baseline **stored** every probe (`201` + a seq) because it did not
  know the kind; the candidate **knows** the kind and refuses the user edge
  (`400`, `kind "..." is not accepted from clients; the edge whitelist is
  user.message`). This is the intended lane split: v2 kinds are committed by
  their owning lanes (task/fs/notify/…), not by clients.
- The baseline probe session log contains nine events (the old
  `assistant.message` probe + eight v2 probes); the candidate log is empty. One
  prefix rule per probe index keeps the rest of the log index-stable.
- The v1 `echo` tool was deleted by W2.1's constant 11-tool surface. The
  scenario's fake provider still calls it, so the candidate returns a structured
  `Tool echo not found` result (pinned as the new behavior). This closes a
  pre-existing R1 red path: the frozen scenario was still assuming `echo`.
- The eight new run-snapshot fields are pinned to their real values, so a wrong
  config or tool-manifest hash fails the gate instead of silently passing.

The bookkeeping classifier now covers **all 17 v2 kinds** with a per-kind reason
(`cmd/r1diff/bookkeeping.go`), not just `run.*`. Kinds without emitters
(`config.rotated`, `placement.*`, `supervision.*`, `guardrail.*`) are frozen in
the vocabulary and classified; when their emitters land, the differential stays
index-stable without new seq-shift rules.

## 5. Catalog reconciliation

- `dsh.goal|todo|plan` → `merged` into `keel.tasks`; `dsh.guard` → `merged` into
  `keel.supervisor`; `dsh.workspace` → `merged` into Group (`mergedInto`), not a
  plugin; `dsh.team` → `absent` (no team-coordination plugin).
- `dsh.skill|preset` → `keel.skills|keel.presets` (renamed, capability
  unchanged); `keel.tools` retired into `keel.tool-executor`.
- Catalog-only ids from the design inventory are kept and given a per-id
  disposition in `COVERAGE.md` §D.2; COVERAGE-only ids gained catalog entries:
  `keel.boot|cli|context|plugin-manager|shell.pwsh|skill-store|skill.distill|
  subagent.claude-code|web|tool-executor|tasks|supervisor|skills|presets|ui`
  and community rows for `dsh-market/dsh-market`, `anbeime/skill`,
  `alchaincyf/nuwa-skill`.
- `keel.creation` is `partial` (v1 scaffolding delivered; v2 extensibility
  aggregation/Native chunk path not); `dsh.experimental` is `absent` as a single
  plugin and explicitly decomposed into W3 capability slices.

## 6. Explicit deferrals / backlog (no half-done code)

| Item | Why deferred | What is guaranteed now | Unblocks at |
|---|---|---|---|
| macOS native target (`nsh package --target macos`) | NSH multi-platform build/signing is a client-toolchain slice; no code in this backend slice | Contract says native target is a first-class target and chunk renders there; no fake artifact | W12 / client-shell |
| NSH 0.5 upgrade (Window API, media `fs.read` grant, `focus-visible`, `activationPolicy`) | web/ client is outside this slice's ownership; upgrade is a coordinated client change | Slot/form contract names the 0.5 capabilities; no half-upgraded runtime | W12 |
| Desktop shell: tray, global shortcuts, updater/OTA | Requires signed desktop packaging and release pipeline | `package`/`iframe`/`chunk` semantics and TUI degradation are frozen; no tray code invented | W12 / release |
| RSS sampler (`scripts/acceptance-rss.sh`, VmRSS sum ≤500MB) | Needs the group-process model and a measurement harness; cgroup peak gate already exists | Existing memory gates untouched; no fake RSS numbers | W4 (G4) |
| Multi-tenant plugin state (`plugin_state(tenant_id, ...)`, `/v1/plugins` filtering) | Store/schema and serve ownership are outside W0.5; alpha zero-compat applies | Manifest/catalog contract is tenant-neutral; no deployment-scoped tenant claim added | W0-8 / W9 |
| layerfs Rust + gRPC (proto, health, ports, SIGKILL sweep) | ADR 0001 Phase A Go CAS is the accepted interim; Rust service is its own wave | Architecture tests record the Phase A exception; service-layer direction still asserted | W1 gRPC |
| Emitters for `config.rotated`, `placement.joined/left`, `supervision.escalated`, `guardrail.tripped` | Owning lanes (config/placement/supervision) have not landed their write paths | Vocabulary, key namespaces, R1 classification and negative probes are frozen; no no-op emitter was added to fake coverage | W2/W6 |
| Four-layer assembler (base/mode/preset/config.toml row-id overlay) | Loadout/servicegraph exists; the row-id overlay assembler is W8/W9 work | Overlay semantics (row id, last-write-wins, rotation anchor) are documented and testable by contract | W8/W9 |
| `provides.backends` closed-action check against the live tool surface | Needs the aggregate-tool registry at assembly time | Manifest parses/validates syntax; `Environment.KnownBackend` is the seam the assembler will use | W2.1/W9 |

These items are **not** implemented as stubs, TODOs with behavior, or disabled
code paths; the only shipped code is the contract/validator/catalog/R1 work
above.

## 7. Residual risks and notes

- The three added community rows use the 2026-11-06 star snapshots recorded in
  `COVERAGE.md` §C; no live GitHub query was made in this slice. The rows are
  marked `registered`, not `delivered`.
- `COVERAGE.md` §A/§B/§C remain human tables; the check maps them to the catalog
  and §H is generated. A prose-only change (e.g. a status word) can still be
  edited by hand; the check fails if it contradicts the catalog's bold/delivered
  rule or loses its mapping.
- The local machine had unrelated Python report servers on 13778/13779 and
  13760, so the evidence capture used 13764/13765. `acceptance-all.sh` keeps the
  registered 13778/13779 and its `port_free` interlock; no unrelated process was
  killed.
- The full `scripts/acceptance-all.sh` battery (race suite, conformance, chaos,
  1000-session memory, handoff) was **not** run in this slice; the R1 segment was
  run standalone against real PostgreSQL, including the negative control. The
  remaining gates are unchanged by this slice and stay owned by the acceptance
  run.
