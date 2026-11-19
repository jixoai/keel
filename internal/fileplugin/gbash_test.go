package fileplugin_test

import (
	"context"
	"strings"
	"testing"
	"time"

	gbash "github.com/ewhauser/gbash"

	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/layerfs/gbashfs"
)

// TestWriteThroughReachesBash is the end-to-end write-through proof: the
// file-plugin writes into layerfs and through to the same gbash workspace the
// sandbox is mounted on, so `cat` sees the bytes without a remount.
func TestWriteThroughReachesBash(t *testing.T) {
	h := newHarness(t)
	workspace, err := gbashfs.NewWorkspace(h.store)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := gbashfs.NewMount(gbashfs.MountOptions{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	service, err := fileplugin.New(fileplugin.Options{
		Store:     h.store,
		Manifest:  h.manifest,
		Committer: h.kernel,
		Leases:    h.kernel,
		Surface:   workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := fileplugin.WithCallID(fileplugin.WithScope(h.ctx, h.scope), "call-w1")
	if _, err := service.Write(ctx, mustJSON(t, map[string]any{"path": "dir/a.txt", "content": "tool-wrote-this"})); err != nil {
		t.Fatal(err)
	}

	runtime, err := gbash.New(gbash.WithFileSystem(mount.Config))
	if err != nil {
		t.Fatal(err)
	}
	sessionCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := runtime.NewSession(sessionCtx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.Exec(sessionCtx, &gbash.ExecutionRequest{Script: `cat /workspace/dir/a.txt`, WorkDir: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != "tool-wrote-this" {
		t.Fatalf("bash read after write-through = %q (exit %d, stderr %q)", result.Stdout, result.ExitCode, result.Stderr)
	}
	// The reverse direction: a bash write lands in the workspace index, which is
	// the capture source for the next fs.capture.
	result, err = session.Exec(sessionCtx, &gbash.ExecutionRequest{Script: `echo from-bash > /workspace/b.txt`, WorkDir: "/workspace"})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("bash write = %+v/%v", result, err)
	}
	found := false
	for _, entry := range workspace.Snapshot() {
		if entry.Path == "b.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("bash write did not land in the workspace snapshot")
	}
}
