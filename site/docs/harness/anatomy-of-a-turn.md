# Anatomy of a turn

This page follows one input through the Harness: from the HTTP request that
commits it, through the claim and the loop, to the committed events a client
finally renders. Read it together with [Recovery and
compaction](/harness/recovery-and-compaction), which covers what changes when a
process dies mid-turn.

## 1. The path

```
① submit       POST /v1/sessions/{id}/input  ──▶ user.message (edge class), one transaction
② claim        worker poll ──▶ Kernel.Claim ──▶ Lease{owner, epoch, readWatermark}
③ load         runner.loadEvents ──▶ read watermark recorded (durable fact)
④ decide       owedTurn? ──▶ model resolve ──▶ fill missing tool results
⑤ loop         loop.StartRun ──▶ model stream ⇄ tool batches (per assistant message)
⑥ commit       assistant.message(inputSeqs) · tool.result(toolCallId) · compaction
⑦ settle       run.finished(summary) ──▶ Kernel.Finish ──▶ requeue unread input?
⑧ project      SSE `id: <seq>` frames ──▶ chat-surface ──▶ ConversationView
```

## 2. ① Submit: commit before anything else

The client sends `{text, requestId?, delivery?, selection?, attachments?}`
(`internal/httpapi/input_commit.go:26`). Both the input endpoint and the retry
endpoint funnel into the same commit path, `commitUserInput`
(`internal/httpapi/input_commit.go:58`), so the payload shape, the
external-session pre-check and the kernel commit cannot drift apart between
them.

- The commit is **edge class**: it carries no lease fence. The kernel validates
  tenant/session/kind/idempotency-key, assigns the `seq`, and **queues the run
  in the same transaction** — an input can never be visible without a run
  queued for it (`internal/httpapi/input_commit.go:58`, `docs/KERNEL.md` §2).
- `requestId` is the durable idempotency key (`KeyRequest`): a retry of the
  same request returns the same submission and sequence, never a second
  message (`internal/kernel/validate.go:37`).
- `delivery` is `queue` (default) or `guide`. `guide` means "inject into the
  running turn"; if no attempt is live, the input endpoint downgrades it to
  `queue` **and the downgrade is visible in the committed event** — the log
  never lies about what happened (`docs/KERNEL.md` §2).

## 3. ② Claim: the run gets an owner

The worker polls for claimable runs and executes each claim in its own
goroutine, bounded by a concurrency semaphore (`internal/agent/worker.go:170`).
`Kernel.Claim` returns a `Lease`: the scope, the fencing token
`owner@epoch`, the expiry, whether the run carries **pending input**, the
previous attempt's **read watermark**, and whether this claim took over an
expired one (`internal/kernel/kernel.go:123`, `internal/kernel/lease.go:16`).

The claim is what makes exactly-one-writer true: every commit of the attempt
carries the token, and the kernel rejects a stale owner with
`stale-epoch`/`lease-lost` (`internal/kernel/kernel.go:355`). The worker renews
the lease at `ttl/3` and cancels the attempt the moment renewal proves the
claim is gone, so a superseded run cannot keep writing
(`internal/agent/worker.go:327`).

## 4. ③–④ Load and decide

`runner.RunAttempt` performs the pre-flight in a strict order
(`internal/agent/runner.go:242`, `:280`):

1. **load** the committed log page by page (`internal/agent/runner.go:911`);
2. **record the read watermark** — `Reads.RecordRead` persists the load
   high-water mark *before* anything else, so whatever happens next (model
   failure, kill, drain), the successor can tell exactly which input this
   attempt never saw (`internal/agent/runner.go:295`);
3. **decode** committed events into the transcript, skipping unreadable
   payloads with a diagnostic instead of failing the run
   (`internal/agent/messages.go:30`, `internal/agent/runner.go:301`);
4. **decide whether a model turn is owed** (`owedTurn`,
   `internal/agent/runner.go:683`): an input is unanswered only if no
   *completed* assistant message references it (see §6); a newest assistant
   with tool calls means their results must be filled; a terminal failure
   (`stopReason: error|aborted`) consumes nothing;
5. **resolve the execution identity** (provider/model/thinking/request patch)
   from the run's selection fact — before anything is committed, so an
   unresolvable selection fails the attempt cleanly (`internal/agent/runner.go:321`);
6. **fill missing tool results**: a previous process may have committed a tool
   call without its result; execution is at-least-once and the kernel keeps one
   result per `toolCallId` (`internal/agent/runner.go:829`);
7. **present unread input last**: input that arrived while the previous attempt
   was running sits *before* that attempt's answer in the log; the runner moves
   the unread inputs to the end of the transcript so the model treats them as
   its prompt instead of "already answered" (`internal/agent/runner.go:360`,
   `:888`).

## 5. ⑤ The loop runs

`loop.StartRun` drives the PI-shaped loop: one model request per turn, one tool
batch per assistant message, stop reasons settling the turn
(`internal/loop/loop.go`). The Harness wraps it with three things:

- **compaction**, when the loadout's compaction service supplies settings
  (`internal/agent/runner.go:489`, `:467`);
