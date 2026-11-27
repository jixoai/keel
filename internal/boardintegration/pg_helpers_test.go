package boardintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/auth"

	"github.com/jixoai/keel-board/protocol"
	keelstore "github.com/jixoai/keel/internal/store"

	boardaudit "github.com/jixoai/keel-board/audit"
	boardsync "github.com/jixoai/keel-board/sync"
)

// testEnv is the real-PostgreSQL fixture for the Phase 1 board surface. The
// board tables live in a throwaway schema; keel's own tables stay in the test
// database's public schema with fresh tenant UUIDs.
type testEnv struct {
	t        *testing.T
	dsn      string
	boardDSN string
	schema   string
	tenant   uuid.UUID
	other    uuid.UUID
	st       *keelstore.Store
	bridge   *accesswire.Bridge
	surf     *Surface

	alice   kAccount
	bob     kAccount
	carol   kAccount
	manager kAccount
	dave    kAccount

	bot    keelstore.BotRecord
	botKey string // the board profile token
	box    *keelstore.SecretBox
}

type kAccount struct {
	record keelstore.AccountRecord
	botID  string
}

func newTestEnv(t *testing.T, mutate func(*Options)) *testEnv {
	t.Helper()
	return newTestEnvBefore(t, mutate, nil)
}

// newTestEnvBefore is newTestEnv with a hook that runs after the keel fixture
// exists but before the board surface is built. Sync tests use it to write the
// tenant's entry/secret rows that the surface assembles at boot.
func newTestEnvBefore(t *testing.T, mutate func(*Options), before func(*testEnv)) *testEnv {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := keelstore.Open(ctx, keelstore.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open keel store: %v", err)
	}
	t.Cleanup(st.Close)

	env := &testEnv{
		t: t, dsn: dsn, st: st,
		schema: fmt.Sprintf("board_phase1_%s", strings.ReplaceAll(uuid.NewString()[:8], "-", "")),
		tenant: uuid.New(), other: uuid.New(),
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate keel schema: %v", err)
	}
	for _, tenant := range []uuid.UUID{env.tenant, env.other} {
		if err := st.CreateTenant(ctx, tenant, "board-"+tenant.String()[:8]); err != nil {
			t.Fatalf("create tenant: %v", err)
		}
		if err := accesswire.EnsureBuiltinGroups(ctx, st, tenant); err != nil {
			t.Fatalf("seed builtin groups: %v", err)
		}
		// Board v1 defaults both switches off (design §20); the fixture opts
		// both tenants in so the RBAC/identity assertions reach the board.
		if _, err := st.SetTenantConfig(ctx, tenant, json.RawMessage(`{"board":{"enabled":true,"sync":{"enabled":true}}}`)); err != nil {
			t.Fatalf("enable board flags: %v", err)
		}
	}
	env.alice = env.account(env.tenant, "alice", "human")
	env.bob = env.account(env.tenant, "bob", "human")
	env.carol = env.account(env.tenant, "carol", "human")
	env.manager = env.account(env.tenant, "manager", "human")
	env.dave = env.account(env.other, "dave", "human")
	// The Bot belongs to Alice; a Bot acts through its Profile's Account.
	env.bot = env.makeBot(env.tenant, env.alice.record.ID, "researcher")
	env.botKey = BotProfileToken(env.bot.ID)
	box, err := keelstore.NewSecretBox(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	env.box = box

	// Alice administers the board and writes; Bob writes but does not
	// administer; Carol reads only.
	for _, key := range []string{access.KeyBoardRead, access.KeyBoardWrite, access.KeyBoardManage} {
		env.grant(env.tenant, env.alice.record.ID, key)
	}
	for _, key := range []string{access.KeyBoardRead, access.KeyBoardWrite} {
		env.grant(env.tenant, env.bob.record.ID, key)
	}
	env.grant(env.tenant, env.carol.record.ID, access.KeyBoardRead)
	// The manager holds only the tenant-level administration key: it must not
	// gain channel content without a relation.
	env.grant(env.tenant, env.manager.record.ID, access.KeyBoardManage)
	for _, key := range []string{access.KeyBoardRead, access.KeyBoardWrite, access.KeyBoardManage} {
		env.grant(env.other, env.dave.record.ID, key)
	}

	bridge, err := accesswire.NewBridge(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("access bridge: %v", err)
	}
	env.bridge = bridge
	options := Options{
		DSN:                 dsn,
		Schema:              env.schema,
		Access:              bridge,
		Store:               st,
		TenantID:            env.tenant,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		Heartbeat:           60 * time.Millisecond,
		StreamLifetime:      -1,
		ChannelScanInterval: -1,
		TenantFlagsTTL:      -1,
		SecretBox:           env.box,
		BlobDir:             t.TempDir(),
		DisableAutomation:   true,
	}
	if mutate != nil {
		mutate(&options)
	}
	if options.Schema != "" {
		env.schema = options.Schema
	}
	env.boardDSN = options.DSN
	if before != nil {
		before(env)
	}
	surface, err := New(ctx, options)
	if err != nil {
		t.Fatalf("board surface: %v", err)
	}
	env.surf = surface
	t.Cleanup(func() {
		surface.Close()
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = st.Pool().Exec(dropCtx, "DROP SCHEMA IF EXISTS "+quoteIdent(env.schema)+" CASCADE")
	})
	return env
}

func (e *testEnv) account(tenant uuid.UUID, username, kind string) kAccount {
	e.t.Helper()
	record, err := e.st.CreateAccount(context.Background(), keelstore.AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: username + "-" + tenant.String()[:8],
		DisplayName: strings.ToUpper(username[:1]) + username[1:], Kind: kind,
	})
	if err != nil {
		e.t.Fatalf("create account %s: %v", username, err)
	}
	return kAccount{record: record}
}

