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
	cfg.DefaultMaxConcurrentRuns = envInt(getenv, EnvGroupMaxRuns, 0)
	cfg.DefaultMemoryLimitBytes = envInt64(getenv, EnvGroupMemoryBytes, 0)
	cfg.LeaseTTL = time.Duration(envInt(getenv, EnvLeaseMS, 0)) * time.Millisecond
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
		"tenant_run_quota", "tenant-document")
	return rt, nil
}

func envInt(getenv func(string) string, name string, def int) int {
	if raw := getenv(name); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			return parsed
		}
	}
	return def
}

func envInt64(getenv func(string) string, name string, def int64) int64 {
	if raw := getenv(name); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return parsed
		}
	}
	return def
}
