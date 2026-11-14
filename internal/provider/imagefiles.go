package provider

// Provider file handles and the durable-image resolver (keel-ga §3).
//
// The chain a stored image travels:
//
//	event log:  {"type":"imageRef","blobId":"…"}      (agentcore.ImageRefContent)
//	  ↓ worker, before the outbound request
//	resolver:   blob meta → capability-driven form selection
//	  ↓
//	wire:       file_id | url | base64                (agentcore.ImageContent)
//
// Two rules make this more than a shape conversion:
//
//  1. The degradation order is fixed: file_id > url > base64. A provider file
//     handle is cheapest (the bytes cross the wire once, and again only when the
//     handle expires); a URL keeps the bytes out of the request body entirely;
//     base64 is universal but re-transfers on every turn (FILE-PROTOCOL.md §2.4).
//  2. Memory is bounded by declaration, not by hope. The blob source streams,
//     and the inline path allocates exactly one buffer of the encoded length,
//     which the provider's MaxInlineBytes bounds before any byte is read. An
//     image over the cap degrades to URL/file_id when available and otherwise
//     fails closed with a diagnostic naming the size and the limit — never an
//     OOM in the run path.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// ErrImageTooLarge is returned when an image exceeds the target's declared
// limit and no cheaper form (file_id/url) is available. It is a *diagnosable*
// refusal: the caller logs it as a warn and the run continues without the image
// (the text prompt still reaches the model), which is the documented fail
// semantics for an unusable attachment.
var ErrImageTooLarge = errors.New("provider: image exceeds the declared limit")

// BlobInfo is the metadata the resolver needs before touching bytes.
type BlobInfo struct {
	BlobID string
	SHA256 string
	Size   int64
	Mime   string
}

// BlobSource is the resolver's seam into the platform blob store. It is
// deliberately narrow and primitive-typed: internal/provider must not import
// internal/blob (which pulls the store), and a test supplies an in-memory
// implementation.
type BlobSource interface {
	// Info reads the blob's metadata for one workspace (tenant).
	Info(ctx context.Context, workspace, blobID string) (BlobInfo, error)
	// Open streams the blob's raw bytes. The caller closes it.
	Open(ctx context.Context, workspace, blobID string) (io.ReadCloser, error)
	// EncodeBase64 writes the blob's base64 form into dst, which must be exactly
	// WireLen(size) long. Writing into a caller-sized buffer is what keeps the
	// inline path at one allocation.
	EncodeBase64(ctx context.Context, workspace, blobID string, dst []byte) error
}

// FileRef is a provider-side file handle: the id the vendor's API takes back,
// the URI a vendor that addresses files by URI uses (Gemini's file.uri), and
// when the handle stops being usable (zero = no declared expiry).
type FileRef struct {
	Provider  string
	FileID    string
	URI       string
	ExpiresAt time.Time
}

// FileUploadRequest is one upload: the bytes are streamed (never buffered) so a
// large original costs no resident memory.
type FileUploadRequest struct {
	Workspace string
	Blob      BlobInfo
	Name      string
	Open      func(context.Context) (io.ReadCloser, error)
}

// FileUploader uploads one blob to a vendor and returns its handle.
type FileUploader interface {
	// Provider names the vendor family this uploader serves ("google",
	// "openai", "anthropic").
	Provider() string
	Upload(ctx context.Context, req FileUploadRequest) (FileRef, error)
}

// URLSigner mints a provider-fetchable URL for one image. A deployment that
// cannot expose blobs publicly (no public base URL, no signing key) leaves it
// nil and the resolver degrades past the url form.
type URLSigner interface {
	ImageURL(ctx context.Context, workspace string, blob BlobInfo, ttl time.Duration) (string, error)
}

// ResolvedImage is the resolver's verdict: the wire block plus why that form was
// chosen. Form is always one of the ImageForm* constants; Note is non-empty only
// when the preferred form was unavailable (the caller surfaces it as a warn, so
// a degraded wire is visible in the log rather than silent).
type ResolvedImage struct {
	Content agentcore.ImageContent
	Form    string
	Note    string
}

