package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// identityTenantTables is the A1 table catalog. It exists as a test constant so
// adding a table to schema.sql without adding it to rlsTenantTables fails here
// (the catalog test below) instead of silently leaving the table readable
// across tenants.
var identityTenantTables = []string{
	"accounts",
	"roles",
	"profiles",
	"bots",
	"team_bots",
	"permission_groups",
	"permission_group_members",
	"permission_group_grants",
	"permission_grants",
	"resource_relations",
	"auth_sessions",
	"auth_events",
	"bot_api_keys",
}

// TestIdentitySchemaMigratesWithRLS pins the A1 schema contract: every new
// table exists after Migrate, is ENABLEd and FORCEd for row security, carries
// the tenant_isolation policy, and is registered in rlsTenantTables.
func TestIdentitySchemaMigratesWithRLS(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()

	registered := make(map[string]bool, len(rlsTenantTables))
	for _, table := range rlsTenantTables {
		registered[table] = true
	}
	for _, table := range identityTenantTables {
		var exists bool
		if err := st.Pool().QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("table %s missing after migrate", table)
		}
		if !registered[table] {
			t.Fatalf("%s is missing from rlsTenantTables", table)
		}
		if !st.RLSEnabled() {
			continue
		}
		var enabled, forced bool
		var policies int
		if err := st.Pool().QueryRow(ctx,
			`SELECT c.relrowsecurity, c.relforcerowsecurity,
			        (SELECT count(*) FROM pg_policy p
			          WHERE p.polrelid = c.oid AND p.polname = 'tenant_isolation')
			   FROM pg_class c
			  WHERE c.oid = to_regclass($1)`, table).
			Scan(&enabled, &forced, &policies); err != nil {
			t.Fatalf("probe rls on %s: %v", table, err)
		}
		if !enabled || !forced || policies != 1 {
			t.Fatalf("%s rls enabled=%v forced=%v policies=%d, want true/true/1",
				table, enabled, forced, policies)
		}
	}

	// The session ownership columns are nullable in A1; A4 closes the contract.
	for _, column := range []struct {
		name   string
		sqlTyp string
	}{
		{"bot_id", "text"},
		{"account_id", "uuid"},
		{"created_by_account_id", "uuid"},
		{"team_bot_id", "text"},
	} {
		var dataType, nullable string
		if err := st.Pool().QueryRow(ctx,
			`SELECT data_type, is_nullable FROM information_schema.columns
			  WHERE table_schema = current_schema()
			    AND table_name = 'sessions' AND column_name = $1`, column.name).
			Scan(&dataType, &nullable); err != nil {
			t.Fatalf("sessions.%s missing: %v", column.name, err)
		}
		if dataType != column.sqlTyp || nullable != "YES" {
			t.Fatalf("sessions.%s = %s nullable=%s, want %s nullable=YES",
				column.name, dataType, nullable, column.sqlTyp)
		}
	}

	// Every ownership projection has its tenant-leading index.
	for _, index := range []string{
		"sessions_by_bot",
		"sessions_by_account",
		"sessions_by_created_by",
		"sessions_by_team_bot",
		"accounts_username",
		"bot_api_keys_hash",
		"permission_grants_unique",
	} {
		var exists bool
		if err := st.Pool().QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, index).Scan(&exists); err != nil {
			t.Fatalf("probe index %s: %v", index, err)
		}
		if !exists {
			t.Fatalf("index %s missing", index)
		}
	}
}

