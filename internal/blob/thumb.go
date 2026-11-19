package blob

// Asynchronous thumbnail generation (change keel-ga §5 blob-lifecycle).
//
// Lifecycle, stated as the durable state machine a reviewer can audit:
//
//	upload completes ──► thumb_status='pending'   (MarkThumbPending, same process)
//	                       │
//	        Thumbnailer.Run │ claims pending rows (periodic + upload notification)
//	                       ▼
//	              decode + resize + PNG
//	                       │
//	          ┌────────────┴────────────┐
//	          ▼                         ▼
//	   thumb_status='ready'       thumb_status='failed'
//	   thumb_blob_id=<new blob>    (terminal; never retried)
//
// The thumbnail bytes are an ordinary blob row (complete, mime image/png), so
// they use the existing storage/read/tenant path; there is no second store.
// The row carries expires_at=now+24h as a safety net: while a parent references
// it, ReleaseExpired's "not pointed at by a thumbnail reference" predicate keeps
// it alive; if a parent disappears before the ready update (a very narrow GC
// race), the orphan thumbnail is reaped by TTL instead of leaking forever.
//
// Failure is contained twice: generation errors only mark the row failed, and
// the upload handler never waits for this goroutine. A bad image therefore
// cannot block the upload response or a chat turn.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register the GIF decoder
	_ "image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/store"
)

const (
	// ThumbMaxEdge is the long-edge bound of a generated thumbnail.
	ThumbMaxEdge = 320
	// thumbMaxPixels bounds decoded dimensions before allocation: a small
	// compressed file can claim enormous width/height ("decompression bomb").
	// 25 MP admits a 5000x5000 original while keeping one RGBA decode under
	// ~100MB in this single-threaded generator.
	thumbMaxPixels int64 = 25_000_000
	// thumbMaxSourceBytes bounds the encoded input. The upload endpoint already
	// caps at 64MiB by default; this is the independent generator-side ceiling.
	thumbMaxSourceBytes int64 = 64 << 20
	// thumbTTL is the orphan safety net described above.
	thumbTTL = 24 * time.Hour
	// ThumbDefaultInterval is the idle scan period. Uploads also wake the
	// generator directly, so this is a recovery path, not the fast path.
	ThumbDefaultInterval = 30 * time.Second
)

var (
	// ErrThumbUnsupported marks an image format the standard library cannot
	// decode (or a non-image MIME). It is a permanent per-blob failure.
	ErrThumbUnsupported = errors.New("blob: thumbnail format not supported")
	// ErrThumbTooLarge marks an input that exceeds the generator's independent
	// byte/pixel ceilings.
	ErrThumbTooLarge = errors.New("blob: image exceeds thumbnail limits")
)

// Thumbnailer turns pending image blobs into 320px PNG blobs. It is safe for
// concurrent Notify calls; Process itself is sequential by design (one large
// decode at a time keeps the process memory bound predictable).
type Thumbnailer struct {
	Store *store.Store
	// Reader streams the original. A small reader (a few streams) is enough.
	Reader *Reader
	// Uploader must be a *bare* uploader (Thumbnails == nil), or a generated PNG
	// would queue itself for thumbnail generation forever.
	Uploader *Uploader
	Logger   *slog.Logger
	// Interval is the recovery scan period. Non-positive uses
	// ThumbDefaultInterval.
	Interval time.Duration
	// Batch is the number of pending rows per scan. Non-positive uses 8.
	Batch int

	notify chan struct{}
}

// NewThumbnailer builds the generator. uploader must not have its own
// Thumbnails hook (see the field comment).
func NewThumbnailer(st *store.Store, reader *Reader, uploader *Uploader, log *slog.Logger) *Thumbnailer {
	if log == nil {
		log = slog.Default()
	}
	return &Thumbnailer{
		Store: st, Reader: reader, Uploader: uploader, Logger: log,
		notify: make(chan struct{}, 1),
	}
}

// Notify wakes Run without blocking. A full channel means a scan is already
// scheduled, which is exactly the desired coalescing.
func (t *Thumbnailer) Notify() {
	if t == nil {
		return
	}
	select {
	case t.notify <- struct{}{}:
	default:
	}
}

// Run scans on Notify and on Interval until ctx ends. It performs one scan at
// start, so pending rows left by a crashed process are picked up immediately.
func (t *Thumbnailer) Run(ctx context.Context) {
	interval := t.Interval
	if interval <= 0 {
		interval = ThumbDefaultInterval
	}
	t.scan(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-t.notify:
		}
		t.scan(ctx)
	}
}

