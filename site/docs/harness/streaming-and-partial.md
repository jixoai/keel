# Streaming and partials

A Harness session is consumed as a stream. This page describes the two channels
a client can observe — committed events and the token-level partial bypass —
and the delivery guarantees around them: resume cursors, wakeups, heartbeat,
admission and the drain frame.

## 1. Two channels, one authority

| Channel | Endpoint | Carries | Cursor effect | Authority |
| --- | --- | --- | --- | --- |
| Committed events | `GET /v1/sessions/{id}/events/stream` | durable ledger events | `id: <seq>` per frame | **The only truth** |
| Partial deltas | same endpoint + `?partials=1` | in-flight token text/thinking | none — partial frames carry no `id:` | advisory, droppable |

The invariant is stated in the transport itself: a partial frame is not an
event — it is never stored, never replayed and never carries an SSE `id`, so it
cannot move a client's `Last-Event-ID` cursor, and a client that resumes from a
cursor never sees a partial twice
(`internal/httpapi/partials.go:5`, `internal/httpapi/server.go:690`).

## 2. The committed stream

`streamEvents` (`internal/httpapi/server.go:628`) does the following, in order:

1. **tenant boundary first** — a foreign or missing session is a 404 *before*
   any SSE headers are written; a stream that would answer 200 with an empty
   body and never end is rejected instead (`internal/httpapi/server.go:648`).
2. **admission** — a token-bucket admitter bounds new streams with 429 +
   `Retry-After`, and the control lane takes a scheduler slot; exhaustion is
   answered the same way (`internal/httpapi/server.go:653`, `:662`).
3. **headers** — `text/event-stream`, `no-store`, `X-Accel-Buffering: no`, then
   an immediate flush so the client sees the stream is live.
4. **wakeups** — the handler subscribes to the process hub, which is woken by
   PostgreSQL `LISTEN/NOTIFY` on commit; a five-second fallback timer covers a
   dropped notification, and a 15-second heartbeat keeps intermediaries from
   reaping an idle stream (`internal/httpapi/hub.go:86`, `internal/httpapi/server.go:715`).
5. **frames** — committed events are written as
   `id: <seq>\nevent: <kind>\ndata: <payload>\n\n`; the `seq` is the resume
   cursor (`internal/httpapi/server.go:628`).

Clients resume with `Last-Event-ID` (header) or `?after=` (query); both land on
the same parser (`internal/httpapi/server.go:639`). A non-streaming snapshot is
`GET /v1/sessions/{id}/events?after=&limit=`.

### What a client must do on gaps

The projection is defensive by contract: an event with
`seq > lastSeq + 1` is buffered (capacity 4096) and flagged
`needsResync()`; the client then re-fetches the missing range
(`list(after=lastSeq)` pages → apply → trust the stream again) instead of
patching the view from live frames. The full rule set lives in
`packages/chat-surface/src/engine.ts` and `docs/CHAT-SURFACE.md` §6.

## 3. Backpressure

The stream path is bounded end to end, because the product's target is a
thousand concurrent sessions inside 2GB (`docs/ARCHITECTURE.md` §1):

- a blocking `Write` is itself the backpressure — a slow client stops the
  producer instead of buffering the whole history in memory;
- the scheduler keeps classes apart (control/SSE small frames vs image/large
  blob streams) so a 1MiB image cannot queue ahead of an approval frame
  (`internal/stream/budget.go:153`);
- per-write and progress deadlines kill stalled clients; already-sent offsets
  are recorded, so a reconnecting client resumes rather than restarts
  (`internal/stream/guard.go:21`);
- blobs (images, files) stream from storage — chunked reads → `base64` →
  socket — and never materialize the whole object in memory
  (`README.md`, `docs/ARCHITECTURE.md` §5).

Overload is always an honest 429/503 with `Retry-After`, never a silent drop of
committed facts.

## 4. Partials: the token-level bypass

The bypass exists so a UI can show text as it is produced without weakening the
log. It is deliberately narrow:

```
provider stream (SSE frames from the model)
   │
   ▼
agentext.StreamTee            wraps the provider StreamFn; extracts text/thinking deltas
   │                          internal/agentext/streamtee.go:24
   ▼
agentext.PartialHub           process-local, per (tenant, session); bounded buffer (16)
   │  Publish / Subscribe     internal/agentext/turns.go:360, :386, :407
   ▼
agentapi.PartialSource        adapts the hub to the HTTP frame type
   │                          internal/agentapi/partials.go:16
   ▼
SSE ?partials=1               event: partial, data: {...}, NO id line
                              internal/httpapi/partials.go:16
```

Properties, verbatim from the design:

- **not durable** — partials never enter the event log and never appear in
  `GET /events`; a reconnecting client cannot replay them
  (`internal/agentext/doc.go`).
- **droppable** — the hub is a bounded ring; a slow consumer drops frames
  rather than blocking the producer (`internal/agentext/turns.go:373`,
  `internal/httpapi/partials.go:28`).
- **no cursor** — because the frame has no `id:`, `Last-Event-ID` is untouched;
  the committed `assistant.message` that follows is the authoritative copy and
  clears the partial on the client
  (`packages/chat-surface/src/engine.ts`).
- **opt-in** — the producer is consulted only when the request asks for
  `?partials=1`, and `GET /v1/capabilities` reports whether the deployment has
  a partial source at all (`internal/agentapi/api.go:262`).

If a process needs to know whether partials are flowing, the hub exposes
counters (published, dropped, subscribers) through its `Stats`
(`internal/agentext/turns.go:430`).

## 5. Draining a stream

A draining process does not cut streams silently. Every open SSE stream
receives a final advisory frame:

```
event: drain
data: {"retry_after_ms": 3000}
```

and then the connection is closed on a rate-limited schedule so a fleet of
clients does not reconnect in a thundering herd
(`internal/httpapi/drain.go:129`, `:159`). New API requests during drain get
503 while probes, metrics and SSE flush paths stay reachable
(`internal/httpapi/drain.go:105`). The client reconnects to the new runtime and
resumes from its cursor: the first frame after reconnection is `cursor + 1` —
committed events are neither lost nor duplicated across a handoff
(`docs/HANDOFF.md` §「怎么用」).

## 6. Acceptance evidence

The streaming behavior is pinned by tests rather than prose:

- `internal/httpapi/ops_test.go` — SSE framing, heartbeat and cursor behavior;
- `internal/httpapi/server_test.go` — tenant boundary before headers, resume
  with `after`/`Last-Event-ID`;
- `internal/agentapi/partials_sse_test.go` — partial frames carry no `id` and do
  not move the cursor;
- `cmd/e2e` — the cross-process stateless scenarios (blob resume, 2000-event
  sequence with no gap) summarized in `README.md`;
- `packages/chat-surface/test` — 10k-event projection bound and the
  reconnect/resync state machine (`docs/CHAT-SURFACE.md` §8).
