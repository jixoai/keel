package layerfs_test

import (
	"testing"

	"github.com/jixoai/keel/internal/layerfs"
)

// The fork view is resolved from one tree hash. These helpers are what make
// "fork inherits the file view without copying manifest rows" real: a path
// lookup walks only the directories on the path, and a flatten decodes the
// inherited file set once.
func TestLookupPathWalksTheTreeWithoutMaterializing(t *testing.T) {
	store, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	aHash, _, err := store.PutBytes([]byte("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	bHash, _, err := store.PutBytes([]byte("beta"))
	if err != nil {
		t.Fatal(err)
	}
	subTree, err := store.PutTree([]layerfs.TreeEntry{{Name: "b.txt", Kind: layerfs.TreeFile, Hash: bHash, Mode: 0o644, Size: 4}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.PutTree([]layerfs.TreeEntry{
		{Name: "a.txt", Kind: layerfs.TreeFile, Hash: aHash, Mode: 0o644, Size: 5},
		{Name: "sub", Kind: layerfs.TreeDir, Hash: subTree, Mode: 0o755},
	})
	if err != nil {
		t.Fatal(err)
	}

	entry, found, err := store.LookupPath(root, "a.txt")
	if err != nil || !found || entry.Hash != aHash {
		t.Fatalf("lookup a.txt = %+v found=%v err=%v", entry, found, err)
	}
	entry, found, err = store.LookupPath(root, "sub/b.txt")
	if err != nil || !found || entry.Hash != bHash {
		t.Fatalf("lookup sub/b.txt = %+v found=%v err=%v", entry, found, err)
	}
	if _, found, err := store.LookupPath(root, "sub/missing.txt"); err != nil || found {
		t.Fatalf("lookup missing = found=%v err=%v", found, err)
	}
	if _, found, err := store.LookupPath(root, "sub"); err != nil || found {
		t.Fatalf("a directory is not a file: found=%v err=%v", found, err)
	}
	if _, _, err := store.LookupPath(root, "../escape"); err == nil {
		t.Fatal("a parent traversal must be refused")
	}

	files, err := store.FlattenTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "a.txt" || files[1].Path != "sub/b.txt" {
		t.Fatalf("flatten = %+v", files)
	}
}
