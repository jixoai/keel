// Package kernel is Keel's contract kernel: the single writer of the session
// event log and the sole issuer of run leases.
//
// The event log is the only truth (AGENTS.md: 无状态是宪法). Everything that
// matters for correctness therefore lives on the write path of that log, and
// this package owns that path:
//
//   - Commit is the only way an event enters the log. It validates the event
//     kind, the tenant/session boundary, the commit class, the idempotency key
//     (requestId / generationId / toolCallId) and the assistant-tool-call ↔
//     tool.result pairing before appending in one transaction.
//   - Claim / Renew / Finish / Release issue and check owner + epoch fencing
//     tokens, so a runtime that was drained or whose lease expired can no
//     longer write.
//   - Reads (LoadEvents, UnresolvedToolCalls, Lookup) carry the tenant boundary
//     and answer ErrNotFound for a foreign session, never leaking existence.
//
// # Single linearization point
//
// The session row is the one place where claims and appends serialize: it
// carries the lease authority (owner / owner_epoch / lease_until) and its lock
// is the same lock sequence allocation takes. Every owner-scoped commit
// re-reads the epoch under that lock, so a claim cannot slip between validation
// and append. All transactions take locks in the fixed order sessions -> runs;
// the runs columns of the same names are only a mirror for legacy queue/status
// readers.
//
// # Commit classes
//
// Owner-scoped kinds (assistant.message, tool.result, compaction, run lifecycle)
// must present the fence of the current claim. Edge-scoped kinds (user.message)
// are client input: they are checked against the tenant/session boundary, the
// edge whitelist and their idempotency key, but never against the lease — the
// client keeps writing during a drain and the next runtime picks the input up.
// An edge endpoint therefore must accept only EdgeKinds() and answer 404 for a
// foreign session (a MODIFIED API recorded by the change).
//
// The kernel is not a plugin and cannot be replaced by one: services receive
// only the narrow interfaces defined here (Committer, Reader, LeaseManager),
// never a table handle. See README.md for the full boundary.
package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Scope is the tenant/session boundary of every kernel operation. Session
// identity is the pair: the same session UUID under two tenants is two
// different, mutually invisible sessions.
type Scope struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
}

// IsZero reports whether either id is missing.
func (s Scope) IsZero() bool {
	return s.TenantID == uuid.Nil || s.SessionID == uuid.Nil
}

func (s Scope) String() string {
	return s.TenantID.String() + "/" + s.SessionID.String()
}

// EventKind is one registered event kind. The string values are the durable
// contract shared with internal/store and every log consumer; the registry in
// validate.go pins them against the store constants.
type EventKind string

// Registered event kinds. These are exactly the kinds the log may contain; an
// unknown kind is rejected at commit time.
const (
	KindUserMessage      EventKind = "user.message"
	KindAssistantMessage EventKind = "assistant.message"
	KindToolResult       EventKind = "tool.result"
	KindCompaction       EventKind = "compaction"
	KindRunStarted       EventKind = "run.started"
	KindRunFailed        EventKind = "run.failed"
	// KindRunFinished is the attempt-level run summary (status, turns, tokens,
	// tool timings, stop reason). The worker writes it under the run's lease
	// just before the settle; it is bookkeeping and never enters the transcript.
	KindRunFinished EventKind = "run.finished"
	// KindExternalEvent is Keel's own record of one external-backend fact: a
	// message the gateway handed to an AgentBackend, or an event the backend
	// pushed back. It is the compatibility log of an external session and it
	// is committed under ClassExternal - no lease, but only into a session
	// marked state_owner=external. The native SSE endpoint replays it like any
	// other Keel-recorded event (external-gateway spec: "SSE 只回放 Keel 记录的
	// 事件"), and the payload carries external_backend/state_owner marks so a
	// UI can tell it apart from a native assistant message.
	KindExternalEvent EventKind = "external.event"
)

// Run status values for the runs row (mirrored from internal/store).
const (
	RunStatusPending = "pending"
	RunStatusRunning = "running"
	RunStatusDone    = "done"
	RunStatusFailed  = "failed"
)

// OwnerToken is the fencing token issued by Claim. Owner identifies the runtime
// instance; Epoch increases monotonically on every claim of the session, so a
// token from an older claim can never be mistaken for the current one.
type OwnerToken struct {
	Owner string
	Epoch int64
}

// IsZero reports whether the token is absent.
func (t OwnerToken) IsZero() bool { return t.Owner == "" && t.Epoch == 0 }

func (t OwnerToken) String() string {
	return fmt.Sprintf("%s@%d", t.Owner, t.Epoch)
}

