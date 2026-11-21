# W7 skills — evidence (2026-11-21)

Lane: `keel-harness-v2` W7 skills. Worktree `keel-w7-skills`, base `d43d0eb`.
Scope: a tenant-scoped skill library whose versions are immutable and content
addressed, the settings/CLI/tool faces over it, the composer's `$` completion
and `/learn`, and the rule that skill scripts never execute inside the library.

## What was delivered

1. **Bundle identity** — `internal/skills/bundle.go`
   - A skill is `SKILL.md` + resources. `LoadBundle`/`BundleFromFiles` hash every
     regular file and derive one identity from a canonical manifest
     (`keel-skill-bundle-v1`, sorted paths, sizes, modes).
   - Equal content has one hash (deduplication); a version directory is
     write-once. `Verify` reports a tampered version as `ErrBundleMismatch`.
   - Bounds: 256 files, 8 MiB per bundle, 2 MiB per file; symlinks are refused.
2. **Registry** — `internal/skilllib`
   - `<root>/<tenant>/.versions/<hash>/` immutable content, `active/<name>/`
     materialised for discovery, `.registry.json` state (atomic replace).
   - Sources carry `trusted`/`untrusted`; a locally authored skill is `local`.
     An untrusted source needs `allowUntrusted` for install **and** update.
   - `pin` activates and holds a version; an update that would move a pinned
     skill is a typed `pinned` conflict. `modified`, `source_missing`,
     `name_mismatch`, `missing_active` are the other visible conflicts.
   - A source still used by a skill cannot be unsubscribed (`source_in_use`).
3. **Surfaces over one library**
   - `GET/POST /v1/settings/skills*` (`internal/agentapi/skills.go`): list
     (`check` reads sources), show (body + manifest + versions), install,
     update, pin/unpin, remove, sources list/subscribe/unsubscribe.
   - `skills.manage` / `skills.sources` route to the same library
     (`internal/toolsurface/skills.go`, `internal/skilllib/backend.go`); the
     closed `manage` set has no execution member.
   - `keel skills list|show|install|update|pin|unpin|remove|sources ...` is a
     thin HTTP client (`cmd/keel/skills.go`), never a second DB path.
   - Settings `/settings/skills` (`web/src/screens/settings-skills.tsx`):
     trust/pin/update/conflict badges, detail dialog (body, resources,
     immutable versions), create, install from a subscribed source, sources
     with in-use refusal, five states.
   - `Host.RefreshSkills` rediscovery makes an install visible to the next run
     without a restart; `active/` is one of the discovery roots.
4. **`$` completion** — `web/src/screens/composer-completion.tsx`
   - One wake character, three groups (技能/文件/任务), fixed order, one shared
     query. The sources are the library (fast read), `GET
     /v1/sessions/{id}/files`, `GET /v1/sessions/{id}/tasks`.
   - Choosing a row replaces the active token and writes `$<label> `; it never
     sends. A bare number (`$5.00`) is not a completion.
5. **`/learn`** — composer sends one message asking the agent to call
   `skills.manage create`; the tool result is the only success signal. The
   notice says "已发送沉淀请求", never "已学习".
6. **Scripts** — `internal/skills` and `internal/skilllib` import no `os/exec`
   and no `bashbridge`; `TestSkillPackagesDoNotExecuteScripts` pins that.
   Executing a skill's script is the bash tool's job (sandbox, job manager,
   timeout-to-background, bounded output).

## Gates

| Gate | Command | Result | Artifact |
|---|---|---|---|
| build | `go build ./...` | pass | `go-build-vet.txt` |
| vet | `go vet ./...` | pass | `go-build-vet.txt` |
| full (DSN) | `KEEL_TEST_DSN=… go test -p 1 ./... -count=1` | 60 packages pass | `go-test-full.txt` |
| conformance | `scripts/conformance.sh` (DSN) | 1113 pass / 0 fail / 1 skip | `conformance.txt` |
| openspec | `openspec validate keel-harness-v2 --strict` | valid | `openspec-validate.txt` |
| web check | `nsh check` | pass | `web-check.txt` |
| web build | `nsh build` | pass | `web-build.txt` |
| TUI | `nsh test --target tui` | 246 pass / 0 fail | `tui-tests.txt` |
| script policy | `go test ./internal/architecture -run TestSkillPackagesDoNotExecuteScripts -v` | pass | `architecture.txt` |
| live CLI | real `keel serve` + `keel skills` | pass | `cli-live.txt`, `cli-source-conflicts.txt` |
| settings wire | real server, curl | pass | `settings-http.txt` |

