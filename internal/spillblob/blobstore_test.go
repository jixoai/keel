package spillblob

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/spill"
	"github.com/jixoai/keel/internal/store"
)

// testStore opens the store named by KEEL_TEST_DSN, skipping when unset so a
// plain `go test ./...` works without a database (the same convention the blob
// and store integration tests use).
func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := store.Open(context.Background(), store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func testBlobs(t *testing.T, st *store.Store) *Blobs {
	t.Helper()
	return New(
		&blob.Uploader{Store: st, CASDir: t.TempDir(), TempDir: t.TempDir()},
		&blob.ImageSource{Store: st, Reader: blob.NewReader(st, 4, 1)},
	)
}

func newTenant(t *testing.T, st *store.Store) uuid.UUID {
	t.Helper()
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "spill-test"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return tenant
}

// TestSpillAndLoadRoundTripsThroughTheTenantBlobStore is the recoverability
// proof of keel.spill: a pruned payload is a normal tenant blob and reads back
// byte-for-byte by its reference.
func TestSpillAndLoadRoundTripsThroughTheTenantBlobStore(t *testing.T) {
	st := testStore(t)
	blobs := testBlobs(t, st)
	tenant := newTenant(t, st)
	ctx := context.Background()
	scope := spill.Scope{TenantID: tenant.String(), SessionID: uuid.NewString()}

	payload := []byte(strings.Repeat("a tool result that must survive pruning\n", 500))
	ref, err := blobs.Spill(ctx, scope, "application/vnd.keel.tool-result+json", payload)
	if err != nil {
		t.Fatalf("Spill: %v", err)
	}
	if ref.ID == "" || ref.SHA256 != spill.Digest(payload) || ref.Size != int64(len(payload)) {
		t.Fatalf("ref = %+v, want the payload identity", ref)
	}
	recovered, err := blobs.Load(ctx, scope, ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(recovered, payload) {
		t.Fatalf("recovered %d bytes, want %d", len(recovered), len(payload))
	}
}

// TestSpillDedupesIdenticalContent: memory hygiene re-spills the same message
// on every turn after a restart; content addressing must return one object.
func TestSpillDedupesIdenticalContent(t *testing.T) {
	st := testStore(t)
	blobs := testBlobs(t, st)
	tenant := newTenant(t, st)
	ctx := context.Background()
	scope := spill.Scope{TenantID: tenant.String(), SessionID: uuid.NewString()}
	payload := []byte("same tool output")

	first, err := blobs.Spill(ctx, scope, "text/plain", payload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := blobs.Spill(ctx, scope, "text/plain", payload)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("dedupe failed: %s vs %s", first.ID, second.ID)
	}
}

// TestSpillIsTenantScoped: a reference from one tenant is not readable by
// another (the blob table's boundary, not an application check).
func TestSpillIsTenantScoped(t *testing.T) {
	st := testStore(t)
	blobs := testBlobs(t, st)
	owner := newTenant(t, st)
	other := newTenant(t, st)
	ctx := context.Background()
	payload := []byte("tenant secret output")
	ref, err := blobs.Spill(ctx, spill.Scope{TenantID: owner.String()}, "text/plain", payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Load(ctx, spill.Scope{TenantID: other.String()}, ref); err == nil {
		t.Fatal("another tenant read the payload")
	}
}

// TestSpillRefusesEmptyPayloadAndMissingScope: a degenerate reference would be
// unrecoverable, so the write is refused instead of rendering a lie.
func TestSpillRefusesEmptyPayloadAndMissingScope(t *testing.T) {
	st := testStore(t)
	blobs := testBlobs(t, st)
	tenant := newTenant(t, st)
	ctx := context.Background()
	if _, err := blobs.Spill(ctx, spill.Scope{TenantID: tenant.String()}, "text/plain", nil); err == nil {
		t.Fatal("empty payload was spilled")
	}
	if _, err := blobs.Spill(ctx, spill.Scope{}, "text/plain", []byte("x")); err == nil {
		t.Fatal("a scope without a tenant was accepted")
	}
	if _, err := blobs.Load(ctx, spill.Scope{TenantID: tenant.String()}, spill.Ref{ID: "not-a-uuid"}); err == nil {
		t.Fatal("an invalid blob id was accepted")
	}
}
