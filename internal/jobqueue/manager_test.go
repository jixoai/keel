package jobqueue

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/bashbridge"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, store.Config{DSN: dsn, MaxConns: 8, MinConns: 1})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func newTestManager(t *testing.T, st *store.Store, tenant uuid.UUID, mutate func(*Options)) *Manager {
	t.Helper()
	dir := t.TempDir()
	opts := Options{
		Store: st,
		Bridge: bashbridge.New(bashbridge.Config{
			HeadLimit: 256,
			TailLimit: 256,
		}),
		Uploader: &blob.Uploader{
			Store:       st,
			CASDir:      dir,
			TempDir:     t.TempDir(),
			PGThreshold: 1 << 20,
			MaxBytes:    512 << 20,
		},
		TenantID:      tenant,
		MaxGlobal:     8,
		MaxPerGroup:   4,
		SweepInterval: time.Hour,
	}
	if mutate != nil {
		mutate(&opts)
	}
	manager, err := New(opts)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	t.Cleanup(manager.Close)
	return manager
}

func readBlob(t *testing.T, st *store.Store, dir string, tenant uuid.UUID, id *uuid.UUID) string {
	t.Helper()
	if id == nil {
		t.Fatal("blob id is nil")
	}
	meta, err := st.BlobMetaOf(context.Background(), tenant, *id)
	if err != nil {
		t.Fatalf("blob meta: %v", err)
	}
	reader := blob.NewReader(st, 4, 1)
	reader.CASDir = dir
	var out strings.Builder
	if _, err := reader.StreamRaw(context.Background(), tenant, *id, meta, &out, 0, 0); err != nil {
		t.Fatalf("read blob: %v", err)
	}
	return out.String()
}

func waitTerminal(t *testing.T, manager *Manager, tenant, jobID uuid.UUID, timeout time.Duration) JobView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		view, err := manager.Status(context.Background(), tenant, jobID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if view.Status != store.BashJobRunning {
			return view
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not settle within %s", jobID, timeout)
	return JobView{}
}

// failingFinishWorkspace is the default scratch workspace with a settle hook
// that always fails: the command runs and the job finishes, but Session.Close
// reports an error. It is the service-nil / double-failure shape whose log the
// manager must keep instead of discarding `session.Close()`.
type failingFinishWorkspace struct{}

func (failingFinishWorkspace) Mount(ctx context.Context) (bashbridge.Mount, error) {
	mount, err := bashbridge.TempDirWorkspace{}.Mount(ctx)
	if err != nil {
		return mount, err
	}
	mount.Finish = func() error { return errors.New("test: workspace settle failed") }
	return mount, nil
}

// TestSyncRunLogsSessionCloseFailure covers the synchronous path: a failed
// settlement is logged and never changes the command's terminal fact.
func TestSyncRunLogsSessionCloseFailure(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	var logs bytes.Buffer
	manager := newTestManager(t, st, tenant, func(opts *Options) {
		opts.Bridge = bashbridge.New(bashbridge.Config{Workspace: failingFinishWorkspace{}})
		opts.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})
	result, err := manager.Run(context.Background(), tenant, uuid.Nil, RunRequest{Command: `true`})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != store.BashJobExited || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("settle failure changed the terminal fact: %#v", result)
	}
	out := logs.String()
	if !strings.Contains(out, "settle bash session") || !strings.Contains(out, "workspace settle failed") {
		t.Fatalf("close failure not logged: %q", out)
	}
}

// TestBackgroundJobLogsSessionCloseFailure covers the job path: the settle
// failure is logged with the job id and the job still settles as exited.
func TestBackgroundJobLogsSessionCloseFailure(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	var logs bytes.Buffer
	manager := newTestManager(t, st, tenant, func(opts *Options) {
		opts.Bridge = bashbridge.New(bashbridge.Config{Workspace: failingFinishWorkspace{}})
		opts.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})
	result, err := manager.Run(context.Background(), tenant, uuid.Nil, RunRequest{
		Command: `true`,
		Timeout: time.Nanosecond,
	})
	if err != nil || result.JobID == nil {
		t.Fatalf("run: %v %#v", err, result)
	}
	view := waitTerminal(t, manager, tenant, *result.JobID, 5*time.Second)
	if view.Status != store.BashJobExited {
		t.Fatalf("terminal status = %q reason=%q", view.Status, view.Reason)
	}
	out := logs.String()
	if !strings.Contains(out, "settle bash session") || !strings.Contains(out, "workspace settle failed") {
		t.Fatalf("close failure not logged: %q", out)
	}
	if !strings.Contains(out, result.JobID.String()) {
		t.Fatalf("close log does not name the job %s: %q", result.JobID, out)
	}
}

