package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/kernel"
)

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

// v2Kinds is the frozen vocabulary the R1 classifier must account for. It is
// spelled out here (instead of reflecting over the kernel) so adding a kind
// without classifying it fails this test.
var v2Kinds = []kernel.EventKind{
	kernel.KindTaskCreated, kernel.KindTaskUpdated, kernel.KindTaskAttached,
	kernel.KindTaskDetached, kernel.KindTaskClosed, kernel.KindTaskNote,
	kernel.KindFSWrite, kernel.KindFSCapture, kernel.KindFSCaptureFailed,
	kernel.KindConfigRotated,
	kernel.KindPlacementJoined, kernel.KindPlacementLeft,
	kernel.KindSupervisionEscalated, kernel.KindGuardrailTripped, kernel.KindResumeAttempt,
	kernel.KindNotificationAck, kernel.KindNotificationPushSent,
}

func TestEveryV2KindIsBookkeepingWithAReason(t *testing.T) {
	for _, kind := range v2Kinds {
		reason, ok := bookkeepingReasons[string(kind)]
		if !ok {
			t.Errorf("v2 kind %q is not classified as bookkeeping; adding it would shift every later conversation seq", kind)
			continue
		}
		if len(strings.TrimSpace(reason)) < 20 {
			t.Errorf("v2 kind %q has no substantive reason (%q)", kind, reason)
		}
	}
}

func TestRunKindsAreClassified(t *testing.T) {
	for _, kind := range []string{"run.started", "run.failed", "run.finished"} {
		if !bookkeepingKinds[kind] {
			t.Errorf("%s must be bookkeeping", kind)
		}
	}
}

func TestSplitBookkeepingCapturesRunSnapshotFields(t *testing.T) {
	events := []map[string]any{{
		"kind": "run.finished",
		"payload": map[string]any{
			"status": "done", "stopReason": "end_turn", "turns": float64(2),
			"configHashBefore": "sha256:before", "configHashAfter": "sha256:after",
			"anchorEventSeq": float64(4), "toolManifestHash": "sha256:manifest",
			"durationMs": float64(123), // volatile: must not be pinned
		},
	}}
	_, bookkeeping := splitBookkeeping(events)
	if len(bookkeeping) != 1 {
		t.Fatalf("bookkeeping = %v", bookkeeping)
	}
	fact := bookkeeping[0]
	for field, want := range map[string]any{
		"configHashBefore": "sha256:before", "configHashAfter": "sha256:after",
		"anchorEventSeq": float64(4), "toolManifestHash": "sha256:manifest",
	} {
		if got := fact[field]; got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
	if _, ok := fact["durationMs"]; ok {
		t.Error("volatile durationMs must not be pinned in bookkeeping")
	}
}

func TestLoadPin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	valid := `{
	  "schema": "keel.r1diff.pin/1",
	  "scenario": "r1-basic-v2",
	  "ref": "e7361f8",
	  "allowlist": "cmd/r1diff/allow-v2.json",
	  "allowRules": 3,
	  "reason": "pre-v2 reference"
	}`
	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	pin, err := loadPin(path)
	if err != nil {
		t.Fatalf("valid pin: %v", err)
	}
	if pin.Ref != "e7361f8" || pin.AllowRules != 3 {
		t.Fatalf("pin = %+v", pin)
	}
	for name, mutate := range map[string]func(string) string{
		"wrong schema":   func(s string) string { return strings.Replace(s, "keel.r1diff.pin/1", "keel.r1diff.pin/2", 1) },
		"wrong scenario": func(s string) string { return strings.Replace(s, "r1-basic-v2", "r1-basic-v1", 1) },
		"no ref":         func(s string) string { return strings.Replace(s, `"ref": "e7361f8"`, `"ref": ""`, 1) },
		"no rules":       func(s string) string { return strings.Replace(s, `"allowRules": 3`, `"allowRules": 0`, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(dir, "bad-"+strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(bad, []byte(mutate(valid)), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := loadPin(bad); err == nil {
				t.Fatalf("%s must be rejected", name)
			}
		})
	}
}

// TestV2ProbesCoverEveryFamily keeps the scenario honest: one probe per v2
// family and every probe carries a payload.
func TestV2ProbesCoverEveryFamily(t *testing.T) {
	wantFamilies := []string{"task.", "fs.", "config.", "placement.", "supervision.", "guardrail.", "resume.", "notification."}
	seen := map[string]bool{}
	for _, probe := range v2KindProbes {
		for _, family := range wantFamilies {
			if strings.HasPrefix(probe.Kind, family) {
				seen[family] = true
			}
		}
		if len(probe.Payload) == 0 {
			t.Errorf("probe %s has no payload", probe.Kind)
		}
	}
	for _, family := range wantFamilies {
		if !seen[family] {
			t.Errorf("no v2 probe covers the %s family", family)
		}
	}
}
