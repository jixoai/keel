package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
)

func newTestServer(t *testing.T, opts ...func(*Config)) (*httptest.Server, *store.Store) {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	// The production edge commits through the contract kernel; the tests wire
	// the same seam so the endpoint behavior under test is the shipped one.
	kernelLog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(kernelLog, kernel.Options{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub(st, log)
	hubCtx, cancel := context.WithCancel(ctx)
	go hub.Run(hubCtx)
	t.Cleanup(func() { cancel(); st.Close() })

	cfg := Config{
		Store:     st,
		Hub:       hub,
		Edge:      kern,
		Events:    kern,
		Blobs:     blob.NewReader(st, 16, 1),
		Uploader:  &blob.Uploader{Store: st, CASDir: t.TempDir()},
		Scheduler: stream.NewScheduler(100),
		Admitter:  stream.NewAdmitter(1000, 2000),
		Limiter:   nil,
		Metrics:   &stream.Metrics{},
		Logger:    log,
		// Provisioning is an admin action; tests authenticate like operators.
		AdminKey: testAdminKey,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	srv := New(cfg)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return hs, st
}

func doJSON(t *testing.T, hc *http.Client, method, url, tenant string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return res.StatusCode
}

// createTestTenant provisions a tenant through POST /v1/tenants, which is
// guarded by the operator key.
func createTestTenant(t *testing.T, hc *http.Client, base, tenant, name string) int {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", base+"/v1/tenants", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAdminKey)
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

func TestSessionIdempotencyAndEvents(t *testing.T) {
	hs, _ := newTestServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	if code := createTestTenant(t, hc, hs.URL, tenant, "t"); code != 201 {
		t.Fatalf("create tenant: %d", code)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant, map[string]string{}, &sess); code != 201 {
		t.Fatalf("create session: %d", code)
	}

	requestID := "req-1"
	var first, second struct {
		SubmissionID string `json:"submissionId"`
		Created      bool   `json:"created"`
		Seq          *int64 `json:"seq"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+sess.SessionID+"/input", tenant,
		map[string]string{"text": "hello", "requestId": requestID}, &first); code != 201 {
		t.Fatalf("first input: %d", code)
	}
	// Replaying the same request (same text) is deduplication.
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+sess.SessionID+"/input", tenant,
		map[string]string{"text": "hello", "requestId": requestID}, &second); code != 200 {
		t.Fatalf("replay input: %d", code)
	}
	if !first.Created || second.Created {
		t.Fatalf("created flags: %v %v", first.Created, second.Created)
	}
	if first.SubmissionID != second.SubmissionID {
		t.Fatalf("submission ids differ: %s %s", first.SubmissionID, second.SubmissionID)
	}
	// The same key with different content is a client bug, not a retry: the
	// kernel answers 409 instead of silently dropping the new text.
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+sess.SessionID+"/input", tenant,
		map[string]string{"text": "hello again", "requestId": requestID}, nil); code != 409 {
		t.Fatalf("conflicting replay: %d, want 409", code)
	}

	var events struct {
		Events []store.Event `json:"events"`
	}
	if code := doJSON(t, hc, "GET", hs.URL+"/v1/sessions/"+sess.SessionID+"/events", tenant, nil, &events); code != 200 {
		t.Fatalf("list events: %d", code)
	}
	if len(events.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events.Events))
	}
	if events.Events[0].Kind != "user.message" {
		t.Fatalf("kind = %s", events.Events[0].Kind)
	}
}

func TestFileUploadAndStreams(t *testing.T) {
	hs, _ := newTestServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	_ = createTestTenant(t, hc, hs.URL, tenant, "f")

	data := make([]byte, 700000)
	for i := range data {
		data[i] = byte(i * 13)
	}
	req, _ := http.NewRequest("POST", hs.URL+"/v1/files", bytes.NewReader(data))
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Content-Type", "image/png")
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 201 {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("upload: %d %s", res.StatusCode, raw)
	}
	var meta struct {
		FileID string `json:"fileId"`
		Size   int64  `json:"size"`
	}
	if err := json.NewDecoder(res.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if meta.Size != int64(len(data)) {
		t.Fatalf("size = %d", meta.Size)
	}

	// base64 stream equals the canonical encoding.
	req, _ = http.NewRequest("GET", hs.URL+"/v1/files/"+meta.FileID+"?format=base64", nil)
	req.Header.Set("X-Tenant-ID", tenant)
	res, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if want := base64.StdEncoding.EncodeToString(data); string(got) != want {
		t.Fatalf("base64 mismatch: %d vs %d bytes", len(got), len(want))
	}

	// A wire-offset resume continues the same sequence.
	off := int64(4 * 1000)
	req, _ = http.NewRequest("GET", hs.URL+"/v1/files/"+meta.FileID+"?format=base64&offset="+fmt.Sprint(off), nil)
	req.Header.Set("X-Tenant-ID", tenant)
	res, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 206 {
		t.Fatalf("resume status = %d", res.StatusCode)
	}
	if string(got) != base64.StdEncoding.EncodeToString(data)[off:] {
		t.Fatal("resume mismatch")
	}

	// Raw Range.
	req, _ = http.NewRequest("GET", hs.URL+"/v1/files/"+meta.FileID, nil)
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Range", "bytes=65534-65540")
	res, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 206 || !bytes.Equal(got, data[65534:65541]) {
		t.Fatalf("range status=%d len=%d", res.StatusCode, len(got))
	}
}

func TestSSEResumeWithLastEventID(t *testing.T) {
	hs, _ := newTestServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	_ = createTestTenant(t, hc, hs.URL, tenant, "s")
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	_ = doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant, map[string]string{}, &sess)

	// Seed two events, then connect from after=1 and expect only seq 2.
	for i := 0; i < 2; i++ {
		_ = doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+sess.SessionID+"/input", tenant,
			map[string]string{"text": fmt.Sprintf("m%d", i), "requestId": fmt.Sprintf("r%d", i)}, nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", hs.URL+"/v1/sessions/"+sess.SessionID+"/events/stream?after=1", nil)
	req.Header.Set("X-Tenant-ID", tenant)
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	var ids []string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			ids = append(ids, strings.TrimPrefix(line, "id: "))
			break // one frame is enough: the cursor proved itself
		}
	}
	if len(ids) != 1 || ids[0] != "2" {
		t.Fatalf("first event id = %v, want [2]", ids)
	}
}

// TestEdgeEventsEndpointTightening is the change's MODIFIED API delta: the edge
// accepts only user.message (any other kind is 400) and answers 404 — not an
// empty list — for a foreign or missing session.
func TestEdgeEventsEndpointTightening(t *testing.T) {
	hs, _ := newTestServer(t)
	hc := hs.Client()
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	if code := createTestTenant(t, hc, hs.URL, tenantA, "a"); code != 201 {
		t.Fatalf("create tenant A: %d", code)
	}
	if code := createTestTenant(t, hc, hs.URL, tenantB, "b"); code != 201 {
		t.Fatalf("create tenant B: %d", code)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenantA, map[string]string{}, &sess); code != 201 {
		t.Fatalf("create session: %d", code)
	}
	eventsURL := hs.URL + "/v1/sessions/" + sess.SessionID + "/events"

	// Every non-edge kind is rejected: the whitelist is exactly user.message.
	for _, kind := range []string{"assistant.message", "tool.result", "compaction", "run.started", "run.failed", "not-a-kind"} {
		var out map[string]any
		code := doJSON(t, hc, "POST", eventsURL, tenantA, map[string]any{"kind": kind, "payload": map[string]any{}}, &out)
		if code != 400 {
			t.Fatalf("POST kind %s = %d, want 400", kind, code)
		}
	}

	// user.message is accepted; the payload requestId is the idempotency key.
	body := map[string]any{"kind": "user.message", "payload": map[string]any{"text": "hi", "requestId": "edge-1"}}
	var first struct {
		Seq     int64 `json:"seq"`
		Deduped bool  `json:"deduped"`
	}
	if code := doJSON(t, hc, "POST", eventsURL, tenantA, body, &first); code != 201 {
		t.Fatalf("POST user.message = %d, want 201", code)
	}
	if first.Seq != 1 || first.Deduped {
		t.Fatalf("first commit = %+v, want seq 1 and no dedupe", first)
	}
	var replay struct {
		Seq     int64 `json:"seq"`
		Deduped bool  `json:"deduped"`
	}
	if code := doJSON(t, hc, "POST", eventsURL, tenantA, body, &replay); code != 201 {
		t.Fatalf("replay = %d, want 201", code)
	}
	if !replay.Deduped || replay.Seq != first.Seq {
		t.Fatalf("replay = %+v, want deduped at seq %d", replay, first.Seq)
	}

	// Cross-tenant write and read answer 404 without leaking existence.
	var out map[string]any
	if code := doJSON(t, hc, "POST", eventsURL, tenantB, body, &out); code != 404 {
		t.Fatalf("cross-tenant POST = %d, want 404", code)
	}
	if code := doJSON(t, hc, "GET", eventsURL, tenantB, nil, &out); code != 404 {
		t.Fatalf("cross-tenant GET = %d, want 404", code)
	}
	// A missing session is the same 404.
	missingURL := hs.URL + "/v1/sessions/" + uuid.NewString() + "/events"
	if code := doJSON(t, hc, "GET", missingURL, tenantA, nil, &out); code != 404 {
		t.Fatalf("missing session GET = %d, want 404", code)
	}
	if code := doJSON(t, hc, "POST", missingURL, tenantA, body, &out); code != 404 {
		t.Fatalf("missing session POST = %d, want 404", code)
	}

	// The rejections wrote nothing and the replay did not duplicate the fact.
	var events struct {
		Events []store.Event `json:"events"`
	}
	if code := doJSON(t, hc, "GET", eventsURL, tenantA, nil, &events); code != 200 {
		t.Fatalf("list events: %d", code)
	}
	if len(events.Events) != 1 || events.Events[0].Kind != "user.message" {
		t.Fatalf("events = %+v, want exactly one user.message", events.Events)
	}
}

// TestInputTenantBoundary checks the /input endpoint's boundary: a foreign or
// missing session is a 404 and no submission bookkeeping is left behind.
func TestInputTenantBoundary(t *testing.T) {
	hs, _ := newTestServer(t)
	hc := hs.Client()
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	_ = createTestTenant(t, hc, hs.URL, tenantA, "a")
	_ = createTestTenant(t, hc, hs.URL, tenantB, "b")
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenantA, map[string]string{}, &sess); code != 201 {
		t.Fatalf("create session: %d", code)
	}
	var out map[string]any
	body := map[string]string{"text": "hi", "requestId": "boundary-1"}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+sess.SessionID+"/input", tenantB, body, &out); code != 404 {
		t.Fatalf("cross-tenant input = %d, want 404", code)
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+uuid.NewString()+"/input", tenantA, body, &out); code != 404 {
		t.Fatalf("missing session input = %d, want 404", code)
	}
	// The owning tenant still sees an empty log.
	var events struct {
		Events []store.Event `json:"events"`
	}
	if code := doJSON(t, hc, "GET", hs.URL+"/v1/sessions/"+sess.SessionID+"/events", tenantA, nil, &events); code != 200 {
		t.Fatalf("list events: %d", code)
	}
	if len(events.Events) != 0 {
		t.Fatalf("events = %+v, want none", events.Events)
	}
}

// TestEdgeWriteEndpointsRequireKernel pins the explicit failure mode: without
// the kernel seam the edge write endpoints answer 503 instead of silently
// falling back to the legacy store writer.
func TestEdgeWriteEndpointsRequireKernel(t *testing.T) {
	hs := httptest.NewServer(New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	defer hs.Close()
	session := uuid.NewString()
	tenant := uuid.NewString()
	for _, tc := range []struct {
		method string
		url    string
		body   any
	}{
		{"POST", hs.URL + "/v1/sessions/" + session + "/events", map[string]any{"kind": "user.message", "payload": map[string]any{"text": "x"}}},
		{"POST", hs.URL + "/v1/sessions/" + session + "/input", map[string]string{"text": "x"}},
		{"GET", hs.URL + "/v1/sessions/" + session + "/events", nil},
		// The stream shares the read seam: without it there is no way to tell a
		// missing session from an empty one, so it fails closed like /events.
		{"GET", hs.URL + "/v1/sessions/" + session + "/events/stream", nil},
	} {
		var out map[string]any
		if code := doJSON(t, http.DefaultClient, tc.method, tc.url, tenant, tc.body, &out); code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s = %d, want 503", tc.method, tc.url, code)
		}
	}
}

// TestInputRequestIDIsScopedToSession is the M3 negative case: a requestId
// reused by another session must not return the first session's submission or
// seq; the second session gets its own row at its own next seq. A replay in
// the same session stays idempotent.
func TestInputRequestIDIsScopedToSession(t *testing.T) {
	hs, _ := newTestServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	if code := createTestTenant(t, hc, hs.URL, tenant, "m3"); code != 201 {
		t.Fatalf("create tenant: %d", code)
	}
	createSession := func() string {
		var sess struct {
			SessionID string `json:"sessionId"`
		}
		if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenant, map[string]string{}, &sess); code != 201 {
			t.Fatalf("create session: %d", code)
		}
		return sess.SessionID
	}
	sessionA, sessionB := createSession(), createSession()
	type receipt struct {
		SubmissionID string `json:"submissionId"`
		Created      bool   `json:"created"`
		Seq          *int64 `json:"seq"`
	}
	input := func(session, requestID, text string) (int, receipt) {
		var out receipt
		code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+session+"/input", tenant,
			map[string]string{"text": text, "requestId": requestID}, &out)
		return code, out
	}
	code, firstA := input(sessionA, "dup-1", "a")
	if code != 201 || !firstA.Created || firstA.Seq == nil || *firstA.Seq != 1 {
		t.Fatalf("A dup-1 = %d %+v, want a created seq 1", code, firstA)
	}
	code, firstB := input(sessionB, "b-1", "b")
	if code != 201 || !firstB.Created || firstB.Seq == nil || *firstB.Seq != 1 {
		t.Fatalf("B b-1 = %d %+v, want a created seq 1", code, firstB)
	}

	// The audit's reproduction: B reuses A's requestId. B must get its own
	// submission at B's next seq, never A's seq/submissionId.
	code, dupB := input(sessionB, "dup-1", "b again")
	if code != 201 || !dupB.Created {
		t.Fatalf("B dup-1 = %d %+v, want a new submission", code, dupB)
	}
	if dupB.Seq == nil || *dupB.Seq != 2 {
		t.Fatalf("B dup-1 seq = %v, want B's own seq 2", dupB.Seq)
	}
	if dupB.SubmissionID == firstA.SubmissionID {
		t.Fatalf("B received session A's submission %s", dupB.SubmissionID)
	}

	// Same-session replay of the same request remains idempotent.
	code, replayB := input(sessionB, "dup-1", "b again")
	if code != 200 || replayB.Created || replayB.SubmissionID != dupB.SubmissionID ||
		replayB.Seq == nil || *replayB.Seq != 2 {
		t.Fatalf("B replay = %d %+v, want the stored submission at seq 2", code, replayB)
	}
	// The same requestId with different content is a conflict, not a replay:
	// answering with the stored receipt would silently drop the new text.
	if code, _ := input(sessionB, "dup-1", "b replay"); code != 409 {
		t.Fatalf("conflicting replay = %d, want 409", code)
	}

	countUser := func(session string) int {
		var events struct {
			Events []store.Event `json:"events"`
		}
		if code := doJSON(t, hc, "GET", hs.URL+"/v1/sessions/"+session+"/events", tenant, nil, &events); code != 200 {
			t.Fatalf("list %s events: %d", session, code)
		}
		n := 0
		for _, event := range events.Events {
			if event.Kind == "user.message" {
				n++
			}
		}
		return n
	}
	if got := countUser(sessionA); got != 1 {
		t.Errorf("session A user events = %d, want 1", got)
	}
	if got := countUser(sessionB); got != 2 {
		t.Errorf("session B user events = %d, want 2 (b-1 and its own dup-1)", got)
	}
}

// TestSSEEventsTenantBoundary pins the stream to the same boundary as GET
// /events: a foreign or missing session is a 404 before the SSE response
// starts, not a 200 with an empty stream.
func TestSSEEventsTenantBoundary(t *testing.T) {
	hs, _ := newTestServer(t)
	hc := hs.Client()
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	if code := createTestTenant(t, hc, hs.URL, tenantA, "sse-a"); code != 201 {
		t.Fatalf("create tenant A: %d", code)
	}
	if code := createTestTenant(t, hc, hs.URL, tenantB, "sse-b"); code != 201 {
		t.Fatalf("create tenant B: %d", code)
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", tenantA, map[string]string{}, &sess); code != 201 {
		t.Fatalf("create session: %d", code)
	}
	stream := hs.URL + "/v1/sessions/" + sess.SessionID + "/events/stream"
	get := func(target, tenant string) *http.Response {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
		req.Header.Set("X-Tenant-ID", tenant)
		res, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	cross := get(stream, tenantB)
	body, _ := io.ReadAll(cross.Body)
	cross.Body.Close()
	if cross.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "session not found") {
		t.Fatalf("cross-tenant stream = %d %s, want 404 session not found", cross.StatusCode, body)
	}
	missing := get(hs.URL+"/v1/sessions/"+uuid.NewString()+"/events/stream", tenantA)
	_, _ = io.Copy(io.Discard, missing.Body)
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing-session stream = %d, want 404", missing.StatusCode)
	}
	own := get(stream, tenantA)
	own.Body.Close()
	if own.StatusCode != http.StatusOK {
		t.Fatalf("own-session stream = %d, want 200", own.StatusCode)
	}
}

// TestTriggerRunRequiresExistingSession: POST /run for a missing or foreign
// session is 404 and creates no run row (an orphan row would never be claimed
// and would inflate the queue depth forever).
func TestTriggerRunRequiresExistingSession(t *testing.T) {
	hs, st := newTestServer(t)
	hc := hs.Client()
	tenant, foreignTenant := uuid.NewString(), uuid.NewString()
	if code := createTestTenant(t, hc, hs.URL, tenant, "run-a"); code != 201 {
		t.Fatalf("create tenant: %d", code)
	}
	if code := createTestTenant(t, hc, hs.URL, foreignTenant, "run-b"); code != 201 {
		t.Fatalf("create foreign tenant: %d", code)
	}
	createSession := func(owner string) uuid.UUID {
		var sess struct {
			SessionID string `json:"sessionId"`
		}
		if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions", owner, map[string]string{}, &sess); code != 201 {
			t.Fatalf("create session: %d", code)
		}
		parsed, err := uuid.Parse(sess.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	session := createSession(tenant)
	foreignSession := createSession(foreignTenant)
	tenantID, foreignID := uuid.MustParse(tenant), uuid.MustParse(foreignTenant)
	ctx := context.Background()

	missing := uuid.New()
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+missing.String()+"/run", tenant, map[string]any{}, nil); code != http.StatusNotFound {
		t.Fatalf("missing-session run = %d, want 404", code)
	}
	if _, err := st.GetRun(ctx, tenantID, missing); !errors.Is(err, store.ErrNoRun) {
		t.Fatalf("missing-session run created a row: %v", err)
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+foreignSession.String()+"/run", tenant, map[string]any{}, nil); code != http.StatusNotFound {
		t.Fatalf("foreign-session run = %d, want 404", code)
	}
	if _, err := st.GetRun(ctx, tenantID, foreignSession); !errors.Is(err, store.ErrNoRun) {
		t.Fatalf("foreign-session run created a row for the caller: %v", err)
	}
	if _, err := st.GetRun(ctx, foreignID, foreignSession); !errors.Is(err, store.ErrNoRun) {
		t.Fatalf("foreign-session run created a row for the owner: %v", err)
	}
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+session.String()+"/run", tenant, map[string]any{}, nil); code != http.StatusAccepted {
		t.Fatalf("own-session run = %d, want 202", code)
	}
	if row, err := st.GetRun(ctx, tenantID, session); err != nil || row.Status != "pending" {
		t.Fatalf("enqueued run = %+v, %v", row, err)
	}
}

// TestExternalSessionNativeEndpointsRejected is the M4 negative case: an
// external session rejects /input and /run with 409 (never a post-commit 500),
// and neither endpoint lands an event, a submission or a run row.
func TestExternalSessionNativeEndpointsRejected(t *testing.T) {
	hs, st := newTestServer(t)
	hc := hs.Client()
	ctx := context.Background()
	tenant := uuid.NewString()
	if code := createTestTenant(t, hc, hs.URL, tenant, "external"); code != 201 {
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

	for attempt := 0; attempt < 2; attempt++ {
		var out map[string]any
		code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+sess.SessionID+"/input", tenant,
			map[string]string{"text": "hi", "requestId": "ext-1"}, &out)
		if code != http.StatusConflict {
			t.Fatalf("external input attempt %d = %d %v, want 409", attempt, code, out)
		}
	}
	// The native event endpoint takes the same NativeEdge path.
	var out map[string]any
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+sess.SessionID+"/events", tenant,
		map[string]any{"kind": "user.message", "payload": map[string]any{"text": "hi", "requestId": "ext-2"}}, &out); code != http.StatusConflict {
		t.Fatalf("external events = %d %v, want 409", code, out)
	}
	var events struct {
		Events []store.Event `json:"events"`
	}
	if code := doJSON(t, hc, "GET", hs.URL+"/v1/sessions/"+sess.SessionID+"/events", tenant, nil, &events); code != 200 {
		t.Fatalf("list events: %d", code)
	}
	if len(events.Events) != 0 {
		t.Fatalf("external session log = %+v, want none", events.Events)
	}
	if subs, err := st.ListSubmissions(ctx, tenantID, sessionID); err != nil || len(subs) != 0 {
		t.Fatalf("external session submissions = %+v, %v; want none", subs, err)
	}

	var runOut map[string]any
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+sess.SessionID+"/run", tenant, map[string]any{}, &runOut); code != http.StatusConflict {
		t.Fatalf("external run = %d %v, want 409", code, runOut)
	}
	if _, err := st.GetRun(ctx, tenantID, sessionID); !errors.Is(err, store.ErrNoRun) {
		t.Fatalf("external session got a run row: %v", err)
	}
}
