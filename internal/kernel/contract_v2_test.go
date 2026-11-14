package kernel

import (
	"encoding/json"
	"testing"
)

// TestSystemLaneAndTaskInitialCard pins the v2 dual-lane task contract: the
// initial card is a system fact (no run exists yet), while a task update during
// a run stays owner-scoped. A pure system kind accepts neither a token nor a
// request without the explicit system marker.
func TestSystemLaneAndTaskInitialCard(t *testing.T) {
	for _, im := range implementations(t) {
		im := im
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)

			// A pure system kind outside the system lane is refused.
			if err := h.commitErr(CommitRequest{Kind: KindNotificationAck,
				NotificationAckKey: "n1:c1", Payload: json.RawMessage(`{"consumerId":"c1"}`)}); !iso(err, ErrInvalidRequest) {
				t.Fatalf("notification.ack without system lane = %v, want invalid-request", err)
			}

			// Initial card: system lane, no lease.
			init := h.mustCommit(CommitRequest{Kind: KindTaskCreated, System: true,
				TaskEventKey: "session:init", Payload: json.RawMessage(`{"title":"资源占用分析"}`)})
			if init.Kind != KindTaskCreated || init.Deduped {
				t.Fatalf("initial task card = %+v", init)
			}

			lease := h.claimScope(h.scope, "runtime-a")

			// The system lane never takes the fence.
			if err := h.commitErr(CommitRequest{Kind: KindNotificationAck, System: true,
				Token: lease.Token, NotificationAckKey: "n1:c1",
				Payload: json.RawMessage(`{"consumerId":"c1"}`)}); !iso(err, ErrInvalidRequest) {
				t.Fatalf("system lane with token = %v, want invalid-request", err)
			}

			// A task update during a run is owner-scoped.
			if err := h.commitErr(CommitRequest{Kind: KindTaskUpdated,
				TaskEventKey: "t1:updated:2", Payload: json.RawMessage(`{"status":"in_progress"}`)}); !iso(err, ErrMissingToken) {
				t.Fatalf("task update without fence = %v, want missing-token", err)
			}
			upd := h.mustCommit(CommitRequest{Kind: KindTaskUpdated, Token: lease.Token,
				TaskEventKey: "t1:updated:2", Payload: json.RawMessage(`{"status":"in_progress"}`)})
			if upd.Deduped || upd.Seq <= init.Seq {
				t.Fatalf("task update = %+v", upd)
			}

			// The task kinds are dual-lane: the same kind also accepts the
			// system lane for harness-created cards.
			h.mustCommit(CommitRequest{Kind: KindTaskNote, System: true,
				TaskEventKey: "t1:note:3", Payload: json.RawMessage(`{"text":"harness note"}`)})
		})
	}
}

// TestFSWriteDoesNotDedupeAgainstToolResult is the negative control the v2
// contract requires: fs.write for tool call "call-1" and the tool.result of
// that same call must occupy different idempotency spaces.
func TestFSWriteDoesNotDedupeAgainstToolResult(t *testing.T) {
	for _, im := range implementations(t) {
		im := im
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			lease := h.claimScope(h.scope, "runtime-a")

			assistant := json.RawMessage(`{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"bash"}]}`)
			h.mustCommit(CommitRequest{Kind: KindAssistantMessage, Token: lease.Token,
				GenerationID: "gen-1", Payload: assistant})

			result := json.RawMessage(`{"toolCallId":"call-1","toolName":"bash","content":[]}`)
			tool := h.mustCommit(CommitRequest{Kind: KindToolResult, Token: lease.Token,
				ToolCallID: "call-1", Payload: result})

			written := json.RawMessage(`{"path":"src/a","hash":"sha256:abc","mode":420}`)
			write := h.mustCommit(CommitRequest{Kind: KindFSWrite, Token: lease.Token,
				FSWriteKey: "call-1:src/a", Payload: written})
			if write.Deduped || write.Seq <= tool.Seq {
				t.Fatalf("fs.write = %+v, tool.result seq = %d", write, tool.Seq)
			}

			replay := h.mustCommit(CommitRequest{Kind: KindFSWrite, Token: lease.Token,
				FSWriteKey: "call-1:src/a", Payload: written})
			if !replay.Deduped || replay.Seq != write.Seq {
				t.Fatalf("fs.write replay = %+v, want deduped seq %d", replay, write.Seq)
			}
		})
	}
}

// TestV2KindsRequireTheirKey pins the namespace table: every v2 kind that
// names a key refuses a commit that omits it, before any payload validation.
func TestV2KindsRequireTheirKey(t *testing.T) {
	for _, im := range implementations(t) {
		im := im
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			lease := h.claimScope(h.scope, "runtime-a")

			cases := []struct {
				name string
				req  CommitRequest
			}{
				{"task.created", CommitRequest{Kind: KindTaskCreated, System: true, Payload: json.RawMessage(`{}`)}},
				{"fs.write", CommitRequest{Kind: KindFSWrite, Token: lease.Token, Payload: json.RawMessage(`{}`)}},
				{"fs.capture", CommitRequest{Kind: KindFSCapture, Token: lease.Token, Payload: json.RawMessage(`{}`)}},
				{"fs.capture_failed", CommitRequest{Kind: KindFSCaptureFailed, System: true, Payload: json.RawMessage(`{}`)}},
				{"config.rotated", CommitRequest{Kind: KindConfigRotated, System: true, Payload: json.RawMessage(`{}`)}},
				{"placement.joined", CommitRequest{Kind: KindPlacementJoined, System: true, Payload: json.RawMessage(`{}`)}},
				{"placement.left", CommitRequest{Kind: KindPlacementLeft, System: true, Payload: json.RawMessage(`{}`)}},
				{"supervision.escalated", CommitRequest{Kind: KindSupervisionEscalated, System: true, Payload: json.RawMessage(`{}`)}},
				{"guardrail.tripped", CommitRequest{Kind: KindGuardrailTripped, System: true, Payload: json.RawMessage(`{}`)}},
				{"resume.attempt", CommitRequest{Kind: KindResumeAttempt, System: true, Payload: json.RawMessage(`{}`)}},
				{"notification.ack", CommitRequest{Kind: KindNotificationAck, System: true, Payload: json.RawMessage(`{}`)}},
				{"notification.push_sent", CommitRequest{Kind: KindNotificationPushSent, System: true, Payload: json.RawMessage(`{}`)}},
				{"run.finished", CommitRequest{Kind: KindRunFinished, Token: lease.Token, Payload: json.RawMessage(`{}`)}},
			}
			for _, c := range cases {
				if err := h.commitErr(c.req); !iso(err, ErrKeyKindMismatch) {
					t.Errorf("%s without its key = %v, want key-kind-mismatch", c.name, err)
				}
			}
		})
	}
}
