package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/externalgw"
	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

type fakeExternal struct {
	mu       sync.Mutex
	calls    []string
	openReq  gateway.SessionOpenRequest
	backend  string
	message  json.RawMessage
	request  string
	cancel   externalgw.CancelRequest
	reason   string
	view     externalgw.SessionView
	sessions []externalgw.SessionView
	backends []externalgw.BackendStatus
	err      error
}

func (f *fakeExternal) record(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
}

func (f *fakeExternal) currentErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeExternal) Open(_ context.Context, scope kernel.Scope, backend string, request gateway.SessionOpenRequest) (externalgw.SessionView, error) {
	f.record("open")
	f.mu.Lock()
	f.backend, f.openReq = backend, request
	view := f.view
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return externalgw.SessionView{}, err
	}
	view.SessionID = scope.SessionID.String()
	return view, nil
}

func (f *fakeExternal) Get(context.Context, kernel.Scope) (externalgw.SessionView, error) {
	f.record("get")
	return f.view, f.currentErr()
}

func (f *fakeExternal) List(context.Context, uuid.UUID) ([]externalgw.SessionView, error) {
	f.record("list")
	return f.sessions, f.currentErr()
}

func (f *fakeExternal) Send(_ context.Context, _ kernel.Scope, message json.RawMessage, requestID string) (externalgw.SendResult, error) {
	f.record("send")
	f.mu.Lock()
	f.message, f.request = message, requestID
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return externalgw.SendResult{}, err
	}
	return externalgw.SendResult{TurnID: "turn-1", Recorded: true}, nil
}

func (f *fakeExternal) Cancel(_ context.Context, _ kernel.Scope, request externalgw.CancelRequest) error {
	f.record("cancel")
	f.mu.Lock()
	f.cancel = request
	err := f.err
	f.mu.Unlock()
	return err
}

func (f *fakeExternal) Close(_ context.Context, _ kernel.Scope, reason string) error {
	f.record("close")
	f.mu.Lock()
	f.reason = reason
	err := f.err
	f.mu.Unlock()
	return err
}

func (f *fakeExternal) Backends() []externalgw.BackendStatus {
	f.record("backends")
	return f.backends
}

func externalTestServer(t *testing.T, fake *fakeExternal) *httptest.Server {
	t.Helper()
	cfg := Config{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBody: 1 << 20,
	}
	if fake != nil {
		cfg.External = fake
	}
	hs := httptest.NewServer(New(cfg))
	t.Cleanup(hs.Close)
	return hs
}

func TestExternalOpenAndQuery(t *testing.T) {
	fake := &fakeExternal{view: externalgw.SessionView{
		StateOwner:     kernel.StateOwnerExternal,
		Backend:        "dsh",
		ExternalRef:    "ref-1",
		Status:         kernel.ExternalStatusActive,
		Metering:       kernel.MeteringExternal,
		Compatible:     true,
		NativeRecovery: false,
	}}
	hs := externalTestServer(t, fake)
	tenant := uuid.NewString()
	var view map[string]any
	code := doJSON(t, hs.Client(), "POST", hs.URL+"/v1/external/sessions", tenant,
		map[string]any{"backend": "dsh", "reference": "cli"}, &view)
	if code != http.StatusCreated {
		t.Fatalf("open = %d %v", code, view)
	}
	for key, want := range map[string]any{
		"stateOwner":      kernel.StateOwnerExternal,
		"externalBackend": "dsh",
		"externalRef":     "ref-1",
		"externalStatus":  kernel.ExternalStatusActive,
		"metering":        kernel.MeteringExternal,
		"compatible":      true,
		"nativeRecovery":  false,
	} {
		if view[key] != want {
			t.Fatalf("open body[%s] = %v, want %v (body %v)", key, view[key], want, view)
		}
	}
	if fake.backend != "dsh" || fake.openReq.Reference != "cli" {
		t.Fatalf("service open args = %q %+v", fake.backend, fake.openReq)
	}
	sessionID := view["sessionId"].(string)
	if code := doJSON(t, hs.Client(), "GET", hs.URL+"/v1/external/sessions/"+sessionID, tenant, nil, &view); code != http.StatusOK {
		t.Fatalf("get = %d", code)
	}
	// Tenant header is mandatory, like every session endpoint.
	var out map[string]any
	if code := doJSON(t, hs.Client(), "POST", hs.URL+"/v1/external/sessions", "", map[string]any{"backend": "dsh"}, &out); code != http.StatusUnauthorized {
		t.Fatalf("open without tenant = %d", code)
	}
	if code := doJSON(t, hs.Client(), "POST", hs.URL+"/v1/external/sessions", tenant, map[string]any{}, &out); code != http.StatusBadRequest {
		t.Fatalf("open without backend = %d", code)
	}
}

