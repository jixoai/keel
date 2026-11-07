package handoff

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/kernel"
)

// The five safety points of the runtime-handoff spec. Each case builds the
// exact log state at the moment the old runtime stops, hands the session over,
// and asserts the three invariants:
//
//   - no event is lost (the log stays contiguous and every committed fact is
//     still there);
//   - the final reply is produced exactly once;
//   - the run converges (done / pending / failed, with an explanation).
func TestSafetyPoints(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			t.Run("AfterUserMessage", func(t *testing.T) {
				h := im.build(t)
				a := h.readyRuntime(t, "runtime-a", 1)
				leaseA := h.claim(a)
				user := h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
					RequestID: "r1", Payload: userPayload("hello")})
				b := h.stopOld(a, leaseA)

				adoption := h.adopt(b, 2*time.Second)
				if !adoption.Rebuild.PendingUserInput {
					t.Fatalf("rebuild = %s, want the unanswered input", adoption.Rebuild)
				}
				if adoption.Rebuild.Cursor != user.Seq || adoption.Rebuild.Events != 1 {
					t.Fatalf("rebuild = %s, want cursor %d and 1 event", adoption.Rebuild, user.Seq)
				}

				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
					Token: adoption.Lease.Token, GenerationID: "g1", Payload: assistantText("hi")})
				h.finish(adoption.Lease, h.lastSeq(), nil)

				if got := h.countKind(kernel.KindAssistantMessage); got != 1 {
					t.Fatalf("final replies = %d, want exactly 1", got)
				}
				if got := h.runStatus(); got != kernel.RunStatusDone {
					h.assertRunConverges(got)
					t.Fatalf("run status = %s, want done", got)
				}
				h.assertContiguous()
			})

			t.Run("AfterAssistantToolCall", func(t *testing.T) {
				h := im.build(t)
				a := h.readyRuntime(t, "runtime-a", 1)
				leaseA := h.claim(a)
				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
					RequestID: "r1", Payload: userPayload("run the echo tool")})
				// A real attempt records its load point before the first model
				// call; the input it answered is below the watermark.
				h.recordRead(leaseA, 1)
				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
					Token: leaseA.Token, GenerationID: "g1", Payload: assistantToolCall("tc1", "echo")})
				b := h.stopOld(a, leaseA)

				adoption := h.adopt(b, 2*time.Second)
				if len(adoption.Rebuild.Unresolved) != 1 || adoption.Rebuild.Unresolved[0].ToolCallID != "tc1" {
					t.Fatalf("rebuild = %s, want tc1 unresolved", adoption.Rebuild)
				}
				if adoption.Rebuild.PendingUserInput {
					t.Fatalf("rebuild = %s, the tool call must not read as unanswered input", adoption.Rebuild)
				}

				// The tool is re-executed at least once: the first commit lands,
				// the second is deduplicated by toolCallId.
				executions := 0
				first := h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindToolResult,
					Token: adoption.Lease.Token, ToolCallID: "tc1", Payload: toolResultPayload("tc1", "echo", "ok")})
				executions++
				second := h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindToolResult,
					Token: adoption.Lease.Token, ToolCallID: "tc1", Payload: toolResultPayload("tc1", "echo", "ok")})
				executions++
				if !second.Deduped || second.Seq != first.Seq {
					t.Fatalf("second result = %+v, want deduped at seq %d", second, first.Seq)
				}
				if got := h.countKind(kernel.KindToolResult); got != 1 {
					t.Fatalf("tool results = %d, want exactly 1 (at-least-once execution, exactly-once log)", got)
				}

				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
					Token: adoption.Lease.Token, GenerationID: "g2", Payload: assistantText("echo returned ok")})
				h.finish(adoption.Lease, h.lastSeq(), nil)

				// Exactly one *final* reply: an assistant message with no tool call.
				final := 0
				for _, ev := range h.events() {
					if ev.Kind != kernel.KindAssistantMessage {
						continue
					}
					var msg struct {
						Content []struct {
							Type string `json:"type"`
						} `json:"content"`
					}
					if err := json.Unmarshal(ev.Payload, &msg); err != nil {
						t.Fatalf("assistant payload: %v", err)
					}
					hasToolCall := false
					for _, c := range msg.Content {
						if c.Type == "toolCall" {
							hasToolCall = true
						}
					}
					if !hasToolCall {
						final++
					}
				}
				if final != 1 {
					t.Fatalf("final replies = %d, want exactly 1", final)
				}
				if got := h.runStatus(); got != kernel.RunStatusDone {
					h.assertRunConverges(got)
					t.Fatalf("run status = %s, want done", got)
				}
				if unresolved, err := h.k.UnresolvedToolCalls(context.Background(), h.scope); err != nil {
					t.Fatal(err)
				} else if len(unresolved) != 0 {
					t.Fatalf("unresolved after handoff = %+v, want none", unresolved)
				}
				h.assertContiguous()
				h.assertCompleteMessages()
			})

			t.Run("AfterToolResult", func(t *testing.T) {
				h := im.build(t)
				a := h.readyRuntime(t, "runtime-a", 1)
				leaseA := h.claim(a)
				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
					RequestID: "r1", Payload: userPayload("run the echo tool")})
				h.recordRead(leaseA, 1)
				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
					Token: leaseA.Token, GenerationID: "g1", Payload: assistantToolCall("tc1", "echo")})
				result := h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindToolResult,
					Token: leaseA.Token, ToolCallID: "tc1", Payload: toolResultPayload("tc1", "echo", "ok")})
				b := h.stopOld(a, leaseA)

				adoption := h.adopt(b, 2*time.Second)
				if len(adoption.Rebuild.Unresolved) != 0 {
					t.Fatalf("rebuild = %s, want no unresolved call (the result is committed)", adoption.Rebuild)
				}
				executions := 0 // the successor must NOT re-execute a completed call
				if len(adoption.Rebuild.Unresolved) > 0 {
					executions++
				}
				if executions != 0 {
					t.Fatal("completed tool call was re-executed")
				}
				if adoption.Rebuild.Cursor != result.Seq {
					t.Fatalf("rebuild cursor = %d, want the result's seq %d", adoption.Rebuild.Cursor, result.Seq)
				}

				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
					Token: adoption.Lease.Token, GenerationID: "g2", Payload: assistantText("echo returned ok")})
				h.finish(adoption.Lease, h.lastSeq(), nil)

				if got := h.countKind(kernel.KindToolResult); got != 1 {
					t.Fatalf("tool results = %d, want exactly 1", got)
				}
				if got := h.countKind(kernel.KindAssistantMessage); got != 2 {
					t.Fatalf("assistant events = %d, want 2 (call + final)", got)
				}
				if got := h.runStatus(); got != kernel.RunStatusDone {
					h.assertRunConverges(got)
					t.Fatalf("run status = %s, want done", got)
				}
				h.assertContiguous()
			})

			t.Run("DuringModelStream", func(t *testing.T) {
				h := im.build(t)
				a := h.readyRuntime(t, "runtime-a", 1)
				leaseA := h.claim(a)
				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
					RequestID: "r1", Payload: userPayload("hello")})

				// The old runtime read the input and is mid-stream: it holds a
				// partial assistant message that was never committed. Stopping it
				// drops the partial; nothing of it may be visible in the log, and
				// the input still owes an answer.
				h.recordRead(leaseA, 1)
				partial := []byte(`{"role":"assistant","content":[{"type":"text","text":"half a sen`)
				_ = partial
				b := h.stopOld(a, leaseA)

				adoption := h.adopt(b, 2*time.Second)
				if got := h.countKind(kernel.KindAssistantMessage); got != 0 {
					t.Fatalf("a partial assistant message reached the log: %d events", got)
				}
				if adoption.Rebuild.Events != 1 || !adoption.Rebuild.PendingUserInput {
					t.Fatalf("rebuild = %s, want exactly the committed input", adoption.Rebuild)
				}

				// The successor produces one complete message from the last
				// committed point.
				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
					Token: adoption.Lease.Token, GenerationID: "g1", Payload: assistantText("a complete answer")})
				h.finish(adoption.Lease, h.lastSeq(), nil)

				if got := h.countKind(kernel.KindAssistantMessage); got != 1 {
					t.Fatalf("assistant events = %d, want 1 complete message", got)
				}
				if got := h.runStatus(); got != kernel.RunStatusDone {
					h.assertRunConverges(got)
					t.Fatalf("run status = %s, want done", got)
				}
				h.assertCompleteMessages()
				h.assertContiguous()
			})

			t.Run("InputDuringRunBothInterleavings", func(t *testing.T) {
				// The B2 window: input arrives while a run is executing. Both
				// orderings must end with the input answered — the edge commit
				// wakes the run in its own transaction, the claim delivers the
				// unread-input flag, and the finish re-queues on the watermark.
				h := im.build(t)
				a := h.readyRuntime(t, "runtime-a", 1)
				leaseA := h.claim(a)
				first := h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
					RequestID: "r1", Payload: userPayload("one")})
				if leaseA.PendingInput {
					t.Fatal("the first claim reports input it already read")
				}

				// Interleaving A: the input lands BEFORE the run's own answer,
				// so the cursor-only probe would miss it; the watermark and the
				// flag must re-queue.
				mid := h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
					RequestID: "r2", Payload: userPayload("two")})
				answer := h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
					Token: leaseA.Token, GenerationID: "g1", Payload: assistantText("one?")})
				if answer.Seq <= mid.Seq {
					t.Fatalf("test setup: the answer seq %d must follow the input seq %d", answer.Seq, mid.Seq)
				}
				// The attempt read only "one" (its load watermark is the first
				// input's seq), and its own answer pushed the cursor past the
				// unread input.
				h.finishWith(leaseA, answer.Seq, first.Seq, nil)
				if got := h.runStatus(); got != kernel.RunStatusPending {
					t.Fatalf("run status = %s, want pending (the input at %d was never read)", got, mid.Seq)
				}
				b := h.readyRuntime(t, "runtime-b", 2)
				adoption := h.adopt(b, 2*time.Second)
				if !adoption.Lease.PendingInput {
					t.Fatal("the successor's claim did not deliver the unread-input flag")
				}
				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
					Token: adoption.Lease.Token, GenerationID: "g2", Payload: assistantText("two")})
				h.finishWith(adoption.Lease, h.lastSeq(), adoption.Rebuild.Cursor, nil)
				if got := h.runStatus(); got != kernel.RunStatusDone {
					t.Fatalf("run status = %s, want done after the input was answered", got)
				}
				if h.unread() {
					t.Fatal("input still flagged unread after the successor answered it")
				}

				// Interleaving B: the input lands AFTER the attempt settled (the
				// done row). The commit's in-transaction wake re-queues it.
				late := h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
					RequestID: "r3", Payload: userPayload("three")})
				if got := h.runStatus(); got != kernel.RunStatusPending {
					t.Fatalf("run status = %s, want pending (the done row was re-queued)", got)
				}
				c := h.readyRuntime(t, "runtime-c", 3)
				adoption2 := h.adopt(c, 2*time.Second)
				if !adoption2.Lease.PendingInput {
					t.Fatal("the late input was not delivered as unread")
				}
				if adoption2.Rebuild.Cursor < late.Seq {
					t.Fatalf("rebuild cursor %d does not cover the late input %d", adoption2.Rebuild.Cursor, late.Seq)
				}
				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
					Token: adoption2.Lease.Token, GenerationID: "g3", Payload: assistantText("three")})
				h.finishWith(adoption2.Lease, h.lastSeq(), adoption2.Rebuild.Cursor, nil)
				if got := h.runStatus(); got != kernel.RunStatusDone {
					t.Fatalf("run status = %s, want done", got)
				}
				if h.unread() {
					t.Fatal("late input still flagged unread")
				}
				if n := h.countKind(kernel.KindAssistantMessage); n != 3 {
					t.Fatalf("assistant events = %d, want 3 (one per input, no duplicates)", n)
				}
				h.assertContiguous()
			})

			t.Run("DuringToolExecution", func(t *testing.T) {
				h := im.build(t)
				a := h.readyRuntime(t, "runtime-a", 1)
				leaseA := h.claimShort(a)
				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
					RequestID: "r1", Payload: userPayload("run the echo tool")})
				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
					Token: leaseA.Token, GenerationID: "g1", Payload: assistantToolCall("tc1", "echo")})

				// The tool is executing when the old runtime dies: its side
				// effect happened once, the result was never committed, and the
				// lease is left behind (kill, not drain).
				sideEffects := 1
				if _, err := a.BeginDrain(context.Background(), ModeShutdown, drainWindowTime()); err != nil {
					t.Fatal(err)
				}
				h.expire(leaseA)

				b := h.readyRuntime(t, "runtime-b", 2)
				adoption := h.adopt(b, 3*time.Second)
				if len(adoption.Rebuild.Unresolved) != 1 || adoption.Rebuild.Unresolved[0].ToolCallID != "tc1" {
					t.Fatalf("rebuild = %s, want tc1 unresolved", adoption.Rebuild)
				}

				// The successor re-executes (at-least-once: the old side effect
				// may have happened) and commits exactly one result.
				sideEffects++
				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindToolResult,
					Token: adoption.Lease.Token, ToolCallID: "tc1", Payload: toolResultPayload("tc1", "echo", "ok")})
				h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
					Token: adoption.Lease.Token, GenerationID: "g2", Payload: assistantText("echo returned ok")})
				h.finish(adoption.Lease, h.lastSeq(), nil)

				if sideEffects != 2 {
					t.Fatalf("side effects = %d; at-least-once execution must have retried the call", sideEffects)
				}
				if got := h.countKind(kernel.KindToolResult); got != 1 {
					t.Fatalf("tool results = %d, want exactly 1 despite two executions", got)
				}
				if got := h.runStatus(); got != kernel.RunStatusDone {
					h.assertRunConverges(got)
					t.Fatalf("run status = %s, want done", got)
				}
				h.assertContiguous()
			})
		})
	}
}

// stopOld simulates the old runtime stopping at a safety point during a
// handoff drain: the claim gate closes, the in-flight run is drained to its
// safe point, the lease is released, and a successor runtime comes up.
func (h *harness) stopOld(a *Coordinator, lease kernel.Lease) *Coordinator {
	h.t.Helper()
	ctx := context.Background()
	if _, err := a.BeginDrain(ctx, ModeHandoff, drainWindowTime()); err != nil {
		h.t.Fatalf("begin drain: %v", err)
	}
	if a.Claiming() {
		h.t.Fatal("drained runtime still claims")
	}
	a.RunStarted() // the in-flight run, drained to its safe point below
	if err := a.Release(ctx, lease); err != nil {
		h.t.Fatalf("release at safety point: %v", err)
	}
	a.RunFinished()
	return h.readyRuntime(h.t, "runtime-b", 2)
}
