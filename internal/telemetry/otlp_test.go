package telemetry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestOTLPExportsWhenEndpointConfigured(t *testing.T) {
	var traces, metrics atomic.Int64
	var traceBody, metricBody bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/traces":
			traces.Add(1)
			_, _ = io.Copy(&traceBody, r.Body)
		case "/v1/metrics":
			metrics.Add(1)
			_, _ = io.Copy(&metricBody, r.Body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	adapter, err := NewOTLP(ctx, OTLPOptions{Endpoint: server.URL, ServiceName: "keel-test"})
	if err != nil {
		t.Fatal(err)
	}
	root, endRoot := adapter.StartManualSpan(SpanOptions{Name: "keel.agent.run"})
	turn, endTurn := root.(ManualStarter).StartManualSpan(SpanOptions{Name: "keel.agent.turn"})
	turn.AddEvent("message_start", SpanAttributes{"role": "assistant"})
	endTurn()
	endRoot()
	adapter.Metrics().Counter("keel.agent.turns", nil).Add(1, SpanAttributes{"model": "fake"})
	if err := adapter.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if traces.Load() == 0 {
		t.Fatal("no trace export request")
	}
	if metrics.Load() == 0 {
		t.Fatal("no metric export request")
	}
	if !bytes.Contains(traceBody.Bytes(), []byte("keel.agent.run")) || !bytes.Contains(traceBody.Bytes(), []byte("keel.agent.turn")) {
		t.Fatalf("trace payload missing spans: %s", traceBody.String())
	}
	if !bytes.Contains(metricBody.Bytes(), []byte("keel.agent.turns")) {
		t.Fatalf("metric payload missing turn counter: %s", metricBody.String())
	}
}

func TestOTLPRequiresEndpoint(t *testing.T) {
	if _, err := NewOTLP(context.Background(), OTLPOptions{}); err == nil {
		t.Fatal("OTLP accepted an empty endpoint")
	}
}
