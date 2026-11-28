# P2 closeout — board lane review

Board review: PASS, P0/P1=0. Six P2s plus the deferred `depth.dead` are fixed in
this worktree. Tested keel commit: `d8d74497`. Board `v0.1.2`/`v0.1.3` tags are unchanged; the board-side
production gate and boundary test ship as `v0.1.4`.

| # | P2 | Fix | Evidence |
|---|---|---|---|
| 1 | 移动端复制链接不可达 | `task.tsx` no longer gates the copy entry on `!isMobile()`; it is a `PressRow` with the 44px touch floor and a task-scoped accessibility label, present on 390 | TUI 390 task test asserts the entry and label; capture `25-task-ready-390.png` asserts it is inside/on-screen |
| 2 | fixture 注入 board 不存在的字段 | Removed entry-level `lastDeliveryAt` (board only sends per-target `lastDeliveryAt`; the page now derives the entry's newest value from real targets). Removed watcher `successes24h`/`failures24h` from DTO, parser, fixture and UI; watcher keeps only real `dead`/`lastError`/`lastDeliveryAt` | `board-wire.ts`, `http.ts`, `fixture-board.ts`, `sync.tsx`, `automation.tsx`; data/TUI/capture rerun |
| 3 | 缺失字段测试缺失/失效 | Added harness scenario `sync-missing` that strips `targets`, `lag`, `failures24h`, `oldestPendingAgeSeconds`; UI now emits one gap note and hides the missing lag/failure metrics. Replaced stale `board-sync-targets-unknown-slack` with `board-sync-targets-empty-slack` | TUI `同步缺失字段——一句缺口说明，不伪造指标`: backend-gap visible; lag/failures/targets-empty hidden |
| 4 | capability/typed error 只是声明 | Board `cmd/board` and keel `internal/boardintegration` call `Capabilities.Require(Durable, CrossProcessLeases, Migrations)` before serving; a weaker adapter fails startup with typed `storage.ErrUnsupported`. Conformance proves cross-process leases with two handles, not just declarations | Board `v0.1.4` (`3d6115b`); `conformance/storage.go`; keel mount gate |
| 5 | boundary test 包列表硬编码 | `TestStorageBoundary` discovers `github.com/jixoai/keel-board/...` dynamically, skips only `pgadapter`, `cmd` and `scripts`, and checks every remaining package; it also asserts `pgadapter` still owns pgx | Board `v0.1.4`; `go test ./pgadapter` |
| 6 | 证据脚本/路径不一致 | `scripts/board-phase2-ui-evidence.mjs` writes to `openspec/changes/keel-board-sync-v2/evidence/ui-gates-*`; `board-phase2-gates.sh` expects 55 screenshots; `verification.txt` names the current branches/commits | scripts diff + verification.txt |
| — | `depth.dead` 被丢弃 | `/watchers/{id}/deliveries` `depth` is parsed into `BoardWatcherDeliveryDepth` and shown in the delivery sheet as 待处理/进行中/已完成/死信 | TUI automation test opens the drawer and asserts `死信 1` |

## Tags

- `v0.1.2` `006a43b` — read-only sync/automation health (unchanged).
- `v0.1.3` `80bff1b` — storage adapter boundary (unchanged).
- `v0.1.4` `3d6115b` — assembly capability gate, discovered boundary proof,
  conformance cross-process lease assertion.
- Keel dependency: `github.com/jixoai/keel-board v0.1.4`.
