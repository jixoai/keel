// Retired tenant api_key shape detection. Minting is gone (A4.2b3a); the
// middleware still recognizes the old grammar so it can answer 401 explicitly.

package auth

import "testing"

func TestLooksLikeKeyRecognizesRetiredShapes(t *testing.T) {
	tests := []struct {
		credential string
		want       bool
	}{
		{"keel_tenant_abc", true},
		{"keel_admin_abc", true},
		{"keel_bot_abc", false},
		{"keel_superuser_abc", false},
		{"keel_tenant_", false},
		{"keel_tenant", false},
		{"", false},
		{"keel_tenant_abc\nX-Injected:", true}, // still a key shape; the middleware refuses it
	}
	for _, test := range tests {
		if got := LooksLikeKey(test.credential); got != test.want {
			t.Fatalf("LooksLikeKey(%q) = %v, want %v", test.credential, got, test.want)
		}
	}
}

func TestValidRoleIsClosed(t *testing.T) {
	for _, role := range []string{RoleTenant, RoleAdmin} {
		if !ValidRole(role) {
			t.Fatalf("ValidRole(%q) = false", role)
		}
	}
	if ValidRole("superuser") {
		t.Fatal("an unknown legacy role was accepted")
	}
}
