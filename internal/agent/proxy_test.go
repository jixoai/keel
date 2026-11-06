package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/provider"
)

func proxyServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runProxy(t *testing.T, body string) ([]provider.AssistantMessageEvent, agentcore.AssistantMessage, error) {
	t.Helper()
	srv := proxyServer(t, body)
	fn := NewProxyStream(ProxyOptions{ProxyURL: srv.URL, AuthToken: "token"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := fn(ctx, "test-model", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var events []provider.AssistantMessageEvent
	for event := range stream.Events() {
		events = append(events, event)
	}
	result, err := stream.Result(ctx)
	return events, result, err
}

func TestProxyToolCallMetadataFromEnd(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"start"}`,
		`data: {"type":"toolcall_start","contentIndex":0,"id":"call-1","toolName":"echo"}`,
		`data: {"type":"toolcall_delta","contentIndex":0,"delta":"{\"x\":"}`,
		`data: {"type":"toolcall_end","contentIndex":0,"toolCall":{"type":"toolCall","id":"call-1","name":"echo","arguments":{"x":1}}}`,
		`data: {"type":"done","reason":"toolUse","usage":{"input":3,"output":4,"totalTokens":7}}`,
		"",
	}, "\n")
	events, result, err := runProxy(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no events")
	}
	if result.StopReason != agentcore.StopReasonToolUse {
		t.Fatalf("stop reason = %q", result.StopReason)
	}
	if result.Usage == nil || result.Usage.InputTokens != 3 || result.Usage.OutputTokens != 4 {
		t.Fatalf("usage = %#v", result.Usage)
	}
	calls := result.ToolCalls()
	if len(calls) != 1 || calls[0].ID != "call-1" || calls[0].Name != "echo" {
		t.Fatalf("tool calls = %#v", calls)
	}
	if string(calls[0].Arguments) != `{"x":1}` {
		t.Fatalf("arguments = %s", calls[0].Arguments)
	}
}

func TestProxyTerminalEventWithoutNewline(t *testing.T) {
	body := `data: {"type":"start"}` + "\n" +
		`data: {"type":"text_start","contentIndex":0}` + "\n" +
		`data: {"type":"text_delta","contentIndex":0,"delta":"hello"}` + "\n" +
		`data: {"type":"done","reason":"stop","usage":{"input":1,"output":1}}`
	_, result, err := runProxy(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != agentcore.StopReasonEndTurn || ContentText(result.Content) != "hello" {
		t.Fatalf("result = %#v / %q", result.StopReason, ContentText(result.Content))
	}
}

func TestProxyStreamEndsWithoutTerminal(t *testing.T) {
	body := `data: {"type":"start"}` + "\n" + `data: {"type":"text_delta","contentIndex":0,"delta":"x"}` + "\n"
	events, result, err := runProxy(t, body)
	if err != nil {
		t.Fatal(err)
	}
	foundError := false
	for _, event := range events {
		if _, ok := event.(provider.StreamErrorEvent); ok {
			foundError = true
		}
	}
	if !foundError {
		t.Fatal("no terminal error event")
	}
	if result.StopReason != agentcore.StopReasonError ||
		!strings.Contains(result.ErrorMessage, "Connection closed by proxy server") {
		t.Fatalf("result = %#v", result)
	}
}

func TestProxyGrowingToolArguments(t *testing.T) {
	var b strings.Builder
	b.WriteString(`data: {"type":"start"}` + "\n")
	b.WriteString(`data: {"type":"toolcall_start","contentIndex":0,"id":"c","toolName":"echo"}` + "\n")
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "data: {\"type\":\"toolcall_delta\",\"contentIndex\":0,\"delta\":%q}\n", `"x"`)
	}
	b.WriteString(`data: {"type":"done","reason":"toolUse","usage":{"input":0,"output":0}}` + "\n")
	events, result, err := runProxy(t, b.String())
	if err != nil {
		t.Fatal(err)
	}
	var lastPartial provider.StreamToolCallEvent
	for _, event := range events {
		if e, ok := event.(provider.StreamToolCallEvent); ok {
			lastPartial = e
		}
	}
	if len(result.ToolCalls()) != 1 {
		t.Fatalf("tool calls = %d", len(result.ToolCalls()))
	}
	// Every delta updates the partial arguments; the final raw text is the
	// concatenation, and the block never keeps a separate growing copy.
	if got := string(lastPartial.Partial.ToolCalls()[0].Arguments); strings.Count(got, `"x"`) != 50 {
		t.Fatalf("partial arguments = %s", got)
	}
}