// Lease is a held claim: the token, the session it covers and its expiry.
type Lease struct {
	Scope Scope
	Token OwnerToken
	Until time.Time
	// PendingInput reports that the run row carries unread edge input: input
	// committed after the previous attempt's read point. A claim only delivers
	// it (the flag is cleared by a finish that proves the input was read), so
	// an abandoned attempt leaves the fact visible to the next claim. A run
	// whose PendingInput is true owes a model turn even when the log's last
	// message is an answer to older input.
	PendingInput bool
	// ReadWatermark is the previous attempt's load high-water mark: input with
	// a higher seq was never part of that attempt's transcript. The runner
	// presents exactly that input to the model last, so an answer produced by
	// the previous attempt cannot make the new input look "already answered"
	// (adversarial review B5). Zero means no attempt has proven a read point.
	ReadWatermark int64
	// Reclaimed is true when this claim took over a run row that was still
	// marked running (its previous lease had expired). It mirrors
	// store.Run.Reclaimed so the worker's lease-expiry metric keeps its exact
	// meaning; it carries no fencing authority.
	Reclaimed bool
}

// Live reports whether the lease is unexpired as of now.
func (l Lease) Live(now time.Time) bool { return l.Until.After(now) }

// LeaseState is the fencing state of one session, as answered by the store's
// clock: Found means a lease record exists (it may be expired); Live means it
// has not expired.
//
// Found=false still carries the last holder in Lease (owner and epoch) so an
// audit can answer "who held it last"; the token there grants nothing. Callers
// must branch on Found/Live first — checkToken does, and it treats a missing
// record as lease-lost before it ever compares tokens.
type LeaseState struct {
	Lease Lease
	Found bool
	Live  bool
}

// Event is one committed fact. Payload is the stored JSON document.
type Event struct {
	Seq     int64
	Kind    EventKind
	Payload json.RawMessage
}

// ToolCallRef identifies one tool call carried by a committed assistant
// message. Seq is the sequence of that assistant message.
type ToolCallRef struct {
	ToolCallID string
	ToolName   string
	Seq        int64
}

// Receipt is what a commit (or a lookup) answers: the sequence the fact was
// committed at, its kind, and whether an existing fact was reused instead of
// appending a second event.
type Receipt struct {
	Seq     int64
	Kind    EventKind
	Deduped bool
}

// KeyKind names one idempotency space. Each space is namespaced in the stored
// dedupe key, so the same literal value in two spaces cannot collide.
type KeyKind string

const (
	// KeyRequest is the client-supplied requestId (user.message).
	KeyRequest KeyKind = "requestId"
	// KeyGeneration is the model-generation id (assistant.message).
	KeyGeneration KeyKind = "generationId"
	// KeyToolCall is the tool call id (tool.result).
	KeyToolCall KeyKind = "toolCallId"
)

// KeyRef is one idempotency key value.
type KeyRef struct {
	Kind  KeyKind
	Value string
}

// IsZero reports whether the key is absent.
func (k KeyRef) IsZero() bool { return k.Value == "" }

func (k KeyRef) String() string { return string(k.Kind) + "=" + k.Value }

// dedupeKey returns the stored key. The namespaces are part of the durable
// format: "tool:" is already used by internal/agent's writer, so pre-existing
// logs dedupe against kernel commits unchanged.
func (k KeyRef) dedupeKey() string {
	if k.IsZero() {
		return ""
	}
	var prefix string
	switch k.Kind {
	case KeyRequest:
		prefix = "req:"
	case KeyGeneration:
		prefix = "gen:"
	case KeyToolCall:
		prefix = "tool:"
	default:
		prefix = string(k.Kind) + ":"
	}
	return prefix + k.Value
}

// CommitClass is the commit category of a kind: the contract's three-lane
// model. The edge lane (client input) and the external lane (gateway records)
// both carry no lease, but they are disjoint: only the edge whitelist may be
// written by a client, and the external lane may only be written into a session
// whose state_owner is external.
type CommitClass string

const (
	// ClassOwner marks run-scoped kinds: assistant.message, tool.result,
	// compaction and the run lifecycle events. They are written by the runtime
	// executing the run and must carry the fence (owner + epoch) of the current
	// claim; a stale token is rejected.
	ClassOwner CommitClass = "owner-scoped"
	// ClassEdge marks client input (user.message). It is checked against the
	// tenant/session boundary, the edge whitelist and its own idempotency key,
	// but never against the lease: during a drain the client keeps writing and
	// the next runtime picks the input up.
	ClassEdge CommitClass = "edge-scoped"
	// ClassExternal marks Keel's compatibility log of an external session
	// (external.event). It carries no lease, but it is not client-writable: the
	// commit is accepted only when the target session is marked
	// state_owner=external and is not closed. This is how a backend event
	// reaches Keel's SSE without the backend ever touching the ledger: the
	// runtime commits what it received, tagged with the session's external
	// marks, and SSE replays the Keel-recorded fact.
	ClassExternal CommitClass = "external-scoped"
)

