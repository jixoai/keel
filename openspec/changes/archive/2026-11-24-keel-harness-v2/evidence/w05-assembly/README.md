# W0.5 assembly — evidence (2026-11-23)

Slice: `keel-harness-v2` **W0.5 四层装配 + 能力 seam 清单**.
Worktree: `.worktrees/keel-w05-assembly`, branch `task/keel-w05-assembly`.
Code commit: `3c4a647` (`feat(assembly): implement four-layer row overlay and seam
capability matrix`). This file and the captured test logs are the evidence
commit that follows it.

Contract sources:

- `openspec/changes/keel-harness-v2/specs/plugin-architecture/spec.md`
  (Requirement: 装配模型 / 能力 seam 可替换)
- `docs/PLUGIN-EXTENSIBILITY.md` §6
- `openspec/changes/keel-harness-v2/design.md` §10, `tasks.md` W0.5
- ADR `openspec/changes/keel-harness-v2/adr/0003-four-layer-row-overlay.md`
- Contract: `docs/ASSEMBLY.md`

## 1. Delivered

| # | Deliverable | Files | Gate |
|---|---|---|---|
| 1 | Pure assembly model: stages/documents/rows, row-id validation, effective resolution, activation, absent semantics | `internal/assembly/{doc,layer,row,build,errors}.go` | `go test ./internal/assembly` |
| 2 | Strict `config.toml` subset, canonical hash, immutable versions, ledger anchor, run pinning | `internal/assembly/config.go` | `TestParseConfig*`, `TestConfigHash*`, `TestRotation*` |
| 3 | Seam registry, full capability matrix, typed startup rejection | `internal/assembly/seam.go` | `TestEverySeamCapabilityGapFailsBeforeStartup`, `TestCapabilityMatrix*` |
| 4 | Production adapter: default blueprint → base; tenant document → config stage; active rows decide the loadout | `internal/loadout/assembly.go`, `internal/loadout/tenant.go` | `go test ./internal/loadout ./internal/servicehost` |
| 5 | Machine/person contract + ADR + task-board update | `docs/ASSEMBLY.md`, `adr/0003-four-layer-row-overlay.md`, `tasks.md` W0.5 | review |

30 new top-level tests (assembly 25, loadout adapter 5), plus existing
`servicehost`/`architecture` suites.

## 2. Contract semantics pinned by the tests

| Contract | Test |
|---|---|
| mode overrides only the matching row; other base rows byte-identical | `TestModeOverridesOnlyTheMatchingRow` |
| preset changes behavior without rewriting base; params are whole-row replacement | `TestPresetChangesBehaviorWithoutRewritingBase` |
| absent row renders as absent (kept in effective, excluded from activation, no stale impl) | `TestAbsentRowRendersAsAbsent` |
| activation is availability-driven, effective order is not reordered | `TestActivationIsAvailabilityDrivenNotDeclarationOrder` |
| row-id collision / unknown row / overlay reordering / stage reordering rejected | `TestRowIdentityCollisionsAreRejected`, `TestUnknownRowIsRejected`, `TestReorderingIsRejected` |
| no-id base rows never participate in overlay; overlay no-id rows fail closed | `TestAnonymousBaseRowsNeverParticipateInOverlay` |
| overlay cannot change layer, requires or add rows | `TestOverlayCannotChangeStructure` |
| requirement cycles/self/unknown and absent-dependency blocking | `TestRequirementErrorsAndBlockedRows` |
| config rotation: in-flight run pins old version, next run pins new; identical content still advances revision | `TestRotationKeepsInFlightRunsOnTheirVersion` |
| anchor must be non-negative and strictly advance | `TestRotationRejectsInvalidAnchorsAndDocuments` |
| hash is canonical (whitespace/comments/key-order/row-order independent) | `TestConfigHashIsCanonical` |
| each of the five seams rejects its capability gap before startup with `unsupported_capability` | `TestEverySeamCapabilityGapFailsBeforeStartup` |
| unknown provider is also an unsupported combination; vocabulary errors are `invalid_capability` | `TestUnknownProviderIsUnsupportedCapability`, `TestMalformedCapabilityVocabularyIsInvalidCapability` |
| production adapter agrees with the existing document→service mapping on nine configurations | `TestAssembledServicesMatchesEnabledServices` |
| base rows mirror the service blueprint and activate in availability order | `TestAssemblyBaseMatchesTheBlueprint` |