func TestSyncRunStoresFullOutputAndBlob(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	manager := newTestManager(t, st, tenant, nil)
	result, err := manager.Run(context.Background(), tenant, uuid.Nil, RunRequest{
		Command: `echo hello; echo oops >&2; printf 'tail-line\n'`,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.JobID != nil {
		t.Fatalf("a synchronous run must not create a job id: %#v", result.JobID)
	}
	if result.Status != store.BashJobExited || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(result.Stdout.Head, "hello") || !strings.Contains(result.Stdout.Head, "tail-line") {
		t.Fatalf("stdout head = %q", result.Stdout.Head)
	}
	if !strings.Contains(result.Stderr.Head, "oops") {
		t.Fatalf("stderr head = %q", result.Stderr.Head)
	}
	if result.Stdout.Bytes != int64(len("hello\n"))+int64(len("tail-line\n")) {
		t.Fatalf("stdout bytes = %d", result.Stdout.Bytes)
	}
	if result.Stdout.BlobID == nil || result.Stderr.BlobID == nil {
		t.Fatalf("blob refs missing: stdout=%v stderr=%v", result.Stdout.BlobID, result.Stderr.BlobID)
	}
	content := readBlob(t, st, manager.upload.CASDir, tenant, result.Stdout.BlobID)
	if content != "hello\ntail-line\n" {
		t.Fatalf("blob content = %q", content)
	}
}

func TestTimeoutContinuesAsJobAndKeepsOutput(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	manager := newTestManager(t, st, tenant, nil)
	result, err := manager.Run(context.Background(), tenant, uuid.Nil, RunRequest{
		Command: `echo tick1; sleep 0.3; echo tick2; sleep 0.3; echo done`,
		Timeout: 120 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.TimedOut || result.JobID == nil {
		t.Fatalf("expected a timed-out job, got %#v", result)
	}
	if result.Status != store.BashJobRunning {
		t.Fatalf("status = %q, want running", result.Status)
	}
	if !strings.Contains(result.Stdout.Head, "tick1") {
		t.Fatalf("pre-timeout output was lost: %q", result.Stdout.Head)
	}
	// The job must keep running after the tool returned, and the final fact must
	// land in the row (not just in the goroutine).
	view := waitTerminal(t, manager, tenant, *result.JobID, 5*time.Second)
	if view.Status != store.BashJobExited {
		t.Fatalf("terminal status = %q reason=%q", view.Status, view.Reason)
	}
	if view.ExitCode == nil || *view.ExitCode != 0 {
		t.Fatalf("exit code = %v", view.ExitCode)
	}
	if !strings.Contains(view.Stdout.Head, "tick1") || !strings.Contains(view.Stdout.Head, "tick2") {
		t.Fatalf("final stdout head = %q", view.Stdout.Head)
	}
	if view.Stdout.BlobID == nil {
		t.Fatal("final blob reference missing")
	}
	content := readBlob(t, st, manager.upload.CASDir, tenant, view.Stdout.BlobID)
	for _, want := range []string{"tick1", "tick2", "done"} {
		if !strings.Contains(content, want) {
			t.Fatalf("blob content lost %q: %q", want, content)
		}
	}
	if view.Live {
		t.Fatal("a settled job must not report Live")
	}
}

func TestKillCancelsJobAndRecordsTerminalState(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	manager := newTestManager(t, st, tenant, nil)
	result, err := manager.Run(context.Background(), tenant, uuid.Nil, RunRequest{
		Command: `echo before; sleep 30`,
		Timeout: 100 * time.Millisecond,
	})
	if err != nil || result.JobID == nil {
		t.Fatalf("run: %v %#v", err, result)
	}
	view, err := manager.Kill(context.Background(), tenant, *result.JobID)
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	if view.Status != store.BashJobCanceled || view.Reason != ReasonKilled {
		t.Fatalf("killed job = %#v", view)
	}
	if view.Live {
		t.Fatal("killed job still reports Live")
	}
	if view.ExitCode != nil && *view.ExitCode != 130 {
		t.Fatalf("exit code = %v, want 130 for a canceled sandbox execution", *view.ExitCode)
	}
}

func TestLimitsRejectNewJobsWithoutKillingExisting(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	manager := newTestManager(t, st, tenant, func(opts *Options) {
		opts.MaxGlobal = 1
		opts.MaxPerGroup = 1
	})
	first, err := manager.Run(context.Background(), tenant, uuid.New(), RunRequest{
		Command: `sleep 30`,
		Timeout: 100 * time.Millisecond,
	})
	if err != nil || first.JobID == nil {
		t.Fatalf("first run: %v %#v", err, first)
	}
	_, err = manager.Run(context.Background(), tenant, uuid.Nil, RunRequest{
		Command: `echo second`,
		Timeout: 100 * time.Millisecond,
	})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("second run error = %v, want ErrLimit", err)
	}
	// The existing job must still be running and killable.
	view, err := manager.Status(context.Background(), tenant, *first.JobID)
	if err != nil || view.Status != store.BashJobRunning {
		t.Fatalf("first job after rejection: %#v %v", view, err)
	}
	if _, err := manager.Kill(context.Background(), tenant, *first.JobID); err != nil {
		t.Fatalf("kill first: %v", err)
	}
	third, err := manager.Run(context.Background(), tenant, uuid.Nil, RunRequest{
		Command: `echo third`,
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("third run after kill: %v", err)
	}
	if third.TimedOut {
		t.Fatalf("third run should have finished within its timeout: %#v", third)
	}
}

func TestRestartConvergesRunningJobsToInterrupted(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	// Simulate a previous process: its goroutine died, the row still says running
	// and its output blob is complete.
	dir := t.TempDir()
	uploader := &blob.Uploader{Store: st, CASDir: dir, TempDir: t.TempDir(), PGThreshold: 1 << 20}
	meta, err := uploader.Put(context.Background(), tenant, "text/plain; charset=utf-8", strings.NewReader("partial-before-crash"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	jobID := uuid.New()
	session := uuid.New()
	if err := st.CreateBashJob(context.Background(), store.BashJob{
		TenantID:     tenant,
		JobID:        jobID,
		SessionID:    &session,
		Command:      "echo partial; sleep 1000",
		Cwd:          "/workspace",
		Status:       store.BashJobRunning,
		StdoutBlobID: &meta.BlobID,
		StdoutBytes:  int64(len("partial-before-crash")),
		StdoutHead:   "partial-before-crash",
	}, 8, 4); err != nil {
		t.Fatalf("create stale job: %v", err)
	}

	manager := newTestManager(t, st, tenant, func(opts *Options) {
		opts.Uploader = uploader
	})
	view, err := manager.Status(context.Background(), tenant, jobID)
	if err != nil {
		t.Fatalf("status after restart: %v", err)
	}
	if view.Status != store.BashJobInterrupted || view.Reason != ReasonHarnessRestart {
		t.Fatalf("recovered job = %#v", view)
	}
	if view.Live {
		t.Fatal("a recovered job must never look live")
	}
	content := readBlob(t, st, dir, tenant, view.Stdout.BlobID)
	if content != "partial-before-crash" {
		t.Fatalf("recovered output = %q", content)
	}
	// Adopt is idempotent over an already-terminal row: it reports facts, it does
	// not invent a running job.
	adopted, err := manager.Adopt(context.Background(), tenant, jobID)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if adopted.Status != store.BashJobInterrupted || adopted.Live {
		t.Fatalf("adopted job = %#v", adopted)
	}
}

func TestAdoptConvergesStaleRunningRow(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	manager := newTestManager(t, st, tenant, nil)
	jobID := uuid.New()
	if err := st.CreateBashJob(context.Background(), store.BashJob{
		TenantID: tenant,
		JobID:    jobID,
		Command:  "echo stale",
		Cwd:      "/workspace",
		Status:   store.BashJobRunning,
	}, 8, 4); err != nil {
		t.Fatalf("create: %v", err)
	}
	view, err := manager.Adopt(context.Background(), tenant, jobID)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if view.Status != store.BashJobInterrupted || view.Reason != ReasonHarnessRestart {
		t.Fatalf("adopt = %#v", view)
	}
}

func TestOrphanJobsAreCanceledWhenSessionArchived(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	session := uuid.New()
	if err := st.CreateSession(context.Background(), tenant, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	manager := newTestManager(t, st, tenant, nil)
	result, err := manager.Run(context.Background(), tenant, session, RunRequest{
		Command: `sleep 30`,
		Timeout: 100 * time.Millisecond,
	})
	if err != nil || result.JobID == nil {
		t.Fatalf("run: %v %#v", err, result)
	}
	if _, err := st.ArchiveSession(context.Background(), tenant, session); err != nil {
		t.Fatalf("archive session: %v", err)
	}
	canceled, err := manager.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if canceled != 1 {
		t.Fatalf("sweep canceled %d jobs, want 1", canceled)
	}
	view, err := manager.Status(context.Background(), tenant, *result.JobID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if view.Status != store.BashJobCanceled || view.Reason != ReasonSessionDeleted || view.Live {
		t.Fatalf("orphan job = %#v", view)
	}
}

func TestCancelSessionCancelsLiveJob(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	session := uuid.New()
	manager := newTestManager(t, st, tenant, nil)
	result, err := manager.Run(context.Background(), tenant, session, RunRequest{
		Command: `sleep 30`,
		Timeout: 100 * time.Millisecond,
	})
	if err != nil || result.JobID == nil {
		t.Fatalf("run: %v %#v", err, result)
	}
	canceled, err := manager.CancelSession(context.Background(), tenant, session)
	if err != nil {
		t.Fatalf("cancel session: %v", err)
	}
	if canceled != 1 {
		t.Fatalf("canceled = %d, want 1", canceled)
	}
	view, err := manager.Status(context.Background(), tenant, *result.JobID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if view.Status != store.BashJobCanceled || view.Reason != ReasonSessionDeleted {
		t.Fatalf("job after session cancel = %#v", view)
	}
}

func TestCloseConvergesLiveJobs(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	manager := newTestManager(t, st, tenant, nil)
	result, err := manager.Run(context.Background(), tenant, uuid.Nil, RunRequest{
		Command: `sleep 30`,
		Timeout: 100 * time.Millisecond,
	})
	if err != nil || result.JobID == nil {
		t.Fatalf("run: %v %#v", err, result)
	}
	manager.Close()
	job, err := st.GetBashJob(context.Background(), tenant, *result.JobID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != store.BashJobInterrupted || job.Reason != ReasonHarnessShutdown {
		t.Fatalf("job after close = %#v", job)
	}
}

func TestScriptExit130IsAnOrdinaryExit(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	manager := newTestManager(t, st, tenant, nil)
	result, err := manager.Run(context.Background(), tenant, uuid.Nil, RunRequest{Command: `exit 130`})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != store.BashJobExited || result.Reason != "" {
		t.Fatalf("script exit 130 = %#v, want exited without a cancellation reason", result)
	}
	if result.ExitCode == nil || *result.ExitCode != 130 {
		t.Fatalf("exit code = %v", result.ExitCode)
	}
}

func TestRunningJobPersistsBoundedProgress(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	manager := newTestManager(t, st, tenant, func(opts *Options) {
		opts.ProgressInterval = 40 * time.Millisecond
	})
	result, err := manager.Run(context.Background(), tenant, uuid.Nil, RunRequest{
		Command: `echo first; sleep 0.8; echo second`,
		Timeout: 100 * time.Millisecond,
	})
	if err != nil || result.JobID == nil {
		t.Fatalf("run: %v %#v", err, result)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		job, err := st.GetBashJob(context.Background(), tenant, *result.JobID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if strings.Contains(job.StdoutHead, "first") {
			if job.Status != store.BashJobRunning {
				t.Fatalf("job already terminal while checking progress: %q", job.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress was never persisted: head=%q status=%q", job.StdoutHead, job.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := manager.Kill(context.Background(), tenant, *result.JobID); err != nil {
		t.Fatalf("kill: %v", err)
	}
}

func Test200MBOutputIsBoundedAndBlobReferenced(t *testing.T) {
	st := testStore(t)
	tenant := uuid.New()
	const size = 200_000_000
	manager := newTestManager(t, st, tenant, func(opts *Options) {
		opts.Bridge = bashbridge.New(bashbridge.Config{HeadLimit: 4096, TailLimit: 4096})
		opts.Uploader = &blob.Uploader{
			Store:       st,
			CASDir:      t.TempDir(),
			TempDir:     t.TempDir(),
			PGThreshold: 1 << 20,
			MaxBytes:    size + (1 << 20),
		}
		opts.MaxOutputBytes = size + (1 << 20)
	})

	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	var peak atomic.Uint64
	stop := make(chan struct{})
	var samplerDone atomic.Bool
	go func() {
		var stats runtime.MemStats
		for {
			select {
			case <-stop:
				samplerDone.Store(true)
				return
			default:
				runtime.ReadMemStats(&stats)
				if stats.HeapAlloc > peak.Load() {
					peak.Store(stats.HeapAlloc)
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	start := time.Now()
	result, err := manager.Run(context.Background(), tenant, uuid.Nil, RunRequest{
		Command: `head -c 200000000 /dev/zero`,
		Timeout: 0,
	})
	close(stop)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for !samplerDone.Load() {
		runtime.Gosched()
	}
	elapsed := time.Since(start)
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
	if result.Stdout.Bytes != size {
		t.Fatalf("stdout bytes = %d, want %d", result.Stdout.Bytes, size)
	}
	if len(result.Stdout.Head) > 4096 || len(result.Stdout.Tail) > 4096 {
		t.Fatalf("head/tail = %d/%d bytes, want <= 4096", len(result.Stdout.Head), len(result.Stdout.Tail))
	}
	if !result.Stdout.Truncated {
		t.Fatal("a 200MB stream must be marked truncated in the model view")
	}
	if result.Stdout.BlobID == nil {
		t.Fatal("blob reference missing")
	}
	meta, err := st.BlobMetaOf(context.Background(), tenant, *result.Stdout.BlobID)
	if err != nil {
		t.Fatalf("blob meta: %v", err)
	}
	if meta.Size != size || !meta.Complete {
		t.Fatalf("blob meta = size %d complete %v", meta.Size, meta.Complete)
	}
	// Read the first and last bytes without materialising the stream.
	reader := blob.NewReader(st, 4, 1)
	reader.CASDir = manager.upload.CASDir
	var first = make([]byte, 8)
	if _, err := reader.StreamRaw(context.Background(), tenant, meta.BlobID, meta, sliceWriter{first}, 0, 8); err != nil {
		t.Fatalf("read first bytes: %v", err)
	}
	last := make([]byte, 8)
	if _, err := reader.StreamRaw(context.Background(), tenant, meta.BlobID, meta, sliceWriter{last}, size-8, 8); err != nil {
		t.Fatalf("read last bytes: %v", err)
	}
	for i := range first {
		if first[i] != 0 || last[i] != 0 {
			t.Fatalf("unexpected payload byte at %d: first=%d last=%d", i, first[i], last[i])
		}
	}
	growth := peak.Load() - baseline.HeapAlloc
	t.Logf("200MB run: elapsed=%s peakHeapGrowth=%dMB", elapsed, growth>>20)
	if growth > 128<<20 {
		t.Fatalf("heap grew by %dMB for a %dMB stream; output is not bounded", growth>>20, size>>20)
	}
}

type sliceWriter struct{ buf []byte }

func (w sliceWriter) Write(p []byte) (int, error) {
	n := copy(w.buf, p)
	if n < len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}
