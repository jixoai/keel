package harnessapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
)

// TestRequireKeyRejectsResourceScopedKey pins the A4.2a fail-fast rule: a
// handler that calls requireKey with a resource-scoped key is a code bug, not
// an implicit "held key" allow. The only legal ways to authorize a resource
// key are requireResource (one row) and requireHeldKey (a create edge).
func TestRequireKeyRejectsResourceScopedKey(t *testing.T) {
	server := New(Config{})
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	rec := httptest.NewRecorder()
	if _, _, ok := server.requireKey(rec, req, access.KeyBotRead); ok {
		t.Fatal("requireKey accepted a resource-scoped key")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestRequireKeyRejectsUnknownKey(t *testing.T) {
	server := New(Config{})
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	rec := httptest.NewRecorder()
	if _, _, ok := server.requireKey(rec, req, "session.fly"); ok {
		t.Fatal("requireKey accepted an unregistered key")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
