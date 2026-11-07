// Tests for the runtime's MCP OAuth housekeeping and challenge visibility.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/mcp/oauth"
)

type fakePendingPruner struct {
	calls atomic.Int32
	rows  int64
	err   error
}

func (f *fakePendingPruner) PruneMCPOAuthPending(context.Context, time.Time) (int64, error) {
	f.calls.Add(1)
	return f.rows, f.err
}

// TestRunMCPOAuthPendingPruner checks the runtime wiring: a sweep at boot, then
// one per interval, and a clean stop on cancellation.
func TestRunMCPOAuthPendingPruner(t *testing.T) {
	pruner := &fakePendingPruner{rows: 2}
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runMCPOAuthPendingPruner(ctx, pruner, 5*time.Millisecond, log)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for pruner.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := pruner.calls.Load(); got < 2 {
		t.Fatalf("prune calls = %d, want at least 2 (boot + ticker)", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pruner did not stop after cancellation")
	}
	if !strings.Contains(out.String(), "mcp oauth pending pruned") {
		t.Errorf("log = %q, want the prune summary", out.String())
	}
}

// TestRunMCPOAuthPendingPrunerLogsFailure keeps housekeeping non-fatal: a
// failing sweep is logged and the loop survives for the next tick.
func TestRunMCPOAuthPendingPrunerLogsFailure(t *testing.T) {
	pruner := &fakePendingPruner{err: errors.New("database unreachable")}
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runMCPOAuthPendingPruner(ctx, pruner, 5*time.Millisecond, log)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for pruner.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := pruner.calls.Load(); got < 2 {
		t.Fatalf("prune calls after a failure = %d, want the loop to continue", got)
	}
	cancel()
	<-done
	if !strings.Contains(out.String(), "mcp oauth pending prune") {
		t.Errorf("log = %q, want the prune failure", out.String())
	}
}

// TestLogMCPChallenge pins the challenge visibility: the OAuth error, scope,
// and resource metadata URL reach the log, and no credential field is emitted.
func TestLogMCPChallenge(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&out, nil))
	logMCPChallenge(log)("demo", oauth.Challenge{
		Error:               "insufficient_scope",
		Scope:               "repo",
		ResourceMetadataURL: "https://mcp.example/.well-known/oauth-protected-resource",
		ErrorDescription:    "needs more scope",
	})
	var record map[string]any
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatalf("decode log line %q: %v", out.String(), err)
	}
	if record["msg"] != "mcp oauth challenge" {
		t.Errorf("msg = %v", record["msg"])
	}
	if record["server"] != "demo" || record["error"] != "insufficient_scope" || record["scope"] != "repo" {
		t.Errorf("challenge fields = %v", record)
	}
	if record["resource_metadata_url"] != "https://mcp.example/.well-known/oauth-protected-resource" {
		t.Errorf("resource metadata URL = %v", record["resource_metadata_url"])
	}
	for _, forbidden := range []string{"access_token", "refresh_token", "client_secret", "code_verifier"} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("challenge log contains %q: %s", forbidden, out.String())
		}
	}
}
