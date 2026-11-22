package harnessext_test

// W10 trigger delivery E2E: a real PostgreSQL store, the real kernel (edge
// commit + event log), the real task log and the real HarnessAPI over HTTP. The
// chain proven here is webhook signature -> fire -> committed user.message ->
// initial task card, plus the fail-closed refusals, the replay window, the
// per-trigger concurrency refusal and the durable catch-up cursor.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/harnessapi"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/tasklog"
)

const e2eWebhookSecret = "e2e-secret-0123456789abcdef"

func e2eSign(secret string, tenantID uuid.UUID, triggerID, timestamp, key string, body []byte) string {
	canonical := "v1\n" + tenantID.String() + "\n" + triggerID + "\n" + timestamp + "\n" + key + "\n"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newSignedRequest(url string, tenant uuid.UUID, body []byte, timestamp, signature string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-ID", tenant.String())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Keel-Webhook", "1")
	req.Header.Set("X-Keel-Timestamp", timestamp)
	req.Header.Set("X-Keel-Signature", signature)
	return req, nil
}

// doSignedPost sends the exact body and returns its status (and optionally
// decodes the JSON answer).
func doSignedPost(t *testing.T, client *http.Client, url string, tenant uuid.UUID,
	body []byte, timestamp, signature string, out any) (int, http.Header) {
	t.Helper()
	req, err := newSignedRequest(url, tenant, body, timestamp, signature)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode signed POST (%d): %v body=%s", res.StatusCode, err, raw)
		}
	}
	return res.StatusCode, res.Header
}

