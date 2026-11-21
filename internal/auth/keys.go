// Package auth resolves and mints credentials.
//
// This file is the retired tenant api_keys vocabulary (A4.2b3a): the tables and
// minting path are gone. What remains is the credential-shape recognizer the
// auth middleware uses to reject a `keel_tenant_` / `keel_admin_` bearer before
// any store lookup (alpha zero compatibility: an old key is a loud 401, never a
// silent fallback). Browser login is the password/session service in service.go;
// machine login is the bot key in botkeys.go.
package auth

import "strings"

// KeyScheme prefixes every retired plaintext key. The trailing underscore keeps
// `keel_tenant_x` unambiguous against the bot scheme (`keel_bot_`).
const KeyScheme = "keel_"

// Retired role tokens the old key grammar carried; LooksLikeKey needs them to
// recognize the shape it must refuse.
const (
	RoleTenant = "tenant"
	RoleAdmin  = "admin"
)

// ValidRole reports whether role is a known retired key role.
func ValidRole(role string) bool { return role == RoleTenant || role == RoleAdmin }

// LooksLikeKey reports whether a credential is in the retired key format. It is
// a pure string test: no allocation beyond a prefix compare, no I/O. The
// serving middleware uses it to reject `keel_tenant_`/`keel_admin_` bearers
// explicitly instead of letting them reach a handler.
func LooksLikeKey(credential string) bool {
	credential = strings.TrimSpace(credential)
	if !strings.HasPrefix(credential, KeyScheme) {
		return false
	}
	rest := credential[len(KeyScheme):]
	role, token, ok := strings.Cut(rest, "_")
	if !ok || token == "" {
		return false
	}
	return ValidRole(role)
}
