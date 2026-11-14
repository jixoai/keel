// Tenancy wiring tests for `keel serve` and `keel admin`: the CLI provisioning
// path (plaintext shown once, nothing else prints it), the API-key middleware
// chain, and the per-tenant edge quota middleware (change keel-ga §2).

package main

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/store"
)

// testStore opens a dedicated database for one test so the CLI assertions do
// not collide with other packages sharing KEEL_TEST_DSN.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("KEEL_TEST_DSN"))
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// TestAdminTenantCreatePrintsKeyOnce pins the CLI contract: the plaintext key
// appears exactly once on stdout, only its hash reaches the database, and the
// printed key actually resolves back to the tenant.
func TestAdminTenantCreatePrintsKeyOnce(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KEEL_TEST_DSN"))
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	// adminStore reads KEEL_DSN; point it at the test database for the duration.
	t.Setenv("KEEL_DSN", dsn)
	var stdout, stderr strings.Builder
	code := runAdminCmd([]string{"tenant", "create", "--name", "cli-tenant", "--label", "cli"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	// The key line is labelled and in the documented format.
	const marker = "API KEY (shown once, store it now):\n"
	index := strings.Index(out, marker)
	if index < 0 {
		t.Fatalf("no key section in output:\n%s", out)
	}
	rest := out[index+len(marker):]
	keyLine, _, found := strings.Cut(rest, "\n")
	if !found {
		t.Fatalf("key line is not terminated:\n%s", out)
	}
	key := strings.TrimSpace(keyLine)
	if !auth.LooksLikeKey(key) {
		t.Fatalf("printed credential %q is not a key", key)
	}
	// The plaintext appears exactly once in the whole output.
	if strings.Count(out, key) != 1 {
		t.Fatalf("the key appears %d times in the output", strings.Count(out, key))
	}
	// Nothing key-shaped beyond the one credential (the prefix is also printed).
	if strings.Count(out, "keel_tenant_") != 2 {
		t.Fatalf("unexpected key-shaped output:\n%s", out)
	}
	// The DSN is never echoed.
	if strings.Contains(out, dsn) || strings.Contains(stderr.String(), dsn) {
		t.Fatal("the command printed the DSN")
	}

	// The key resolves to the printed tenant.
	st, err := store.Open(context.Background(), store.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tenantID := uuid.MustParse(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0][len("tenant:  "):]))
	resolved, err := st.LookupAPIKey(context.Background(), auth.HashKey(key))
	if err != nil {
		t.Fatalf("the printed key does not resolve: %v", err)
	}
	if resolved.TenantID != tenantID {
		t.Fatalf("key resolves to %s, want %s", resolved.TenantID, tenantID)
	}
	// The database holds the hash, never the plaintext.
	var stored []byte
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT key_hash FROM api_keys WHERE tenant_id = $1`, tenantID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(key))
	if string(stored) != string(sum[:]) {
		t.Fatal("the stored hash is not the hash of the printed key")
	}
}

func TestAdminKeyCreateAndRevoke(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KEEL_TEST_DSN"))
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	t.Setenv("KEEL_DSN", dsn)
	var stdout, stderr strings.Builder
	if code := runAdminCmd([]string{"tenant", "create", "--name", "cli-keys", "--no-key"}, &stdout, &stderr); code != 0 {
		t.Fatalf("create tenant: %d %s", code, stderr.String())
	}
	tenantID := strings.TrimSpace(strings.SplitN(stdout.String(), "\n", 2)[0][len("tenant:  "):])

	stdout.Reset()
	stderr.Reset()
	if code := runAdminCmd([]string{"key", "create", "--tenant", tenantID, "--role", "admin"}, &stdout, &stderr); code != 0 {
		t.Fatalf("create key: %d %s", code, stderr.String())
	}
	out := stdout.String()
	keyIndex := strings.Index(out, "API KEY (shown once, store it now):\n")
	if keyIndex < 0 {
		t.Fatalf("no key in output:\n%s", out)
	}
	key := strings.TrimSpace(strings.SplitN(out[keyIndex+len("API KEY (shown once, store it now):\n"):], "\n", 2)[0])
	if !strings.HasPrefix(key, auth.KeyScheme+auth.RoleAdmin+"_") {
		t.Fatalf("admin key %q has the wrong role prefix", key)
	}
	keyIDIndex := strings.Index(out, "key id:  ")
	if keyIDIndex < 0 {
		t.Fatalf("no key id in output:\n%s", out)
	}
	keyID := strings.TrimSpace(strings.SplitN(out[keyIDIndex+len("key id:  "):], "\n", 2)[0])

	stdout.Reset()
	stderr.Reset()
	if code := runAdminCmd([]string{"key", "revoke", "--tenant", tenantID, "--key", keyID}, &stdout, &stderr); code != 0 {
		t.Fatalf("revoke: %d %s", code, stderr.String())
	}
	st, err := store.Open(context.Background(), store.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.LookupAPIKey(context.Background(), auth.HashKey(key)); err == nil {
		t.Fatal("a revoked key still resolves")
	}
	// Revoking a key the tenant does not own is a non-zero exit, not a silent OK.
	stderr.Reset()
	if code := runAdminCmd([]string{"key", "revoke", "--tenant", uuid.NewString(), "--key", keyID}, io_discard{}, &stderr); code == 0 {
		t.Fatal("revoking under the wrong tenant succeeded")
	}
}

// io_discard is an io.Writer that drops everything (the CLI must not need a
// buffer to be exercised).
type io_discard struct{}

func (io_discard) Write(p []byte) (int, error) { return len(p), nil }

func TestAdminUsageAndQuotaCommands(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KEEL_TEST_DSN"))
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	t.Setenv("KEEL_DSN", dsn)
	var stdout, stderr strings.Builder
	if code := runAdminCmd([]string{"tenant", "create", "--name", "cli-usage", "--no-key"}, &stdout, &stderr); code != 0 {
		t.Fatalf("create: %d %s", code, stderr.String())
	}
	tenantID := strings.TrimSpace(strings.SplitN(stdout.String(), "\n", 2)[0][len("tenant:  "):])

	stdout.Reset()
	stderr.Reset()
	if code := runAdminCmd([]string{"quota", "set", "--tenant", tenantID, "--streams", "7", "--storage-bytes", "1024"}, &stdout, &stderr); code != 0 {
		t.Fatalf("quota set: %d %s", code, stderr.String())
	}
	stdout.Reset()
	if code := runAdminCmd([]string{"quota", "show", "--tenant", tenantID}, &stdout, &stderr); code != 0 {
		t.Fatalf("quota show: %d %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "streams:    7") {
		t.Fatalf("quota show did not report the new limit:\n%s", stdout.String())
	}

	stdout.Reset()
	if code := runAdminCmd([]string{"usage", "show", "--tenant", tenantID}, &stdout, &stderr); code != 0 {
		t.Fatalf("usage show: %d %s", code, stderr.String())
	}
	usageOut := stdout.String()
	if !strings.Contains(usageOut, "tokens") || !strings.Contains(usageOut, "bytes") {
		t.Fatalf("usage output lacks the two dimensions:\n%s", usageOut)
	}
	for _, forbidden := range []string{"price", "amount", "currency", "cost"} {
		if strings.Contains(strings.ToLower(usageOut), forbidden) {
			t.Fatalf("usage output carries %q:\n%s", forbidden, usageOut)
		}
	}
}

// TestEdgeQuotaMiddlewareRejectsSaturatedTenant pins the wiring: the middleware
// charges the resolved tenant, a saturated tenant is 429 with Retry-After, and
// the neighbouring tenant keeps its capacity.
func TestEdgeQuotaMiddlewareRejectsSaturatedTenant(t *testing.T) {
	full, other := uuid.New(), uuid.New()
	src := quota.QuotaSourceFunc(func(_ context.Context, tenantID uuid.UUID) (quota.TenantQuota, error) {
		if tenantID == full || tenantID == other {
			return quota.TenantQuota{Streams: 1}, nil
		}
		return quota.TenantQuota{}, nil
	})
	edge := quota.NewEdge(src, nil)
	var reached int
	middleware := &edgeQuotaMiddleware{Edge: edge, Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	})}
	server := httptest.NewServer(middleware)
	defer server.Close()

	// Hold one stream for `full` so the next one is refused.
	release, _, err := edge.AcquireStream(context.Background(), full)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/sessions/x/events/stream", nil)
	req.Header.Set("X-Tenant-ID", full.String())
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("saturated tenant stream: %d, want 429", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Fatal("the 429 carries no Retry-After")
	}
	// The other tenant is unaffected.
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/sessions/x/events/stream", nil)
	req.Header.Set("X-Tenant-ID", other.String())
	res, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("neighbour tenant: %d, want 200", res.StatusCode)
	}
	release()

	// A JSON path is not charged at all (the middleware is a no-op there).
	before := edge.ActiveStreams(full)
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/sessions", nil)
	req.Header.Set("X-Tenant-ID", full.String())
	res, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("non-stream path: %d", res.StatusCode)
	}
	if edge.ActiveStreams(full) != before {
		t.Fatal("a JSON path consumed a stream slot")
	}
}

// TestEdgeQuotaMiddlewareRejectsOversizedUpload pins the 413 dimension.
func TestEdgeQuotaMiddlewareRejectsOversizedUpload(t *testing.T) {
	tenant := uuid.New()
	src := quota.QuotaSourceFunc(func(context.Context, uuid.UUID) (quota.TenantQuota, error) {
		return quota.TenantQuota{Streams: 5, StorageBytes: 1000}, nil
	})
	edge := quota.NewEdge(src, func(context.Context, uuid.UUID) (int64, error) { return 900, nil })
	var reached bool
	middleware := &edgeQuotaMiddleware{Edge: edge, Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusCreated)
	})}
	server := httptest.NewServer(middleware)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/files", strings.NewReader(strings.Repeat("x", 200)))
	req.Header.Set("X-Tenant-ID", tenant.String())
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d, want 413", res.StatusCode)
	}
	if reached {
		t.Fatal("the refused upload reached the handler")
	}
	// A write that fits passes and consumes a stream slot that is then released.
	reached = false
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/files", strings.NewReader(strings.Repeat("x", 50)))
	req.Header.Set("X-Tenant-ID", tenant.String())
	res, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated || !reached {
		t.Fatalf("fitting upload: %d reached=%v", res.StatusCode, reached)
	}
	if got := edge.ActiveStreams(tenant); got != 0 {
		t.Fatalf("the upload slot leaked: %d active", got)
	}
}

// TestAPIKeyAuthorizerWrapsServeChain pins the composition order of the serving
// chain: a key request is authenticated and reaches the dispatch, while a
// keyless request reaches the cookie handler untouched.
func TestAPIKeyAuthorizerWrapsServeChain(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	if err := st.CreateTenant(ctx, tenantID, "serve-chain"); err != nil {
		t.Fatal(err)
	}
	minted, err := auth.MintKey(auth.RoleTenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAPIKey(ctx, tenantID, uuid.New(), minted.Hash, minted.Prefix, minted.Role, ""); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var seen []string
	var seenTenants []string
	// productAuth stands in for the cookie/passphrase handler; dispatch is what
	// it forwards to. A key request reaches dispatch with the key's tenant; a
	// keyless request is recorded by the cookie handler (which then forwards in
	// production), proving the authorizer did not consume it.
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, "dispatch:"+r.URL.Path)
		seenTenants = append(seenTenants, r.Header.Get(auth.HeaderTenantID))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	productAuth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, "cookie:"+r.URL.Path)
		mu.Unlock()
		dispatch.ServeHTTP(w, r)
	})
	chain := &auth.Authorizer{Keys: apiKeyStore{store: st}, Verified: dispatch, Next: productAuth}
	server := httptest.NewServer(chain)
	defer server.Close()

	// Key request: authenticated, tenant injected, dispatch reached.
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Plaintext)
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("key request: %d", res.StatusCode)
	}
	mu.Lock()
	if len(seen) != 1 || seen[0] != "dispatch:/v1/sessions" {
		mu.Unlock()
		t.Fatalf("key request path = %v (the cookie handler must not see a key request)", seen)
	}
	if seenTenants[0] != tenantID.String() {
		mu.Unlock()
		t.Fatalf("dispatched tenant = %q", seenTenants[0])
	}
	mu.Unlock()

	// Keyless request: the cookie handler owns it.
	mu.Lock()
	seen = nil
	seenTenants = nil
	mu.Unlock()
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/sessions", nil)
	res, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("keyless request: %d", res.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "cookie:/v1/sessions" || seen[1] != "dispatch:/v1/sessions" {
		t.Fatalf("keyless path = %v", seen)
	}
	if seenTenants[0] != "" || len(seenTenants) != 1 {
		t.Fatalf("the authorizer wrote a tenant on a keyless request: %v", seenTenants)
	}
}
