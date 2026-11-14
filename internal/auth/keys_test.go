// API key credential tests: minting, hashing, format detection and the
// middleware's 401 semantics (change keel-ga §2).

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMintKeyFormatAndHash(t *testing.T) {
	for _, role := range []string{RoleTenant, RoleAdmin} {
		minted, err := MintKey(role)
		if err != nil {
			t.Fatalf("mint %s: %v", role, err)
		}
		if !strings.HasPrefix(minted.Plaintext, KeyScheme+role+"_") {
			t.Fatalf("%s key %q lacks the role prefix", role, minted.Plaintext)
		}
		if !LooksLikeKey(minted.Plaintext) {
			t.Fatalf("%s key is not recognised as a key", role)
		}
		if len(minted.Hash) != 32 {
			t.Fatalf("hash length %d", len(minted.Hash))
		}
		if !strings.HasPrefix(minted.Plaintext, minted.Prefix) || len(minted.Prefix) >= len(minted.Plaintext) {
			t.Fatalf("prefix %q is not a strict prefix of the key", minted.Prefix)
		}
		// Two mints never collide.
		other, err := MintKey(role)
		if err != nil {
			t.Fatal(err)
		}
		if other.Plaintext == minted.Plaintext || string(other.Hash) == string(minted.Hash) {
			t.Fatal("two mints produced the same key")
		}
	}
	if _, err := MintKey("superuser"); err == nil {
		t.Fatal("an unknown role was minted")
	}
}

func TestLooksLikeKeyRejectsUnrelatedCredentials(t *testing.T) {
	cases := map[string]bool{
		"keel_tenant_abc":              true,
		"keel_admin_abc":               true,
		"keel_owner_abc":               false,
		"keel_tenant_":                 false,
		"keel_tenant":                  false,
		"hunter2":                      false,
		"":                             false,
		"keel_tenant_abc\nX-Injected:": true, // still a key shape; the lookup decides
	}
	for credential, want := range cases {
		if got := LooksLikeKey(credential); got != want {
			t.Fatalf("LooksLikeKey(%q) = %v, want %v", credential, got, want)
		}
	}
}

// fakeKeys is the KeyStore seam: one live key, everything else unknown.
type fakeKeys struct {
	hash   []byte
	record KeyRecord
	err    error
	// looked reports the hashes that reached the seam (proves the plaintext is
	// hashed before the lookup).
	looked [][]byte
}

func (f *fakeKeys) LookupAPIKey(_ context.Context, hash []byte) (KeyRecord, error) {
	f.looked = append(f.looked, hash)
	if f.err != nil {
		return KeyRecord{}, f.err
	}
	if len(hash) != len(f.hash) {
		return KeyRecord{}, errors.New("unknown key")
	}
	for i := range hash {
		if hash[i] != f.hash[i] {
			return KeyRecord{}, errors.New("unknown key")
		}
	}
	return f.record, nil
}

func newTestAuthorizer(keys KeyStore) (*Authorizer, *httptest.Server) {
	var seen struct {
		tenant    string
		auth      string
		apiKey    string
		identity  Identity
		hasIdent  bool
		path      string
		nextCalls int
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.tenant = r.Header.Get(HeaderTenantID)
		seen.auth = r.Header.Get("Authorization")
		seen.apiKey = r.Header.Get("X-API-Key")
		seen.path = r.URL.Path
		seen.identity, seen.hasIdent = IdentityFrom(r.Context())
		seen.nextCalls++
		w.WriteHeader(http.StatusOK)
	})
	authorizer := &Authorizer{Keys: keys, Next: handler}
	server := httptest.NewServer(authorizer)
	return authorizer, server
}

func TestAuthorizerResolvesTenantAndStripsCredential(t *testing.T) {
	tenantID, keyID := uuid.New(), uuid.New()
	minted, err := MintKey(RoleTenant)
	if err != nil {
		t.Fatal(err)
	}
	keys := &fakeKeys{
		hash:   HashKey(minted.Plaintext),
		record: KeyRecord{TenantID: tenantID, KeyID: keyID, Prefix: minted.Prefix, Role: RoleTenant},
	}
	var captured struct {
		tenant   string
		auth     string
		apiKey   string
		identity Identity
		ok       bool
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.tenant = r.Header.Get(HeaderTenantID)
		captured.auth = r.Header.Get("Authorization")
		captured.apiKey = r.Header.Get("X-API-Key")
		captured.identity, captured.ok = IdentityFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(&Authorizer{Keys: keys, Next: next})
	defer server.Close()

	// Bearer key: tenant injected, credential stripped, identity attached.
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Plaintext)
	// A caller-supplied tenant must be replaced, never trusted.
	req.Header.Set(HeaderTenantID, uuid.NewString())
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bearer key: %d", res.StatusCode)
	}
	if captured.tenant != tenantID.String() {
		t.Fatalf("tenant = %q, want the key's %q", captured.tenant, tenantID)
	}
	if captured.auth != "" || captured.apiKey != "" {
		t.Fatalf("credential leaked downstream: auth=%q apiKey=%q", captured.auth, captured.apiKey)
	}
	if !captured.ok || captured.identity.TenantID != tenantID || captured.identity.KeyID != keyID {
		t.Fatalf("identity = %+v ok=%v", captured.identity, captured.ok)
	}

	// X-API-Key takes the same path.
	captured = struct {
		tenant   string
		auth     string
		apiKey   string
		identity Identity
		ok       bool
	}{}
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/sessions", nil)
	req.Header.Set("X-API-Key", minted.Plaintext)
	res, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || captured.tenant != tenantID.String() {
		t.Fatalf("x-api-key: %d tenant=%q", res.StatusCode, captured.tenant)
	}
	// The lookup received a hash, never the plaintext.
	for _, hash := range keys.looked {
		if strings.Contains(string(hash), "keel_") {
			t.Fatalf("the plaintext reached the key store: %q", hash)
		}
	}
}