func e2eGetTriggers(t *testing.T, client *http.Client, url string, tenant uuid.UUID) ([]harnessext.TriggerStatus, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/v1/triggers", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant.String())
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/triggers: %d %s", res.StatusCode, raw)
	}
	var body struct {
		Triggers []harnessext.TriggerStatus `json:"triggers"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode triggers: %v body=%s", err, raw)
	}
	return body.Triggers, string(raw)
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", message)
}

func findTriggerStatus(t *testing.T, statuses []harnessext.TriggerStatus, id string) harnessext.TriggerStatus {
	t.Helper()
	for _, status := range statuses {
		if status.ID == id {
			return status
		}
	}
	t.Fatalf("trigger %q not in %+v", id, statuses)
	return harnessext.TriggerStatus{}
}

// gatedStarter blocks only the named trigger, so the concurrency test can hold
// one real fire in flight while the second HTTP request arrives.
type gatedStarter struct {
	inner interface {
		StartTurn(context.Context, harnessext.StartTurnRequest) (harnessext.StartTurnResult, error)
	}
	triggerID string
	entered   chan struct{}
	release   chan struct{}
}

func (s *gatedStarter) StartTurn(ctx context.Context, req harnessext.StartTurnRequest) (harnessext.StartTurnResult, error) {
	if mark, ok := req.Extras["trigger"].(map[string]any); ok && mark["id"] == s.triggerID {
		select {
		case <-s.entered:
		default:
			close(s.entered)
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return harnessext.StartTurnResult{}, ctx.Err()
		}
	}
	return s.inner.StartTurn(ctx, req)
}

func TestTriggerWebhookHMACAndInitialCardE2E(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	log := testLogger()

	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(klog, kernel.Options{})
	tenantID := uuid.New()
	if err := st.CreateTenant(ctx, tenantID, "w10-triggers"); err != nil {
		t.Fatal(err)
	}
	// Every trigger fires as one Bot (A4.2b3a follow-up): the fixture owns a
	// real Bot so registration validates and the fired session is attributable.
	bot, _ := createE2EBot(t, st, ctx, tenantID, "w10-trigger-bot")
	owner := storeTriggerOwner{store: st}
	hooks := harnessext.NewHookBus(harnessext.HookBusOptions{Logger: log})
	defer hooks.Close()
	starter := harnessext.NewTurnStarter(storeSessions{Store: st}, kern, hooks, log)
	tasks := tasklog.New(tasklog.Options{
		Committer:  kern,
		Reader:     kern,
		Projection: tasklog.NewPGProjection(st),
	})
	const prompt = "hmac ping from the webhook"
	engine := harnessext.NewTriggerEngine(harnessext.TriggerEngineOptions{
		Starter:   starter,
		Hooks:     hooks,
		Logger:    log,
		State:     st,
		Tasks:     tasks,
		Bots:      owner,
		Ownership: owner,
	})
	defer engine.Close()
	// The webhooks are tenant-bound: GET /v1/triggers is a tenant slice, and a
	// tenantless trigger is intentionally not listable to any tenant.
	if _, err := engine.Register(harnessext.TriggerSpec{
		ID: "web.test.hmac", Kind: harnessext.TriggerWebhook, BotID: bot.ID,
		TenantID: tenantID.String(), Prompt: prompt, Enabled: true,
		WebhookSecret: e2eWebhookSecret, MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Register(harnessext.TriggerSpec{
		ID: "web.test.unconfigured", Kind: harnessext.TriggerWebhook, BotID: bot.ID,
		TenantID: tenantID.String(), Prompt: "p", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	api := harnessapi.New(harnessapi.Config{Triggers: engine, Logger: log})
	server := httptest.NewServer(api)
	defer server.Close()
	client := server.Client()
	fireURL := server.URL + "/v1/triggers/web.test.hmac/fire"
	body := []byte(`{"idempotencyKey":"e2e-k1"}`)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature := e2eSign(e2eWebhookSecret, tenantID, "web.test.hmac", timestamp, "e2e-k1", body)

	var fired struct {
		Result harnessext.FireResult `json:"result"`
	}
	if code, _ := doSignedPost(t, client, fireURL, tenantID, body, timestamp, signature, &fired); code != http.StatusOK {
		t.Fatalf("signed fire: %d %+v", code, fired)
	}
	if fired.Result.Status != "fired" || fired.Result.Deduped || fired.Result.TaskCardID == "" {
		t.Fatalf("signed result = %+v", fired.Result)
	}
	sessionID := uuid.MustParse(fired.Result.SessionID)
	taskID := uuid.MustParse(fired.Result.TaskCardID)
	if want := tasklog.InitialTaskID(tenantID, sessionID); taskID != want {
		t.Fatalf("task card id = %s, want the session initial card %s", taskID, want)
	}
	card, err := st.GetTaskCardRow(ctx, tenantID, sessionID, taskID)
	if err != nil {
		t.Fatalf("initial card row: %v", err)
	}
	if card.Status != tasklog.StatusOpen || card.Origin != tasklog.OriginSession || card.Title != prompt {
		t.Fatalf("initial card = %+v", card)
	}

	// Exactly one user.message exists for the fire.
	events, err := kern.LoadEvents(ctx, kernel.Scope{TenantID: tenantID, SessionID: sessionID}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	userMessages := 0
	for _, event := range events {
		if event.Kind == kernel.KindUserMessage {
			userMessages++
			if !bytes.Contains(event.Payload, []byte(`"trigger"`)) {
				t.Fatalf("user.message lacks trigger provenance: %s", event.Payload)
			}
		}
	}
	if userMessages != 1 {
		t.Fatalf("user.message count = %d, want 1", userMessages)
	}

	// The wire status proves a secret is configured without exposing it.
	statuses, rawStatus := e2eGetTriggers(t, client, server.URL, tenantID)
	if strings.Contains(rawStatus, e2eWebhookSecret) {
		t.Fatalf("GET /v1/triggers leaked the secret: %s", rawStatus)
	}
	hmacStatus := findTriggerStatus(t, statuses, "web.test.hmac")
	if !hmacStatus.WebhookConfigured || hmacStatus.Fires != 1 {
		t.Fatalf("hmac status = %+v", hmacStatus)
	}
	if unconfigured := findTriggerStatus(t, statuses, "web.test.unconfigured"); unconfigured.WebhookConfigured {
		t.Fatalf("unconfigured trigger reports a secret: %+v", unconfigured)
	}

	// A repeated delivery inside the window is deduped, not committed twice.
	var duplicate struct {
		Result harnessext.FireResult `json:"result"`
	}
	if code, _ := doSignedPost(t, client, fireURL, tenantID, body, timestamp, signature, &duplicate); code != http.StatusOK {
		t.Fatalf("duplicate delivery: %d", code)
	}
	if !duplicate.Result.Deduped || duplicate.Result.SessionID != fired.Result.SessionID {
		t.Fatalf("duplicate result = %+v", duplicate.Result)
	}
	events, err = kern.LoadEvents(ctx, kernel.Scope{TenantID: tenantID, SessionID: sessionID}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	userMessages = 0
	for _, event := range events {
		if event.Kind == kernel.KindUserMessage {
			userMessages++
		}
	}
	if userMessages != 1 {
		t.Fatalf("duplicate delivery committed a second user.message")
	}

	// Bad signature, stale timestamp and an unconfigured trigger all fail
	// closed, and every refusal is in the audit.
	badBody := []byte(`{"idempotencyKey":"e2e-bad"}`)
	badTimestamp := strconv.FormatInt(time.Now().Unix(), 10)
	badSignature := "sha256=" + strings.Repeat("0", 64)
	if code, _ := doSignedPost(t, client, fireURL, tenantID, badBody, badTimestamp, badSignature, nil); code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d, want 401", code)
	}
	staleTimestamp := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	staleSignature := e2eSign(e2eWebhookSecret, tenantID, "web.test.hmac", staleTimestamp, "e2e-stale", badBody)
	if code, _ := doSignedPost(t, client, fireURL, tenantID, badBody, staleTimestamp, staleSignature, nil); code != http.StatusUnauthorized {
		t.Fatalf("stale timestamp: %d, want 401", code)
	}
	openBody := []byte(`{"idempotencyKey":"e2e-open"}`)
	openTimestamp := strconv.FormatInt(time.Now().Unix(), 10)
	openSignature := e2eSign(e2eWebhookSecret, tenantID, "web.test.unconfigured", openTimestamp, "e2e-open", openBody)
	if code, _ := doSignedPost(t, client,
		server.URL+"/v1/triggers/web.test.unconfigured/fire", tenantID,
		openBody, openTimestamp, openSignature, nil); code != http.StatusForbidden {
		t.Fatalf("unconfigured trigger: %d, want 403", code)
	}
	statuses, _ = e2eGetTriggers(t, client, server.URL, tenantID)
	hmacStatus = findTriggerStatus(t, statuses, "web.test.hmac")
	if hmacStatus.Rejected < 2 || hmacStatus.Dedupes != 1 {
		t.Fatalf("audit counters after refusals: %+v", hmacStatus)
	}
	unconfigured := findTriggerStatus(t, statuses, "web.test.unconfigured")
	if unconfigured.Rejected != 1 {
		t.Fatalf("unconfigured refusal not audited: %+v", unconfigured)
	}

	// Manual fire stays an authenticated operator action (no signature).
	if code := doJSON(t, client, http.MethodPost, fireURL, tenantID,
		map[string]any{"idempotencyKey": "e2e-manual"}, nil); code != http.StatusOK {
		t.Fatalf("manual fire: %d", code)
	}
}

func TestTriggerConcurrencyLimitOverHTTP(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	log := testLogger()
	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(klog, kernel.Options{})
	tenantID := uuid.New()
	if err := st.CreateTenant(ctx, tenantID, "w10-concurrency"); err != nil {
		t.Fatal(err)
	}
	bot, _ := createE2EBot(t, st, ctx, tenantID, "w10-concurrency-bot")
	owner := storeTriggerOwner{store: st}
	starter := harnessext.NewTurnStarter(storeSessions{Store: st}, kern, nil, log)
	gated := &gatedStarter{
		inner:     starter,
		triggerID: "web.test.busy",
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(gated.release) }) })
	engine := harnessext.NewTriggerEngine(harnessext.TriggerEngineOptions{
		Starter: gated, Logger: log, State: st, Bots: owner, Ownership: owner,
	})
	defer engine.Close()
	if _, err := engine.Register(harnessext.TriggerSpec{
		ID: "web.test.busy", Kind: harnessext.TriggerWebhook, BotID: bot.ID,
		TenantID: tenantID.String(), Prompt: "busy", Enabled: true,
		WebhookSecret: e2eWebhookSecret, MaxConcurrent: 1,
	}); err != nil {
		t.Fatal(err)
	}
	api := harnessapi.New(harnessapi.Config{Triggers: engine, Logger: log})
	server := httptest.NewServer(api)
	defer server.Close()
	client := server.Client()
	fireURL := server.URL + "/v1/triggers/web.test.busy/fire"

	firstBody := []byte(`{"idempotencyKey":"busy-1"}`)
	firstTimestamp := strconv.FormatInt(time.Now().Unix(), 10)
	firstSignature := e2eSign(e2eWebhookSecret, tenantID, "web.test.busy", firstTimestamp, "busy-1", firstBody)
	firstStatus := make(chan int, 1)
	go func() {
		// No t.Fatal in a goroutine: report a sentinel status and let the test
		// goroutine fail with context.
		req, err := newSignedRequest(fireURL, tenantID, firstBody, firstTimestamp, firstSignature)
		if err != nil {
			firstStatus <- -1
			return
		}
		res, err := client.Do(req)
		if err != nil {
			firstStatus <- -1
			return
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		firstStatus <- res.StatusCode
	}()
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first fire never reached the starter")
	}

	secondBody := []byte(`{"idempotencyKey":"busy-2"}`)
	secondTimestamp := strconv.FormatInt(time.Now().Unix(), 10)
	secondSignature := e2eSign(e2eWebhookSecret, tenantID, "web.test.busy", secondTimestamp, "busy-2", secondBody)
	code, headers := doSignedPost(t, client, fireURL, tenantID, secondBody, secondTimestamp, secondSignature, nil)
	if code != http.StatusTooManyRequests {
		t.Fatalf("second in-flight delivery: %d, want 429", code)
	}
	if headers.Get("Retry-After") == "" {
		t.Fatal("429 response has no Retry-After")
	}
	releaseOnce.Do(func() { close(gated.release) })
	select {
	case code := <-firstStatus:
		if code != http.StatusOK {
			t.Fatalf("first delivery: %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first delivery did not finish")
	}
	statuses, _ := e2eGetTriggers(t, client, server.URL, tenantID)
	status := findTriggerStatus(t, statuses, "web.test.busy")
	if status.Throttled != 1 || status.Fires != 1 {
		t.Fatalf("concurrency audit: %+v", status)
	}
}

// recordingStarter counts fires and records their reasons for the catch-up
// test; it does not need a kernel.
type recordingStarter struct {
	mu     sync.Mutex
	starts []harnessext.StartTurnRequest
}

func (s *recordingStarter) StartTurn(_ context.Context, req harnessext.StartTurnRequest) (harnessext.StartTurnResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts = append(s.starts, req)
	return harnessext.StartTurnResult{
		TenantID: req.TenantID, SessionID: req.SessionID, Seq: int64(len(s.starts)), RequestID: req.RequestID,
	}, nil
}

func (s *recordingStarter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.starts)
}

func (s *recordingStarter) requests() []harnessext.StartTurnRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]harnessext.StartTurnRequest(nil), s.starts...)
}

func TestTriggerCatchUpUsesDurableCursorE2E(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	if err := st.CreateTenant(ctx, tenantID, "w10-catchup"); err != nil {
		t.Fatal(err)
	}
	bot, _ := createE2EBot(t, st, ctx, tenantID, "w10-catchup-bot")
	owner := storeTriggerOwner{store: st}
	seed := time.Now().UTC().Add(-3500 * time.Millisecond)
	if err := st.SaveTriggerState(ctx, store.TriggerState{
		TenantID:        tenantID,
		TriggerID:       "e2e.tick",
		LastScheduledAt: seed,
		LastFiredAt:     seed,
		LastStatus:      "fired",
	}); err != nil {
		t.Fatal(err)
	}
	starter := &recordingStarter{}
	engine := harnessext.NewTriggerEngine(harnessext.TriggerEngineOptions{
		Starter: starter, Logger: testLogger(), State: st, CatchUpLimit: 2,
		Bots: owner, Ownership: owner,
	})
	defer engine.Close()
	if _, err := engine.Register(harnessext.TriggerSpec{
		ID: "e2e.tick", Kind: harnessext.TriggerInterval, Schedule: "1s",
		BotID: bot.ID, Prompt: "tick", TenantID: tenantID.String(), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 5*time.Second, func() bool { return starter.count() >= 2 }, "durable catch-up fires")
	waitForCondition(t, 5*time.Second, func() bool {
		status, _ := engine.Get("e2e.tick")
		return status.Skipped >= 1
	}, "durable catch-up skip audit")
	status, _ := engine.Get("e2e.tick")
	if status.LastScheduledAt == nil || !status.LastScheduledAt.After(seed) {
		t.Fatalf("status cursor did not advance: %+v", status.LastScheduledAt)
	}
	sawCatchUp := false
	for _, req := range starter.requests() {
		if mark, ok := req.Extras["trigger"].(map[string]any); ok && mark["reason"] == "catchup" {
			sawCatchUp = true
		}
	}
	if !sawCatchUp {
		t.Fatalf("no catch-up turn: %+v", starter.requests())
	}
	engine.Close()

	cursor, ok, err := st.LoadTriggerState(ctx, tenantID, "e2e.tick")
	if err != nil || !ok {
		t.Fatalf("load persisted cursor: ok=%v err=%v", ok, err)
	}
	if !cursor.LastScheduledAt.After(seed) {
		t.Fatalf("persisted cursor did not advance: %v (seed %v)", cursor.LastScheduledAt, seed)
	}

	// Restart over the same database: registering again must not replay a
	// window that was already processed. A genuinely newer window may fire on
	// its normal timer, so the assertion is "no duplicate requestId", not "no
	// start at all".
	beforeRequests := starter.requests()
	seen := make(map[string]bool, len(beforeRequests))
	for _, req := range beforeRequests {
		seen[req.RequestID] = true
	}
	restarted := harnessext.NewTriggerEngine(harnessext.TriggerEngineOptions{
		Starter: starter, Logger: testLogger(), State: st, CatchUpLimit: 2,
		Bots: owner, Ownership: owner,
	})
	defer restarted.Close()
	if _, err := restarted.Register(harnessext.TriggerSpec{
		ID: "e2e.tick", Kind: harnessext.TriggerInterval, Schedule: "1s",
		BotID: bot.ID, Prompt: "tick", TenantID: tenantID.String(), Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	for _, req := range starter.requests()[len(beforeRequests):] {
		if seen[req.RequestID] {
			t.Fatalf("restart replayed an already processed window: %q", req.RequestID)
		}
	}
}
