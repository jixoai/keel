// Package blob streams image bytes between PostgreSQL chunk rows (or the local
// CAS) and HTTP without ever materialising a whole image in memory.
//
// The base64 path is the hot one: 64KiB chunk → at most two carry bytes → one
// 87,384-byte encoded block → Write. A 10MB image therefore costs a constant
// ~150KB per stream no matter how large the image is.
package blob

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
)

// Reader streams blobs out of the store.
type Reader struct {
	Store   *store.Store
	CASDir  string
	SrcPool *stream.BufferPool // holds 64KiB read batches
	DstPool *stream.BufferPool // holds base64 output blocks
	// Batch is the number of chunks fetched per query. 1 keeps per-stream memory
	// at ~150KB; 2 is the 1Gbps mode (PG needs 715 QPS not 1430).
	Batch int
}

// NewReader builds a reader with pools sized for the given concurrency and
// chunk batch (1 = ~150KB/stream, 2 = the 1Gbps mode).
func NewReader(s *store.Store, concurrency, batch int) *Reader {
	if concurrency <= 0 {
		concurrency = 1000
	}
	if batch <= 0 {
		batch = 1
	}
	return &Reader{
		Store:   s,
		SrcPool: stream.NewBufferPool(store.ChunkSize*batch, concurrency),
		DstPool: stream.NewBufferPool(base64.StdEncoding.EncodedLen(store.ChunkSize*batch), concurrency),
		Batch:   batch,
	}
}

// WireLen returns the base64 length of a blob of size bytes.
func WireLen(size int64) int64 { return 4 * ((size + 2) / 3) }

// blocks returns a function yielding successive data blocks from srcOffset to
// the end of the blob. The returned block is only valid until the next call.
func (r *Reader) blocks(ctx context.Context, tenantID, blobID uuid.UUID, meta store.BlobMeta, srcOffset int64, buf []byte) (func() ([]byte, error), func(), error) {
	if meta.Storage == store.StorageLocalCAS {
		if r.CASDir == "" {
			return nil, nil, errors.New("blob: CAS dir not configured")
		}
		path, err := CASPath(r.CASDir, meta.SHA256)
		if err != nil {
			return nil, nil, err
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, fmt.Errorf("blob: open cas: %w", err)
		}
		if _, err := f.Seek(srcOffset, io.SeekStart); err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("blob: seek cas: %w", err)
		}
		next := func() ([]byte, error) {
			// Pooled buffers are returned with len 0 and the full capacity;
			// Read on a zero-length slice would report (0, nil) forever.
			window := buf[:cap(buf)]
			n, err := f.Read(window)
			if n > 0 {
				return window[:n], nil
			}
			if errors.Is(err, io.EOF) {
				return nil, nil
			}
			return nil, err
		}
		return next, func() { f.Close() }, nil
	}

	seq := int32(srcOffset / store.ChunkSize)
	skip := int(srcOffset % store.ChunkSize)
	next := func() ([]byte, error) {
		batch, err := r.Store.ReadChunkBatch(ctx, tenantID, blobID, seq, r.Batch, buf)
		if err != nil {
			return nil, err
		}
		if len(batch.Data) == 0 {
			return nil, nil
		}
		seq += int32(r.Batch)
		data := batch.Data
		if skip > 0 {
			if skip >= len(data) {
				skip -= len(data)
				return nil, nil
			}
			data = data[skip:]
			skip = 0
		}
		return data, nil
	}
	return next, func() {}, nil
}

// StreamBase64 writes the blob as base64 to w, starting at wireOffset (a multiple
// of 4, as reported by the client's Range header). It returns the number of wire
// bytes written.
func (r *Reader) StreamBase64(ctx context.Context, tenantID, blobID uuid.UUID, meta store.BlobMeta, w io.Writer, wireOffset int64) (int64, error) {
	if !meta.Complete {
		return 0, store.ErrIncompleteBlob
	}
	total := WireLen(meta.Size)
	if wireOffset < 0 || wireOffset > total || wireOffset%4 != 0 {
		return 0, fmt.Errorf("blob: invalid wire offset %d (total %d)", wireOffset, total)
	}
	// 4 base64 chars ≡ 3 source bytes, so a wire offset is always a source
	// offset that preserves the base64 grouping.
	srcOff := wireOffset / 4 * 3
	remaining := meta.Size - srcOff
	if remaining <= 0 {
		return 0, nil
	}

	src := r.SrcPool.Get()
	defer func() { r.SrcPool.Put(src) }()
	dst := r.DstPool.Get()
	defer func() { r.DstPool.Put(dst) }()

	next, closeFn, err := r.blocks(ctx, tenantID, blobID, meta, srcOff, src)
	if err != nil {
		return 0, err
	}
	defer closeFn()

	carry := make([]byte, 0, 3)
	var written int64
	for remaining > 0 {
		data, err := next()
		if err != nil {
			return written, err
		}
		if len(data) == 0 {
			return written, fmt.Errorf("blob: missing data after %d bytes", meta.Size-remaining)
		}
		if int64(len(data)) > remaining {
			data = data[:remaining]
		}
		remaining -= int64(len(data))

		buf := data
		if len(carry) > 0 {
			// A previous block ended mid-group: stitch before encoding.
			joined := make([]byte, 0, len(carry)+len(buf))
			joined = append(joined, carry...)
			joined = append(joined, buf...)
			buf = joined
		}
		n3 := len(buf) - len(buf)%3
		if n3 > 0 {
			enc := dst[:base64.StdEncoding.EncodedLen(n3)]
			base64.StdEncoding.Encode(enc, buf[:n3])
			if _, err := w.Write(enc); err != nil {
				return written, err
			}
			written += int64(len(enc))
		}
		carry = append(carry[:0], buf[n3:]...)
	}
	if len(carry) > 0 {
		enc := dst[:base64.StdEncoding.EncodedLen(len(carry))]
		base64.StdEncoding.Encode(enc, carry)
		if _, err := w.Write(enc); err != nil {
			return written, err
		}
		written += int64(len(enc))
	}
	return written, nil
}

// StreamRaw writes the blob's raw bytes, starting at srcOffset and writing at
// most length bytes (0 = to the end).
func (r *Reader) StreamRaw(ctx context.Context, tenantID, blobID uuid.UUID, meta store.BlobMeta, w io.Writer, srcOffset, length int64) (int64, error) {
	if !meta.Complete {
		return 0, store.ErrIncompleteBlob
	}
	if srcOffset < 0 || srcOffset > meta.Size {
		return 0, fmt.Errorf("blob: invalid offset %d (size %d)", srcOffset, meta.Size)
	}
	if length <= 0 || srcOffset+length > meta.Size {
		length = meta.Size - srcOffset
	}
	src := r.SrcPool.Get()
	defer func() { r.SrcPool.Put(src) }()
	next, closeFn, err := r.blocks(ctx, tenantID, blobID, meta, srcOffset, src)
	if err != nil {
		return 0, err
	}
	defer closeFn()
	var written int64
	for length > 0 {
		data, err := next()
		if err != nil {
			return written, err
		}
		if len(data) == 0 {
			return written, errors.New("blob: missing data")
		}
		if int64(len(data)) > length {
			data = data[:length]
		}
		n, err := w.Write(data)
		written += int64(n)
		length -= int64(n)
		if err != nil {
			return written, err
		}
	}
	return written, nil
}
