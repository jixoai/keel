package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/logsafe"
	"github.com/jixoai/keel/internal/metrics"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeReady struct{ err error }

func (f fakeReady) CheckReady(context.Context) error { return f.err }

func TestPrometheusMetricsEndpoint(t *testing.T) {
	reg := metrics.NewRegistry()
	metrics.NewSet(reg)
	metrics.BindGoRuntime(reg)
	hs := httptest.NewServer(New(Config{MetricsExporter: reg, Logger: discardLogger()}))
	defer hs.Close()

	res, err := http.Get(hs.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain; version=0.0.4") {
		t.Fatalf("content type = %q", got)
	}
	text := string(body)
	for _, want := range []string{
		"# TYPE keel_runs_started_total counter",
		"# TYPE keel_run_attempt_duration_seconds histogram",
		"# TYPE keel_run_attempts_total counter",
		"# TYPE go_goroutines gauge",
		"keel_run_attempt_duration_seconds_bucket{le=\"+Inf\"} 0",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"session_id=", "run_id=", "tenant_id=", "request_id="} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("high-cardinality label %q present:\n%s", forbidden, text)
		}
	}
}

func TestMetricsEndpointNotConfigured(t *testing.T) {
	hs := httptest.NewServer(New(Config{Logger: discardLogger()}))
	defer hs.Close()
	res, err := http.Get(hs.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestReadyEndpoint(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		hs := httptest.NewServer(New(Config{Ready: fakeReady{}, Logger: discardLogger()}))
		defer hs.Close()
		res, err := http.Get(hs.URL + "/v1/ready")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("status = %d", res.StatusCode)
		}
		var body map[string]any
		_ = json.NewDecoder(res.Body).Decode(&body)
		if body["ready"] != true {
			t.Fatalf("body = %v", body)
		}
	})

	t.Run("store down", func(t *testing.T) {
		hs := httptest.NewServer(New(Config{
			Ready:  fakeReady{err: fmt.Errorf("store: ready: ping: connection refused")},
			Logger: discardLogger(),
		}))
		defer hs.Close()
		res, err := http.Get(hs.URL + "/v1/ready")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", res.StatusCode)
		}
		if res.Header.Get("Retry-After") == "" {
			t.Fatal("missing Retry-After")
		}
		body, _ := io.ReadAll(res.Body)
		if strings.Contains(string(body), "connection refused") {
			t.Fatalf("internal detail leaked: %s", body)
		}
	})
}

