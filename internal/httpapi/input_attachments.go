package httpapi

// Input-time attachment validation (change keel-plugin-attachment).
//
// The attachment lifecycle is owned by internal/blob and internal/agent; this
// file is the admission check the shared commit path performs before the first
// ledger row exists. It answers three questions a client cannot be trusted
// with: does the referenced blob belong to this tenant and is it complete, does
// the declared MIME agree with the stored record, and is the object inside the
// deployment's size/storage bounds. Anything that fails leaves no event.
//
// The one tolerated degradation is a reference without a fileId: it stays in
// the payload and becomes an explicit notice when the transcript is rebuilt
// (internal/agent/messages.go), so the model knows the attachment exists.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/attachment"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/store"
)

// validateInputAttachments parses the client array, validates every reference
// that names a blob, and returns the canonical stored JSON. A nil result means
// "no attachment field".
func validateInputAttachments(ctx context.Context, st *store.Store, tenantID uuid.UUID, raw json.RawMessage, opts ClientInputOptions) (json.RawMessage, error) {
	refs, err := attachment.ParseRefs(raw)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}
	maxUpload := opts.MaxUpload
	if maxUpload <= 0 {
		maxUpload = DefaultMaxUpload
	}
	referenced := false
	for index := range refs {
		ref := &refs[index]
		if ref.FileID == "" {
			// The existing notice semantics: display-only metadata without bytes
			// is not an error and must not disappear.
			continue
		}
		referenced = true
		blobID := uuid.MustParse(ref.FileID)
		meta, err := st.BlobMetaOf(ctx, tenantID, blobID)
		if errors.Is(err, store.ErrNoBlob) || (err == nil && !meta.Complete) {
			return nil, &attachment.FieldError{
				Field:   fmt.Sprintf("attachments[%d].fileId", index),
				Message: "fileId does not name a complete file of this tenant",
			}
		}
		if err != nil {
			return nil, err
		}
		if meta.Size > maxUpload {
			return nil, &attachment.FieldError{
				Field:   fmt.Sprintf("attachments[%d].size", index),
				Status:  413,
				Message: fmt.Sprintf("file is %d bytes; the upload limit is %d bytes", meta.Size, maxUpload),
			}
		}
		declaredMime := attachment.NormalizeMime(ref.Mime)
		storedMime := attachment.NormalizeMime(meta.Mime)
		if declaredMime != "" && declaredMime != storedMime {
			// The blob record is the authority: a client cannot widen its MIME
			// into an image/document lane the bytes never belonged to.
			return nil, &attachment.FieldError{
				Field:   fmt.Sprintf("attachments[%d].mime", index),
				Message: fmt.Sprintf("declared MIME %q does not match the stored file (%q)", declaredMime, storedMime),
			}
		}
		// Canonicalize to the stored facts: the plugin projection and the model
		// dispatch both read these fields, so neither may carry a client guess.
		ref.Mime = storedMime
		ref.Size = meta.Size
	}
	if referenced && opts.Quota != nil {
		// The bytes were counted when the blob completed. Re-checking the tenant
		// against the ceiling (rather than charging the size again) catches a
		// lowered quota without double-counting the same object. The refusal is
		// field-level at the array, where the client can point at its input.
		if _, _, err := opts.Quota.CheckStored(ctx, tenantID); err != nil {
			if errors.Is(err, quota.ErrStorageQuota) {
				return nil, &attachment.FieldError{
					Field: "attachments", Status: 413, Message: err.Error(),
				}
			}
			return nil, err
		}
	}
	return attachment.MarshalRefs(refs)
}
