// Row Level Security migration (spec: tenancy-quotas / 行级安全).
//
// The application is the only writer, and every query already carries
// tenant_id. RLS is the second line of defence: a query that forgets its
// predicate returns zero rows instead of another tenant's data.
//
// Shape of the migration (and why each part exists):
//
//   - A dedicated non-owner role (keel_app by default) is created without
//     LOGIN: the application connects as that role only through SET ROLE, so
//     the cluster's connection string keeps whatever operator identity the
//     deployment uses. RLS is bypassed by table owners and by superusers, so
//     "connect as a non-owner" is a policy requirement, not a suggestion.
//   - The role receives USAGE on the schema and SELECT/INSERT/UPDATE/DELETE on
//     the tenant tables. It is not granted to the current user implicitly: the
//     application calls set_config('app.tenant_id', ...) and SET ROLE itself.
//   - Every tenant table gets ENABLE + FORCE ROW LEVEL SECURITY and one policy
//     whose USING and WITH CHECK clauses compare tenant_id to
//     current_setting('app.tenant_id', true). The 'true' (missing_ok) form
//     makes an unset setting yield NULL, so the comparison is never true and
//     the query returns no rows (fail closed, not an error).
//   - The role may bypass the policies only when it explicitly asks: the
//     operator path (provisioning, cross-tenant admin reads) sets
//     app.bypass_rls = 'on', which the policy admits as an OR term. Without it
//     a migration or an admin query would silently see nothing.
//
// The migration is idempotent: CREATE ROLE ... IF NOT EXISTS equivalents,
// ALTER POLICY replacement and CREATE POLICY guarded by a catalog probe. Running
// it repeatedly converges instead of failing.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// RLSRoleName is the non-owner role the RLS policies bind to. It is a rolsuper
// == false, rolbypassrls == false role, which is what makes FORCE ROW LEVEL
// SECURITY effective.
const RLSRoleName = "keel_app"

// rlsTenantTables are the tables whose rows belong to exactly one tenant.
//
// `tenants` is included: its primary key IS tenant_id, so a tenant-scoped
// session sees exactly its own tenant row (the role needs SELECT on it to
// resolve names). The auth lookup that runs before a tenant is known uses the
// bypass marker, so this policy never blocks sign-in.
//
// `plugins` is excluded on purpose: plugin enablement is a property of the
// process, not of a tenant (see schema.sql), so a tenant-scoped policy there
// would be a lie. The tenant role gets no grant on it at all.
var rlsTenantTables = []string{
	"tenants",
	"sessions",
	"events",
	"checkpoints",
	"submissions",
	"tasks",
	"blobs",
	"blob_chunks",
	"blob_refs",
	"runs",
	"usage_ledger",
	"tenant_usage",
	"api_keys", // the auth lookup runs with app.bypass_rls='on' (see LookupAPIKey)
	"mcp_oauth_pending",
	"mcp_oauth_credentials",
	"model_settings",
	"model_settings_history",
	"tenant_secrets",
	"agents",
	"agent_teams",
	"task_cards",
	"task_events",
	"bash_jobs",
	"approval_provenance",
	"notifications",
	"file_manifest",
	"session_snapshot",
	"mcp_app_audit",
}

// ApplyRLS applies the RLS migration. It is safe to run on every boot, on a
// database that already carries the policy, and concurrently from several
// processes (test packages and replicas start together).
//
// Serialization: one transaction holding an advisory lock, so the policy
// statements never run against each other. CREATE POLICY is not idempotent in
// PostgreSQL (there is no IF NOT EXISTS and no CREATE OR REPLACE), so
// concurrency has to be excluded rather than tolerated — the earlier
// statement-at-a-time form raced exactly there. The transaction's own DDL
// failures are retried the same way the schema migration's are.
//
// A database whose connection cannot create roles (a managed service without
// CREATEROLE) fails loudly: silently skipping would leave FORCE ROW LEVEL
// SECURITY on with no role to serve, and every query would return zero rows.
func (s *Store) ApplyRLS(ctx context.Context) error {
	// Fast path: when every tenant table already carries ENABLE + FORCE and the
	// policy, there is nothing to do. This is not an optimization — the policy
	// statements take ACCESS EXCLUSIVE locks, so re-applying them on every boot
	// (and in every test that opens a store) blocks live traffic behind DDL for
	// no reason. A read-only catalog probe settles it.
	applied, err := s.rlsApplied(ctx, rlsTenantTables)
	if err != nil {
		return fmt.Errorf("store: apply rls: %w", err)
	}
	if applied {
		return nil
	}
	statements := rlsStatements(RLSRoleName, rlsTenantTables)
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		err := s.applyRLSOnce(ctx, statements)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryableTx(err) && !isRLSTransient(err) {
			return fmt.Errorf("store: apply rls: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("store: apply rls: %w", ctx.Err())
		case <-time.After(time.Duration(50+attempt*75) * time.Millisecond):
		}
	}
	return fmt.Errorf("store: apply rls: %w", lastErr)
}

