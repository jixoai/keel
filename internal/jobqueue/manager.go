// Package jobqueue is keel's JobManager for bash background jobs. It owns the
// durable facts (registration, limits, queries, output references, terminal
// state, restart convergence and orphan recovery); it does not implement shell
// semantics. The carrier of a job is an in-process goroutine plus its own gbash
// session created through bashbridge, because gbash's background jobs die when
// their Exec returns and one session serializes Exec (docs/GBASH-CONTRACT.md
// §5.2–§5.4).
//
// Two facts decide the shape of this package:
//
//   - A timeout never kills. The caller's timer fires, the job keeps running in
//     its own goroutine/session, and the model gets the job id plus the bounded
//     output produced so far. Only kill/cancel calls ctx.Cancel.
//   - A restart cannot re-attach a job: goroutines die with the process. Recovery
//     therefore converges every still-running row to `interrupted` with a
//     harness-restart reason and keeps the output blobs readable. It never
//     reports a job as running after a restart.
package jobqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/bashbridge"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/store"
)

// Terminal reasons written to bash_jobs.reason.
const (
	ReasonHarnessRestart  = "harness_restart"
	ReasonHarnessShutdown = "harness_shutdown"
	ReasonSessionDeleted  = "session_deleted"
	ReasonKilled          = "killed"
	ReasonToolCanceled    = "tool_canceled"
)

// ErrLimit reports that the global or per-group running cap is reached. The
// caller refuses the new job; existing jobs keep running.
var ErrLimit = store.ErrBashJobLimit

// ErrClosed reports a start after the manager shut down.
var ErrClosed = errors.New("jobqueue: manager is closed")

// Options configures a Manager.
type Options struct {
	Store  *store.Store
	Bridge *bashbridge.Bridge
	// Uploader stores full output. Nil disables blob output: the model still
	// gets head/tail, but no reference is recorded.
	Uploader *blob.Uploader
	// TenantID scopes restart recovery. Zero converges every tenant, which is
	// correct for a single-tenant harness and the no-auth dev mode.
	TenantID uuid.UUID
	// MaxGlobal / MaxPerGroup cap concurrently running jobs. Non-positive
	// defaults are 8 / 4.
	MaxGlobal   int
	MaxPerGroup int
	// MaxOutputBytes caps one stream's blob. Zero defaults to 1GiB.
	MaxOutputBytes int64
	// SweepInterval is the orphan scan period. Non-positive defaults to 15s.
	SweepInterval time.Duration
	// ProgressInterval persists a running job's bounded head/tail window. A
	// process crash cannot flush the goroutine's buffer, so this is what keeps
	// "output stays readable after a restart" true for an unfinished job.
	// Non-positive defaults to 2s.
	ProgressInterval time.Duration
	Logger           *slog.Logger
	// Now is the clock seam for tests.
	Now func() time.Time
}

// Output is one stream's bounded fact.
type Output struct {
	Head      string     `json:"head,omitempty"`
	Tail      string     `json:"tail,omitempty"`
	Bytes     int64      `json:"bytes"`
	Truncated bool       `json:"truncated,omitempty"`
	BlobID    *uuid.UUID `json:"blobId,omitempty"`
	// Error reports that the full stream could not be stored (the head/tail
	// window is still valid). It never means the command failed.
	Error string `json:"error,omitempty"`
}

// RunRequest is one bash run.
type RunRequest struct {
	Command string
	Cwd     string
	Env     map[string]string
	// Timeout > 0 makes the run a background job: on expiry the call returns
	// with TimedOut and the job keeps running. Timeout <= 0 runs synchronously.
	Timeout time.Duration
}

// RunResult is the tool-facing result of a run.
type RunResult struct {
	JobID    *uuid.UUID    `json:"jobId,omitempty"`
	Status   string        `json:"status"`
	Reason   string        `json:"reason,omitempty"`
	ExitCode *int          `json:"exitCode,omitempty"`
	TimedOut bool          `json:"timedOut"`
	Duration time.Duration `json:"-"`
	Cwd      string        `json:"cwd"`
	Stdout   Output        `json:"stdout"`
	Stderr   Output        `json:"stderr"`
}

