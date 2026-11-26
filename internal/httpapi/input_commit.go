package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/attachment"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/logsafe"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/sessionbinding"
	"github.com/jixoai/keel/internal/store"
)

// ErrInputExternalSession is the native input path's refusal of an external
// session: the session exists for this tenant, but its state is owned by an
// external backend, so a native user.message would be committed and then never
// picked up by a native run. The kernel enforces the same rule inside the
// session transaction; this is the pre-check that keeps the refusal before any
// write.
//
// Exported because the product API's MCP app bridge commits through the same
// path (CommitClientInput) and must map the refusal onto its own wire.
var ErrInputExternalSession = errors.New("session is owned by an external backend; native input is not supported")

// ErrInputArchivedSession refuses native input to a soft-deleted (archived)
// session. The archive is a read-only state: the ledger stays, but continuing
// the work needs a new session. The check runs before any write, so a refused
// input leaves no event.
var ErrInputArchivedSession = errors.New("session is archived; it is read-only")

// DefaultMaxUpload is the deployment default for one stored object. The input
// commit path re-checks an attachment against it so a blob stored through a
// different route (or before a lowered setting) cannot bypass the bound.
const DefaultMaxUpload int64 = 64 << 20

// ClientInputOptions are the deployment facts the shared commit path needs
// beyond the request itself. The zero value keeps the defaults (64 MiB, no
// quota enforcer), which is what a test harness without a product edge wants.
type ClientInputOptions struct {
	// MaxUpload bounds one attachment's stored size.
	MaxUpload int64
	// Quota measures the tenant's stored bytes before a reference is accepted.
	// Nil skips the check; the upload middleware is still the primary gate.
	Quota *quota.Edge
}

// ClientInput is one client input, validated by its caller and ready for the
// shared commit path. POST /v1/sessions/{id}/input fills it from the request
// body; POST /v1/sessions/{id}/retry fills it from a committed user.message;
// the MCP app bridge fills it from a confirmed ui/message.
type ClientInput struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	RequestID string
	Text      string
	// Delivery is "" (absent), kernel.DeliveryQueue or kernel.DeliveryGuide.
	// The value is the effective mode: the guide lease probe and the downgrade
	// rule belong to the input endpoint, which decides it before committing.
	Delivery string
	// Selection travels verbatim (RawMessage) into the payload: the value the
	// caller sent is the value in the log. Attachments are parsed and
	// canonicalized by the shared commit path, so only the blob's authoritative
	// facts are persisted. A non-object selection never reaches here (the input
	// endpoint vets it first).
	Selection   json.RawMessage
	Attachments json.RawMessage
	// RetryOf, when set, is the user.message seq this input re-asks. It is the
	// retry endpoint's ledger mark; the kernel shape-checks it.
	RetryOf *int64
}

// ClientInputCommit is the outcome of the shared commit path.
type ClientInputCommit struct {
	Submission store.Submission
	Created    bool
}

// userInput and inputCommit are the internal names of the exported shapes, kept
// so the existing callers read unchanged.
type userInput = ClientInput
type inputCommit = ClientInputCommit

// commitUserInput is the single write path of client input: the input endpoint
// and the retry endpoint both commit through it, so the payload shape, the
// external-session pre-check, the kernel commit (which queues the run in its
// own transaction) and the submissions bookkeeping cannot drift between them.
//
// It returns the store/kernel error verbatim; writeInputError owns the wire
// mapping.
func (s *Server) commitUserInput(ctx context.Context, in userInput) (inputCommit, error) {
	return CommitClientInput(ctx, s.cfg.Store, s.cfg.Edge, in, ClientInputOptions{
		MaxUpload: s.cfg.MaxUpload,
		Quota:     s.cfg.Quota,
	})
}

