// Legacy tenant api_key tests: minting and format detection. The serving path
// no longer accepts these credentials (see cmd/keel/auth.go); the operator
// provisioning surface still mints them.

package auth

import (
	"context"
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
