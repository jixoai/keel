package kernel

import (
	"context"
	"testing"
)

// TestEdgeCommitWakesTheRun pins the M1 contract: committing a user.message
// queues (or keeps queued) the session's run in the SAME transaction, so the
// edge cannot end up with a committed message and no run for it — the shape
// that left input unprocessed when the HTTP layer's bookkeeping failed after
// the commit.
func TestEdgeCommitWakesTheRun(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			scope := h.bareSession() // no run row yet

			// The commit alone queues the run: no store enqueue, no handler.
			receipt := h.mustCommit(CommitRequest{Scope: scope, Kind: KindUserMessage,
				RequestID: "wake-1", Payload: userPayload("hello")})
			pin, err := h.k.RunPin(ctx, scope)
			if err != nil {
				t.Fatalf("run pin after the edge commit: %v", err)
			}
			if pin.Status != RunStatusPending {
				t.Fatalf("run status = %s, want pending (the commit must queue it)", pin.Status)
			}
			lease, err := h.k.ClaimScope(ctx, scope, "runtime-a", h.ttl())
			if err != nil {
				t.Fatalf("claim after the edge commit: %v", err)
			}

			// A second input while the run is executing keeps it running: the
			// finish probe (read-through watermark) is what re-queues it.
			second := h.mustCommit(CommitRequest{Scope: scope, Kind: KindUserMessage,
				RequestID: "wake-2", Payload: userPayload("still here")})
			if second.Seq <= receipt.Seq {
				t.Fatalf("second input seq = %d, want > %d", second.Seq, receipt.Seq)
			}
			pin, err = h.k.RunPin(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.Status != RunStatusRunning {
				t.Fatalf("run status = %s, want running (an executing run is not re-queued mid-flight)", pin.Status)
			}
			// The owner finishes with the truthful watermark (the first input):
			// the second input forces a re-queue instead of being stranded.
			if err := h.k.Finish(ctx, FinishRequest{
				Lease: lease, Cursor: second.Seq, ReadThrough: receipt.Seq}); err != nil {
				t.Fatalf("finish: %v", err)
			}
			pin, err = h.k.RunPin(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.Status != RunStatusPending {
				t.Fatalf("run status = %s, want pending for the unread input", pin.Status)
			}

			// A deduplicated replay (same requestId, same payload) wakes nothing
			// new but still answers the receipt; the log holds one copy.
			replay := h.mustCommit(CommitRequest{Scope: scope, Kind: KindUserMessage,
				RequestID: "wake-1", Payload: userPayload("hello")})
			if !replay.Deduped || replay.Seq != receipt.Seq {
				t.Fatalf("replay = %+v, want the committed receipt %d", replay, receipt.Seq)
			}
			n := 0
			for _, ev := range h.events(scope) {
				if ev.Kind == KindUserMessage {
					n++
				}
			}
			if n != 2 {
				t.Fatalf("user messages = %d, want 2", n)
			}
		})
	}
}

// TestEdgeCommitNeverWakesExternalSessions: an external session has no native
// run/lease flow, not even when Keel records an edge-scoped message for it.
func TestEdgeCommitNeverWakesExternalSessions(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			scope := h.bareSession()
			if _, err := h.k.OpenExternalSession(ctx, scope, externalMarks("dsh", "ext-1")); err != nil {
				t.Fatalf("open external session: %v", err)
			}
			h.mustCommit(CommitRequest{Scope: scope, Kind: KindUserMessage,
				RequestID: "ext-input", Payload: userPayload("hello backend")})

			if _, err := h.k.RunPin(ctx, scope); !iso(err, ErrNoRunnable) {
				t.Fatalf("external session got a run row: %v", err)
			}
			if _, err := h.k.ClaimScope(ctx, scope, "runtime-a", h.ttl()); !iso(err, ErrNoRunnable) {
				t.Fatalf("external session became claimable: %v", err)
			}
		})
	}
}

// TestEdgeKeyConflictDocumentsTheContract pins the tightened idempotency
// semantics the HTTP edge relies on: the same key with the same content is a
// replay (dedupe), the same key with different content is a conflict and
// appends nothing.
func TestEdgeKeyConflictDocumentsTheContract(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			scope := h.bareSession()
			first := h.mustCommit(CommitRequest{Scope: scope, Kind: KindUserMessage,
				RequestID: "conflict-1", Payload: userPayload("first")})

			replay := h.mustCommit(CommitRequest{Scope: scope, Kind: KindUserMessage,
				RequestID: "conflict-1", Payload: userPayload("first")})
			if !replay.Deduped || replay.Seq != first.Seq {
				t.Fatalf("same content replay = %+v, want dedupe at %d", replay, first.Seq)
			}

			err := h.commitErr(CommitRequest{Scope: scope, Kind: KindUserMessage,
				RequestID: "conflict-1", Payload: userPayload("different")})
			if err == nil {
				t.Fatal("different content under the same requestId was accepted")
			}
			if !iso(err, ErrKeyConflict) {
				t.Fatalf("conflict error = %v, want ErrKeyConflict", err)
			}
			if n := h.eventCount(scope); n != 1 {
				t.Fatalf("events after the conflict = %d, want 1 (nothing appended)", n)
			}
		})
	}
}