// ImageTarget is the endpoint + credential of the attempt that is sending the
// image. An upload must use the same account as the request that references the
// handle: a file id uploaded under another key is unusable, and a provider
// account is the unit the cache key must respect.
type ImageTarget struct {
	// Provider is the resolved provider identity ("google", "anthropic", "openai").
	Provider string
	// BaseURL is the endpoint root the attempt is calling (…/v1beta, …/v1).
	BaseURL string
	// APIKey is the attempt's credential. It is never logged or echoed.
	APIKey string
}

// uploaderFor builds the Files API client for this target, or nil when the
// vendor has no upload path this deployment implements (the resolver then
// degrades to url/base64).
func (t ImageTarget) uploaderFor(client *http.Client) FileUploader {
	switch t.Provider {
	case "google":
		if strings.TrimSpace(t.APIKey) == "" {
			return nil
		}
		return NewGeminiFileUploader(t.BaseURL, t.APIKey, client)
	case "anthropic":
		if strings.TrimSpace(t.APIKey) == "" {
			return nil
		}
		return NewAnthropicFileUploader(t.BaseURL, t.APIKey, client)
	case "openai":
		if strings.TrimSpace(t.APIKey) == "" {
			return nil
		}
		return NewOpenAIFileUploader(t.BaseURL, t.APIKey, client)
	default:
		return nil
	}
}

// ImageResolver turns durable references into wire blocks for one target.
type ImageResolver struct {
	// Blobs reads the platform side. Required.
	Blobs BlobSource
	// Files is the provider handle cache. Nil disables the file_id form (the
	// resolver degrades to url/base64).
	Files *FileCache
	// URLs mints public image URLs. Nil disables the url form.
	URLs URLSigner
	// URLTTL is the lifetime of a minted URL (default 10 minutes).
	URLTTL time.Duration
	// HTTPClient is used by the vendor uploaders (nil = http.DefaultClient;
	// tests inject an httptest client).
	HTTPClient *http.Client
	// Now is the clock (nil = time.Now).
	Now func() time.Time
	// uploaderOverride replaces the target-derived uploader. It is the test seam
	// (a fake uploader counts calls and can expire handles on command) and is
	// never set in production.
	uploaderOverride FileUploader
}

// uploaderFor returns the uploader for this target: the test override when set,
// otherwise the vendor-specific Files API client.
func (r *ImageResolver) uploaderFor(target ImageTarget) FileUploader {
	if r.uploaderOverride != nil {
		return r.uploaderOverride
	}
	return target.uploaderFor(r.HTTPClient)
}

func (r *ImageResolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *ImageResolver) urlTTL() time.Duration {
	if r.URLTTL > 0 {
		return r.URLTTL
	}
	return 10 * time.Minute
}