- **guide injection**: the loop polls the steering feed before the first
  request, after every completed turn (all sibling tool results committed) and
  again after `PrepareNextTurn` (`internal/agent/runner.go:447`);
- **request-boundary snapshots**: `PrepareRequest` pins what each request
  actually presents, so the consumption fact is derived by request order, never
  by reading live state while the loop runs ahead through its event buffer
  (`internal/agent/runner.go:455`).

## 6. ⑥ Commit: what lands in the ledger

Every event the loop produces is committed through `Committer.Commit` with the
run's fencing token (`internal/agent/runner.go:953`):

| Event | Key | Payload highlights | Citation |
| --- | --- | --- | --- |
| `assistant.message` | `generationId` (`u<promptSeq>-a<ordinal>`) | pi `AssistantMessage` + **`inputSeqs`** + usage/stopReason | `internal/agent/runner.go:562`, `:651` |
| `tool.result` | `toolCallId` | result content/details, `isError` | `internal/agent/runner.go:943` |
| `compaction` | — | `summary`, `firstKept`, `summarized`, `timestamp` | `internal/agent/runner.go:478` |
| `run.finished` | — | the attempt summary (below) | `internal/agent/worker.go:391` |

`inputSeqs` is the load-bearing fact of the whole design:
`assistant.message.payload.inputSeqs` lists the committed `user.message`
sequences that request presented to the model. "Unanswered input" therefore
becomes a fact in the log rather than a watermark heuristic — a successor can
tell a queue input from a consumed guide input, and a crash between the
assistant commit and the settle cannot duplicate an answer
(`internal/agent/runner.go:707`, `docs/KERNEL.md` §2).

`generationId` is derived from the committed log, so a retried attempt derives
the same id and the kernel's dedupe returns the already-committed answer
instead of appending a second one (`internal/agent/runner.go:634`,
`internal/kernel/validate.go:36`).

### An event sequence for one tool-using turn

```
seq  kind                payload (abridged)
1    user.message        {text: "check the workspace", requestId: "req_1", delivery: "queue"}
2    assistant.message   {content: [{type:"toolCall", id:"tc_1", name:"workspace.list"}],
                          inputSeqs: [1], stopReason: "tool_use"}          ← commits seq 1's consumption
3    tool.result         {toolCallId: "tc_1", content: [{type:"text", text:"src/ web/ docs/"}]}
4    assistant.message   {content: [{type:"text", text:"…"}], inputSeqs: [1], stopReason: "end_turn"}
5    run.finished        {status:"done", turns: 2, tokens:{…}, toolTimings: [{tool:"workspace.list", durationMs: 90}],
                          context:{messages: 4, tokens: 812}, stopReason:"end_turn", durationMs: 2140}
```

A `guide` input committed while seq 2–4 were running would appear as its own
`user.message` (say seq 3) and be consumed by the *next* assistant that
references it in `inputSeqs`; the transcript keeps a work segment per consumed
guide input so the UI can show it (`packages/chat-surface/src/projection.ts`).

## 7. ⑦ Settle

Before settling, the worker commits `run.finished` — the attempt's own account:
status, turns, tokens, tool timings, final context size, stop reason, error,
provider/model and duration (`internal/agent/runner.go:146`,
`internal/agent/worker.go:388`). It is bookkeeping, not a message: the
transcript decoder skips it (`internal/kernel/validate.go:31`).

Then `Kernel.Finish` settles the run under the same lease
(`internal/agent/worker.go:399`):

- the **cursor** and the **read watermark** travel with the settle; input above
  the watermark re-queues the session — this is why a message that arrived
  mid-run is answered rather than stranded (`internal/agent/runner.go:207`);
- a **cancel** settles terminally instead of re-queuing (`Cancelled: true`);
- the pinned **loadout** is written into the run row for audit
  (`internal/agent/worker.go:414`, `docs/LOADOUT.md` §5).

Failure policy: an attempt that fails is retried up to `MaxAttempts` (default
3, `internal/agent/worker.go:163`); a provider failure is delivered by the loop
as a terminal `stopReason: error` message, which the worker converts into a
failed settle so the input is retried and the run row records why
(`internal/agent/worker.go:361`).

## 8. ⑧ Projection

The client's view is built from committed events only. The SSE endpoint emits
them with `id: <seq>`; partials travel as frames without an id
(`internal/httpapi/server.go:628`, [Streaming and
partials](/harness/streaming-and-partial)). `@keel/chat-surface` parses the
payloads, projects them into turns/work segments/queue/decisions, and keeps
`run.started`/`run.finished` as the turn boundaries
(`packages/chat-surface/src/projection.ts`).

Two client-visible facts come straight from the log rather than from the
server's runtime state:

- **readiness** — the queue may only drain at a `run.finished` boundary, not
  when an assistant message arrives (`packages/chat-surface/src/projection.ts`);
- **degradation** — if a `guide` was committed but no live turn consumed it,
  the event says `delivery: queue` and the UI returns the item to the queue in
  place (`docs/CHAT-SURFACE.md` §5).