// CommitRequest is a proposal to append exactly one event.
//
// Scope and Kind are required. Token is required for owner-scoped kinds
// (assistant.message, tool.result, compaction, run.started, run.failed) and
// carries the fencing token returned by Claim; edge-scoped and external-scoped
// kinds must not present one. Exactly one idempotency key is required per kind:
// requestId for user.message, generationId for assistant.message, toolCallId for
// tool.result, and requestId again for external.event (the gateway's per-event
// identity, e.g. "<backend>:<externalRef>:<seq>"). Bookkeeping kinds carry no
// key. Payload is the JSON document to store; for tool.result it must contain
// the same toolCallId as the request, and for assistant.message every toolCall
// block must have a non-empty, unique id. external.event needs a JSON object
// with non-empty "backend" and "externalRef" strings (the provenance marks).
type CommitRequest struct {
	Scope   Scope
	Kind    EventKind
	Token   OwnerToken
	Payload json.RawMessage

	// NativeEdge marks an edge-scoped write that comes from the native client
	// API (/v1/sessions/{id}/input, /events) rather than from an internal
	// edge-seam writer (the external gateway). A native edge write to a session
	// marked state_owner=external is refused under the session lock, before
	// anything is appended: an external session has no native run/lease flow,
	// so the input would be committed and then never picked up. Internal edge
	// writers leave it false: the external gateway's own replay contract relies
	// on the edge seam still recording messages for external sessions. It is
	// only valid on ClassEdge kinds.
	NativeEdge bool

	// Idempotency keys. See KeyKind for which kind requires which key.
	RequestID    string
	GenerationID string
	ToolCallID   string
}

// FinishRequest settles a claimed run. Cursor is the last sequence the run
// consumed or produced; Err is the run's failure, if any; MaxAttempts is the
// retry ceiling the store's finish policy uses.
//
// ReadThrough is the load high-water mark: the highest committed sequence the
// run's transcript was built from. Input committed after it (a user.message
// with a larger seq) was NOT seen by this run and forces the row back to
// pending — the cursor alone cannot tell, because the run's own commits push
// it past input that arrived mid-run. Zero (or a value above Cursor) falls
// back to Cursor, the legacy behavior.
type FinishRequest struct {
	Lease       Lease
	Cursor      int64
	ReadThrough int64
	Err         error
	MaxAttempts int
	// Cancelled marks an attempt a caller stopped on purpose. It is a terminal
	// outcome: the run does not re-queue (a cancel that immediately re-runs the
	// same input is a no-op in disguise), while the unanswered input stays in
	// the log and is answered by the next run a later input starts. Err is
	// still set (context.Canceled) so logs and metrics see the aborted attempt.
	Cancelled bool
	// Loadout, when set, records the service loadout the run pinned. It is
	// written to the run row in the same transaction that settles the run, so
	// an audit can answer "which revision/generation did this run finish
	// under" without trusting process memory; OwnerEpoch is always filled from
	// the settled lease token.
	Loadout RunLoadout
}

// RunLoadout is the service-loadout identity a run pinned: the immutable
// snapshot header plus the graph generation that served it, and the fencing
// epoch the run finished with. It is run metadata, never a routing input.
type RunLoadout struct {
	ID         string
	Revision   int64
	Generation int64
	OwnerEpoch int64
}

// probeKnown reports whether the caller supplied a usable read-through
// watermark. Without one the finish cannot tell which input the attempt read,
// so the edge-written pending_input flag is used instead of guessing from the
// cursor.
func (r FinishRequest) probeKnown() bool {
	return r.ReadThrough > 0 && r.ReadThrough <= r.Cursor
}

// probeAfter is the sequence the "did new input arrive?" probe reads from.
func (r FinishRequest) probeAfter() int64 {
	if !r.probeKnown() {
		return r.Cursor
	}
	return r.ReadThrough
}

// Reason classifies a kernel rejection. It is the vocabulary of the audit
// trail; errors.Is works against the sentinel errors below.
type Reason string

const (
	ReasonNotFound           Reason = "not-found"
	ReasonUnknownKind        Reason = "unknown-kind"
	ReasonInvalidRequest     Reason = "invalid-request"
	ReasonKeyKindMismatch    Reason = "key-kind-mismatch"
	ReasonMissingToken       Reason = "missing-token"
	ReasonLeaseLost          Reason = "lease-lost"
	ReasonLeaseExpired       Reason = "lease-expired"
	ReasonStaleEpoch         Reason = "stale-epoch"
	ReasonLeaseHeld          Reason = "lease-held"
	ReasonNoRunnable         Reason = "no-runnable"
	ReasonOrphanToolResult   Reason = "orphan-tool-result"
	ReasonToolResultMismatch Reason = "tool-result-mismatch"
	ReasonDuplicateToolCall  Reason = "duplicate-tool-call"
	ReasonQuota              Reason = "quota"
	ReasonStoreFailure       Reason = "store-failure"
	// ReasonKeyConflict: an idempotency key that already names a committed
	// fact was replayed with different content. A retry of the same request is
	// deduplication; a different request under the same key is a client bug
	// and must not silently receive the stale receipt.
	ReasonKeyConflict Reason = "key-conflict"
	// ReasonExternalConflict: the session cannot be used as an external one
	// (it is a native session with history), or a native run/lease operation
	// targeted an external session.
	ReasonExternalConflict Reason = "external-conflict"
	// ReasonNotExternal: an external-scoped commit targeted a session whose
	// state_owner is not external (or whose external_status is closed). The
	// backend compatibility log never accepts a native session.
	ReasonNotExternal Reason = "not-external"
	// ReasonExternalSession: a native edge commit targeted a session owned by
	// an external backend. It is refused under the session row lock, before the
	// append, so the external gateway's "record but never wake" contract stays
	// intact while the native client API cannot strand an input.
	ReasonExternalSession Reason = "external-session"
)