// Resolve turns one durable image reference into the wire block the target
// accepts, applying the fixed degradation order. The second return is nil only
// when a form was produced; a refusal is always an error naming the reason (a
// missing blob, an oversized image, an upload failure with no cheaper form).
//
// caps is the target driver's declaration (ImageCapsOf on the driver, or the
// resolved identity's ImageCaps).
func (r *ImageResolver) Resolve(ctx context.Context, workspace string, target ImageTarget, ref agentcore.ImageRefContent, caps ImageCaps) (ResolvedImage, error) {
	if r == nil || r.Blobs == nil {
		return ResolvedImage{}, errors.New("provider: image resolver has no blob source")
	}
	blobID := strings.TrimSpace(ref.BlobID)
	if blobID == "" {
		return ResolvedImage{}, errors.New("provider: image reference carries no blobId")
	}
	info, err := r.Blobs.Info(ctx, workspace, blobID)
	if err != nil {
		return ResolvedImage{}, fmt.Errorf("provider: image %s: %w", blobID, err)
	}
	if ref.MimeType != "" && info.Mime == "" {
		info.Mime = ref.MimeType
	}
	mime := info.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}

	// A blob over the provider's declared ceiling cannot be sent in any form: the
	// vendor would reject it, so refuse before spending an upload.
	if caps.MaxImageBytes > 0 && info.Size > caps.MaxImageBytes {
		return ResolvedImage{}, fmt.Errorf("%w: image %s is %d bytes, provider %s accepts at most %d",
			ErrImageTooLarge, blobID, info.Size, target.Provider, caps.MaxImageBytes)
	}

	// 1. file_id — the cheapest form. An upload/handle-expiry failure is not fatal
	// on its own: it degrades, with the reason recorded.
	note := ""
	if caps.SupportsFileID && r.Files != nil {
		uploader := r.uploaderFor(target)
		if uploader == nil {
			note = fmt.Sprintf("provider %s declares file support but has no upload path wired", target.Provider)
		} else {
			file, err := r.Files.GetOrUpload(ctx, uploader, workspace, info, ref.Name, r.blobOpener())
			if err != nil {
				note = fmt.Sprintf("provider file upload failed (%v)", err)
			} else {
				return ResolvedImage{
					Content: agentcore.ImageContent{
						Type:     agentcore.ContentTypeImage,
						FileID:   file.FileID,
						MimeType: mime,
					},
					Form: ImageFormFileID,
				}, nil
			}
		}
	}

	// 2. url — keeps the bytes out of the request body.
	if caps.SupportsImageURL && r.URLs != nil {
		url, urlErr := r.URLs.ImageURL(ctx, workspace, info, r.urlTTL())
		if urlErr == nil && strings.TrimSpace(url) != "" {
			content := agentcore.ImageContent{Type: agentcore.ContentTypeImage, URL: url, MimeType: mime}
			if note != "" {
				return ResolvedImage{Content: content, Form: ImageFormURL, Note: note + "; degraded to url"}, nil
			}
			return ResolvedImage{Content: content, Form: ImageFormURL}, nil
		}
		if urlErr != nil && note == "" {
			note = fmt.Sprintf("image url unavailable (%v)", urlErr)
		}
	}

	// 3. base64 — always available, bounded by the declared inline cap.
	limit := caps.inlineLimit()
	if limit > 0 && info.Size > limit {
		reason := fmt.Sprintf("image %s is %d bytes, inline limit for provider %s is %d", blobID, info.Size, target.Provider, limit)
		if note != "" {
			reason = note + "; " + reason
		}
		return ResolvedImage{}, fmt.Errorf("%w: %s", ErrImageTooLarge, reason)
	}
	wire, err := r.encodeBase64(ctx, workspace, blobID, info.Size)
	if err != nil {
		if note != "" {
			return ResolvedImage{}, fmt.Errorf("provider: image %s: %s; base64 fallback failed: %w", blobID, note, err)
		}
		return ResolvedImage{}, fmt.Errorf("provider: image %s: %w", blobID, err)
	}
	return ResolvedImage{
		Content: agentcore.ImageContent{Type: agentcore.ContentTypeImage, Data: wire, MimeType: mime},
		Form:    ImageFormBase64,
		Note:    note,
	}, nil
}

