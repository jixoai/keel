package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/superagent"
)

// TestWriterLeaseHealthDetectsATerminatedBackend is the P1-3 regression with a
// real PostgreSQL server: a lease whose backend is terminated by a restart or
// failover must report unhealthy (so Run stops writing), and the advisory lock
// is released with that backend so a second process can take over.
func TestWriterLeaseHealthDetectsATerminatedBackend(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	first, err := Open(ctx, Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	t.Cleanup(first.Close)
	second, err := Open(ctx, Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(second.Close)

	tenant := uuid.New()
	lease, err := first.AcquireWriterLease(ctx, tenant)
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	if _, err := second.AcquireWriterLease(ctx, tenant); !errors.Is(err, superagent.ErrWriterLeaseHeld) {
		t.Fatalf("second lease while healthy = %v, want ErrWriterLeaseHeld", err)
	}
	if err := lease.Healthy(ctx); err != nil {
		t.Fatalf("healthy lease reported unhealthy: %v", err)
	}
	held := lease.(*superagentWriterLease)
	var pid int
	if err := held.conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read lease backend pid: %v", err)
	}
	// Simulate PostgreSQL restart/failover killing the owning session.
	if _, err := second.pool.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatalf("terminate lease backend: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := lease.Healthy(ctx); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Healthy stayed green after the lease backend was terminated")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The session-level advisory lock died with the backend; a second process
	// can acquire the tenant (the first Run has stopped by then).
	deadline = time.Now().Add(5 * time.Second)
	for {
		takeover, err := second.AcquireWriterLease(ctx, tenant)
		if err == nil {
			if err := takeover.Release(ctx); err != nil {
				t.Fatalf("takeover release: %v", err)
			}
			break
		}
		if !errors.Is(err, superagent.ErrWriterLeaseHeld) {
			t.Fatalf("takeover lease: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the tenant stayed locked after its lease backend died")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Releasing the dead lease must not panic; it may report the lost connection.
	_ = lease.Release(ctx)
}
