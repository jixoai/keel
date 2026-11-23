// Package spillblob adapts the tenant blob store to the keel.spill capability.
//
// It is the production implementation of spill.Store: bytes are written through
// the same blob uploader every attachment uses (tenant-scoped rows, content
// dedupe, the shared CAS/layerfs byte directory) and read back through the same
// streaming reader. No second memory store is introduced — the spill reference
// is a blob id, exactly like an image reference.
package spillblob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/spill"
)

// MaxLoadBytes bounds a single Load call. It matches the uploader's default
// cap: a payload the store accepted can always be read back, and an oversized
// reference is refused instead of materializing unbounded memory.
const MaxLoadBytes = 64 << 20

// Blobs implements spill.Store over the tenant blob tables and byte store.
type Blobs struct {
	// Uploader writes the payload. Required.
	Uploader *blob.Uploader
	// Source reads the payload back (metadata + streaming). Required for Load.
	Source *blob.ImageSource
}

var _ spill.Store = (*Blobs)(nil)

// New builds a spill store from the process's blob seams. Either seam may be
// nil: Spill/Load then fail with a named wiring error instead of panicking.
func New(uploader *blob.Uploader, source *blob.ImageSource) *Blobs {
	return &Blobs{Uploader: uploader, Source: source}
}

// Spill writes data for the scope's tenant and returns its durable reference.
// The blob uploader dedupes by (tenant, sha256), so re-spilling the same
// content is a row lookup rather than a second copy.
func (b *Blobs) Spill(ctx context.Context, scope spill.Scope, mime string, data []byte) (spill.Ref, error) {
	if b == nil || b.Uploader == nil {
		return spill.Ref{}, errors.New("spillblob: no blob uploader is wired")
	}
	if len(data) == 0 {
		return spill.Ref{}, errors.New("spillblob: refusing to spill an empty payload")
	}
	tenant, err := tenantID(scope)
	if err != nil {
		return spill.Ref{}, err
	}
	if strings.TrimSpace(mime) == "" {
		mime = "application/octet-stream"
	}
	meta, err := b.Uploader.Put(ctx, tenant, mime, bytes.NewReader(data))
	if err != nil {
		return spill.Ref{}, fmt.Errorf("spillblob: store payload: %w", err)
	}
	ref := spill.Ref{
		ID:     meta.BlobID.String(),
		MIME:   meta.Mime,
		SHA256: hexDigest(meta.SHA256),
		Size:   meta.Size,
	}
	// The uploader computed the digest while spooling; a mismatch would mean the
	// payload changed underneath us. Refuse rather than return a reference whose
	// hash does not describe the bytes the caller passed.
	if ref.SHA256 != spill.Digest(data) {
		return spill.Ref{}, fmt.Errorf("spillblob: stored digest %s does not match the payload", ref.SHA256)
	}
	return ref, nil
}

// Load reads a spilled payload back by reference. It verifies the digest so a
// corrupt or wrong reference is an error, never silently different content.
func (b *Blobs) Load(ctx context.Context, scope spill.Scope, ref spill.Ref) ([]byte, error) {
	if b == nil || b.Source == nil {
		return nil, errors.New("spillblob: no blob source is wired")
	}
	tenant, err := tenantID(scope)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref.ID) == "" {
		return nil, errors.New("spillblob: load needs a blob id")
	}
	info, err := b.Source.Info(ctx, tenant.String(), ref.ID)
	if err != nil {
		return nil, fmt.Errorf("spillblob: read metadata: %w", err)
	}
	if ref.Size > 0 && info.Size != ref.Size {
		return nil, fmt.Errorf("spillblob: reference size %d does not match stored size %d", ref.Size, info.Size)
	}
	if info.Size > MaxLoadBytes {
		return nil, fmt.Errorf("spillblob: stored payload is %d bytes (cap %d)", info.Size, MaxLoadBytes)
	}
	rc, err := b.Source.Open(ctx, tenant.String(), ref.ID)
	if err != nil {
		return nil, fmt.Errorf("spillblob: open payload: %w", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, MaxLoadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("spillblob: read payload: %w", err)
	}
	if int64(len(data)) != info.Size {
		return nil, fmt.Errorf("spillblob: read %d of %d bytes", len(data), info.Size)
	}
	if ref.SHA256 != "" && spill.Digest(data) != ref.SHA256 {
		return nil, fmt.Errorf("spillblob: payload digest does not match reference %s", ref.ID)
	}
	return data, nil
}

// tenantID parses the scope's tenant. A malformed or zero value is refused:
// querying a zero tenant would return "not found" for every payload and hide
// the wiring bug.
func tenantID(scope spill.Scope) (uuid.UUID, error) {
	raw := strings.TrimSpace(scope.TenantID)
	if raw == "" {
		return uuid.Nil, errors.New("spillblob: the scope has no tenant id")
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("spillblob: invalid tenant id %q: %w", raw, err)
	}
	if id == uuid.Nil {
		return uuid.Nil, errors.New("spillblob: the scope's tenant id is zero")
	}
	return id, nil
}

// hexDigest renders a stored sha256 digest.
func hexDigest(sum []byte) string {
	if len(sum) != sha256.Size {
		return ""
	}
	return hex.EncodeToString(sum)
}
