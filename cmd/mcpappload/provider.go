// provider mode: an OpenAI-compatible chat-completions stub for the G5
// fixture. It never talks to a real provider and never reads a credential.
//
// The runtime runs at -runs 64, so the fixture must hold 64 model calls open
// long enough to observe the run-concurrency gauge. The model name selects the
// dwell time: a name containing "slow" sleeps for -slow-ms, everything else
// answers after -fast-ms. The response is a valid streaming completion in the
// same wire shape scripts/mcp-apps-e2e/stub-provider.py records.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type stubProvider struct {
	slowDelay time.Duration
	fastDelay time.Duration
	requests  atomic.Int64
	slow      atomic.Int64
	fast      atomic.Int64
}

func runProvider(args []string) error {
	fs := flag.NewFlagSet("provider", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:13688", "listen address")
	slowMS := fs.Int("slow-ms", 3000, "dwell time for models whose name contains \"slow\"")
	fastMS := fs.Int("fast-ms", 50, "dwell time for other models")
	if err := fs.Parse(args); err != nil {
		return err
	}
	provider := &stubProvider{
		slowDelay: time.Duration(*slowMS) * time.Millisecond,
		fastDelay: time.Duration(*fastMS) * time.Millisecond,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data": []any{
				map[string]any{"id": "perf-fast", "object": "model"},
				map[string]any{"id": "perf-slow", "object": "model"},
			},
		})
	})
	mux.HandleFunc("/v1/chat/completions", provider.chat)
	mux.HandleFunc("/control/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"requests": provider.requests.Load(),
			"slow":     provider.slow.Load(),
			"fast":     provider.fast.Load(),
		})
	})
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "mcpappload provider: %s slow=%s fast=%s\n",
		ln.Addr(), provider.slowDelay, provider.fastDelay)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second}
	return server.Serve(ln)
}

func (p *stubProvider) chat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &request)
	p.requests.Add(1)

	delay := p.fastDelay
	if strings.Contains(strings.ToLower(request.Model), "slow") {
		delay = p.slowDelay
		p.slow.Add(1)
	} else {
		p.fast.Add(1)
	}
	if delay > 0 {
		time.Sleep(delay)
	}

	if !request.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-perf-stub",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   request.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "perf ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 2, "total_tokens": 2},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeChunk := func(delta map[string]any, finish string) {
		chunk := map[string]any{
			"id":      "chatcmpl-perf-stub",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   request.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		raw, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	writeChunk(map[string]any{"role": "assistant"}, "")
	writeChunk(map[string]any{"content": "perf ok"}, "")
	writeChunk(map[string]any{}, "stop")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
