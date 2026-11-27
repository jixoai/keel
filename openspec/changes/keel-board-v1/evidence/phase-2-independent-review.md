# Phase 2 independent review (16.3)

- Reviewer: independent subagent, read-only static review; no Codex.
- Code under review: `295087c5..b3336c5e` (`task/keel-board-phase2-visual`).
- Scope: the Phase 2 board surface, sync/automation states, claim/lease
  semantics, mobile 390 layout, tests and evidence template. The reviewer did
  not run tests or edit files.

## Round 1 — P0=0, P1=2

1. **P1-1 sync health called a credential failure an all-clear.**
   `sync.tsx` only counted dead letters and outbound failures in
   `暂无已知故障`, while credential-invalid/missing badges were already shown.
   Fixed in `b3336c5e`: the summary now includes credential issues and says
   `凭据问题会让对应入口停止收发；轮换后自动恢复。`; the TUI sync test
   asserts it after retrying all dead letters.
2. **P1-2 read-only users were promised a re-claim.**
   The claim-lost branch was reachable without `board.write`, and its copy said
   `等待租约到期后可重新认领`; the viewer explanation was suppressed. Fixed in
   `b3336c5e`: `can("board.write")` gates the claim-lost action path and the
   action row; read-only copy says `当前账号只有查看权限`, and the reader TUI
   test asserts it.

The same fix set also corrected the watcher `post` label, real
`reaction:slack:…` dead-letter references, thread/channel labels, filter titles,
lease-relative copy, `leased` delivery state and the stale-token move filter.

## Round 2 — one new P1, fixed

The reviewer found that the first stale-token fix also hid settlement moves when
`/me` was unavailable but this tab still held a valid claim token. Fixed in
`b3336c5e`: `boardAvailableMoves` accepts `identityKnown`; a token is honored
when `mine` is true or identity is explicitly unknown, while a known-other claim
still filters token-bearing moves. Both button and keyboard callers pass
`identityKnown: boardStore.meLoaded()`; data tests cover both branches.

## Final verdict

> P1-1 PASS, P1-2 PASS, new P1 PASS. No new P0/P1.
> **P0/P1=0, 16.3 PASS.**

Residual P2 items are recorded in `DISPOSITION.md` (watcher dead-letter DTO gap,
390 selected-tab auto-scroll/short-screen filter sheet, provider error detail in
server logs). They do not block Phase 2.