func TestDrainRejectsNewRequests(t *testing.T) {
	drain := NewDrainer(3000, 1000)
	var logs bytes.Buffer
	logger := slog.New(logsafe.New(slog.NewJSONHandler(&logs, nil)))
	hs := httptest.NewServer(New(Config{Drain: drain, Logger: logger}))
	defer hs.Close()

	// Healthy before drain: probes are reachable.
	before, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	before.Body.Close()
	if before.StatusCode != 200 {
		t.Fatalf("health before drain = %d", before.StatusCode)
	}

	drain.Begin()

	session := uuid.NewString()
	req, _ := http.NewRequest("POST", hs.URL+"/v1/sessions/"+session+"/input", strings.NewReader(`{"text":"x"}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("draining request status = %d", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	rejection := logs.String()
	for _, want := range []string{`"route":"draining"`, `"draining":true`, `"status":503`, `"session_id":"` + session + `"`} {
		if !strings.Contains(rejection, want) {
			t.Fatalf("drain rejection log missing %q:\n%s", want, rejection)
		}
	}

	// Probes stay reachable; readiness reports the drain.
	probe, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	probe.Body.Close()
	if probe.StatusCode != 200 {
		t.Fatalf("health while draining = %d", probe.StatusCode)
	}
	ready, err := http.Get(hs.URL + "/v1/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Body.Close()
	if ready.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ready while draining = %d", ready.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(ready.Body).Decode(&body)
	if body["draining"] != true {
		t.Fatalf("ready body = %v", body)
	}
}

// sseEvent is one parsed SSE frame.
type sseEvent struct {
	id    string
	event string
	data  string
}

func readSSEEvent(t *testing.T, br *bufio.Reader) sseEvent {
	t.Helper()
	var ev sseEvent
	have := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read sse: %v (event so far %+v)", err, ev)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if have || ev.event != "" {
				return ev
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			ev.id = strings.TrimPrefix(line, "id: ")
			have = true
		case strings.HasPrefix(line, "event: "):
			ev.event = strings.TrimPrefix(line, "event: ")
			have = true
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
			have = true
		case strings.HasPrefix(line, ":"):
			// comment/heartbeat
		}
	}
}

func TestSSEDrainFlushesCommittedEvents(t *testing.T) {
	drain := NewDrainer(2500, 1000)
	hs, st := newTestServer(t, func(c *Config) { c.Drain = drain })
	hc := hs.Client()
	ctx := context.Background()
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "drain"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "one"}); err != nil {
		t.Fatal(err)
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, "GET", hs.URL+"/v1/sessions/"+session.String()+"/events/stream?after=0", nil)
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
	if first.id != "1" || first.event != "user.message" {
		t.Fatalf("first event = %+v", first)
	}

	// Commit another event after the cursor, then start draining. The drain
	// branch must flush it before the drain frame.
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "two"}); err != nil {
		t.Fatal(err)
	}
	drain.Begin()

	var second sseEvent
	for {
		ev := readSSEEvent(t, br)
		if ev.event == "drain" {
			// A wakeup may race the drain flush; the second event must have
			// arrived first.
			if second.id != "2" || second.event != "user.message" {
				t.Fatalf("drain arrived before committed event 2: second=%+v drain=%+v", second, ev)
			}
			var payload struct {
				RetryAfterMS int `json:"retry_after_ms"`
			}
			if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
				t.Fatalf("drain payload %q: %v", ev.data, err)
			}
			if payload.RetryAfterMS != 2500 {
				t.Fatalf("retry_after_ms = %d", payload.RetryAfterMS)
			}
			break
		}
		if ev.event == "user.message" {
			second = ev
		}
	}
	// The handler returns after its drain slot; the body must end.
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read to EOF: %v", err)
	}
	if strings.Contains(string(rest), "event: user.message") {
		t.Fatalf("events after drain: %s", rest)
	}
}

func TestRequestLoggingRedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(logsafe.New(slog.NewJSONHandler(&buf, nil)))
	hs := httptest.NewServer(New(Config{Logger: logger}))
	defer hs.Close()

	tenant := uuid.NewString()
	req, _ := http.NewRequest("POST", hs.URL+"/v1/not-a-route", strings.NewReader(`{"text":"message body secret"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenant)
	req.Header.Set("Authorization", "Bearer credential-secret")
	req.Header.Set("X-API-Key", "credential-apikey")
	req.Header.Set("X-Request-ID", "req-abc")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := res.Header.Get("X-Request-ID"); got != "req-abc" {
		t.Fatalf("X-Request-ID = %q", got)
	}
	logged := buf.String()
	for _, secret := range []string{"credential-secret", "credential-apikey", "message body secret"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaked %q:\n%s", secret, logged)
		}
	}
	for _, want := range []string{`"request_id":"req-abc"`, `"tenant_id":"` + tenant + `"`, `"status":404`} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log missing %q:\n%s", want, logged)
		}
	}
}

func TestWaitCloseSpreadsCloses(t *testing.T) {
	d := NewDrainer(1000, 100) // 10ms per close
	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := d.WaitClose(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("five closes finished in %v, want staggered", elapsed)
	}
}

func TestMetricsSeriesStableAcrossSessions(t *testing.T) {
	reg := metrics.NewRegistry()
	metrics.NewSet(reg)
	metrics.BindGoRuntime(reg)
	streamMetrics := &stream.Metrics{}
	metrics.BindStreams(reg, func() int64 { return streamMetrics.StreamsActive.Load() })
	metrics.BindQueueDepth(reg, fixedPendingZero{})
	hs, st := newTestServer(t, func(c *Config) {
		c.MetricsExporter = reg
		c.Metrics = streamMetrics
	})
	defer hs.Close()

	countSeries := func() int {
		t.Helper()
		res, err := http.Get(hs.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		n := 0
		for _, line := range strings.Split(string(body), "\n") {
			if line != "" && !strings.HasPrefix(line, "#") {
				n++
			}
		}
		return n
	}
	before := countSeries()
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "series"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := st.CreateSession(context.Background(), tenant, uuid.New()); err != nil {
			t.Fatal(err)
		}
	}
	after := countSeries()
	if before != after {
		t.Fatalf("series count grew with sessions: %d -> %d", before, after)
	}
}

type fixedPendingZero struct{}

func (fixedPendingZero) CountPendingRuns(context.Context) (int64, error) { return 0, nil }
