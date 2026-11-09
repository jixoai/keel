package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

// errInputExternalSession is the native input path's refusal of an external
// session: the session exists for this tenant, but its state is owned by an
// external backend, so a native user.message would be committed and then never
// picked up by a native run. The kernel enforces the same rule inside the
// session transaction; this is the pre-check that keeps the refusal before any
// write.
var errInputExternalSession = errors.New("session is owned by an external backend; native input is not supported")

// userInput is one client input, validated by its caller and ready for the
// shared commit path. POST /v1/sessions/{id}/input fills it from the request
// body; POST /v1/sessions/{id}/retry fills it from a committed user.message.
type userInput struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	RequestID string
	Text      string
	// Delivery is "" (absent), kernel.DeliveryQueue or kernel.DeliveryGuide.
	// The value is the effective mode: the guide lease probe and the downgrade
	// rule belong to the input endpoint, which decides it before committing.
	Delivery string
	// Selection and Attachments travel verbatim (RawMessage) into the payload:
	// the value the caller sent is the value in the log. A non-object selection
	// never reaches here (the input endpoint vets it first).
	Selection   json.RawMessage
	Attachments json.RawMessage
	// RetryOf, when set, is the user.message seq this input re-asks. It is the
	// retry endpoint's ledger mark; the kernel shape-checks it.
	RetryOf *int64
}

// inputCommit is the outcome of the shared commit path.
type inputCommit struct {
	Submission store.Submission
	Created    bool
}

// commitUserInput is the single write path of client input: the input endpoint
// and the retry endpoint both commit through it, so the payload shape, the
// external-session pre-check, the kernel commit (which queues the run in its
// own transaction) and the submissions bookkeeping cannot drift between them.
//
// It returns the store/kernel error verbatim; writeInputError owns the wire
// mapping.
func (s *Server) commitUserInput(ctx context.Context, in userInput) (inputCommit, error) {
	fields := map[string]any{"text": in.Text, "requestId": in.RequestID}
	if in.Delivery != "" {
		fields["delivery"] = in.Delivery
	}
	if presentRawJSON(in.Selection) {
		// Verbatim: a RawMessage passes the object through the payload marshal
		// without re-encoding its fields, so the value the client sent is the
		// value in the log.
		fields["selection"] = json.RawMessage(in.Selection)
	}
	if presentRawJSON(in.Attachments) {
		fields["attachments"] = json.RawMessage(in.Attachments)
	}
	if in.RetryOf != nil {
		fields["retryOf"] = *in.RetryOf
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return inputCommit{}, err
	}
	if s.cfg.Edge == nil {
		return inputCommit{}, errKernelUnwired
	}
	// Class pre-check before the commit: an external session has no native
	// run/lease flow, so the write is refused instead of appending a message
	// and then failing to queue a run (M4's "commit then 500"). The kernel's
	// edge seam still records messages for external sessions (the external
	// gateway's SSE replay depends on that contract), so the refusal belongs to
	// the native client API, before anything is written.
	owner, exists, err := s.cfg.Store.SessionOwner(ctx, in.TenantID, in.SessionID)
	if err != nil {
		return inputCommit{}, err
	}
	if !exists {
		return inputCommit{}, store.ErrNoSession
	}
	if owner == store.StateOwnerExternal {
		return inputCommit{}, errInputExternalSession
	}
	// Edge-scoped commit: user.message carries the requestId as its idempotency
	// key and never a fencing token (client input keeps landing during a drain;
	// the next runtime picks it up).
	receipt, err := s.cfg.Edge.Commit(ctx, kernel.CommitRequest{
		Scope:      kernel.Scope{TenantID: in.TenantID, SessionID: in.SessionID},
		Kind:       kernel.KindUserMessage,
		RequestID:  in.RequestID,
		Payload:    payload,
		NativeEdge: true,
	})
	if err != nil {
		return inputCommit{}, err
	}
	// The committed event is the truth; the submissions row is client-facing
	// bookkeeping kept for the response contract. A row that predates a crash
	// between commit and bookkeeping is repaired here. The commit itself already
	// queued the run in its own transaction (kernel edge commit), so a failure
	// in this bookkeeping can no longer strand the input.
	sub, created, err := s.cfg.Store.Submit(ctx, in.TenantID, in.RequestID, in.SessionID, uuid.New())
	if err != nil {
		return inputCommit{}, err
	}
	if sub.Seq == nil {
		if err := s.cfg.Store.SetSubmissionStatus(ctx, in.TenantID, in.SessionID, in.RequestID, "committed", receipt.Seq); err != nil {
			return inputCommit{}, err
		}
		sub.Seq = &receipt.Seq
		// The response reports the state the bookkeeping now holds, not the
		// transient "pending" the row was inserted with.
		sub.Status = "committed"
	}
	// Enqueue on replays too: if the first enqueue failed after the event was
	// committed, the client's retry repairs it without creating a second event.
	if err := s.cfg.Store.EnqueueRun(ctx, in.TenantID, in.SessionID); err != nil {
		return inputCommit{}, err
	}
	return inputCommit{Submission: sub, Created: created}, nil
}

// presentRawJSON reports whether a raw JSON fragment carries a value: an absent
// field and the literal null are both "not present", so a caller copying a
// field never invents a null the source did not have.
func presentRawJSON(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

// writeInputError maps the shared input commit path's refusals onto the wire: a
// missing session is 404, an external session is 409, and everything else keeps
// the edge/enqueue mappings (unwired kernel 503, kernel rejections, 500).
func writeInputError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInputExternalSession):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, store.ErrNoSession), errors.Is(err, store.ErrExternalSession):
		writeEnqueueError(w, err)
	default:
		writeEdgeError(w, err)
	}
}
