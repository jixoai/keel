package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// loadSessionEvents reads one session's whole log (the tests assert on
// committed facts, not on HTTP echoes).
func loadSessionEvents(t *testing.T, st *store.Store, tenant, session uuid.UUID) []store.Event {
	t.Helper()
	events, err := st.LoadEvents(context.Background(), tenant, session, 0, 1000)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	return events
}

// committedInput returns the committed user.message payload carrying requestID
// and its seq.
func committedInput(t *testing.T, st *store.Store, tenant, session uuid.UUID, requestID string) (map[string]json.RawMessage, int64) {
	t.Helper()
	for _, ev := range loadSessionEvents(t, st, tenant, session) {
		if ev.Kind != store.EventUserMessage {
			continue
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("decode user.message: %v", err)
		}
		var got string
		_ = json.Unmarshal(payload["requestId"], &got)
		if got == requestID {
			return payload, ev.Seq
		}
	}
	t.Fatalf("no committed user.message with requestId %q", requestID)
	return nil, 0
}

// rawJSONEqual compares a stored JSON fragment against an expected Go value by
// content (Postgres decides key order and spacing on read-back).
func rawJSONEqual(t *testing.T, got json.RawMessage, want any) {
	t.Helper()
	wantRaw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := json.Unmarshal(wantRaw, &wantVal); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Fatalf("stored value = %s, want %s", got, wantRaw)
	}
}

// postRetry posts one retry request. requestIDHeader, when set, rides along as
// X-Request-ID to pin that the client-supplied idempotency header does not
// deduplicate retries.
func postRetry(t *testing.T, hc *http.Client, hs *httptest.Server, tenant string, session uuid.UUID, turnID, requestIDHeader string, out any) int {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"turnId": turnID})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/retry", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", "application/json")
	if requestIDHeader != "" {
		req.Header.Set("X-Request-ID", requestIDHeader)
	}
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("decode retry %d: %v body=%s", res.StatusCode, err, body)
		}
	}
	return res.StatusCode
}

type retryBody struct {
	Created   *bool  `json:"created"`
	Seq       *int64 `json:"seq"`
	RequestID string `json:"requestId"`
	Error     string `json:"error"`
}

// TestRetryTurnAppendsANewInputAndQueuesTheRun pins the ledger semantics of
// POST /v1/sessions/{id}/retry: the original events are never rewritten, a NEW
// user.message repeats the original input's text/attachments/selection with a
// fresh retry:<uuid> requestId, delivery queue, and retryOf naming the original
// input seq — and the run is enqueued for it.
// productServer is the retry tests' server with the seam a deployment wires:
// the product layer validates selections, and a retry carries the stored one
// verbatim, so the tests use the permissive stand-in (a validator that accepts
// any object) unless they are testing the refusal itself.
func productServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	return newTestServer(t, func(c *Config) {
		c.ValidateSelection = func(raw json.RawMessage) error {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
				return fmt.Errorf("selection must be a JSON object")
			}
			return nil
		}
	})
}

