// Tenant-scoped database access through the RLS boundary.
//
// The operator pool (Store.pool) connects as whatever identity the deployment's
// DSN names — normally the database owner — and is used by provisioning, the
// migration, the kernel's cross-tenant queue and the admin API. Those paths
// must see every tenant, which is exactly what FORCE ROW LEVEL SECURITY
// forbids for the table owner.
//
// The tenant pool connects as the non-owner role (RLSRoleName) with
// app.bypass_rls='off'. A request-scoped transaction on it sets app.tenant_id
// and nothing else, so:
//
//   - a query that forgets its tenant predicate returns zero rows instead of
//     another tenant's data;
//   - the same transaction identity is what a test uses to prove the boundary
//     (see TestRLSCrossTenantReadsReturnNoRows).
//
// The setting travels with the transaction (SET LOCAL), so a pooled connection
// never carries a tenant between checkouts.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrRLSUnavailable reports that the tenant pool does not exist (RLS disabled
// for this deployment). Callers must not silently fall back to the operator
// pool: that would turn a missing enforcement into an open one.
var ErrRLSUnavailable = errors.New("store: tenant-scoped access is not available (RLS disabled)")

// ErrNoTenantScope reports a nil tenant id: an unscoped tenant transaction
// would see nothing (fail closed) and is a caller bug.
var ErrNoTenantScope = errors.New("store: tenant scope requires a tenant id")

// TenantTx is one tenant-scoped transaction. Every statement inside it is
// filtered by row-level security to TenantID; the caller still writes its own
// tenant_id predicate (RLS is the second line, not the first).
type TenantTx struct {
	Tx       pgx.Tx
	TenantID uuid.UUID
}

// Query is a convenience pass-through.
func (t TenantTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.Tx.Query(ctx, sql, args...)
}

// QueryRow is a convenience pass-through.
func (t TenantTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.Tx.QueryRow(ctx, sql, args...)
}

// Exec is a convenience pass-through.
func (t TenantTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.Tx.Exec(ctx, sql, args...)
}

// TenantScoped runs fn inside a transaction on the tenant pool with
// app.tenant_id set to tenantID. The transaction is read-write; a caller that
// only reads still gets the RLS setting.
//
// Isolation: read committed (the pool default). A tenant transaction is short —
// one request's write or read — and read committed keeps it from holding
// snapshots that block vacuum.
func (s *Store) TenantScoped(ctx context.Context, tenantID uuid.UUID, fn func(TenantTx) error) error {
	if tenantID == uuid.Nil {
		return ErrNoTenantScope
	}
	if s.tenantPool == nil {
		return ErrRLSUnavailable
	}
	tx, err := s.tenantPool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin tenant tx: %w", err)
	}
	defer tx.Rollback(ctx)
	// set_config(..., true) scopes the setting to this transaction, so the
	// pooled connection is clean when it returns to the pool.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return fmt.Errorf("store: set tenant scope: %w", err)
	}
	if err := fn(TenantTx{Tx: tx, TenantID: tenantID}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit tenant tx: %w", err)
	}
	return nil
}

// OperatorScoped runs fn inside a transaction on the tenant pool with both
// app.tenant_id (the tenant being acted on) and app.bypass_rls='on' set. It is
// the escape hatch the policy's bypass term exists for: cross-tenant operator
// work that must still run as the non-owner role (for example an admin listing
// that has already filtered by tenant in SQL, or the auth lookup that resolves
// a key before its tenant is known).
//
// It is deliberately NOT reachable from a request handler: a request path that
// can choose its tenant is the bug RLS is meant to catch.
func (s *Store) OperatorScoped(ctx context.Context, fn func(TenantTx) error) error {
	if s.tenantPool == nil {
		return ErrRLSUnavailable
	}
	tx, err := s.tenantPool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin operator tx: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.bypass_rls', 'on', true)`); err != nil {
		return fmt.Errorf("store: set operator bypass: %w", err)
	}
	if err := fn(TenantTx{Tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit operator tx: %w", err)
	}
	return nil
}

// RLSEnabled reports whether this deployment has the tenant pool.
func (s *Store) RLSEnabled() bool { return s.tenantPool != nil }
