package agentext

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/tasklog"
)

const testWebhookSecret = "0123456789abcdef0123456789abcdef"

func signWebhook(secret string, tenantID uuid.UUID, triggerID, timestamp, key string, body []byte) string {
	canonical := "v1\n" + tenantID.String() + "\n" + triggerID + "\n" + timestamp + "\n" + key + "\n"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func webhookFire(tenantID uuid.UUID, triggerID, key, body string, now time.Time, secret string) FireRequest {
	timestamp := fmt.Sprintf("%d", now.Unix())
	return FireRequest{
		IdempotencyKey:   key,
		Reason:           "webhook",
		WebhookTimestamp: timestamp,
		WebhookSignature: signWebhook(secret, tenantID, triggerID, timestamp, key, []byte(body)),
		WebhookBody:      []byte(body),
	}
}

func TestTriggerWebhookHMACVerification(t *testing.T) {
	now := time.Now().UTC()
	starter := &countingStarter{}
	engine := NewTriggerEngine(TriggerEngineOptions{
		Starter:          starter,
		Logger:           testLogger(),
		Now:              func() time.Time { return now },
		WebhookTolerance: time.Minute,
	})
	defer engine.Close()
	tenant := uuid.New()
	if _, err := engine.Register(TriggerSpec{
		ID: "secure.hook", Kind: TriggerWebhook, Prompt: "ping", Enabled: true,
		WebhookSecret: testWebhookSecret,
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"idempotencyKey":"k1"}`

	first, err := engine.Fire(context.Background(), "secure.hook", tenant,
		webhookFire(tenant, "secure.hook", "k1", body, now, testWebhookSecret))
	if err != nil || first.Status != "fired" || first.Deduped {
		t.Fatalf("signed fire: %+v err=%v", first, err)
	}
	if starter.count() != 1 {
		t.Fatalf("signed fire started %d turns", starter.count())
	}

	// A repeated delivery inside the window is accepted but deduped: no second
	// turn, and the audit records the dedupe instead of silently dropping it.
	second, err := engine.Fire(context.Background(), "secure.hook", tenant,
		webhookFire(tenant, "secure.hook", "k1", body, now, testWebhookSecret))
	if err != nil || !second.Deduped || second.SessionID != first.SessionID {
		t.Fatalf("repeated delivery: %+v err=%v", second, err)
	}
	if starter.count() != 1 {
		t.Fatalf("repeated delivery started a second turn")
	}
	status, _ := engine.Get("secure.hook")
	if status.Dedupes != 1 || status.Fires != 1 {
		t.Fatalf("dedupe counters: fires=%d dedupes=%d", status.Fires, status.Dedupes)
	}

	// Wrong signature: fail closed with a typed 401, no turn and an audit row.
	bad := webhookFire(tenant, "secure.hook", "k2", body, now, testWebhookSecret)
	bad.WebhookSignature = "sha256=" + strings.Repeat("0", 64)
	if _, err := engine.Fire(context.Background(), "secure.hook", tenant, bad); !errors.Is(err, ErrTriggerWebhookUnauthorized) {
		t.Fatalf("bad signature: %v", err)
	}
	// Stale timestamp: the signature itself is valid, the window is not.
	stale := webhookFire(tenant, "secure.hook", "k3", body, now.Add(-2*time.Minute), testWebhookSecret)
	if _, err := engine.Fire(context.Background(), "secure.hook", tenant, stale); !errors.Is(err, ErrTriggerWebhookUnauthorized) {
		t.Fatalf("stale timestamp: %v", err)
	}
	// Future timestamp beyond the window is equally refused.
	future := webhookFire(tenant, "secure.hook", "k4", body, now.Add(2*time.Minute), testWebhookSecret)
	if _, err := engine.Fire(context.Background(), "secure.hook", tenant, future); !errors.Is(err, ErrTriggerWebhookUnauthorized) {
		t.Fatalf("future timestamp: %v", err)
	}
	if starter.count() != 1 {
		t.Fatalf("rejected deliveries started turns: %d", starter.count())
	}
	status, _ = engine.Get("secure.hook")
	if status.Rejected != 3 || status.Failures != 0 {
		t.Fatalf("rejection audit: rejected=%d failures=%d audit=%+v", status.Rejected, status.Failures, status.Audit)
	}

	// The secret is never in the wire form (status, audit or plugin snapshot).
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testWebhookSecret) || strings.Contains(string(raw), "WebhookSecret\"") {
		t.Fatalf("trigger status leaked the secret: %s", raw)
	}
	if !status.WebhookConfigured {
		t.Fatal("webhookConfigured should be true without exposing the value")
	}

	// A manual fire of the same trigger is an authenticated operator action and
	// does not need a signature.
	if result, err := engine.Fire(context.Background(), "secure.hook", tenant,
		FireRequest{IdempotencyKey: "manual-1", Reason: "manual"}); err != nil || result.Status != "fired" {
		t.Fatalf("manual fire: %+v err=%v", result, err)
	}
}

func TestTriggerWebhookWithoutSecretFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	starter := &countingStarter{}
	engine := NewTriggerEngine(TriggerEngineOptions{Starter: starter, Logger: testLogger()})
	defer engine.Close()
	tenant := uuid.New()
	if _, err := engine.Register(TriggerSpec{
		ID: "open.hook", Kind: TriggerWebhook, Prompt: "p", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Even a correctly shaped signature is unusable: there is no key to verify
	// against, so the trigger refuses every signed delivery.
	req := webhookFire(tenant, "open.hook", "k1", `{}`, now, testWebhookSecret)
	if _, err := engine.Fire(context.Background(), "open.hook", tenant, req); !errors.Is(err, ErrTriggerWebhookUnconfigured) {
		t.Fatalf("unconfigured trigger: %v", err)
	}
	if starter.count() != 0 {
		t.Fatalf("unconfigured trigger started a turn")
	}
	status, _ := engine.Get("open.hook")
	if status.Rejected != 1 || len(status.Audit) == 0 || status.Audit[0].Status != "rejected" {
		t.Fatalf("unconfigured refusal not audited: %+v", status)
	}
}

// requests copies the starts recorded by countingStarter (defined in
// agentext_test.go) so assertions do not race the scheduler goroutine.
func (s *countingStarter) requests() []StartTurnRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StartTurnRequest(nil), s.starts...)
}

type blockingStarter struct {
	started chan StartTurnRequest
	release chan struct{}
}

func newBlockingStarter() *blockingStarter {
	return &blockingStarter{started: make(chan StartTurnRequest, 4), release: make(chan struct{})}
}

func (s *blockingStarter) StartTurn(ctx context.Context, req StartTurnRequest) (StartTurnResult, error) {
	s.started <- req
	select {
	case <-s.release:
	case <-ctx.Done():
		return StartTurnResult{}, ctx.Err()
	}
	return StartTurnResult{TenantID: req.TenantID, SessionID: req.SessionID, Seq: 1, RequestID: req.RequestID}, nil
}

func TestTriggerConcurrencyCapRefusesInsteadOfQueueing(t *testing.T) {
	starter := newBlockingStarter()
	engine := NewTriggerEngine(TriggerEngineOptions{Starter: starter, Logger: testLogger()})
	defer engine.Close()
	tenant := uuid.New()
	if _, err := engine.Register(TriggerSpec{
		ID: "busy.hook", Kind: TriggerWebhook, Prompt: "p", Enabled: true,
		MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := engine.Fire(context.Background(), "busy.hook", tenant,
			FireRequest{IdempotencyKey: "first", Reason: "manual"})
		firstDone <- err
	}()
	select {
	case <-starter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first fire never reached the starter")
	}
	status, _ := engine.Get("busy.hook")
	if status.InFlight != 1 {
		t.Fatalf("inFlight = %d, want 1", status.InFlight)
	}
	if _, err := engine.Fire(context.Background(), "busy.hook", tenant,
		FireRequest{IdempotencyKey: "second", Reason: "manual"}); !errors.Is(err, ErrTriggerBusy) {
		t.Fatalf("second concurrent fire: %v", err)
	}
	status, _ = engine.Get("busy.hook")
	if status.Throttled != 1 || status.Fires != 0 {
		t.Fatalf("throttle audit: throttled=%d fires=%d", status.Throttled, status.Fires)
	}
	close(starter.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first fire: %v", err)
	}
	// After the slot frees, the same key can run (the first was not remembered
	// as a failed fire).
	if _, err := engine.Fire(context.Background(), "busy.hook", tenant,
		FireRequest{IdempotencyKey: "second", Reason: "manual"}); err != nil {
		t.Fatalf("fire after release: %v", err)
	}
}

type fakeTriggerStateStore struct {
	mu    sync.Mutex
	state store.TriggerState
	ok    bool
	saves []store.TriggerState
}

func (f *fakeTriggerStateStore) LoadTriggerState(context.Context, uuid.UUID, string) (store.TriggerState, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.ok, nil
}

func (f *fakeTriggerStateStore) SaveTriggerState(_ context.Context, state store.TriggerState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves = append(f.saves, state)
	if !f.ok || state.LastScheduledAt.After(f.state.LastScheduledAt) {
		f.state = state
		f.ok = true
	}
	return nil
}

func (f *fakeTriggerStateStore) lastSave(t *testing.T) store.TriggerState {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.saves) == 0 {
		t.Fatal("no cursor was saved")
	}
	return f.saves[len(f.saves)-1]
}

func TestTriggerCatchUpPlanIsBounded(t *testing.T) {
	now := time.Now().UTC()
	engine := &TriggerEngine{catchUpWindow: 24 * time.Hour}
	rt := &triggerRuntime{
		spec:         TriggerSpec{ID: "t", Kind: TriggerInterval, Schedule: "1s"},
		interval:     time.Second,
		catchUpLimit: 2,
	}
	fire, dropped, newest := engine.catchUpPlan(rt, now.Add(-10*time.Second), now)
	if len(fire) != 2 || dropped != 8 {
		t.Fatalf("interval plan: fire=%d dropped=%d", len(fire), dropped)
	}
	// Only the newest windows are replayed; the oldest eight are coalesced.
	if fire[len(fire)-1].After(now) {
		t.Fatalf("interval plan replayed the future: %v", fire)
	}
	if fire[0].After(fire[1]) || fire[1].Sub(fire[0]) != time.Second {
		t.Fatalf("interval plan is not chronological: %v", fire)
	}
	if !newest.Equal(fire[len(fire)-1]) {
		t.Fatalf("interval newest = %v, want the last replayed window %v", newest, fire[len(fire)-1])
	}

	cronRT := &triggerRuntime{
		spec:         TriggerSpec{ID: "c", Kind: TriggerCron, Schedule: "*/2 * * * *"},
		catchUpLimit: 2,
	}
	cronRT.cron = mustCron(t, "*/2 * * * *")
	cronFire, cronDropped, cronNewest := engine.catchUpPlan(cronRT, now.Add(-10*time.Minute), now)
	if len(cronFire) == 0 || len(cronFire) > 2 {
		t.Fatalf("cron plan fire=%v", cronFire)
	}
	if cronDropped+len(cronFire) < 2 {
		t.Fatalf("cron plan dropped=%d fire=%d, expected multiple missed windows", cronDropped, len(cronFire))
	}
	for i := 1; i < len(cronFire); i++ {
		if !cronFire[i].After(cronFire[i-1]) {
			t.Fatalf("cron plan is not chronological: %v", cronFire)
		}
	}
	if len(cronFire) > 0 && !cronNewest.Equal(cronFire[len(cronFire)-1]) {
		t.Fatalf("cron newest = %v, want the last replayed window %v", cronNewest, cronFire[len(cronFire)-1])
	}

	// The lookback window caps how far a restart may reach back.
	windowed := &TriggerEngine{catchUpWindow: time.Minute}
	narrow, narrowDropped, _ := windowed.catchUpPlan(rt, now.Add(-10*time.Minute), now)
	if len(narrow) != 2 || narrowDropped < 500 {
		t.Fatalf("lookback plan: fire=%d dropped=%d", len(narrow), narrowDropped)
	}
}

func mustCron(t *testing.T, expression string) *cronSchedule {
	t.Helper()
	schedule, err := parseCron(expression)
	if err != nil {
		t.Fatal(err)
	}
	return schedule
}

func TestTriggerCatchUpAfterRestartAndCursor(t *testing.T) {
	tenant := uuid.New()
	starter := &countingStarter{}
	seed := time.Now().UTC().Add(-3500 * time.Millisecond)
	state := &fakeTriggerStateStore{
		ok: true,
		state: store.TriggerState{
			TenantID:        tenant,
			TriggerID:       "catchup.tick",
			LastScheduledAt: seed,
			LastFiredAt:     seed,
			LastStatus:      "fired",
		},
	}
	engine := NewTriggerEngine(TriggerEngineOptions{
		Starter:      starter,
		Logger:       testLogger(),
		State:        state,
		CatchUpLimit: 2,
	})
	defer engine.Close()
	if _, err := engine.Register(TriggerSpec{
		ID: "catchup.tick", Kind: TriggerInterval, Schedule: "1s",
		Prompt: "tick", TenantID: tenant.String(), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return starter.count() >= 2 }, "catch-up fires")
	waitFor(t, 3*time.Second, func() bool {
		status, _ := engine.Get("catchup.tick")
		return status.Skipped >= 1
	}, "skipped audit")
	status, _ := engine.Get("catchup.tick")
	if status.LastScheduledAt == nil || !status.LastScheduledAt.After(seed) {
		t.Fatalf("cursor did not advance: %+v", status.LastScheduledAt)
	}
	if status.Fires < 2 || status.Rejected != 0 {
		t.Fatalf("catch-up counters: fires=%d rejected=%d", status.Fires, status.Rejected)
	}
	seenCatchUp := false
	for _, req := range starter.requests() {
		if mark, ok := req.Extras["trigger"].(map[string]any); ok && mark["reason"] == "catchup" {
			seenCatchUp = true
		}
	}
	if !seenCatchUp {
		t.Fatalf("no catch-up reason reached the turn: %+v", starter.requests())
	}
	saved := state.lastSave(t)
	if !saved.LastScheduledAt.After(seed) {
		t.Fatalf("saved cursor did not advance: %v (seed %v)", saved.LastScheduledAt, seed)
	}
	if saved.LastScheduledAt.Sub(seed) > 5*time.Second {
		t.Fatalf("catch-up replayed too far: %v (seed %v)", saved.LastScheduledAt, seed)
	}

	// Stop the first scheduler before the restart so the window under test is
	// the cursor replay, not a fresh timer tick.
	engine.Close()

	// Restart: a new engine over the same cursor must not replay the same
	// window again. A newer window may fire on its normal timer, so compare
	// request ids instead of the raw count.
	before := starter.requests()
	seen := make(map[string]bool, len(before))
	for _, req := range before {
		seen[req.RequestID] = true
	}
	restarted := NewTriggerEngine(TriggerEngineOptions{
		Starter:      starter,
		Logger:       testLogger(),
		State:        state,
		CatchUpLimit: 2,
	})
	defer restarted.Close()
	if _, err := restarted.Register(TriggerSpec{
		ID: "catchup.tick", Kind: TriggerInterval, Schedule: "1s",
		Prompt: "tick", TenantID: tenant.String(), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	for _, req := range starter.requests()[len(before):] {
		if seen[req.RequestID] {
			t.Fatalf("restart replayed an already processed window: %q", req.RequestID)
		}
	}
}

type fakeTaskCards struct {
	mu    sync.Mutex
	calls []kernel.Scope
	card  tasklog.Card
	err   error
}

func (f *fakeTaskCards) EnsureCard(_ context.Context, scope kernel.Scope) (tasklog.Card, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, scope)
	if f.err != nil {
		return tasklog.Card{}, f.err
	}
	card := f.card
	card.TenantID = scope.TenantID
	card.SessionID = scope.SessionID
	return card, nil
}

func TestTriggerCreatesInitialTaskCard(t *testing.T) {
	taskID := uuid.New()
	tasks := &fakeTaskCards{card: tasklog.Card{TaskID: taskID, Status: tasklog.StatusOpen, Origin: tasklog.OriginSession}}
	engine := NewTriggerEngine(TriggerEngineOptions{
		Starter: &countingStarter{}, Logger: testLogger(), Tasks: tasks,
	})
	defer engine.Close()
	tenant := uuid.New()
	if _, err := engine.Register(TriggerSpec{
		ID: "card.hook", Kind: TriggerWebhook, Prompt: "p", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Fire(context.Background(), "card.hook", tenant,
		FireRequest{IdempotencyKey: "card-1", Reason: "manual"})
	if err != nil || result.Status != "fired" {
		t.Fatalf("fire: %+v err=%v", result, err)
	}
	if result.TaskCardID != taskID.String() || result.TaskCardError != "" {
		t.Fatalf("task card result = %+v", result)
	}
	tasks.mu.Lock()
	if len(tasks.calls) != 1 || tasks.calls[0].TenantID != tenant ||
		tasks.calls[0].SessionID.String() != result.SessionID {
		t.Fatalf("EnsureCard scopes = %+v", tasks.calls)
	}
	tasks.mu.Unlock()
	status, _ := engine.Get("card.hook")
	if status.Audit[0].TaskCardID != taskID.String() {
		t.Fatalf("audit lost the card id: %+v", status.Audit[0])
	}

	// A card failure does not roll the committed turn back; it is visible.
	tasks.err = errors.New("task log unavailable")
	result, err = engine.Fire(context.Background(), "card.hook", tenant,
		FireRequest{IdempotencyKey: "card-2", Reason: "manual"})
	if err != nil || result.Status != "fired" || !strings.Contains(result.TaskCardError, "task log unavailable") {
		t.Fatalf("card failure: %+v err=%v", result, err)
	}
}

func TestTriggerRejectsUnsafeConfiguration(t *testing.T) {
	engine := NewTriggerEngine(TriggerEngineOptions{Starter: &countingStarter{}, Logger: testLogger()})
	defer engine.Close()
	cases := []TriggerSpec{
		{ID: "short", Kind: TriggerWebhook, Prompt: "p", Enabled: true, WebhookSecret: "short"},
		{ID: "negative", Kind: TriggerWebhook, Prompt: "p", Enabled: true, MaxConcurrent: -1},
		{ID: "bad-window", Kind: TriggerWebhook, Prompt: "p", Enabled: true, WebhookToleranceSec: 86401},
		{ID: "bad-catchup", Kind: TriggerWebhook, Prompt: "p", Enabled: true, CatchUpLimit: -1},
	}
	for _, spec := range cases {
		if _, err := engine.Register(spec); !errors.Is(err, ErrInvalidTrigger) {
			t.Errorf("%s: %v, want ErrInvalidTrigger", spec.ID, err)
		}
	}
}
