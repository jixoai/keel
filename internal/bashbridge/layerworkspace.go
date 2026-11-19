package bashbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/layerfs"
	"github.com/jixoai/keel/internal/layerfs/gbashfs"
)

// DefaultCaptureTimeout bounds the settle step that runs when a sandbox
// session closes. Capture is part of the run's terminal facts, so it gets its
// own deadline instead of the run context (which may already be canceled).
const DefaultCaptureTimeout = 30 * time.Second

// SessionIdentity is the keel session one sandbox execution belongs to. The
// bash aggregate tool installs it on the tool-call context (it is the only
// place that sees both the run scope and the task-gate-stamped taskId), and the
// workspace provider keys its per-session workspace by it.
type SessionIdentity struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	// TaskID is the attribution card the task gate stamped into the call
	// arguments. Empty outside a run (the plugin UI bridge).
	TaskID string
	// CallID is the model tool-call id; it is the idempotency key of the
	// fs.capture event the run's close commits.
	CallID string
	// JobID is the harness background-job id this execution belongs to (the
	// JobManager stamps it before the job's session is created). Empty means the
	// execution is a run's tool call: its close settles on the owner lane with
	// the live run fence. Non-empty means a background job that may outlive the
	// run, so its close settles on the system lane with jobId attribution and no
	// owner token.
	JobID string
}

// Valid reports whether the identity names a session.
func (i SessionIdentity) Valid() bool {
	return i.TenantID != uuid.Nil && i.SessionID != uuid.Nil
}

type sessionIdentityKey struct{}

// WithSessionIdentity attaches the run's session identity to a context.
func WithSessionIdentity(ctx context.Context, identity SessionIdentity) context.Context {
	return context.WithValue(ctx, sessionIdentityKey{}, identity)
}

// SessionIdentityFromContext reads the ambient session identity.
func SessionIdentityFromContext(ctx context.Context) (SessionIdentity, bool) {
	identity, ok := ctx.Value(sessionIdentityKey{}).(SessionIdentity)
	return identity, ok && identity.Valid()
}

// LayerWorkspaceOptions configures the production workspace provider.
type LayerWorkspaceOptions struct {
	// Store is the layerfs byte layer. Required.
	Store *layerfs.Store
	// Service is the file plugin that owns the session manifest and the single
	// fs.capture commit path. It is a getter because the provider is wired
	// before the service (the service's session surface is this provider).
	Service func() *fileplugin.Service
	// Point is the sandbox mount path. Empty uses DefaultMountPoint.
	Point string
	// CaptureTimeout bounds one settle. Non-positive uses DefaultCaptureTimeout.
	CaptureTimeout time.Duration
	// Logger is used for capture failures and per-session seed facts.
	Logger *slog.Logger
}

// LayerWorkspaces is the production Workspace: one gbashfs workspace per keel
// session, mounted at a non-root path over the process-wide layerfs store.
//
// The same instance is the file plugin's execution space: a write tool call
// writes through into the session's workspace after its fs.write commits, so a
// later bash call reads the same bytes, and a bash write lands in the same
// workspace index the next capture reads. Writes from bash enter the CAS when
// the file handle closes; capture turns the index into tree objects and one
// fs.capture event (fileplugin.CaptureWorkspace for a run's tool call,
// fileplugin.CaptureWorkspaceSystem for a background job with a jobId).
//
// Sessions are independent: their workspaces (and therefore their path views)
// are keyed by (tenant, session). The map lives for the process lifetime; a
// session's workspace is never reused by another session.
type LayerWorkspaces struct {
	store          *layerfs.Store
	service        func() *fileplugin.Service
	point          string
	captureTimeout time.Duration
	logger         *slog.Logger
	fallback       Workspace

	// fallbackWarn keeps the no-session-identity warning to once per provider
	// instance: the fallback is a property of the deployment (UI bridge / direct
	// bridge use), not of each command, so a per-mount warn would be a log storm.
	fallbackWarn sync.Once

	mu       sync.Mutex
	sessions map[sessionKey]*gbashfs.Workspace
}

