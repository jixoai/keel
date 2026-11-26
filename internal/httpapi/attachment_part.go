package httpapi

// AttachmentPart projection (change keel-plugin-attachment, task 3.1).
//
// A plugin UI never reads blob bytes; it receives this read-only document and
// draws a preview from it. MIME/size/thumbnail lifecycle come from the blob
// record (the authority); name/width/height are display facts the event payload
// carries; delivery describes the last attempt's transport decision. The shape
// carries no bytes, base64, CAS path, tenant header or credential.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/attachment"
	"github.com/jixoai/keel/internal/store"
)

const (
	attachmentPartReasonMax = 400
	attachmentPartNameMax   = attachment.NameMaxBytes
)

func (s *Server) attachmentPart(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("X-Tenant-ID is required"))
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad file id"))
		return
	}
	meta, err := s.cfg.Store.BlobMetaOf(r.Context(), tenantID, id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNoBlob) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err)
		return
	}
	if !meta.Complete {
		// A projection of an in-flight object would let a preview claim bytes
		// that do not exist yet; the input path refuses the same state with a
		// field error.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "attachment is not complete",
			"fileId": meta.BlobID,
		})
		return
	}
	query := r.URL.Query()
	name := strings.TrimSpace(query.Get("name"))
	if len(name) > attachmentPartNameMax {
		writeErr(w, http.StatusBadRequest, errors.New("name must be at most 256 bytes"))
		return
	}
	width, ok := attachmentPartDimension(w, query.Get("width"), "width")
	if !ok {
		return
	}
	height, ok := attachmentPartDimension(w, query.Get("height"), "height")
	if !ok {
		return
	}
	native := false
	if raw := strings.TrimSpace(query.Get("native")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("native must be a boolean"))
			return
		}
		native = parsed
	}
	reason := strings.TrimSpace(query.Get("reason"))
	if len(reason) > attachmentPartReasonMax {
		writeErr(w, http.StatusBadRequest, errors.New("reason must be at most 400 bytes"))
		return
	}
	ref := attachment.Ref{
		FileID: meta.BlobID.String(),
		Name:   name,
		Mime:   meta.Mime,
		Size:   meta.Size,
		Width:  width,
		Height: height,
	}
	part := attachment.BuildPart(ref, attachment.BlobFacts{
		Mime:        meta.Mime,
		Size:        meta.Size,
		ThumbStatus: meta.ThumbStatus,
	}, attachment.Delivery{Native: native, Reason: reason})
	writeJSON(w, http.StatusOK, part)
}

func attachmentPartDimension(w http.ResponseWriter, raw, field string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		writeErr(w, http.StatusBadRequest, errors.New(field+" must be a non-negative integer"))
		return 0, false
	}
	return value, true
}
