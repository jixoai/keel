package harnessapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
)

// compactFake is the manual-compaction seam double: it records calls and replays
// the first result for a repeated requestId, mirroring the adapter contract.
type compactFake struct {
	mu      sync.Mutex
	calls   []string
	results map[string]CompactResult
	err     error
}

func (f *compactFake) compact(_ context.Context, _, sessionID uuid.UUID, requestID string) (CompactResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, requestID)
	if f.err != nil {
		return CompactResult{}, f.err
	}
	if result, ok := f.results[requestID]; ok {
		result.Replayed = true
		result.Performed = false
		return result, nil
	}
	result := CompactResult{
		CheckpointID:  42,
		Performed:     true,
		TokensBefore:  9000,
		TokensAfter:   1500,
		KeptFromSeq:   11,
		PolicyVersion: "cp1-abc",
		Reason:        "compacted",
	}
	f.results[requestID] = result
	return result, nil
}

func (f *compactFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestCompactRouteManualCompaction pins POST /v1/sessions/{id}/compact: a
// performed call answers 201 with the identifier-only result, and a repeated
// requestId is passed through unchanged so the seam can replay the same
// checkpoint (200, replayed=true).
func TestCompactRouteManualCompaction(t *testing.T) {
	seam := &compactFake{results: map[string]CompactResult{}}
	server := httptest.NewServer(New(Config{CompactSession: seam.compact, Access: permitAllAccess(), Logger: testLogger()}))
	defer server.Close()
	tenant := uuid.New()
	session := uuid.New()
	url := server.URL + "/v1/sessions/" + session.String() + "/compact"

	var first CompactResult
	if code := doJSON(t, server.Client(), http.MethodPost, url, tenant, map[string]any{"requestId": "r1"}, &first); code != http.StatusCreated {
		t.Fatalf("first compact status = %d, want 201", code)
	}
	if !first.Performed || first.CheckpointID != 42 || first.TokensBefore != 9000 ||
		first.TokensAfter != 1500 || first.KeptFromSeq != 11 || first.PolicyVersion != "cp1-abc" {
		t.Fatalf("first result = %+v", first)
	}
	if first.SessionID != session.String() || first.RequestID != "r1" {
		t.Fatalf("result identifiers = %+v", first)
	}

	var replay CompactResult
	if code := doJSON(t, server.Client(), http.MethodPost, url, tenant, map[string]any{"requestId": "r1"}, &replay); code != http.StatusOK {
		t.Fatalf("replayed compact status = %d, want 200", code)
	}
	if !replay.Replayed || replay.Performed || replay.CheckpointID != first.CheckpointID {
		t.Fatalf("replay = %+v, want the same checkpoint replayed", replay)
	}
	if seam.callCount() != 2 {
		t.Fatalf("seam calls = %d, want 2 (the seam owns idempotency)", seam.callCount())
	}
}

// TestCompactRouteRefusals pins the gates: missing seam 501, missing/empty
// requestId 400, policy-disabled 409, unknown session 404 and no session.write
// 403 without touching the seam.
func TestCompactRouteRefusals(t *testing.T) {
	tenant := uuid.New()
	session := uuid.New()
	url := "/v1/sessions/" + session.String() + "/compact"

	t.Run("no seam", func(t *testing.T) {
		server := httptest.NewServer(New(Config{Access: permitAllAccess(), Logger: testLogger()}))
		defer server.Close()
		if code := doJSON(t, server.Client(), http.MethodPost, server.URL+url, tenant, map[string]any{"requestId": "r1"}, nil); code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501", code)
		}
	})

	t.Run("missing requestId", func(t *testing.T) {
		seam := &compactFake{results: map[string]CompactResult{}}
		server := httptest.NewServer(New(Config{CompactSession: seam.compact, Access: permitAllAccess(), Logger: testLogger()}))
		defer server.Close()
		if code := doJSON(t, server.Client(), http.MethodPost, server.URL+url, tenant, map[string]any{}, nil); code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", code)
		}
		if seam.callCount() != 0 {
			t.Fatal("the seam was called without a requestId")
		}
	})

	t.Run("policy disabled", func(t *testing.T) {
		seam := &compactFake{results: map[string]CompactResult{}, err: ErrCompactDisabled}
		server := httptest.NewServer(New(Config{CompactSession: seam.compact, Access: permitAllAccess(), Logger: testLogger()}))
		defer server.Close()
		if code := doJSON(t, server.Client(), http.MethodPost, server.URL+url, tenant, map[string]any{"requestId": "r1"}, nil); code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", code)
		}
	})

	t.Run("session busy", func(t *testing.T) {
		seam := &compactFake{results: map[string]CompactResult{}, err: ErrCompactBusy}
		server := httptest.NewServer(New(Config{CompactSession: seam.compact, Access: permitAllAccess(), Logger: testLogger()}))
		defer server.Close()
		if code := doJSON(t, server.Client(), http.MethodPost, server.URL+url, tenant, map[string]any{"requestId": "r1"}, nil); code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", code)
		}
	})

	t.Run("quota refused", func(t *testing.T) {
		seam := &compactFake{results: map[string]CompactResult{}, err: ErrCompactQuota}
		server := httptest.NewServer(New(Config{CompactSession: seam.compact, Access: permitAllAccess(), Logger: testLogger()}))
		defer server.Close()
		if code := doJSON(t, server.Client(), http.MethodPost, server.URL+url, tenant, map[string]any{"requestId": "r1"}, nil); code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", code)
		}
	})

	t.Run("unknown session", func(t *testing.T) {
		seam := &compactFake{results: map[string]CompactResult{}, err: ErrSessionNotFound}
		server := httptest.NewServer(New(Config{CompactSession: seam.compact, Access: permitAllAccess(), Logger: testLogger()}))
		defer server.Close()
		if code := doJSON(t, server.Client(), http.MethodPost, server.URL+url, tenant, map[string]any{"requestId": "r1"}, nil); code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
		}
	})

	t.Run("no session.write", func(t *testing.T) {
		denied := permitAllAccess()
		denied.decisions = map[string]access.Decision{
			access.KeySessionWrite: {
				Allow: false, Key: access.KeySessionWrite,
				Reason: access.ReasonMissingRelation, Detail: "viewer cannot write",
			},
		}
		seam := &compactFake{results: map[string]CompactResult{}}
		server := httptest.NewServer(New(Config{CompactSession: seam.compact, Access: denied, Logger: testLogger()}))
		defer server.Close()
		if code := doJSON(t, server.Client(), http.MethodPost, server.URL+url, tenant, map[string]any{"requestId": "r1"}, nil); code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", code)
		}
		if seam.callCount() != 0 {
			t.Fatal("the seam was called without session.write")
		}
	})
}