// Sentinel errors. Every rejection wraps one of these, so callers branch with
// errors.Is instead of string matching.
var (
	// ErrNotFound: the (tenant, session) pair does not exist. A session owned
	// by another tenant answers exactly the same way, leaking no existence.
	ErrNotFound = errors.New("kernel: not found")
	// ErrUnknownKind: the event kind is not registered.
	ErrUnknownKind = errors.New("kernel: unknown event kind")
	// ErrInvalidRequest: malformed request (missing scope, missing or extra
	// idempotency key, payload that cannot back the pairing contract).
	ErrInvalidRequest = errors.New("kernel: invalid request")
	// ErrKeyKindMismatch: an idempotency key was supplied for a kind that does
	// not carry it (or the wrong key kind was supplied).
	ErrKeyKindMismatch = errors.New("kernel: idempotency key does not belong to this event kind")
	// ErrMissingToken: a run-scoped kind was committed without a fencing token.
	ErrMissingToken = errors.New("kernel: missing owner token")
	// ErrLeaseLost: no lease record exists for the session (released, finished
	// or never claimed).
	ErrLeaseLost = errors.New("kernel: lease lost")
	// ErrLeaseExpired: the lease record exists but has expired.
	ErrLeaseExpired = errors.New("kernel: lease expired")
	// ErrStaleEpoch: the token belongs to an older claim (owner or epoch
	// mismatch). This is the handoff rejection.
	ErrStaleEpoch = errors.New("kernel: stale owner token")
	// ErrLeaseHeld: a live lease is held by another owner (claim conflict).
	ErrLeaseHeld = errors.New("kernel: lease held by another owner")
	// ErrNoRunnable: nothing claimable right now.
	ErrNoRunnable = errors.New("kernel: no claimable run")
	// ErrOrphanToolResult: a tool.result references a toolCallId that no
	// committed assistant message carries.
	ErrOrphanToolResult = errors.New("kernel: orphan tool result")
	// ErrToolResultMismatch: the result does not match the call it is attached
	// to (different tool name), i.e. it was hung on the wrong call.
	ErrToolResultMismatch = errors.New("kernel: tool result does not match its call")
	// ErrDuplicateToolCall: one assistant message declares the same toolCallId
	// twice.
	ErrDuplicateToolCall = errors.New("kernel: duplicate tool call id")
	// ErrQuota: the admission seam rejected the operation.
	ErrQuota = errors.New("kernel: quota rejected")
	// ErrKeyConflict: a retry under an idempotency key carried different
	// content than the committed fact it names.
	ErrKeyConflict = errors.New("kernel: idempotency key already names a different fact")
	// ErrNotExternalSession: an external-scoped commit targeted a session that
	// is not an open external session. The distinction is structural: the
	// external lane may only extend a session the backend owns.
	ErrNotExternalSession = errors.New("kernel: session is not an open external session")
	// ErrExternalSession: a native edge write targeted a session owned by an
	// external backend; the input would never be picked up by a native run.
	ErrExternalSession = errors.New("kernel: session is owned by an external backend")
	// ErrStoreFailure: the persistence layer failed; nothing was appended.
	ErrStoreFailure = errors.New("kernel: store failure")
)

// retryableError marks a transient transaction failure (deadlock, serialization
// failure, lock unavailable) that the caller may replay with the same request:
// lease operations are conditional and the kernel's idempotency keys make a
// repeated write a no-op. Adapters wrap such failures with markRetryable; the
// kernel replays them with backoff before surfacing a store failure.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

// markRetryable marks err as a transient failure. It is a no-op for nil.
func markRetryable(err error) error {
	if err == nil {
		return nil
	}
	return &retryableError{err: err}
}

// isRetryableTx reports whether err (or anything it wraps) is a transient
// transaction failure.
func isRetryableTx(err error) bool {
	var r *retryableError
	return errors.As(err, &r)
}

// retryBudget is how many times one kernel write is attempted before the store
// failure is surfaced, mirroring internal/store's retry count.
const retryBudget = 6

