// Tenancy surface tests: who-am-I, provisioning (plaintext key shown once), the
// quota projection, and the 403/401 operator boundary (change keel-ga §2).

package agentapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/quota"
)

// fakeProvisioner is the Provisioner seam with the store's semantics (idempotent
// tenant create, hashes never echoed).
type fakeProvisioner struct {
	mu       sync.Mutex
	tenants  map[uuid.UUID]string
	keys     map[uuid.UUID][]APIKeyView
	minted   map[uuid.UUID][]byte
	revoked  map[uuid.UUID]bool
	keyOrder []uuid.UUID
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{
		tenants: map[uuid.UUID]string{},
		keys:    map[uuid.UUID][]APIKeyView{},
		minted:  map[uuid.UUID][]byte{},
		revoked: map[uuid.UUID]bool{},
	}
}

func (f *fakeProvisioner) CreateTenant(_ context.Context, tenantID uuid.UUID, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tenants[tenantID]; ok {
		return false, nil
	}
	f.tenants[tenantID] = name
	return true, nil
}

func (f *fakeProvisioner) MintAPIKey(_ context.Context, tenantID, keyID uuid.UUID, hash []byte, prefix, role, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minted[keyID] = append([]byte(nil), hash...)
	f.keyOrder = append(f.keyOrder, keyID)
	f.keys[tenantID] = append(f.keys[tenantID], APIKeyView{
		KeyID: keyID.String(), Prefix: prefix, Role: role, Label: label, CreatedAt: time.Now().UTC(),
	})
	return nil
}

func (f *fakeProvisioner) TenantExists(_ context.Context, tenantID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tenants[tenantID]
	return ok, nil
}

func (f *fakeProvisioner) ListAPIKeys(_ context.Context, tenantID uuid.UUID) ([]APIKeyView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]APIKeyView(nil), f.keys[tenantID]...), nil
}

func (f *fakeProvisioner) RevokeAPIKey(_ context.Context, tenantID, keyID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.keys[tenantID] {
		if f.keys[tenantID][i].KeyID == keyID.String() {
			f.revoked[keyID] = true
			return true, nil
		}
	}
	return false, nil
}

// fakeQuota is the QuotaReporter seam.
type fakeQuota struct {
	limits  quota.TenantQuota
	stored  int64
	streams map[uuid.UUID]int
}

func (f fakeQuota) TenantQuota(context.Context, uuid.UUID) (quota.TenantQuota, error) {
	return f.limits, nil
}

func (f fakeQuota) StoredBytes(context.Context, uuid.UUID) (int64, error) { return f.stored, nil }

func (f fakeQuota) ActiveStreams(tenantID uuid.UUID) int { return f.streams[tenantID] }

// identityServer wraps a handler the way the auth middleware does on the
// SERVER side. The identity must be attached there: a request context is
// client-side state and never crosses the wire, so a test that set it on the
// outgoing request would prove nothing.
func identityServer(next http.Handler, identity auth.Identity) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), identity)))
	}))
}

// principalServer attaches the edge-resolved auth principal the way cmd/keel's
// middleware does.
func principalServer(next http.Handler, principal auth.Principal) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
}

func TestWhoAmIReportsIdentityAndPermissions(t *testing.T) {
	tenantID, accountID := uuid.New(), uuid.New()
	fake := &fakeAccess{
		principal: access.Principal{TenantID: tenantID, AccountID: accountID},
		grants:    access.Grants{access.KeySessionRead: access.ScopeOwn},
	}
	edge := auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: tenantID, AccountID: accountID,
		Username: "alice", DisplayName: "Alice",
	}
	server := principalServer(New(Config{Access: fake, Logger: testLogger()}), edge)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/auth/whoami", nil)
	req.Header.Set("X-Tenant-ID", tenantID.String())
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("whoami: %d %v", res.StatusCode, body)
	}
	if body["tenantId"] != tenantID.String() {
		t.Fatalf("tenantId = %v", body["tenantId"])
	}
	account, _ := body["account"].(map[string]any)
	if account["id"] != accountID.String() || account["username"] != "alice" {
		t.Fatalf("account = %v", account)
	}
	permissions, _ := body["permissions"].(map[string]any)
	if permissions[access.KeySessionRead] != string(access.ScopeOwn) {
		t.Fatalf("permissions = %v", permissions)
	}
	// The old tenant role is gone, and no credential material is echoed.
	if _, ok := body["role"]; ok {
		t.Fatal("whoami still returns the old tenant role")
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "keel_bot_") || strings.Contains(string(raw), "key_hash") {
		t.Fatal("whoami leaked credential material")
	}
}

