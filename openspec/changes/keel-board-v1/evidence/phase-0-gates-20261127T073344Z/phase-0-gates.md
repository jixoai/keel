# keel-board Phase 0 evidence

Generated at 2026-11-27T08:07:04Z by `scripts/board-phase0-evidence.sh`.

## Immutable revision

| Field | Value |
| --- | --- |
| Repository | `github.com/jixoai/keel-board` |
| Branch | `main` |
| Commit | `d5a9e0a6d983685994b5df49c266350d80ab4dbd` |
| Tag | `v0.1.0` |
| Worktree | clean |
| Go | `go version go1.27.4 darwin/arm64` |
| PostgreSQL client | `psql (PostgreSQL) 18.7 (Homebrew)` |
| License files | present |

## Gates

| Gate | Exit | Seconds |
| --- | ---: | ---: |
| `gofmt` | 0 | 0 |
| `vet` | 0 | 0 |
| `test` | 0 | 59 |
| `race` | 0 | 76 |
| `e2e` | 0 | 24 |
| `stress` | 0 | 816 |

Full output: `commands.log`. E2E report: `e2e/report.html`.

## Independent review

Findings are recorded verbatim in `phase-0-independent-review.md`; the fix list and re-review live beside it.

## Result

PASS: every gate exited 0, the tree is clean, the commit is tagged, and the license files exist.