// retryBackoff is the wait before attempt n (0-based).
func retryBackoff(attempt int) time.Duration {
	return time.Duration(20+attempt*25) * time.Millisecond
}

// waitRetry sleeps before the next attempt, honoring cancellation.
func waitRetry(ctx context.Context, attempt int) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(retryBackoff(attempt)):
		return nil
	}
}

// Operation names for audit records.
const (
	OpCommit  = "commit"
	OpClaim   = "claim"
	OpRenew   = "renew"
	OpFinish  = "finish"
	OpRelease = "release"
	OpRead    = "read"
	OpLookup  = "lookup"
	// OpExternal names the external-session mark operations.
	OpExternal = "external"
	// OpPin names the loadout pin write of a claim (handoff audit material).
	OpPin = "pin"
)

// Error is the typed kernel rejection. It wraps one of the sentinels above, so
// errors.Is(err, kernel.ErrStaleEpoch) works while the audit trail keeps every
// detail (who, what, which epoch).
type Error struct {
	Reason Reason
	Op     string
	Scope  Scope
	Kind   EventKind
	Token  OwnerToken
	Detail string
	Err    error
}

func (e *Error) Error() string {
	parts := "kernel: " + e.Op
	if e.Kind != "" {
		parts += " " + string(e.Kind)
	}
	parts += " (" + e.Scope.String() + "): " + string(e.Reason)
	if e.Token != (OwnerToken{}) {
		parts += " [token " + e.Token.String() + "]"
	}
	if e.Detail != "" {
		parts += ": " + e.Detail
	}
	return parts
}

// Unwrap exposes the sentinel (and any storage error) to errors.Is/As.
func (e *Error) Unwrap() error { return e.Err }

// Violation is one audited rejection. Auditing is a log/observability concern:
// it never changes control flow and the response to the caller carries no more
// information than the error.
type Violation struct {
	Reason Reason
	Op     string
	Scope  Scope
	Kind   EventKind
	Token  OwnerToken
	Detail string
}

// AuditFunc observes rejections. It must not block.
type AuditFunc func(Violation)

// Limits is the admission seam consulted on the claim and commit paths. The
// kernel owns no quota policy: the default loadout supplies per-tenant limits
// here (concurrency, event rate), and the edge keeps enforcing stream and
// bandwidth limits. A nil Limits means "admit everything".
type Limits interface {
	// AdmitRun gates claiming a run for the scope. A nil error reserves the
	// admission slot; a non-nil error must leave no reservation behind.
	AdmitRun(ctx context.Context, scope Scope) error
	// ReleaseRun returns the slot reserved by a successful AdmitRun when the
	// attempt ends (Finish or Release), whatever the outcome. The kernel calls
	// it at most once per settled claim; implementations must clamp at zero so
	// a rejected attempt can never underflow the counter.
	ReleaseRun(ctx context.Context, scope Scope)
	// AdmitEvent gates appending an event of this kind to the scope.
	AdmitEvent(ctx context.Context, scope Scope, kind EventKind) error
}

// Options configures a Kernel. The zero value is a working kernel with no
// audit sink, no commit observer and no extra limits.
type Options struct {
	Audit  AuditFunc
	Limits Limits
	// OnCommit, when set, observes the kind of every event that entered the
	// log (deduplicated replays are not reported). It replaces the store's
	// event observer on the production write path: metrics that used to be fed
	// by store.AppendEvent now come from here. It must not block.
	OnCommit func(EventKind)
}

// Kernel is the facade over the kernel's persistence seam. It is safe for
// concurrent use; all serialization happens in the store (per session for
// appends, per run row for leases).
type Kernel struct {
	log      LogStore
	audit    AuditFunc
	limits   Limits
	onCommit func(EventKind)
}

// New builds a kernel over a LogStore.
func New(log LogStore, opts Options) *Kernel {
	return &Kernel{log: log, audit: opts.Audit, limits: opts.Limits, onCommit: opts.OnCommit}
}

// Commit validates and appends one event, replaying the whole transaction on
// transient failures (deadlock / serialization / lock unavailable). The replay
// is safe because the request carries its idempotency key: a repeated attempt
// either appends once or returns the already committed fact.
func (k *Kernel) Commit(ctx context.Context, req CommitRequest) (Receipt, error) {
	var lastErr error
	for attempt := 0; attempt < retryBudget; attempt++ {
		receipt, err := k.commitOnce(ctx, req)
		if err == nil {
			return receipt, nil
		}
		if !isRetryableTx(err) {
			return Receipt{}, err
		}
		lastErr = err
		if waitErr := waitRetry(ctx, attempt); waitErr != nil {
			lastErr = fmt.Errorf("%v: %w", lastErr, waitErr)
			break
		}
	}
	k.auditViolation(Violation{Reason: ReasonStoreFailure, Op: OpCommit, Scope: req.Scope, Kind: req.Kind, Token: req.Token, Detail: lastErr.Error()})
	return Receipt{}, &Error{Reason: ReasonStoreFailure, Op: OpCommit, Scope: req.Scope, Kind: req.Kind, Token: req.Token, Detail: lastErr.Error(), Err: fmt.Errorf("%w: %w", ErrStoreFailure, lastErr)}
}