func (t *Thumbnailer) scan(ctx context.Context) {
	batch := t.Batch
	if batch <= 0 {
		batch = 8
	}
	jobs, err := t.Store.PendingThumbnails(ctx, batch)
	if err != nil {
		if ctx.Err() == nil {
			t.Logger.Warn("thumbnail scan failed", "err", err)
		}
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		if err := t.processOne(ctx, job); err != nil && ctx.Err() == nil {
			t.Logger.Warn("thumbnail generation failed", "tenant", job.TenantID, "blob", job.BlobID, "err", err)
		}
	}
}

// processOne generates and records one thumbnail. Generation failure marks the
// row failed; only a store error while marking is returned as a scan error.
func (t *Thumbnailer) processOne(ctx context.Context, job store.ThumbJob) error {
	thumbID, err := t.generate(ctx, job)
	if err != nil {
		if _, ferr := t.Store.SetThumbFailed(ctx, job.TenantID, job.BlobID); ferr != nil {
			return fmt.Errorf("%v (mark failed: %w)", err, ferr)
		}
		return err
	}
	ok, err := t.Store.SetThumbReady(ctx, job.TenantID, job.BlobID, thumbID)
	if err != nil {
		return err
	}
	if !ok {
		// The source was deleted or already settled while we generated. The
		// thumbnail row carries the orphan TTL, so no explicit cleanup is needed.
		t.Logger.Debug("thumbnail source moved on; orphan left to TTL",
			"tenant", job.TenantID, "blob", job.BlobID, "thumb", thumbID)
	}
	return nil
}

// generate returns the id of the thumbnail blob it stored.
func (t *Thumbnailer) generate(parent context.Context, job store.ThumbJob) (uuid.UUID, error) {
	if t.Store == nil || t.Reader == nil || t.Uploader == nil {
		return uuid.Nil, errors.New("blob: thumbnailer is not configured")
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	meta, err := t.Store.BlobMetaOf(ctx, job.TenantID, job.BlobID)
	if err != nil {
		return uuid.Nil, err
	}
	if !meta.Complete {
		return uuid.Nil, fmt.Errorf("%w: blob is not complete", store.ErrIncompleteBlob)
	}
	if meta.Size <= 0 || meta.Size > thumbMaxSourceBytes {
		return uuid.Nil, fmt.Errorf("%w: %d bytes", ErrThumbTooLarge, meta.Size)
	}
	if !supportedThumbMime(meta.Mime) {
		return uuid.Nil, fmt.Errorf("%w: %s", ErrThumbUnsupported, meta.Mime)
	}

	// Header first: dimensions are known before a full decode allocates pixels.
	if err := t.checkDimensions(ctx, meta); err != nil {
		return uuid.Nil, err
	}
	img, err := t.decode(ctx, meta)
	if err != nil {
		return uuid.Nil, err
	}
	w, h := thumbnailDims(img.Bounds().Dx(), img.Bounds().Dy())
	if w <= 0 || h <= 0 {
		return uuid.Nil, fmt.Errorf("%w: empty image", ErrThumbUnsupported)
	}
	thumb := resizeBilinear(img, w, h)

	var encoded bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&encoded, thumb); err != nil {
		return uuid.Nil, fmt.Errorf("blob: encode thumbnail: %w", err)
	}
	thumbMeta, created, err := t.Uploader.PutDetailed(ctx, job.TenantID, "image/png", bytes.NewReader(encoded.Bytes()))
	if err != nil {
		return uuid.Nil, err
	}
	if created {
		// The orphan TTL is metadata for a row this generator created. A dedupe
		// hit may be a user's existing blob (same sha256): changing its expiry
		// would silently schedule a user upload for deletion.
		expires := time.Now().Add(thumbTTL)
		if _, err := t.Store.SetBlobExpiry(ctx, job.TenantID, thumbMeta.BlobID, &expires); err != nil {
			// The thumbnail is already stored and ready to be recorded; the expiry
			// is a safety net, so a failure here must not turn a good thumbnail into
			// a failed one.
			t.Logger.Warn("thumbnail expiry mark failed", "tenant", job.TenantID, "thumb", thumbMeta.BlobID, "err", err)
		}
	}
	return thumbMeta.BlobID, nil
}

