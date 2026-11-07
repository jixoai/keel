package store

import (
	"context"
	"strings"
	"testing"
)

// TestMigrateLeavesPoolSettingsAlone pins the discipline behind
// migrateSessionSettings: the timeout-free session belongs to the migration
// transaction only. If the SET ever leaks into a pooled connection, every
// request-path query would run without a statement timeout.
func TestMigrateLeavesPoolSettingsAlone(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var statement, lock string
	if err := st.Pool().QueryRow(ctx,
		"SELECT current_setting('statement_timeout'), current_setting('lock_timeout')").
		Scan(&statement, &lock); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if statement != "5s" {
		t.Fatalf("pool statement_timeout = %q, want 5s (the migration session leaked)", statement)
	}
	if lock != "0" {
		t.Fatalf("pool lock_timeout = %q, want 0 (the migration session leaked)", lock)
	}
}

// TestMigrateTransactionRunsWithoutStatementTimeout proves the migration
// transaction itself sees statement_timeout = 0 while it holds the settings.
func TestMigrateTransactionRunsWithoutStatementTimeout(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, migrateSessionSettings); err != nil {
		t.Fatalf("apply settings: %v", err)
	}
	var statement string
	if err := tx.QueryRow(ctx, "SELECT current_setting('statement_timeout')").Scan(&statement); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(statement) != "0" {
		t.Fatalf("migration statement_timeout = %q, want 0", statement)
	}
	var lock string
	if err := tx.QueryRow(ctx, "SELECT current_setting('lock_timeout')").Scan(&lock); err != nil {
		t.Fatal(err)
	}
	if lock != "30s" {
		t.Fatalf("migration lock_timeout = %q, want 30s", lock)
	}
}
