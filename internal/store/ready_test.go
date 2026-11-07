package store

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jixoai/keel/internal/handoff"
	"github.com/jixoai/keel/internal/kernel"
)

func readyTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := Open(context.Background(), Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	// Readiness covers the kernel's additive columns, and cmd/runtime applies
	// the kernel migration at boot. Tests that check readiness directly must do
	// the same, so `go test ./internal/store` on a fresh database is not racy
	// against whichever package migrates first. (The handoff tables are applied
	// only by the readiness tests; see applyHandoffSchema.)
	if _, err := kernel.NewPG(context.Background(), st.Pool()); err != nil {
		t.Fatalf("apply kernel migration: %v", err)
	}
	return st
}

// applyHandoffSchema applies the handoff audit migration (runtime_instances +
// handoffs), which cmd/runtime runs at boot after store/kernel. Only readiness
// tests need it; keeping it out of readyTestStore avoids adding two DDL
// statements (and their table locks) to every store test.
func applyHandoffSchema(t *testing.T, st *Store) {
	t.Helper()
	if _, err := handoff.NewPGRegistry(context.Background(), st.Pool()); err != nil {
		t.Fatalf("apply handoff migration: %v", err)
	}
}

func TestCheckReady(t *testing.T) {
	st := readyTestStore(t)
	applyHandoffSchema(t, st)
	if err := st.CheckReady(context.Background()); err != nil {
		t.Fatalf("ready: %v", err)
	}
}

// TestCheckReadyRequiresEveryKernelColumn proves the readiness probe tracks
// every migrated column, not just owner_epoch. It uses an isolated temp schema:
// the table probe is anchored on public (migrated by readyTestStore), while the
// column probe follows current_schema(). The temp schema is created with
// exactly the columns the probe names, so it reports ready; dropping any one of
// them must flip it back to not-ready.
func TestCheckReadyRequiresEveryKernelColumn(t *testing.T) {
	st := readyTestStore(t) // migrate public schema and kernel columns first
	applyHandoffSchema(t, st)
	dsn := os.Getenv("KEEL_TEST_DSN")
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "ready_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE")
	})
	statements := []string{
		`CREATE TABLE ` + ident + `.events (dedupe_key text)`,
		`CREATE TABLE ` + ident + `.runs (status text, owner text, owner_epoch bigint,
		                                            loadout_id text, loadout_revision bigint,
		                                            pending_input boolean, read_watermark bigint,
		                                            loadout_generation bigint, loadout_owner_epoch bigint)`,
		`CREATE TABLE ` + ident + `.tasks (status text)`,
		`CREATE TABLE ` + ident + `.sessions (owner text, owner_epoch bigint, lease_until timestamptz,
		                                              state_owner text, external_backend text,
		                                              external_ref text, external_status text,
		                                              external_recovery text, external_updated_at timestamptz,
		                                              metering text)`,
	}
	for _, statement := range statements {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	searchPath, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := searchPath.Query()
	query.Set("search_path", schema)
	searchPath.RawQuery = query.Encode()
	pool, err := pgxpool.New(ctx, searchPath.String())
	if err != nil {
		t.Fatal(err)
	}
	tempStore := &Store{pool: pool, acquireTimeout: time.Second}
	defer tempStore.Close()
	if err := tempStore.CheckReady(ctx); err != nil {
		t.Fatalf("complete temp schema not ready: %v", err)
	}
	for _, column := range []struct {
		table  string
		name   string
		sqlTyp string
	}{
		{"sessions", "lease_until", "timestamptz"},
		{"sessions", "state_owner", "text"},
		{"sessions", "external_backend", "text"},
		{"runs", "owner", "text"},
		{"runs", "loadout_id", "text"},
		{"runs", "loadout_revision", "bigint"},
		{"runs", "pending_input", "boolean"},
		{"runs", "read_watermark", "bigint"},
		{"runs", "loadout_generation", "bigint"},
		{"runs", "loadout_owner_epoch", "bigint"},
	} {
		if _, err := admin.Exec(ctx,
			"ALTER TABLE "+ident+"."+column.table+" DROP COLUMN "+column.name); err != nil {
			t.Fatal(err)
		}
		if err := tempStore.CheckReady(ctx); err == nil {
			t.Fatalf("schema without %s.%s reported ready", column.table, column.name)
		}
		if _, err := admin.Exec(ctx,
			"ALTER TABLE "+ident+"."+column.table+" ADD COLUMN "+column.name+" "+column.sqlTyp); err != nil {
			t.Fatal(err)
		}
	}
	if err := tempStore.CheckReady(ctx); err != nil {
		t.Fatalf("restored temp schema not ready: %v", err)
	}
}

func TestCheckReadyAfterClose(t *testing.T) {
	st := readyTestStore(t)
	st.Close()
	if err := st.CheckReady(context.Background()); err == nil {
		t.Fatal("closed store reported ready")
	}
}

func TestCountPendingRuns(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "queue"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	after, err := st.CountPendingRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Other test packages share the database, so assert our run is visible
	// rather than an exact global count.
	if after < 1 {
		t.Fatalf("pending runs = %d, want at least 1", after)
	}
}

func TestClaimRunReportsExpiredLease(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	// ClaimRun is a global queue shared with every other package's kernel
	// claims. Retry with a fresh session if a concurrent claim took our row;
	// the assertion needs our own run to come back with previous_status=running.
	for attempt := 0; attempt < 5; attempt++ {
		tenant, session := runTestSession(t, st)
		if err := st.EnqueueRun(ctx, tenant, session); err != nil {
			t.Fatal(err)
		}
		makeOldest := func() {
			t.Helper()
			if _, err := st.Pool().Exec(ctx,
				`UPDATE runs SET updated_at = now() - interval '100 years'
				 WHERE tenant_id = $1 AND session_id = $2`, tenant, session); err != nil {
				t.Fatal(err)
			}
		}
		makeOldest()
		first, err := st.ClaimRun(ctx, 10*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if first.TenantID != tenant || first.SessionID != session {
			continue // another package's claim won the race
		}
		if first.Reclaimed {
			t.Fatal("first claim reported a reclaim")
		}
		time.Sleep(40 * time.Millisecond)
		makeOldest()
		second, err := st.ClaimRun(ctx, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if second.TenantID != tenant || second.SessionID != session {
			continue
		}
		if !second.Reclaimed {
			t.Fatal("second claim did not report an expired lease")
		}
		return
	}
	t.Skip("global queue kept returning other packages' runs")
}

func TestEventObserverSeesCommittedKinds(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "observer"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 8)
	st.SetObserver(func(kind string) { seen <- kind })
	if _, err := st.AppendEvent(ctx, tenant, session, EventCompaction, map[string]string{"summary": "s"}); err != nil {
		t.Fatal(err)
	}
	select {
	case kind := <-seen:
		if kind != EventCompaction {
			t.Fatalf("observed %q", kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("observer was not called")
	}
}