func TestExternalSendCancelCloseAndBackends(t *testing.T) {
	fake := &fakeExternal{
		view:     externalgw.SessionView{StateOwner: kernel.StateOwnerExternal, Status: kernel.ExternalStatusActive},
		backends: []externalgw.BackendStatus{{Name: "dsh", State: "ready", Health: "ok", Sessions: 1}},
	}
	hs := externalTestServer(t, fake)
	tenant := uuid.NewString()
	session := uuid.NewString()
	base := hs.URL + "/v1/external/sessions/" + session

	var send map[string]any
	if code := doJSON(t, hs.Client(), "POST", base+"/messages", tenant, map[string]any{"text": "hi", "requestId": "r1"}, &send); code != http.StatusAccepted {
		t.Fatalf("send = %d %v", code, send)
	}
	if string(fake.message) != `{"text":"hi"}` {
		t.Fatalf("service message = %s", fake.message)
	}
	if fake.request != "r1" {
		t.Fatalf("service request id = %q", fake.request)
	}
	if send["turnId"] != "turn-1" || send["recorded"] != true {
		t.Fatalf("send body = %v", send)
	}
	var canceled map[string]any
	if code := doJSON(t, hs.Client(), "POST", base+"/cancel", tenant, map[string]any{"turnId": "t-1", "reason": "stop"}, &canceled); code != http.StatusOK {
		t.Fatalf("cancel = %d", code)
	}
	if fake.cancel.TurnID != "t-1" || fake.cancel.Reason != "stop" {
		t.Fatalf("cancel args = %+v", fake.cancel)
	}
	var closed map[string]any
	if code := doJSON(t, hs.Client(), "POST", base+"/close", tenant, map[string]any{"reason": "done"}, &closed); code != http.StatusOK {
		t.Fatalf("close = %d", code)
	}
	if fake.reason != "done" {
		t.Fatalf("close reason = %q", fake.reason)
	}
	var backends struct {
		Backends []externalgw.BackendStatus `json:"backends"`
	}
	if code := doJSON(t, hs.Client(), "GET", hs.URL+"/v1/external/backends", tenant, nil, &backends); code != http.StatusOK || len(backends.Backends) != 1 {
		t.Fatalf("backends = %d %+v", code, backends)
	}
}

func TestExternalErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"not found", kernel.ErrNotFound, http.StatusNotFound},
		{"backend unknown", externalgw.ErrBackendUnknown, http.StatusBadRequest},
		{"backend unavailable", externalgw.ErrBackendUnavailable, http.StatusServiceUnavailable},
		{"unmetered denied", externalgw.ErrUnmeteredDenied, http.StatusForbidden},
		{"session limit", externalgw.ErrSessionLimit, http.StatusTooManyRequests},
		{"closed", externalgw.ErrSessionClosed, http.StatusConflict},
		{"already external", externalgw.ErrAlreadyExternal, http.StatusConflict},
		{"timeout", &gateway.TimeoutError{Method: "session.open", Timeout: time.Second}, http.StatusGatewayTimeout},
		{"rpc", &gateway.RPCError{Code: -32000, Message: "boom"}, http.StatusBadGateway},
		{"internal", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeExternal{err: tc.err}
			hs := externalTestServer(t, fake)
			tenant := uuid.NewString()
			var out map[string]any
			code := doJSON(t, hs.Client(), "GET", hs.URL+"/v1/external/sessions/"+uuid.NewString(), tenant, nil, &out)
			if code != tc.status {
				t.Fatalf("get = %d %v, want %d", code, out, tc.status)
			}
		})
	}
}

func TestExternalRoutesUnconfigured(t *testing.T) {
	hs := externalTestServer(t, nil)
	var out map[string]any
	if code := doJSON(t, hs.Client(), "GET", hs.URL+"/v1/external/backends", uuid.NewString(), nil, &out); code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured external gateway = %d", code)
	}
}

// TestExternalSessionRejectsNativeEdgeWrites is the security gate: an external
// session refuses the native /input and /events with 409 and lands zero events,
// while a native session still accepts the same /events write.
func TestExternalSessionRejectsNativeEdgeWrites(t *testing.T) {
	hs, st := newTestServer(t)
	hc := hs.Client()
	ctx := context.Background()
	tenant := uuid.NewString()
	if code := createTestTenant(t, hc, hs.URL, tenant, "external-edge"); code != 201 {
		t.Fatalf("create tenant: %d", code)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant, map[string]string{}, &sess); code != 201 {
		t.Fatalf("create session: %d", code)
	}
	tenantID := uuid.MustParse(tenant)
	sessionID := uuid.MustParse(sess.SessionID)
	if _, err := st.CreateExternalSession(ctx, tenantID, sessionID, store.ExternalMarks{Backend: "dsh"}); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+sess.SessionID+"/input", tenant,
		map[string]string{"text": "hi", "requestId": "ext-input"}, &out); code != http.StatusConflict {
		t.Fatalf("external /input = %d %v, want 409", code, out)
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+sess.SessionID+"/events", tenant,
		map[string]any{"kind": "user.message", "payload": map[string]string{"text": "hi", "requestId": "ext-event"}}, &out); code != http.StatusConflict {
		t.Fatalf("external /events = %d %v, want 409", code, out)
	}
	var events struct {
		Events []store.Event `json:"events"`
	}
	if code := doJSON(t, hc, "GET", hs.URL+"/v1/sessions/"+sess.SessionID+"/events", tenant, nil, &events); code != 200 {
		t.Fatalf("list events: %d", code)
	}
	if len(events.Events) != 0 {
		t.Fatalf("refused native edge writes landed events: %+v", events.Events)
	}
	// A native session is unaffected by the NativeEdge flag.
	var native struct {
		SessionID string `json:"sessionId"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant, map[string]string{}, &native); code != 201 {
		t.Fatalf("create native session: %d", code)
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+native.SessionID+"/events", tenant,
		map[string]any{"kind": "user.message", "payload": map[string]string{"text": "hi", "requestId": "native-event"}}, &out); code != 201 {
		t.Fatalf("native /events = %d %v, want 201", code, out)
	}
}
