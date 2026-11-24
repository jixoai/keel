# Events and protocol

The durable contract of the Core is the **event ledger**. Everything else — transcripts,
checkpoints, SSE frames, the Harness UI — is a projection of it. This page lists the event kinds,
how a commit is classified and made idempotent, what an input's delivery mode means, and what the
SSE stream guarantees.

## 1. The event kinds (the whole vocabulary)

The registry is `internal/kernel/validate.go:46`: **25 registered kinds** (rows group a family
that shares one shape). A kind that is not registered cannot enter the log; the string values are
pinned against `internal/store` by a test, and the edge whitelist (`EdgeKinds`) is exactly the
edge-scoped entries.

| Kind | Class | Idempotency key | Emitted today | Semantics |
|---|---|---|---|---|
| `user.message` | edge-scoped | `requestId` | yes (client input) | the only kind a client may write; `payload.delivery: queue\|guide` |
| `assistant.message` | owner-scoped | `generationId` | yes (loop) | a model answer; `payload.inputSeqs` records the inputs presented to the model for this request |
| `tool.result` | owner-scoped | `toolCallId` | yes (loop) | a tool result, paired with its call by `toolCallId` |
| `compaction` | owner-scoped | — (none) | yes (compaction service) | the fact that context was compacted |
| `run.started` | owner-scoped | — (none) | **no — reserved vocabulary** | implemented fences, no runtime emits it (the run row already records start/attempts) |
| `run.failed` | owner-scoped | — (none) | **no — reserved vocabulary** | same as above |
| `run.finished` | owner-scoped | `run` (`KeyRun`) | yes (worker, once per settled attempt) | the attempt summary: status/turns/tokens/toolTimings/context/stopReason/error/durationMs |
| `external.event` | external-scoped | `requestId` | yes (external gateway) | Keel's compatibility log of an external backend fact; only valid in a session with `state_owner=external` |
| `task.created` · `task.updated` · `task.attached` · `task.detached` · `task.closed` · `task.note` | owner-scoped, dual-lane | `task` | yes (task-log service) | the task card's append-only history; the card and its event view are projections of these rows |
| `fs.write` | owner-scoped | `fs` | yes (file plugin) | one successful workspace file write |
| `fs.capture` | owner-scoped, dual-lane | `capture` | yes (file plugin / harness job) | a capture settlement; a background job outlives its run and commits on the system lane with `payload.jobId` |
| `fs.capture_failed` | system-scoped | `capture-failed` | yes (file plugin) | a capture that failed, as its own fact |
| `config.rotated` | system-scoped | `config` | **no — vocabulary only** | the ledger anchor of a `config.toml` version; rotation is a library API today (`docs/ASSEMBLY.md:205`) |
| `placement.joined` | system-scoped | `placement` | **no — vocabulary only** | a session's group membership fact; the placement package keeps membership in rows, not events |
| `placement.left` | system-scoped | `placement` | **no — vocabulary only** | leaving a group |
| `supervision.escalated` | system-scoped | `supervision` | yes (supervisor) | the supervisor escalated a stalled/abandoned run |
| `guardrail.tripped` | system-scoped | `guardrail` | yes (agent guardrails) | a guardrail refused or stopped an attempt |
| `resume.attempt` | system-scoped | `resume` | yes (supervisor) | a resume of a recovered attempt |
| `notification.ack` | system-scoped | `notif-ack` | yes (notify) | a consumer consumed a notification |
| `notification.push_sent` | system-scoped | `notif-push` | yes (notify) | the one native/in-page push fallback for a notification |

Three clarifications that the source insists on (`internal/kernel/validate.go:34`):

- `run.started` / `run.failed` are **reserved**: the vocabulary and fences exist, but no runtime
  emits them. Do not read their presence as "the loop emits lifecycle events".
- `run.finished` is different: the worker emits it once per settled attempt because a client needs
  turns/tokens/stop reason without reconstructing them from the transcript. It is bookkeeping, not
  a message — the transcript decoder skips it (`internal/kernel/kernel.go:87`).
- **Owner-scoped is not the same as "written under a lease".** A kind marked `system: true` in the
  registry (every `task.*` kind and `fs.capture`) is owner-scoped while a run owns the fact, and may
  also be committed through the system lane when no run does — the initial task card exists before
  any run, and a capture job outlives the attempt that started it
  (`internal/kernel/validate.go:56`).