// CommitClientInput is the package-level form of the shared commit path. The
// product API's MCP app bridge uses it so a confirmed ui/message becomes a
// normal user.message through exactly the same validation, commit and
// bookkeeping as POST /v1/sessions/{id}/input.
func CommitClientInput(ctx context.Context, st *store.Store, edge kernel.Committer, in ClientInput, opts ClientInputOptions) (ClientInputCommit, error) {
	if edge == nil {
		return ClientInputCommit{}, errKernelUnwired
	}
	if st == nil {
		return ClientInputCommit{}, errors.New("store is not wired for the input commit path")
	}
	// Class pre-check before the commit: an external session has no native
	// run/lease flow, so the write is refused instead of appending a message
	// and then failing to queue a run (M4's "commit then 500"). The kernel's
	// edge seam still records messages for external sessions (the external
	// gateway's SSE replay depends on that contract), so the refusal belongs to
	// the native client API, before anything is written.
	owner, exists, err := st.SessionOwner(ctx, in.TenantID, in.SessionID)
	if err != nil {
		return ClientInputCommit{}, err
	}
	if !exists {
		return ClientInputCommit{}, store.ErrNoSession
	}
	if active, err := st.SessionActive(ctx, in.TenantID, in.SessionID); err != nil {
		return ClientInputCommit{}, err
	} else if !active {
		// The session row exists but is archived: a write is a state-machine
		// violation (A1: archived -> running is illegal), not a missing row.
		return ClientInputCommit{}, ErrInputArchivedSession
	}
	if owner == store.StateOwnerExternal {
		return ClientInputCommit{}, ErrInputExternalSession
	}
	// Attachments are validated before the ledger write: ownership, completion,
	// MIME consistency and size. The returned value is the canonical stored
	// shape (blob size/MIME become the authority; unknown client fields drop).
	attachments, err := validateInputAttachments(ctx, st, in.TenantID, in.Attachments, opts)
	if err != nil {
		return ClientInputCommit{}, err
	}
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
	if len(attachments) > 0 {
		fields["attachments"] = attachments
	}
	if in.RetryOf != nil {
		fields["retryOf"] = *in.RetryOf
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return ClientInputCommit{}, err
	}
	// Edge-scoped commit: user.message carries the requestId as its idempotency
	// key and never a fencing token (client input keeps landing during a drain;
	// the next runtime picks it up).
	receipt, err := edge.Commit(ctx, kernel.CommitRequest{
		Scope:      kernel.Scope{TenantID: in.TenantID, SessionID: in.SessionID},
		Kind:       kernel.KindUserMessage,
		RequestID:  in.RequestID,
		Payload:    payload,
		NativeEdge: true,
	})
	if err != nil {
		return ClientInputCommit{}, err
	}
	// The event is the truth; this projection is what the registry's delete/
	// archive guard reads. Writing it after the commit means a crash in between
	// loses only the projection (a retry repairs it; an operator can rebuild it
	// from the ledger), never the selection fact. A failure here fails the
	// request too — the client's idempotent retry re-runs the same write — so a
	// committed binding never silently goes unindexed.
	if err := sessionbinding.New(st).Record(ctx, in.TenantID, in.SessionID, payload, receipt.Seq, receipt.Deduped); err != nil {
		return ClientInputCommit{}, err
	}
	// The committed event is the truth; the submissions row is client-facing
	// bookkeeping kept for the response contract. A row that predates a crash
	// between commit and bookkeeping is repaired here. The commit itself already
	// queued the run in its own transaction (kernel edge commit), so a failure
	// in this bookkeeping can no longer strand the input.
	sub, created, err := st.Submit(ctx, in.TenantID, in.RequestID, in.SessionID, uuid.New())
	if err != nil {
		return ClientInputCommit{}, err
	}
	if sub.Seq == nil {
		if err := st.SetSubmissionStatus(ctx, in.TenantID, in.SessionID, in.RequestID, "committed", receipt.Seq); err != nil {
			return ClientInputCommit{}, err
		}
		sub.Seq = &receipt.Seq
		// The response reports the state the bookkeeping now holds, not the
		// transient "pending" the row was inserted with.
		sub.Status = "committed"
	}
	// Enqueue on replays too: if the first enqueue failed after the event was
	// committed, the client's retry repairs it without creating a second event.
	if err := st.EnqueueRun(ctx, in.TenantID, in.SessionID); err != nil {
		return ClientInputCommit{}, err
	}
	return ClientInputCommit{Submission: sub, Created: created}, nil
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
	// Attachment refusals carry their own status and field path: 400 for a
	// malformed/foreign/incomplete reference, 413 for size or storage quota.
	var attachmentErr *attachment.FieldError
	if errors.As(err, &attachmentErr) {
		writeJSON(w, attachmentErr.HTTPStatus(), map[string]any{
			"error": logsafe.Redact(err.Error()),
			"field": attachmentErr.InputField(),
		})
		return
	}
	if errors.Is(err, quota.ErrStorageQuota) {
		// The request references stored bytes the tenant may no longer hold; the
		// message names the ceiling. It is a 413, the same class as an oversized
		// upload.
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	if errors.Is(err, quota.ErrConfig) {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	switch {
	case errors.Is(err, ErrInputExternalSession), errors.Is(err, ErrInputArchivedSession):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, store.ErrNoSession), errors.Is(err, store.ErrExternalSession):
		writeEnqueueError(w, err)
	default:
		writeEdgeError(w, err)
	}
}

// requireWritableSession refuses a native write to a missing (404) or archived
// (409) session. The input commit path has its own pre-check; the run and
// event edges use this one so an archived session cannot be resumed through a
// side door.
func (s *Server) requireWritableSession(w http.ResponseWriter, r *http.Request, tenantID, sessionID uuid.UUID) bool {
	if s.cfg.Store == nil {
		return true
	}
	if _, exists, err := s.cfg.Store.SessionOwner(r.Context(), tenantID, sessionID); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	} else if !exists {
		writeErr(w, http.StatusNotFound, store.ErrNoSession)
		return false
	}
	active, err := s.cfg.Store.SessionActive(r.Context(), tenantID, sessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	if !active {
		writeErr(w, http.StatusConflict, ErrInputArchivedSession)
		return false
	}
	return true
}
