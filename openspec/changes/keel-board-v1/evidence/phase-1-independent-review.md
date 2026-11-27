# Phase 1 final independent review

Captured from two independent reviewer runs (project rule: independent subagents, no Codex).
Formatting normalized; findings unchanged.

- Code baseline: `e0cfe5e77e78aa71879bc61c8daf3c42bc016b47` (hardening code + evidence; docs/pin commit `e54c1050` follows it).
- Verdict: **PASS** — P0 = 0, P1 = 0. 13.3 is signable on this code tree.
- The reviewer changed nothing and used a separate PostgreSQL database; the temporary database and /tmp archive copy were removed.

## Round-1 P0/P1 closure (independently verified)

| Item | Result | Evidence |
| --- | --- | --- |
| P0-1 sync channel ACL | closed | Write path `sync_manager.go` Create/Update and boot both call `validateRouteACL` (`sync_routes.go`); the target must exist and have a `board_channel` relation on itself or an ancestor. Disabled entries can stage. Historical bad entries stay disabled, start no worker, and report `routesReady:false`/`routeIssue`. Orphan adoption is limited to a path with zero own/ancestor/descendant relation, under one transaction with sorted advisory locks; `board.manage` only. Route inheritance, descendant blocking and leaf adoption are covered by PG tests. E2E: missing channel 400 → POST /channels 201 → entry 201 → restart → webhook import → alice reads 200. |
| P1-1 board.enabled bypass | closed | `serve.go` mounts `Gate(Augment(bridge))` after the contract rebase; disabled tenants answer 404 `board_disabled` before bridge guards/supervisor. `Mirror` takes `Surface.BoardEnabled` and stops `SyncOnce`/`SyncTicket` (flag read failure is false). E2E: supervisor calls 0 and board head unchanged; resume works. |
| P1-2 duplicate block | closed | `TaskInfo.Version` maps the root message version; block intent includes ticket + assignment + reason digest, and the request ID appends the version. Retry with the same open intent is one Attention; block → answer → block(same reason) opens a second; a new assignment opens a new one. Mutation (constant version) turned the target test RED. |
| Channel-level authorization (new) | closed | Bridge actions use `Authorize(..., task.Channel, PermissionLabel)` instead of `AuthorizeAnywhere`. E2E: a board.write user with no relation is 404 for all seven actions, supervisor calls 0, head unchanged; tenant-admin/bot-operator members can claim/complete/block. Mirror grants the operator groups `owner` on the mirrored channel; bot-operator's preset carries board.read/write (team scope, no board.manage). |

## Gate evidence

The three gate scripts ran on the pinned commit and exited 0:

- `scripts/keel-board-phase1-gate.sh`: boardintegration real PG + race, access/accessgate/cmd tests, full `-p 1` suite, gofmt/vet, binary size.
- `scripts/keel-board-phase1-sync-gate.sh`: entry/flag/secret tests, boardintegration + race, full `-p 1`, board v0.1.1 conformance, 10/10 sync E2E scenarios (including fresh channel, sidecar 5xx/malformed/timeout, permalink), format/vet, binary size.
- `scripts/keel-board-phase1-superagent-gate.sh`: public-PG superagent tests at `-p 1`, race, binary size, and 4/4 mutation checks.

Binary size on the clean checkout: keel linux/amd64 +2,960 KiB, arm64 +2,560 KiB; runtime +8/+0 KiB; 4096 KiB budget, OK. `cmd/runtime` adds no dependency.

Reviewer mutations (all RED): remove entry ACL check, make `CheckEnabled` always true, treat mirror read failure as empty, restore the old block request ID, remove the durable worker-row gate, constant block version, swap channel `Authorize` back to `AuthorizeAnywhere`.

## P2 (non-blocking, recorded for Phase 2 backlog)

1. Production tenant flags cache is 5s and the mirror tick is 30s, so disabling is bounded eventual consistency; tests use TTL=-1 and do not cover the cache path.
2. Orphan adoption checks descendants, but ordinary `PutResourceRelation` does not take the same advisory lock. A narrow race can add a descendant relation between the check and commit.
3. An answer recorded but still delivery-pending leaves the ticket blocked and does not advance the board message version; a same-reason block in that window can reuse the closed Attention.
4. Unauthenticated requests to a disabled tenant get 401 rather than 404; the bridge reply/mutation guard can reveal that an id is a mirrored root to a same-tenant caller without a channel relation (no content or cross-tenant leak).
5. Route ACL is checked at create/update/boot, not continuously; deleting a channel relation later does not stop imports until restart.
6. The superagent E2E uses a memory fake supervisor for its state machine; the real orchestrator's Ask/Answer path is covered by unit tests and the contract E2E, not by the live sync E2E.
7. The sidecar timeout step asserts at least one real timeout and eventual success; it does not explicitly query the dead-letter list to prove absence.
8. `kit-imports.mjs` reports three pre-existing violations in untouched web files (attachment-row, books/groups, ui/kit/components/styles).

## Taggability / 13.3

The reviewer approved 13.3 on `e0cfe5e`/`e54c1050`: P0/P1 clear, all three Phase 1 gates exit 0 on the same code, evidence/pin present, worktree clean, no resident processes. 13.4 is signed with this record; the change stays active for Phase 2.
