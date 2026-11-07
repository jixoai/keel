// Frame IO: newline-delimited JSON frames over a byte stream.
//
// The wire format is the one MCP's stdio transport uses: one JSON-RPC message
// per line, compact, UTF-8. The reader is incremental on purpose
// (external-gateway spec: streaming JSON, frame limit, bounded peak memory):
//
//   - bytes are consumed from the stream in fixed-size chunks (frameReadBuffer)
//     as they arrive; nothing ever waits for "the whole frame" before parsing;
//   - the frame limit is enforced while the frame streams in, so a provider
//     that sends a huge frame is cut off after limit+1 bytes instead of being
//     buffered (a hostile 10GB frame cannot allocate 10GB);
//   - a frame ends at its newline (outside a JSON string) or at the completion
//     of a top-level container, so a truncated frame costs exactly one frame
//     and the next line is read from a known position. Pretty-printed frames
//     with raw newlines are therefore not one frame - they are rejected line by
//     line and reported, like MCP's own stdio framing, and a frame that is
//     malformed can never swallow the frames after it.
//
// Peak memory of one read is therefore: frameReadBuffer (fixed) + one frame
// (at most the configured limit) + the decoded message payload (at most the
// same limit). The readers and writers never hold more than the limit plus a
// constant.

package gateway

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// frameReadBuffer is the fixed read-ahead. It bounds how many bytes a frame
// reader pulls from the stream beyond what the parser has consumed.
const frameReadBuffer = 32 * 1024

// FrameReader reads newline-delimited JSON frames from one stream. It is not
// safe for concurrent use; one reader belongs to one goroutine.
type FrameReader struct {
	br       *bufio.Reader
	maxBytes int
	frame    []byte
	// poison is set when framing is lost (FrameDesync): every later call keeps
	// returning it instead of silently reading misaligned bytes.
	poison error
}

// NewFrameReader returns a reader over r with a per-frame byte limit. A
// non-positive limit means DefaultMaxFrameBytes.
func NewFrameReader(r io.Reader, maxFrameBytes int) *FrameReader {
	if maxFrameBytes <= 0 {
		maxFrameBytes = DefaultMaxFrameBytes
	}
	return &FrameReader{
		br:       bufio.NewReaderSize(r, frameReadBuffer),
		maxBytes: maxFrameBytes,
	}
}

// MaxFrameBytes is the configured per-frame limit.
func (fr *FrameReader) MaxFrameBytes() int { return fr.maxBytes }

// frameScanState is the incremental JSON structural state: everything needed to
// know whether a quote is a delimiter or content, and whether a byte is inside
// the current frame.
type frameScanState struct {
	depth    int
	inString bool
	escaped  bool
	started  bool
}

// scanFrame appends the bytes of one frame from buf to frame. It consumes
// leading whitespace first; it stops at the frame's boundary.
//
// The boundary rule is MCP's stdio framing: a frame is one line, so a newline
// outside a JSON string always ends the frame, even mid-structure. That is
// what keeps a truncated frame from swallowing the frames that follow it - a
// bad frame must cost exactly that frame (external-gateway spec: 坏帧隔离).
// A complete top-level container also ends the frame, so a peer that omits the
// final newline (or writes two compact frames back to back) is still framed
// correctly.
//
// It returns the (possibly reallocated) frame, how many bytes of buf were
// consumed, whether the frame is complete, and whether the frame crossed the
// size limit. On overflow the offending byte is NOT consumed, so the caller can
// continue scanning it in discard mode.
func scanFrame(buf []byte, frame []byte, maxBytes int, st *frameScanState) (out []byte, consumed int, complete bool, overflow bool) {
	for i := 0; i < len(buf); i++ {
		b := buf[i]
		if !st.started {
			if isJSONSpace(b) {
				consumed = i + 1
				continue
			}
			st.started = true
		}
		if len(frame) >= maxBytes {
			return frame, i, false, true
		}
		// Update the structural state with b, then keep the byte.
		if st.inString {
			switch {
			case st.escaped:
				st.escaped = false
			case b == '\\':
				st.escaped = true
			case b == '"':
				st.inString = false
			}
		} else {
			switch b {
			case '"':
				st.inString = true
			case '{', '[':
				st.depth++
			case '}', ']':
				if st.depth > 0 {
					st.depth--
				}
			}
		}
		if b == '\n' && !st.inString {
			// End of line, end of frame; the newline itself is not part of it.
			// A CR before the LF belongs to the line ending, not to the frame.
			if len(frame) > 0 && frame[len(frame)-1] == '\r' {
				frame = frame[:len(frame)-1]
			}
			return frame, i + 1, true, false
		}
		frame = append(frame, b)
		consumed = i + 1
		if !st.inString && st.depth == 0 && (b == '}' || b == ']') {
			// The outermost container closed: the frame is complete even if the
			// peer forgot the newline, and "{...}{...}" on one line still splits.
			return frame, consumed, true, false
		}
	}
	return frame, consumed, false, false
}