// commitOnce is one attempt of Commit: the validation order is deliberate — the
// request shape (kind, commit class, idempotency key), then, inside the session
// transaction that locks the linearization point, the tenant/session existence,
// the fencing token, the idempotency lookup, the pairing contract, and only
// then the append. Fencing comes before idempotency so a stale writer always
// learns it is stale instead of receiving a silent success for a fact it had
// already written.
func (k *Kernel) commitOnce(ctx context.Context, req CommitRequest) (Receipt, error) {
	spec, ok := kindRegistry[req.Kind]
	if !ok {
		return Receipt{}, k.reject(ReasonUnknownKind, OpCommit, req.Scope, req.Kind, req.Token, fmt.Sprintf("kind %q is not registered", req.Kind), ErrUnknownKind)
	}
	if req.Scope.IsZero() {
		return Receipt{}, k.reject(ReasonInvalidRequest, OpCommit, req.Scope, req.Kind, req.Token, "tenant and session are required", ErrInvalidRequest)
	}
	key, err := spec.keyFor(req)
	if err != nil {
		return Receipt{}, k.reject(ReasonKeyKindMismatch, OpCommit, req.Scope, req.Kind, req.Token, err.Error(), ErrKeyKindMismatch)
	}
	if spec.class == ClassOwner && req.Token.IsZero() {
		return Receipt{}, k.reject(ReasonMissingToken, OpCommit, req.Scope, req.Kind, req.Token, "owner-scoped kinds need the fencing token returned by Claim", ErrMissingToken)
	}
	if spec.class == ClassEdge && !req.Token.IsZero() {
		return Receipt{}, k.reject(ReasonInvalidRequest, OpCommit, req.Scope, req.Kind, req.Token, "edge-scoped kinds do not take an owner token", ErrInvalidRequest)
	}
	if spec.class == ClassExternal && !req.Token.IsZero() {
		return Receipt{}, k.reject(ReasonInvalidRequest, OpCommit, req.Scope, req.Kind, req.Token, "external-scoped kinds do not take an owner token", ErrInvalidRequest)
	}
	// NativeEdge is a native-client marker: it is meaningless (and rejected) on
	// owner-scoped and external-scoped kinds; the gateway's external-class
	// commits never set it.
	if req.NativeEdge && spec.class != ClassEdge {
		return Receipt{}, k.reject(ReasonInvalidRequest, OpCommit, req.Scope, req.Kind, req.Token, "NativeEdge is only valid for edge-scoped kinds", ErrInvalidRequest)
	}
	if k.limits != nil {
		if err := k.limits.AdmitEvent(ctx, req.Scope, req.Kind); err != nil {
			return Receipt{}, k.reject(ReasonQuota, OpCommit, req.Scope, req.Kind, req.Token, err.Error(), ErrQuota)
		}
	}
	// Decode the payload facts the pairing contract relies on. A payload that
	// cannot back that contract is rejected before anything is written. The
	// edge kinds are validated too: user.message is the one kind a client can
	// write, and a payload the transcript rebuild cannot decode would poison
	// the session (the event stays in the log, so every later rebuild fails).
	var call *ToolCallRef
	switch req.Kind {
	case KindUserMessage:
		if err := validateUserMessagePayload(req.Payload); err != nil {
			return Receipt{}, k.reject(ReasonInvalidRequest, OpCommit, req.Scope, req.Kind, req.Token, err.Error(), ErrInvalidRequest)
		}
	case KindToolResult:
		ref, err := validateToolResultPayload(req)
		if err != nil {
			return Receipt{}, k.reject(ReasonInvalidRequest, OpCommit, req.Scope, req.Kind, req.Token, err.Error(), ErrInvalidRequest)
		}
		call = &ref
	case KindAssistantMessage:
		if err := validateAssistantPayload(req.Payload); err != nil {
			reason := ReasonInvalidRequest
			sentinel := ErrInvalidRequest
			if errors.Is(err, ErrDuplicateToolCall) {
				reason, sentinel = ReasonDuplicateToolCall, ErrDuplicateToolCall
			}
			return Receipt{}, k.reject(reason, OpCommit, req.Scope, req.Kind, req.Token, err.Error(), sentinel)
		}
	case KindRunFinished:
		if err := validateRunFinishedPayload(req.Payload); err != nil {
			return Receipt{}, k.reject(ReasonInvalidRequest, OpCommit, req.Scope, req.Kind, req.Token, err.Error(), ErrInvalidRequest)
		}
	case KindExternalEvent:
		if err := validateExternalEventPayload(req.Payload); err != nil {
			return Receipt{}, k.reject(ReasonInvalidRequest, OpCommit, req.Scope, req.Kind, req.Token, err.Error(), ErrInvalidRequest)
		}
	}

	txn, err := k.log.BeginSession(ctx, req.Scope.TenantID, req.Scope.SessionID)
	if err != nil {
		return Receipt{}, k.classifyStoreError(OpCommit, req, err)
	}
	defer txn.Rollback(ctx)

	// The session class is read inside the session transaction, under the row
	// lock BeginSession took. The two lanes are mirror images of each other:
	//   - a native edge commit (NativeEdge) may not append to an external
	//     session (see the ClassEdge branch below);
	//   - an external-scoped commit may only extend a session the backend owns
	//     (see the ClassExternal branch below).
	if spec.class == ClassOwner {
		state, err := txn.Lease(ctx)
		if err != nil {
			return Receipt{}, k.classifyStoreError(OpCommit, req, err)
		}
		if err := checkToken(state, req.Token); err != nil {
			return Receipt{}, k.classifyStoreError(OpCommit, req, err)
		}
	}
	if spec.class == ClassExternal {
		// The external lane is only for sessions the backend owns. This is the
		// structural refusal of "an external event dressed up as a native
		// session": the marks are read under the same session lock the append
		// takes, so a concurrent close either precedes this commit (refusing
		// it) or follows it.
		state, err := txn.ExternalState(ctx)
		if err != nil {
			return Receipt{}, k.classifyStoreError(OpCommit, req, err)
		}
		if state.StateOwner != StateOwnerExternal {
			return Receipt{}, k.reject(ReasonNotExternal, OpCommit, req.Scope, req.Kind, req.Token,
				fmt.Sprintf("state_owner is %q; external.event is only accepted for an external session", state.StateOwner), ErrNotExternalSession)
		}
		if state.Status == ExternalStatusClosed {
			return Receipt{}, k.reject(ReasonNotExternal, OpCommit, req.Scope, req.Kind, req.Token,
				"the external session is closed", ErrNotExternalSession)
		}
	}
	if spec.class == ClassEdge && req.NativeEdge {
		// A native message for an external session would be committed and then
		// never picked up by a native run. The check is inside the locked
		// transaction (not only in the HTTP pre-check), so a promotion that
		// committed before this read is visible, and one that commits after
		// blocks on the same row: neither ordering can land a native
		// user.message on a session that is becoming external.
		state, err := txn.ExternalState(ctx)
		if err != nil {
			return Receipt{}, k.classifyStoreError(OpCommit, req, err)
		}
		if state.StateOwner == StateOwnerExternal {
			return Receipt{}, k.reject(ReasonExternalSession, OpCommit, req.Scope, req.Kind, req.Token,
				"native edge input is not accepted for an external session", ErrExternalSession)
		}
	}

	if !key.IsZero() {
		if existing, found, err := txn.Dedupe(ctx, key.dedupeKey()); err != nil {
			return Receipt{}, k.classifyStoreError(OpCommit, req, err)
		} else if found {
			// Same key + same content is a retry: return the committed fact.
			// Same key + different content is a client bug: answering with the
			// old receipt would silently drop the new content.
			if !samePayload(existing.Payload, req.Payload) {
				return Receipt{}, k.reject(ReasonKeyConflict, OpCommit, req.Scope, req.Kind, req.Token,
					fmt.Sprintf("idempotency key %s already names a committed %s with different content", key.dedupeKey(), existing.Kind),
					ErrKeyConflict)
			}
			return Receipt{Seq: existing.Seq, Kind: existing.Kind, Deduped: true}, nil
		}
	}

	if call != nil {
		ref, found, err := findToolCall(ctx, txn, call.ToolCallID)
		if err != nil {
			return Receipt{}, k.classifyStoreError(OpCommit, req, err)
		}
		if !found {
			return Receipt{}, k.reject(ReasonOrphanToolResult, OpCommit, req.Scope, req.Kind, req.Token,
				fmt.Sprintf("no committed tool call with id %q", call.ToolCallID), ErrOrphanToolResult)
		}
		if call.ToolName != "" && ref.ToolName != "" && call.ToolName != ref.ToolName {
			return Receipt{}, k.reject(ReasonToolResultMismatch, OpCommit, req.Scope, req.Kind, req.Token,
				fmt.Sprintf("call %q is named %q but the result is named %q", call.ToolCallID, ref.ToolName, call.ToolName), ErrToolResultMismatch)
		}
	}

	seq, err := txn.Append(ctx, req.Kind, req.Payload, key.dedupeKey())
	if err != nil {
		return Receipt{}, k.classifyStoreError(OpCommit, req, err)
	}
	if err := txn.Commit(ctx); err != nil {
		return Receipt{}, k.classifyStoreError(OpCommit, req, err)
	}
	if k.onCommit != nil {
		k.onCommit(req.Kind)
	}
	return Receipt{Seq: seq, Kind: req.Kind}, nil
}