func (e *testEnv) makeBot(tenant, accountID uuid.UUID, botID string) keelstore.BotRecord {
	e.t.Helper()
	role, err := e.st.CreateRole(context.Background(), keelstore.RoleRecord{
		TenantID: tenant, ID: uuid.New(), Name: "Board " + botID,
	})
	if err != nil {
		e.t.Fatalf("create role: %v", err)
	}
	profile, err := e.st.CreateProfile(context.Background(), keelstore.ProfileRecord{
		TenantID: tenant, ID: uuid.New(), AccountID: accountID, RoleID: role.ID,
	})
	if err != nil {
		e.t.Fatalf("create profile: %v", err)
	}
	harness, _ := json.Marshal(map[string]any{
		"tools": []string{},
		"context": map[string]any{
			"permissions": []string{access.KeyBoardRead, access.KeyBoardWrite, access.KeyBoardManage},
		},
	})
	bot, err := e.st.CreateBot(context.Background(), keelstore.BotRecord{
		TenantID: tenant, ID: botID, Name: "Researcher", ProfileID: profile.ID,
		ModelKind: "text", DefaultModel: json.RawMessage(`{"providerId":"fake","modelId":"fake"}`),
		Harness: harness,
	})
	if err != nil {
		e.t.Fatalf("create bot: %v", err)
	}
	return bot
}

func (e *testEnv) grant(tenant, account uuid.UUID, key string) {
	e.t.Helper()
	_, err := e.st.CreatePermissionGrant(context.Background(), keelstore.PermissionGrantRecord{
		TenantID: tenant, ID: uuid.New(), SubjectType: keelstore.PermissionSubjectAccount,
		SubjectID: account, PermissionKey: key, Scope: keelstore.PermissionScopeTenant,
		GrantedBy: account,
	})
	if err != nil {
		e.t.Fatalf("grant %s: %v", key, err)
	}
}

func (e *testEnv) relation(tenant uuid.UUID, channel, relation string, account uuid.UUID) {
	e.t.Helper()
	_, err := e.st.PutResourceRelation(context.Background(), keelstore.ResourceRelation{
		TenantID: tenant, ResourceType: BoardChannelResourceType, ResourceID: channel,
		Relation: relation, SubjectType: keelstore.RelationSubjectAccount,
		SubjectID: account.String(), CreatedBy: account,
	})
	if err != nil {
		e.t.Fatalf("put relation: %v", err)
	}
}

func (e *testEnv) principal(who string) (auth.Principal, bool) {
	switch who {
	case "alice":
		return e.human("alice", e.alice.record), true
	case "bob":
		return e.human("bob", e.bob.record), true
	case "carol":
		return e.human("carol", e.carol.record), true
	case "manager":
		return e.human("manager", e.manager.record), true
	case "dave":
		return e.human("dave", e.dave.record), true
	case "bot":
		return auth.Principal{
			Kind: auth.PrincipalBot, TenantID: e.tenant, AccountID: e.alice.record.ID,
			BotID: e.bot.ID, Username: e.alice.record.Username, DisplayName: "Alice",
		}, true
	default:
		return auth.Principal{}, false
	}
}