## 2. Commit classes: who carries the fence

`internal/kernel/kernel.go:292` defines four classes; a commit's class is a property of its kind,
not of the caller.

| Class | Kinds | Fence (lease token) | Checked against |
|---|---|---|---|
| `owner-scoped` | `assistant.message`, `tool.result`, `compaction`, `run.*`, `fs.write`, and the dual-lane kinds while a run owns the fact | **required** | the session's current owner + epoch |
| `edge-scoped` | `user.message` | **must be absent** | tenant/session existence, edge whitelist, idempotency |
| `external-scoped` | `external.event` | must be absent | the session must be marked `state_owner=external` and not closed |
| `system-scoped` | `fs.capture_failed`, `config.rotated`, `placement.*`, `supervision.escalated`, `guardrail.tripped`, `resume.attempt`, `notification.*`, plus the dual-lane kinds when no run owns the fact | must be absent — the commit must set `CommitRequest.System` | the kind must be `ClassSystem` or declared `system: true`; never client-writable |

The last class is the one that keeps "harness fact" from becoming "forged lease": a system-scoped
commit is accepted only through the explicit system lane, so a harness-driven fact (a failed
capture, a placement change, a notification ack) never has to fabricate an owner token
(`internal/kernel/kernel.go:306`). A client cannot reach that lane at all: the edge endpoint is
exactly the `ClassEdge` entries.

The edge whitelist is exactly the `ClassEdge` entries (`EdgeKinds()` in
`internal/kernel/validate.go:102`), so `POST /v1/sessions/{id}/events` accepts `user.message` and
answers 400 for anything else; a cross-tenant or missing session answers 404. During a handoff
drain, edge-scoped input still commits (that is how users keep typing while the process is being
replaced) — the loop consumes it in whichever process owns the lease next
(`docs/SERVICE-GRAPH.md:204`).

External sessions are symmetric in the other direction: native edge writes to them are refused with
409 (`ErrExternalSession`), and `/v1/sessions/{id}/run` refuses to queue a run for them — the
kernel's enqueue predicate filters on `state_owner <> 'external'` as fail-closed, so even a dirty
run row is never claimed.

## 3. Idempotency: one key, one scope

A commit carries exactly one idempotency key, and the kind decides which space it lives in
(`keyFor` in `internal/kernel/validate.go:188`):

- `requestId` — `user.message`, `external.event`
- `generationId` — `assistant.message`
- `toolCallId` — `tool.result`
- `run` — `run.finished`: the attempt id, so a retried attempt is a new fact while a commit retry
  of the same attempt dedupes (`internal/kernel/validate.go:53`)
- `task` — the six `task.*` kinds; `fs` — `fs.write`; `capture` / `capture-failed` — `fs.capture` /
  `fs.capture_failed`; `config` / `placement` / `supervision` / `guardrail` / `resume` /
  `notif-ack` / `notif-push` — the matching system-scoped kind
  (`internal/kernel/kernel.go:234`)
- `compaction`, `run.started` and `run.failed` declare no key and must carry **none**; passing one
  is an error, not a silent ignore

The uniqueness scope is `(tenant_id, session_id, dedupe_key)` — a partial unique index
(`internal/store/schema.sql:117`). Consequences:

- **Retrying the same input is safe.** `POST /v1/sessions/{id}/input` with the same `requestId`
  returns the same submission (verified response: `{"created":true,"requestId":"…","seq":1,…}`);
  the scope is the session, so the same `requestId` in two sessions is two independent commits and
  can never return another session's submission.
- **Tool side effects are at-least-once, the log is exactly-once.** The execution layer may run a
  tool twice (crash recovery, handoff), but a dedupe key can only be inserted once per session, so
  the final log holds exactly one result per `toolCallId`. Replay-safe ≠ exactly-once, and the
  framework does not pretend otherwise (`docs/ARCHITECTURE.md:46`).
- Comparing payloads ignores formatting: the same request with re-ordered keys or different
  whitespace is still the same request (`samePayload`/`canonicalJSON`, `internal/kernel/validate.go:637`).

