package harnessapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/approval"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/notify"
)

type apiClock struct{ now time.Time }

func (c *apiClock) Now() time.Time          { return c.now }
func (c *apiClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newApprovalAPI(t *testing.T) (*httptest.Server, *harnessext.DecisionBroker, *approval.Service) {
	t.Helper()
	broker := harnessext.NewDecisionBroker(harnessext.DecisionBrokerOptions{
		Store:  harnessext.NewMemoryDecisionStore(),
		Logger: testLogger(),
	})
	t.Cleanup(broker.Close)
	service := approval.NewService(approval.Options{Logger: testLogger()})
	server := httptest.NewServer(New(Config{
		Decisions: broker,
		Approvals: service,
		Logger:    testLogger(),
	}))
	t.Cleanup(server.Close)
	return server, broker, service
}

func openApproval(t *testing.T, server *httptest.Server, tenant, session uuid.UUID, body map[string]any) harnessext.Decision {
	t.Helper()
	var opened struct {
		Decision harnessext.Decision `json:"decision"`
	}
	code := doJSON(t, server.Client(), http.MethodPost,
		server.URL+"/v1/sessions/"+session.String()+"/decisions", tenant, body, &opened)
	if code != http.StatusCreated {
		t.Fatalf("open decision = %d", code)
	}
	return opened.Decision
}

// TestApprovalAnswerKeepsOpenContext pins P1-A: the real HTTP answer path must
// merge the open provenance row (MCP arguments/digest/attempt identity) with
// the answer metadata, not replace the whole Context with requestId etc.
func TestApprovalAnswerKeepsOpenContext(t *testing.T) {
	server, _, service := newApprovalAPI(t)
	tenant, session := uuid.New(), uuid.New()
	decision := openApproval(t, server, tenant, session, map[string]any{
		"kind":    "approval.confirm",
		"prompt":  "Delete?",
		"options": []map[string]string{{"id": "allow", "label": "Allow"}, {"id": "deny", "label": "Deny"}},
	})
	opened := map[string]string{
		"arguments":         `{"path":"/tmp/x"}`,
		"argumentsDigest":   "sha256:abc",
		"argumentsRedacted": "true",
		"attemptId":         "attempt-1",
		"toolCallId":        "call-1",
		"server":            "filesystem",
		"tool":              "delete_file",
	}
	if _, _, err := service.Open(context.Background(), approval.Record{
		TenantID: tenant.String(), SessionID: session.String(), DecisionID: decision.DecisionID,
		Kind: decision.Kind, Mode: approval.ModeConfirm, Actor: "agent",
		Channel: approval.ChannelTool, Context: opened,
	}); err != nil {
		t.Fatalf("open provenance: %v", err)
	}

	var answered struct {
		Provenance approval.Record `json:"provenance"`
	}
	req, _ := http.NewRequest(http.MethodPost,
		server.URL+"/v1/sessions/"+session.String()+"/decisions/"+decision.DecisionID,
		jsonBody(t, map[string]any{"optionId": "allow"}))
	req.Header.Set("X-Tenant-ID", tenant.String())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Keel-Actor", "owner@example.test")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("answer = %d", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&answered); err != nil {
		t.Fatal(err)
	}
	for key, value := range opened {
		if answered.Provenance.Context[key] != value {
			t.Fatalf("answer lost open context[%s] = %q, want %q (context=%v)",
				key, answered.Provenance.Context[key], value, answered.Provenance.Context)
		}
	}
	if answered.Provenance.Context["requestId"] != decision.RequestID {
		t.Fatalf("answer context requestId = %q, want %q", answered.Provenance.Context["requestId"], decision.RequestID)
	}
}

