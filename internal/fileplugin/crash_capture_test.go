package fileplugin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/kernel"
)

// failCaptureOnce simulates a process dying before the fs.capture commit: the
// first capture commit returns an error, so no event and no manifest row is
// durable; the next process re-runs the capture.
type failCaptureOnce struct {
	kernel.Committer
	failed bool
}

func (f *failCaptureOnce) Commit(ctx context.Context, req kernel.CommitRequest) (kernel.Receipt, error) {
	if req.Kind == kernel.KindFSCapture && !f.failed {
		f.failed = true
		return kernel.Receipt{}, errors.New("simulated SIGKILL before the capture commit")
	}
	return f.Committer.Commit(ctx, req)
}

// TestCaptureCrashBeforeCommitRecoversExactlyOnce is G2's capture crash cell:
// a crash before the capture commit leaves zero fs.capture events; a restarted
// process (fresh service + manifest over the same store) captures the same
// workspace and ends with exactly one fs.capture event and one manifest row.
func TestCaptureCrashBeforeCommitRecoversExactlyOnce(t *testing.T) {
	h := newHarness(t)
	h.write(t, "call-w1", "a.txt", "alpha")

	crashing, err := fileplugin.New(fileplugin.Options{
		Store:       h.store,
		Manifest:    fileplugin.NewMemoryManifest(),
		Committer:   &failCaptureOnce{Committer: h.kernel},
		Leases:      h.kernel,
		Surface:     h.surface,
		CaptureRoot: h.surface.Root,
	})
	if err != nil {
		t.Fatalf("crashing service: %v", err)
	}
	if _, err := crashing.CaptureWorkspace(h.ctx, h.scope, "call-c1", 0, "task-1"); err == nil {
		t.Fatal("the capture with the simulated crash succeeded")
	}
	if got := countCaptures(h.events(t)); got != 0 {
		t.Fatalf("fs.capture after the crash = %d, want 0", got)
	}

	restartedManifest := fileplugin.NewMemoryManifest()
	restarted, err := fileplugin.New(fileplugin.Options{
		Store:       h.store,
		Manifest:    restartedManifest,
		Committer:   h.kernel,
		Leases:      h.kernel,
		Surface:     h.surface,
		CaptureRoot: h.surface.Root,
	})
	if err != nil {
		t.Fatalf("restarted service: %v", err)
	}
	capture, err := restarted.CaptureWorkspace(h.ctx, h.scope, "call-c1", 0, "task-1")
	if err != nil {
		t.Fatalf("recovered capture: %v", err)
	}
	if capture.Seq == 0 {
		t.Fatal("recovered capture has no sequence")
	}
	if got := countCaptures(h.events(t)); got != 1 {
		t.Fatalf("fs.capture after recovery = %d, want exactly 1", got)
	}
	rows, err := restartedManifest.List(h.ctx, h.scope.TenantID, h.scope.SessionID)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != "a.txt" {
		t.Fatalf("manifest rows = %+v, want a.txt", rows)
	}
}

func countCaptures(events []kernel.Event) int {
	count := 0
	for _, event := range events {
		if event.Kind == kernel.KindFSCapture {
			count++
		}
	}
	return count
}
