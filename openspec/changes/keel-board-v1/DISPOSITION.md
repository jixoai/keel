# keel-board-v1 — Phase 2 disposition

Phase 2 is delivered; the board is the single task face and the retired
`/superagent` UI/API is deleted. This file records the small set of items that
are deliberately not complete, why, and where they went. No item below is
represented as passing in the UI or evidence.

## Partial data sources (14.2 / 15.5)

`board` v0.1.1 has no API for:

- a global list of external targets per sync entry (targets are keyed by board
  thread; there is no list-by-key endpoint),
- timestamps for the oldest pending inbox/outbox row (the status DTO carries
  counts only), or
- a 24h failure count.

The sync page therefore shows entries, inbox/outbox depth, dead letters,
outbound failures, retries and credential state, and labels targets, time lag
and 24h counts as **unknown** (`lag: null` from the mount layer). It does not
synthesize values. Tasks 14.2 and 15.5 remain partially checked with this note.
Adding these fields requires a board module API change (v0.1.x) and is deferred
to the next board change, not hidden behind a client fallback.

## Actions without a backend endpoint

`takeover`, `release.force` and `answer` have no board v0.1.1 endpoint. The
server therefore never places them in `moves`; the UI does not invent them. A
tab that lost a claim token can wait for lease expiry or ask the task owner /
channel manager to assign or cancel. The claim-lost copy says exactly that. If
the board later adds these actions, the normal `moves` contract will surface
them without a client rule.

## Residual P2 (backlog)

1. Tenant board flags use a 5s cache and the mirror ticks every 30s, so a
   disable is bounded eventual consistency (tests use TTL=-1).
2. Ordinary `PutResourceRelation` does not share the orphan-adoption advisory
   lock; a very narrow race can add a descendant relation between the check and
   the adoption commit.
3. An answer recorded but still delivery-pending can leave a same-reason block
   reusing the closed Attention.
4. Route ACL is checked at entry create/update/boot, not continuously; deleting
   a channel relation later stops visibility but not imports until restart.
5. A same-tenant caller without a channel relation can learn that an id is a
   mirrored root from the bridge's 409 guard (no content or cross-tenant leak).
6. The retired live-GitHub/acceptance scripts are archived under
   `scripts/archive/retired-superagent/`; a board-native live E2E is future
   work. The old API routes return 404.
7. `kit-imports.mjs` reports three pre-existing violations in untouched web
   files; `nsh check` itself passes.

## Verification

`scripts/board-phase2-gates.sh` is the final gate: exclusive full TUI, full Go
tests, the UI screenshot/state HTML (26/26 matrix, UNCOVERED=0), file coverage
(UNCOVERED=0), and OpenSpec validation. Evidence and the pin are in
`evidence/phase-2-gates.md` / `phase-2-pin.txt` and
`evidence/phase-2-ui-gates-*`.
