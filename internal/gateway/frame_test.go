// Framing tests: the scenarios the external-gateway spec calls out - large
// frames consumed in chunks with bounded peak memory, frame limits enforced
// while the peer is still streaming, and bad frames that do not take the
// connection down.

package gateway

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameReaderWriterRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	writer := NewFrameWriter(&buffer, 1<<20)
	frames := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"event.stream","params":{"sessionId":"s1"}}`,
		`{"jsonrpc":"2.0","id":"two","result":{"ok":true}}`,
	}
	for _, frame := range frames {
		if err := writer.WriteFrame([]byte(frame)); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	reader := NewFrameReader(&buffer, 1<<20)
	for index, want := range frames {
		got, err := reader.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", index, err)
		}
		if string(got) != want {
			t.Fatalf("frame %d = %q, want %q", index, got, want)
		}
	}
	if _, err := reader.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("after last frame: %v, want io.EOF", err)
	}
}

// chunkedReader hands out fixed-size chunks and records the largest read.
type chunkedReader struct {
	data      []byte
	chunkSize int
	offset    int
	reads     int
	maxRead   int
	total     int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	size := r.chunkSize
	if size > len(p) {
		size = len(p)
	}
	if size > len(r.data)-r.offset {
		size = len(r.data) - r.offset
	}
	copy(p, r.data[r.offset:r.offset+size])
	r.offset += size
	r.reads++
	r.total += size
	if size > r.maxRead {
		r.maxRead = size
	}
	return size, nil
}

// TestFrameReaderLargeFrameConsumedInChunks is the spec's "大参数" scenario:
// a frame far larger than any single buffer is consumed chunk by chunk and the
// peak read is bounded by the reader's own buffer.
func TestFrameReaderLargeFrameConsumedInChunks(t *testing.T) {
	payload := strings.Repeat("x", 512*1024)
	frame := `{"jsonrpc":"2.0","id":7,"result":{"blob":"` + payload + `"}}`
	source := &chunkedReader{data: []byte(frame + "\n"), chunkSize: 1024}
	reader := NewFrameReader(source, 4<<20)

	got, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if len(got) != len(frame) {
		t.Fatalf("frame length = %d, want %d", len(got), len(frame))
	}
	if source.maxRead > frameReadBuffer {
		t.Errorf("largest single read = %d, want <= %d (fixed reader buffer)", source.maxRead, frameReadBuffer)
	}
	if source.maxRead >= len(frame) {
		t.Errorf("largest read %d equals the frame size: the reader buffered the whole frame at once", source.maxRead)
	}
	if source.reads < 100 {
		t.Errorf("reads = %d, want many chunked reads", source.reads)
	}
}

// infiniteReader never ends and never contains a frame boundary: the strongest
// proof that an oversize frame is cut off while the peer is still streaming.
type infiniteReader struct {
	filler byte
	read   int
}

func (r *infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.filler
	}
	r.read += len(p)
	return len(p), nil
}

// TestFrameReaderOverCapAbortsWhileStreaming is the bounded-memory proof: an
// infinite stream cannot make the reader buffer more than the limit plus a
// bounded discard budget.
func TestFrameReaderOverCapAbortsWhileStreaming(t *testing.T) {
	const limit = 8 * 1024
	source := &infiniteReader{filler: 'x'}
	reader := NewFrameReader(source, limit)

	_, err := reader.ReadFrame()
	if err == nil {
		t.Fatal("ReadFrame on an infinite over-cap stream returned nil error")
	}
	var frameErr *FrameError
	if !errors.As(err, &frameErr) {
		t.Fatalf("error = %v, want *FrameError", err)
	}
	if frameErr.Kind != FrameTooLarge && frameErr.Kind != FrameDesync {
		t.Fatalf("kind = %q, want too_large or desync", frameErr.Kind)
	}
	budget := int64(2*limit + 2*frameReadBuffer)
	if int64(source.read) > budget {
		t.Fatalf("consumed %d bytes from the peer, want <= %d (the frame limit plus a bounded discard)", source.read, budget)
	}
	// Framing is lost for a stream that never produced a boundary: the error
	// is sticky rather than silently re-reading misaligned bytes.
	if _, err := reader.ReadFrame(); err == nil {
		t.Fatal("expected the desync error to be sticky")
	}
}

// TestFrameReaderResyncsAfterOversizeFrame: an oversized frame followed by a
// good one costs exactly that frame; the connection keeps working.
func TestFrameReaderResyncsAfterOversizeFrame(t *testing.T) {
	oversized := `{"jsonrpc":"2.0","id":1,"result":{"blob":"` + strings.Repeat("y", 300) + `"}}`
	good := `{"jsonrpc":"2.0","id":2,"result":{"ok":true}}`
	reader := NewFrameReader(strings.NewReader(oversized+"\n"+good+"\n"), 256)

	_, err := reader.ReadFrame()
	var frameErr *FrameError
	if !errors.As(err, &frameErr) || frameErr.Kind != FrameTooLarge {
		t.Fatalf("first frame error = %v, want non-fatal too_large", err)
	}
	if frameErr.Fatal {
		t.Fatalf("oversize frame should be recoverable, got fatal: %v", frameErr)
	}
	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("second ReadFrame: %v", err)
	}
	if string(frame) != good {
		t.Fatalf("second frame = %q, want %q", frame, good)
	}
}

func TestFrameReaderStrayLineIsOneFrameAndTheNextSurvives(t *testing.T) {
	// A bad frame must cost exactly that frame: the reader hands the stray
	// line to the caller (which rejects it) and keeps the next frame intact.
	good := `{"jsonrpc":"2.0","id":1,"result":{}}`
	reader := NewFrameReader(strings.NewReader("INFO not json at all\n\n"+good+"\n"), 1<<16)

	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("first ReadFrame: %v", err)
	}
	if string(frame) != "INFO not json at all" {
		t.Fatalf("first frame = %q, want the stray line", frame)
	}
	frame, err = reader.ReadFrame()
	if err != nil {
		t.Fatalf("second ReadFrame: %v", err)
	}
	if string(frame) != good {
		t.Fatalf("second frame = %q, want %q", frame, good)
	}
}

func TestFrameReaderTruncatedFrameDoesNotSwallowTheNext(t *testing.T) {
	truncated := `{"jsonrpc":"2.0","id":9,"result":{"oops"`
	good := `{"jsonrpc":"2.0","id":2,"result":{"ok":true}}`
	reader := NewFrameReader(strings.NewReader(truncated+"\n"+good+"\n"), 1<<16)

	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("first ReadFrame: %v", err)
	}
	if string(frame) != truncated {
		t.Fatalf("first frame = %q, want the truncated line", frame)
	}
	frame, err = reader.ReadFrame()
	if err != nil {
		t.Fatalf("second ReadFrame: %v", err)
	}
	if string(frame) != good {
		t.Fatalf("second frame = %q, want %q (the truncated frame swallowed it)", frame, good)
	}
}

// TestFrameReaderFramesWithoutTrailingNewline: a peer that forgets the final
// newline, or writes two compact frames back to back, is still framed right.
func TestFrameReaderFramesWithoutTrailingNewline(t *testing.T) {
	first := `{"jsonrpc":"2.0","id":1,"result":{}}`
	second := `{"jsonrpc":"2.0","id":2,"result":{}}`
	reader := NewFrameReader(strings.NewReader(first+second), 1<<16)
	for index, want := range []string{first, second} {
		frame, err := reader.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", index, err)
		}
		if string(frame) != want {
			t.Fatalf("frame %d = %q, want %q", index, frame, want)
		}
	}
}

// TestFrameReaderPrettyPrintedInputIsRejectedLineByLine documents the framing
// contract: frames are lines (MCP's stdio framing), so pretty-printed input is
// not one frame. What matters is that it never blocks the frames after it.
func TestFrameReaderPrettyPrintedInputIsRejectedLineByLine(t *testing.T) {
	pretty := "{\n  \"jsonrpc\": \"2.0\",\n  \"id\": 1,\n  \"result\": {}\n}\n"
	good := `{"jsonrpc":"2.0","id":2,"result":{}}`
	reader := NewFrameReader(strings.NewReader(pretty+good+"\n"), 1<<16)

	seen := 0
	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if string(frame) == good {
			break
		}
		if _, parseErr := ParseMessage(frame); parseErr == nil {
			t.Fatalf("pretty-printed line %q parsed as a message; framing changed", frame)
		}
		seen++
		if seen > 10 {
			t.Fatal("the readable frame was never reached")
		}
	}
	if seen == 0 {
		t.Fatal("pretty-printed input produced no rejectable lines")
	}
}

func TestFrameReaderHalfFrameAtEOF(t *testing.T) {
	reader := NewFrameReader(strings.NewReader(`{"jsonrpc":"2.0","id":1`), 1<<16)
	_, err := reader.ReadFrame()
	var frameErr *FrameError
	if !errors.As(err, &frameErr) || frameErr.Kind != FrameMalformed {
		t.Fatalf("error = %v, want malformed at EOF", err)
	}
	if _, err := reader.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the partial frame: %v, want io.EOF", err)
	}
}

func TestFrameReaderCRLFAndBlankLines(t *testing.T) {
	first := `{"jsonrpc":"2.0","id":1,"result":{}}`
	second := `{"jsonrpc":"2.0","id":2,"result":{}}`
	reader := NewFrameReader(strings.NewReader("\r\n"+first+"\r\n\r\n"+second+"\n"), 1<<16)
	for index, want := range []string{first, second} {
		frame, err := reader.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", index, err)
		}
		if string(frame) != want {
			t.Fatalf("frame %d = %q, want %q", index, frame, want)
		}
	}
}

func TestFrameWriterRefusesOversizeFrame(t *testing.T) {
	var buffer bytes.Buffer
	writer := NewFrameWriter(&buffer, 64)
	err := writer.WriteFrame([]byte(strings.Repeat("z", 65)))
	var frameErr *FrameError
	if !errors.As(err, &frameErr) || frameErr.Kind != FrameTooLarge || frameErr.Direction != "outbound" {
		t.Fatalf("error = %v, want outbound too_large", err)
	}
	if buffer.Len() != 0 {
		t.Fatalf("writer wrote %d bytes for a rejected frame", buffer.Len())
	}
}
