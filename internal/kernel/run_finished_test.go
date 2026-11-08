package kernel

import (
	"encoding/json"
	"testing"
)

// TestUserMessageDeliveryIsValidated pins the delivery vocabulary at the edge:
// queue and guide are the only two modes, an absent field means queue, and a
// misspelled mode is refused before it can reach the log (a durable "guide"
// typo would otherwise be read as queue by every successor, silently).
func TestUserMessageDeliveryIsValidated(t *testing.T) {
	for _, im := range implementations(t) {
		im := im
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			for i, ok := range []string{`"queue"`, `"guide"`} {
				payload := json.RawMessage(`{"text":"hi","requestId":"r-` + string(rune('a'+i)) + `","delivery":` + ok + `}`)
				h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "r-" + string(rune('a'+i)), Payload: payload})
			}
			for _, bad := range []json.RawMessage{
				json.RawMessage(`{"text":"hi","delivery":"steer"}`),
				json.RawMessage(`{"text":"hi","delivery":null}`),
				json.RawMessage(`{"text":"hi","delivery":1}`),
			} {
				if err := h.commitErr(CommitRequest{Kind: KindUserMessage, RequestID: "r-bad", Payload: bad}); !iso(err, ErrInvalidRequest) {
					t.Fatalf("delivery payload %s = %v, want invalid-request", bad, err)
				}
			}
			if n := h.eventCount(h.scope); n != 2 {
				t.Fatalf("event count = %d, want 2 (rejected deliveries must not land)", n)
			}
		})
	}
}

// TestRunFinishedPayloadIsValidated pins the attempt summary contract: the
// status is the attempt outcome, the counters are non-negative integers, and a
// malformed summary is refused before it reaches the log (a client would
// otherwise report a run that cannot be read).
func TestRunFinishedPayloadIsValidated(t *testing.T) {
	for _, im := range implementations(t) {
		im := im
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			lease := h.claimScope(h.scope, "runtime-a")

			// The full shape the worker writes.
			valid := json.RawMessage(`{
				"status":"done","turns":2,
				"tokens":{"input":120,"output":30,"total":150},
				"toolTimings":[{"tool":"clock","durationMs":3},{"tool":"echo","durationMs":1,"isError":true}],
				"context":{"messages":6,"tokens":120},
				"stopReason":"end_turn","durationMs":900}`)
			first := h.mustCommit(CommitRequest{Kind: KindRunFinished, Token: lease.Token, Payload: valid})
			if first.Kind != KindRunFinished || first.Deduped {
				t.Fatalf("run.finished = %+v", first)
			}
			// Bookkeeping kinds carry no idempotency key: a retried attempt's
			// summary is a second fact.
			second := h.mustCommit(CommitRequest{Kind: KindRunFinished, Token: lease.Token,
				Payload: json.RawMessage(`{"status":"failed","stopReason":"error"}`)})
			if second.Seq != first.Seq+1 {
				t.Fatalf("second run.finished seq = %d, want %d", second.Seq, first.Seq+1)
			}
			// A cancelled attempt is a terminal attempt outcome, not a failure.
			h.mustCommit(CommitRequest{Kind: KindRunFinished, Token: lease.Token,
				Payload: json.RawMessage(`{"status":"cancelled","stopReason":"aborted"}`)})

			before := h.eventCount(h.scope)
			for _, bad := range []json.RawMessage{
				json.RawMessage(`{}`),
				json.RawMessage(`{"status":"pending"}`),
				json.RawMessage(`{"status":7}`),
				json.RawMessage(`{"status":"done","turns":-1}`),
				json.RawMessage(`{"status":"done","durationMs":"fast"}`),
				json.RawMessage(`{"status":"done","tokens":[]}`),
				json.RawMessage(`{"status":"done","tokens":{"input":"many"}}`),
				json.RawMessage(`{"status":"done","toolTimings":{}}`),
				json.RawMessage(`{"status":"done","toolTimings":[{"isError":"yes"}]}`),
				json.RawMessage(`[]`),
			} {
				if err := h.commitErr(CommitRequest{Kind: KindRunFinished, Token: lease.Token, Payload: bad}); !iso(err, ErrInvalidRequest) {
					t.Fatalf("payload %s = %v, want invalid-request", bad, err)
				}
			}
			if h.eventCount(h.scope) != before {
				t.Fatal("rejected summaries reached the log")
			}
			// The summary is owner-scoped: without the fence it cannot be written.
			if err := h.commitErr(CommitRequest{Kind: KindRunFinished,
				Payload: json.RawMessage(`{"status":"done"}`)}); !iso(err, ErrMissingToken) {
				t.Fatalf("unfenced run.finished = %v", err)
			}
			// A stale owner cannot write a summary after its lease was taken over.
			stale := OwnerToken{Owner: "runtime-a", Epoch: lease.Token.Epoch - 1}
			if err := h.commitErr(CommitRequest{Kind: KindRunFinished, Token: stale,
				Payload: json.RawMessage(`{"status":"done"}`)}); !iso(err, ErrStaleEpoch) {
				t.Fatalf("stale run.finished = %v", err)
			}
		})
	}
}
