package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

type fakeStore struct {
	runs []RunRecord

	requeueOutcome RequeueOutcome
	requeueErr     error
	requeued       []uuid.UUID
	parkChanged    bool
	parkErr        error
	parked         map[uuid.UUID]string
	parkReasons    map[uuid.UUID]string

	auto        map[uuid.UUID]int
	resumes     map[uuid.UUID]int
	escalations map[uuid.UUID]int
	escalatedAt map[uuid.UUID]int
	last        map[uuid.UUID]time.Time

	selection json.RawMessage
	listErr   error
}

func newFakeStore(runs ...RunRecord) *fakeStore {
	return &fakeStore{
		runs:           runs,
		requeueOutcome: RequeueOutcome{Status: "requeued"},
		parked:         map[uuid.UUID]string{},
		parkReasons:    map[uuid.UUID]string{},
		auto:           map[uuid.UUID]int{},
		resumes:        map[uuid.UUID]int{},
		escalations:    map[uuid.UUID]int{},
		escalatedAt:    map[uuid.UUID]int{},
		last:           map[uuid.UUID]time.Time{},
	}
}

func (s *fakeStore) ListSupervisorRuns(context.Context, int) ([]RunRecord, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]RunRecord(nil), s.runs...), nil
}

func (s *fakeStore) RequeueRun(_ context.Context, tenantID, sessionID uuid.UUID) (RequeueOutcome, error) {
	if s.requeueErr != nil {
		return RequeueOutcome{}, s.requeueErr
	}
	s.requeued = append(s.requeued, sessionID)
	if s.requeueOutcome.Status == "requeued" || s.requeueOutcome.Status == "pending" {
		for i := range s.runs {
			if s.runs[i].SessionID == sessionID {
				s.runs[i].Status = "pending"
			}
		}
	}
	return s.requeueOutcome, nil
}

func (s *fakeStore) ParkRun(_ context.Context, tenantID, sessionID uuid.UUID, reason string) (bool, error) {
	if s.parkErr != nil {
		return false, s.parkErr
	}
	s.parkReasons[sessionID] = reason
	if !s.parkChanged {
		return false, nil
	}
	s.parked[sessionID] = reason
	for i := range s.runs {
		if s.runs[i].SessionID == sessionID {
			s.runs[i].Status = "failed"
			s.runs[i].LastError = reason
		}
	}
	return true, nil
}

func (s *fakeStore) ResumeAttemptCount(_ context.Context, _, sessionID uuid.UUID) (int, error) {
	return s.resumes[sessionID], nil
}

func (s *fakeStore) AutoResumeCount(_ context.Context, _, sessionID uuid.UUID) (int, error) {
	return s.auto[sessionID], nil
}

func (s *fakeStore) LastAutoResume(_ context.Context, _, sessionID uuid.UUID) (time.Time, bool, error) {
	at, ok := s.last[sessionID]
	return at, ok, nil
}

func (s *fakeStore) EscalationCount(_ context.Context, _, sessionID uuid.UUID) (int, error) {
	return s.escalations[sessionID], nil
}

func (s *fakeStore) EscalationsSince(_ context.Context, _, sessionID uuid.UUID, _ time.Time) (int, error) {
	return s.escalatedAt[sessionID], nil
}

func (s *fakeStore) LatestSelection(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error) {
	return s.selection, nil
}

type fakeLedger struct {
	commits []kernel.CommitRequest
	seq     int64
	err     error
}

func (l *fakeLedger) Commit(_ context.Context, req kernel.CommitRequest) (kernel.Receipt, error) {
	if l.err != nil {
		return kernel.Receipt{}, l.err
	}
	l.seq++
	l.commits = append(l.commits, req)
	return kernel.Receipt{Seq: l.seq, Kind: req.Kind}, nil
}

func (l *fakeLedger) Lookup(context.Context, kernel.Scope, kernel.KeyRef) (kernel.Receipt, bool, error) {
	return kernel.Receipt{}, false, nil
}

func (l *fakeLedger) ofKind(kind kernel.EventKind) []kernel.CommitRequest {
	var out []kernel.CommitRequest
	for _, commit := range l.commits {
		if commit.Kind == kind {
			out = append(out, commit)
		}
	}
	return out
}

