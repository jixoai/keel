# keel-ga §1 compaction evidence (2026-11-20)

Lane: P1B2. Baseline `6e53bff`; WIP `a91d285` reviewed and completed on top.
The evidence files were generated on the completion tree and are committed
together with it (see the commit that contains this directory).

## What was verified

- Compaction event commit and checkpoint save are ordered: the runner commits the
  event in the ordered loop-event consumer (after every `message_end` /
  `tool_execution_end` it summarizes), then saves the checkpoint.
  `internal/agent/runner.go:738` (`case agentcore.CompactionEvent`).
- Save failure is best-effort: the committed event is never rolled back and the
  run keeps going. `internal/agent/runner.go:738-802`.
- Checkpoint row: `upto_seq` = compaction event seq, `kept_from_seq` = first
  retained event seq (NULL/0 when not anchored).
  `internal/kernel/checkpoint.go:12,32`;
  `internal/kernel/mem.go:236,261`;
  `internal/kernel/pg.go:462,483`;
  schema `internal/store/schema.sql:84-94`.
- Replay: newest checkpoint summary + events with `seq >= kept_from_seq`;
  compaction events covered by the summary are skipped.
  `internal/agent/runner.go:1278` (`modelView`), `:1313` (`checkpointMessage`).
- Anchor resolution: `firstKeptTranscript` removes the in-memory tool-loadout
  system declaration (never an event) so the index refers to the committed
  transcript. `internal/agentcore/event.go:162-167`, `internal/loop/loop.go:755-779`,
  `internal/agent/runner.go:1344` (`checkpointAnchor`).
- P1 fail-safe (review 2026-11-20): the compaction cut index is in memory order.
  `presentUnreadInputLast` now reorders message seqs in lockstep and returns
  `movedStart`; if the cut lands in the moved unread block, or a guide input was
  injected mid-run, the checkpoint is saved unanchored (`kept_from_seq` NULL) so
  replay uses the full log instead of a log-order index that can point past a
  moved input. `internal/agent/runner.go:551-565,792-798,1186`;
  `internal/agent/steering.go:154`.
- Fallback: no checkpoint, or `kept_from_seq` unset/0, replays the full log. The
  event log is never truncated and stays readable by seq.
- Docs: `docs/ARCHITECTURE.md:46-65` (at-least-once tools, single-session serial,
  non-persistent partial, checkpoint replay); `docs/HOST.md:78-83`.

## Tests

`01-kernel-checkpoint.txt` (mem + PG, `KEEL_TEST_DSN`):

- `TestCheckpointProjection/mem`, `/pg`: upsert by `upto_seq`, anchor preserved,
  newest row wins, identical replay is still one row, validation errors,
  missing session = `ErrNotFound`.
- `TestCheckpointTenantIsolation/mem`, `/pg`: foreign session sees no row.

`02-agent-compaction.txt` (real PostgreSQL):

- `TestCompactionPersistsEventAndCheckpoint`: one compaction event + one
  `checkpoints` row; original events readable by seq; replay of the same
  compaction upserts (row count stays 1).
- `TestCompactionCheckpointFailureKeepsEvent`: failing checkpoint store still
  leaves exactly one committed compaction event.
- `TestSecondCompactionAnchorsFromCheckpoint`: a second compaction starts from
  the first checkpoint's summary + tail and anchors its own checkpoint against
  that transcript view; replay uses the newest summary.
- `TestCheckpointCutInMovedUnreadInputFailSafe`: an input committed during the
  previous attempt sits before that attempt's answer in the log, so
  `presentUnreadInputLast` moves it to the tail; a cut inside that moved block
  must not anchor to a later log seq. Asserts `kept_from_seq <= the moved
  input's seq`, the replay contains the input, and a successor does not spin on
  it (no `owedTurn` livelock). Verified to fail with the pre-fix anchor
  (`kept_from_seq=5` for input seq 4).
- `TestNewRunReplaysFromCheckpoint`: fresh runner asks the model for summary +
  retained assistant tail + new input; summarized user input is not re-sent;
  the full log still decodes more messages.
- `TestReplayFallsBackWithoutCheckpoint`: checkpoint row deleted -> full log
  replay (summarized input + inline compaction marker present).
- `TestReplayFallsBackWithoutAnchor`: `kept_from_seq = NULL` -> full log replay.

## Gates

- `03-checkpoints-schema.txt`: DDL excerpt.
- `04-full-test.txt`: `go test -p 1 -count=1 ./...` with `KEEL_TEST_DSN`
  (only the DSN from `.env.local` exported) -> exit 0, 55 packages ok, no FAIL.
- `05-conformance.txt`: `scripts/conformance.sh` ->
  TOTAL 1084 pass, 0 fail, 1 skip (`TestHelperProcess` helper-process skip).
- `06-gofmt-build-vet.txt`: gofmt clean, `go build ./...` exit 0,
  `go vet` on the changed packages exit 0.

Note: the first full-suite attempt sourced all of `.env.local`, which exported
`KEEL_SERVE_PASSWORD` and made `cmd/keel.TestAgentCLIRendersFieldValidation`
fail for an unrelated reason. Re-running with only `KEEL_TEST_DSN` exported is
green; CI's environment does not carry the local serve passphrase.

## Reproduce

```sh
export KEEL_TEST_DSN="$(grep -E '^KEEL_TEST_DSN=' .env.local | head -1 | cut -d= -f2-)"
go test ./internal/kernel -run 'TestCheckpoint' -count=1 -v
go test ./internal/agent -run 'TestCompaction|TestNewRunReplays|TestReplayFallsBack' -count=1 -v
go test -p 1 -count=1 ./...
scripts/conformance.sh /tmp/keel-conformance.json
```