func TestAuthorizerRejectsInvalidCredentials(t *testing.T) {
	minted, err := MintKey(RoleTenant)
	if err != nil {
		t.Fatal(err)
	}
	keys := &fakeKeys{
		hash:   HashKey(minted.Plaintext),
		record: KeyRecord{TenantID: uuid.New(), KeyID: uuid.New(), Prefix: minted.Prefix, Role: RoleTenant},
	}
	server := httptest.NewServer(&Authorizer{Keys: keys, Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})})
	defer server.Close()

	cases := []struct {
		name   string
		header string
		value  string
	}{
		{"unknown bearer key", "Authorization", "Bearer keel_tenant_aaaaaaaa_unknown"},
		{"unknown x-api-key", "X-API-Key", "keel_tenant_aaaaaaaa_unknown"},
		{"truncated key", "Authorization", "Bearer keel_tenant_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/sessions", nil)
			req.Header.Set(tc.header, tc.value)
			res, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s: %d, want 401", tc.name, res.StatusCode)
			}
			if res.Header.Get("WWW-Authenticate") == "" {
				t.Fatalf("%s: no WWW-Authenticate", tc.name)
			}
		})
	}

	// A key store error is a 401 too: an unknown/revoked key must not become a
	// 500 that tells the caller the key exists.
	broken := &fakeKeys{err: errors.New("store: api key not found")}
	brokenServer := httptest.NewServer(&Authorizer{Keys: broken, Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("a failed lookup reached the next handler")
	})})
	defer brokenServer.Close()
	req, _ := http.NewRequest(http.MethodGet, brokenServer.URL+"/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Plaintext)
	res, err := brokenServer.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("broken store: %d, want 401", res.StatusCode)
	}

	// A key-shaped credential with no store configured fails closed.
	bare := httptest.NewServer(&Authorizer{Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("an unverifiable key reached the next handler")
	})})
	defer bare.Close()
	req, _ = http.NewRequest(http.MethodGet, bare.URL+"/v1/sessions", nil)
	req.Header.Set("X-API-Key", "keel_tenant_aaaaaaaa_unknown")
	res, err = bare.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no store: %d, want 401", res.StatusCode)
	}
}

// TestAuthorizerLeavesNonKeyTrafficAlone pins the coexistence requirement: a
// request with no key, a cookie session, or a passphrase bearer must reach the
// existing handler untouched, so the browser surface keeps working.
func TestAuthorizerLeavesNonKeyTrafficAlone(t *testing.T) {
	keys := &fakeKeys{err: errors.New("no lookup expected")}
	cases := []struct {
		name   string
		setup  func(*http.Request)
		tenant string
	}{
		{"no credential", func(*http.Request) {}, "tenant-from-cookie"},
		{"cookie", func(r *http.Request) { r.Header.Set("Cookie", "keel_session=abc.def") }, "tenant-from-cookie"},
		{"passphrase bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer hunter2") }, "tenant-from-cookie"},
		{"basic scheme", func(r *http.Request) { r.Header.Set("Authorization", "Basic dXNlcjpwdw==") }, "tenant-from-cookie"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			var tenant string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				tenant = r.Header.Get(HeaderTenantID)
				w.WriteHeader(http.StatusOK)
			})
			server := httptest.NewServer(&Authorizer{Keys: keys, Next: next})
			defer server.Close()
			req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/sessions", nil)
			req.Header.Set(HeaderTenantID, tc.tenant)
			tc.setup(req)
			res, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if !reached || res.StatusCode != http.StatusOK {
				t.Fatalf("%s did not reach the fallback handler (%d)", tc.name, res.StatusCode)
			}
			if tenant != tc.tenant {
				t.Fatalf("%s: tenant header was rewritten to %q", tc.name, tenant)
			}
		})
	}
	if len(keys.looked) != 0 {
		t.Fatalf("non-key traffic triggered %d key lookups", len(keys.looked))
	}
}

func TestIdentityAdminFlag(t *testing.T) {
	admin := Identity{Role: RoleAdmin}
	if !admin.Admin() {
		t.Fatal("an admin identity does not report admin")
	}
	tenant := Identity{Role: RoleTenant}
	if tenant.Admin() {
		t.Fatal("a tenant identity reports admin")
	}
}

func TestWithIdentityRoundTrip(t *testing.T) {
	tenantID := uuid.New()
	ctx := WithIdentity(context.Background(), Identity{TenantID: tenantID, Role: RoleAdmin})
	identity, ok := IdentityFrom(ctx)
	if !ok || identity.TenantID != tenantID || identity.Role != RoleAdmin {
		t.Fatalf("identity = %+v ok=%v", identity, ok)
	}
	if _, ok := IdentityFrom(context.Background()); ok {
		t.Fatal("a bare context reported an identity")
	}
}
