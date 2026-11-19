package jobqueue

import (
	"context"
	"errors"
	"io"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/bashbridge"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/store"
)

// outputSink tees one stream into (a) a fixed head/tail window and (b) the blob
// uploader. The uploader spools to a temp file and then writes chunks/CAS, so
// memory stays constant no matter how large the stream is; gbash's own capture
// buffer is small and never the output path.
//
// A blob failure never fails the command: the bounded window remains the model
// fact and Output.Error records what was lost. A write into a closed pipe is
// treated the same way, so a reader that stopped early (the output cap) cannot
// deadlock the sandbox.
type outputSink struct {
	bound  *bashbridge.Bounded
	reader *io.PipeReader
	writer *io.PipeWriter
	result chan uploadResult
	broken atomic.Bool
	wrote  atomic.Bool
}

// errEmptyOutput closes an unused pipe so no zero-byte blob is created.
var errEmptyOutput = errors.New("jobqueue: empty output stream")

type uploadResult struct {
	meta store.BlobMeta
	err  error
}

func newOutputSink(ctx context.Context, uploader *blob.Uploader, tenantID uuid.UUID, bound *bashbridge.Bounded) *outputSink {
	sink := &outputSink{bound: bound}
	if uploader == nil {
		return sink
	}
	reader, writer := io.Pipe()
	sink.reader = reader
	sink.writer = writer
	sink.result = make(chan uploadResult, 1)
	go func() {
		meta, err := uploader.Put(ctx, tenantID, "text/plain; charset=utf-8", reader)
		// If Put stopped before EOF (cap/error), unblock any in-flight Write.
		_ = reader.CloseWithError(err)
		sink.result <- uploadResult{meta: meta, err: err}
	}()
	return sink
}

// Write implements io.Writer.
func (s *outputSink) Write(p []byte) (int, error) {
	_, _ = s.bound.Write(p)
	if len(p) > 0 {
		s.wrote.Store(true)
	}
	if s.writer == nil || s.broken.Load() {
		return len(p), nil
	}
	if _, err := s.writer.Write(p); err != nil {
		s.broken.Store(true)
	}
	// The command must keep running even when its full output could not be
	// stored; the head/tail window still captures the fact.
	return len(p), nil
}

// Snapshot returns the current bounded window (safe while the job runs).
func (s *outputSink) Snapshot() Output {
	head, tail, total := s.bound.Snapshot()
	return Output{Head: head, Tail: tail, Bytes: total, Truncated: total > int64(len(head)+len(tail))}
}

// Close finishes the upload and returns the terminal output fact.
func (s *outputSink) Close() Output {
	if s.writer != nil {
		if !s.wrote.Load() {
			// Nothing was printed: close the reader first so Put returns without
			// reserving an empty blob row.
			_ = s.reader.CloseWithError(errEmptyOutput)
		}
		_ = s.writer.Close()
	}
	out := s.Snapshot()
	if s.result == nil {
		return out
	}
	res := <-s.result
	if errors.Is(res.err, errEmptyOutput) {
		return out
	}
	if res.err == nil {
		if res.meta.BlobID != uuid.Nil {
			blobID := res.meta.BlobID
			out.BlobID = &blobID
		}
		if res.meta.Size < out.Bytes {
			out.Truncated = true
		}
		return out
	}
	out.Truncated = true
	switch {
	case errors.Is(res.err, blob.ErrTooLarge):
		out.Error = "output exceeded the blob cap; only head/tail were kept"
	default:
		out.Error = "output store failed: " + res.err.Error()
	}
	return out
}