## 4. Delivery: `queue` and `guide`

`user.message.payload.delivery` is the durable contract between the edge (which writes the intent)
and the loop (which consumes it) (`internal/kernel/validate.go:378`):

- **`queue`** (the default; an absent field means `queue`, so old clients and old dumps keep
  working): the input enters the model context after the current run ends, at the next claim.
- **`guide`**: the input is injected inside the current turn, **after all sibling tool results have
  been committed**, one message at a time, without interrupting the model stream.

If there is no active attempt, the HTTP edge degrades `guide` to `queue` — and the written event
says `delivery: queue`, so the client can see what actually happened and re-order its UI.

The injection point is deliberately *not* the commit moment but the **request boundary**: the seqs
injected are attributed by the loop's `PrepareRequest` snapshot and merged into the following
`assistant.message.inputSeqs`. The loop runs ahead of the runner through an event buffer, so
reading live state at commit time would attribute "a guide meant for the next request" to the
previous answer (`docs/KERNEL.md:54`).

## 5. What an answer consumed: `inputSeqs`

`assistant.message.payload.inputSeqs` is the set of `user.message` seqs presented to the model for
that request. It is the log's own record of "what did this answer actually see", and the kernel uses
it to answer a harder question: **is any input still unanswered?**
(`docs/KERNEL.md:58`)

- An input is *unanswered* if no **completed** assistant (`end_turn`/`tool_use`/…) references it in
  `inputSeqs`. A terminal message with `stopReason: error|aborted` does not count as an answer: a
  failure will be retried, a cancellation will not be re-run.
- Terminal failed messages do not enter the model context at all — replaying an empty/error
  assistant would poison the next request. When the latest assistant is a terminal failure,
  `owedTurn` is true, so "tool turn completed + next provider call failed" cannot be mistaken for
  "nothing left to do" and settled as `done`.
- `read_watermark` remains as a diagnostic and for old logs / upgraded prefixes: before the first
  assistant carrying `inputSeqs`, inputs are judged by the watermark; from that point on,
  `inputSeqs` decides. `runs.pending_input` and `runs.read_watermark` survive a takeover precisely
  so a drain/kill/exit does not swallow unread input
  (`internal/kernel/schema/0004-pending-input.sql:25`, `docs/HANDOFF.md:150`).

## 6. Settling: the status policy

The worker commits `run.finished` under the same lease before settling; a failed summary write does
not change the run's terminal state. The status policy is `finishStatus`
(`internal/kernel/validate.go:314`):

| Situation | Resulting run status |
|---|---|
| Cancelled (`FinishRequest.Cancelled`) | `failed` — terminal. The input stays unanswered in the log; the next run (started by later input) answers it. Re-queueing would make the cancel a no-op. |
| Attempt failed, attempt budget not spent | `pending` — retry |
| Attempt failed, attempt budget spent | `failed` |
| Success, newer input arrived while it ran | `pending` (`moreInput`) |
| Success, no newer input | `done` |

Provider failures are terminal messages with `stopReason: error` by PI semantics; the runner
classifies them as attempt failure and the worker settles as failure while the retry policy
applies as usual (`docs/KERNEL.md:74`).

## 7. Reading the log: snapshots, SSE, partials

**Snapshot and resume.** `GET /v1/sessions/{id}/events?after=<seq>&limit=<n>` returns committed
events with a cursor. `GET /v1/sessions/{id}/events/stream?after=<seq>` is the SSE stream: the
`id:` field of a frame is the event `seq`, so an SSE client resumes with `Last-Event-ID` (or
`after`) and never misses committed events. Subscribers are woken by `LISTEN/NOTIFY` rather than
polling (`internal/store/schema.sql:313`), with heartbeats as the backstop.

**Partials are a bypass, not part of the ledger.** With `partials=1`, token-level frames are pushed
as `event: partial` **without** an `id:`; they never enter the events table, never move
`Last-Event-ID`, and are not replayed on reconnect. A client that disconnects mid-partial receives
only committed events on reconnect — the complete message arrives with its own `id`.

**Drain frames.** When a runtime drains (SIGTERM or handoff), every open SSE connection first
receives the events it has not read, then:

