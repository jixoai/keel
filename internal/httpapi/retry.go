package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// Errors of the retry resolution. The parsing rules are pinned with the client
// contract: the only turn identifier a client can produce is the projection's
// derived turn id, "t_<seq>" (packages/chat-surface/src/projection.ts derives
// it from a turn's first input event, or from the assistant event of a turn
// the projection built without one).
var (
	// errBadTurnID is a malformed turnId (400).
	errBadTurnID = errors.New(`turnId must be the client turn id, "t_<seq>"`)
	// errUnknownTurn is a well-formed turnId whose seq names no committed
	// event of this session (404).
	errUnknownTurn = errors.New("turn not found")
	// errTurnNotUserTurn is a turn that has no user input to re-ask: a
	// maintenance/continuation event, or an assistant answer that records no
	// consumed input (400).
	errTurnNotUserTurn = errors.New("turn is not a user turn")
)

// retryTurn implements POST /v1/sessions/{id}/retry {turnId}: it appends a NEW
// user.message repeating the input an earlier turn answered, plus a run for it.
// The ledger only grows — the original events are never rewritten, and the new
// message carries retryOf: <original input seq> so the relationship is a fact
// of the log.
//
// Retrying is deliberately not idempotent: a retry is an intentional second
// ask, so the same turnId retried twice creates two attempts on purpose. The
// client-supplied requestId/X-Request-ID dedupe of POST /input therefore does
// not apply here — a retry that deduplicated against the original input would
// be a no-op — and the requestId is generated fresh as "retry:<uuid>" per call.
//
// Errors: a malformed turnId is 400, a turn that is not a user turn is 400, an
// unknown turn seq is 404, and a missing or foreign session is 404 (the read
// seam carries the tenant boundary; the shared commit path refuses an external
// session with 409).
func (s *Server) retryTurn(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad session id: %w", err))
		return
	}
	var body struct {
		TurnID string `json:"turnId"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	turnID := strings.TrimSpace(body.TurnID)
	if turnID == "" {
		writeErr(w, 400, errors.New("turnId is required"))
		return
	}
	if s.cfg.Events == nil {
		writeErr(w, http.StatusServiceUnavailable, errKernelUnwired)
		return
	}
	original, inputSeq, err := s.resolveRetryInput(r.Context(), tenantID, sessionID, turnID)
	if err != nil {
		switch {
		case errors.Is(err, errBadTurnID), errors.Is(err, errTurnNotUserTurn):
			writeErr(w, http.StatusBadRequest, err)
		case errors.Is(err, errUnknownTurn):
			writeErr(w, http.StatusNotFound, err)
		default:
			// A foreign or missing session is kernel.ErrNotFound → 404; a
			// transient store failure stays a 500.
			writeEdgeError(w, err)
		}
		return
	}
	// The original input is re-asked with its own text, attachments and
	// selection; delivery is queue, because a retry belongs to the next run —
	// there is no live turn for it to steer.
	var source struct {
		Text        string          `json:"text"`
		Attachments json.RawMessage `json:"attachments"`
		Selection   json.RawMessage `json:"selection"`
	}
	if err := json.Unmarshal(original, &source); err != nil {
		writeErr(w, 500, fmt.Errorf("retry: the committed input payload is not readable: %w", err))
		return
	}
	// The re-ask carries the stored selection verbatim, so it must clear the
	// same gate a fresh input does: a selection the current configuration can
	// no longer resolve is refused here (400 with the reason) instead of being
	// committed and failing asynchronously as a run.
	if len(source.Selection) > 0 {
		if err := s.vetSelection(source.Selection); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("the stored selection can no longer be resolved: %w", err))
			return
		}
	}
	requestID := "retry:" + uuid.NewString()
	retryOf := inputSeq
	commit, err := s.commitUserInput(r.Context(), userInput{
		TenantID:    tenantID,
		SessionID:   sessionID,
		RequestID:   requestID,
		Text:        source.Text,
		Delivery:    kernel.DeliveryQueue,
		Selection:   source.Selection,
		Attachments: source.Attachments,
		RetryOf:     &retryOf,
	})
	if err != nil {
		writeInputError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"created":   commit.Created,
		"seq":       commit.Submission.Seq,
		"requestId": requestID,
	})
}

// resolveRetryInput maps the client's turn identifier onto the committed
// user.message the turn answered, returning that payload and its seq.
//
// Resolution:
//
//   - a user.message at the turn's seq is the turn's own prompt (the turn was
//     created when that input was admitted, or it has no assistant answer yet);
//   - an assistant.message resolves through its inputSeqs — the newest seq
//     recorded there is the input the turn answered;
//   - any other event (compaction, run bookkeeping) is not a user turn.
func (s *Server) resolveRetryInput(ctx context.Context, tenantID, sessionID uuid.UUID, turnID string) (json.RawMessage, int64, error) {
	seq, err := turnSeq(turnID)
	if err != nil {
		return nil, 0, err
	}
	scope := kernel.Scope{TenantID: tenantID, SessionID: sessionID}
	event, found, err := s.loadEventAt(ctx, scope, seq)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, errUnknownTurn
	}
	switch event.Kind {
	case kernel.KindUserMessage:
		return event.Payload, event.Seq, nil
	case kernel.KindAssistantMessage:
		var payload struct {
			InputSeqs []int64 `json:"inputSeqs"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || len(payload.InputSeqs) == 0 {
			return nil, 0, errTurnNotUserTurn
		}
		input, found, err := s.loadEventAt(ctx, scope, payload.InputSeqs[len(payload.InputSeqs)-1])
		if err != nil {
			return nil, 0, err
		}
		if !found || input.Kind != kernel.KindUserMessage {
			return nil, 0, errTurnNotUserTurn
		}
		return input.Payload, input.Seq, nil
	default:
		return nil, 0, errTurnNotUserTurn
	}
}

// turnSeq parses the projection's derived turn id, "t_<seq>". The empty
// suffix, a non-integer and a non-positive seq are all malformed: event
// sequences start at 1.
func turnSeq(turnID string) (int64, error) {
	raw, ok := strings.CutPrefix(turnID, "t_")
	if !ok {
		return 0, errBadTurnID
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seq <= 0 {
		return 0, errBadTurnID
	}
	return seq, nil
}

// loadEventAt reads exactly the committed event at seq. found=false means the
// session exists but has no such event (an empty page); a missing or foreign
// session is the read seam's ErrNotFound, which travels to the caller's 404.
func (s *Server) loadEventAt(ctx context.Context, scope kernel.Scope, seq int64) (kernel.Event, bool, error) {
	events, err := s.cfg.Events.LoadEvents(ctx, scope, seq-1, 1)
	if err != nil {
		return kernel.Event{}, false, err
	}
	if len(events) == 0 || events[0].Seq != seq {
		return kernel.Event{}, false, nil
	}
	return events[0], true, nil
}