// encodeBase64 allocates exactly the encoded length and lets the blob source fill
// it chunk-wise, so the inline path holds one buffer (the wire bytes) and never a
// second copy of the raw image.
func (r *ImageResolver) encodeBase64(ctx context.Context, workspace, blobID string, size int64) (string, error) {
	if size < 0 {
		return "", fmt.Errorf("blob %s reports a negative size", blobID)
	}
	buf := make([]byte, base64WireLen(size))
	if err := r.Blobs.EncodeBase64(ctx, workspace, blobID, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// blobOpener adapts the blob source into a FileUploadRequest's byte source, so
// uploaders stream from the platform store.
func (r *ImageResolver) blobOpener() func(string, string) (io.ReadCloser, error) {
	return func(workspace, blobID string) (io.ReadCloser, error) {
		return r.Blobs.Open(context.Background(), workspace, blobID)
	}
}

// base64WireLen is 4*ceil(n/3) without importing internal/blob (whose constant
// is the same formula); keeping the formula here avoids a dependency edge from
// the provider layer into the storage layer.
func base64WireLen(size int64) int64 { return 4 * ((size + 2) / 3) }

// ---------------------------------------------------------------------------
// Provider file cache: (sha256, provider, workspace) -> handle, TTL lazy refresh
// ---------------------------------------------------------------------------

// FileCache remembers provider file handles so the same image is uploaded once
// per vendor per workspace. The key is the content digest (not the blob id), so
// two tenants' identical bytes still upload separately (a provider account is
// per-tenant) while a re-uploaded identical file hits the cache.
//
// Expiry is lazy: a handle whose ExpiresAt has passed is re-uploaded the next
// time it is read, never on a timer. That is the Gemini 48h case — a session
// that idles past the window repays one upload, and the session does not fail.
type FileCache struct {
	mu      sync.Mutex
	entries map[string]cachedFile
	// max bounds the map; the oldest-expiring entries are dropped first. 0 means
	// a default of 4096 handles (a few hundred KB of bookkeeping).
	max int
}

type cachedFile struct {
	ref      FileRef
	storedAt time.Time
}

// NewFileCache builds a cache with the given bound (0 = default).
func NewFileCache(max int) *FileCache {
	if max <= 0 {
		max = 4096
	}
	return &FileCache{entries: make(map[string]cachedFile, max/8+1), max: max}
}

// fileKey is (sha256, provider, workspace): the digest identifies the content,
// the provider names whose handle it is, and the workspace scopes the account.
func fileKey(provider, workspace string, blob BlobInfo) string {
	digest := blob.SHA256
	if digest == "" {
		digest = fmt.Sprintf("%s:%d", blob.BlobID, blob.Size)
	}
	sum := sha256.Sum256([]byte(provider + "\x00" + workspace + "\x00" + digest))
	return hex.EncodeToString(sum[:16])
}

// GetOrUpload returns a live handle for the blob, uploading when the cache holds
// nothing usable. open streams the bytes for the upload (workspace, blobID).
func (c *FileCache) GetOrUpload(ctx context.Context, uploader FileUploader, workspace string, blob BlobInfo, name string, open func(workspace, blobID string) (io.ReadCloser, error)) (FileRef, error) {
	if c == nil {
		return FileRef{}, errors.New("provider: file cache is not configured")
	}
	key := fileKey(uploader.Provider(), workspace, blob)
	now := time.Now()

	c.mu.Lock()
	entry, ok := c.entries[key]
	c.mu.Unlock()
	if ok && (entry.ref.ExpiresAt.IsZero() || now.Before(entry.ref.ExpiresAt)) {
		return entry.ref, nil
	}

	req := FileUploadRequest{
		Workspace: workspace,
		Blob:      blob,
		Name:      name,
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			return open(workspace, blob.BlobID)
		},
	}
	ref, err := uploader.Upload(ctx, req)
	if err != nil {
		// A stale entry is not reusable (its handle expired), so it is dropped
		// rather than served: a wrong handle fails the provider call anyway.
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return FileRef{}, err
	}
	ref.Provider = uploader.Provider()
	c.mu.Lock()
	c.evictLocked(now)
	c.entries[key] = cachedFile{ref: ref, storedAt: now}
	c.mu.Unlock()
	return ref, nil
}

// evictLocked drops expired entries and, if still over the bound, the entries
// with the nearest expiry. Called with the lock held.
func (c *FileCache) evictLocked(now time.Time) {
	if len(c.entries) < c.max {
		return
	}
	type candidate struct {
		key string
		at  time.Time
	}
	candidates := make([]candidate, 0, len(c.entries))
	for key, entry := range c.entries {
		if !entry.ref.ExpiresAt.IsZero() && !now.Before(entry.ref.ExpiresAt) {
			delete(c.entries, key)
			continue
		}
		at := entry.ref.ExpiresAt
		if at.IsZero() {
			at = entry.storedAt
		}
		candidates = append(candidates, candidate{key: key, at: at})
	}
	if len(c.entries) < c.max {
		return
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].at.Before(candidates[j].at) })
	drop := len(c.entries) - c.max + 1
	for i := 0; i < drop && i < len(candidates); i++ {
		delete(c.entries, candidates[i].key)
	}
}

// Len reports the cached handle count (diagnostics/tests).
func (c *FileCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