// contextAndActor builds a request context carrying who's edge principal and
// the board Actor that principal resolves to. It is the direct-authorizer
// fixture (no HTTP layer).
func (e *testEnv) contextAndActor(who string) (context.Context, protocol.Actor) {
	e.t.Helper()
	principal, ok := e.principal(who)
	if !ok {
		e.t.Fatalf("no principal %q", who)
	}
	ctx := withTestPrincipal(context.Background(), principal)
	actor, err := (&Identity{Store: e.st}).Actor(testRequest(ctx))
	if err != nil {
		e.t.Fatalf("actor %q: %v", who, err)
	}
	return ctx, actor
}

func (e *testEnv) human(name string, record keelstore.AccountRecord) auth.Principal {
	return auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: record.TenantID, AccountID: record.ID,
		Username: record.Username, DisplayName: record.DisplayName,
	}
}

// server starts an httptest server with a stand-in for the auth edge: it reads
// the test's actor header and injects the edge principal into the request
// context exactly where cmd/keel's middleware does.
func (e *testEnv) server() *httptest.Server {
	e.t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal, ok := e.principal(r.Header.Get("X-Test-Who")); ok {
			r = r.WithContext(auth.WithPrincipal(r.Context(), principal))
		}
		e.surf.ServeHTTP(w, r)
	})
	server := httptest.NewServer(handler)
	e.t.Cleanup(server.Close)
	return server
}

type response struct {
	status int
	body   []byte
}

func (e *testEnv) do(t *testing.T, server *httptest.Server, method, path, who string, body any) response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, server.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	if who != "" {
		request.Header.Set("X-Test-Who", who)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return response{status: resp.StatusCode, body: payload}
}

func (e *testEnv) decode(t *testing.T, resp response, target any) {
	t.Helper()
	if resp.status < 200 || resp.status >= 300 {
		t.Fatalf("status %d: %s", resp.status, resp.body)
	}
	if err := json.Unmarshal(resp.body, target); err != nil {
		t.Fatalf("decode %s: %v", resp.body, err)
	}
}

func (e *testEnv) boardError(t *testing.T, resp response, wantStatus int, wantCode string) map[string]any {
	t.Helper()
	if resp.status != wantStatus {
		t.Fatalf("status = %d, want %d: %s", resp.status, wantStatus, resp.body)
	}
	var decoded struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.body, &decoded); err != nil {
		t.Fatalf("decode error body %s: %v", resp.body, err)
	}
	if decoded.Error.Code != wantCode {
		t.Fatalf("code = %q, want %q: %s", decoded.Error.Code, wantCode, resp.body)
	}
	return map[string]any{"message": decoded.Error.Message, "hint": decoded.Error.Hint}
}

func quoteIdent(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func boardMirror(channel string) boardsync.SyncFunc { return boardsync.Mirror(channel) }

// withTestPrincipal carries an edge principal on a bare context for direct
// authorizer tests.
func withTestPrincipal(ctx context.Context, principal auth.Principal) context.Context {
	return auth.WithPrincipal(ctx, principal)
}

// testRequest builds a request whose context carries the edge principal.
func testRequest(ctx context.Context) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/v1/board", nil).WithContext(ctx)
}

// newTestServer wraps a handler in an httptest server.
func newTestServer(handler http.Handler) *httptest.Server {
	return httptest.NewServer(handler)
}

// envDo performs one unauthenticated JSON request against a test server.
func envDo(t *testing.T, server *httptest.Server, method, path string) response {
	t.Helper()
	request, err := http.NewRequest(method, server.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response{status: resp.StatusCode, body: body}
}

// boardAuditEvent is a bounded operational audit row the adapter must persist
// without carrying a body or a credential.
func boardAuditEvent(tenant string) boardaudit.Event {
	return boardaudit.Event{
		Tenant: tenant, Kind: boardaudit.KindWebhookAccepted,
		Key: "fake", Platform: "fake", Status: boardaudit.StatusAccepted,
		Attempts: 1, Detail: map[string]any{"status": "ok"},
	}
}
