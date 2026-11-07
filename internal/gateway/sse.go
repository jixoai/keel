// A pull SSE reader for the Streamable HTTP transport.
//
// It parses the event-stream grammar the way the MCP transport does (lines end
// with CR, LF or CRLF; comments are ignored; data lines join with "\n"), but it
// hands events back one at a time instead of dispatching through a callback.
// That matters for backpressure: the consumer pulls an event, pushes it into
// the bounded queue, and stops pulling while the queue is full, which stops
// reading the HTTP body and lets TCP push back on the server. A per-event byte
// cap bounds the memory even if the server never sends a blank line.

package gateway

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// sseEvent is one dispatched event.
type sseEvent struct {
	Event string
	Data  string
	ID    string
}

// sseReader reads events from one stream.
type sseReader struct {
	br       *bufio.Reader
	maxBytes int
}

// newSSEReader returns a reader with a per-event byte cap.
func newSSEReader(r io.Reader, maxEventBytes int) *sseReader {
	if maxEventBytes <= 0 {
		maxEventBytes = DefaultMaxFrameBytes
	}
	return &sseReader{br: bufio.NewReaderSize(r, frameReadBuffer), maxBytes: maxEventBytes}
}

// ReadEvent returns the next event that carries data, or io.EOF at a clean end.
// Events without data (comments, keep-alives, id-only lines) are skipped.
func (r *sseReader) ReadEvent() (sseEvent, error) {
	var (
		event   sseEvent
		data    []string
		bytesIn int
		line    []byte
	)
	// finishLine consumes the completed line and reports whether the event is
	// now complete (a blank line closed it and data was pending).
	finishLine := func() (bool, error) {
		if len(line) == 0 {
			if len(data) == 0 {
				return false, nil
			}
			event.Data = strings.Join(data, "\n")
			return true, nil
		}
		if line[0] != ':' {
			field, value := splitField(line)
			switch field {
			case "data":
				bytesIn += len(value)
				if len(data) > 0 {
					bytesIn++
				}
				if bytesIn > r.maxBytes {
					return false, &FrameError{
						Kind:   FrameTooLarge,
						Size:   bytesIn,
						Limit:  r.maxBytes,
						Detail: fmt.Sprintf("SSE event over %d bytes", r.maxBytes),
					}
				}
				data = append(data, value)
			case "event":
				event.Event = value
			case "id":
				if !strings.ContainsRune(value, 0) {
					event.ID = value
				}
			}
		}
		line = line[:0]
		return false, nil
	}

	for {
		b, err := r.br.ReadByte()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return sseEvent{}, err
			}
			// Deliver the unterminated tail line, then the pending event.
			if len(line) > 0 {
				done, ferr := finishLine()
				if ferr != nil {
					return sseEvent{}, ferr
				}
				if done {
					return event, nil
				}
			}
			if len(data) > 0 {
				event.Data = strings.Join(data, "\n")
				return event, nil
			}
			return sseEvent{}, io.EOF
		}
		if b == '\r' || b == '\n' {
			done, ferr := finishLine()
			if ferr != nil {
				return sseEvent{}, ferr
			}
			if done {
				return event, nil
			}
			if b == '\r' {
				// CRLF belongs to this line ending; a lone CR is a line ending of
				// its own (the next byte, if any, starts the next line).
				next, peekErr := r.br.Peek(1)
				if peekErr == nil && len(next) == 1 && next[0] == '\n' {
					_, _ = r.br.ReadByte()
				}
			}
			continue
		}
		line = append(line, b)
		if len(line) > r.maxBytes {
			return sseEvent{}, &FrameError{
				Kind:   FrameTooLarge,
				Size:   len(line),
				Limit:  r.maxBytes,
				Detail: fmt.Sprintf("SSE line over %d bytes", r.maxBytes),
			}
		}
	}
}

// splitField splits "field: value" and trims the single leading space the
// event-stream grammar allows after the colon.
func splitField(line []byte) (field, value string) {
	index := bytes.IndexByte(line, ':')
	if index < 0 {
		return string(line), ""
	}
	return string(line[:index]), strings.TrimPrefix(string(line[index+1:]), " ")
}
