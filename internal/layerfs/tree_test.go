package layerfs

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestTreeHashIsStableAcrossWalks(t *testing.T) {
	store := newTestStore(t, Options{})
	left, right := t.TempDir(), t.TempDir()
	writeFile(t, left, "a.txt", "alpha", 0o644)
	writeFile(t, left, "dir/b.txt", "beta", 0o600)
	writeFile(t, right, "dir/b.txt", "beta", 0o600)
	writeFile(t, right, "a.txt", "alpha", 0o644)
	first, err := store.Capture(left, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Capture(right, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Tree != second.Tree {
		t.Fatalf("tree hashes differ: %s vs %s", first.Tree, second.Tree)
	}
	if first.Count != 2 || second.Count != 2 {
		t.Fatalf("counts = %d/%d", first.Count, second.Count)
	}
}

func TestTreeDedupesIdenticalSubtrees(t *testing.T) {
	store := newTestStore(t, Options{})
	root := t.TempDir()
	writeFile(t, root, "one/shared.txt", "same", 0o644)
	writeFile(t, root, "two/shared.txt", "same", 0o644)
	snapshot, err := store.Capture(root, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Count != 2 {
		t.Fatalf("count = %d", snapshot.Count)
	}
	if snapshot.NewObjects != 1 {
		t.Fatalf("new objects = %d, want 1 (identical bytes dedupe)", snapshot.NewObjects)
	}
	entries, err := store.Tree(snapshot.Tree)
	if err != nil {
		t.Fatal(err)
	}
	var one, two Hash
	for _, entry := range entries {
		switch entry.Name {
		case "one":
			one = entry.Hash
		case "two":
			two = entry.Hash
		}
	}
	if one == "" || two == "" || one != two {
		t.Fatalf("subtree hashes = %s/%s, want identical", one, two)
	}
}

func TestCaptureIgnoresGitObjectsByDefault(t *testing.T) {
	store := newTestStore(t, Options{})
	root := t.TempDir()
	writeFile(t, root, "src/main.go", "package main", 0o644)
	writeFile(t, root, ".git/objects/aa/bb", "object", 0o644)
	writeFile(t, root, ".git/config", "config", 0o644)
	snapshot, err := store.Capture(root, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range snapshot.Files {
		if file.Path == ".git/objects/aa/bb" {
			t.Fatal(".git/objects was captured")
		}
	}
	foundConfig := false
	for _, file := range snapshot.Files {
		if file.Path == ".git/config" {
			foundConfig = true
		}
	}
	if !foundConfig {
		t.Fatal(".git/config should be captured (only .git/objects is ignored by default)")
	}
}

func TestCaptureCacheSkipsUnchangedFiles(t *testing.T) {
	store := newTestStore(t, Options{})
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha", 0o644)
	writeFile(t, root, "dir/b.txt", "beta", 0o644)
	cache := NewStatCache()
	first, err := store.Capture(root, CaptureOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if first.NewObjects != 2 {
		t.Fatalf("first new objects = %d", first.NewObjects)
	}
	second, err := store.Capture(root, CaptureOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if second.NewObjects != 0 || second.ReusedObjects != 2 {
		t.Fatalf("second capture new/reused = %d/%d, want 0/2", second.NewObjects, second.ReusedObjects)
	}
	if second.Tree != first.Tree {
		t.Fatalf("tree changed without a content change: %s vs %s", second.Tree, first.Tree)
	}
	if got := cache.PutCount(); got != 2 {
		t.Fatalf("stat cache rehashed unchanged files: %d hashes, want 2", got)
	}
	// A changed file is rehashed, the other is not.
	writeFile(t, root, "a.txt", "alpha2", 0o644)
	third, err := store.Capture(root, CaptureOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if third.NewObjects != 1 || third.ReusedObjects != 1 {
		t.Fatalf("third capture new/reused = %d/%d", third.NewObjects, third.ReusedObjects)
	}
	if got := cache.PutCount(); got != 3 {
		t.Fatalf("stat cache hashed %d files after one change, want 3", got)
	}
}

func TestMaterializeRoundTrip(t *testing.T) {
	store := newTestStore(t, Options{})
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha", 0o640)
	writeFile(t, root, "nested/deep/b.bin", string(bytes.Repeat([]byte{0, 1, 2, 3}, 1024)), 0o600)
	writeFile(t, root, "empty.txt", "", 0o644)
	snapshot, err := store.Capture(root, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := store.Materialize(snapshot.Tree, target); err != nil {
		t.Fatal(err)
	}
	for _, file := range snapshot.Files {
		original, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatal(err)
		}
		materialized, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatalf("materialized %s: %v", file.Path, err)
		}
		if !bytes.Equal(original, materialized) {
			t.Fatalf("materialized %s differs", file.Path)
		}
	}
	info, err := os.Stat(filepath.Join(target, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
}

func TestMaterializeRejectsTraversalNames(t *testing.T) {
	store := newTestStore(t, Options{})
	hash := HashBytes([]byte("x"))
	if _, _, err := store.PutBytes([]byte("x")); err != nil {
		t.Fatal(err)
	}
	raw := []byte("keel-tree-v1\n../escape\x00file\x000644\x00" + string(hash) + "\x000\n")
	treeHash, _, err := store.PutBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Materialize(treeHash, t.TempDir()); err == nil {
		t.Fatal("materialize accepted a traversal entry")
	}
}

func TestReadRangeOnMissingObject(t *testing.T) {
	store := newTestStore(t, Options{})
	var out bytes.Buffer
	if _, err := store.ReadRange(HashBytes([]byte("missing")), 0, 0, &out); err == nil {
		t.Fatal("read of a missing object succeeded")
	}
}

func TestObjectStatAndHas(t *testing.T) {
	store := newTestStore(t, Options{})
	hash, size, err := store.Put(bytes.NewReader([]byte("stat me")))
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.Stat(hash)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != size || !store.Has(hash) {
		t.Fatalf("stat = %+v", info)
	}
	if store.Has(HashBytes([]byte("absent"))) {
		t.Fatal("Has reported an absent object")
	}
}

func TestPutFileStreams(t *testing.T) {
	store := newTestStore(t, Options{})
	path := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(path, bytes.Repeat([]byte("z"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := store.PutFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Size != 1<<20 || !result.Created {
		t.Fatalf("result = %+v", result)
	}
	object, err := store.Open(result.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer object.Close()
	n, err := io.Copy(io.Discard, object)
	if err != nil || n != 1<<20 {
		t.Fatalf("copy = %d/%v", n, err)
	}
}
