package fileplugin_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/kernel"
)

// TestFSWriteFollowsToolResultWithoutKeyCollision is the negative control the
// v2 contract requires: fs.write for tool call "call-1" and the tool.result of
// that same call must occupy different idempotency spaces, and the fs.write
// event must be able to follow the tool.result without deduping against it.
func TestFSWriteFollowsToolResultWithoutKeyCollision(t *testing.T) {
	h := newHarness(t)
	scope := kernel.Scope{TenantID: h.scope.TenantID, SessionID: h.scope.SessionID}
	assistant := json.RawMessage(`{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"write"}]}`)
	if _, err := h.kernel.Commit(h.ctx, kernel.CommitRequest{
		Scope: scope, Kind: kernel.KindAssistantMessage, Token: h.lease.Token, GenerationID: "gen-1", Payload: assistant,
	}); err != nil {
		t.Fatal(err)
	}
	toolResult, err := h.kernel.Commit(h.ctx, kernel.CommitRequest{
		Scope: scope, Kind: kernel.KindToolResult, Token: h.lease.Token, ToolCallID: "call-1",
		Payload: json.RawMessage(`{"toolCallId":"call-1","toolName":"write","content":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	written := h.write(t, "call-1", "src/a.txt", "hello")
	if written.Seq <= toolResult.Seq {
		t.Fatalf("fs.write seq %d did not follow tool.result seq %d", written.Seq, toolResult.Seq)
	}
	// The two idempotency spaces are distinct.
	toolLookup, found, err := h.kernel.Lookup(h.ctx, scope, kernel.KeyRef{Kind: kernel.KeyToolCall, Value: "call-1"})
	if err != nil || !found || toolLookup.Seq != toolResult.Seq {
		t.Fatalf("tool lookup = %+v/%v/%v", toolLookup, found, err)
	}
	writeLookup, found, err := h.kernel.Lookup(h.ctx, scope, kernel.KeyRef{Kind: kernel.KeyFSWrite, Value: "call-1:src/a.txt"})
	if err != nil || !found || writeLookup.Seq != written.Seq {
		t.Fatalf("fs.write lookup = %+v/%v/%v", writeLookup, found, err)
	}
	if toolLookup.Seq == writeLookup.Seq {
		t.Fatal("tool.result and fs.write share a sequence")
	}
	// A replay of the same tool call dedupes to the first fs.write event.
	replay, err := h.service.Write(h.call("call-1"), mustJSON(t, map[string]any{"path": "src/a.txt", "content": "hello"}))
	if err != nil || !replay.Replayed || replay.Seq != written.Seq {
		t.Fatalf("replay = %+v/%v", replay, err)
	}
}

// TestCaptureCommitsExactlyOneBatchEvent pins the capture contract: a snapshot
// of N files is ONE fs.capture event, and the manifest rows carry that event's
// sequence.
func TestCaptureCommitsExactlyOneBatchEvent(t *testing.T) {
	h := newHarness(t)
	h.write(t, "call-w1", "a.txt", "alpha")
	h.write(t, "call-w2", "dir/b.txt", "beta")
	h.write(t, "call-w3", "dir/c.txt", "alpha") // same bytes as a.txt: one object, two paths

	before := len(h.events(t))
	capture, err := h.service.CaptureWorkspace(h.ctx, h.scope, "call-c1", 0, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if capture.Count != 3 || capture.Seq == 0 {
		t.Fatalf("capture = %+v", capture)
	}
	events := h.events(t)
	captures := 0
	var captureEvent kernel.Event
	for _, event := range events[before:] {
		if event.Kind == kernel.KindFSCapture {
			captures++
			captureEvent = event
		}
	}
	if captures != 1 {
		t.Fatalf("fs.capture events = %d, want exactly 1 (events=%d)", captures, len(events)-before)
	}
	if captureEvent.Seq != capture.Seq {
		t.Fatalf("capture event seq = %d, result = %d", captureEvent.Seq, capture.Seq)
	}
	var payload map[string]any
	if err := json.Unmarshal(captureEvent.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["count"].(float64) != 3 {
		t.Fatalf("capture payload = %v", payload)
	}
	entries, err := h.manifest.List(h.ctx, h.scope.TenantID, h.scope.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("manifest rows = %d", len(entries))
	}
	for _, entry := range entries {
		if entry.UpdatedSeq != capture.Seq {
			t.Fatalf("manifest %s updated_seq = %d, want %d", entry.Path, entry.UpdatedSeq, capture.Seq)
		}
		if entry.LastTaskID != "task-1" {
			t.Fatalf("manifest %s last_task_id = %q", entry.Path, entry.LastTaskID)
		}
	}
	// Both paths with identical bytes point at one object. Its refcount is the
	// number of distinct (session, eventSeq) references: two explicit writes plus
	// the capture event.
	if entries[0].Hash != entries[2].Hash {
		t.Fatalf("identical content hashes differ: %s vs %s", entries[0].Hash, entries[2].Hash)
	}
	if refs := h.store.RefCount(entries[0].Hash); refs != 3 {
		t.Fatalf("refcount = %d, want 3 (two fs.write events + one fs.capture)", refs)
	}
	tree, ok, err := h.manifest.Snapshot(h.ctx, h.scope.TenantID, h.scope.SessionID, capture.Seq)
	if err != nil || !ok {
		t.Fatalf("snapshot = %s/%v/%v", tree, ok, err)
	}
	if tree.Prefixed() != capture.Tree {
		t.Fatalf("snapshot tree = %s, capture tree = %s", tree.Prefixed(), capture.Tree)
	}
	// Replaying the same capture key appends nothing.
	replay, err := h.service.CaptureWorkspace(h.ctx, h.scope, "call-c1", 0, "task-1")
	if err != nil || !replay.Replayed || replay.Seq != capture.Seq {
		t.Fatalf("capture replay = %+v/%v", replay, err)
	}
	if got := len(h.events(t)); got != len(events) {
		t.Fatalf("capture replay appended events: %d -> %d", len(events), got)
	}
}

func TestCaptureFailureIsOneSystemEvent(t *testing.T) {
	h := newHarness(t)
	broken, err := fileplugin.New(fileplugin.Options{
		Store:       h.store,
		Manifest:    h.manifest,
		Committer:   h.kernel,
		Leases:      h.kernel,
		CaptureRoot: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.CaptureWorkspace(h.ctx, h.scope, "call-bad", 7, ""); fileplugin.CodeOf(err) != fileplugin.CodeIO {
		t.Fatalf("capture failure = %v", err)
	}
	events := h.events(t)
	failed, captures := 0, 0
	for _, event := range events {
		switch event.Kind {
		case kernel.KindFSCaptureFailed:
			failed++
		case kernel.KindFSCapture:
			captures++
		}
	}
	if failed != 1 || captures != 0 {
		t.Fatalf("capture_failed = %d, capture = %d", failed, captures)
	}
}

// TestCaptureOneHundredThousandFilesIsOneEvent is the scale gate: an install
// that writes 100k files must not produce 100k ledger events.
func TestCaptureOneHundredThousandFilesIsOneEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("100k-file capture is the scale gate; skipped in -short")
	}
	h := newHarness(t)
	root := t.TempDir()
	// One flat directory keeps inode creation cheap; identical content proves
	// dedupe at the same time.
	for i := 0; i < 100_000; i++ {
		name := filepath.Join(root, fmt.Sprintf("f%06d", i))
		file, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("x")); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	service, err := fileplugin.New(fileplugin.Options{
		Store:       h.store,
		Manifest:    h.manifest,
		Committer:   h.kernel,
		Leases:      h.kernel,
		CaptureRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	capture, err := service.CaptureWorkspace(h.ctx, h.scope, "call-100k", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if capture.Count != 100_000 {
		t.Fatalf("capture count = %d", capture.Count)
	}
	if capture.NewObjects != 1 {
		t.Fatalf("new objects = %d, want 1 (identical bytes)", capture.NewObjects)
	}
	captures := 0
	for _, event := range h.events(t) {
		if event.Kind == kernel.KindFSCapture {
			captures++
		}
	}
	if captures != 1 {
		t.Fatalf("fs.capture events = %d, want exactly 1", captures)
	}
	entries, err := h.manifest.List(h.ctx, h.scope.TenantID, h.scope.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 100_000 {
		t.Fatalf("manifest rows = %d", len(entries))
	}
	if refs := h.store.RefCount(entries[0].Hash); refs != 1 {
		t.Fatalf("refcount = %d, want 1", refs)
	}
}
