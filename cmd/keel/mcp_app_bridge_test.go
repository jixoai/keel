package main

// The production adapter over a live MCP connection (P3.1/P3.2): AppTool is the
// ownership gate the HTTP bridge relies on, CallTool reaches the real stdio
// fixture, and ReadResource is scoped to the named server. This is the seam the
// httptest-level fake cannot prove.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

func TestMCPAppRuntimeAdapterOverLiveManager(t *testing.T) {
	harness := newMCPSurfaceHarness(t)
	runtimeInterface := mcpAppRuntimeOrNil(harness.rt)
	if runtimeInterface == nil {
		t.Fatal("the booted runtime has no MCP manager")
	}
	runtime := runtimeInterface.(mcpAppRuntime)

	tool, ok := runtime.AppTool("promptfixture", "fixture_echo")
	if !ok {
		t.Fatal("the fixture's app-visible tool did not resolve")
	}
	if tool.Name != "fixture_echo" || tool.Destructive {
		t.Fatalf("tool = %+v", tool)
	}
	if _, ok := runtime.AppTool("promptfixture", "no_such_tool"); ok {
		t.Fatal("an unknown tool resolved")
	}
	if _, ok := runtime.AppTool("another-server", "fixture_echo"); ok {
		t.Fatal("a tool resolved under another server's name")
	}

	ctx := context.Background()
	result, err := runtime.CallTool(ctx, "promptfixture", "fixture_echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "fixture-called") {
		t.Fatalf("result = %+v", result)
	}

	read, err := runtime.ReadResource(ctx, "promptfixture", "ui://fixture/panel")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(read.Contents) != 1 || read.Contents[0].MimeType != "text/html;profile=mcp-app" {
		t.Fatalf("resource = %+v", read)
	}
	if _, err := runtime.ReadResource(ctx, "another-server", "ui://fixture/panel"); err == nil {
		t.Fatal("a resource read resolved under another server's name")
	}
}

// TestMCPAppInputCommitWritesUserMessage pins the ui/message send branch's
// write path: the seam commits a user.message with delivery=queue through
// httpapi.CommitClientInput, and a replayed requestId is idempotent.
func TestMCPAppInputCommitWritesUserMessage(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "mcp-app-input-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}

	mem := kernel.NewMemStore()
	mem.CreateSession(tenant, session)
	k := kernel.New(mem, kernel.Options{})
	commit := mcpAppInputCommit(st, k)

	requestID := "mcp-app:mcpapp_0123456789abcdef:frame-1"
	input, err := commit(ctx, tenant, session, requestID, "hello from the app", nil)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !input.Created || input.RequestID != requestID || input.SubmissionID == "" {
		t.Fatalf("commit = %+v", input)
	}

	events, err := k.LoadEvents(ctx, kernel.Scope{TenantID: tenant, SessionID: session}, 0, 10)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	if len(events) != 1 || events[0].Kind != kernel.KindUserMessage {
		t.Fatalf("events = %+v, want one user.message", events)
	}
	var payload struct {
		Text      string `json:"text"`
		RequestID string `json:"requestId"`
		Delivery  string `json:"delivery"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Text != "hello from the app" || payload.RequestID != requestID || payload.Delivery != kernel.DeliveryQueue {
		t.Fatalf("payload = %+v", payload)
	}

	// A replay of the same frame is one fact, not two.
	replay, err := commit(ctx, tenant, session, requestID, "hello from the app", nil)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Created {
		t.Fatalf("replay created a second submission: %+v", replay)
	}
	events, err = k.LoadEvents(ctx, kernel.Scope{TenantID: tenant, SessionID: session}, 0, 10)
	if err != nil {
		t.Fatalf("load events after replay: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events after replay = %d, want 1", len(events))
	}
}
