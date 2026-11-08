package main

import "testing"

// TestSplitBookkeepingKeepsTheConversation pins the normalization the R1 gate
// relies on: bookkeeping events leave the indexed conversation (so an additive
// fact does not shift every later path), the conversation keeps its order and
// payloads, and the caller's raw events are not mutated (the SSE cursor still
// needs the real seq).
func TestSplitBookkeepingKeepsTheConversation(t *testing.T) {
	events := []map[string]any{
		{"seq": float64(1), "kind": "user.message", "payload": map[string]any{"text": "one"}},
		{"seq": float64(2), "kind": "run.finished", "payload": map[string]any{
			"status": "done", "stopReason": "end_turn", "turns": float64(1), "durationMs": float64(9),
		}},
		{"seq": float64(3), "kind": "assistant.message", "payload": map[string]any{"role": "assistant"}},
		{"seq": float64(4), "kind": "run.started", "payload": map[string]any{"attempt": float64(1)}},
	}
	conversation, bookkeeping := splitBookkeeping(events)
	if len(conversation) != 2 {
		t.Fatalf("conversation = %d events, want 2", len(conversation))
	}
	if conversation[0]["kind"] != "user.message" || conversation[1]["kind"] != "assistant.message" {
		t.Fatalf("conversation order = %v", conversation)
	}
	if conversation[0]["seq"] != float64(1) || conversation[1]["seq"] != float64(2) {
		t.Fatalf("seqs were not renumbered: %v %v", conversation[0]["seq"], conversation[1]["seq"])
	}
	if len(bookkeeping) != 2 {
		t.Fatalf("bookkeeping = %d facts, want 2", len(bookkeeping))
	}
	if bookkeeping[0]["kind"] != "run.finished" || bookkeeping[0]["status"] != "done" ||
		bookkeeping[0]["stopReason"] != "end_turn" || bookkeeping[0]["turns"] != float64(1) {
		t.Fatalf("bookkeeping fact = %v", bookkeeping[0])
	}
	if _, ok := bookkeeping[0]["durationMs"]; ok {
		t.Fatal("volatile measurement leaked into the pinned bookkeeping fact")
	}
	// The raw slice keeps the real seq for the SSE cursor.
	if events[2]["seq"] != float64(3) {
		t.Fatalf("raw event mutated: seq = %v", events[2]["seq"])
	}
}

// TestNormalizeFramesRenumbers pins the SSE half: bookkeeping frames are
// dropped and the remaining ids are positional, so both sides of the
// differential compare the same frame sequence.
func TestNormalizeFramesRenumbers(t *testing.T) {
	frames := []sseFrame{
		{ID: "5", Event: "assistant.message"},
		{ID: "6", Event: "run.finished"},
		{ID: "7", Event: "tool.result"},
	}
	out := normalizeFrames(frames)
	if len(out) != 2 {
		t.Fatalf("frames = %d, want 2", len(out))
	}
	if out[0].ID != "1" || out[1].ID != "2" {
		t.Fatalf("ids = %q %q, want positional 1,2", out[0].ID, out[1].ID)
	}
	if out[0].Event != "assistant.message" || out[1].Event != "tool.result" {
		t.Fatalf("order = %+v", out)
	}
}