func TestRetryTurnAppendsANewInputAndQueuesTheRun(t *testing.T) {
	hs, st := productServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)
	ctx := context.Background()

	// The original input is planted through the raw edge endpoint so it can
	// carry attachments and a selection next to its text.
	originalText := "  what is the status?\n  (please retry this)  "
	originalPayload := map[string]any{
		"text":        originalText,
		"requestId":   "orig-1",
		"attachments": []map[string]any{{"fileId": "blob-1", "name": "log.txt", "mime": "text/plain"}},
		"selection":   map[string]any{"providerId": "s2api", "modelId": "deepseek-v4.1-flash"},
	}
	var appended struct {
		Seq int64 `json:"seq"`
	}
	if code := doJSON(t, hc, http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/events", tenant,
		map[string]any{"kind": "user.message", "payload": originalPayload}, &appended); code != 201 {
		t.Fatalf("append original input: %d", code)
	}
	before := loadSessionEvents(t, st, tenantID, session)
	if len(before) != 1 || appended.Seq != 1 {
		t.Fatalf("original log = %d events, seq %d", len(before), appended.Seq)
	}

	turnID := "t_" + strconv.FormatInt(appended.Seq, 10)
	var retry retryBody
	if code := postRetry(t, hc, hs, tenant, session, turnID, "", &retry); code != http.StatusCreated {
		t.Fatalf("retry: %d (%+v)", code, retry)
	}
	if retry.Created == nil || !*retry.Created {
		t.Fatalf("retry body = %+v, want created true", retry)
	}
	if retry.Seq == nil || *retry.Seq != appended.Seq+1 {
		t.Fatalf("retry seq = %v, want %d", retry.Seq, appended.Seq+1)
	}
	if !strings.HasPrefix(retry.RequestID, "retry:") {
		t.Fatalf("retry requestId = %q, want the generated retry:<uuid> key", retry.RequestID)
	}

	payload, seq := committedInput(t, st, tenantID, session, retry.RequestID)
	if seq != *retry.Seq {
		t.Fatalf("committed retry seq = %d, response said %d", seq, *retry.Seq)
	}
	var text, delivery string
	if err := json.Unmarshal(payload["text"], &text); err != nil || text != originalText {
		t.Fatalf("retry text = %q (err %v), want the original %q", text, err, originalText)
	}
	if err := json.Unmarshal(payload["delivery"], &delivery); err != nil || delivery != kernel.DeliveryQueue {
		t.Fatalf("retry delivery = %q (err %v), want %q", delivery, err, kernel.DeliveryQueue)
	}
	var retryOf int64
	if err := json.Unmarshal(payload["retryOf"], &retryOf); err != nil || retryOf != appended.Seq {
		t.Fatalf("retryOf = %v (err %v), want the original input seq %d", retryOf, err, appended.Seq)
	}
	rawJSONEqual(t, payload["attachments"], originalPayload["attachments"])
	rawJSONEqual(t, payload["selection"], originalPayload["selection"])
	var newRequestID string
	if err := json.Unmarshal(payload["requestId"], &newRequestID); err != nil || newRequestID != retry.RequestID {
		t.Fatalf("stored requestId = %q (err %v), want %q", newRequestID, err, retry.RequestID)
	}

	// The original events are untouched: the ledger only grew by one append.
	after := loadSessionEvents(t, st, tenantID, session)
	if len(after) != len(before)+1 {
		t.Fatalf("log grew from %d to %d events, want exactly one append", len(before), len(after))
	}
	for i := range before {
		if before[i].Seq != after[i].Seq || before[i].Kind != after[i].Kind || !bytes.Equal(before[i].Payload, after[i].Payload) {
			t.Fatalf("original event %d changed: %+v -> %+v", before[i].Seq, before[i], after[i])
		}
	}

	// The run is enqueued (no worker runs in this test, so it stays pending).
	run, err := st.GetRun(ctx, tenantID, session)
	if err != nil {
		t.Fatalf("run after retry: %v", err)
	}
	if run.Status != store.RunPending {
		t.Fatalf("run status = %q, want %q", run.Status, store.RunPending)
	}

	// The same turnId retried twice is two attempts on purpose — even when the
	// client reuses its own idempotency header: a retry is an intentional
	// second ask, not a replay of the first.
	var second retryBody
	if code := postRetry(t, hc, hs, tenant, session, turnID, "client-supplied-key", &second); code != http.StatusCreated {
		t.Fatalf("second retry: %d (%+v)", code, second)
	}
	if second.Seq == nil || *second.Seq != *retry.Seq+1 || second.RequestID == retry.RequestID {
		t.Fatalf("second retry = %+v, want a new attempt after %d", second, *retry.Seq)
	}
	var attempts int
	for _, ev := range loadSessionEvents(t, st, tenantID, session) {
		if ev.Kind != store.EventUserMessage {
			continue
		}
		var payload struct {
			RetryOf *int64 `json:"retryOf"`
		}
		_ = json.Unmarshal(ev.Payload, &payload)
		if payload.RetryOf != nil && *payload.RetryOf == appended.Seq {
			attempts++
		}
	}
	if attempts != 2 {
		t.Fatalf("retry attempts in the log = %d, want 2", attempts)
	}
}