// TestAbandonedClaimKeepsPendingInput is the B3 regression: the unread-input
// flag is delivered by a claim but only CLEARED by a finish that proves the
// input was read. An attempt abandoned before it reads the log (drain, crash,
// kill) must leave the input visible to the next claim, or the successor
// inspects a log whose tail is an old answer and settles done without ever
// answering the input.
func TestAbandonedClaimKeepsPendingInput(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			scope := h.bareSession()

			first := h.mustCommit(CommitRequest{Scope: scope, Kind: KindUserMessage,
				RequestID: "u1", Payload: userPayload("one")})
			lease1 := h.claimScope(scope, "runtime-a")
			if !lease1.PendingInput {
				t.Fatal("the first claim did not deliver the queued input")
			}
			if lease1.ReadWatermark != 0 {
				t.Fatalf("a fresh session's watermark = %d, want 0 (no proven read point)", lease1.ReadWatermark)
			}
			// Input arrives while the attempt is running; the attempt answers
			// only the input it read (watermark = the first input's seq).
			h.mustCommit(CommitRequest{Scope: scope, Kind: KindUserMessage,
				RequestID: "u2", Payload: userPayload("two")})
			answer := h.mustCommit(CommitRequest{Scope: scope, Kind: KindAssistantMessage,
				Token: lease1.Token, GenerationID: "g1", Payload: assistantPayload("one?")})
			if err := h.k.Finish(ctx, FinishRequest{
				Lease: lease1, Cursor: answer.Seq, ReadThrough: first.Seq}); err != nil {
				t.Fatal(err)
			}
			pin, err := h.k.RunPin(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.Status != RunStatusPending || !pin.PendingInput {
				t.Fatalf("after the partial run: status=%s flag=%t, want pending with the flag set", pin.Status, pin.PendingInput)
			}
			if pin.ReadWatermark != first.Seq {
				t.Fatalf("watermark after the partial run = %d, want %d", pin.ReadWatermark, first.Seq)
			}

			// Attempt 2 claims and is ABANDONED before reading (the drain/kill
			// path: it never finishes). The claim must not have consumed the
			// flag.
			lease2 := h.claimShort(scope, "runtime-b")
			if !lease2.PendingInput {
				t.Fatal("the abandoned attempt's claim did not deliver the input")
			}
			h.expire(lease2)

			// The next owner must still see the input as unread.
			lease3 := h.claimScope(scope, "runtime-c")
			if !lease3.PendingInput {
				t.Fatal("abandoning an attempt lost the unread input")
			}
			if lease3.ReadWatermark != first.Seq {
				t.Fatalf("an abandoned attempt changed the watermark: %d, want %d", lease3.ReadWatermark, first.Seq)
			}
			// Reading everything and finishing clears it.
			if err := h.k.Finish(ctx, FinishRequest{
				Lease: lease3, Cursor: answer.Seq, ReadThrough: answer.Seq}); err != nil {
				t.Fatal(err)
			}
			pin, err = h.k.RunPin(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.Status != RunStatusDone || pin.PendingInput {
				t.Fatalf("after the proving finish: status=%s flag=%t, want done and clear", pin.Status, pin.PendingInput)
			}
			if pin.ReadWatermark != answer.Seq {
				t.Fatalf("watermark after the proving finish = %d, want %d", pin.ReadWatermark, answer.Seq)
			}
		})
	}
}

// TestRecordReadIsFencedAndMonotonic covers the durable read point: it is a
// fenced run-scoped write and it never moves backwards.
func TestRecordReadIsFencedAndMonotonic(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			scope := h.scope
			lease := h.claimScope(scope, "runtime-a")

			if err := h.k.RecordRead(ctx, lease, 5); err != nil {
				t.Fatalf("record read: %v", err)
			}
			pin, err := h.k.RunPin(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.ReadWatermark != 5 {
				t.Fatalf("watermark = %d, want 5", pin.ReadWatermark)
			}
			// A lower (delayed) write never rewinds it.
			if err := h.k.RecordRead(ctx, lease, 3); err != nil {
				t.Fatalf("record read (lower): %v", err)
			}
			pin, err = h.k.RunPin(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.ReadWatermark != 5 {
				t.Fatalf("watermark rewound to %d", pin.ReadWatermark)
			}
			// A stale owner cannot write it.
			stale := lease
			stale.Token.Epoch++
			if err := h.k.RecordRead(ctx, stale, 9); !iso(err, ErrStaleEpoch) {
				t.Fatalf("stale record read = %v, want stale-epoch", err)
			}
			// A missing token is refused.
			if err := h.k.RecordRead(ctx, Lease{Scope: scope}, 9); !iso(err, ErrMissingToken) {
				t.Fatalf("record read without token = %v, want missing-token", err)
			}
		})
	}
}

// TestFinishPersistsWatermarkOnFailure is the N1 kernel contract: a finish that
// reports a run error must still persist the read-through watermark, because
// that is what lets the successor see input the failed attempt never read.
func TestFinishPersistsWatermarkOnFailure(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			scope := h.scope
			lease := h.claimScope(scope, "runtime-a")

			// The attempt reports its load point only at Finish (a caller that
			// did not record one while running): the failure must not drop it.
			// It committed an answer (seq 2) and then failed.
			h.mustCommit(CommitRequest{Scope: scope, Kind: KindAssistantMessage,
				Token: lease.Token, GenerationID: "g1", Payload: assistantPayload("partial")})
			runErr := context.DeadlineExceeded
			if err := h.k.Finish(ctx, FinishRequest{
				Lease: lease, Cursor: 2, ReadThrough: 1, Err: runErr, MaxAttempts: 5}); err != nil {
				t.Fatalf("finish with error: %v", err)
			}
			pin, err := h.k.RunPin(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if pin.ReadWatermark != 1 {
				t.Fatalf("a failed finish dropped the watermark: %d, want 1", pin.ReadWatermark)
			}
			if pin.Status != RunStatusPending {
				t.Fatalf("run status = %s, want pending (the attempt may retry)", pin.Status)
			}
			if !pin.PendingInput {
				t.Fatal("a failed finish did not leave unread input pending")
			}
		})
	}
}
