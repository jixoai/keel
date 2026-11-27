package boardintegration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestBoardStoreRLSUsesTenantSetting is P1-5: the board store runs on a real
// non-superuser, NOBYPASSRLS role that owns its tables, so FORCE RLS applies to
// the store's own queries. The test proves the store's tenant setting is what
// makes a row visible:
//
//   - a raw connection as the same role, with no keel.tenant_id, sees zero rows;
//   - boardstore.Load(tenant) sees the row;
//   - Load(other tenant) sees nothing;
//   - the HTTP surface (the same board store) reads the row back.
//
// Making the board store's setTenant a no-op therefore turns the Load and HTTP
// assertions red; it cannot stay green behind a superuser DSN.
func TestBoardStoreRLSUsesTenantSetting(t *testing.T) {
	baseDSN := os.Getenv("KEEL_TEST_DSN")
	if baseDSN == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	roleDSN, schema := newRestrictedBoardRole(t, baseDSN)
	env := newTestEnv(t, func(opts *Options) {
		opts.DSN = roleDSN
		opts.Schema = schema
	})
	ctx := context.Background()
	server := env.server()

	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "work"}); resp.status != 201 {
		t.Fatalf("create channel: %d: %s", resp.status, resp.body)
	}
	if resp := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "RLS-BODY"}); resp.status != 201 {
		t.Fatalf("post under restricted board role: %d: %s", resp.status, resp.body)
	}

	// The same non-superuser role, with no tenant setting, must see nothing.
	conn, err := pgx.Connect(ctx, roleDSN)
	if err != nil {
		t.Fatalf("connect as board role: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "SET search_path TO "+quoteIdent(schema)); err != nil {
		t.Fatalf("set search path: %v", err)
	}
	var raw int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM board_events`).Scan(&raw); err != nil {
		t.Fatalf("count without tenant: %v", err)
	}
	if raw != 0 {
		t.Fatalf("restricted role saw %d rows without keel.tenant_id", raw)
	}

	bypass, err := RoleBypassesRLS(ctx, roleDSN)
	if err != nil {
		t.Fatalf("RoleBypassesRLS: %v", err)
	}
	if bypass {
		t.Fatal("the restricted board role reports superuser/BYPASSRLS")
	}

	records, err := env.surf.store.Load(ctx, env.tenant.String())
	if err != nil {
		t.Fatalf("board store Load(tenant): %v", err)
	}
	if len(records) == 0 {
		t.Fatal("boardstore.Load saw no rows: the store's tenant setting is not effective")
	}
	other, err := env.surf.store.Load(ctx, env.other.String())
	if err != nil {
		t.Fatalf("board store Load(other): %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("board store Load(other) saw %d rows", len(other))
	}

	read := env.do(t, server, "GET", "/v1/board/messages?channel=work", "alice", nil)
	if read.status != 200 || !strings.Contains(string(read.body), "RLS-BODY") {
		t.Fatalf("HTTP read under restricted board role = %d: %s", read.status, read.body)
	}
}

// newRestrictedBoardRole creates a LOGIN role with NOSUPERUSER and NOBYPASSRLS,
// a schema it owns, and a DSN for it. Role creation is a test prerequisite: a
// database that cannot CREATE ROLE fails the test rather than skipping it, so a
// deployment that thinks RLS is exercised cannot pass vacuously.
func newRestrictedBoardRole(t *testing.T, baseDSN string) (string, string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		t.Fatalf("connect as test administrator: %v", err)
	}
	defer admin.Close(context.Background())

	parsed, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse KEEL_TEST_DSN: %v", err)
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	if database == "" {
		t.Fatalf("KEEL_TEST_DSN names no database")
	}
	host := parsed.Host
	if host == "" {
		// A socket DSN has no host authority; a separate LOGIN role is easiest to
		// exercise over TCP. CI and local PostgreSQL both listen there.
		host = "127.0.0.1:5432"
	}
	role := "board_rls_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	passwordBytes := make([]byte, 18)
	if _, err := rand.Read(passwordBytes); err != nil {
		t.Fatalf("generate role password: %v", err)
	}
	password := hex.EncodeToString(passwordBytes)
	roleIdent := quoteIdent(role)
	schema := "board_rls_schema_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	schemaIdent := quoteIdent(schema)
	dbIdent := quoteIdent(database)

	if _, err := admin.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS`,
		roleIdent, password)); err != nil {
		t.Fatalf("create restricted board role (test environment must allow CREATE ROLE): %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		conn, err := pgx.Connect(cleanup, baseDSN)
		if err != nil {
			return
		}
		defer conn.Close(context.Background())
		_, _ = conn.Exec(cleanup, `DROP OWNED BY `+roleIdent)
		_, _ = conn.Exec(cleanup, `DROP ROLE IF EXISTS `+roleIdent)
	})
	if _, err := admin.Exec(ctx, `GRANT CONNECT ON DATABASE `+dbIdent+` TO `+roleIdent); err != nil {
		t.Fatalf("grant connect: %v", err)
	}
	if _, err := admin.Exec(ctx, `GRANT CREATE ON DATABASE `+dbIdent+` TO `+roleIdent); err != nil {
		t.Fatalf("grant create on database: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schemaIdent+` AUTHORIZATION `+roleIdent); err != nil {
		t.Fatalf("create schema for restricted role: %v", err)
	}

	var super, bypass bool
	if err := admin.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`, role).Scan(&super, &bypass); err != nil {
		t.Fatalf("read role attributes: %v", err)
	}
	if super || bypass {
		t.Fatalf("test role is superuser=%v bypassrls=%v, want both false", super, bypass)
	}

	roleURL := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(role, password),
		Host:     host,
		Path:     "/" + database,
		RawQuery: parsed.RawQuery,
	}
	query := roleURL.Query()
	if query.Get("sslmode") == "" {
		query.Set("sslmode", "disable")
	}
	query.Del("host")
	roleURL.RawQuery = query.Encode()
	// Prove the role DSN works before the fixture starts.
	probe, err := pgx.Connect(ctx, roleURL.String())
	if err != nil {
		t.Fatalf("connect as restricted board role: %v", err)
	}
	_ = probe.Close(context.Background())
	return roleURL.String(), schema
}
