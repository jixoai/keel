# Approvals and questions

An agent that cannot ask is an agent that guesses. The Harness therefore has a
first-class primitive — a **decision** — for everything that needs a human
answer: tool approvals, clarifying questions, choices between options.

The design rule is the same as everywhere else: the primitive lives in the
product layer, and the ledger's vocabulary does not grow. A decision is **not**
a kernel event kind (`internal/kernel/validate.go:36`); it is a durable row
projected onto the existing tasks bookkeeping table with an answer journal
(`internal/agentext/doc.go`).

## 1. The primitive

```
open ──▶ (waiting) ──┬─▶ answered      (option and/or free text; the answer is final)
                     ├─▶ timed_out     (bounded; default 5m, max 24h)
                     └─▶ cancelled     (the session was cancelled)
```

- Statuses: `open`, `answered`, `timed_out`, `cancelled`
  (`internal/agentext/decisions.go:28`).
- An answer is an option id, a free-text string, or both — free text is only
  accepted when the decision declared `allowFreeText`
  (`internal/agentext/decisions.go:49`, `:309`).
- Timeouts are bounded per decision: `timeoutMs` on open, default 5 minutes,
  capped at 24 hours (`internal/agentext/decisions.go:162`, `:163`).
- Per session there is a bound on in-process bookkeeping (512 decisions); the
  durable rows themselves are unbounded but a session opening thousands of
  decisions is a bug (`internal/agentext/decisions.go:167`).

## 2. Identity and idempotency

Two keys, both durable, both at different levels:

| Key | Scope | Meaning |
| --- | --- | --- |
| `requestId` (opener's key) | the question | opening twice with the same key returns the same decision (`DecisionIDFor`, `internal/agentext/decisions.go:222`) |
| `decisionId` (deterministic UUID) | the answer | an answer is idempotent: the same answer replays as a no-op; a *different* answer is a conflict, never an overwrite |

Answer semantics, exactly as implemented
(`internal/agentext/decisions.go:309`):

- already `answered` with the **same** answer → returns the row, `deduped=true`;
- already `answered` **differently** → `ErrDecisionConflict` (HTTP 409);
- already `timed_out`/`cancelled` → `ErrDecisionClosed` (HTTP 409);
- unknown option id, or free text when not allowed → `ErrInvalidDecision`
  (HTTP 400).

Because the decision id is deterministic, a client that resends an answer after
a network failure converges instead of forking the conversation.

## 3. The lifecycle of a question

```
run (model or tool policy wants a human decision)
  │  DecisionBroker.Open {requestId, kind, prompt, options, allowFreeText, timeoutMs}
  │    ├─ durable row saved (tasks bookkeeping)               decisions.go:233
  │    ├─ hook emitted: decision.opened                       hooks.go:25
  │    └─ timeout scheduled (bounded, cancellable)            decisions.go:542
  ▼
client sees the question
  │  GET  /v1/sessions/{id}/decisions            (list)
  │  GET  /v1/sessions/{id}/decisions/{decisionId}
  │  POST /v1/sessions/{id}/decisions/{decisionId}  {optionId?|text?}
  │    └─ first answer wins; hook: decision.answered          decisions.go:606
  ▼
the waiter resumes
  │  DecisionBroker.Wait returns the terminal decision        decisions.go:374
  ▼
the run continues (or settles) — the answer is a fact again
```

Cancelling a session cancels its open decisions in one pass — `CancelSession`
(`internal/agentext/decisions.go:437`) — so a stopped run never leaves
questions that nobody can answer.

The broker is also the only place where the answer journal lives: rows carry
their own ordered events (`DecisionEvent`, `internal/agentext/decisions.go:78`),
which is how an operator can reconstruct "asked at T1, answered at T2" without
touching the ledger.

## 4. Reaching the client

Decisions are exposed to clients in two equivalent ways:

1. **HTTP** — the `/v1/sessions/{id}/decisions` routes, mounted by the product
   API next to every other `/v1` route (`internal/agentapi/api.go:173`). The
   wire type is the `Decision` document (id, kind, prompt, options,
   recommendation, status, answer, timestamps).
2. **Projection** — the UI consumes `decision` events. A host either feeds them
   from the decisions API or synthesizes the same shape from a SSE-adjacent
   channel; the projection only knows the event shape, never the API
   (`docs/CHAT-SURFACE.md` §3).

The projection treats an unanswered decision as an **interrupt**: a run that
finishes with an open decision is not "done", and AG-UI clients receive
`RUN_FINISHED {outcome: {type: "interrupt", interrupts: [...]}}` with
`interrupt.id = decisionId` (`docs/CHAT-SURFACE.md` §7). The human answer then
opens a new run — the question never blocks a worker thread.

If the deployment has no decision broker wired, the routes answer 503 and
`GET /v1/capabilities` reports `"decisions": false`, so a client hides the
section instead of guessing (`internal/agentapi/api.go:261`).

## 5. What a decision is not

- **Not a kernel event.** The kernel never learns what an approval means; it
  only ever sees the next committed `user.message`/`assistant.message`
  (`internal/agentext/doc.go`).
- **Not a hook.** Hooks observe; decisions ask and block. The hook bus carries
  `decision.opened`/`decision.answered` as *notifications* of the primitive
  (`internal/agentext/hooks.go:25`), never as the mechanism itself.
- **Not a UI state.** The UI may render optimistically, but the durable row is
  the answer of record; a refresh re-reads it from the API.

## 6. Planned: plugin-facing approvals

The planned plugin standard adds one more consumer: a plugin UI can **request
an approval** through the host bridge (`keel.plugin.*`), and plugin
installation/enablement itself must go through a durable approval before it
takes effect
(`openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`, "permissions and sandbox").
Both are marked **planned** — the current checkout has no plugin bridge and no
plugin install path.