## 3. Verification (run in this worktree)

Captured logs:

- `test-assembly.txt` — `go test -count=1 -v ./internal/assembly` → PASS
- `test-loadout.txt` — `go test -count=1 -v ./internal/loadout` → PASS
- `test-targeted.txt` — `./internal/architecture` PASS, `./internal/servicehost`
  PASS, `go vet ./internal/assembly ./internal/loadout` OK

```text
$ go test -count=1 ./internal/assembly
ok  	github.com/jixoai/keel/internal/assembly	0.142s

$ go test -count=1 ./internal/loadout
ok  	github.com/jixoai/keel/internal/loadout	0.187s

$ go test -count=1 ./internal/architecture
ok  	github.com/jixoai/keel/internal/architecture	6.742s

$ go test -count=1 ./internal/servicehost
ok  	github.com/jixoai/keel/internal/servicehost	5.729s

$ go vet ./internal/assembly ./internal/loadout
vet: OK
```

`go test ./...` was deliberately not run (slice rule); only the targeted
packages above.

## 4. Wired vs deferred (no fake completion)

### Wired in production

- `loadout.FromTenantConfig` now derives the selected services from
  `loadout.AssembledServices` = base → serve mode → config overlay. The active
  rows are the authority; a blocked dependency fails loadout construction.
- `loadout.AssemblyConfig` maps `host.TenantConfig` onto the config stage:
  disabled services become absent rows, and the document-derived llm version
  becomes an impl override.
- `servicegraph` remains the only loader/activator and factory owner. No second
  DI/runtime, no second tool registry, no second plugin vocabulary
  (`assembly.Layer` is the `pluginpkg.Layer` alias).

### Deferred to W8/W9 (named, not simulated)

| Deferred item | Why it is not in this slice |
|---|---|
| Reading production `config.toml` from disk | Production configuration source is still `host.TenantConfig` (deployment document/admin write); there is no `config.toml` file in the repo. `ParseConfig` is implemented and tested, but not wired as the process source. |
| `config.toml.new → fsync → rename()` file adapter | `Rotation` owns the version/anchor semantics (tested); file atomicity needs the hosting process's file lifecycle and belongs with the W8/W9 loader. |
| `headless` / `tui` / `sdk` mode bundles | Only `serve` is the servicehost assembly today. `loadout.AssemblyMode` returns an error naming W8/W9 for the other modes instead of an empty fake bundle. |
| Registering local/remote/SSH seam providers and enforcing requirements at process startup | The closed vocabulary, full matrix and `Prepare` rejection are implemented and tested. Declaring capability sets for the current real adapters (and enforcing them in `servicehost`) would be a promise W8/W9 must verify; no fake provider set is registered. |

## 5. Files

```text
internal/assembly/doc.go        package contract and import boundary
internal/assembly/layer.go      Layer alias, Stage, Mode
internal/assembly/row.go        RowID, Row, Document, EffectiveRow
internal/assembly/build.go      stack validation, overlay, activation, Blocked
internal/assembly/errors.go     stable issue codes, typed capability errors
internal/assembly/config.go     strict config.toml, canonical hash, Rotation/Pinned
internal/assembly/seam.go       five seams, closed capabilities, Registry/Matrix/Check
internal/assembly/*_test.go     25 tests
internal/loadout/assembly.go    production adapter
internal/loadout/tenant.go      FromTenantConfig uses AssembledServices
internal/loadout/assembly_test.go  5 adapter tests
docs/ASSEMBLY.md                machine/person contract
adr/0003-four-layer-row-overlay.md  decision record
```

Package boundary check: `internal/assembly` imports only stdlib +
`internal/pluginpkg`; `go test ./internal/architecture` stays green.
