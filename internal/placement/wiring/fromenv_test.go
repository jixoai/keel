package wiring

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// nopCommitter satisfies the wiring's kernel seam; FromEnv only needs the
// interface to construct the gate, never a committed event.
type nopCommitter struct{}

func (nopCommitter) Commit(context.Context, kernel.CommitRequest) (kernel.Receipt, error) {
	return kernel.Receipt{}, nil
}

func (nopCommitter) Lookup(context.Context, kernel.Scope, kernel.KeyRef) (kernel.Receipt, bool, error) {
	return kernel.Receipt{}, false, nil
}

func envConfig() Config {
	return Config{
		Store:    &store.Store{},
		Kernel:   nopCommitter{},
		WorkerID: "env-test",
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func lookup(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// TestFromEnvKeepsDefaultsForEmptyValues pins the compatibility half of the
// contract: an unset or empty variable keeps the documented default (zero =
// unlimited/lease default) instead of becoming a parse error.
func TestFromEnvKeepsDefaultsForEmptyValues(t *testing.T) {
	rt, err := FromEnv(lookup(map[string]string{
		EnvGroupMaxRuns:     "",
		EnvGroupMemoryBytes: " ",
		EnvLeaseMS:          "",
	}), envConfig())
	if err != nil {
		t.Fatalf("FromEnv with empty values: %v", err)
	}
	if rt == nil || rt.Gate == nil || rt.Edge == nil {
		t.Fatalf("runtime = %+v, want a wired gate and edge", rt)
	}
}

// TestFromEnvParsesValidValues is the positive path for the integers. A
// memory-bounded deployment must also wire an estimator: the boot then accepts
// the limit instead of failing the memory guard.
func TestFromEnvParsesValidValues(t *testing.T) {
	cfg := envConfig()
	cfg.MemoryEstimator = func(context.Context, uuid.UUID, uuid.UUID) (int64, error) {
		return 1 << 20, nil
	}
	rt, err := FromEnv(lookup(map[string]string{
		EnvGroupMaxRuns:     "7",
		EnvGroupMemoryBytes: "1073741824",
		EnvLeaseMS:          "2500",
		EnvQueueIntervalMS:  "1500",
	}), cfg)
	if err != nil {
		t.Fatalf("FromEnv with valid values: %v", err)
	}
	if rt == nil {
		t.Fatal("FromEnv returned a nil runtime")
	}
	if rt.QueueInterval != 1500*time.Millisecond {
		t.Fatalf("queue interval = %s, want 1.5s", rt.QueueInterval)
	}
}

// TestFromEnvRejectsMemoryBudgetWithoutEstimator is the misconfiguration guard:
// a memory-bounded group with no estimate source fails the boot by name instead
// of failing every admission at request time.
func TestFromEnvRejectsMemoryBudgetWithoutEstimator(t *testing.T) {
	_, err := FromEnv(lookup(map[string]string{EnvGroupMemoryBytes: "1048576"}), envConfig())
	if err == nil {
		t.Fatal("FromEnv accepted a memory-bounded group without an estimator")
	}
	if !strings.Contains(err.Error(), EnvGroupMemoryBytes) || !strings.Contains(err.Error(), "MemoryEstimator") {
		t.Fatalf("error = %q, want the variable and the missing estimator", err)
	}
}

// TestFromEnvRejectsInvalidIntegers is the regression guard for the silent
// fallback: a typo or a negative count in a bounded setting must stop the boot
// with the variable name and value in the error, not quietly restore the
// unlimited default.
func TestFromEnvRejectsInvalidIntegers(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{EnvGroupMaxRuns, "ten"},
		{EnvGroupMaxRuns, "10m"},
		{EnvGroupMaxRuns, "-1"},
		{EnvGroupMemoryBytes, "1GiB"},
		{EnvGroupMemoryBytes, "9223372036854775808"}, // overflows int64
		{EnvGroupMemoryBytes, "-1024"},
		{EnvLeaseMS, "2.5"},
		{EnvLeaseMS, "-250"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			_, err := FromEnv(lookup(map[string]string{tc.name: tc.value}), envConfig())
			if err == nil {
				t.Fatalf("FromEnv accepted %s=%q", tc.name, tc.value)
			}
			if !strings.Contains(err.Error(), tc.name) || !strings.Contains(err.Error(), tc.value) {
				t.Fatalf("error = %q, want the variable name and value", err)
			}
		})
	}
}

// TestEnvIntHelpers pins the helper contract directly (trim, default, error).
func TestEnvIntHelpers(t *testing.T) {
	if got, err := envInt(lookup(nil), "X", 9); err != nil || got != 9 {
		t.Fatalf("unset envInt = %d/%v, want the default 9", got, err)
	}
	if got, err := envInt(lookup(map[string]string{"X": " 42 "}), "X", 9); err != nil || got != 42 {
		t.Fatalf("envInt = %d/%v, want 42", got, err)
	}
	if _, err := envInt(lookup(map[string]string{"X": "nope"}), "X", 9); err == nil {
		t.Fatal("envInt accepted a non-integer")
	}
	if got, err := envInt64(lookup(nil), "X", 9); err != nil || got != 9 {
		t.Fatalf("unset envInt64 = %d/%v, want the default 9", got, err)
	}
	if got, err := envInt64(lookup(map[string]string{"X": "9001"}), "X", 9); err != nil || got != 9001 {
		t.Fatalf("envInt64 = %d/%v, want 9001", got, err)
	}
	if _, err := envInt64(lookup(map[string]string{"X": "9.5"}), "X", 9); err == nil {
		t.Fatal("envInt64 accepted a non-integer")
	}
}
