// Package fileplugin is the session-scoped file plugin: it resolves a
// `file_manifest` (path → content hash), drives layerfs for bytes, appends the
// one `fs.write` event per explicit write, and writes through to the materialized
// execution space when one is attached.
//
// Boundaries (spec: tool-surface / layerfs, plugin-boundaries `keel.fs`):
//
//   - layerfs owns bytes and knows nothing about paths; this package owns
//     path → hash and the session scope.
//   - read/write/edit are thin tool interfaces over this service. Version
//     preconditions and conflict detection live here, not in the tool layer:
//     a write to an existing path must carry the hash the model read, and a
//     mismatch is a typed `conflict` the model resolves by reading again.
//   - the single write path is: resolve manifest → layerfs.Put → commit
//     fs.write → manifest upsert → retain (session, eventSeq) → write-through.
//     No other code path appends fs.write.
package fileplugin

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/layerfs"
)

// Typed error codes. They are the contract the tool surface maps onto its own
// structured error document.
const (
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeInvalid         = "invalid_arguments"
	CodeQuotaExceeded   = "quota_exceeded"
	CodeIO              = "io"
	CodeUnavailable     = "unavailable"
	CodeRunScopeMissing = "run_scope_missing"
	CodePluginDisabled  = "plugin_disabled"
)

// Error is the plugin's typed failure. The tool surface maps Code onto its
// structured error; the model can react without parsing prose.
type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Message + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf builds a typed error.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf reports the typed code of err ("" for an untyped error).
func CodeOf(err error) string {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

// ErrPluginDisabled is the sentinel the harnessext backend returns when the
// keel.file plugin is not enabled. The tool surface maps it to plugin_disabled.
var ErrPluginDisabled = errors.New("fileplugin: the keel.file plugin is not enabled")

// Scope is the session scope of one file operation. TaskID is attribution only:
// it never isolates files.
type Scope struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	TaskID    string
}

// Valid reports whether the tenant/session pair is present.
func (s Scope) Valid() bool { return s.TenantID != uuid.Nil && s.SessionID != uuid.Nil }

// Entry is one manifest row.
type Entry struct {
	Path       string
	Hash       layerfs.Hash
	Mode       uint32
	Size       int64
	UpdatedSeq int64
	LastTaskID string
}

// ReadResult is one read page.
type ReadResult struct {
	Path          string `json:"path"`
	Hash          string `json:"hash"`
	Size          int64  `json:"size"`
	Mode          uint32 `json:"mode"`
	Seq           int64  `json:"seq"`
	Offset        int    `json:"offset"`
	Limit         int    `json:"limit"`
	TotalLines    int    `json:"totalLines"`
	ReturnedLines int    `json:"returnedLines"`
	NextOffset    int    `json:"nextOffset"`
	Truncated     bool   `json:"truncated"`
	Content       string `json:"content"`
}

// WriteResult is the fact one write committed.
type WriteResult struct {
	Path     string `json:"path"`
	Hash     string `json:"hash"`
	Size     int64  `json:"size"`
	Mode     uint32 `json:"mode"`
	Seq      int64  `json:"seq"`
	Created  bool   `json:"created"`
	Replayed bool   `json:"replayed,omitempty"`
}

// EditResult is the fact one edit committed.
type EditResult struct {
	Path         string `json:"path"`
	Hash         string `json:"hash"`
	Size         int64  `json:"size"`
	Mode         uint32 `json:"mode"`
	Seq          int64  `json:"seq"`
	Replacements int    `json:"replacements"`
	Replayed     bool   `json:"replayed,omitempty"`
}

// CaptureResult is the fact one batch capture committed.
type CaptureResult struct {
	Tree          string `json:"treeHash"`
	Count         int    `json:"count"`
	Seq           int64  `json:"seq"`
	BaseSeq       int64  `json:"baseSeq"`
	NewObjects    int    `json:"newObjects"`
	ReusedObjects int    `json:"reusedObjects"`
	Bytes         int64  `json:"bytes"`
	// Deleted is how many paths the settlement removed from the manifest (paths
	// a previous capture established, absent from this snapshot).
	Deleted int `json:"deleted"`
	// DeletedTruncated reports that the event payload carries only the first
	// CaptureDeletedPayloadLimit of Deleted paths.
	DeletedTruncated bool `json:"deletedTruncated,omitempty"`
	// SnapshotVersion is the per-session settlement version assigned by this
	// process. Capture settlements are serialized per session, so a larger
	// version is a strictly later view. A replay is not a new settlement and
	// reports 0; the original event carries its version in the payload.
	SnapshotVersion int64 `json:"snapshotVersion"`
	Replayed        bool  `json:"replayed,omitempty"`
}

// --- ambient operation identity -------------------------------------------

type scopeKey struct{}
type callIDKey struct{}

// WithScope attaches the session scope to a tool execution context.
func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// ScopeFromContext reads the ambient scope.
func ScopeFromContext(ctx context.Context) (Scope, bool) {
	scope, ok := ctx.Value(scopeKey{}).(Scope)
	return scope, ok && scope.Valid()
}

// WithCallID attaches the model tool-call id. It is the fs.write idempotency
// identity: a retried call must dedupe against the first event.
func WithCallID(ctx context.Context, callID string) context.Context {
	return context.WithValue(ctx, callIDKey{}, callID)
}

// CallIDFromContext reads the ambient tool-call id.
func CallIDFromContext(ctx context.Context) (string, bool) {
	callID, ok := ctx.Value(callIDKey{}).(string)
	return callID, ok && callID != ""
}