func TestWhoAmIReportsBotIdentity(t *testing.T) {
	tenantID, accountID, keyID := uuid.New(), uuid.New(), uuid.New()
	fake := &fakeAccess{
		principal: access.Principal{TenantID: tenantID, AccountID: accountID, BotID: "support-1"},
		grants:    access.Grants{access.KeySessionRead: access.ScopeTeam},
	}
	edge := auth.Principal{
		Kind: auth.PrincipalBot, TenantID: tenantID, AccountID: accountID, BotID: "support-1",
		KeyID: keyID, KeyPrefix: "keel_bot_abcd", Username: "bot-account",
	}
	server := principalServer(New(Config{Access: fake, Logger: testLogger()}), edge)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/auth/whoami", nil)
	req.Header.Set("X-Tenant-ID", tenantID.String())
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	bot, _ := body["bot"].(map[string]any)
	if bot["id"] != "support-1" {
		t.Fatalf("bot = %v", bot)
	}
	if body["keyId"] != keyID.String() || body["keyPrefix"] != "keel_bot_abcd" {
		t.Fatalf("key identity = %v / %v", body["keyId"], body["keyPrefix"])
	}
}

func TestWhoAmIFailsClosedWithoutAccess(t *testing.T) {
	tenantID := uuid.New()
	server := principalServer(New(Config{Logger: testLogger()}), auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: tenantID, AccountID: uuid.New(),
	})
	defer server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/auth/whoami", nil)
	req.Header.Set("X-Tenant-ID", tenantID.String())
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unwired whoami = %d, want 503", res.StatusCode)
	}
}

