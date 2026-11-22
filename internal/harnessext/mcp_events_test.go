// MCP event surface in the extension layer (P4.5): progress is a transient
// partial frame bound to a toolCallId, the hook event name is appended to the
// closed set, and neither touches the ledger.

package harnessext

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/mcp"
)

func TestPartialHubPublishesMCPProgressPerSession(t *testing.T) {
	hub := NewPartialHub()
	tenant := uuid.New()
	sessionA, sessionB := uuid.New(), uuid.New()
	subA, cancelA := hub.Subscribe(tenant, sessionA)
	defer cancelA()
	subB, cancelB := hub.Subscribe(tenant, sessionB)
	defer cancelB()

	total := 10.0
	if !hub.PublishMCPProgress(tenant, sessionA, "call-1", mcp.ProgressNotification{
		Progress: 3, Total: &total, Message: "halfway",
	}) {
		t.Fatal("no subscriber received the progress frame")
	}
	select {
	case frame := <-subA:
		if frame.Type != PartialMCPProgress {
			t.Fatalf("type = %q", frame.Type)
		}
		if frame.MessageID != "call-1" {
			t.Fatalf("messageId = %q, want the toolCallId", frame.MessageID)
		}
		if frame.Progress == nil || *frame.Progress != 3 || frame.Total == nil || *frame.Total != 10 || frame.Message != "halfway" {
			t.Fatalf("frame = %+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("session A never received the frame")
	}
	select {
	case frame := <-subB:
		t.Fatalf("progress crossed sessions: %+v", frame)
	case <-time.After(50 * time.Millisecond):
	}
	// Dropped frames are harmless: an empty toolCallId is ignored, never
	// published to every session.
	if hub.PublishMCPProgress(tenant, sessionA, "", mcp.ProgressNotification{Progress: 1}) {
		t.Fatal("a frame without a toolCallId was published")
	}
}

func TestMCPProgressMessageIsBounded(t *testing.T) {
	hub := NewPartialHub()
	tenant, session := uuid.New(), uuid.New()
	sub, cancel := hub.Subscribe(tenant, session)
	defer cancel()
	long := strings.Repeat("x", 4096) + "\x00\x01\x07" + strings.Repeat("y", 64)
	hub.PublishMCPProgress(tenant, session, "call-1", mcp.ProgressNotification{Progress: 1, Message: long})
	select {
	case frame := <-sub:
		if len(frame.Message) > 256+len("…") {
			t.Fatalf("message is unbounded: %d bytes", len(frame.Message))
		}
		if !strings.HasSuffix(frame.Message, "…") {
			t.Fatalf("message was not truncated: %q", frame.Message[:min(len(frame.Message), 40)])
		}
		for _, r := range frame.Message {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("control character %U survived the bound", r)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("no progress frame was published")
	}
}

func TestMCPSurfaceChangedHookEventIsInTheClosedSet(t *testing.T) {
	if !ValidHookEvent(string(HookMCPSurfaceChanged)) {
		t.Fatal("mcp.surface.changed is not a valid hook event")
	}
	found := false
	for _, event := range HookEvents() {
		if event == HookMCPSurfaceChanged {
			found = true
		}
	}
	if !found {
		t.Fatal("the closed set does not list mcp.surface.changed")
	}
}
