package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Tenant secret integration tests (change keel-model-settings, 密钥管理).
// Skipped when KEEL_TEST_DSN is unset, like the other store integration tests.

// TestTenantSecretEncryptedAtRest pins the storage contract: the value is
// AES-GCM ciphertext in the column (the plaintext is not recoverable from the
// row without the key), the list projection carries only names and timestamps,
// and the value only ever comes back through the decrypting read.
func TestTenantSecretEncryptedAtRest(t *testing.T) {
	st := tenancyTestStore(t)
	box := testSecretBox(t)
	ctx := context.Background()
	tenant := uuid.New()
	plaintext := []byte("sk-live-8f3a-do-not-log")

	updatedAt, err := st.PutTenantSecret(ctx, tenant, "s2api", plaintext, box)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if updatedAt.IsZero() {
		t.Fatal("put must report updated_at")
	}

	var sealed []byte
	if err := st.Pool().QueryRow(ctx,
		`SELECT ciphertext FROM tenant_secrets WHERE tenant_id = $1 AND name = $2`,
		tenant, "s2api").Scan(&sealed); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if len(sealed) == 0 {
		t.Fatal("ciphertext column is empty")
	}
	if bytes.Contains(sealed, plaintext) {
		t.Fatal("the plaintext is present in the stored ciphertext")
	}
	opened, err := box.Open(sealed, TenantSecretAAD(tenant, "s2api"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("decrypted value = %q, want the stored one", opened)
	}
	// The AAD binds the row: the same ciphertext does not open under another
	// name or another tenant.
	if _, err := box.Open(sealed, TenantSecretAAD(tenant, "other")); err == nil {
		t.Fatal("a ciphertext must not open under a different name")
	}
	if _, err := box.Open(sealed, TenantSecretAAD(uuid.New(), "s2api")); err == nil {
		t.Fatal("a ciphertext must not open under a different tenant")
	}

	value, found, err := st.GetTenantSecret(ctx, tenant, "s2api", box)
	if err != nil || !found {
		t.Fatalf("get = (found %v, err %v), want the stored secret", found, err)
	}
	if !bytes.Equal(value, plaintext) {
		t.Fatal("the decrypted value differs from the stored one")
	}
	if _, found, err := st.GetTenantSecret(ctx, tenant, "never-stored", box); err != nil || found {
		t.Fatalf("missing secret = (found %v, err %v), want (false, nil)", found, err)
	}

	list, err := st.ListTenantSecrets(ctx, tenant)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Name != "s2api" {
		t.Fatalf("list = %+v, want one entry named s2api", list)
	}
	if list[0].CreatedAt.IsZero() || list[0].UpdatedAt.IsZero() {
		t.Fatalf("list entry must carry timestamps: %+v", list[0])
	}

	// An overwrite keeps created_at, moves updated_at, and stays ciphertext.
	created := list[0].CreatedAt
	time.Sleep(2 * time.Millisecond)
	if _, err := st.PutTenantSecret(ctx, tenant, "s2api", []byte("sk-live-rotated"), box); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	overwritten, err := st.ListTenantSecrets(ctx, tenant)
	if err != nil {
		t.Fatalf("list after overwrite: %v", err)
	}
	if len(overwritten) != 1 {
		t.Fatalf("overwrite must keep one row, got %+v", overwritten)
	}
	if !overwritten[0].CreatedAt.Equal(created) {
		t.Fatalf("created_at moved on overwrite: %v -> %v", created, overwritten[0].CreatedAt)
	}
	if !overwritten[0].UpdatedAt.After(created) {
		t.Fatalf("updated_at did not move on overwrite: %v", overwritten[0].UpdatedAt)
	}

	// Delete reports whether anything was there and is idempotent.
	deleted, err := st.DeleteTenantSecret(ctx, tenant, "s2api")
	if err != nil || !deleted {
		t.Fatalf("delete = (%v, %v), want (true, nil)", deleted, err)
	}
	if deleted, err := st.DeleteTenantSecret(ctx, tenant, "s2api"); err != nil || deleted {
		t.Fatalf("second delete = (%v, %v), want (false, nil)", deleted, err)
	}
}

// TestTenantSecretsCrossTenantAndMissingBox: the store is tenant-scoped, and a
// deployment without KEEL_SECRET_KEY (nil box) refuses every read and write
// instead of degrading to plaintext.
func TestTenantSecretsCrossTenantAndMissingBox(t *testing.T) {
	st := tenancyTestStore(t)
	box := testSecretBox(t)
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()

	if _, err := st.PutTenantSecret(ctx, a, "s2api", []byte("tenant-a-key"), box); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if _, found, err := st.GetTenantSecret(ctx, b, "s2api", box); err != nil || found {
		t.Fatalf("tenant b read = (found %v, err %v), want nothing", found, err)
	}
	if list, err := st.ListTenantSecrets(ctx, b); err != nil || len(list) != 0 {
		t.Fatalf("tenant b list = (%+v, %v), want empty", list, err)
	}
	if deleted, err := st.DeleteTenantSecret(ctx, b, "s2api"); err != nil || deleted {
		t.Fatalf("tenant b delete = (%v, %v), want (false, nil)", deleted, err)
	}
	if _, found, err := st.GetTenantSecret(ctx, a, "s2api", box); err != nil || !found {
		t.Fatalf("tenant a's secret must survive tenant b's delete: (found %v, err %v)", found, err)
	}

	// No key box: fail closed, never plaintext.
	if _, _, err := st.GetTenantSecret(ctx, a, "s2api", nil); !errors.Is(err, ErrSecretKeyMissing) {
		t.Fatalf("read without a box = %v, want ErrSecretKeyMissing", err)
	}
	if _, err := st.PutTenantSecret(ctx, a, "s2api", []byte("plaintext-fallback"), nil); !errors.Is(err, ErrSecretKeyMissing) {
		t.Fatalf("write without a box = %v, want ErrSecretKeyMissing", err)
	}
	// The refused write stored nothing.
	value, found, err := st.GetTenantSecret(ctx, a, "s2api", box)
	if err != nil || !found || !bytes.Equal(value, []byte("tenant-a-key")) {
		t.Fatalf("the refused write changed the row: (found %v, err %v)", found, err)
	}
}