// Lookup answers whether an idempotency key already has a committed fact,
// without writing and without taking the session write lock. The worker uses
// it before calling the model: a generation that is already committed is
// reused instead of re-requested.
//
// A key that is not found answers (Receipt{}, false, nil); a session that does
// not exist (including another tenant's) answers ErrNotFound.
func (k *Kernel) Lookup(ctx context.Context, scope Scope, key KeyRef) (Receipt, bool, error) {
	if scope.IsZero() {
		return Receipt{}, false, &Error{Reason: ReasonInvalidRequest, Op: OpLookup, Scope: scope, Detail: "tenant and session are required", Err: ErrInvalidRequest}
	}
	events, found, err := k.log.FindDedupe(ctx, scope.TenantID, scope.SessionID, key.dedupeKey())
	if err != nil {
		return Receipt{}, false, k.classifyStoreError(OpLookup, CommitRequest{Scope: scope}, err)
	}
	if found {
		return Receipt{Seq: events.Seq, Kind: events.Kind, Deduped: true}, true, nil
	}
	exists, err := k.log.SessionExists(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		return Receipt{}, false, k.classifyStoreError(OpLookup, CommitRequest{Scope: scope}, err)
	}
	if !exists {
		return Receipt{}, false, &Error{Reason: ReasonNotFound, Op: OpLookup, Scope: scope, Err: ErrNotFound}
	}
	return Receipt{}, false, nil
}

