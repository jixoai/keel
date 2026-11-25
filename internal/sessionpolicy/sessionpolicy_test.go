package sessionpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/store"
)

// fakeStore is the durable seam double: it returns one stored document and
// records writes. It never invents narrowing; that is the service's job.
type fakeStore struct {
	document json.RawMessage
	found    bool
	revision int64
	err      error
	written  json.RawMessage
	setCalls int
}

func (f *fakeStore) GetSessionCheckpointPolicy(context.Context, uuid.UUID, uuid.UUID) (store.SessionCheckpointPolicy, bool, error) {
	if f.err != nil {
		return store.SessionCheckpointPolicy{}, false, f.err
	}
	if !f.found {
		return store.SessionCheckpointPolicy{}, false, nil
	}
	return store.SessionCheckpointPolicy{Policy: f.document, Revision: f.revision}, true, nil
}

func (f *fakeStore) SetSessionCheckpointPolicy(_ context.Context, _ uuid.UUID, _ uuid.UUID, document json.RawMessage) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.setCalls++
	f.written = document
	f.revision++
	f.document = document
	f.found = true
	return f.revision, nil
}

func tenantPolicy() compaction.CheckpointPolicy {
	return compaction.CheckpointPolicy{
		Enabled:         true,
		ThresholdTokens: 50000,
		RetainTokens:    20000,
		MaxCheckpoints:  3,
		TTL:             24 * time.Hour,
		ManualCompact:   true,
	}
}

func testService(store *fakeStore) *Service {
	return New(store, tenantPolicy)
}

// TestGetWithoutOverride pins the baseline projection: no row means the tenant
// policy at revision 0, with no narrowed fields.
func TestGetWithoutOverride(t *testing.T) {
	svc := testService(&fakeStore{})
	view, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.Source != "tenant" || view.Revision != 0 || view.Override != nil {
		t.Fatalf("view = %+v, want the tenant policy at revision 0", view)
	}
	if view.Effective.MaxCheckpoints != 3 || view.Effective.TTLHours != 24 ||
		view.Effective.RetainTokens != 20000 || !view.Effective.ManualCompact {
		t.Fatalf("effective = %+v, want the tenant policy", view.Effective)
	}
	if view.Effective.PolicyVersion == "" {
		t.Fatal("effective policy has no version")
	}
}

// TestPutNarrowsAndReports pins the session ceiling: a request above the tenant
// bound is clamped, the stored document is the request, and the response names
// the clamped field.
func TestPutNarrowsAndReports(t *testing.T) {
	st := &fakeStore{}
	svc := testService(st)
	max := 10
	retain := 999999
	ttl := 72
	view, err := svc.Put(context.Background(), uuid.New(), uuid.New(), compaction.CheckpointPolicyOverrideDocument{
		MaxCheckpoints: &max,
		RetainTokens:   &retain,
		TTLHours:       &ttl,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if st.setCalls != 1 {
		t.Fatalf("store writes = %d, want 1", st.setCalls)
	}
	if view.Source != "session" || view.Revision != 1 {
		t.Fatalf("view = %+v", view)
	}
	if view.Effective.MaxCheckpoints != 3 || view.Effective.RetainTokens != 20000 || view.Effective.TTLHours != 24 {
		t.Fatalf("effective = %+v, want tenant ceilings", view.Effective)
	}
	if len(view.Narrowed) != 3 {
		t.Fatalf("narrowed = %+v, want three fields", view.Narrowed)
	}
	// The stored document is the caller's request, not the clamped result.
	var stored compaction.CheckpointPolicyOverrideDocument
	if err := json.Unmarshal(st.written, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.MaxCheckpoints == nil || *stored.MaxCheckpoints != 10 {
		t.Fatalf("stored document = %s, want the requested 10", st.written)
	}
	// A narrower request within the ceiling is not clamped.
	small := 1
	view, err = svc.Put(context.Background(), uuid.New(), uuid.New(), compaction.CheckpointPolicyOverrideDocument{MaxCheckpoints: &small})
	if err != nil {
		t.Fatalf("Put narrow: %v", err)
	}
	if len(view.Narrowed) != 0 || view.Effective.MaxCheckpoints != 1 {
		t.Fatalf("narrow view = %+v", view)
	}
}

// TestPutRejectsInvalid pins validation before any write.
func TestPutRejectsInvalid(t *testing.T) {
	st := &fakeStore{}
	svc := testService(st)
	negative := -1
	if _, err := svc.Put(context.Background(), uuid.New(), uuid.New(), compaction.CheckpointPolicyOverrideDocument{RetainTokens: &negative}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Put err = %v, want ErrInvalid", err)
	}
	if st.setCalls != 0 {
		t.Fatal("an invalid document reached the store")
	}
}

// TestCheckpointPolicyResolver pins the runner seam: no row returns the tenant
// policy, a row returns the narrowed policy, and a missing session is not an
// error for the run path.
func TestCheckpointPolicyResolver(t *testing.T) {
	ctx := context.Background()
	tenant, session := uuid.New(), uuid.New()

	svc := testService(&fakeStore{})
	policy, err := svc.CheckpointPolicy(ctx, tenant, session)
	if err != nil || policy.MaxCheckpoints != 3 {
		t.Fatalf("resolver without row = (%+v, %v)", policy, err)
	}

	max := 1
	document, _ := json.Marshal(compaction.CheckpointPolicyOverrideDocument{MaxCheckpoints: &max})
	svc = testService(&fakeStore{document: document, found: true, revision: 2})
	policy, err = svc.CheckpointPolicy(ctx, tenant, session)
	if err != nil || policy.MaxCheckpoints != 1 {
		t.Fatalf("resolver with row = (%+v, %v)", policy, err)
	}

	// Stored garbage falls back to the tenant policy and reports the reason.
	svc = testService(&fakeStore{document: json.RawMessage(`{"retainTokens":-1}`), found: true})
	if _, err := svc.CheckpointPolicy(ctx, tenant, session); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stored invalid err = %v, want ErrInvalid", err)
	}

	// A missing session is the tenant policy with no error: a claim already
	// proved existence before the resolver runs.
	svc = testService(&fakeStore{err: store.ErrNoSession})
	if policy, err := svc.CheckpointPolicy(ctx, tenant, session); err != nil || policy.MaxCheckpoints != 3 {
		t.Fatalf("missing session resolver = (%+v, %v)", policy, err)
	}
}

// TestGetMissingSession pins the HTTP-facing sentinel.
func TestGetMissingSession(t *testing.T) {
	svc := testService(&fakeStore{err: store.ErrNoSession})
	if _, err := svc.Get(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Get err = %v, want ErrSessionNotFound", err)
	}
}
