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

	"github.com/jixoai/keel/internal/capturepolicy"
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
	// CaptureMode is the run's pinned capture policy. Empty consults the
	// attempt context (capturepolicy.CaptureModeFromContext) and then the default
	// (per-tool-call). The bash tool's context wrapper copies the pinned value
	// here; the workspace provider never re-reads configuration at close time,
	// so a rotation cannot change a run already in flight.
	CaptureMode capturepolicy.CaptureMode
}

// Valid reports whether the identity names a session.
func (i SessionIdentity) Valid() bool {
	return i.TenantID != uuid.Nil && i.SessionID != uuid.Nil
}

// captureMode resolves the identity's pinned policy. The explicit identity
// value wins; a context pinned by the runner is the fallback; an unset policy
// is the layerfs default. An unknown value is a typed error, never a silent
// per-tool-call fallback.
func (i SessionIdentity) captureMode(ctx context.Context) (capturepolicy.CaptureMode, error) {
	mode := i.CaptureMode
	if mode == "" {
		if pinned, ok := capturepolicy.CaptureModeFromContext(ctx); ok {
			mode = pinned
		}
	}
	parsed, err := capturepolicy.ParseCaptureMode(string(mode))
	if err != nil {
		return "", err
	}
	return parsed, nil
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
	// pending records sessions whose automatic settle was suppressed by the
	// run's capture mode. A checkpoint consumes the marker and captures once;
	// an explicit checkpoint after an automatic settle is a no-op.
	pending map[sessionKey]struct{}
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
		pending:        make(map[sessionKey]struct{}),
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
	// The policy is resolved at mount time from the run's pinned identity, not
	// at close time from configuration: a rotation affects only later runs. It
	// is resolved before the workspace exists so an unknown policy refuses the
	// mount without creating/seeding anything.
	mode, err := identity.captureMode(ctx)
	if err != nil {
		return Mount{}, err
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
			// A background job always settles at its own close, whatever the
			// run's policy: the job may outlive the run, so suppressing its
			// settle would leave its writes without any later trigger (the
			// layerfs spec treats a job's close as a system fact).
			if strings.TrimSpace(identity.JobID) != "" {
				return l.capture(ctx, identity)
			}
			switch mode {
			case capturepolicy.CaptureModeCheckpoint, capturepolicy.CaptureModeManual:
				l.markPending(identity)
				return nil
			default:
				return l.capture(ctx, identity)
			}
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
	callID := strings.TrimSpace(identity.CallID)
	if callID == "" {
		// No model tool-call id (a direct bridge call): one id per session close
		// keeps the event unique without inventing a replay identity.
		callID = "bash:" + uuid.NewString()
	}
	result, err := l.settle(ctx, identity, callID)
	if err != nil {
		l.logger.Warn("bash workspace capture failed",
			"tenant_id", identity.TenantID, "session_id", identity.SessionID,
			"call_id", callID, "job_id", identity.JobID, "err", err)
		return err
	}
	l.logger.Debug("bash workspace captured",
		"tenant_id", identity.TenantID, "session_id", identity.SessionID,
		"call_id", callID, "job_id", identity.JobID, "tree", result.Tree, "count", result.Count, "seq", result.Seq)
	return nil
}

// Checkpoint settles the session's suppressed workspace changes as exactly one
// fs.capture. It is the explicit trigger of the checkpoint/manual policies: the
// first caller after one or more bash executions consumes the pending marker
// and captures; a second call, or a call after per-tool-call already settled,
// returns captured=false without appending an event. The call id is the
// idempotency key (a stable caller id makes a retry replay instead of
// duplicating).
//
// The owner lane is used, so the caller must execute while the run's lease is
// live. The run-completion hook and the model-visible bash checkpoint action
// both satisfy that; a checkpoint outside a run fails with the service's typed
// lease error instead of committing an unfenced fact.
func (l *LayerWorkspaces) Checkpoint(ctx context.Context, identity SessionIdentity) (fileplugin.CaptureResult, bool, error) {
	if !identity.Valid() {
		return fileplugin.CaptureResult{}, false, fileplugin.Errorf(fileplugin.CodeRunScopeMissing,
			"checkpoint needs the ambient session identity")
	}
	if strings.TrimSpace(identity.JobID) != "" {
		return fileplugin.CaptureResult{}, false, fileplugin.Errorf(fileplugin.CodeInvalid,
			"a background job does not checkpoint: its own close settles the job's changes")
	}
	if !l.consumePending(identity) {
		return fileplugin.CaptureResult{}, false, nil
	}
	callID := strings.TrimSpace(identity.CallID)
	if callID == "" {
		callID = "checkpoint:" + uuid.NewString()
	} else {
		// The prefix keeps an explicit checkpoint out of the key namespace of a
		// per-tool-call settlement that happens to share the model call id.
		callID = "checkpoint:" + callID
	}
	result, err := l.settle(ctx, identity, callID)
	if err != nil {
		// The capture did not settle (and the service committed its
		// capture_failed fact): restore the marker so a later checkpoint can
		// retry, instead of leaving changes that no trigger will ever see.
		l.markPending(identity)
		l.logger.Warn("bash workspace checkpoint failed",
			"tenant_id", identity.TenantID, "session_id", identity.SessionID,
			"call_id", callID, "err", err)
		return fileplugin.CaptureResult{}, false, err
	}
	if result.Replayed {
		// The key already settled a snapshot. Changes that arrived after that
		// settlement are still pending; keep the marker so a fresh checkpoint
		// (a new key) can capture them. Returning the replay is still the honest
		// answer to "settle this key".
		l.markPending(identity)
	}
	l.logger.Debug("bash workspace checkpointed",
		"tenant_id", identity.TenantID, "session_id", identity.SessionID,
		"call_id", callID, "tree", result.Tree, "count", result.Count, "seq", result.Seq)
	return result, true, nil
}

// settle performs one capture settlement on the identity's lane.
func (l *LayerWorkspaces) settle(ctx context.Context, identity SessionIdentity, callID string) (fileplugin.CaptureResult, error) {
	service := l.serviceValue()
	if service == nil {
		return fileplugin.CaptureResult{}, errors.New("bashbridge: the file service is not wired; the run's workspace changes were not captured")
	}
	jobID := strings.TrimSpace(identity.JobID)
	captureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.captureTimeout)
	defer cancel()
	scope := fileplugin.Scope{
		TenantID:  identity.TenantID,
		SessionID: identity.SessionID,
		TaskID:    strings.TrimSpace(identity.TaskID),
	}
	if jobID != "" {
		return service.CaptureWorkspaceSystem(captureCtx, scope, callID, 0, scope.TaskID, jobID)
	}
	return service.CaptureWorkspace(captureCtx, scope, callID, 0, scope.TaskID)
}

func (l *LayerWorkspaces) markPending(identity SessionIdentity) {
	key := sessionKey{tenantID: identity.TenantID, sessionID: identity.SessionID}
	l.mu.Lock()
	l.pending[key] = struct{}{}
	l.mu.Unlock()
}

func (l *LayerWorkspaces) consumePending(identity SessionIdentity) bool {
	key := sessionKey{tenantID: identity.TenantID, sessionID: identity.SessionID}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.pending[key]; !ok {
		return false
	}
	delete(l.pending, key)
	return true
}

func (l *LayerWorkspaces) serviceValue() *fileplugin.Service {
	if l.service == nil {
		return nil
	}
	return l.service()
}
