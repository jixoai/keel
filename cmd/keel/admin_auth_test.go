package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

// TestAdminBootstrapAndResetCLI drives the local operator commands against the
// real database: a one-time setup token (stderr + 0600 file), refusal after a
// super admin exists, and the recovery reset that revokes sessions and forces a
// password change.
func TestAdminBootstrapAndResetCLI(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	t.Setenv("KEEL_DSN", dsn)
	tenantID := uuid.New()
	t.Setenv("KEEL_SERVE_TENANT", tenantID.String())

	tokenFile := filepath.Join(t.TempDir(), "setup-token")
	var stdout, stderr bytes.Buffer
	if code := runAdminCmd([]string{"bootstrap", "--token-file", tokenFile}, &stdout, &stderr); code != 0 {
		t.Fatalf("bootstrap exit %d\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "expires:") || !strings.Contains(stderr.String(), auth.SetupTokenScheme) {
		t.Fatalf("bootstrap output missing token/expiry:\n%s\n%s", stdout.String(), stderr.String())
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(token, auth.SetupTokenScheme) {
		t.Fatalf("token file = %q", token)
	}
	info, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", info.Mode().Perm())
	}

	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	cleanupAuthTenant(t, st, tenantID)
	if live, err := st.LiveBootstrapToken(ctx, tenantID, time.Now().UTC()); err != nil || !live {
		t.Fatalf("token not live: %v err=%v", live, err)
	}
	hash, err := auth.HashPassword("admin-password-123")
	if err != nil {
		t.Fatal(err)
	}
	root, err := st.CreateAccount(ctx, store.AccountRecord{
		TenantID: tenantID, ID: uuid.New(), Username: "root-" + tenantID.String()[:8],
		DisplayName: "Root", Kind: "human", PasswordHash: hash, IsSuperAdmin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A second bootstrap must refuse once a super admin exists.
	stdout.Reset()
	stderr.Reset()
	if code := runAdminCmd([]string{"bootstrap"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "already exists") {
		t.Fatalf("bootstrap after super admin: exit=%d stderr=%s", code, stderr.String())
	}

	// The recovery reset revokes a live session and sets a temporary password.
	session, err := st.CreateAuthSession(ctx, store.AuthSession{
		TenantID: tenantID, SessionID: uuid.New(), AccountID: root.ID,
		IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runAdminCmd([]string{"reset", "--username", root.Username}, &stdout, &stderr); code != 0 {
		t.Fatalf("reset exit %d\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	temp := lineAfter(stdout.String(), "TEMPORARY PASSWORD")
	if temp == "" {
		t.Fatalf("reset output has no temporary password:\n%s", stdout.String())
	}
	updated, err := st.GetAccount(ctx, tenantID, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.VerifyPassword(updated.PasswordHash, temp); err != nil {
		t.Fatalf("temporary password does not verify: %v", err)
	}
	revoked, err := st.GetAuthSessionByID(ctx, session.SessionID)
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("reset did not revoke the session: %+v err=%v", revoked, err)
	}
	if must, err := st.AccountMustChangePassword(ctx, tenantID, root.ID); err != nil || !must {
		t.Fatalf("reset must-change = %v err=%v", must, err)
	}
	// Unknown users are an error, not a silent success.
	if code := runAdminCmd([]string{"reset", "--username", "nobody"}, &stdout, &stderr); code != 1 {
		t.Fatalf("reset unknown user exit = %d", code)
	}
}

func lineAfter(output, marker string) string {
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		if strings.Contains(line, marker) {
			for _, candidate := range lines[i+1:] {
				if candidate = strings.TrimSpace(candidate); candidate != "" {
					return candidate
				}
			}
		}
	}
	return ""
}