func TestApprovalAnswerRecordsValidatedProvenance(t *testing.T) {
	server, _, _ := newApprovalAPI(t)
	tenant, session := uuid.New(), uuid.New()
	decision := openApproval(t, server, tenant, session, map[string]any{
		"kind":    "approval.confirm",
		"prompt":  "Deploy?",
		"options": []map[string]string{{"id": "yes", "label": "Yes"}, {"id": "no", "label": "No"}},
	})

	var answered struct {
		Decision   harnessext.Decision `json:"decision"`
		Duplicate  bool                `json:"duplicate"`
		Provenance approval.Record     `json:"provenance"`
	}
	req, _ := http.NewRequest(http.MethodPost,
		server.URL+"/v1/sessions/"+session.String()+"/decisions/"+decision.DecisionID,
		jsonBody(t, map[string]any{"optionId": "yes"}))
	req.Header.Set("X-Tenant-ID", tenant.String())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Keel-Actor", "owner@example.test")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("answer = %d", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&answered); err != nil {
		t.Fatal(err)
	}
	if answered.Provenance.Status != approval.StatusAnswered || !answered.Provenance.Validated {
		t.Fatalf("provenance = %+v", answered.Provenance)
	}
	if answered.Provenance.Actor != "owner@example.test" || answered.Provenance.Channel != approval.ChannelHTTP {
		t.Fatalf("provenance context = %+v", answered.Provenance)
	}

	// The audit is readable next to the decision.
	var audit struct {
		Decision   harnessext.Decision `json:"decision"`
		Provenance approval.Record     `json:"provenance"`
	}
	code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/sessions/"+session.String()+"/approvals/"+decision.DecisionID, tenant, nil, &audit)
	if code != http.StatusOK || audit.Provenance.OptionID != "yes" || !audit.Provenance.Validated {
		t.Fatalf("audit = %d %+v", code, audit)
	}

	// Replaying the same answer is idempotent and keeps one record.
	duplicate := doJSON(t, server.Client(), http.MethodPost,
		server.URL+"/v1/sessions/"+session.String()+"/decisions/"+decision.DecisionID, tenant,
		map[string]any{"optionId": "yes"}, &answered)
	if duplicate != http.StatusOK || !answered.Duplicate {
		t.Fatalf("duplicate answer = %d duplicate=%v", duplicate, answered.Duplicate)
	}
	var list struct {
		Approvals []map[string]any `json:"approvals"`
		Count     int              `json:"count"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/sessions/"+session.String()+"/approvals", tenant, nil, &list); code != http.StatusOK || list.Count != 1 {
		t.Fatalf("approval list = %d %+v", code, list)
	}
}

func TestApprovalOtherEscapeIsNotValidated(t *testing.T) {
	server, _, _ := newApprovalAPI(t)
	tenant, session := uuid.New(), uuid.New()
	decision := openApproval(t, server, tenant, session, map[string]any{
		"kind":          "approval.choice",
		"prompt":        "Which region?",
		"options":       []map[string]string{{"id": "eu", "label": "Europe"}, {"id": "us", "label": "US"}},
		"allowFreeText": true,
	})
	var answered struct {
		Provenance approval.Record `json:"provenance"`
	}
	code := doJSON(t, server.Client(), http.MethodPost,
		server.URL+"/v1/sessions/"+session.String()+"/decisions/"+decision.DecisionID, tenant,
		map[string]any{"text": "latam"}, &answered)
	if code != http.StatusOK {
		t.Fatalf("answer = %d", code)
	}
	if answered.Provenance.Validated || !answered.Provenance.Escape {
		t.Fatalf("escape provenance = %+v", answered.Provenance)
	}
	if answered.Provenance.Actor != approval.UnknownActor {
		t.Fatalf("missing actor was not recorded honestly: %+v", answered.Provenance)
	}
}

func TestNonApprovalDecisionHasNoProvenance(t *testing.T) {
	server, broker, service := newApprovalAPI(t)
	tenant, session := uuid.New(), uuid.New()
	decision, _, err := broker.Open(context.Background(), harnessext.OpenDecisionRequest{
		TenantID: tenant, SessionID: session, Kind: "clarify", Prompt: "Which file?",
		Options: []harnessext.DecisionOption{{ID: "a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var answered map[string]any
	code := doJSON(t, server.Client(), http.MethodPost,
		server.URL+"/v1/sessions/"+session.String()+"/decisions/"+decision.DecisionID, tenant,
		map[string]any{"optionId": "a"}, &answered)
	if code != http.StatusOK {
		t.Fatalf("answer = %d", code)
	}
	if _, present := answered["provenance"]; present {
		t.Fatalf("clarify answer gained approval provenance: %+v", answered)
	}
	rows, err := service.List(context.Background(), tenant, session)
	if err != nil || len(rows) != 0 {
		t.Fatalf("clarify provenance rows = %v err=%v", rows, err)
	}
}

func TestApprovalRoutesAreTenantScoped(t *testing.T) {
	server, _, _ := newApprovalAPI(t)
	tenant, session := uuid.New(), uuid.New()
	decision := openApproval(t, server, tenant, session, map[string]any{
		"kind": "approval.prompt", "prompt": "x", "allowFreeText": true,
	})
	other := uuid.New()
	if code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/sessions/"+session.String()+"/approvals/"+decision.DecisionID, other, nil, nil); code != http.StatusNotFound {
		t.Fatalf("foreign tenant audit = %d, want 404", code)
	}
}

type notifyAPI struct {
	server  *httptest.Server
	service *notify.Service
	clock   *apiClock
	tenant  uuid.UUID
	session uuid.UUID
}

func newNotifyAPI(t *testing.T) *notifyAPI {
	t.Helper()
	tenant, session := uuid.New(), uuid.New()
	mem := kernel.NewMemStore()
	mem.CreateSession(tenant, session)
	clock := &apiClock{now: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	service := notify.NewService(notify.Options{
		Store:     notify.NewMemoryStore(),
		Committer: kernel.New(mem, kernel.Options{}),
		Logger:    testLogger(),
		Now:       clock.Now,
		URLTTL:    30 * time.Minute,
	})
	server := httptest.NewServer(New(Config{Notifications: service, Access: permitAllAccess(), Logger: testLogger()}))
	t.Cleanup(server.Close)
	return &notifyAPI{server: server, service: service, clock: clock, tenant: tenant, session: session}
}

func (n *notifyAPI) push(t *testing.T, consumers ...notify.ConsumerSpec) notify.View {
	t.Helper()
	view, err := n.service.Push(context.Background(), notify.PushRequest{
		TenantID:  n.tenant,
		SessionID: n.session,
		Text:      "please look",
		Consumers: consumers,
	})
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func tokenOf(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query().Get("token")
}

func TestNotificationRoutesAckListAndURL(t *testing.T) {
	api := newNotifyAPI(t)
	view := api.push(t, notify.ConsumerSpec{ID: "web", Kind: notify.ConsumerClient, Online: true},
		notify.ConsumerSpec{ID: "mail", Kind: notify.ConsumerURL},
		notify.ConsumerSpec{ID: "backup", Kind: notify.ConsumerURL})

	var list struct {
		Count         int           `json:"count"`
		Notifications []notify.View `json:"notifications"`
	}
	code := doJSON(t, api.server.Client(), http.MethodGet,
		api.server.URL+"/v1/notifications?sessionId="+api.session.String(), api.tenant, nil, &list)
	if code != http.StatusOK || list.Count != 1 || list.Notifications[0].NotificationID != view.NotificationID {
		t.Fatalf("list = %d %+v", code, list)
	}
	if list.Notifications[0].URLs != nil {
		t.Fatalf("list re-exposed capability URLs: %+v", list.Notifications[0].URLs)
	}

	var acked struct {
		Notification notify.View `json:"notification"`
		Duplicate    bool        `json:"duplicate"`
	}

	// The URL route needs no tenant header: the token is the capability.
	open := api.server.URL + view.URLs["mail"]
	var consumed struct {
		Consumed bool `json:"consumed"`
	}
	if code := doJSON(t, api.server.Client(), http.MethodGet, open, uuid.Nil, nil, &consumed); code != http.StatusOK || !consumed.Consumed {
		t.Fatalf("open url = %d %+v", code, consumed)
	}
	if code := doJSON(t, api.server.Client(), http.MethodGet, open, uuid.Nil, nil, nil); code != http.StatusGone {
		t.Fatalf("replayed url = %d, want 410", code)
	}
	// Any consumption consumes the notification: the other capability URL minted
	// with it is dead too (the token is real, so 410 rather than 404).
	if code := doJSON(t, api.server.Client(), http.MethodGet, api.server.URL+view.URLs["backup"], uuid.Nil, nil, nil); code != http.StatusGone {
		t.Fatalf("sibling url after consumption = %d, want 410", code)
	}

	code = doJSON(t, api.server.Client(), http.MethodPost,
		api.server.URL+"/v1/notifications/"+view.NotificationID+"/ack", api.tenant,
		map[string]any{"consumerId": "web"}, &acked)
	if code != http.StatusOK || acked.Notification.Status != notify.StatusConsumed || acked.Duplicate {
		t.Fatalf("ack = %d %+v", code, acked)
	}
}

func TestNotificationPixelAndExpiry(t *testing.T) {
	api := newNotifyAPI(t)
	view := api.push(t, notify.ConsumerSpec{ID: "pixel", Kind: notify.ConsumerImage})
	pixel := api.server.URL + view.URLs["pixel"]
	for i := 0; i < 2; i++ {
		var tracked struct {
			Tracked bool `json:"tracked"`
		}
		if code := doJSON(t, api.server.Client(), http.MethodGet, pixel, uuid.Nil, nil, &tracked); code != http.StatusOK || !tracked.Tracked {
			t.Fatalf("pixel fetch %d = %d %+v", i, code, tracked)
		}
	}

	expiring := api.push(t, notify.ConsumerSpec{ID: "late", Kind: notify.ConsumerURL})
	api.clock.Advance(31 * time.Minute)
	if code := doJSON(t, api.server.Client(), http.MethodGet, api.server.URL+expiring.URLs["late"], uuid.Nil, nil, nil); code != http.StatusGone {
		t.Fatalf("expired url = %d, want 410", code)
	}
}

func TestNotificationRoutesAreTenantScoped(t *testing.T) {
	api := newNotifyAPI(t)
	view := api.push(t, notify.ConsumerSpec{ID: "web", Kind: notify.ConsumerClient, Online: true})
	other := uuid.New()
	var list struct {
		Count int `json:"count"`
	}
	if code := doJSON(t, api.server.Client(), http.MethodGet,
		api.server.URL+"/v1/notifications", other, nil, &list); code != http.StatusOK || list.Count != 0 {
		t.Fatalf("foreign list = %d %+v", code, list)
	}
	if code := doJSON(t, api.server.Client(), http.MethodPost,
		api.server.URL+"/v1/notifications/"+view.NotificationID+"/ack", other,
		map[string]any{"consumerId": "web"}, nil); code != http.StatusNotFound {
		t.Fatalf("foreign ack = %d, want 404", code)
	}
}

// TestNotificationTokenRoutesDoNotLeakExistence pins the A4.2b2 token contract:
// a missing, wrong or unknown target answers 401 (never 404), so the capability
// routes cannot be used to probe whether a notification exists.
func TestNotificationTokenRoutesDoNotLeakExistence(t *testing.T) {
	api := newNotifyAPI(t)
	view := api.push(t,
		notify.ConsumerSpec{ID: "mail", Kind: notify.ConsumerURL},
		notify.ConsumerSpec{ID: "pixel", Kind: notify.ConsumerImage})
	validToken := tokenOf(t, view.URLs["mail"])

	probes := []struct {
		name   string
		method string
		path   string
		tenant uuid.UUID
	}{
		{"open without token", http.MethodGet, "/v1/notifications/" + view.NotificationID + "/open", uuid.Nil},
		{"open with wrong token", http.MethodGet, "/v1/notifications/" + view.NotificationID + "/open?token=not-the-token", uuid.Nil},
		{"open on a missing notification", http.MethodGet, "/v1/notifications/" + uuid.NewString() + "/open?token=not-the-token", uuid.Nil},
		{"open with a foreign tenant hint", http.MethodGet, "/v1/notifications/" + view.NotificationID + "/open?token=" + validToken, uuid.New()},
		{"pixel without token", http.MethodGet, "/v1/notifications/" + view.NotificationID + "/pixel", uuid.Nil},
		{"pixel with wrong token", http.MethodGet, "/v1/notifications/" + view.NotificationID + "/pixel?token=not-the-token", uuid.Nil},
	}
	for _, probe := range probes {
		var body map[string]any
		code := doJSON(t, api.server.Client(), probe.method, api.server.URL+probe.path, probe.tenant, nil, &body)
		if code != http.StatusUnauthorized {
			t.Fatalf("%s = %d, want 401; body=%v", probe.name, code, body)
		}
		if body["code"] != "invalid_token" {
			t.Fatalf("%s: code = %v, want invalid_token", probe.name, body["code"])
		}
	}

	// The real token still works, and a second notification's pixel token is
	// still accepted (the refused probes did not consume anything).
	var opened struct {
		Consumed bool `json:"consumed"`
	}
	if code := doJSON(t, api.server.Client(), http.MethodGet,
		api.server.URL+view.URLs["mail"], uuid.Nil, nil, &opened); code != http.StatusOK || !opened.Consumed {
		t.Fatalf("open with the real token = %d %+v", code, opened)
	}
	pixelView := api.push(t, notify.ConsumerSpec{ID: "pixel2", Kind: notify.ConsumerImage})
	var tracked struct {
		Tracked bool `json:"tracked"`
	}
	if code := doJSON(t, api.server.Client(), http.MethodGet,
		api.server.URL+pixelView.URLs["pixel2"], uuid.Nil, nil, &tracked); code != http.StatusOK || !tracked.Tracked {
		t.Fatalf("pixel with the real token = %d %+v", code, tracked)
	}
}

func TestNotificationRoutesWithoutServiceAre503(t *testing.T) {
	server := httptest.NewServer(New(Config{Logger: testLogger()}))
	defer server.Close()
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/notifications", uuid.New(), nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("list without service = %d, want 503", code)
	}
	if code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/notifications/"+uuid.NewString()+"/open?token=x", uuid.Nil, nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("open without service = %d, want 503", code)
	}
}

func jsonBody(t *testing.T, body any) *bytes.Reader {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(raw)
}