// checkToken maps a lease state and a presented token onto the fencing
// decision. It is shared by Commit (via CommitRequest) and the lease verbs.
func checkToken(state LeaseState, token OwnerToken) error {
	if !state.Found {
		return ErrLeaseLost
	}
	if state.Lease.Token != token {
		return ErrStaleEpoch
	}
	if !state.Live {
		return ErrLeaseExpired
	}
	return nil
}

// reject builds and audits a rejection.
func (k *Kernel) reject(reason Reason, op string, scope Scope, kind EventKind, token OwnerToken, detail string, sentinel error) *Error {
	k.auditViolation(Violation{Reason: reason, Op: op, Scope: scope, Kind: kind, Token: token, Detail: detail})
	msg := detail
	if msg == "" {
		msg = string(reason)
	}
	return &Error{Reason: reason, Op: op, Scope: scope, Kind: kind, Token: token, Detail: detail, Err: fmt.Errorf("%w: %s", sentinel, msg)}
}

func (k *Kernel) auditViolation(v Violation) {
	if k.audit == nil {
		return
	}
	switch v.Reason {
	case ReasonNoRunnable, ReasonLeaseHeld:
		// Idle polling and claim conflicts are routine; do not flood the audit.
		return
	}
	if v.Op == OpRead && v.Reason == ReasonNotFound {
		// 404 on a read is a normal answer, not a violation.
		return
	}
	k.audit(v)
}

// classifyStoreError turns a store error into a kernel rejection, mapping the
// sentinels the seam is allowed to return.
func (k *Kernel) classifyStoreError(op string, req CommitRequest, err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return k.reject(ReasonNotFound, op, req.Scope, req.Kind, req.Token, "", ErrNotFound)
	case errors.Is(err, ErrExternalSession):
		return k.reject(ReasonExternalSession, op, req.Scope, req.Kind, req.Token, "", ErrExternalSession)
	case errors.Is(err, ErrLeaseLost):
		return k.reject(ReasonLeaseLost, op, req.Scope, req.Kind, req.Token, "", ErrLeaseLost)
	case errors.Is(err, ErrLeaseExpired):
		return k.reject(ReasonLeaseExpired, op, req.Scope, req.Kind, req.Token, "", ErrLeaseExpired)
	case errors.Is(err, ErrStaleEpoch):
		return k.reject(ReasonStaleEpoch, op, req.Scope, req.Kind, req.Token, "", ErrStaleEpoch)
	case errors.Is(err, ErrLeaseHeld):
		return k.reject(ReasonLeaseHeld, op, req.Scope, req.Kind, req.Token, "", ErrLeaseHeld)
	case errors.Is(err, ErrNoRunnable):
		return k.reject(ReasonNoRunnable, op, req.Scope, req.Kind, req.Token, "", ErrNoRunnable)
	default:
		// A transient failure keeps its marker (and skips the audit) so the
		// commit path can replay it; the marker surfaces as one audited store
		// failure only when the retry budget is spent.
		if !isRetryableTx(err) {
			k.auditViolation(Violation{Reason: ReasonStoreFailure, Op: op, Scope: req.Scope, Kind: req.Kind, Token: req.Token, Detail: err.Error()})
		}
		return &Error{Reason: ReasonStoreFailure, Op: op, Scope: req.Scope, Kind: req.Kind, Token: req.Token, Detail: err.Error(), Err: fmt.Errorf("%w: %w", ErrStoreFailure, err)}
	}
}
