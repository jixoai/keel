package pg_test

import (
	"context"
	"os"
	"testing"

	"github.com/jixoai/keel/internal/pluginstore"
	"github.com/jixoai/keel/internal/pluginstore/conformance"
	pluginstorepg "github.com/jixoai/keel/internal/pluginstore/pg"
	"github.com/jixoai/keel/internal/store"
)

// TestPGConformance runs the shared contract against the real PostgreSQL
// adapter. It is skipped when KEEL_TEST_DSN is unset; the rest of the suite
// still exercises the same contract in memory.
func TestPGConformance(t *testing.T) {
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
	adapter := pluginstorepg.New(st.Pool())
	conformance.Run(t, func(t *testing.T) pluginstore.Store { return adapter })
}
