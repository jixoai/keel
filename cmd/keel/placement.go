package main

import (
	"log/slog"
	"os"

	"github.com/jixoai/keel/internal/kernel"
	placementwiring "github.com/jixoai/keel/internal/placement/wiring"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/store"
)

// newPlacementRuntime assembles the durable run admission gate from the
// deployment environment. The tenant quota is resolved from the same
// configuration the kernel's admission used, so `keel serve` enforces one
// vocabulary in the database instead of a second process-local guess.
func newPlacementRuntime(st *store.Store, limits *quota.Limits, committer kernel.Committer, runtimeID string, log *slog.Logger) (*placementwiring.Runtime, error) {
	return placementwiring.FromEnv(os.Getenv, placementwiring.Config{
		Store:    st,
		Kernel:   committer,
		Quota:    limits,
		WorkerID: runtimeID,
		Logger:   log,
	})
}
