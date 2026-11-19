package gbashfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	gbash "github.com/ewhauser/gbash"

	"github.com/jixoai/keel/internal/layerfs"
)

func newWorkspace(t *testing.T) (*Workspace, *layerfs.Store) {
	t.Helper()
	store, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace, err := NewWorkspace(store)
	if err != nil {
		t.Fatal(err)
	}
	return workspace, store
}

func TestWorkspaceFileOperations(t *testing.T) {
	workspace, _ := newWorkspace(t)
	ctx := context.Background()
	if err := workspace.MkdirAll(ctx, "/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := workspace.OpenFile(ctx, "/dir/a.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// Append through a second handle.
	file, err = workspace.OpenFile(ctx, "/dir/a.txt", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(" world")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := workspace.Stat(ctx, "/dir/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len("hello world")) {
		t.Fatalf("size = %d", info.Size())
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	reader, err := workspace.Open(ctx, "/dir/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(raw) != "hello world" {
		t.Fatalf("read = %q/%v", raw, err)
	}
	entries, err := workspace.ReadDir(ctx, "/dir")
	if err != nil || len(entries) != 1 || entries[0].Name() != "a.txt" {
		t.Fatalf("readdir = %v/%v", entries, err)
	}
	// Rename and remove.
	if err := workspace.Rename(ctx, "/dir/a.txt", "/dir/b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Stat(ctx, "/dir/a.txt"); err == nil {
		t.Fatal("old path still exists after rename")
	}
	if _, err := workspace.Stat(ctx, "/dir/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Remove(ctx, "/dir/b.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Stat(ctx, "/dir/b.txt"); err == nil {
		t.Fatal("removed file still exists")
	}
}

func TestWorkspaceSymlinks(t *testing.T) {
	workspace, _ := newWorkspace(t)
	ctx := context.Background()
	if err := workspace.MkdirAll(ctx, "/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteFile("d/target.txt", 0o644, strings.NewReader("target")); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Symlink(ctx, "target.txt", "/d/link"); err != nil {
		t.Fatal(err)
	}
	target, err := workspace.Readlink(ctx, "/d/link")
	if err != nil || target != "target.txt" {
		t.Fatalf("readlink = %q/%v", target, err)
	}
	resolved, err := workspace.Realpath(ctx, "/d/link")
	if err != nil || resolved != "/d/target.txt" {
		t.Fatalf("realpath = %q/%v", resolved, err)
	}
	info, err := workspace.Stat(ctx, "/d/link")
	if err != nil || info.IsDir() || info.Size() != int64(len("target")) {
		t.Fatalf("stat through symlink = %+v/%v", info, err)
	}
	lstat, err := workspace.Lstat(ctx, "/d/link")
	if err != nil || lstat.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("lstat = %v/%v", lstat, err)
	}
}

func TestWorkspaceLargeFileStreams(t *testing.T) {
	workspace, _ := newWorkspace(t)
	payload := bytes.Repeat([]byte("0123456789abcdef"), (4<<20)/16)
	if err := workspace.WriteFile("big.bin", 0o644, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	reader, err := workspace.Open(context.Background(), "/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("large file read back %d bytes, want %d", len(got), len(payload))
	}
}

func TestMountRejectsRootAndReservedPaths(t *testing.T) {
	for _, point := range []string{"/", "/bin", "/usr/bin", "/tmp", "/bin/tools"} {
		if _, err := ValidateMountPoint(point); err == nil {
			t.Fatalf("mount point %q was accepted", point)
		}
	}
	if point, err := ValidateMountPoint(""); err != nil || point != DefaultMountPoint {
		t.Fatalf("default mount point = %q/%v", point, err)
	}
}

func TestGBashUsesLayerfsAtANonRootMount(t *testing.T) {
	workspace, store := newWorkspace(t)
	mount, err := NewMount(MountOptions{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := gbash.New(gbash.WithFileSystem(mount.Config))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	session, err := runtime.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.Exec(ctx, &gbash.ExecutionRequest{
		Script: `echo hello > /workspace/a.txt
mkdir -p /workspace/d
echo nested > /workspace/d/b.txt
cat /workspace/a.txt`,
		WorkDir: "/workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", result.ExitCode, result.Stderr)
	}
	if strings.TrimSpace(result.Stdout) != "hello" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	snapshot := workspace.Snapshot()
	paths := map[string]bool{}
	for _, entry := range snapshot {
		paths[entry.Path] = true
		if !store.Has(entry.Hash) {
			t.Fatalf("snapshot entry %s points at a missing object", entry.Path)
		}
	}
	for _, want := range []string{"a.txt", "d/b.txt"} {
		if !paths[want] {
			t.Fatalf("bash write %s did not land in the workspace: %v", want, paths)
		}
	}
	// The sandbox layout must never reach layerfs.
	for candidate := range workspace.nodes {
		if strings.HasPrefix(candidate, "/bin") || strings.HasPrefix(candidate, "/usr/bin") || strings.HasPrefix(candidate, "/tmp") {
			t.Fatalf("sandbox layout path %q polluted the layerfs workspace", candidate)
		}
	}
	// Write-through: a keel-side write is immediately visible to bash.
	if err := workspace.WriteFile("a.txt", 0o644, strings.NewReader("tool-wrote-this")); err != nil {
		t.Fatal(err)
	}
	result, err = session.Exec(ctx, &gbash.ExecutionRequest{Script: `cat /workspace/a.txt`, WorkDir: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Stdout) != "tool-wrote-this" {
		t.Fatalf("bash after write-through = %q", result.Stdout)
	}
}
