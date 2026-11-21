package auth

import (
	"strings"
	"testing"
	"time"
)

func TestLimiterUsernameAndIPDimensions(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	limiter := NewLimiter()
	limiter.SetClock(func() time.Time { return now })

	for i := 0; i < limiterFreeAttempts; i++ {
		if wait := limiter.Check("Alice", "10.0.0.1"); wait != 0 {
			t.Fatalf("free attempt %d locked for %s", i+1, wait)
		}
		if wait := limiter.Failure("Alice", "10.0.0.1"); wait != 0 {
			t.Fatalf("free failure %d locked for %s", i+1, wait)
		}
	}
	if wait := limiter.Failure("alice", "10.0.0.1"); wait < time.Second || wait > 2*time.Second {
		t.Fatalf("fourth failure wait = %s, want about 1s", wait)
	}
	// Case changes do not create a new username bucket, and the username lock
	// applies even from a fresh IP.
	if wait := limiter.Check("ALICE", "10.0.0.1"); wait <= 0 {
		t.Fatal("case-folded username is not locked")
	}
	if wait := limiter.Check("alice", "10.0.0.2"); wait <= 0 {
		t.Fatal("username dimension did not lock a fresh IP")
	}
	// The lock expires with the clock.
	now = now.Add(2 * time.Second)
	if wait := limiter.Check("alice", "10.0.0.1"); wait != 0 {
		t.Fatalf("expired lock still waits %s", wait)
	}
	// A successful login for this username clears the username dimension, but
	// only decrements the shared IP.
	limiter.Success("alice", "10.0.0.1")
	if wait := limiter.Check("alice", "10.0.0.2"); wait != 0 {
		t.Fatalf("username bucket survived success: %s", wait)
	}
}

func TestLimiterIPDimensionLocksAcrossUsernames(t *testing.T) {
	now := time.Now().UTC()
	limiter := NewLimiter()
	limiter.SetClock(func() time.Time { return now })
	for i := 0; i < limiterFreeAttempts; i++ {
		limiter.Failure("user-"+string(rune('a'+i)), "192.0.2.9")
	}
	wait := limiter.Failure("user-z", "192.0.2.9")
	if wait <= 0 {
		t.Fatal("IP dimension did not lock after repeated failures")
	}
	if got := limiter.Check("brand-new-user", "192.0.2.9"); got <= 0 {
		t.Fatal("a new username from the locked IP was allowed")
	}
}

func TestLimiterBackoffCapsAtFifteenMinutes(t *testing.T) {
	now := time.Now().UTC()
	limiter := NewLimiter()
	limiter.SetClock(func() time.Time { return now })
	var last time.Duration
	for i := 0; i < 40; i++ {
		last = limiter.Failure("spray", "203.0.113.7")
	}
	if last != limiterMaxDelay {
		t.Fatalf("cap = %s, want %s", last, limiterMaxDelay)
	}
	if last > 15*time.Minute {
		t.Fatalf("backoff exceeded 15m: %s", last)
	}
}

func TestLimiterForgetsIdleDimensions(t *testing.T) {
	now := time.Now().UTC()
	limiter := NewLimiter()
	limiter.SetClock(func() time.Time { return now })
	for i := 0; i < limiterFreeAttempts+1; i++ {
		limiter.Failure("stale", "")
	}
	now = now.Add(limiterFailureWindow + time.Minute)
	if wait := limiter.Check("stale", ""); wait != 0 {
		t.Fatalf("idle dimension still waits %s", wait)
	}
}

func TestRateLimitedError(t *testing.T) {
	err := &RateLimitedError{RetryAfter: 90 * time.Second}
	if !strings.Contains(err.Error(), "1m30s") {
		t.Fatalf("error text = %q", err.Error())
	}
}
