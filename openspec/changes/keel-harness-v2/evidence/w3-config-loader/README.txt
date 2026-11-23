W3 config.toml disk loader — evidence (2026-11-23)
==================================================

> REBASE NOTE (2026-11-23, rebased onto a549e87 = W11's canonical vocabulary):
> the branch originally introduced `internal/capturemode`. W11 already ships
> `internal/capturepolicy` (value + context pin, no layerfs alias), so the
> rebase dropped the duplicate package and converted every reference to
> `capturepolicy` (`CaptureMode`, `ParseCaptureMode`, `CaptureModePerToolCall`,
> ...). The vocabulary test moved to `internal/capturepolicy/capturemode_test.go`.
> The evidence below was regenerated on the rebased tree.

Scope
-----
Closes the W3 explicit gap recorded in tasks.md and docs/ASSEMBLY.md §1: the
repository parsed config.toml as a pure model and had version/anchor semantics,
but no disk loader. This slice adds the strict startup load, the atomic
`config.toml.new` -> fsync -> rename rotation, immutable per-attempt pinning,
and the settings -> tenant-document/runtime mapping for
`filesystem.capture.mode`.

What landed
-----------
1. One parser, one vocabulary
   - internal/assembly/config.go: strict parser now accepts exactly one settings
     table, `[filesystem.capture] mode = "per-tool-call|checkpoint|manual"`.
     Unknown tables/keys, duplicate table/key, non-string/empty/padded values
     and an unknown mode are field-level errors (`config-table`, `config-key`,
     `config-duplicate`, `config-value`). The setting participates in
     Config.Hash(), so content changes move the version hash.
   - internal/capturepolicy (W11's canonical leaf package): the vocabulary
     (CaptureMode, the three values, ParseCaptureMode, typed
     CaptureModeError/CaptureTriggerError, context pin). This branch no longer
     introduces `internal/capturemode`; the rebase deleted it and the layerfs
     alias. assembly can validate the value without importing the layerfs
     service package, and the kernel -> layerfs layering violation stays gone
     (TestKernelDoesNotImportV2Layers).
   - No second parser, schema, DI container or event identity was introduced.

2. Disk loader — internal/configfile
   - Open: loads the active file once. Missing = empty optional document;
     unreadable/invalid = startup error (fail closed).
   - Rotate(data, anchor): parse+validate first, then write `config.toml.new`
     (0600, O_TRUNC), fsync the file, rename over `config.toml`, fsync the
     directory, and only then advance assembly.Rotation. A parse/write/rename
     failure leaves the active file and version untouched and removes staging;
     no half-written config.toml is observable.
   - Reload(anchor): adopts an externally rotated active file (same
     strictly-advancing anchor rule).
   - Pin(): deep copy through assembly.Rotation; an attempt that pinned before a
     rotation keeps its content and version.
   - Status(): path/existence, revision, hash, anchor seq, capture mode,
     rotation count and the last RotationFact (before/after revision+hash, time,
     bytes). The host logs it at startup and exposes it through
     GET /v1/capabilities `configFile` (path intentionally omitted).

3. Tenant/runtime mapping and precedence
   - configfile.OverlayTenant(pinned, tenant) copies the tenant document and
     applies the file setting. The input document is not mutated.
   - Fixed precedence: boot defaults < stored admin JSON document <
     config.toml setting. The admin JSON path keeps governing every key the
     file does not name; a file rotation changes the next attempt only.
   - cmd/keel: `--config-file` / `KEEL_CONFIG_FILE` (default
     $KEEL_HOME/config.toml) loads at boot, overlays the boot tenant document
     and passes the loader to the per-attempt capture resolver. The resolver
     merges the stored document then the loaded file pin, and records
     `captureSource=config.toml@revision=<n>`. checkpoint/manual still require
     their triggers, so a trigger-less mode fails closed.

Tests
-----
All targeted packages pass serially against a fresh local PostgreSQL 18
database (`keel_w3_config_test`), created and dropped by this run; no Docker, no
leftover sessions:

  KEEL_TEST_DSN=postgres://kzf@127.0.0.1:5432/keel_w3_config_test?sslmode=disable \
    go test -p 1 -count=1 ./internal/assembly ./internal/configfile \
      ./internal/capturepolicy ./internal/host ./cmd/keel ./internal/harnessapi \
      ./internal/architecture ./internal/agent ./internal/layerfs \
      ./internal/bashbridge ./internal/toolsurface ./internal/servicehost \
      ./internal/loadout

Result: test-targeted-summary.txt is the package-level result (all ok);
test-targeted.txt is the per-test result lines from the same verbose run
(package logs omitted). internal/harnessapi needs the PostgreSQL DSN: its
identity/audit tests exercise the real store and do not run meaningfully
without it.

Focused suites captured separately:
  test-assembly.txt          settings parse/hash/rejection + hand-built validation
  test-configfile.txt        startup load, missing path rejection, atomic
                             rotation, in-flight pin, invalid rejection, no
                             partial file, stale staging, capture-mode overlay,
                             restart, external reload, concurrent pin/rotate
  test-configfile-race.txt   same package under -race
  test-capturepolicy.txt     closed vocabulary, typed errors, context pin
  test-cmd-keel.txt          resolver precedence (boot < stored < config.toml)
                             and removal fallback (no stuck file value)
  test-harnessapi.txt        /v1/capabilities configFile block (and nil omission)
  gofmt.txt                  empty (all formatted)
  vet.txt                    go vet clean for touched packages
  openspec-strict.txt        Change 'keel-harness-v2' is valid

Environment note: the shared `postgres` database in this workstation already
contains legacy agents/agent_teams/api_keys rows, so harnessapi/cmd-keel PG
tests refuse to migrate there (pre-existing schema state, not this change).
The fresh isolated database removes that interference; internal/harnessapi PG
tests need `KEEL_TEST_DSN` and report package-level failures without it. One
harnessapi rate-sampling test was timing-sensitive in one non-serial run; it
passed alone and in the final serial run.

Same class of flake observed at this rebase: one `internal/agent` test
(`TestCancelAfterCommittedAnswerIsNotAborted`) panicked with "Fail in goroutine
after ... has completed" in a run that overlapped another test process on the
same machine; the package passed alone and in the final serial run above. It is
the known shared-DSN/parallel-run sensitivity, not a product-path failure.

Limitations
-----------
- The file currently carries only filesystem.capture.mode: it is the only
  tenant setting the run path resolves per attempt. quota/models/permissions
  stay on the existing env/admin-JSON paths until their file representation is
  defined; the parser rejects unknown settings instead of half-applying them.
- config.toml [[rows]] are parsed and pinned with the version, but the
  production base bundle still contains only service rows (keel.<service>#service).
  Overrides for non-service rows (for example keel.shell#bash-local) reach the
  assembly model but do not change the running service graph until W8/W9 adds
  those rows to the base.
- The ledger anchor is supplied by the caller (the host commits config.rotated
  with configHash/revision and passes its seq). This slice does not add an HTTP
  rotation trigger or a second event writer. At startup there is no rotation
  event, so the anchor is 0; revision is the in-process rotation count, while
  the content hash is stable across restarts.
- There is no production caller of Rotate/Reload yet: they are library APIs
  covered by tests. `keel serve` only calls Open at boot, and the per-attempt
  resolver applies the loaded pin. The anchors/ledger event for a production
  rotation remain W8/W9.
- The serve boot overlay (generation tenant) and the per-attempt resolver use
  different inputs on purpose: the resolver receives the PRE-overlay boot
  document, then applies the file pin itself. This is what makes a rotation
  that removes filesystem.capture.mode fall back to per-tool-call (or to the
  stored admin document) instead of retaining the removed file value. Tests
  TestEffectiveCaptureModeFallsBackWhenFileSettingIsRemoved pin the four
  fallback cases.
- Round-tripping a file setting back into the stored JSON document is not
  automatic: the file overlays at read time, it does not rewrite tenants.config.