// discardFrame consumes bytes without keeping them until the frame boundary is
// found (resynchronization), spending at most budget bytes. It returns how many
// bytes were consumed and whether the boundary was reached.
func discardFrame(buf []byte, budget *int, st *frameScanState) (consumed int, resynced bool) {
	for i := 0; i < len(buf); i++ {
		if *budget <= 0 {
			return i, false
		}
		b := buf[i]
		*budget--
		// The structural state must keep tracking, or the boundary that follows
		// cannot be recognized.
		if st.inString {
			switch {
			case st.escaped:
				st.escaped = false
			case b == '\\':
				st.escaped = true
			case b == '"':
				st.inString = false
			}
			continue
		}
		switch b {
		case '"':
			st.inString = true
		case '{', '[':
			st.depth++
		case '}', ']':
			if st.depth > 0 {
				st.depth--
			}
			if st.depth == 0 {
				return i + 1, true
			}
		case '\n':
			return i + 1, true
		}
	}
	return len(buf), false
}

// ReadFrame returns the next frame's raw bytes without the terminator.
//
// Errors:
//
//   - io.EOF when the stream ended cleanly between frames;
//   - a non-fatal *FrameError when one bad frame was skipped and the reader is
//     resynchronized (the next call reads the following frame);
//   - a fatal *FrameError (FrameDesync) when framing is lost; every subsequent
//     call returns the same error.
func (fr *FrameReader) ReadFrame() ([]byte, error) {
	if fr.poison != nil {
		return nil, fr.poison
	}
	fr.frame = fr.frame[:0]
	st := frameScanState{}
	for {
		buf, err := fr.fill()
		if err != nil {
			if errors.Is(err, io.EOF) && !st.started {
				return nil, io.EOF
			}
			if errors.Is(err, io.EOF) {
				return nil, &FrameError{
					Kind:   FrameMalformed,
					Size:   fr.frameLen(),
					Detail: "stream ended in the middle of a frame",
				}
			}
			return nil, err
		}
		frame, consumed, complete, overflow := scanFrame(buf, fr.frame, fr.maxBytes, &st)
		fr.frame = frame
		fr.br.Discard(consumed)
		if complete {
			return fr.frame, nil
		}
		if overflow {
			if err := fr.resync(&st); err != nil {
				return nil, err
			}
			return nil, &FrameError{
				Kind:   FrameTooLarge,
				Size:   fr.maxBytes,
				Limit:  fr.maxBytes,
				Detail: fmt.Sprintf("frame over %d bytes was skipped", fr.maxBytes),
			}
		}
	}
}

// resync discards the rest of the oversized frame. Framing survives if a
// boundary appears within a bounded budget; otherwise the reader is poisoned.
func (fr *FrameReader) resync(st *frameScanState) error {
	budget := fr.maxBytes
	for {
		buf, err := fr.fill()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil // the stream ended; the next call reports io.EOF
			}
			fr.poison = err
			return err
		}
		consumed, resynced := discardFrame(buf, &budget, st)
		fr.br.Discard(consumed)
		if resynced {
			return nil
		}
		if budget <= 0 {
			desync := &FrameError{
				Kind:   FrameDesync,
				Size:   fr.maxBytes,
				Limit:  fr.maxBytes,
				Fatal:  true,
				Detail: "no frame boundary within the resynchronization budget",
			}
			fr.poison = desync
			return desync
		}
	}
}

// fill returns the currently buffered bytes, reading more if none are buffered.
func (fr *FrameReader) fill() ([]byte, error) {
	if fr.br.Buffered() == 0 {
		if _, err := fr.br.Peek(1); err != nil {
			return nil, err
		}
	}
	buf, err := fr.br.Peek(fr.br.Buffered())
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func (fr *FrameReader) frameLen() int { return len(fr.frame) }

// FrameWriter writes newline-delimited JSON frames. Callers serialize writes
// themselves; the writer is otherwise stateless.
type FrameWriter struct {
	w        io.Writer
	maxBytes int
}

// NewFrameWriter returns a writer that refuses frames over the limit. A
// non-positive limit means DefaultMaxFrameBytes.
func NewFrameWriter(w io.Writer, maxFrameBytes int) *FrameWriter {
	if maxFrameBytes <= 0 {
		maxFrameBytes = DefaultMaxFrameBytes
	}
	return &FrameWriter{w: w, maxBytes: maxFrameBytes}
}

// WriteFrame writes one frame and its terminator. The frame limit is enforced
// before anything reaches the wire, so a too-large outbound message never
// leaves a half frame on the stream.
func (fw *FrameWriter) WriteFrame(payload []byte) error {
	if len(payload) == 0 {
		return errors.New("gateway: refusing to write an empty frame")
	}
	if len(payload) > fw.maxBytes {
		return &FrameError{
			Kind:      FrameTooLarge,
			Direction: "outbound",
			Size:      len(payload),
			Limit:     fw.maxBytes,
		}
	}
	return writeAll(fw.w, append(append(make([]byte, 0, len(payload)+1), payload...), '\n'))
}

// writeAll writes data completely, tolerating short writes.
func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// isJSONSpace reports whether b is JSON whitespace (RFC 8259).
func isJSONSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
}