// TestIdentityRLSIsolatesTenants proves the A1 tables behave like every other
// tenant table. One row per table is inserted for tenant A and tenant B through
// the operator pool; a tenant-scoped transaction for A must see exactly its own
// row in every table, even without a tenant predicate, and must not be able to
// write a B-tagged row.
func TestIdentityRLSIsolatesTenants(t *testing.T) {
	st := tenancyStore(t)
	if !st.RLSEnabled() {
		t.Skip("RLS is disabled for this store (KEEL_TEST_DSN cluster cannot create roles)")
	}
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		if err := st.CreateTenant(ctx, tenant, "identity-rls-"+tenant.String()[:8]); err != nil {
			t.Fatal(err)
		}
	}

	accountA, accountB := uuid.New(), uuid.New()
	roleA, roleB := uuid.New(), uuid.New()
	profileA, profileB := uuid.New(), uuid.New()
	groupA, groupB := uuid.New(), uuid.New()
	grantA, grantB := uuid.New(), uuid.New()
	sessionA, sessionB := uuid.New(), uuid.New()
	authSessionA, authSessionB := uuid.New(), uuid.New()
	keyA, keyB := uuid.New(), uuid.New()
	// Insert one row per table for each tenant. The operator pool carries the
	// bypass marker, so these writes are not subject to the policy; the
	// tenant-scoped reads below are.
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO accounts (tenant_id, account_id, username, display_name, kind)
		 VALUES ($1, $2, 'alice', 'Alice', 'human'), ($3, $4, 'bob', 'Bob', 'human')`,
		tenantA, accountA, tenantB, accountB); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO roles (tenant_id, role_id, name, instruction)
		 VALUES ($1, $2, 'role-a', ''), ($3, $4, 'role-b', '')`,
		tenantA, roleA, tenantB, roleB); err != nil {
		t.Fatalf("insert roles: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO profiles (tenant_id, profile_id, account_id, role_id)
		 VALUES ($1, $2, $3, $4), ($5, $6, $7, $8)`,
		tenantA, profileA, accountA, roleA, tenantB, profileB, accountB, roleB); err != nil {
		t.Fatalf("insert profiles: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO bots (tenant_id, bot_id, name, profile_id, model_kind, default_model, harness)
		 VALUES ($1, 'bot-a', 'Bot A', $2, 'text', '{"providerId":"p","modelId":"m"}', '{}'),
		        ($3, 'bot-b', 'Bot B', $4, 'text', '{"providerId":"p","modelId":"m"}', '{}')`,
		tenantA, profileA, tenantB, profileB); err != nil {
		t.Fatalf("insert bots: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO team_bots (tenant_id, team_bot_id, name, mission, roster)
		 VALUES ($1, 'team-a', 'Team A', 'mission a', '[]'),
		        ($2, 'team-b', 'Team B', 'mission b', '[]')`,
		tenantA, tenantB); err != nil {
		t.Fatalf("insert team bots: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO permission_groups (tenant_id, group_id, name)
		 VALUES ($1, $2, 'group-a'), ($3, $4, 'group-b')`,
		tenantA, groupA, tenantB, groupB); err != nil {
		t.Fatalf("insert permission groups: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO permission_group_members (tenant_id, group_id, account_id, added_by)
		 VALUES ($1, $2, $3, $3), ($4, $5, $6, $6)`,
		tenantA, groupA, accountA, tenantB, groupB, accountB); err != nil {
		t.Fatalf("insert permission group members: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO permission_group_grants (tenant_id, group_id, permission_key, scope)
		 VALUES ($1, $2, 'session.read', 'own'), ($3, $4, 'session.read', 'own')`,
		tenantA, groupA, tenantB, groupB); err != nil {
		t.Fatalf("insert permission group grants: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO permission_grants
		    (tenant_id, grant_id, subject_type, subject_id, permission_key, scope, granted_by)
		 VALUES ($1, $2, 'account', $3, 'session.read', 'own', $3),
		        ($4, $5, 'account', $6, 'session.read', 'own', $6)`,
		tenantA, grantA, accountA, tenantB, grantB, accountB); err != nil {
		t.Fatalf("insert permission grants: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO resource_relations
		    (tenant_id, resource_type, resource_id, relation, subject_type, subject_id, created_by)
		 VALUES ($1, 'session', $2, 'owner', 'account', $3, $4),
		        ($5, 'session', $6, 'owner', 'account', $7, $8)`,
		tenantA, sessionA.String(), accountA.String(), accountA,
		tenantB, sessionB.String(), accountB.String(), accountB); err != nil {
		t.Fatalf("insert resource relations: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO auth_sessions (tenant_id, session_id, account_id, expires_at)
		 VALUES ($1, $2, $3, now() + interval '1 hour'), ($4, $5, $6, now() + interval '1 hour')`,
		tenantA, authSessionA, accountA, tenantB, authSessionB, accountB); err != nil {
		t.Fatalf("insert auth sessions: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO auth_events (tenant_id, account_id, kind)
		 VALUES ($1, $2, 'login_success'), ($3, $4, 'login_success')`,
		tenantA, accountA, tenantB, accountB); err != nil {
		t.Fatalf("insert auth events: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO bot_api_keys (tenant_id, key_id, key_hash, key_prefix, account_id, bot_id)
		 VALUES ($1, $2, $3, 'keel_bot_a', $4, 'bot-a'),
		        ($5, $6, $7, 'keel_bot_b', $8, 'bot-b')`,
		tenantA, keyA, BotAPIKeyHash("key-a-"+tenantA.String()), accountA,
		tenantB, keyB, BotAPIKeyHash("key-b-"+tenantB.String()), accountB); err != nil {
		t.Fatalf("insert bot api keys: %v", err)
	}

	if err := st.TenantScoped(ctx, tenantA, func(tx TenantTx) error {
		for _, table := range identityTenantTables {
			var count int
			query := `SELECT count(*) FROM ` + quoteIdent(table)
			if err := tx.QueryRow(ctx, query).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				t.Fatalf("tenant A sees %d rows in %s, want 1", count, table)
			}
		}
		var foreign int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM accounts WHERE account_id = $1`, accountB).Scan(&foreign); err != nil {
			return err
		}
		if foreign != 0 {
			t.Fatalf("tenant A sees tenant B's account")
		}
		return nil
	}); err != nil {
		t.Fatalf("tenant A scope: %v", err)
	}

	// An unrelated tenant sees nothing, not even the tenants row it does not own.
	if err := st.TenantScoped(ctx, uuid.New(), func(tx TenantTx) error {
		for _, table := range identityTenantTables {
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+quoteIdent(table)).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("unrelated tenant sees %d rows in %s", count, table)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("unrelated scope: %v", err)
	}

	// A tenant-scoped write tagged with another tenant is refused by WITH CHECK.
	err := st.TenantScoped(ctx, tenantA, func(tx TenantTx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO accounts (tenant_id, account_id, username, display_name, kind)
			 VALUES ($1, $2, 'cross', 'Cross', 'human')`, tenantB, uuid.New())
		return err
	})
	if err == nil {
		t.Fatal("a cross-tenant account write was accepted")
	}
}