// TestRetryTurnResolvesThroughAssistantInputSeqs pins the second resolution
// path: a turn the client built from an assistant message (turnId t_<assistant
// seq>) resolves to the newest user input that assistant's inputSeqs records,
// and a turn on a non-conversation event is refused with 400.
func TestRetryTurnResolvesThroughAssistantInputSeqs(t *testing.T) {
	hs, st := productServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)
	ctx := context.Background()
	scope := kernel.Scope{TenantID: tenantID, SessionID: session}

	if code := doJSON(t, hc, http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]string{"text": "first question", "requestId": "orig-input"}, nil); code != 201 {
		t.Fatalf("original input: %d", code)
	}

	// The assistant answer is written through the kernel (an owner-scoped kind
	// the edge endpoint refuses), under the session's run lease.
	pg, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	k := kernel.New(pg, kernel.Options{})
	lease, err := k.ClaimScope(ctx, scope, "retry-resolution-test", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer func() {
		if err := k.Release(context.Background(), lease); err != nil {
			t.Logf("release: %v", err)
		}
	}()
	if _, err := k.Commit(ctx, kernel.CommitRequest{
		Scope: scope, Kind: kernel.KindAssistantMessage, Token: lease.Token, GenerationID: "gen-1",
		Payload: json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"answer"}],"inputSeqs":[1]}`),
	}); err != nil {
		t.Fatalf("commit assistant: %v", err)
	}
	// A maintenance event is a committed fact without a user input.
	if _, err := k.Commit(ctx, kernel.CommitRequest{
		Scope: scope, Kind: kernel.KindCompaction, Token: lease.Token,
		Payload: json.RawMessage(`{"summary":"compacted","status":"completed"}`),
	}); err != nil {
		t.Fatalf("commit compaction: %v", err)
	}

	var retry retryBody
	if code := postRetry(t, hc, hs, tenant, session, "t_2", "", &retry); code != http.StatusCreated {
		t.Fatalf("retry of the assistant turn: %d (%+v)", code, retry)
	}
	if retry.Seq == nil || *retry.Seq != 4 {
		t.Fatalf("retry seq = %v, want 4 (appended after the compaction)", retry.Seq)
	}
	payload, _ := committedInput(t, st, tenantID, session, retry.RequestID)
	var text string
	_ = json.Unmarshal(payload["text"], &text)
	if text != "first question" {
		t.Fatalf("resolved text = %q, want the input seq 1 answered by the assistant", text)
	}
	var retryOf int64
	if err := json.Unmarshal(payload["retryOf"], &retryOf); err != nil || retryOf != 1 {
		t.Fatalf("retryOf = %v (err %v), want 1", retryOf, err)
	}

	// The run already exists (claimed above) and must stay running: a retry
	// never downgrades a live attempt.
	run, err := st.GetRun(ctx, tenantID, session)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Status != store.RunRunning {
		t.Fatalf("run status = %q, want %q", run.Status, store.RunRunning)
	}

	// A turn on the compaction event is not a user turn.
	var notUser retryBody
	if code := postRetry(t, hc, hs, tenant, session, "t_3", "", &notUser); code != http.StatusBadRequest {
		t.Fatalf("retry of a maintenance turn: %d (%+v)", code, notUser)
	}
	if !strings.Contains(notUser.Error, "not a user turn") {
		t.Fatalf("error = %q", notUser.Error)
	}
}

// TestRetryTurnRefusals: an unknown turn and a foreign or missing session are
// 404 without leaking existence; a malformed turnId is 400; nothing is written
// on any refusal.
func TestRetryTurnRefusals(t *testing.T) {
	hs, st := productServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)

	if code := doJSON(t, hc, http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/input", tenant,
		map[string]string{"text": "hello", "requestId": "refusal-orig"}, nil); code != 201 {
		t.Fatalf("original input: %d", code)
	}
	before := loadSessionEvents(t, st, tenantID, session)

	// Unknown turn: the session exists, the seq names no committed event.
	var unknown retryBody
	if code := postRetry(t, hc, hs, tenant, session, "t_7", "", &unknown); code != http.StatusNotFound {
		t.Fatalf("unknown turn: %d (%+v)", code, unknown)
	}
	if !strings.Contains(unknown.Error, "turn not found") {
		t.Fatalf("unknown turn error = %q", unknown.Error)
	}

	// A session that does not exist under the tenant: 404.
	var missing retryBody
	if code := postRetry(t, hc, hs, tenant, uuid.New(), "t_1", "", &missing); code != http.StatusNotFound {
		t.Fatalf("missing session: %d (%+v)", code, missing)
	}

	// A foreign tenant sees the same 404 (never a 403/existence leak).
	otherTenant := uuid.NewString()
	if code := createTestTenant(t, hc, hs.URL, otherTenant, "foreign"); code != 201 {
		t.Fatalf("create foreign tenant: %d", code)
	}
	var foreign retryBody
	if code := postRetry(t, hc, hs, otherTenant, session, "t_1", "", &foreign); code != http.StatusNotFound {
		t.Fatalf("foreign tenant: %d (%+v)", code, foreign)
	}
	if !strings.Contains(foreign.Error, "session not found") {
		t.Fatalf("foreign tenant error = %q", foreign.Error)
	}

	// Malformed identifiers never reach the log either.
	for _, turnID := range []string{"banana", "t_", "t_x", "t_0", "t_-2", "x_1", "1"} {
		var bad retryBody
		if code := postRetry(t, hc, hs, tenant, session, turnID, "", &bad); code != http.StatusBadRequest {
			t.Fatalf("turnId %q: %d (%+v), want 400", turnID, code, bad)
		}
		if !strings.Contains(bad.Error, "turnId") {
			t.Fatalf("turnId %q error = %q", turnID, bad.Error)
		}
	}
	var empty retryBody
	if code := postRetry(t, hc, hs, tenant, session, "", "", &empty); code != http.StatusBadRequest {
		t.Fatalf("empty turnId: %d (%+v)", code, empty)
	}
	if !strings.Contains(empty.Error, "turnId is required") {
		t.Fatalf("empty turnId error = %q", empty.Error)
	}

	// No refusal appended anything.
	after := loadSessionEvents(t, st, tenantID, session)
	if len(after) != len(before) {
		t.Fatalf("log grew from %d to %d events across refusals", len(before), len(after))
	}
}

// TestRetryRefusesAnUnresolvableStoredSelection pins the gate the re-ask shares
// with a fresh input: the retry copies the stored selection verbatim, so a
// selection the current configuration can no longer resolve is refused before
// anything is written, instead of being committed and failing asynchronously as
// a run.
func TestRetryRefusesAnUnresolvableStoredSelection(t *testing.T) {
	hs, st := newTestServer(t, func(c *Config) {
		c.ValidateSelection = func(raw json.RawMessage) error {
			var sel struct {
				ProviderID string `json:"providerId"`
				ModelID    string `json:"modelId"`
			}
			if err := json.Unmarshal(raw, &sel); err != nil {
				return err
			}
			if sel.ProviderID != "s2api" {
				return fmt.Errorf("unknown provider %q", sel.ProviderID)
			}
			return nil
		}
	})
	hc := hs.Client()
	tenant := uuid.NewString()
	session := inputSetup(t, hs, tenant)
	tenantID := uuid.MustParse(tenant)

	var appended struct {
		Seq int64 `json:"seq"`
	}
	if code := doJSON(t, hc, http.MethodPost, hs.URL+"/v1/sessions/"+session.String()+"/events", tenant,
		map[string]any{
			"kind": "user.message",
			"payload": map[string]any{
				"text":      "ask the old gateway",
				"requestId": "orig-gone",
				"selection": map[string]any{"providerId": "gone-gw", "modelId": "deepseek-v4.1-flash"},
			},
		}, &appended); code != 201 {
		t.Fatalf("append original input: %d", code)
	}
	before := loadSessionEvents(t, st, tenantID, session)

	var retry retryBody
	code := postRetry(t, hc, hs, tenant, session, "t_"+strconv.FormatInt(appended.Seq, 10), "", &retry)
	if code != http.StatusBadRequest {
		t.Fatalf("retry with an unresolvable selection: %d (%+v), want 400", code, retry)
	}
	if after := loadSessionEvents(t, st, tenantID, session); len(after) != len(before) {
		t.Fatalf("refused retry wrote %d events (before %d)", len(after), len(before))
	}
}