Notes on the full run:

- `.env.local` exports `KEEL_SERVE_PASSWORD`; the CLI unit tests intentionally
  do **not** stub `POST /v1/auth/login`, so a suite run that inherits that
  variable fails every CLI test (pre-existing tests included). The recorded run
  unsets the serve variables and exports only `KEEL_TEST_DSN`.
- `internal/store TestPlacementSchemaMigratesWithRLS` deadlocked once in a
  parallel full run (shared test DB); rerunning the package alone and the full
  suite with `-p 1` passes. Not a skills change (no schema was added).
- The app TUI file passed in isolation and in the final full TUI run; one
  earlier parallel run hit its 30s timeout under load.

## P1 fix — subscription sources are confined to the managed root

A-axis finding: the model could call `skills sources subscribe` with any host
directory (for example `/etc`) and then install from it, reading a whole tree.

- `internal/skilllib/library.go:resolveSourceDir` resolves a subscription inside
  the managed source root (default `<skills root>/sources`, configurable with
  `KEEL_SKILLS_SOURCE_ROOT` / `--skills-source-root`), symlink-resolves both the
  root and the candidate, and refuses a path whose real location is outside the
  root. `/etc`, `..` and a symlink pointing outside are typed
  `source_outside_root`; a legitimate `team` directory is accepted.
- The model tool face (`skills sources subscribe`, `skills manage create`) forces
  `untrusted` and ignores `trust`/`allowUntrusted`; `skills manage create` and
  every edit also set `ForceUntrusted`, so naming a trusted source cannot
  inherit its label. The `skills` tool schema no longer exposes either argument.
  Trusted subscriptions / `allowUntrusted` remain operator inputs on the
  authenticated HTTP/CLI face (documented in `internal/agentapi/skills.go` and
  `keel skills --help`).
- P2: wire responses carry logical paths only — `Record.Path` is
  `active/<name>`, sources are `team` not an absolute directory, `Snapshot.Root`
  is not serialized and `GET /v1/settings/skills` omits `root`. Host paths in
  internal failures are logged, not returned (`writeSkillError` fallback and
  the library's sanitized messages).
- Regression: `source-confinement.txt` (managed-root accept, `/etc`/`..`/
  symlink reject, model trust ignored, schema has no trust arguments) and
  `w7fix-live.txt` (real server: `/etc` and `..` → 400, managed `team` install
  succeeds, raw settings response contains no `/tmp`).

## Live smoke

The server ran with a fresh tenant against the test DSN, a temporary skills root
and `--no-worker`; the binary was the same `cmd/keel`. The transcript shows:

- empty list → install → list/show → a second install making a second immutable
  version → pin/unpin → remove;
- subscribe a trusted local source → install from it → move the source → list
  shows `UPDATE` → pin → update refused `409 pinned` → unpin → update applies →
  `source_in_use` refusal on unsubscribe → remove then unsubscribe;
- the on-disk tree and `.registry.json` in `cli-live.txt` show the
  content-addressed `.versions/<hash>` directories and the recorded
  current/pinned hashes.

## Known limits (honest, not hidden)

- Only `local` sources are syncable; `git`/`registry` kinds are refused with
  `unsupported_source` (accepting a source this build cannot sync would make
  "check for updates" a lie).
- The `$` menu is pointer/tap selected; the NSH `Input` exposes no key event to
  bind arrow-key navigation (the rows are real buttons, so Tab reaches them).
- The registry is single-writer per process (the deployment's default
  single-process model). Two `keel serve` processes sharing one root are out of
  scope, like the plugin manager's root.
- The prompt index is refreshed through `Host.RefreshSkills`; an in-flight run
  keeps the registry it resolved, by design.
- No database schema was added: the library is a tenant-scoped directory tree,
  exactly like plugin packages; settings/CLI still go through the HTTP API.
