Files: 107; covered: 107; UNCOVERED: 0; excluded: 65.

| File | Coverage evidence | Status |
|---|---|---|
| `.github/workflows/ci.yml` | CI parity: `go test -p 1 ./...` + `nsh check` | COVERED |
| `README.md` | docs pin scan + `openspec validate --strict` | COVERED |
| `cmd/board-sync-e2e/main.go` | `go test -p 1 ./...` (compile) + sync route tests | COVERED |
| `cmd/board-sync-e2e/superagent.go` | `go test -p 1 ./...` (compile) + sync route tests | COVERED |
| `cmd/keel/board.go` | `go test -p 1 ./...` (cmd/keel mount/serve) | COVERED |
| `cmd/keel/board_serve_options_test.go` | `go test -p 1 ./...` (cmd/keel mount options) | COVERED |
| `cmd/keel/board_superagent.go` | `go test` cmd/keel gate + board 404 TUI | COVERED |
| `cmd/keel/board_superagent_test.go` | `go test` cmd/keel gate + board 404 TUI | COVERED |
| `cmd/keel/serve.go` | `go test -p 1 ./...` (cmd/keel mount/serve) | COVERED |
| `docs/HARNESS.md` | docs pin scan + `go test` harnessapi 404/report/ask | COVERED |
| `docs/RUNBOOK.md` | docs pin scan + `openspec validate --strict` | COVERED |
| `internal/access/builtin.go` | `go test` access/builtin + permission screenshots | COVERED |
| `internal/access/builtin_test.go` | `go test` access/builtin + permission screenshots | COVERED |
| `internal/accessgate/matrix.go` | `go test` accessgate (report/ask only) + 404 TUI | COVERED |
| `internal/boardintegration/augment.go` | `go test` boardintegration moves/me + SSE augment live | COVERED |
| `internal/boardintegration/authorizer.go` | `go test` boardintegration authorization | COVERED |
| `internal/boardintegration/channel_owners.go` | `go test` boardintegration channel-owner tests | COVERED |
| `internal/boardintegration/flags_pg_test.go` | `go test` boardintegration flag matrix PG | COVERED |
| `internal/boardintegration/me.go` | `go test` TestBoardMe + identity badge | COVERED |
| `internal/boardintegration/moves_pg_test.go` | `go test` TestTaskMoves* / TestBoardMe / sync merge | COVERED |
| `internal/boardintegration/pg_helpers_test.go` | `go test` boardintegration PG helpers | COVERED |
| `internal/boardintegration/surface.go` | `go test` boardintegration HTTP/RBAC surface | COVERED |
| `internal/boardintegration/sync_admin.go` | `go test` boardintegration sync/admin/runtime PG | COVERED |
| `internal/boardintegration/sync_admin_pg_test.go` | `go test` boardintegration sync/admin/runtime PG | COVERED |
| `internal/boardintegration/sync_config.go` | `go test` boardintegration sync/admin/runtime PG | COVERED |
| `internal/boardintegration/sync_manager.go` | `go test` boardintegration sync/admin/runtime PG | COVERED |
| `internal/boardintegration/sync_routes.go` | `go test` boardintegration sync/admin/runtime PG | COVERED |
| `internal/boardintegration/sync_routes_pg_test.go` | `go test` boardintegration sync/admin/runtime PG | COVERED |
| `internal/boardintegration/sync_runtime_pg_test.go` | `go test` boardintegration sync/admin/runtime PG | COVERED |
| `internal/boardintegration/sync_workers.go` | `go test` boardintegration sync/admin/runtime PG | COVERED |
| `internal/harnessapi/superagent.go` | `go test` harnessapi 404 + worker report/ask | COVERED |
| `internal/harnessapi/superagent_test.go` | `go test` harnessapi 404 + worker report/ask | COVERED |
| `internal/store/relations.go` | `go test` store relations/orphan PG | COVERED |
| `internal/store/relations_orphan_pg_test.go` | `go test` store relations/orphan PG | COVERED |
| `internal/superagentboard/board.go` | `go test` superagentboard bridge/commands/token | COVERED |
| `internal/superagentboard/bridge.go` | `go test` superagentboard bridge/commands/token | COVERED |
| `internal/superagentboard/bridge_test.go` | `go test` superagentboard bridge/commands/token | COVERED |
| `internal/superagentboard/commands.go` | `go test` superagentboard bridge/commands/token | COVERED |
| `internal/superagentboard/mirror.go` | `go test` mirror imports archived + screenshot 24 | COVERED |
| `internal/superagentboard/mirror_pg_test.go` | `go test` mirror imports archived + screenshot 24 | COVERED |
| `internal/superagentboard/mirror_test.go` | `go test` mirror imports archived + screenshot 24 | COVERED |
| `internal/superagentboard/token.go` | `go test` superagentboard bridge/commands/token | COVERED |
| `openspec/changes/keel-board-v1/DISPOSITION.md` | documented Phase 2 limitations + `openspec validate --strict` | COVERED |
| `openspec/changes/keel-board-v1/design.md` | `openspec validate --strict` | COVERED |
| `openspec/changes/keel-board-v1/tasks.md` | `openspec validate --strict` | COVERED |
| `scripts/archive/retired-superagent/README.md` | retired-surface check + `openspec validate` | COVERED |
| `scripts/archive/retired-superagent/acceptance/main.go` | `go test -p 1 ./...` (archive package compile) + retired-surface check | COVERED |
| `scripts/archive/retired-superagent/acceptance/page.html` | retired-surface check + board gone screenshot 36 | COVERED |
| `scripts/archive/retired-superagent/acceptance/product.mjs` | `node --check`/`bash -n` + retired-surface check | COVERED |
| `scripts/archive/retired-superagent/acceptance/report.mjs` | `node --check`/`bash -n` + retired-surface check | COVERED |
| `scripts/archive/retired-superagent/acceptance/serve.sh` | `node --check`/`bash -n` + retired-surface check | COVERED |
| `scripts/archive/retired-superagent/acceptance/tui.mjs` | `node --check`/`bash -n` + retired-surface check | COVERED |
| `scripts/archive/retired-superagent/live-github/README.md` | retired-surface check + `openspec validate` | COVERED |
| `scripts/archive/retired-superagent/live-github/bootstrap.mjs` | `node --check`/`bash -n` + retired-surface check | COVERED |
| `scripts/archive/retired-superagent/live-github/e2e.mjs` | `node --check`/`bash -n` + retired-surface check | COVERED |
| `scripts/archive/retired-superagent/live-github/provider.mjs` | `node --check`/`bash -n` + retired-surface check | COVERED |
| `scripts/archive/retired-superagent/live-github/run.sh` | `node --check`/`bash -n` + retired-surface check | COVERED |
| `scripts/archive/retired-superagent/live-github/set-worker-tools.mjs` | `node --check`/`bash -n` + retired-surface check | COVERED |
| `scripts/binary-size.sh` | `bash -n` + phase-1 evidence | COVERED |
| `scripts/board-fake-sse-server.mjs` | SSE live fake-server test | COVERED |
| `scripts/board-phase2-gates.sh` | this gate (`bash -n` + executed in this run) | COVERED |
| `scripts/board-phase2-ui-evidence.mjs` | UI evidence (HTML / 53 shots / 26 matrix) | COVERED |
| `scripts/keel-board-phase1-gate.sh` | `bash -n` + phase-1 evidence | COVERED |
| `scripts/keel-board-phase1-pin.sh` | `bash -n` + phase-1 evidence | COVERED |
| `scripts/keel-board-phase1-superagent-gate.sh` | `bash -n` + phase-1 evidence | COVERED |
| `scripts/keel-board-phase1-sync-gate.sh` | `bash -n` + phase-1 evidence | COVERED |
| `scripts/keel-creation-e2e/e2e.mjs` | `node --check` | COVERED |
| `web/App.tsx` | board TUI + screenshots + full TUI | COVERED |
| `web/README.md` | docs pin scan + `openspec validate --strict` | COVERED |
| `web/evidence/board-capture.template.tsx` | UI screenshots (53) | COVERED |
| `web/evidence/board-sse-live.template.tsx` | SSE live fake-server test | COVERED |
| `web/src/data/board-sse.test.ts` | SSE tests (3) | COVERED |
| `web/src/data/board-wire.ts` | board data tests + SSE tests + board TUI | COVERED |
| `web/src/data/board.test.ts` | board data tests (22) | COVERED |
| `web/src/data/board.ts` | board data tests + SSE tests + board TUI | COVERED |
| `web/src/data/contract.ts` | `nsh check` + board data tests (old types removed) | COVERED |
| `web/src/data/fixture-board.ts` | board data tests + board TUI + screenshots | COVERED |
| `web/src/data/fixture-identity.ts` | board TUI permission/identity + screenshots | COVERED |
| `web/src/data/fixture.ts` | board TUI permission/identity + screenshots | COVERED |
| `web/src/data/http.ts` | board data tests + SSE tests (real `KeelHttp`) | COVERED |
| `web/src/data/identity-stream.test.ts` | full TUI (identity/session suite) | COVERED |
| `web/src/data/identity-stream.ts` | full TUI (identity/session suite) | COVERED |
| `web/src/data/permission-catalog.ts` | board data tests + permission screenshots | COVERED |
| `web/src/data/route.test.ts` | route tests: `/superagent*` -> gone | COVERED |
| `web/src/data/route.ts` | route tests: `/superagent*` -> gone + board TUI | COVERED |
| `web/src/data/store.ts` | route tests + board TUI | COVERED |
| `web/src/data/superagent.test.ts` | deleted old suite; route tests + full TUI | COVERED |
| `web/src/data/superagent.ts` | deleted old client; route tests + board TUI gone + screenshot 36 | COVERED |
| `web/src/screens/board/automation.tsx` | board TUI automation + screenshots 34-37 | COVERED |
| `web/src/screens/board/board.tui.test.tsx` | board TUI (12) | COVERED |
| `web/src/screens/board/harness.tsx` | UI screenshots (53, all scenarios) | COVERED |
| `web/src/screens/board/page.tsx` | board TUI + five-state screenshots | COVERED |
| `web/src/screens/board/queue.tsx` | board TUI queue + screenshots 01-14 | COVERED |
| `web/src/screens/board/state.ts` | board TUI actions/dialogs | COVERED |
| `web/src/screens/board/sync.tsx` | board TUI sync + screenshots 30-33 | COVERED |
| `web/src/screens/board/task.tsx` | board TUI task/lease/actions + screenshots 20-28 | COVERED |
| `web/src/screens/books/books.tui.test.tsx` | books TUI + board TUI + full TUI | COVERED |
| `web/src/screens/books/chapters.tsx` | books TUI + board TUI + full TUI | COVERED |
| `web/src/screens/books/management.ts` | books TUI + board TUI + full TUI | COVERED |
| `web/src/screens/books/shell.tsx` | books TUI + board TUI + full TUI | COVERED |
| `web/src/screens/books/superagent.harness.tsx` | deleted old board; route tests + screenshot 36 + full TUI | COVERED |
| `web/src/screens/books/superagent.tsx` | deleted old board; route tests + screenshot 36 + full TUI | COVERED |
| `web/src/screens/books/superagent.tui.test.tsx` | deleted old board; route tests + screenshot 36 + full TUI | COVERED |
| `web/src/screens/command-palette.tsx` | board TUI + full TUI | COVERED |
| `web/src/screens/commands-local-error.test.tsx` | full TUI (538) | COVERED |
| `web/src/screens/settings-models.test.tsx` | full TUI (538) | COVERED |
| `web/theme.json` | UI screenshots (theme tokens) + `nsh check` | COVERED |
