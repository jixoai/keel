// Migration-session settings test: the handoff DDL must follow the same
// SET LOCAL discipline as store/kernel migrations.

package handoff

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrateSessionSettingsAreTransactionLocal pins the discipline: the
// timeout-free statement belongs to the migration transaction only, and a lock
// wait is bounded by lock_timeout rather than the request-path statement
// timeout. A leaked SET would make every later query on that pooled connection
// unbounded.
func TestMigrateSessionSettingsAreTransactionLocal(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// Simulate the runtime pool's request-path settings.
	if _, err := conn.Exec(ctx, "SET statement_timeout = 5000; SET lock_timeout = 0"); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, migrateSessionSettings); err != nil {
		t.Fatal(err)
	}
	var statement, lock int
	if err := tx.QueryRow(ctx,
		`SELECT (SELECT setting::int FROM pg_settings WHERE name = 'statement_timeout'),
		        (SELECT setting::int FROM pg_settings WHERE name = 'lock_timeout')`).
		Scan(&statement, &lock); err != nil {
		t.Fatal(err)
	}
	if statement != 0 || lock != 30_000 {
		t.Fatalf("migration settings = statement %d ms, lock %d ms; want 0/30000", statement, lock)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT (SELECT setting::int FROM pg_settings WHERE name = 'statement_timeout'),
		        (SELECT setting::int FROM pg_settings WHERE name = 'lock_timeout')`).
		Scan(&statement, &lock); err != nil {
		t.Fatal(err)
	}
	if statement != 5000 || lock != 0 {
		t.Fatalf("SET LOCAL leaked out of the migration transaction: statement %d ms, lock %d ms", statement, lock)
	}
}
