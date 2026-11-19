package gbashfs_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/layerfs"
	"github.com/jixoai/keel/internal/layerfs/gbashfs"
)

// TestCaptureMatchesDirectoryCapture pins the workspace capture to the same
// canonical form layerfs.Store.Capture produces: the same files and directories
// yield the same file entries and the same root tree hash, no matter whether
// they were indexed in memory or walked on disk. That equality is what lets a
// bash workspace's fs.capture be read by the directory-based capture path
// (materialize/snapshot) without a second tree dialect.
func TestCaptureMatchesDirectoryCapture(t *testing.T) {
	ctx := context.Background()
	store, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	files := map[string]string{
		"a.txt":          "alpha",
		"docs/b.md":      "beta",
		"docs/deep/c":    "alpha", // duplicate content: one object, two paths
		"empty-file.txt": "",
	}
	workspace, err := gbashfs.NewWorkspace(store)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	// The workspace root is 0755; t.TempDir() is 0700 and the root directory's
	// mode is part of the canonical tree.
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := workspace.WriteFile(name, 0o644, strings.NewReader(content)); err != nil {
			t.Fatalf("workspace write %s: %v", name, err)
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The default ignore list keeps .git/objects out of the capture in both
	// models (it is itself content-addressed).
	ignoredContent := "ignored-object"
	if err := workspace.WriteFile(".git/objects/pack", 0o644, strings.NewReader(ignoredContent)); err != nil {
		t.Fatal(err)
	}
	ignoredTarget := filepath.Join(root, ".git", "objects", "pack")
	if err := os.MkdirAll(filepath.Dir(ignoredTarget), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ignoredTarget, []byte(ignoredContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// An empty directory is part of the tree in both models.
	if err := workspace.MkdirAll(ctx, "/empty-dir", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink is not representable in the layerfs tree; both captures skip it.
	if err := workspace.Symlink(ctx, "/a.txt", "/link.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}

	workspaceSnapshot, err := workspace.Capture(ctx)
	if err != nil {
		t.Fatalf("workspace capture: %v", err)
	}
	directorySnapshot, err := store.Capture(root, layerfs.CaptureOptions{})
	if err != nil {
		t.Fatalf("directory capture: %v", err)
	}
	if workspaceSnapshot.Tree != directorySnapshot.Tree {
		t.Fatalf("tree hashes differ: workspace %s, directory %s", workspaceSnapshot.Tree, directorySnapshot.Tree)
	}
	if workspaceSnapshot.Count != directorySnapshot.Count || workspaceSnapshot.Count != len(files) {
		t.Fatalf("count: workspace %d, directory %d, want %d", workspaceSnapshot.Count, directorySnapshot.Count, len(files))
	}
	if workspaceSnapshot.Bytes != directorySnapshot.Bytes {
		t.Fatalf("bytes: workspace %d, directory %d", workspaceSnapshot.Bytes, directorySnapshot.Bytes)
	}
	if workspaceSnapshot.Skipped != 1 || directorySnapshot.Skipped != 1 {
		t.Fatalf("skipped: workspace %d, directory %d, want the symlink skipped in both", workspaceSnapshot.Skipped, directorySnapshot.Skipped)
	}
	if len(workspaceSnapshot.Files) != len(directorySnapshot.Files) {
		t.Fatalf("file entries: workspace %d, directory %d", len(workspaceSnapshot.Files), len(directorySnapshot.Files))
	}
	for index := range workspaceSnapshot.Files {
		got, want := workspaceSnapshot.Files[index], directorySnapshot.Files[index]
		if got != want {
			t.Fatalf("file entry %d: workspace %+v, directory %+v", index, got, want)
		}
	}
	// Duplicate content is one object: the two entries share the hash.
	hashes := map[layerfs.Hash]int{}
	for _, entry := range workspaceSnapshot.Files {
		hashes[entry.Hash]++
	}
	if hashes[layerfs.HashBytes([]byte("alpha"))] != 2 {
		t.Fatalf("duplicate content hashes = %v", hashes)
	}
	if mode := func() fs.FileMode {
		for _, entry := range workspaceSnapshot.Files {
			if entry.Path == "docs/b.md" {
				return fs.FileMode(entry.Mode)
			}
		}
		return 0
	}(); mode != 0o644 {
		t.Fatalf("file mode = %04o", mode)
	}
}

// TestCaptureEmptyWorkspace pins that a run that wrote nothing still settles a
// valid (empty) tree instead of failing: capture runs once per bash call.
func TestCaptureEmptyWorkspace(t *testing.T) {
	store, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspace, err := gbashfs.NewWorkspace(store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := workspace.Capture(context.Background())
	if err != nil {
		t.Fatalf("empty capture: %v", err)
	}
	if !snapshot.Tree.Valid() || snapshot.Count != 0 || snapshot.Bytes != 0 {
		t.Fatalf("empty snapshot = %+v", snapshot)
	}
	if _, err := store.Tree(snapshot.Tree); err != nil {
		t.Fatalf("empty tree not readable: %v", err)
	}
}