// JobView is a job plus its live/persisted output facts.
type JobView struct {
	JobID      uuid.UUID  `json:"jobId"`
	SessionID  *uuid.UUID `json:"sessionId,omitempty"`
	Command    string     `json:"command"`
	Cwd        string     `json:"cwd"`
	Status     string     `json:"status"`
	Reason     string     `json:"reason,omitempty"`
	ExitCode   *int       `json:"exitCode,omitempty"`
	Stdout     Output     `json:"stdout"`
	Stderr     Output     `json:"stderr"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	// Live is true only while this process still owns the goroutine. After a
	// restart every row is terminal, so Live stays false and no caller can
	// mistake a recovered fact for a running job.
	Live bool `json:"live"`
}

type liveJob struct {
	id        uuid.UUID
	tenantID  uuid.UUID
	sessionID uuid.UUID
	groupKey  string
	command   string
	cwd       string
	startedAt time.Time

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	// stopProgress stops the periodic head/tail flush before the final finish.
	stopProgress chan struct{}

	stdout *outputSink
	stderr *outputSink

	mu           sync.Mutex
	cancelReason string
	outcome      *jobOutcome
}

type jobOutcome struct {
	status     string
	reason     string
	exitCode   *int
	stdout     Output
	stderr     Output
	finishedAt time.Time
}

// Manager is the JobManager. It is safe for concurrent use.
type Manager struct {
	store  *store.Store
	bridge *bashbridge.Bridge
	upload *blob.Uploader
	tenant uuid.UUID
	now    func() time.Time
	logger *slog.Logger

	maxGlobal   int
	maxPerGroup int
	maxOutput   int64
	sweep       time.Duration
	progress    time.Duration

	mu     sync.Mutex
	live   map[uuid.UUID]*liveJob
	closed bool

	startOnce sync.Once
	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// New builds a Manager. Start must be called to run restart recovery and the
// orphan sweeper.
func New(opts Options) (*Manager, error) {
	if opts.Store == nil {
		return nil, errors.New("jobqueue: a store is required")
	}
	bridge := opts.Bridge
	if bridge == nil {
		bridge = bashbridge.New(bashbridge.Config{})
	}
	maxGlobal := opts.MaxGlobal
	if maxGlobal <= 0 {
		maxGlobal = 8
	}
	maxPerGroup := opts.MaxPerGroup
	if maxPerGroup <= 0 {
		maxPerGroup = 4
	}
	maxOutput := opts.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = 1 << 30
	}
	uploader := opts.Uploader
	if uploader != nil {
		// Copy so MaxOutputBytes takes effect without mutating a shared uploader
		// the caller may use for images.
		copied := *uploader
		if copied.MaxBytes <= 0 {
			copied.MaxBytes = maxOutput
		}
		uploader = &copied
	}
	interval := opts.SweepInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	progress := opts.ProgressInterval
	if progress <= 0 {
		progress = 2 * time.Second
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	return &Manager{
		store:       opts.Store,
		bridge:      bridge,
		upload:      uploader,
		tenant:      opts.TenantID,
		now:         now,
		logger:      logger,
		maxGlobal:   maxGlobal,
		maxPerGroup: maxPerGroup,
		maxOutput:   maxOutput,
		sweep:       interval,
		progress:    progress,
		live:        make(map[uuid.UUID]*liveJob),
		stop:        make(chan struct{}),
	}, nil
}

// Start performs restart recovery once and starts the orphan sweeper. Recovery
// is the only correct "adoption" available: no goroutine survives the process,
// so a still-running row is converged to interrupted with the harness-restart
// reason and its output references stay readable.
func (m *Manager) Start(ctx context.Context) error {
	var startErr error
	m.startOnce.Do(func() {
		converged, err := m.convergeOnStart(ctx)
		if err != nil {
			startErr = err
			return
		}
		if len(converged) > 0 {
			m.logger.Info("converged bash jobs from a previous harness process",
				"count", len(converged), "reason", ReasonHarnessRestart)
		}
		m.wg.Add(1)
		go m.sweeper(ctx)
	})
	return startErr
}

func (m *Manager) convergeOnStart(ctx context.Context) ([]store.BashJob, error) {
	var scope *uuid.UUID
	if m.tenant != uuid.Nil {
		tenant := m.tenant
		scope = &tenant
	}
	return m.store.ConvergeRunningBashJobs(ctx, scope, store.BashJobInterrupted, ReasonHarnessRestart)
}

func (m *Manager) sweeper(ctx context.Context) {
	defer m.wg.Done()
	ticker := time.NewTicker(m.sweep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-ticker.C:
			if _, err := m.Sweep(ctx); err != nil && ctx.Err() == nil {
				m.logger.Warn("bash job orphan sweep failed", "err", err)
			}
		}
	}
}

// Sweep cancels live jobs whose chat session was archived or deleted. It is
// idempotent and also callable directly (tests use it instead of waiting for
// the timer).
func (m *Manager) Sweep(ctx context.Context) (int, error) {
	live := m.liveSnapshot()
	canceled := 0
	for _, job := range live {
		if job.sessionID == uuid.Nil {
			continue
		}
		active, err := m.store.SessionActive(ctx, job.tenantID, job.sessionID)
		if err != nil {
			return canceled, err
		}
		if !active {
			if m.cancelLive(job.id, ReasonSessionDeleted) {
				canceled++
			}
			select {
			case <-job.done:
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
		}
	}
	return canceled, nil
}

// Run executes one bash request. With Timeout <= 0 it is synchronous and
// returns the terminal result. With Timeout > 0 it registers a job; if the
// timer fires first the job keeps running and the result reports TimedOut with
// the job id and the head/tail produced so far.
func (m *Manager) Run(ctx context.Context, tenantID, sessionID uuid.UUID, req RunRequest) (RunResult, error) {
	if req.Timeout <= 0 {
		return m.runSync(ctx, tenantID, req)
	}
	job, err := m.startJob(ctx, tenantID, sessionID, req)
	if err != nil {
		return RunResult{}, err
	}
	timer := time.NewTimer(req.Timeout)
	defer timer.Stop()
	select {
	case <-job.done:
		return m.resultFromJob(job, false), nil
	case <-timer.C:
		// The goroutine may have settled in the same instant the timer fired;
		// prefer the terminal fact over a timed-out view of a finished job.
		job.mu.Lock()
		settled := job.outcome != nil
		job.mu.Unlock()
		return m.resultFromJob(job, !settled), nil
	case <-ctx.Done():
		m.cancelLive(job.id, ReasonToolCanceled)
		select {
		case <-job.done:
		case <-time.After(10 * time.Second):
		}
		return m.resultFromJob(job, false), nil
	}
}

func (m *Manager) runSync(ctx context.Context, tenantID uuid.UUID, req RunRequest) (RunResult, error) {
	if req.Command == "" {
		return RunResult{}, errors.New("jobqueue: command is empty")
	}
	uploadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
	defer cancel()
	started := m.now()
	session, err := m.bridge.NewSession(ctx)
	if err != nil {
		return RunResult{}, err
	}
	stdout := newOutputSink(uploadCtx, m.upload, tenantID, m.bridge.NewBounded())
	stderr := newOutputSink(uploadCtx, m.upload, tenantID, m.bridge.NewBounded())
	result, runErr := session.Run(ctx, bashbridge.RunSpec{
		Command: req.Command,
		WorkDir: req.Cwd,
		Env:     req.Env,
		Stdout:  stdout,
		Stderr:  stderr,
	})
	_ = session.Close()
	stdoutOut := stdout.Close()
	stderrOut := stderr.Close()
	out := RunResult{
		Status:   store.BashJobExited,
		Duration: m.now().Sub(started),
		Cwd:      cwdOr(req.Cwd, m.bridge),
		Stdout:   stdoutOut,
		Stderr:   stderrOut,
	}
	if runErr != nil {
		out.Status = store.BashJobFailed
		out.Reason = runErr.Error()
		return out, nil
	}
	exit := result.ExitCode
	out.ExitCode = &exit
	if canceledExecution(result) {
		out.Status = store.BashJobCanceled
		out.Reason = ReasonToolCanceled
	}
	return out, nil
}

func (m *Manager) startJob(ctx context.Context, tenantID, sessionID uuid.UUID, req RunRequest) (*liveJob, error) {
	if req.Command == "" {
		return nil, errors.New("jobqueue: command is empty")
	}
	groupKey := ""
	if sessionID != uuid.Nil {
		groupKey = sessionID.String()
	}
	jobID := uuid.New()
	job := store.BashJob{
		TenantID:  tenantID,
		JobID:     jobID,
		SessionID: optionalUUID(sessionID),
		GroupKey:  groupKey,
		Command:   req.Command,
		Cwd:       cwdOr(req.Cwd, m.bridge),
		Status:    store.BashJobRunning,
	}
	if err := m.store.CreateBashJob(ctx, job, m.maxGlobal, m.maxPerGroup); err != nil {
		return nil, err
	}
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	uploadCtx, uploadCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
	living := &liveJob{
		id:           jobID,
		tenantID:     tenantID,
		sessionID:    sessionID,
		groupKey:     groupKey,
		command:      req.Command,
		cwd:          job.Cwd,
		startedAt:    m.now(),
		ctx:          jobCtx,
		cancel:       cancel,
		done:         make(chan struct{}),
		stopProgress: make(chan struct{}),
		stdout:       newOutputSink(uploadCtx, m.upload, tenantID, m.bridge.NewBounded()),
		stderr:       newOutputSink(uploadCtx, m.upload, tenantID, m.bridge.NewBounded()),
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		uploadCancel()
		_ = living.stdout.Close()
		_ = living.stderr.Close()
		_, _ = m.store.FinishBashJob(context.Background(), tenantID, jobID, store.BashJobFinish{
			Status: store.BashJobInterrupted,
			Reason: ReasonHarnessShutdown,
		})
		return nil, ErrClosed
	}
	m.live[jobID] = living
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		progressDone := make(chan struct{})
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer close(progressDone)
			m.progressLoop(jobCtx, living)
		}()
		defer close(living.done)
		defer uploadCancel()
		m.runJob(jobCtx, living, req)
		close(living.stopProgress)
		<-progressDone
	}()
	return living, nil
}

// progressLoop persists the bounded output window of a running job. A crash
// cannot run the final finish path, so this periodic fact is what makes
// recovery report readable output instead of an empty row.
func (m *Manager) progressLoop(ctx context.Context, job *liveJob) {
	ticker := time.NewTicker(m.progress)
	defer ticker.Stop()
	for {
		select {
		case <-job.stopProgress:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			stdout := job.stdout.Snapshot()
			stderr := job.stderr.Snapshot()
			updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_, err := m.store.UpdateBashJobProgress(updateCtx, job.tenantID, job.id,
				stdout.Head, stdout.Tail, stderr.Head, stderr.Tail,
				stdout.Bytes, stderr.Bytes, stdout.Truncated || stderr.Truncated)
			cancel()
			if err != nil && ctx.Err() == nil {
				m.logger.Warn("persist bash job progress", "job", job.id.String(), "err", err)
			}
		}
	}
}

func (m *Manager) runJob(ctx context.Context, job *liveJob, req RunRequest) {
	// A background job is a harness-owned execution unit that may outlive the
	// run attempt that started it. Stamp its id on the ambient session identity
	// before the session is created, so the workspace close settles fs.capture
	// on the system lane with jobId attribution instead of requiring the parent
	// run's lease (which is gone by then). The identity itself comes from the
	// caller (bash tool / nohup); a job started without one (direct manager use)
	// keeps the no-capture fallback workspace.
	if identity, ok := bashbridge.SessionIdentityFromContext(ctx); ok {
		identity.JobID = job.id.String()
		ctx = bashbridge.WithSessionIdentity(ctx, identity)
	}
	session, err := m.bridge.NewSession(ctx)
	var result bashbridge.Result
	var runErr error
	if err != nil {
		runErr = err
	} else {
		result, runErr = session.Run(ctx, bashbridge.RunSpec{
			Command: req.Command,
			WorkDir: req.Cwd,
			Env:     req.Env,
			Stdout:  job.stdout,
			Stderr:  job.stderr,
		})
		_ = session.Close()
	}
	stdoutOut := job.stdout.Close()
	stderrOut := job.stderr.Close()

	status, reason := terminalStatus(result, runErr, job.cancelReasonNow())
	finish := store.BashJobFinish{
		Status:          status,
		Reason:          reason,
		StdoutBlobID:    stdoutOut.BlobID,
		StderrBlobID:    stderrOut.BlobID,
		StdoutBytes:     stdoutOut.Bytes,
		StderrBytes:     stderrOut.Bytes,
		StdoutHead:      stdoutOut.Head,
		StdoutTail:      stdoutOut.Tail,
		StderrHead:      stderrOut.Head,
		StderrTail:      stderrOut.Tail,
		OutputTruncated: stdoutOut.Truncated || stderrOut.Truncated,
	}
	if runErr == nil {
		exit := result.ExitCode
		finish.ExitCode = &exit
	}
	if _, err := m.store.FinishBashJob(context.WithoutCancel(ctx), job.tenantID, job.id, finish); err != nil {
		m.logger.Warn("finish bash job", "job", job.id.String(), "err", err)
	}
	outcome := &jobOutcome{
		status:     status,
		reason:     reason,
		exitCode:   finish.ExitCode,
		stdout:     stdoutOut,
		stderr:     stderrOut,
		finishedAt: m.now(),
	}
	job.mu.Lock()
	job.outcome = outcome
	job.mu.Unlock()
	m.mu.Lock()
	delete(m.live, job.id)
	m.mu.Unlock()
}

// resultFromJob renders a job's current facts. A live job without an outcome
// yields the snapshot taken while it is still running and no exit code.
func (m *Manager) resultFromJob(job *liveJob, timedOut bool) RunResult {
	out := RunResult{
		JobID:    &job.id,
		Status:   store.BashJobRunning,
		TimedOut: timedOut,
		Cwd:      job.cwd,
	}
	job.mu.Lock()
	outcome := job.outcome
	cancelReason := job.cancelReason
	job.mu.Unlock()
	if outcome != nil {
		out.Status = outcome.status
		out.Reason = outcome.reason
		out.ExitCode = outcome.exitCode
		out.Stdout = outcome.stdout
		out.Stderr = outcome.stderr
		out.Duration = outcome.finishedAt.Sub(job.startedAt)
		return out
	}
	out.Stdout = job.stdout.Snapshot()
	out.Stderr = job.stderr.Snapshot()
	out.Duration = m.now().Sub(job.startedAt)
	if cancelReason != "" {
		out.Reason = cancelReason
		if cancelReason == ReasonHarnessShutdown {
			out.Status = store.BashJobInterrupted
		} else {
			out.Status = store.BashJobCanceled
		}
	}
	return out
}

func terminalStatus(result bashbridge.Result, runErr error, cancelReason string) (string, string) {
	switch cancelReason {
	case ReasonKilled:
		return store.BashJobCanceled, ReasonKilled
	case ReasonSessionDeleted:
		return store.BashJobCanceled, ReasonSessionDeleted
	case ReasonToolCanceled:
		return store.BashJobCanceled, ReasonToolCanceled
	case ReasonHarnessShutdown:
		return store.BashJobInterrupted, ReasonHarnessShutdown
	}
	if runErr != nil {
		return store.BashJobFailed, "harness_error: " + runErr.Error()
	}
	if canceledExecution(result) {
		return store.BashJobCanceled, ReasonToolCanceled
	}
	return store.BashJobExited, ""
}

// canceledExecution distinguishes a ctx cancellation from a script that
// happens to `exit 130`: only gbash's cancellation path writes its control
// diagnostic. A script's own 130 is an ordinary exited job.
func canceledExecution(result bashbridge.Result) bool {
	return result.ExitCode == 130 && strings.Contains(result.ControlStderr, "canceled")
}

// List returns the tenant's jobs, running first, newest first.
func (m *Manager) List(ctx context.Context, tenantID uuid.UUID, limit int) ([]JobView, error) {
	jobs, err := m.store.ListBashJobs(ctx, tenantID, "", limit)
	if err != nil {
		return nil, err
	}
	views := make([]JobView, 0, len(jobs))
	for _, job := range jobs {
		views = append(views, m.viewJob(job))
	}
	return views, nil
}

// Status returns one job with a live head/tail snapshot while it is running.
func (m *Manager) Status(ctx context.Context, tenantID, jobID uuid.UUID) (JobView, error) {
	job, err := m.store.GetBashJob(ctx, tenantID, jobID)
	if err != nil {
		return JobView{}, err
	}
	return m.viewJob(job), nil
}

// Adopt restores a job's facts and settles its terminal state. A running row
// with no live carrier in this process is converged to interrupted with the
// harness-restart reason; a live row is reported as running (it was never
// detached, so there is nothing to re-attach).
func (m *Manager) Adopt(ctx context.Context, tenantID, jobID uuid.UUID) (JobView, error) {
	job, err := m.store.GetBashJob(ctx, tenantID, jobID)
	if err != nil {
		return JobView{}, err
	}
	if job.Status != store.BashJobRunning {
		return m.viewJob(job), nil
	}
	if _, ok := m.liveJob(jobID); ok {
		return m.viewJob(job), nil
	}
	if _, err := m.store.FinishBashJob(ctx, tenantID, jobID, store.BashJobFinish{
		Status: store.BashJobInterrupted,
		Reason: ReasonHarnessRestart,
	}); err != nil {
		return JobView{}, err
	}
	job, err = m.store.GetBashJob(ctx, tenantID, jobID)
	if err != nil {
		return JobView{}, err
	}
	return m.viewJob(job), nil
}

// Kill cancels a live job's context and waits for it to settle. A running row
// with no live carrier is dead by definition and is converged to interrupted.
func (m *Manager) Kill(ctx context.Context, tenantID, jobID uuid.UUID) (JobView, error) {
	job, err := m.store.GetBashJob(ctx, tenantID, jobID)
	if err != nil {
		return JobView{}, err
	}
	if job.Status != store.BashJobRunning {
		return m.viewJob(job), nil
	}
	live, ok := m.liveJob(jobID)
	if !ok {
		if _, err := m.store.FinishBashJob(ctx, tenantID, jobID, store.BashJobFinish{
			Status: store.BashJobInterrupted,
			Reason: ReasonHarnessRestart,
		}); err != nil {
			return JobView{}, err
		}
		job, err = m.store.GetBashJob(ctx, tenantID, jobID)
		if err != nil {
			return JobView{}, err
		}
		return m.viewJob(job), nil
	}
	m.cancelLive(jobID, ReasonKilled)
	select {
	case <-live.done:
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
	}
	job, err = m.store.GetBashJob(context.WithoutCancel(ctx), tenantID, jobID)
	if err != nil {
		return JobView{}, err
	}
	return m.viewJob(job), nil
}

// CancelSession cancels the session's live jobs and converges any running row
// whose carrier is gone. It is the explicit orphan-recovery hook (the sweeper
// calls the same path when a session is archived).
func (m *Manager) CancelSession(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error) {
	if sessionID == uuid.Nil {
		return 0, nil
	}
	jobs, err := m.store.RunningBashJobsBySession(ctx, tenantID, sessionID)
	if err != nil {
		return 0, err
	}
	canceled := 0
	for _, job := range jobs {
		if live, ok := m.liveJob(job.JobID); ok {
			if m.cancelLive(job.JobID, ReasonSessionDeleted) {
				canceled++
			}
			select {
			case <-live.done:
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
			continue
		}
		if _, err := m.store.FinishBashJob(ctx, tenantID, job.JobID, store.BashJobFinish{
			Status: store.BashJobCanceled,
			Reason: ReasonSessionDeleted,
		}); err != nil {
			return canceled, err
		}
		canceled++
	}
	return canceled, nil
}

// Close cancels every live job (interrupted: harness shutdown) and waits for
// the goroutines. It never leaves a goroutine behind.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		live := make([]*liveJob, 0, len(m.live))
		for _, job := range m.live {
			job.setCancelReason(ReasonHarnessShutdown)
			job.cancel()
			live = append(live, job)
		}
		m.mu.Unlock()
		close(m.stop)
		deadline := time.After(15 * time.Second)
		for _, job := range live {
			select {
			case <-job.done:
			case <-deadline:
			}
		}
		m.wg.Wait()
	})
}

func (m *Manager) viewJob(job store.BashJob) JobView {
	view := JobView{
		JobID:      job.JobID,
		SessionID:  job.SessionID,
		Command:    job.Command,
		Cwd:        job.Cwd,
		Status:     job.Status,
		Reason:     job.Reason,
		ExitCode:   job.ExitCode,
		Stdout:     Output{Head: job.StdoutHead, Tail: job.StdoutTail, Bytes: job.StdoutBytes, Truncated: job.OutputTruncated, BlobID: job.StdoutBlobID},
		Stderr:     Output{Head: job.StderrHead, Tail: job.StderrTail, Bytes: job.StderrBytes, Truncated: job.OutputTruncated, BlobID: job.StderrBlobID},
		CreatedAt:  job.CreatedAt,
		StartedAt:  job.StartedAt,
		FinishedAt: job.FinishedAt,
	}
	if live, ok := m.liveJob(job.JobID); ok {
		live.mu.Lock()
		outcome := live.outcome
		live.mu.Unlock()
		if outcome != nil {
			// The goroutine settled but has not yet removed itself from the live
			// map: report the terminal fact, never a stale "running".
			view.Status = outcome.status
			view.Reason = outcome.reason
			view.ExitCode = outcome.exitCode
			view.Stdout = outcome.stdout
			view.Stderr = outcome.stderr
			view.Live = false
			if !outcome.finishedAt.IsZero() {
				finished := outcome.finishedAt
				view.FinishedAt = &finished
			}
			return view
		}
		view.Live = true
		view.Status = store.BashJobRunning
		view.Stdout = mergeLiveOutput(view.Stdout, live.stdout.Snapshot())
		view.Stderr = mergeLiveOutput(view.Stderr, live.stderr.Snapshot())
	}
	return view
}

func mergeLiveOutput(persisted Output, live Output) Output {
	persisted.Head = live.Head
	persisted.Tail = live.Tail
	persisted.Bytes = live.Bytes
	persisted.Truncated = live.Truncated
	return persisted
}

func (m *Manager) liveSnapshot() []*liveJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*liveJob, 0, len(m.live))
	for _, job := range m.live {
		out = append(out, job)
	}
	return out
}

func (m *Manager) liveJob(id uuid.UUID) (*liveJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.live[id]
	return job, ok
}

func (m *Manager) cancelLive(id uuid.UUID, reason string) bool {
	job, ok := m.liveJob(id)
	if !ok {
		return false
	}
	job.setCancelReason(reason)
	job.cancel()
	return true
}

func (j *liveJob) setCancelReason(reason string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cancelReason == "" {
		j.cancelReason = reason
	}
}

func (j *liveJob) cancelReasonNow() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cancelReason
}

func optionalUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func cwdOr(cwd string, bridge *bashbridge.Bridge) string {
	if cwd != "" {
		return cwd
	}
	return bashbridge.DefaultMountPoint
}

// LimitReached reports whether err is the job cap refusal.
func LimitReached(err error) bool { return errors.Is(err, store.ErrBashJobLimit) }

// String renders a job view for logs/tests.
func (v JobView) String() string {
	return fmt.Sprintf("%s %s %s", v.JobID, v.Status, v.Command)
}
