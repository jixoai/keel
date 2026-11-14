package kernel

import (
	"context"
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

// TestUserMessageSelectionIsValidated pins the shape of the selection fact: the
// kernel persists the object without interpreting it (the product layer owns
// the vocabulary), but a non-object would be replayed to every successor
// exactly as written.
func TestUserMessageSelectionIsValidated(t *testing.T) {
	for _, im := range implementations(t) {
		im := im
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "r-sel",
				Payload: json.RawMessage(`{"text":"hi","selection":{"providerId":"s2api","modelId":"m","reasoningLevel":"high"}}`)})
			for _, bad := range []json.RawMessage{
				json.RawMessage(`{"text":"hi","selection":"s2api/m"}`),
				json.RawMessage(`{"text":"hi","selection":null}`),
				json.RawMessage(`{"text":"hi","selection":["m"]}`),
			} {
				if err := h.commitErr(CommitRequest{Kind: KindUserMessage, RequestID: "r-sel-bad", Payload: bad}); !iso(err, ErrInvalidRequest) {
					t.Fatalf("selection payload %s = %v, want invalid-request", bad, err)
				}
			}
			if n := h.eventCount(h.scope); n != 1 {
				t.Fatalf("event count = %d, want 1 (rejected selections must not land)", n)
			}
		})
	}
}

// TestUserMessageRetryOfIsValidated pins the shape of the retry mark: retryOf
// names the earlier user input this input re-asks, so it must be a non-negative
// integer (a string or a float would be replayed to every successor exactly as
// written). It is shape-only, like selection: the kernel stores the fact.
func TestUserMessageRetryOfIsValidated(t *testing.T) {
	for _, im := range implementations(t) {
		im := im
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "r-retry",
				Payload: json.RawMessage(`{"text":"hi","requestId":"r-retry","retryOf":3,"delivery":"queue"}`)})
			// Zero is a legal counter value for the shape check (no input is
			// committed at seq 0; the resolution layer owns the semantics).
			h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "r-retry-zero",
				Payload: json.RawMessage(`{"text":"hi","retryOf":0}`)})
			for _, bad := range []json.RawMessage{
				json.RawMessage(`{"text":"hi","retryOf":"3"}`),
				json.RawMessage(`{"text":"hi","retryOf":-1}`),
				json.RawMessage(`{"text":"hi","retryOf":1.5}`),
				json.RawMessage(`{"text":"hi","retryOf":null}`),
				json.RawMessage(`{"text":"hi","retryOf":true}`),
				json.RawMessage(`{"text":"hi","retryOf":[1]}`),
			} {
				if err := h.commitErr(CommitRequest{Kind: KindUserMessage, RequestID: "r-retry-bad", Payload: bad}); !iso(err, ErrInvalidRequest) {
					t.Fatalf("retryOf payload %s = %v, want invalid-request", bad, err)
				}
			}
			if n := h.eventCount(h.scope); n != 2 {
				t.Fatalf("event count = %d, want 2 (rejected retryOf marks must not land)", n)
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
			first := h.mustCommit(CommitRequest{Kind: KindRunFinished, Token: lease.Token, RunID: "run-1", Payload: valid})
			if first.Kind != KindRunFinished || first.Deduped {
				t.Fatalf("run.finished = %+v", first)
			}
			// A commit retry of the same attempt dedupes by run id; a new
			// attempt is a new fact.
			replay := h.mustCommit(CommitRequest{Kind: KindRunFinished, Token: lease.Token, RunID: "run-1", Payload: valid})
			if !replay.Deduped || replay.Seq != first.Seq {
				t.Fatalf("run.finished replay = %+v, want deduped seq %d", replay, first.Seq)
			}
			if err := h.commitErr(CommitRequest{Kind: KindRunFinished, Token: lease.Token, RunID: "run-1",
				Payload: json.RawMessage(`{"status":"failed","stopReason":"error"}`)}); !iso(err, ErrKeyConflict) {
				t.Fatalf("run.finished key reuse with different payload = %v, want key conflict", err)
			}
			second := h.mustCommit(CommitRequest{Kind: KindRunFinished, Token: lease.Token, RunID: "run-2",
				Payload: json.RawMessage(`{"status":"failed","stopReason":"error"}`)})
			if second.Seq != first.Seq+1 {
				t.Fatalf("second run.finished seq = %d, want %d", second.Seq, first.Seq+1)
			}
			// A cancelled attempt is a terminal attempt outcome, not a failure.
			h.mustCommit(CommitRequest{Kind: KindRunFinished, Token: lease.Token, RunID: "run-3",
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
				json.RawMessage(`{"status":"done","provider":7}`),
				json.RawMessage(`{"status":"done","model":["gpt"]}`),
				json.RawMessage(`{"status":"done","configHashBefore":7}`),
				json.RawMessage(`{"status":"done","configHashAfter":[]}`),
				json.RawMessage(`[]`),
			} {
				if err := h.commitErr(CommitRequest{Kind: KindRunFinished, Token: lease.Token, RunID: "run-bad", Payload: bad}); !iso(err, ErrInvalidRequest) {
					t.Fatalf("payload %s = %v, want invalid-request", bad, err)
				}
			}
			if h.eventCount(h.scope) != before {
				t.Fatal("rejected summaries reached the log")
			}
			// The summary is owner-scoped: without the fence it cannot be written.
			if err := h.commitErr(CommitRequest{Kind: KindRunFinished, RunID: "run-nofence",
				Payload: json.RawMessage(`{"status":"done"}`)}); !iso(err, ErrMissingToken) {
				t.Fatalf("unfenced run.finished = %v", err)
			}
			// A stale owner cannot write a summary after its lease was taken over.
			stale := OwnerToken{Owner: "runtime-a", Epoch: lease.Token.Epoch - 1}
			if err := h.commitErr(CommitRequest{Kind: KindRunFinished, Token: stale, RunID: "run-stale",
				Payload: json.RawMessage(`{"status":"done"}`)}); !iso(err, ErrStaleEpoch) {
				t.Fatalf("stale run.finished = %v", err)
			}
		})
	}
}

// TestFinishProbeIgnoresTerminalFailures pins the parity between the kernel's
// re-queue probe and the runner's owedTurn rule: a terminal failure presented
// the input but consumed nothing, so pending_input must stay set. Without the
// exclusion the probe would settle the session "done" with the input unanswered.
func TestFinishProbeIgnoresTerminalFailures(t *testing.T) {
	for _, im := range implementations(t) {
		im := im
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "r1", Payload: userPayload("one")})
			h.mustCommit(CommitRequest{Kind: KindUserMessage, RequestID: "r2", Payload: userPayload("two")})
			lease := h.claimScope(h.scope, "runtime-a")
			h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: lease.Token, GenerationID: "u2-a0",
				Payload: json.RawMessage(`{"role":"assistant","content":[],"stopReason":"error","inputSeqs":[1,2]}`)})
			if err := h.k.Finish(ctx, FinishRequest{Lease: lease, Cursor: 3, ReadThrough: 1, MaxAttempts: 3}); err != nil {
				t.Fatalf("finish: %v", err)
			}
			pin, err := h.k.RunPin(ctx, h.scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.Status != RunStatusPending {
				t.Fatalf("run status = %s, want pending (seq 2 was presented by a terminal failure only)", pin.Status)
			}
		})
	}
}