func (t *Thumbnailer) checkDimensions(ctx context.Context, meta store.BlobMeta) error {
	rc, err := t.open(ctx, meta)
	if err != nil {
		return err
	}
	defer rc.Close()
	cfg, format, err := image.DecodeConfig(io.LimitReader(rc, thumbMaxSourceBytes+1))
	if err != nil {
		return fmt.Errorf("blob: decode image header: %w", err)
	}
	if !supportedThumbFormat(format) {
		return fmt.Errorf("%w: %s", ErrThumbUnsupported, format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Errorf("%w: %dx%d image", ErrThumbUnsupported, cfg.Width, cfg.Height)
	}
	if int64(cfg.Width)*int64(cfg.Height) > thumbMaxPixels {
		return fmt.Errorf("%w: %dx%d exceeds %d pixels", ErrThumbTooLarge, cfg.Width, cfg.Height, thumbMaxPixels)
	}
	return nil
}

func (t *Thumbnailer) decode(ctx context.Context, meta store.BlobMeta) (image.Image, error) {
	rc, err := t.open(ctx, meta)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	img, format, err := image.Decode(io.LimitReader(rc, thumbMaxSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("blob: decode image: %w", err)
	}
	if !supportedThumbFormat(format) {
		return nil, fmt.Errorf("%w: %s", ErrThumbUnsupported, format)
	}
	return img, nil
}

// open adapts the blob reader to an io.ReadCloser through a pipe, so a decode
// never materialises the encoded image in memory. Closing early aborts the
// streaming goroutine.
func (t *Thumbnailer) open(ctx context.Context, meta store.BlobMeta) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	go func() {
		_, err := t.Reader.StreamRaw(ctx, meta.TenantID, meta.BlobID, meta, pw, 0, 0)
		_ = pw.CloseWithError(err)
	}()
	return pr, nil
}

func supportedThumbMime(mime string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0])) {
	case "image/jpeg", "image/png", "image/gif":
		return true
	}
	return false
}

func supportedThumbFormat(format string) bool {
	switch strings.ToLower(format) {
	case "jpeg", "png", "gif":
		return true
	}
	return false
}

// thumbnailDims fits the longest edge to ThumbMaxEdge, never upscaling.
func thumbnailDims(w, h int) (int, int) {
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	if w <= ThumbMaxEdge && h <= ThumbMaxEdge {
		return w, h
	}
	if w >= h {
		nh := int(math.Round(float64(h) * ThumbMaxEdge / float64(w)))
		if nh < 1 {
			nh = 1
		}
		return ThumbMaxEdge, nh
	}
	nw := int(math.Round(float64(w) * ThumbMaxEdge / float64(h)))
	if nw < 1 {
		nw = 1
	}
	return nw, ThumbMaxEdge
}

// resizeBilinear scales src to w×h. Standard library only: image.Image.At gives
// premultiplied 16-bit RGBA, which interpolates correctly for alpha too.
func resizeBilinear(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw == 0 || sh == 0 {
		return dst
	}
	scaleX := float64(sw) / float64(w)
	scaleY := float64(sh) / float64(h)
	for y := 0; y < h; y++ {
		fy := (float64(y)+0.5)*scaleY - 0.5
		y0 := int(math.Floor(fy))
		wy := fy - float64(y0)
		if y0 < 0 {
			y0, wy = 0, 0
		}
		if y0 >= sh-1 {
			y0, wy = sh-1, 0
		}
		y1 := y0 + 1
		if y1 >= sh {
			y1 = sh - 1
		}
		for x := 0; x < w; x++ {
			fx := (float64(x)+0.5)*scaleX - 0.5
			x0 := int(math.Floor(fx))
			wx := fx - float64(x0)
			if x0 < 0 {
				x0, wx = 0, 0
			}
			if x0 >= sw-1 {
				x0, wx = sw-1, 0
			}
			x1 := x0 + 1
			if x1 >= sw {
				x1 = sw - 1
			}
			r00, g00, b00, a00 := src.At(b.Min.X+x0, b.Min.Y+y0).RGBA()
			r10, g10, b10, a10 := src.At(b.Min.X+x1, b.Min.Y+y0).RGBA()
			r01, g01, b01, a01 := src.At(b.Min.X+x0, b.Min.Y+y1).RGBA()
			r11, g11, b11, a11 := src.At(b.Min.X+x1, b.Min.Y+y1).RGBA()
			mix := func(v00, v10, v01, v11 uint32) uint8 {
				top := float64(v00)*(1-wx) + float64(v10)*wx
				bot := float64(v01)*(1-wx) + float64(v11)*wx
				return uint8(math.Round((top*(1-wy) + bot*wy) / 257))
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = mix(r00, r10, r01, r11)
			dst.Pix[i+1] = mix(g00, g10, g01, g11)
			dst.Pix[i+2] = mix(b00, b10, b01, b11)
			dst.Pix[i+3] = mix(a00, a10, a01, a11)
		}
	}
	return dst
}