```
event: drain
data: {"retry_after_ms":3000}
```

The client should reconnect against the new owner after that hint and resume with
`Last-Event-ID`; the first frame of the new stream is `cursor+1`.

**A verified stream.** A single input against the fake provider produces exactly this sequence
(captured live, `…` elided):

```json
{"events":[
  {"seq":1,"kind":"user.message","payload":{"text":"hello keel","requestId":"9e5088a0-…"}},
  {"seq":2,"kind":"assistant.message","payload":{"role":"assistant","content":[{"text":"…","type":"text"}],"inputSeqs":[1],"stopReason":"end_turn"}},
  {"seq":3,"kind":"run.finished","payload":{"status":"done","turns":1,"provider":"fake","model":"fake","stopReason":"end_turn","durationMs":24}}
]}
```

Note what is *not* in the log: no `run.started`, no partial frames, no lease or epoch bookkeeping.
Those are run-row and process concerns; the log records facts a successor must be able to replay.

## 8. Validation: what the kernel refuses

The kernel validates payload shapes that a replay depends on, at commit time, because a bad payload
in the log is permanent (`internal/kernel/validate.go:260`):

- `user.message`: `text`/`requestId` must be strings if present; `delivery` must be exactly
  `queue`/`guide`; `selection` must be a JSON object; `retryOf` must be a non-negative integer.
  Unknown fields are allowed so the edge can carry metadata without a kernel change.
- `assistant.message`: every `toolCall` block must have an `id` and a `name`, and no `id` may repeat
  inside the message — otherwise a result could no longer be attached to a unique call
  (`internal/kernel/validate.go:260`).
- `tool.result`: `payload.toolCallId` must equal the request's `toolCallId`, and the result must
  point at a committed assistant call for that id (`internal/kernel/validate.go:284`).
- `run.finished`: `status` must be one of `done`/`failed`/`cancelled`; counters must be
  non-negative integers when present; `toolTimings` must be an array of objects (never null)
  (`internal/kernel/validate.go:417`).
- `external.event`: the payload must name `backend` and `externalRef`, and `stateOwner` must be
  `external` when present (`internal/kernel/validate.go:569`).

## 9. Ask the server, do not guess

The wire contract for clients is versioned: Harness API is prefixed `/v1` and only grows
(`internal/harnessapi/api.go:53`). A client must not infer features from a binary version; it asks
`GET /v1/capabilities` and downgrades on `false` flags. The document is the feature map built in
`internal/harnessapi/api.go:556`; abridged — deployment-wired blocks (`plugins`, `pluginRevision`,
`mcpApps`, `configFile`) appear only when that seam is wired, and every feature flag follows the
seam it actually needs:

```json
{"apiVersion":"v1","serviceVersion":"keel-agent/0.1.0",
 "features":{"decisions":true,"hooks":true,"partials":true,"plugins":true,
             "plugins.manage":true,"plugins.audit":true,"plugins.approvals":true,"plugins.bridge":true,
             "sessions.cancel":true,"sessions.list":true,
             "session.search":true,"session.export":true,"session.deliverables":true,
             "session.filePreview":true,"task.panel":true,"team.tasks":true,
             "settings.models":true,"settings.secrets":false,"settings.skills":true,
             "skills.completion":true,"subagents":true,"triggers":true,
             "mcp.apps":true,"mcp.apps.sandboxOrigin":false,"mcp.apps.context":true,
             "mcp.apps.tools":true,"mcp.apps.resources":true,"mcp.apps.message":true,
             "mcp.apps.links":true,"mcp.events":false},
 "limits":{"sessionsPageDefault":50,"sessionsPageMax":200,"subagentsPageMax":200,
           "teamTasksPageDefault":50,"teamTasksPageMax":200,
           "searchPageDefault":20,"searchPageMax":100,
           "exportPageDefault":200,"exportPageMax":500,"exportPageBytes":524288,
           "deliverablesDefault":100,"deliverablesMax":500},
 "hooks":["subagent.started","subagent.finished","trigger.before","trigger.after",
          "turn.started","turn.finished","decision.opened","decision.answered","mcp.surface.changed"],
 "plugins":["keel.example"]}
```

The endpoint reference is on the next page: [Harness API reference](/core/agentapi).
