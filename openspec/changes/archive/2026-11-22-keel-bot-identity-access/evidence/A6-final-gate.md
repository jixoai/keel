# A6 final gate — 2026-11-22

> Branch: `task/keel-bot-identity-access` at `7fa9bf2` (product `affed73`, E2E script `de009d3`).
> Gate: **P0=0 / P1=0 / P2=0** for the A6 acceptance lane.

## Real-service E2E

- Evidence: `.agents/reports/keel/identity-access-e2e/20261122-141307/`
- Public report: https://local-gaubee-keel-bot-identity-e2e.fde2e.com/report.html
- Result: 10/10 steps PASS, 304/304 assertions, 38 screenshots, `UNCOVERED=0` (558 files).
- Stack: fresh PostgreSQL database, real `keel serve --auth required`, real Chrome via
  playwright-core, deterministic scripted model for captain→member delegation.
- Shared session and cross-account Bot roster are driven through the real
  `/v1/{sessions,bots}/{id}/shares` API, not DB fixtures; revoke is followed by the next
  403/UI disappearance.
- Cleanup: `cleanup.json` records `portFree=yes`, `databaseDropped=yes`,
  `legacyDatabaseDropped=yes`, `serverExited=yes`; no `keel_e2e_identity%` database remains.

## Independent review

- Reviewer verdict: **PASS**, P0=0 / P1=0.
- P1-1 (E2E share fixtures / false gaps) closed by `de009d3`.
- P1-2 (HTTP subagent `bot.invoke` target union / ownership-binding precedence) closed by
  `affed73`; tests `TestSpawnAuthorizesEveryNamedTarget`,
  `TestSpawnedBotIDFollowsChildSelection`.
- P2-1 (revoke after subject deletion) closed in `affed73`.
- Remaining non-blocking P2s: share-list truncation at 200 and the spawn ownership
  two-transaction window.

## Mechanical gates

- `go test -p 1 -count=1 ./...` on a fresh `KEEL_TEST_DSN` database: all packages pass.
- `go build ./...`, `go vet ./internal/harnessapi ./internal/harnessext`: clean.
- `nsh check`: clean; `nsh test --target tui .` on the integrated branch: 281/281 tests pass.
- `openspec validate keel-bot-identity-access --strict`: valid.
- Route matrix: `176 declarations, 176 registered routes`.
- 375/390/768/1024/1440: no horizontal overflow; 390 shell targets are ≥44×44.
- Review instance: `https://local-gaubee-keel-bot-identity-app.fde2e.com` (fresh build from
  this branch; admin password rotated for review access).

## UI foundation (Owner-reported)

- Dialog/AlertDialog are centered and scrollable; the account-create form's Input is
  visible, styled and typeable at 1440 and 390.
- Top band carries only the brand/plugin slot; chapters live in the desktop rail and the
  phone's five bottom tabs; account/live/todo/settings/logout live in settings.
- Solid-glass materials synced across the kit; 44 components in the gallery.

## Merge note

The branch is not yet merged into the `keel` main submodule; the parent `fde2e` pointer
must be updated only after that merge and when the main worktree's other session WIP is
clear.
