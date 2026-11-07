package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/handoff"
	"github.com/jixoai/keel/internal/store"
)

const runtimeAdminKey = "runtime-control-test-key"

// newRuntimeFixture builds a server whose drain is mode-aware and whose
// runtime control is a real (audit-only) coordinator.
func newRuntimeFixture(t *testing.T) (*httptest.Server, *store.Store, *ModeDrainer, *handoff.Coordinator) {
	t.Helper()
	drain := NewModeDrainer(1500, 1000)
	coord, err := handoff.New(handoff.Config{
		Identity: handoff.Identity{ID: "runtime-test", LoadoutID: "keel-default", Revision: 3},
		Registry: handoff.NewMemRegistry(),
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	if err := coord.Register(context.Background()); err != nil {
		t.Fatalf("register runtime: %v", err)
	}
	if err := coord.Ready(context.Background()); err != nil {
		t.Fatalf("ready runtime: %v", err)
	}
	hs, st := newTestServer(t, func(c *Config) {
		c.Drain = drain
		c.Control = coord
		c.AdminKey = runtimeAdminKey
	})
	return hs, st, drain, coord
}

func adminRequest(t *testing.T, method, url string, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+runtimeAdminKey)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestRuntimeStatusRoute(t *testing.T) {
	hs, _, _, _ := newRuntimeFixture(t)
	defer hs.Close()

	// Without the bearer key the route is closed.
	res, err := http.Get(hs.URL + "/v1/runtime/status")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", res.StatusCode)
	}

	res, err = hs.Client().Do(adminRequest(t, "GET", hs.URL+"/v1/runtime/status", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["runtimeId"] != "runtime-test" || body["loadoutId"] != "keel-default" {
		t.Fatalf("status body = %v", body)
	}
	if body["draining"] != false || body["claiming"] != true {
		t.Fatalf("status body = %v", body)
	}
	if rev, ok := body["loadoutRevision"].(float64); !ok || rev != 3 {
		t.Fatalf("loadoutRevision = %v", body["loadoutRevision"])
	}
}

func TestRuntimeDrainShutdownRejectsInput(t *testing.T) {
	hs, st, _, _ := newRuntimeFixture(t)
	defer hs.Close()
	ctx := context.Background()
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "drain-shutdown"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}

	res, err := hs.Client().Do(adminRequest(t, "POST", hs.URL+"/v1/runtime/drain",
		`{"mode":"shutdown","deadlineMs":2000}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("drain status = %d", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["drainMode"] != "shutdown" || body["draining"] != true || body["claiming"] != false {
		t.Fatalf("drain body = %v", body)
	}

	// Input is refused in shutdown mode: the client retries against the new
	// runtime.
	input, _ := http.NewRequest("POST", hs.URL+"/v1/sessions/"+session.String()+"/input",
		strings.NewReader(`{"text":"during shutdown"}`))
	input.Header.Set("X-Tenant-ID", tenant.String())
	in, err := hs.Client().Do(input)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Body.Close()
	if in.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("shutdown input status = %d, want 503", in.StatusCode)
	}
	if in.Header.Get("Retry-After") == "" {
		t.Fatal("shutdown rejection is missing Retry-After")
	}
	// Probes stay up so the orchestrator can watch the drain.
	health, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health during shutdown drain = %d", health.StatusCode)
	}
}

func TestRuntimeDrainHandoffAdmitsEdgeWrites(t *testing.T) {
	hs, st, _, _ := newRuntimeFixture(t)
	defer hs.Close()
	ctx := context.Background()
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "drain-handoff"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}

	before, _ := http.NewRequest("POST", hs.URL+"/v1/sessions/"+session.String()+"/input",
		strings.NewReader(`{"text":"before","requestId":"req-1"}`))
	before.Header.Set("X-Tenant-ID", tenant.String())
	res, err := hs.Client().Do(before)
	if err != nil {
		t.Fatal(err)
	}
	var first map[string]any
	_ = json.NewDecoder(res.Body).Decode(&first)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("pre-drain input = %d", res.StatusCode)
	}
	firstSeq := first["seq"].(float64)

	// Handoff drain via the admin route.
	drainRes, err := hs.Client().Do(adminRequest(t, "POST", hs.URL+"/v1/runtime/drain",
		`{"mode":"handoff","deadlineMs":2000}`))
	if err != nil {
		t.Fatal(err)
	}
	var drainBody map[string]any
	_ = json.NewDecoder(drainRes.Body).Decode(&drainBody)
	drainRes.Body.Close()
	if drainRes.StatusCode != http.StatusOK {
		t.Fatalf("handoff drain = %d", drainRes.StatusCode)
	}
	if drainBody["drainMode"] != "handoff" || drainBody["claiming"] != false {
		t.Fatalf("handoff drain body = %v", drainBody)
	}

	// Edge input keeps landing: accepted, appended at the next seq.
	during, _ := http.NewRequest("POST", hs.URL+"/v1/sessions/"+session.String()+"/input",
		strings.NewReader(`{"text":"during handoff","requestId":"req-2"}`))
	during.Header.Set("X-Tenant-ID", tenant.String())
	in, err := hs.Client().Do(during)
	if err != nil {
		t.Fatal(err)
	}
	var duringBody map[string]any
	_ = json.NewDecoder(in.Body).Decode(&duringBody)
	in.Body.Close()
	if in.StatusCode != http.StatusCreated {
		t.Fatalf("handoff input status = %d, want 201 (the edge path stays open)", in.StatusCode)
	}
	if seq, _ := duringBody["seq"].(float64); seq != firstSeq+1 {
		t.Fatalf("input seq = %v, want %v", duringBody["seq"], firstSeq+1)
	}
	// The same request id is still idempotent.
	replay, _ := http.NewRequest("POST", hs.URL+"/v1/sessions/"+session.String()+"/input",
		strings.NewReader(`{"text":"during handoff","requestId":"req-2"}`))
	replay.Header.Set("X-Tenant-ID", tenant.String())
	rep, err := hs.Client().Do(replay)
	if err != nil {
		t.Fatal(err)
	}
	var replayBody map[string]any
	_ = json.NewDecoder(rep.Body).Decode(&replayBody)
	rep.Body.Close()
	if rep.StatusCode != http.StatusOK || replayBody["created"] != false {
		t.Fatalf("replay = %d %v, want a deduplicated 200", rep.StatusCode, replayBody)
	}
	if seq, _ := replayBody["seq"].(float64); seq != firstSeq+1 {
		t.Fatalf("replay seq = %v, want %v", replayBody["seq"], firstSeq+1)
	}

	// The event landed in the durable log.
	events, err := st.LoadEvents(ctx, tenant, session, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("committed events = %d, want 2 (nothing dropped during the drain)", len(events))
	}

	// Non-edge routes are refused during the handoff drain: reads and
	// "start executing" requests belong to the new runtime.
	runReq, _ := http.NewRequest("POST", hs.URL+"/v1/sessions/"+session.String()+"/run", nil)
	runReq.Header.Set("X-Tenant-ID", tenant.String())
	runRes, err := hs.Client().Do(runReq)
	if err != nil {
		t.Fatal(err)
	}
	runRes.Body.Close()
	if runRes.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("trigger run during handoff drain = %d, want 503", runRes.StatusCode)
	}
	readReq, _ := http.NewRequest("GET", hs.URL+"/v1/sessions/"+session.String()+"/events", nil)
	readReq.Header.Set("X-Tenant-ID", tenant.String())
	readRes, err := hs.Client().Do(readReq)
	if err != nil {
		t.Fatal(err)
	}
	readRes.Body.Close()
	if readRes.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("event read during handoff drain = %d, want 503", readRes.StatusCode)
	}
}

// TestSSEHandoffDrainFlushesAndKeepsEdgeInput drives the SSE path through a
// handoff drain: committed events reach the client, an input accepted during
// the drain is flushed before the drain frame, and the stream then closes with
// the reconnect hint (the client's cue to resume on the new runtime).
func TestSSEHandoffDrainFlushesAndKeepsEdgeInput(t *testing.T) {
	hs, st, drain, _ := newRuntimeFixture(t)
	defer hs.Close()
	hc := hs.Client()
	ctx := context.Background()
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "sse-handoff"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "one"}); err != nil {
		t.Fatal(err)
	}

	streamCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(streamCtx, "GET",
		hs.URL+"/v1/sessions/"+session.String()+"/events/stream?after=0", nil)
	req.Header.Set("X-Tenant-ID", tenant.String())
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("stream status = %d", res.StatusCode)
	}
	br := bufio.NewReader(res.Body)
	first := readSSEEvent(t, br)
	if first.id != "1" {
		t.Fatalf("first frame = %+v", first)
	}

	// A client input accepted by the old runtime mid-drain must reach the
	// stream before it closes.
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "two"}); err != nil {
		t.Fatal(err)
	}
	drain.BeginHandoff(2 * time.Second)

	var second sseEvent
	for {
		ev := readSSEEvent(t, br)
		if ev.event == "drain" {
			if second.id != "2" {
				t.Fatalf("drain arrived before the committed event: second=%+v drain=%+v", second, ev)
			}
			var payload struct {
				RetryAfterMS int `json:"retry_after_ms"`
			}
			if err := json.Unmarshal([]byte(ev.data), &payload); err != nil || payload.RetryAfterMS != 1500 {
				t.Fatalf("drain frame = %q (err %v)", ev.data, err)
			}
			break
		}
		if ev.event == "user.message" {
			second = ev
		}
	}
	// The stream ends after its rate-limited close slot: the client reconnects
	// to the new runtime with Last-Event-ID=2 and resumes at seq 3.
	if _, err := io.ReadAll(br); err != nil {
		t.Fatalf("read to EOF: %v", err)
	}
	if got := drain.DrainMode(); got != handoff.ModeHandoff {
		t.Fatalf("drain mode = %q", got)
	}
}

func TestRuntimeDrainValidation(t *testing.T) {
	hs, _, _, _ := newRuntimeFixture(t)
	defer hs.Close()

	// Unknown mode.
	res, err := hs.Client().Do(adminRequest(t, "POST", hs.URL+"/v1/runtime/drain", `{"mode":"banana"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown mode = %d (%s)", res.StatusCode, body)
	}

	// First drain wins; a second is a conflict.
	first, err := hs.Client().Do(adminRequest(t, "POST", hs.URL+"/v1/runtime/drain", `{"mode":"handoff","deadlineMs":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first drain = %d", first.StatusCode)
	}
	second, err := hs.Client().Do(adminRequest(t, "POST", hs.URL+"/v1/runtime/drain", `{"mode":"shutdown","deadlineMs":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second drain = %d, want 409", second.StatusCode)
	}
}

// TestRuntimeControlUnconfigured answers 503 instead of pretending.
func TestRuntimeControlUnconfigured(t *testing.T) {
	hs := httptest.NewServer(New(Config{AdminKey: runtimeAdminKey, Logger: discardLogger()}))
	defer hs.Close()
	res, err := http.Get(hs.URL + "/v1/runtime/status")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", res.StatusCode)
	}
	authed, err := hs.Client().Do(adminRequest(t, "GET", hs.URL+"/v1/runtime/status", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer authed.Body.Close()
	if authed.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status = %d, want 503", authed.StatusCode)
	}
}

// TestEdgeWritePath pins the admission set: only client-input endpoints pass a
// handoff drain.
func TestEdgeWritePath(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/sessions/11111111-1111-1111-1111-111111111111/input", true},
		{"POST", "/v1/sessions/11111111-1111-1111-1111-111111111111/events", true},
		{"GET", "/v1/sessions/11111111-1111-1111-1111-111111111111/events", false},
		{"POST", "/v1/sessions/11111111-1111-1111-1111-111111111111/run", false},
		{"POST", "/v1/sessions", false},
		{"POST", "/v1/files", false},
		{"GET", "/v1/sessions/11111111-1111-1111-1111-111111111111/events/stream", false},
	}
	for _, tc := range cases {
		if got := edgeWritePath(tc.method, tc.path); got != tc.want {
			t.Errorf("edgeWritePath(%s %s) = %t, want %t", tc.method, tc.path, got, tc.want)
		}
	}
}
