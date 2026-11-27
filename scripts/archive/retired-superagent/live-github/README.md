> **RETIRED** — the super-agent board HTTP routes are deleted; this script is history and will 404. See ../README.md.

# superagent-live-github

Live GitHub E2E for change `keel-collab-superagent` (task 5.2). It drives a real
`keel serve` against a throwaway GitHub repository and proves the whole
Source → Ticket → worker → Attention → Channel → done path with real issue
comments:

```
real issue -> Source poll -> Ticket + Assignment -> worker session
  -> model `ask` tool -> Attention + issue question comment
  -> model `report waiting` -> blocked ticket
  -> marker dedupe probe + regression suite (blocked window)
  -> product HTTP API answer -> issue answer comment -> worker turn
  -> model `report ready` -> done + issue completion comment
  -> product /superagent board + ticket detail screenshots
```

## Requirements

- `go`, `node`, `gh`, a local PostgreSQL (`createdb`/`dropdb`), Chrome and
  `playwright-core`; `nsh` only when the web build is missing.
- **A GitHub user token** with `repo` scope (a classic PAT or `gh auth token`).
  The adapter resolves `GET /user` to recognise the deployment's own comments,
  so a GitHub App installation token (no user login) is refused by design.
- Only a repository whose name contains `keel-test-` is accepted; the script
  refuses anything else and never writes outside the configured repository.

## Usage

```bash
scripts/superagent-live-github/run.sh
# reuse an existing throwaway repo and a warm build/web bundle:
KEEL_LIVE_REPO=owner/keel-test-... KEEL_LIVE_SKIP_BUILD=1 KEEL_LIVE_SKIP_WEB=1 \
  scripts/superagent-live-github/run.sh
```

Environment (all optional): `KEEL_LIVE_RUN_DIR`, `KEEL_LIVE_BIN`,
`KEEL_LIVE_WEB_DIR`, `KEEL_LIVE_PORT`, `KEEL_LIVE_MODEL_PORT`,
`KEEL_LIVE_KEEP_STACK=1`, `KEEL_LIVE_HEADLESS=false`.

The script creates a temporary database (dropped on exit), starts a scripted
OpenAI-compatible provider and two `keel serve` passes (bootstrap, then the
super-agent deployment), creates the issue, runs `e2e.mjs`, and on success
publishes the report to `$RUN/public`. A failed run publishes nothing, so a
stale PASS is never served.

## Report

- `$RUN/report.html` (+ `shots/`, `runtime/`, `data/`), rendered by
  `scripts/superagent-acceptance/report.mjs` from `report.json`.
- The meta binds the run to `git rev-parse HEAD`, tracked-tree dirtiness and the
  binary's SHA-256 (`LIVE_GIT_COMMIT` / `LIVE_GIT_DIRTY` /
  `LIVE_BIN_SHA256`), so the report can be checked against the commit it ran.
- Serve `$RUN/public` and expose it with the project tunnel; the E2E lane does
  not guess a public hostname.

## Token and actor limitations

- The token is read from `gh auth token` into the process environment only. It
  is never written to a file, log, report or commit; the public copy is
  scanned for token-looking strings before publishing.
- The live run uses one account (the token owner). The narrow self-filter
  (owner + exact trailing `<!-- keel-message:... -->`) is covered by unit tests
  for the cases a single account cannot produce live: a human quoting the
  marker, and the owner writing the marker mid-comment. A second human account
  is not exercised live.
- GitHub App installation tokens are not supported (see Requirements). A future
  adapter could key on the `<app>[bot]` author instead.

## Other known limits

- The worker model is a deterministic scripted provider. `ask`/`report`,
  their validation, idempotency, audit and write-back are the real product
  paths; model judgment quality is not evaluated.
- The lane opens an issue thread (the PR thread kind shares the adapter path but
  is not opened).
- List endpoints follow GitHub's `Link rel="next"` pages (bounded to 20 pages
  per endpoint per poll).
