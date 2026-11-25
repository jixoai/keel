package harnessapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/sessionpolicy"
	"github.com/jixoai/keel/internal/store"
)

type policyStoreFake struct {
	document json.RawMessage
	found    bool
	revision int64
}

func (f *policyStoreFake) GetSessionCheckpointPolicy(context.Context, uuid.UUID, uuid.UUID) (store.SessionCheckpointPolicy, bool, error) {
	if !f.found {
		return store.SessionCheckpointPolicy{}, false, nil
	}
	return store.SessionCheckpointPolicy{Policy: f.document, Revision: f.revision}, true, nil
}

func (f *policyStoreFake) SetSessionCheckpointPolicy(_ context.Context, _ uuid.UUID, _ uuid.UUID, document json.RawMessage) (int64, error) {
	f.document = document
	f.found = true
	f.revision++
	return f.revision, nil
}

func policyServer(t *testing.T, seam SessionCheckpointPolicyService, accessSeam Access) *httptest.Server {
	t.Helper()
	return httptest.NewServer(New(Config{CheckpointPolicies: seam, Access: accessSeam, Logger: testLogger()}))
}

func tenantCheckpointPolicy() compaction.CheckpointPolicy {
	return compaction.CheckpointPolicy{
		Enabled:         true,
		ThresholdTokens: 50000,
		RetainTokens:    20000,
		MaxCheckpoints:  3,
		TTL:             24 * time.Hour,
		ManualCompact:   true,
	}
}

// TestCheckpointPolicyRouteProjection pins GET: the tenant policy, the stored
// override and the clamped effective policy travel together.
func TestCheckpointPolicyRouteProjection(t *testing.T) {
	st := &policyStoreFake{
		document: json.RawMessage(`{"maxCheckpoints":10,"ttlHours":72}`),
		found:    true,
		revision: 2,
	}
	svc := sessionpolicy.New(st, tenantCheckpointPolicy)
	server := policyServer(t, svc, permitAllAccess())
	defer server.Close()
	session := uuid.New()

	var view sessionpolicy.View
	code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/sessions/"+session.String()+"/checkpoint-policy", uuid.New(), nil, &view)
	if code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", code)
	}
	if view.Source != "session" || view.Revision != 2 {
		t.Fatalf("view = %+v", view)
	}
	if view.Effective.MaxCheckpoints != 3 || view.Effective.TTLHours != 24 {
		t.Fatalf("effective = %+v, want tenant ceilings", view.Effective)
	}
	if len(view.Narrowed) != 2 {
		t.Fatalf("narrowed = %+v, want maxCheckpoints and ttl", view.Narrowed)
	}
}

// TestCheckpointPolicyRouteWrite pins PUT: a session can narrow, the response
// carries the effective policy, and an invalid document never reaches the store.
func TestCheckpointPolicyRouteWrite(t *testing.T) {
	st := &policyStoreFake{}
	svc := sessionpolicy.New(st, tenantCheckpointPolicy)
	server := policyServer(t, svc, permitAllAccess())
	defer server.Close()
	url := server.URL + "/v1/sessions/" + uuid.New().String() + "/checkpoint-policy"

	var view sessionpolicy.View
	if code := doJSON(t, server.Client(), http.MethodPut, url, uuid.New(),
		map[string]any{"maxCheckpoints": 1, "manualCompact": false}, &view); code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", code)
	}
	if view.Effective.MaxCheckpoints != 1 || view.Effective.ManualCompact {
		t.Fatalf("effective = %+v", view.Effective)
	}
	if !st.found || st.revision != 1 {
		t.Fatalf("stored = %+v", st)
	}

	// An invalid (negative) document is refused by the service before storage.
	if code := doJSON(t, server.Client(), http.MethodPut, url, uuid.New(),
		map[string]any{"retainTokens": -1}, nil); code != http.StatusBadRequest {
		t.Fatalf("invalid PUT status = %d, want 400", code)
	}
	// Unknown fields are rejected by the strict body decoder.
	if code := doJSON(t, server.Client(), http.MethodPut, url, uuid.New(),
		map[string]any{"invented": true}, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown field PUT status = %d, want 400", code)
	}
}

// TestCheckpointPolicyRouteRefusals pins the gates: unwired 501, missing session
// 404, and read/write permission keys enforced before the seam runs.
func TestCheckpointPolicyRouteRefusals(t *testing.T) {
	session := uuid.New()
	base := "/v1/sessions/" + session.String() + "/checkpoint-policy"

	t.Run("unwired", func(t *testing.T) {
		server := httptest.NewServer(New(Config{Access: permitAllAccess(), Logger: testLogger()}))
		defer server.Close()
		if code := doJSON(t, server.Client(), http.MethodGet, server.URL+base, uuid.New(), nil, nil); code != http.StatusNotImplemented {
			t.Fatalf("GET status = %d, want 501", code)
		}
	})

	t.Run("missing session", func(t *testing.T) {
		svc := sessionpolicy.New(missingPolicyStore{}, tenantCheckpointPolicy)
		server := policyServer(t, svc, permitAllAccess())
		defer server.Close()
		if code := doJSON(t, server.Client(), http.MethodGet, server.URL+base, uuid.New(), nil, nil); code != http.StatusNotFound {
			t.Fatalf("GET status = %d, want 404", code)
		}
	})

	t.Run("read denied", func(t *testing.T) {
		denied := permitAllAccess()
		denied.decisions = map[string]access.Decision{
			access.KeySessionRead: {Allow: false, Key: access.KeySessionRead, Reason: access.ReasonMissingRelation},
		}
		server := policyServer(t, sessionpolicy.New(&policyStoreFake{}, tenantCheckpointPolicy), denied)
		defer server.Close()
		if code := doJSON(t, server.Client(), http.MethodGet, server.URL+base, uuid.New(), nil, nil); code != http.StatusForbidden {
			t.Fatalf("GET status = %d, want 403", code)
		}
	})

	t.Run("write denied", func(t *testing.T) {
		denied := permitAllAccess()
		denied.decisions = map[string]access.Decision{
			access.KeySessionWrite: {Allow: false, Key: access.KeySessionWrite, Reason: access.ReasonMissingRelation},
		}
		server := policyServer(t, sessionpolicy.New(&policyStoreFake{}, tenantCheckpointPolicy), denied)
		defer server.Close()
		if code := doJSON(t, server.Client(), http.MethodPut, server.URL+base, uuid.New(),
			map[string]any{"maxCheckpoints": 1}, nil); code != http.StatusForbidden {
			t.Fatalf("PUT status = %d, want 403", code)
		}
	})
}

type missingPolicyStore struct{}

func (missingPolicyStore) GetSessionCheckpointPolicy(context.Context, uuid.UUID, uuid.UUID) (store.SessionCheckpointPolicy, bool, error) {
	return store.SessionCheckpointPolicy{}, false, store.ErrNoSession
}

func (missingPolicyStore) SetSessionCheckpointPolicy(context.Context, uuid.UUID, uuid.UUID, json.RawMessage) (int64, error) {
	return 0, store.ErrNoSession
}
