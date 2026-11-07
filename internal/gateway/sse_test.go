// SSE reader tests: line endings, comments, multi-line data, caps and the
// buffered tail at EOF.

package gateway

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEReaderLineEndingsAndFields(t *testing.T) {
	input := ": keep-alive\n" +
		"id: 17\r\n" +
		"event: message\r\n" +
		"data: {\"a\":1}\r\n" +
		"\r\n" +
		"data: first line\ndata: second line\n\n" +
		"data: bare-cr\r\r" +
		"data: {\"tail\":true}"
	reader := newSSEReader(strings.NewReader(input), 1<<20)

	first, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if first.ID != "17" || first.Event != "message" || first.Data != `{"a":1}` {
		t.Fatalf("first event = %+v", first)
	}
	second, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("second event: %v", err)
	}
	if second.Data != "first line\nsecond line" {
		t.Fatalf("multi-line data = %q", second.Data)
	}
	// A lone CR is a line ending of its own: "data: bare-cr\r" then "\r"
	// dispatches the event, and the unterminated tail becomes the next one.
	third, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("third event: %v", err)
	}
	if third.Data != "bare-cr" {
		t.Fatalf("CR-terminated data = %q, want bare-cr", third.Data)
	}
	fourth, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("fourth event: %v", err)
	}
	if fourth.Data != `{"tail":true}` {
		t.Fatalf("tail event = %q", fourth.Data)
	}
	if _, err := reader.ReadEvent(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last event: %v, want io.EOF", err)
	}
}

func TestSSEReaderDeliversTailWithoutBlankLine(t *testing.T) {
	reader := newSSEReader(strings.NewReader("data: last"), 1<<20)
	event, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if event.Data != "last" {
		t.Fatalf("event = %+v, want the unterminated tail", event)
	}
}

func TestSSEReaderCapsEvents(t *testing.T) {
	payload := "data: " + strings.Repeat("x", 4096) + "\n"
	reader := newSSEReader(strings.NewReader(payload+"\n"), 512)
	_, err := reader.ReadEvent()
	var frameErr *FrameError
	if !errors.As(err, &frameErr) || frameErr.Kind != FrameTooLarge {
		t.Fatalf("error = %v, want *FrameError too_large", err)
	}
}

func TestSSEReaderCapsUnterminatedLines(t *testing.T) {
	reader := newSSEReader(strings.NewReader("data: "+strings.Repeat("y", 4096)), 512)
	if _, err := reader.ReadEvent(); err == nil {
		t.Fatal("an unterminated over-cap line was accepted")
	}
}

func TestSSEReaderSkipsEventsWithoutData(t *testing.T) {
	reader := newSSEReader(strings.NewReader("id: 5\n\nevent: ping\n\ndata: real\n\n"), 1<<20)
	event, err := reader.ReadEvent()
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if event.Data != "real" {
		t.Fatalf("event = %+v, want the data-bearing one", event)
	}
}
