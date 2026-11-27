package boardintegration

// Tenant SecretResolver for board sync (keel-board-v1 design §14.8 / task
// 10.6).
//
// A sync entry stores secret NAMES only (role -> tenant secret name). This file
// turns those names into boardsync.Secret values through keel's existing
// encrypted tenant_secrets store (store.GetTenantSecret + KEEL_SECRET_KEY). No
// method returns a value to a caller that renders it: the only paths that see
// plaintext are provider construction and an adapter's HTTP call. Status,
// metrics, audit and error values carry one of configured/missing/invalid.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"

	boardsync "github.com/jixoai/keel-board/sync"
)

// CredentialStatus is the only credential fact the board mount publishes.
type CredentialStatus string

const (
	// CredentialConfigured means every required secret resolved and decrypted.
	CredentialConfigured CredentialStatus = "configured"
	// CredentialMissing means a referenced secret (or the secret store itself)
	// is absent.
	CredentialMissing CredentialStatus = "missing"
	// CredentialInvalid means a referenced value exists but cannot be used
	// (decryption failed, it is empty, or the name is malformed).
	CredentialInvalid CredentialStatus = "invalid"
)

// secretNamePattern is the tenant secret grammar. It matches the settings
// surface (internal/harnessapi), so a board entry can only reference a secret
// an operator can actually store.
var secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// ValidateSecretRef checks a stored secret NAMES map. It never inspects a
// value; there are no values here.
func ValidateSecretRef(ref map[string]string) error {
	for role, name := range ref {
		if !validSecretRole(role) {
			return fmt.Errorf("secretRef.%s: unknown credential role", role)
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("secretRef.%s: a tenant secret name is required", role)
		}
		if !secretNamePattern.MatchString(name) {
			return fmt.Errorf("secretRef.%s: %q is not a tenant secret name (letters, digits, underscore; max 64)", role, name)
		}
	}
	return nil
}

func validSecretRole(role string) bool {
	if role == "" || len(role) > 40 {
		return false
	}
	for _, r := range role {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// SecretResolver resolves one entry's credential set.
type SecretResolver struct {
	Store *store.Store
	// Box decrypts tenant secrets. Nil means the deployment has no
	// KEEL_SECRET_KEY: every reference is missing and nothing is ever stored in
	// plaintext as a fallback.
	Box *store.SecretBox
}

// Resolve loads one named secret. found=false means the tenant never stored
// that name or the secret store is unavailable; invalid=true means the row
// exists but cannot be decrypted or is empty. The error text never contains a
// value.
func (r SecretResolver) Resolve(ctx context.Context, tenantID uuid.UUID, name string) (secret boardsync.Secret, found bool, invalid bool, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false, false, nil
	}
	if !secretNamePattern.MatchString(name) {
		return "", false, true, fmt.Errorf("boardintegration: %q is not a tenant secret name", name)
	}
	if r.Store == nil || r.Box == nil {
		return "", false, false, nil
	}
	value, ok, err := r.Store.GetTenantSecret(ctx, tenantID, name, r.Box)
	if err != nil {
		if errors.Is(err, store.ErrSecretKeyMissing) {
			return "", false, false, nil
		}
		// A decrypt failure is an invalid credential, not a missing one. The
		// store's error is reduced to a stable description: it may name a
		// ciphertext, never a plaintext.
		return "", false, true, errors.New("boardintegration: tenant secret could not be decrypted")
	}
	if !ok {
		return "", false, false, nil
	}
	if len(value) == 0 {
		return "", true, true, errors.New("boardintegration: tenant secret is empty")
	}
	return boardsync.Secret(value), true, false, nil
}

// credentialReport is the outcome of resolving one entry's credential roles.
type credentialReport struct {
	Status CredentialStatus
	// Secret is the role map the provider constructor needs. It is populated
	// only when Status == configured.
	Secret map[string]boardsync.Secret
	// Missing and Invalid name the roles, never the values, for the operator
	// log. They are empty when configured.
	Missing []string
	Invalid []string
}

// resolveEntryCredential resolves every role the platform requires. The role
// map is a closed vocabulary per platform, so a typo in configuration cannot
// silently skip a required credential.
func (r SecretResolver) resolveEntryCredential(ctx context.Context, tenantID uuid.UUID, platform string, ref map[string]string) credentialReport {
	required, optional := credentialRoles(platform)
	report := credentialReport{Status: CredentialConfigured, Secret: make(map[string]boardsync.Secret)}
	resolve := func(role string, required bool) {
		name := strings.TrimSpace(ref[role])
		if name == "" {
			if required {
				report.Missing = append(report.Missing, role)
			}
			return
		}
		secret, found, invalid, err := r.Resolve(ctx, tenantID, name)
		switch {
		case invalid:
			report.Invalid = append(report.Invalid, role)
		case !found:
			report.Missing = append(report.Missing, role)
		case err != nil:
			report.Invalid = append(report.Invalid, role)
		default:
			report.Secret[role] = secret
		}
	}
	for _, role := range required {
		resolve(role, true)
	}
	for _, role := range optional {
		resolve(role, false)
	}
	switch {
	case len(report.Invalid) > 0:
		report.Status = CredentialInvalid
	case len(report.Missing) > 0:
		report.Status = CredentialMissing
	}
	if report.Status != CredentialConfigured {
		report.Secret = nil
	}
	return report
}

// credentialRoles is the closed role vocabulary of one platform.
func credentialRoles(platform string) (required, optional []string) {
	switch platform {
	case "slack":
		return []string{"signingSecret"}, []string{"botToken"}
	case "github":
		return []string{"webhookSecret"}, []string{"token"}
	case "linear":
		return []string{"webhookSecret"}, []string{"apiToken"}
	case "chatsdk":
		return []string{"bridgeSecret"}, nil
	default:
		return nil, nil
	}
}

// knownCredentialRoles reports whether a role belongs to the platform's closed
// vocabulary. Create/update validation uses it so a misspelled role is a 400,
// not a silently ignored credential.
func knownCredentialRoles(platform string) map[string]bool {
	required, optional := credentialRoles(platform)
	known := make(map[string]bool, len(required)+len(optional))
	for _, role := range append(append([]string(nil), required...), optional...) {
		known[role] = true
	}
	return known
}
