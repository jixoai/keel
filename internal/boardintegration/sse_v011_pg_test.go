package boardintegration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/store"
)

// sseFrame is one complete SSE frame. id-only frames carry no data; they are
// the board v0.1.1 cursor-advance contract for events the caller may not see.
type sseFrame struct {
	id   string
	data string
}

// readSSEFrame reads lines until one frame is complete. Comments (heartbeats,
// the opening `: connected`) are skipped.
func readSSEFrame(t *testing.T, lines <-chan string, timeout time.Duration) sseFrame {
	t.Helper()
	deadline := time.After(timeout)
	frame := sseFrame{}
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("SSE stream closed while reading a frame")
			}
			switch {
			case strings.HasPrefix(line, "id: "):
				frame.id = strings.TrimSpace(strings.TrimPrefix(line, "id: "))
			case strings.HasPrefix(line, "data: "):
				frame.data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			case line == "":
				if frame.id != "" || frame.data != "" {
					return frame
				}
			}
		case <-deadline:
			t.Fatal("timed out reading an SSE frame")
		}
	}
}

func readSSEDataFrame(t *testing.T, lines <-chan string, timeout time.Duration) sseFrame {
	t.Helper()
	for {
		frame := readSSEFrame(t, lines, timeout)
		if frame.data != "" {
			return frame
		}
		if frame.id == "" {
			t.Fatal("SSE frame had neither id nor data")
		}
	}
}

type sseEventBody struct {
	Cursor  string `json:"cursor"`
	Message *struct {
		Text string `json:"text"`
	} `json:"message"`
}

// TestBoardSSEHiddenEventIDOnly covers P1-4: an event in a channel the caller
// cannot read advances the cursor with an id-only frame, never its content.
func TestBoardSSEHiddenEventIDOnly(t *testing.T) {
	env := newTestEnv(t, nil)
	server := env.server()
	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "work"}); resp.status != 201 {
		t.Fatalf("create work: %d", resp.status)
	}
	env.relation(env.tenant, "work", store.RelationViewer, env.carol.record.ID)
	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "private"}); resp.status != 201 {
		t.Fatalf("create private: %d", resp.status)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, lines := openSSE(t, ctx, server.URL+"/v1/board/events", "carol", "")
	defer stream.Body.Close()

	if resp := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "private", "text": "HIDDEN-BODY"}); resp.status != 201 {
		t.Fatalf("private post: %d", resp.status)
	}
	if resp := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "VISIBLE-BODY"}); resp.status != 201 {
		t.Fatalf("work post: %d", resp.status)
	}

	deadline := 3 * time.Second
	var idOnly []string
	var visible sseFrame
	for visible.data == "" {
		frame := readSSEFrame(t, lines, deadline)
		if frame.data == "" {
			if frame.id == "" {
				t.Fatal("SSE frame had neither id nor data")
			}
			idOnly = append(idOnly, frame.id)
			continue
		}
		visible = frame
	}
	if len(idOnly) == 0 {
		t.Fatal("a permission-hidden event produced no id-only frame")
	}
	if strings.Contains(visible.data, "HIDDEN-BODY") {
		t.Fatalf("the visible frame carried hidden content: %s", visible.data)
	}
	var body sseEventBody
	if err := json.Unmarshal([]byte(visible.data), &body); err != nil {
		t.Fatalf("decode visible event: %v", err)
	}
	if body.Message == nil || !strings.Contains(body.Message.Text, "VISIBLE-BODY") {
		t.Fatalf("unexpected visible event: %s", visible.data)
	}
	for _, id := range idOnly {
		if id == body.Cursor {
			t.Fatalf("the visible cursor %s was emitted as an id-only frame", id)
		}
	}
}

// TestBoardSSELastEventIDWinsOverAfter covers P1-3: a reconnect header is
// authoritative over a stale `after` query.
func TestBoardSSELastEventIDWinsOverAfter(t *testing.T) {
	env := newTestEnv(t, nil)
	server := env.server()
	if resp := env.do(t, server, "POST", "/v1/board/channels", "alice", map[string]any{"path": "work"}); resp.status != 201 {
		t.Fatalf("create work: %d", resp.status)
	}
	env.relation(env.tenant, "work", store.RelationViewer, env.carol.record.ID)

	ctx, cancel := context.WithCancel(context.Background())
	stream, lines := openSSE(t, ctx, server.URL+"/v1/board/events", "carol", "")
	if resp := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "first"}); resp.status != 201 {
		t.Fatalf("first post: %d", resp.status)
	}
	first := readSSEDataFrame(t, lines, 3*time.Second)
	if resp := env.do(t, server, "POST", "/v1/board/messages", "alice", map[string]any{"channel": "work", "text": "second"}); resp.status != 201 {
		t.Fatalf("second post: %d", resp.status)
	}
	second := readSSEDataFrame(t, lines, 3*time.Second)
	cancel()
	stream.Body.Close()
	if first.id == "" || second.id == "" {
		t.Fatalf("event ids = %q/%q", first.id, second.id)
	}

	// `after` says start at the second event; the reconnect header says start at
	// the first. The header wins, so the second event is replayed.
	resumeURL := server.URL + "/v1/board/events?after=" + second.id
	resumeCtx, resumeCancel := context.WithCancel(context.Background())
	defer resumeCancel()
	resumed, resumedLines := openSSE(t, resumeCtx, resumeURL, "carol", first.id)
	defer resumed.Body.Close()
	replayed := readSSEDataFrame(t, resumedLines, 3*time.Second)
	if !strings.Contains(replayed.data, "second") {
		t.Fatalf("Last-Event-ID did not win over after; replayed %s", replayed.data)
	}
}
