# W2.4 task-log — evidence

Branch `task/keel-w2-tasklog`, base `7f82d40`. All commands ran inside the
worktree; the test DSN was read from `KEEL_TEST_DSN_W2_TASKLOG` in `.env.local`
and never printed.

## Delivered

| Item | State | Where |
|---|---|---|
| `internal/tasklog` projection from the ledger (`task_cards` / `task_events`) | done | `internal/tasklog/{types,service,projection,pg}.go` |
| Durable tables + RLS | done | `internal/store/schema.sql`, `internal/store/rls.go`, `internal/store/tasks.go` |
| Mandatory taskId (attribution gate, auto initial card, typed refusal) | done | `internal/toolsurface/tasks.go` (`gatedTool`), `internal/toolsurface/builtins.go` |
| `tasks` tool, all 8 actions (`create/update/attach/detach/close/list/show/note`) | done | `internal/toolsurface/tasks.go`, `internal/agentext/tasks.go` |
| `keel.tasks` plugin backend (disabled ⇒ `plugin_disabled`) | done | `internal/agentext/tasks.go`, `internal/pluginpkg/builtin/keel.tasks.yaml` |
| Sub-agent card with `parentTaskId` (spawn link) | done | `keel.tasks` `subagent.started` hook → `Service.LinkSubagent` |
| Fork card projection (`originSeq`, `parentTaskId` → origin card) | task-log half done; `agent.fork` stays `not_implemented` | `Service.ForkCard`; file-view prefix remains a layerfs dependency |
| Tests | done | `internal/tasklog/tasklog_test.go`, `internal/toolsurface/taskgate_test.go`, `internal/agentext/tasks_test.go` |

## Gates

```text
$ go build ./...                                        # PASS (no output)
$ go vet ./internal/tasklog/ ./internal/toolsurface/ \
        ./internal/agentext/ ./internal/store/ ./internal/agent/ ./cmd/keel/
                                                         # PASS (no output)
$ KEEL_TEST_DSN=… go test -race \
        ./internal/tasklog/ ./internal/toolsurface/ \
        ./internal/agentext/ ./internal/store/ ./internal/agent/
ok  internal/tasklog   4.0s
ok  internal/toolsurface 1.5s
ok  internal/agentext  15.3s
ok  internal/store     9.0s
ok  internal/agent    24.0s
$ KEEL_TEST_DSN=… go test ./...                          # EXIT=0, 47 packages ok, 0 FAIL
$ ./scripts/agent-runtime-e2e.sh --evidence …/w2.4-tasklog/agent-runtime-e2e
[agent-runtime-e2e] PASS — evidence in …/w2.4-tasklog/agent-runtime-e2e
```

The e2e run kept its own artifacts in `agent-runtime-e2e/` (`serve.log`,
`wire-requests.jsonl`, member/captain events and run rows, port release). The
script drops its dedicated database and kills serve/stub on exit; no
`keel serve`, stub provider or e2e binary process was left (`pgrep` empty).

## Test coverage that matters

- `TestInitialCardIsIdempotentAndAutoAttributed` — deterministic initial card,
  `Resolve("")` attribution, malformed/foreign ids typed, exactly one
  `task.created` in the ledger.
- `TestProjectionRebuildIsIdempotentAndRepairsStrayRows` — two rebuilds yield
  identical stats; row count equals the ledger's task-event count; payloads are
  equal event by event; a row written directly around the ledger is erased.
- `TestAttachDetachNoteAndCloseAreEvents` — attach replacement, detach, note,
  close (second close is a no-op), required ledger kinds present.
- `TestForkCreatesANewCardPointingAtTheOrigin` — `parentTaskId`/`originSeq`,
  cross-session child projection, relation survives rebuilds, cross-tenant fork
  refused as not-found.
- `TestSubagentCardLinksToParentActiveCard` — link is idempotent (one
  `task.updated`).
- `TestCrossTenantTaskAccessIsRefused` — resolve/show/close/list of another
  tenant's card all answer not-found/empty.
- `TestTaskGate*` — missing taskId stamped from the ambient card; unknown card
  refused before the tool runs with `task_not_found`; no resolvable card ⇒
  `task_id_required`; valid id passed through; non-run bridge calls untouched;
  wrapping does not move the manifest hash; disabled plugin ⇒ `plugin_disabled`.
- `TestTasksPlugin*` — plugin action routing, card/show documents, typed errors,
  subagent hook, missing service.

## Boundary notes / deviations (explicit)

1. **`internal/agent/runner.go` (+ its regression test)** — 6 task kinds were
   added to the *bookkeeping tail* skip list of `legacyOwedTurn`. Without this,
   a `task.created` committed right after a child session's first input (the
   sub-agent spawn link) is the newest ledger event; the heuristic read it as
   "nothing owed", and the child run settled `done` with 0 turns without calling
   the model (reproduced in the first e2e run). Task kinds are bookkeeping by
   the `task-log` spec ("no transcript decoder appends it to the model
   context"), so the tail list must skip them. This is the only change outside
   the listed paths; it is two lines plus the mirror list in
   `review_regression_test.go`.
2. Test-only edits outside the listed files: `internal/toolsurface/toolsurface_test.go`
   (tasks.create is no longer reserved; nil backend now `not_wired`),
   `cmd/keel/plugin_prompt_test.go` (new `newPluginSurface` argument),
   `internal/agentext/plugin_builtin_test.go` (register the new builtin pair).
3. `agent.fork` remains `not_implemented` by design; this slice provides the
   task-log half (`ForkCard`: new session card, `parentTaskId` → origin,
   `originSeq`) but not the layerfs file-view prefix.

## Open / follow-ups

- `tasks.create` inside a run that later needs a fresh successor run is safe
  because read watermarks are recorded per attempt; the bookkeeping skip list is
  what makes the tail heuristic robust regardless.
- `RebuildSession` materializes one session's task events in memory while
  folding; sessions with very large logs may want a chunked fold later (the
  projection rows themselves are written in one transaction).
- The task-log event payload shape (`taskId` in the payload, camelCase fields)
  is defined in `internal/tasklog/types.go`; W4's panel should read the
  projection, never the payload directly.
