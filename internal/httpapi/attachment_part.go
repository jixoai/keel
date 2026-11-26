package httpapi

// AttachmentPart projection (change keel-plugin-attachment, task 3.1).
//
// The projection is message-scoped, not a bare blob lookup: the display facts
// (name/width/height) come from the committed user.message identified by `seq`,
// while MIME/size/thumbnail lifecycle come from the blob record (the authority).
// `delivery` is attempt-scoped and is not persisted with the message, so this
// endpoint reports it as not evaluated rather than trusting a caller-supplied
// value. A plugin UI never reads blob bytes.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/attachment"
	"github.com/jixoai/keel/internal/store"
)

const attachmentPartNotAttemptScoped = "delivery is decided during an attempt and is not stored with this message"

func (s *Server) attachmentPart(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad session id"))
		return
	}
	fileID, err := uuid.Parse(r.PathValue("fileId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad file id"))
		return
	}
	seq, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("seq")), 10, 64)
	if err != nil || seq <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("seq must name the committed user.message carrying the attachment"))
		return
	}
	events, err := s.cfg.Store.LoadEvents(r.Context(), tenantID, sessionID, seq-1, 1)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(events) != 1 || events[0].Seq != seq {
		writeErr(w, http.StatusNotFound, errors.New("message not found"))
		return
	}
	if events[0].Kind != store.EventUserMessage {
		writeErr(w, http.StatusBadRequest, errors.New("seq must name a user.message"))
		return
	}
	var payload struct {
		Attachments []attachment.Ref `json:"attachments"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		writeErr(w, http.StatusInternalServerError, errors.New("the committed message payload is not readable"))
		return
	}
	var ref attachment.Ref
	found := false
	for _, candidate := range payload.Attachments {
		if candidate.FileID == fileID.String() {
			ref = candidate
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, errors.New("attachment not found in this message"))
		return
	}
	meta, err := s.cfg.Store.BlobMetaOf(r.Context(), tenantID, fileID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNoBlob) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err)
		return
	}
	if !meta.Complete {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "attachment is not complete",
			"fileId": meta.BlobID,
		})
		return
	}
	part := attachment.BuildPart(ref, attachment.BlobFacts{
		Mime:        meta.Mime,
		Size:        meta.Size,
		ThumbStatus: meta.ThumbStatus,
	}, attachment.Delivery{Native: false, Reason: attachmentPartNotAttemptScoped})
	writeJSON(w, http.StatusOK, part)
}