// rlsApplied reports whether every named table already has row security enabled
// and forced, with the tenant_isolation policy present. A table that does not
// exist yet reports "not applied": the policy pass then fails loudly rather than
// silently leaving the table unprotected.
func (s *Store) rlsApplied(ctx context.Context, tables []string) (bool, error) {
	names := make([]string, 0, len(tables))
	for _, table := range tables {
		names = append(names, table)
	}
	var pending int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*)
		   FROM unnest($1::text[]) AS t(name)
		  WHERE NOT EXISTS (
		      SELECT 1
		        FROM pg_class c
		        JOIN pg_namespace n ON n.oid = c.relnamespace
		       WHERE n.nspname = current_schema()
		         AND c.relname = t.name
		         AND c.relkind = 'r'
		         AND c.relrowsecurity
		         AND c.relforcerowsecurity
		         AND EXISTS (SELECT 1 FROM pg_policy p
		                      WHERE p.polrelid = c.oid AND p.polname = 'tenant_isolation')
		  )`, names).Scan(&pending); err != nil {
		return false, err
	}
	return pending == 0, nil
}

// isRLSTransient reports the failures a concurrent RLS migration can produce
// that are worth retrying: a duplicate policy (the other process won the race
// between its catalog probe and its CREATE) and a concurrently-updated catalog
// tuple.
func isRLSTransient(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42710": // duplicate_object: policy already exists
			return true
		case "XX000": // tuple concurrently updated (catalog write race)
			return true
		}
	}
	return false
}

// applyRLSOnce runs the whole migration in one transaction under an advisory
// lock. The lock is transaction-scoped, so it releases on commit or rollback.
func (s *Store) applyRLSOnce(ctx context.Context, statements []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, migrateSessionSettings); err != nil {
		return err
	}
	// A distinct lock key from the schema migration's (714322): the RLS pass runs
	// right after it and must not deadlock against another booting replica that
	// holds the schema lock while waiting for this one.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(714323)"); err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("%w (statement: %s)", err, firstLine(statement))
		}
	}
	return tx.Commit(ctx)
}

// firstLine trims a multi-line statement to its first line for an error message
// (the error text must stay readable and must never be a whole DO block).
func firstLine(statement string) string {
	if index := strings.IndexByte(statement, '\n'); index >= 0 {
		return statement[:index]
	}
	if len(statement) > 120 {
		return statement[:120] + "..."
	}
	return statement
}

// rlsStatements builds the migration body. It is a function so a test can
// assert the shape (FORCE on every tenant table, the app.tenant_id predicate,
// the bypass term) without a database.
func rlsStatements(role string, tables []string) []string {
	statements := []string{
		// The connecting (operator) role must be able to SET ROLE into the app role,
		// otherwise the tenant-scoped path cannot assume it. Superusers may grant
		// membership; a managed cluster that refuses the grant is warned about, not
		// failed: the deployment may have granted it out of band.
		fmt.Sprintf(`DO $$
BEGIN
    EXECUTE format('GRANT %%I TO CURRENT_USER', %s);
EXCEPTION WHEN OTHERS THEN
    RAISE WARNING 'keel: cannot grant %s to the current user; grant it out of band for tenant-scoped connections';
END
$$`, quoteLiteral(role), role),
		// The role must exist before grants/policies name it. A cluster where
		// the role exists keeps its attributes; the ALTERs below reassert the two
		// that make RLS effective (no superuser, no BYPASSRLS).
		fmt.Sprintf(`DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
        EXECUTE format('CREATE ROLE %%I NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE', %s);
    END IF;
END
$$`, quoteLiteral(role), quoteLiteral(role)),
		fmt.Sprintf(`DO $$
BEGIN
    EXECUTE format('ALTER ROLE %%I NOSUPERUSER NOBYPASSRLS', %s);
EXCEPTION WHEN insufficient_privilege THEN
    RAISE WARNING 'keel: cannot assert RLS role attributes on %s (managed cluster?); RLS stays enabled';
END
$$`, quoteLiteral(role), role),
		// Grants: the role may read and write the tenant tables, nothing else.
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, quoteIdent(role)),
	}
	for _, table := range tables {
		statements = append(statements, fmt.Sprintf(
			`GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE %s TO %s`, quoteIdent(table), quoteIdent(role)))
	}
	// Sequences back the bigserial keys (tenant_usage, plugin_audit). The role
	// inserts rows, so it needs to advance them.
	statements = append(statements,
		fmt.Sprintf(`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s`, quoteIdent(role)))
	for _, table := range tables {
		statements = append(statements,
			// ENABLE turns the policy on for ordinary roles; FORCE makes it apply to
			// the table owner as well (the difference between "a bug in the app is
			// contained" and "a bug in the app is contained only when it does not run
			// as the owner").
			fmt.Sprintf(`ALTER TABLE %s ENABLE ROW LEVEL SECURITY`, quoteIdent(table)),
			fmt.Sprintf(`ALTER TABLE %s FORCE ROW LEVEL SECURITY`, quoteIdent(table)),
			fmt.Sprintf(`DROP POLICY IF EXISTS tenant_isolation ON %s`, quoteIdent(table)),
			fmt.Sprintf(`CREATE POLICY tenant_isolation ON %s
    USING (
        current_setting('app.bypass_rls', true) = 'on'
        OR (
            COALESCE(current_setting('app.tenant_id', true), '') <> ''
            AND tenant_id::text = current_setting('app.tenant_id', true)
        )
    )
    WITH CHECK (
        current_setting('app.bypass_rls', true) = 'on'
        OR (
            COALESCE(current_setting('app.tenant_id', true), '') <> ''
            AND tenant_id::text = current_setting('app.tenant_id', true)
        )
    )`, quoteIdent(table)),
		)
	}
	return statements
}

// quoteIdent renders a SQL identifier for the migration (identifiers cannot be
// bind parameters in DDL). The names come from this file's own list, never from
// a request.
func quoteIdent(name string) string {
	return `"` + name + `"`
}

// quoteLiteral renders a single-quoted SQL literal for DDL that cannot take
// parameters. Only this file's fixed role name reaches it.
func quoteLiteral(value string) string {
	return `'` + value + `'`
}
