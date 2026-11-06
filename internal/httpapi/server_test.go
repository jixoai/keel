package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub(st, log)
	hubCtx, cancel := context.WithCancel(ctx)
	go hub.Run(hubCtx)
	t.Cleanup(func() { cancel(); st.Close() })

	srv := New(Config{
		Store:     st,
		Hub:       hub,
		Blobs:     blob.NewReader(st, 16, 1),
		Uploader:  &blob.Uploader{Store: st, CASDir: t.TempDir()},
		Scheduler: stream.NewScheduler(100),
		Admitter:  stream.NewAdmitter(1000, 2000),
		Limiter:   nil,
		Metrics:   &stream.Metrics{},
		Logger:    log,
	})
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

func TestSessionIdempotencyAndEvents(t *testing.T) {
	hs, _ := newTestServer(t)
	hc := hs.Client()
	tenant := uuid.NewString()
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/tenants", tenant, map[string]string{"name": "t"}, nil); code != 201 {
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
	if code := doJSON(t, hc, "POST", hs.URL+"/v1/sessions/"+sess.SessionID+"/input", tenant,
		map[string]string{"text": "hello again", "requestId": requestID}, &second); code != 200 {
		t.Fatalf("replay input: %d", code)
	}
	if !first.Created || second.Created {
		t.Fatalf("created flags: %v %v", first.Created, second.Created)
	}
	if first.SubmissionID != second.SubmissionID {
		t.Fatalf("submission ids differ: %s %s", first.SubmissionID, second.SubmissionID)
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
	_ = doJSON(t, hc, "POST", hs.URL+"/v1/tenants", tenant, map[string]string{"name": "f"}, nil)

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
	_ = doJSON(t, hc, "POST", hs.URL+"/v1/tenants", tenant, map[string]string{"name": "s"}, nil)
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
