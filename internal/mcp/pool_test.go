package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestServerSlotBackoff pins the reconnect backoff schedule and its reset: a
// crashing server is retried at 1s, 2s, 4s ... up to 30s, and a successful
// reconnect clears the failure budget.
func TestServerSlotBackoff(t *testing.T) {
	slot := newServerSlot(time.Minute)
	now := time.Now()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for index, expected := range want {
		if got := slot.recordFailure(errors.New("boom"), now); got != expected {
			t.Fatalf("failure %d delay = %s, want %s", index+1, got, expected)
		}
		if ok, _ := slot.canAttempt(now); ok {
			t.Fatalf("failure %d allowed an immediate attempt", index+1)
		}
		if ok, wait := slot.canAttempt(now.Add(expected)); !ok || wait != 0 {
			t.Fatalf("failure %d: attempt at %s = (ok=%v, wait=%s), want ready", index+1, expected, ok, wait)
		}
		// Advance the clock so the next delay is derived from the new attempt.
		now = now.Add(expected)
	}
	slot.recordSuccess(true, now)
	if !slot.healthy || slot.failures != 0 || !slot.nextAttempt.IsZero() || slot.reconnects != 1 {
		t.Fatalf("slot after success = %+v", slot)
	}
	if ok, _ := slot.canAttempt(now); !ok {
		t.Fatal("a successful reconnect did not clear the backoff")
	}
}

// TestHealthCheckUnhealthyBacksOff pins the borrow contract: a failed probe
// marks the slot unhealthy and the next reconnect is refused with a typed
// backoff error until the deadline.
func TestHealthCheckUnhealthyBacksOff(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("tools/list", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"tools": []any{}}, nil
	})
	client := connectTestClient(t, clientTransport)
	entry := ServerEntry{Name: "fixture"}
	manager := &Manager{
		servers: []*Server{{Entry: entry, Client: client}},
		options: ConnectOptions{},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx := context.Background()

	if err := manager.HealthCheck(ctx, "fixture"); err != nil {
		t.Fatalf("healthy probe: %v", err)
	}
	status := manager.PoolStatus()[0]
	if !status.Healthy || status.Failures != 0 || status.Reconnects != 0 {
		t.Fatalf("healthy status = %+v", status)
	}

	// A closed connection is the deterministic probe failure.
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := manager.HealthCheck(ctx, "fixture"); !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("failed probe = %v, want ErrServerUnavailable", err)
	}
	status = manager.PoolStatus()[0]
	if status.Healthy || status.Failures != 1 || status.NextRetry.IsZero() || status.CheckError == "" {
		t.Fatalf("failed status = %+v", status)
	}

	// EnsureConnected must respect the backoff instead of thrashing.
	err := manager.EnsureConnected(ctx, "fixture")
	var backoff *ReconnectBackoffError
	if !errors.As(err, &backoff) || !errors.Is(err, ErrReconnectBackoff) {
		t.Fatalf("borrow during backoff = %v, want ReconnectBackoffError", err)
	}
}

// TestEnsureConnectedReconnectsAfterBackoff pins the ordering bug this suite
// guards: a failed probe schedules the reconnect, and the next borrow after the
// backoff window actually performs it. Probing again before checking the
// backoff would re-schedule it forever and never reconnect.
func TestEnsureConnectedReconnectsAfterBackoff(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("tools/list", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"tools": []any{}}, nil
	})
	client := connectTestClient(t, clientTransport)
	manager := &Manager{
		servers: []*Server{{Entry: ServerEntry{Name: "fixture"}, Client: client}},
		entries: map[string]ServerEntry{"fixture": {
			Name:   "fixture",
			Config: map[string]any{"command": "/nonexistent-keel-mcp-binary"},
		}},
		options: ConnectOptions{},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx := context.Background()
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	// Force the probe due: a freshly created slot is otherwise "checked just
	// now" and the cheap path would return it without I/O.
	slot := manager.slot("fixture")
	slot.mu.Lock()
	slot.lastCheck = time.Time{}
	slot.mu.Unlock()
	// The failed probe marks the slot unhealthy and schedules the reconnect.
	err := manager.EnsureConnected(ctx, "fixture")
	var backoff *ReconnectBackoffError
	if !errors.As(err, &backoff) {
		t.Fatalf("first borrow = %v, want ReconnectBackoffError", err)
	}
	// Simulate the backoff window elapsing.
	slot.mu.Lock()
	slot.nextAttempt = time.Time{}
	slot.mu.Unlock()

	err = manager.EnsureConnected(ctx, "fixture")
	if err == nil {
		t.Fatal("reconnect to a nonexistent binary succeeded")
	}
	if errors.Is(err, ErrReconnectBackoff) {
		t.Fatalf("second borrow = %v, want the reconnect attempt (not another backoff)", err)
	}
	if !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("second borrow = %v, want ErrServerUnavailable from the reconnect", err)
	}
}

// TestEnsureConnectedUsesHealthySlot pins the cheap path: a freshly connected,
// healthy slot is returned without a reconnect and without a probe.
func TestEnsureConnectedUsesHealthySlot(t *testing.T) {
	clientTransport, server := newTestServer(t)
	client := connectTestClient(t, clientTransport)
	manager := &Manager{
		servers: []*Server{{Entry: ServerEntry{Name: "fixture"}, Client: client}},
		options: ConnectOptions{HealthCheckInterval: time.Hour},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := manager.EnsureConnected(context.Background(), "fixture"); err != nil {
		t.Fatalf("healthy borrow: %v", err)
	}
	if requests := server.requests("tools/list"); len(requests) != 0 {
		t.Fatalf("healthy borrow probed the server %d times", len(requests))
	}
	if status := manager.PoolStatus(); len(status) != 1 || !status[0].Healthy || status[0].Reconnects != 0 {
		t.Fatalf("pool status = %+v", status)
	}
}