var (
	_ Workspace                 = (*LayerWorkspaces)(nil)
	_ fileplugin.SessionSurface = (*LayerWorkspaces)(nil)
	_ fileplugin.SnapshotSource = (*LayerWorkspaces)(nil)
)

type sessionKey struct {
	tenantID  uuid.UUID
	sessionID uuid.UUID
}

// NewLayerWorkspaces builds the provider. It does not touch the database.
func NewLayerWorkspaces(opts LayerWorkspaceOptions) (*LayerWorkspaces, error) {
	if opts.Store == nil {
		return nil, errors.New("bashbridge: a layerfs store is required for the layer workspace")
	}
	point, err := gbashfs.ValidateMountPoint(opts.Point)
	if err != nil {
		return nil, err
	}
	timeout := opts.CaptureTimeout
	if timeout <= 0 {
		timeout = DefaultCaptureTimeout
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &LayerWorkspaces{
		store:          opts.Store,
		service:        opts.Service,
		point:          point,
		captureTimeout: timeout,
		logger:         logger,
		fallback:       TempDirWorkspace{},
		sessions:       make(map[sessionKey]*gbashfs.Workspace),
	}, nil
}

// Mount implements Workspace. A context without a session identity (the plugin
// UI bridge, direct bridge use in tests) is an explicit degradation, not a
// silent one: the execution gets a throwaway temp directory, nothing it writes
// is captured or persisted, and a Warn is emitted once per provider instance
// naming the reason. The model tool path always carries the identity; this
// fallback only covers callers with no run scope.
func (l *LayerWorkspaces) Mount(ctx context.Context) (Mount, error) {
	identity, ok := SessionIdentityFromContext(ctx)
	if !ok {
		l.fallbackWarn.Do(func() {
			l.logger.Warn("bash workspace fallback: execution is not persisted",
				"reason", "the context carries no keel session identity (plugin UI bridge or direct bridge use)",
				"workspace", "throwaway temp dir, no capture")
		})
		return l.fallback.Mount(ctx)
	}
	workspace, err := l.workspace(ctx, identity.TenantID, identity.SessionID)
	if err != nil {
		return Mount{}, err
	}
	point := l.point
	return Mount{
		Point: point,
		FS:    workspace,
		Finish: func() error {
			return l.capture(ctx, identity)
		},
	}, nil
}

// WriteFileFor implements fileplugin.SessionSurface: a committed file-tool
// write is written through to the session's workspace before the tool returns,
// so the next bash call reads it without a remount.
func (l *LayerWorkspaces) WriteFileFor(ctx context.Context, scope fileplugin.Scope, path string, mode uint32, r io.Reader) error {
	if err := scopeFailure(scope); err != nil {
		return err
	}
	workspace, err := l.workspace(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		return err
	}
	// The path is session-relative (NormalizePath); anchor it at the workspace
	// root so a bash `cd` cannot change where the tool's write lands.
	absolute := "/" + strings.TrimPrefix(path, "/")
	return workspace.WriteFile(absolute, mode, r)
}

// Snapshot implements fileplugin.SnapshotSource: the capture snapshot is the
// session workspace's index, not a host directory walk.
func (l *LayerWorkspaces) Snapshot(ctx context.Context, scope fileplugin.Scope) (layerfs.Snapshot, error) {
	if err := scopeFailure(scope); err != nil {
		return layerfs.Snapshot{}, err
	}
	workspace, err := l.workspace(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		return layerfs.Snapshot{}, err
	}
	return workspace.Capture(ctx)
}

func scopeFailure(scope fileplugin.Scope) error {
	if !scope.Valid() {
		return fileplugin.Errorf(fileplugin.CodeRunScopeMissing, "the layer workspace needs the ambient session scope")
	}
	return nil
}

// workspace returns the session's workspace, creating and seeding it from the
// file manifest on first use. Seeds are the session's path index: without them
// bash would start from an empty sandbox while read/write/edit still see the
// manifest.
func (l *LayerWorkspaces) workspace(ctx context.Context, tenantID, sessionID uuid.UUID) (*gbashfs.Workspace, error) {
	key := sessionKey{tenantID: tenantID, sessionID: sessionID}
	l.mu.Lock()
	defer l.mu.Unlock()
	if workspace := l.sessions[key]; workspace != nil {
		return workspace, nil
	}
	workspace, err := gbashfs.NewWorkspace(l.store)
	if err != nil {
		return nil, fmt.Errorf("bashbridge: session workspace: %w", err)
	}
	entries, err := l.manifestEntries(ctx, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		if err := workspace.Seed(entries); err != nil {
			return nil, fmt.Errorf("bashbridge: seed session workspace: %w", err)
		}
	}
	l.sessions[key] = workspace
	l.logger.Debug("session workspace mounted", "tenant_id", tenantID, "session_id", sessionID, "files", len(entries))
	return workspace, nil
}

func (l *LayerWorkspaces) manifestEntries(ctx context.Context, tenantID, sessionID uuid.UUID) ([]layerfs.FileEntry, error) {
	service := l.serviceValue()
	if service == nil {
		return nil, errors.New("bashbridge: the file service is not wired")
	}
	entries, err := service.List(ctx, fileplugin.Scope{TenantID: tenantID, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("bashbridge: read session manifest: %w", err)
	}
	files := make([]layerfs.FileEntry, len(entries))
	for index, entry := range entries {
		files[index] = layerfs.FileEntry{Path: entry.Path, Hash: entry.Hash, Mode: entry.Mode, Size: entry.Size}
	}
	return files, nil
}

// capture settles the execution's workspace changes as exactly one fs.capture
// (or one fs.capture_failed on the failure path, committed by the service). It
// uses a fresh deadline because the run context may already be canceled
// (timeout, kill) while the workspace still holds the run's writes.
//
// The settlement lane follows the identity: a run's tool call is fenced by the
// live run lease (owner lane), while a harness background job outlives the run
// and settles as a system fact with jobId attribution (the lease may already be
// gone, so requiring it would lose the job's writes forever).
func (l *LayerWorkspaces) capture(ctx context.Context, identity SessionIdentity) error {
	service := l.serviceValue()
	if service == nil {
		return errors.New("bashbridge: the file service is not wired; the run's workspace changes were not captured")
	}
	callID := strings.TrimSpace(identity.CallID)
	if callID == "" {
		// No model tool-call id (a direct bridge call): one id per session close
		// keeps the event unique without inventing a replay identity.
		callID = "bash:" + uuid.NewString()
	}
	jobID := strings.TrimSpace(identity.JobID)
	captureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.captureTimeout)
	defer cancel()
	scope := fileplugin.Scope{
		TenantID:  identity.TenantID,
		SessionID: identity.SessionID,
		TaskID:    strings.TrimSpace(identity.TaskID),
	}
	var (
		result fileplugin.CaptureResult
		err    error
	)
	if jobID != "" {
		result, err = service.CaptureWorkspaceSystem(captureCtx, scope, callID, 0, scope.TaskID, jobID)
	} else {
		result, err = service.CaptureWorkspace(captureCtx, scope, callID, 0, scope.TaskID)
	}
	if err != nil {
		l.logger.Warn("bash workspace capture failed",
			"tenant_id", identity.TenantID, "session_id", identity.SessionID,
			"call_id", callID, "job_id", jobID, "err", err)
		return err
	}
	l.logger.Debug("bash workspace captured",
		"tenant_id", identity.TenantID, "session_id", identity.SessionID,
		"call_id", callID, "job_id", jobID, "tree", result.Tree, "count", result.Count, "seq", result.Seq)
	return nil
}

func (l *LayerWorkspaces) serviceValue() *fileplugin.Service {
	if l.service == nil {
		return nil
	}
	return l.service()
}