type fakeNotifier struct {
	requests []Notification
	result   NotificationResult
	err      error
}

func (n *fakeNotifier) Notify(_ context.Context, req Notification) (NotificationResult, error) {
	n.requests = append(n.requests, req)
	if n.err != nil {
		return NotificationResult{}, n.err
	}
	if n.result == (NotificationResult{}) {
		return NotificationResult{ID: "notification-1", Status: "sent"}, nil
	}
	return n.result, nil
}

type fakeParentEscalator struct {
	calls  int
	parent uuid.UUID
	child  uuid.UUID
	err    error
}

func (p *fakeParentEscalator) Escalate(_ context.Context, parent, child kernel.Scope, _ string) (string, error) {
	p.calls++
	p.parent = parent.SessionID
	p.child = child.SessionID
	if p.err != nil {
		return "", p.err
	}
	return "parent_notified", nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestSupervisor(t *testing.T, store Store, ledger kernel.Committer, policy Policy, version ConfigVersion) *Supervisor {
	t.Helper()
	sup, err := New(Config{
		Store:  store,
		Ledger: ledger,
		Policy: func() (Policy, ConfigVersion) { return policy, version },
		Logger: testLogger(),
		Now:    func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sup
}

func failedRun(tenant, session uuid.UUID, lastError string) RunRecord {
	return RunRecord{
		TenantID: tenant, SessionID: session, Status: "failed",
		LastError: lastError, UpdatedAt: time.Now().UTC().Add(-time.Minute),
	}
}

func decodePayload(t *testing.T, commit kernel.CommitRequest) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(commit.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return payload
}

func TestScanAutoResumesFailedRun(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	store := newFakeStore(failedRun(tenant, session, "model stream failed"))
	ledger := &fakeLedger{}
	sup := newTestSupervisor(t, store, ledger, DefaultPolicy(), ConfigVersion{Hash: "sha256:one", Revision: 7})

	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Recovered != 1 || len(store.requeued) != 1 {
		t.Fatalf("report = %+v, requeued = %v", report, store.requeued)
	}
	facts := ledger.ofKind(kernel.KindResumeAttempt)
	if len(facts) != 1 {
		t.Fatalf("resume facts = %d, want 1", len(facts))
	}
	if !facts[0].System {
		t.Fatal("resume fact was not committed on the system lane")
	}
	payload := decodePayload(t, facts[0])
	if payload["actor"] != "supervisor" || payload["result"] != "requeued" ||
		payload["counted"] != true || payload["reason"] != "run_failed" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload["configHash"] != "sha256:one" || payload["configRevision"] != float64(7) {
		t.Fatalf("config identity = %+v", payload)
	}
}

func TestScanRespectsBackoff(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	store := newFakeStore(failedRun(tenant, session, "model down"))
	store.auto[session] = 1
	store.last[session] = time.Now().UTC()
	policy := DefaultPolicy()
	policy.Resume.Backoff = Backoff{Initial: time.Hour, Multiplier: 2, Max: time.Hour}
	sup := newTestSupervisor(t, store, &fakeLedger{}, policy, ConfigVersion{})

	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Backoff != 1 || len(store.requeued) != 0 {
		t.Fatalf("report = %+v, requeued = %v", report, store.requeued)
	}
}

func TestScanEscalatesToParentAtExhaustion(t *testing.T) {
	tenant, session, parent := uuid.New(), uuid.New(), uuid.New()
	store := newFakeStore(failedRun(tenant, session, "still failing"))
	store.auto[session] = 3
	ledger := &fakeLedger{}
	escalator := &fakeParentEscalator{}
	sup := newTestSupervisor(t, store, ledger, DefaultPolicy(), ConfigVersion{Revision: 2})
	sup.parentOf = func(context.Context, uuid.UUID, uuid.UUID) (uuid.UUID, bool, error) {
		return parent, true, nil
	}
	sup.parentEscalator = escalator

	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Escalated != 1 || escalator.calls != 1 || escalator.parent != parent || escalator.child != session {
		t.Fatalf("report = %+v, escalator = %+v", report, escalator)
	}
	if len(store.requeued) != 0 {
		t.Fatalf("an exhausted run was requeued: %v", store.requeued)
	}
	facts := ledger.ofKind(kernel.KindSupervisionEscalated)
	if len(facts) != 1 {
		t.Fatalf("escalation facts = %d, want 1", len(facts))
	}
	payload := decodePayload(t, facts[0])
	if payload["level"] != "parent" || payload["result"] != "parent_notified" ||
		payload["parentSessionId"] != parent.String() || payload["configRevision"] != float64(2) {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestScanEscalatesToOwnerWithoutParent(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	store := newFakeStore(failedRun(tenant, session, "still failing"))
	store.auto[session] = 3
	ledger := &fakeLedger{}
	notifier := &fakeNotifier{}
	sup := newTestSupervisor(t, store, ledger, DefaultPolicy(), ConfigVersion{})
	sup.notifier = notifier

	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Escalated != 1 || len(notifier.requests) != 1 {
		t.Fatalf("report = %+v, notifications = %+v", report, notifier.requests)
	}
	if notifier.requests[0].SessionID != session || !strings.Contains(notifier.requests[0].Text, "Session "+session.String()) {
		t.Fatalf("notification = %+v", notifier.requests[0])
	}
	payload := decodePayload(t, ledger.ofKind(kernel.KindSupervisionEscalated)[0])
	if payload["level"] != "owner" || payload["result"] != "owner_notified" ||
		payload["notificationId"] != "notification-1" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestScanOnExhaustedStop(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	store := newFakeStore(failedRun(tenant, session, "still failing"))
	store.auto[session] = 3
	ledger := &fakeLedger{}
	policy := DefaultPolicy()
	policy.Resume.OnExhausted = ExhaustedStop
	notifier := &fakeNotifier{}
	sup := newTestSupervisor(t, store, ledger, policy, ConfigVersion{})
	sup.notifier = notifier

	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Escalated != 1 || len(notifier.requests) != 0 {
		t.Fatalf("report = %+v, notifications = %+v", report, notifier.requests)
	}
	payload := decodePayload(t, ledger.ofKind(kernel.KindSupervisionEscalated)[0])
	if payload["level"] != "stop" || payload["result"] != "stopped" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestScanCrashLoopExhaustsInsteadOfReclaiming(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	expired := time.Now().UTC().Add(-time.Minute)
	store := newFakeStore(RunRecord{
		TenantID: tenant, SessionID: session, Status: "running",
		LeaseUntil: &expired, UpdatedAt: expired,
	})
	store.auto[session] = 3
	store.parkChanged = true
	ledger := &fakeLedger{}
	notifier := &fakeNotifier{}
	sup := newTestSupervisor(t, store, ledger, DefaultPolicy(), ConfigVersion{})
	sup.notifier = notifier

	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Parked != 1 || report.Escalated != 1 || len(store.requeued) != 0 {
		t.Fatalf("report = %+v, requeued = %v", report, store.requeued)
	}
	if len(notifier.requests) != 1 || len(ledger.ofKind(kernel.KindSupervisionEscalated)) != 1 {
		t.Fatalf("notifications=%d escalations=%d", len(notifier.requests), len(ledger.ofKind(kernel.KindSupervisionEscalated)))
	}
}

func TestScanManualParksPendingRun(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	store := newFakeStore(RunRecord{
		TenantID: tenant, SessionID: session, Status: "pending",
		UpdatedAt: time.Now().UTC().Add(-time.Minute),
	})
	store.parkChanged = true
	ledger := &fakeLedger{}
	policy := DefaultPolicy()
	policy.Resume.Mode = ModeManual
	sup := newTestSupervisor(t, store, ledger, policy, ConfigVersion{})

	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Parked != 1 || len(store.requeued) != 0 {
		t.Fatalf("report = %+v, requeued = %v", report, store.requeued)
	}
	facts := ledger.ofKind(kernel.KindResumeAttempt)
	if len(facts) != 1 {
		t.Fatalf("park facts = %d, want 1", len(facts))
	}
	payload := decodePayload(t, facts[0])
	if payload["result"] != "parked" || payload["counted"] != false ||
		payload["reason"] != "policy_manual" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestScanNeverParksAndDoesNotResumeFailures(t *testing.T) {
	tenant := uuid.New()
	pending, failed := uuid.New(), uuid.New()
	store := newFakeStore(
		RunRecord{TenantID: tenant, SessionID: pending, Status: "pending", UpdatedAt: time.Now().UTC()},
		failedRun(tenant, failed, "failure"),
	)
	store.parkChanged = true
	ledger := &fakeLedger{}
	policy := DefaultPolicy()
	policy.Resume.Mode = ModeNever
	sup := newTestSupervisor(t, store, ledger, policy, ConfigVersion{})

	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Parked != 1 || len(store.requeued) != 0 {
		t.Fatalf("report = %+v, requeued = %v", report, store.requeued)
	}
	if store.parked[pending] == "" {
		t.Fatal("the pending run was not parked")
	}
	if store.parked[failed] != "" {
		t.Fatal("an already-failed run was parked again")
	}
	facts := ledger.ofKind(kernel.KindResumeAttempt)
	if len(facts) != 1 {
		t.Fatalf("resume facts = %d, want 1 parked fact", len(facts))
	}
	payload := decodePayload(t, facts[0])
	if payload["reason"] != "policy_never" || payload["result"] != "parked" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestScanAutoLeavesPendingToWorker(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	store := newFakeStore(RunRecord{
		TenantID: tenant, SessionID: session, Status: "pending",
		UpdatedAt: time.Now().UTC(),
	})
	ledger := &fakeLedger{}
	sup := newTestSupervisor(t, store, ledger, DefaultPolicy(), ConfigVersion{})
	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Queued != 1 || len(store.requeued) != 0 || len(store.parked) != 0 || len(ledger.commits) != 0 {
		t.Fatalf("report = %+v, requeued=%v parked=%v commits=%d", report, store.requeued, store.parked, len(ledger.commits))
	}
}

func TestScanSkipsOperatorCancelledRun(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	store := newFakeStore(failedRun(tenant, session, "context canceled"))
	ledger := &fakeLedger{}
	sup := newTestSupervisor(t, store, ledger, DefaultPolicy(), ConfigVersion{})
	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Cancelled != 1 || len(store.requeued) != 0 || len(ledger.commits) != 0 {
		t.Fatalf("report = %+v, requeued=%v commits=%d", report, store.requeued, len(ledger.commits))
	}
}

func TestScanProviderFallbackSelection(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	store := newFakeStore(failedRun(tenant, session, "insufficient_quota: the account is out of credit"))
	store.selection = json.RawMessage(`{"providerId":"p1","modelId":"m1","reasoningLevel":"low"}`)
	ledger := &fakeLedger{}
	policy := DefaultPolicy()
	policy.Resume.ProviderFallback = ProviderFallback{{ProviderID: "p2", ModelID: "m2"}}
	sup := newTestSupervisor(t, store, ledger, policy, ConfigVersion{})
	sup.validate = func(raw json.RawMessage) error {
		var selection map[string]any
		if err := json.Unmarshal(raw, &selection); err != nil {
			return err
		}
		if selection["providerId"] != "p2" || selection["modelId"] != "m2" {
			return errors.New("unexpected selection")
		}
		return nil
	}

	if _, err := sup.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	payload := decodePayload(t, ledger.ofKind(kernel.KindResumeAttempt)[0])
	if payload["reason"] != "provider_exhausted" || payload["providerFallback"] != "p2/m2" {
		t.Fatalf("payload = %+v", payload)
	}
	selection, ok := payload["selection"].(map[string]any)
	if !ok || selection["providerId"] != "p2" || selection["modelId"] != "m2" {
		t.Fatalf("selection = %+v", payload["selection"])
	}
}

func TestScanProviderFallbackInvalidStillResumes(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	store := newFakeStore(failedRun(tenant, session, "quota exceeded"))
	store.selection = json.RawMessage(`{"providerId":"p1","modelId":"m1"}`)
	ledger := &fakeLedger{}
	policy := DefaultPolicy()
	policy.Resume.ProviderFallback = ProviderFallback{{ProviderID: "missing", ModelID: "nope"}}
	sup := newTestSupervisor(t, store, ledger, policy, ConfigVersion{})
	sup.validate = func(json.RawMessage) error { return errors.New("model is not configured") }

	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Recovered != 1 {
		t.Fatalf("report = %+v", report)
	}
	payload := decodePayload(t, ledger.ofKind(kernel.KindResumeAttempt)[0])
	if payload["fallbackError"] != "model is not configured" {
		t.Fatalf("fallbackError = %+v", payload["fallbackError"])
	}
	if _, present := payload["selection"]; present {
		t.Fatalf("an invalid selection was recorded: %+v", payload["selection"])
	}
}

func TestConfigRotationAffectsOnlyLaterAttempts(t *testing.T) {
	tenant := uuid.New()
	first, second := uuid.New(), uuid.New()
	store := newFakeStore(failedRun(tenant, first, "first failure"))
	ledger := &fakeLedger{}
	policy := DefaultPolicy()
	version := ConfigVersion{Hash: "sha256:rev1", Revision: 1}
	sup, err := New(Config{
		Store: store, Ledger: ledger, Logger: testLogger(),
		Policy: func() (Policy, ConfigVersion) { return policy, version },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := sup.Scan(context.Background()); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	firstFact := decodePayload(t, ledger.ofKind(kernel.KindResumeAttempt)[0])

	// Rotate both the policy and the configuration identity, then admit a new
	// failure under the new generation.
	policy.Resume.MaxAttempts = 9
	version = ConfigVersion{Hash: "sha256:rev2", Revision: 2}
	store.runs = append(store.runs, failedRun(tenant, second, "second failure"))
	if _, err := sup.Scan(context.Background()); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	facts := ledger.ofKind(kernel.KindResumeAttempt)
	if len(facts) != 2 {
		t.Fatalf("resume facts = %d, want 2", len(facts))
	}
	secondFact := decodePayload(t, facts[1])
	if firstFact["configRevision"] != float64(1) || secondFact["configRevision"] != float64(2) {
		t.Fatalf("versions = %+v / %+v", firstFact["configRevision"], secondFact["configRevision"])
	}
	// The first fact is immutable history: rotating the policy must not rewrite it.
	if again := decodePayload(t, facts[0]); again["configRevision"] != float64(1) {
		t.Fatalf("the first fact changed after rotation: %+v", again)
	}
}

func TestScanIsIdempotentOnceRequeued(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	store := newFakeStore(failedRun(tenant, session, "failure"))
	ledger := &fakeLedger{}
	sup := newTestSupervisor(t, store, ledger, DefaultPolicy(), ConfigVersion{})
	if _, err := sup.Scan(context.Background()); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if _, err := sup.Scan(context.Background()); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if len(store.requeued) != 1 {
		t.Fatalf("requeued %d times, want 1", len(store.requeued))
	}
	if facts := ledger.ofKind(kernel.KindResumeAttempt); len(facts) != 1 {
		t.Fatalf("resume facts = %d, want 1", len(facts))
	}
	if store.runs[0].Status != "pending" {
		t.Fatalf("run status = %q, want pending", store.runs[0].Status)
	}
}

func TestScanEscalatesOncePerFailureEpoch(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	store := newFakeStore(failedRun(tenant, session, "failure"))
	store.auto[session] = 3
	store.escalatedAt[session] = 1
	ledger := &fakeLedger{}
	notifier := &fakeNotifier{}
	sup := newTestSupervisor(t, store, ledger, DefaultPolicy(), ConfigVersion{})
	sup.notifier = notifier

	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Escalated != 0 || len(notifier.requests) != 0 || len(ledger.commits) != 0 {
		t.Fatalf("report = %+v, notifications=%d commits=%d", report, len(notifier.requests), len(ledger.commits))
	}
}

func TestScanReportsStoreFailuresWithoutStopping(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	store := newFakeStore(failedRun(tenant, session, "failure"))
	store.requeueErr = errors.New("database unavailable")
	ledger := &fakeLedger{}
	sup := newTestSupervisor(t, store, ledger, DefaultPolicy(), ConfigVersion{})
	report, err := sup.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan returned a global error: %v", err)
	}
	if len(report.Errors) != 1 || !strings.Contains(report.Errors[0], "database unavailable") {
		t.Fatalf("errors = %+v", report.Errors)
	}
}