func TestProvisionTenantReturnsKeyOnce(t *testing.T) {
	provisioner := newFakeProvisioner()
	server := httptest.NewServer(New(Config{
		Provisioner: provisioner,
		AdminKey:    func(*http.Request) bool { return true },
		Logger:      testLogger(),
	}))
	defer server.Close()

	var created struct {
		TenantID string `json:"tenantId"`
		Created  bool   `json:"created"`
		APIKey   struct {
			KeyID  string `json:"keyId"`
			Prefix string `json:"prefix"`
			Role   string `json:"role"`
			Key    string `json:"key"`
			Notice string `json:"notice"`
		} `json:"apiKey"`
	}
	if code := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/admin/tenants", uuid.Nil,
		map[string]any{"name": "acme"}, &created); code != http.StatusCreated {
		t.Fatalf("provision: %d", code)
	}
	if !created.Created || created.APIKey.Key == "" {
		t.Fatalf("body = %+v", created)
	}
	if !strings.HasPrefix(created.APIKey.Key, auth.KeyScheme+auth.RoleTenant+"_") {
		t.Fatalf("key %q is not in the tenant key format", created.APIKey.Key)
	}
	if created.APIKey.Notice == "" {
		t.Fatal("the answer does not warn that the key is shown once")
	}
	// The minted hash is the hash of the returned plaintext, and the plaintext
	// itself is nowhere in the provisioner's state. The snapshot is taken under
	// the lock and inspected after it is released: holding a fake's mutex across
	// an HTTP call deadlocks the server goroutine that needs the same mutex.
	provisioner.mu.Lock()
	hash := append([]byte(nil), provisioner.minted[uuid.MustParse(created.APIKey.KeyID)]...)
	views := append([]APIKeyView(nil), provisioner.keys[uuid.MustParse(created.TenantID)]...)
	provisioner.mu.Unlock()
	if string(hash) != string(auth.HashKey(created.APIKey.Key)) {
		t.Fatal("the stored hash does not match the returned key")
	}
	for _, view := range views {
		if strings.Contains(view.Prefix, strings.TrimPrefix(created.APIKey.Key, created.APIKey.Prefix)) {
			t.Fatal("the store kept part of the secret beyond the prefix")
		}
	}

	// Listing shows the prefix only: no route can reproduce the key.
	var listed struct {
		Keys []map[string]any `json:"keys"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/admin/tenants/"+created.TenantID+"/keys", uuid.Nil, nil, &listed); code != 200 {
		t.Fatalf("list keys: %d", code)
	}
	if len(listed.Keys) != 1 {
		t.Fatalf("keys = %v", listed.Keys)
	}
	if _, present := listed.Keys[0]["key"]; present {
		t.Fatal("the key list carries the plaintext")
	}

	// A re-provision of the same id reports created=false and does not rewrite
	// the name.
	var again struct {
		Created bool `json:"created"`
	}
	if code := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/admin/tenants", uuid.Nil,
		map[string]any{"name": "renamed", "tenantId": created.TenantID, "noApiKey": true}, &again); code != 201 {
		t.Fatalf("re-provision: %d", code)
	}
	if again.Created {
		t.Fatal("re-provision reported a creation")
	}
}

func TestProvisioningRequiresAdminCredential(t *testing.T) {
	provisioner := newFakeProvisioner()
	server := httptest.NewServer(New(Config{
		Provisioner: provisioner,
		// No AdminKey seam and (in this process) no KEEL_ADMIN_KEY: the route
		// must answer 401, never 200.
		Logger: testLogger(),
	}))
	defer server.Close()
	if code := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/admin/tenants", uuid.Nil,
		map[string]any{"name": "nope"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated provision: %d, want 401", code)
	}

	// A valid tenant key is authenticated but not authorized: 403, not 401.
	tenantServer := identityServer(New(Config{Provisioner: provisioner, Logger: testLogger()}),
		auth.Identity{TenantID: uuid.New(), KeyID: uuid.New(), Role: auth.RoleTenant})
	defer tenantServer.Close()
	if code := doJSON(t, tenantServer.Client(), http.MethodPost, tenantServer.URL+"/v1/admin/tenants",
		uuid.Nil, map[string]any{"name": "nope"}, nil); code != http.StatusForbidden {
		t.Fatalf("tenant credential on provisioning: %d, want 403", code)
	}
	if len(provisioner.tenants) != 0 {
		t.Fatalf("a refused call created %d tenants", len(provisioner.tenants))
	}

	// An admin key identity passes.
	adminServer := identityServer(New(Config{Provisioner: provisioner, Logger: testLogger()}),
		auth.Identity{TenantID: uuid.New(), KeyID: uuid.New(), Role: auth.RoleAdmin})
	defer adminServer.Close()
	if code := doJSON(t, adminServer.Client(), http.MethodPost, adminServer.URL+"/v1/admin/tenants",
		uuid.Nil, map[string]any{"name": "ok", "noApiKey": true}, nil); code != http.StatusCreated {
		t.Fatalf("admin credential: %d", code)
	}
}

func TestQuotaProjectionHasNoMoney(t *testing.T) {
	tenantID := uuid.New()
	server := httptest.NewServer(New(Config{
		Quota: fakeQuota{
			limits:  quota.TenantQuota{Streams: 4, BytesPerSec: 1 << 20, StorageBytes: 1 << 30},
			stored:  1234,
			streams: map[uuid.UUID]int{tenantID: 2},
		},
		Logger: testLogger(),
	}))
	defer server.Close()

	var body map[string]any
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/quota", tenantID, nil, &body); code != 200 {
		t.Fatalf("quota: %d", code)
	}
	raw, _ := json.Marshal(body)
	for _, forbidden := range []string{"price", "amount", "currency", "cost"} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Fatalf("the quota answer carries %q: %s", forbidden, raw)
		}
	}
	usage, ok := body["usage"].(map[string]any)
	if !ok {
		t.Fatalf("body = %v", body)
	}
	if usage["storageBytes"] != float64(1234) || usage["streams"] != float64(2) {
		t.Fatalf("usage = %v", usage)
	}

	// Without the seam the route is 501, not a zeroed answer that looks real.
	bare := httptest.NewServer(New(Config{Logger: testLogger()}))
	defer bare.Close()
	if code := doJSON(t, bare.Client(), http.MethodGet, bare.URL+"/v1/quota", tenantID, nil, nil); code != http.StatusNotImplemented {
		t.Fatalf("unwired quota: %d, want 501", code)
	}
}

// scopedLister answers a fixed page only to its owning tenant.
type scopedLister struct {
	owner    uuid.UUID
	sessions []SessionSummary
}

func (l *scopedLister) ListSessionSummaries(_ context.Context, tenantID uuid.UUID, _ int, _ string) (SessionPage, error) {
	if tenantID != l.owner {
		return SessionPage{}, nil
	}
	return SessionPage{Sessions: append([]SessionSummary(nil), l.sessions...)}, nil
}

// TestCrossTenantSessionIs404 pins the spec's "跨租户 404": a session that
// belongs to another tenant answers exactly like one that does not exist. The
// lister is tenant-scoped, so the assertion is that the route never leaks the
// row and always answers 404.
func TestCrossTenantSessionIs404(t *testing.T) {
	tenantA, tenantB := uuid.New(), uuid.New()
	secret := uuid.New()
	// A lister that answers strictly by tenant is what production does (every
	// query carries tenant_id); this fake mirrors that so the route-level
	// assertion is about the answer, not about the fake's convenience.
	lister := &scopedLister{owner: tenantA, sessions: []SessionSummary{
		{SessionID: secret.String(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Title: "belongs to A"},
	}}
	archive := func(_ context.Context, tenantID, sessionID uuid.UUID) (bool, error) {
		return tenantID == tenantA && sessionID == secret, nil
	}
	server := httptest.NewServer(New(Config{Sessions: lister, ArchiveSession: archive, Access: permitAllAccess(), Logger: testLogger()}))
	defer server.Close()

	// Tenant A sees it.
	var page struct {
		Sessions []SessionSummary `json:"sessions"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions", tenantA, nil, &page); code != 200 {
		t.Fatalf("tenant A list: %d", code)
	}
	if len(page.Sessions) != 1 {
		t.Fatalf("tenant A sees %d sessions, want 1", len(page.Sessions))
	}
	// Tenant B sees an empty list, not A's session.
	page = struct {
		Sessions []SessionSummary `json:"sessions"`
	}{}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions", tenantB, nil, &page); code != 200 {
		t.Fatalf("tenant B list: %d", code)
	}
	if len(page.Sessions) != 0 {
		t.Fatalf("tenant B sees %d of A's sessions", len(page.Sessions))
	}
	// Deleting A's session as B is a 404 — the same answer as a missing id.
	if code := doJSON(t, server.Client(), http.MethodDelete, server.URL+"/v1/sessions/"+secret.String(), tenantB, nil, nil); code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete: %d, want 404", code)
	}
	if code := doJSON(t, server.Client(), http.MethodDelete, server.URL+"/v1/sessions/"+uuid.NewString(), tenantB, nil, nil); code != http.StatusNotFound {
		t.Fatalf("missing delete: %d, want 404", code)
	}
	// A's own delete works.
	if code := doJSON(t, server.Client(), http.MethodDelete, server.URL+"/v1/sessions/"+secret.String(), tenantA, nil, nil); code != 200 {
		t.Fatalf("own delete: %d", code)
	}
}
