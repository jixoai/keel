package wiring

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/placement"
)

// Deployment environment names. They are read once at boot; the tenant
// run-quota is not here because it comes from the same tenant document the
// kernel admission used.
const (
	// EnvStrategy selects the default placement policy: pack (default), user or
	// pin.
	EnvStrategy = "KEEL_PLACEMENT_STRATEGY"
	// EnvPinGroupID is the pin strategy's configured target group.
	EnvPinGroupID = "KEEL_PLACEMENT_GROUP_ID"
	// EnvGroupMaxRuns sizes a group the automatic strategy creates on first
	// use. Zero means no group-level limit (the deployment worker concurrency
	// and the tenant quota still apply). An existing group's row wins.
	EnvGroupMaxRuns = "KEEL_GROUP_MAX_CONCURRENT_RUNS"
	// EnvGroupMemoryBytes sizes the pack strategy's memory budget. Zero means
	// unbounded.
	EnvGroupMemoryBytes = "KEEL_GROUP_MEMORY_LIMIT_BYTES"
	// EnvLeaseMS bounds the placement lease the gate creates. Zero uses the
	// placement default.
	EnvLeaseMS = "KEEL_PLACEMENT_LEASE_MS"
	// EnvQueueIntervalMS is the crash-recovery queue sweep interval. Zero uses
	// DefaultQueueInterval.
	EnvQueueIntervalMS = "KEEL_PLACEMENT_QUEUE_INTERVAL_MS"
)

// FromEnv completes cfg from the deployment environment and builds the gate.
// It exists so `cmd/runtime` and `cmd/keel serve` share one parsing of the
// placement settings (and one log line describing the effective policy).
func FromEnv(getenv func(string) string, cfg Config) (*Runtime, error) {
	if getenv == nil {
		return nil, fmt.Errorf("placement wiring: getenv is required")
	}
	cfg.Strategy = placement.Strategy(strings.TrimSpace(getenv(EnvStrategy)))
	if raw := strings.TrimSpace(getenv(EnvPinGroupID)); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%s is not a UUID: %w", EnvPinGroupID, err)
		}
		cfg.PinGroupID = &parsed
	}
	defaultMaxRuns, err := envInt(getenv, EnvGroupMaxRuns, 0)
	if err != nil {
		return nil, err
	}
	cfg.DefaultMaxConcurrentRuns = defaultMaxRuns
	defaultMemory, err := envInt64(getenv, EnvGroupMemoryBytes, 0)
	if err != nil {
		return nil, err
	}
	cfg.DefaultMemoryLimitBytes = defaultMemory
	if defaultMemory > 0 && cfg.MemoryEstimator == nil {
		// A memory-bounded group needs an observed estimate for both packing and
		// the admission ceiling. Booting without one would turn every admission
		// into a typed unsupported failure at request time; failing here names the
		// misconfiguration once, at the only moment an operator can fix it.
		return nil, fmt.Errorf(
			"%s=%d configures a memory-bounded group but no memory estimator is wired; "+
				"remove the limit or wire placement.Config.MemoryEstimator",
			EnvGroupMemoryBytes, defaultMemory)
	}
	leaseMS, err := envInt(getenv, EnvLeaseMS, 0)
	if err != nil {
		return nil, err
	}
	cfg.LeaseTTL = time.Duration(leaseMS) * time.Millisecond
	queueMS, err := envInt(getenv, EnvQueueIntervalMS, 0)
	if err != nil {
		return nil, err
	}
	if queueMS > 0 {
		cfg.QueueInterval = time.Duration(queueMS) * time.Millisecond
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	rt, err := New(cfg)
	if err != nil {
		return nil, err
	}
	effective := cfg.Strategy
	if effective == "" {
		effective = placement.Pack
	}
	cfg.Logger.Info("placement admission gate", "strategy", string(effective),
		"group_max_concurrent_runs", cfg.DefaultMaxConcurrentRuns,
		"group_memory_limit_bytes", cfg.DefaultMemoryLimitBytes,
		"queue_sweep_interval", rt.QueueInterval.String(),
		"tenant_run_quota", "tenant-document")
	return rt, nil
}

// envInt parses an optional integer setting. An absent or empty value keeps the
// default; a value that does not parse is a boot-time configuration error. The
// error names the variable and the offending value: silently falling back would
// make a typo (for example "10m") turn a bounded group into an unlimited one.
func envInt(getenv func(string) string, name string, def int) (int, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return def, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not an integer: %w", name, raw, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s=%q must be >= 0", name, raw)
	}
	return parsed, nil
}

// envInt64 is envInt for byte counts.
func envInt64(getenv func(string) string, name string, def int64) (int64, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return def, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not an integer: %w", name, raw, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s=%q must be >= 0", name, raw)
	}
	return parsed, nil
}
