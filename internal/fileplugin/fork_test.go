package fileplugin_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/layerfs"
	"github.com/jixoai/keel/internal/sessionfork"
)

// fakeForks answers the fork fact of one child session.
type fakeForks struct {
	child uuid.UUID
	fact  sessionfork.Fact
}

func (f fakeForks) SessionFork(_ context.Context, tenantID, sessionID uuid.UUID) (sessionfork.Fact, bool, error) {
	if sessionID != f.child {
		return sessionfork.Fact{}, false, nil
	}
	return f.fact, true, nil
}

// staticSnapshots is the execution-space snapshot seam: the capture sees
// exactly these files.
type staticSnapshots struct {
	snapshot layerfs.Snapshot
}

func (s staticSnapshots) Snapshot(context.Context, fileplugin.Scope) (layerfs.Snapshot, error) {
	return s.snapshot, nil
}

// TestForkFileViewIsAnOverlay pins the file half of fork: the child reads the
// parent snapshot's files by tree hash, its own writes win, and a capture of an
// unchanged inherited file does not copy the parent's manifest row.
func TestForkFileViewIsAnOverlay(t *testing.T) {
	store, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	baseHash, _, err := store.PutBytes([]byte("base\n"))
	if err != nil {
		t.Fatal(err)
	}
	baseTree, err := store.PutTree([]layerfs.TreeEntry{
		{Name: "shared.txt", Kind: layerfs.TreeFile, Hash: baseHash, Mode: 0o644, Size: 5},
	})
	if err != nil {
		t.Fatal(err)
	}

	tenant, child := uuid.New(), uuid.New()
	mem := kernel.NewMemStore()
	mem.CreateSession(tenant, child)
	mem.EnqueueRun(tenant, child)
	kern := kernel.New(mem, kernel.Options{})
	ctx := context.Background()
	if _, err := kern.ClaimScope(ctx, kernel.Scope{TenantID: tenant, SessionID: child}, "fork-file-test", time.Hour); err != nil {
		t.Fatal(err)
	}
	manifest := fileplugin.NewMemoryManifest()
	service, err := fileplugin.New(fileplugin.Options{
		Store:     store,
		Manifest:  manifest,
		Committer: kern,
		Leases:    kern,
		Forks:     fakeForks{child: child, fact: sessionfork.Fact{ParentSessionID: uuid.New(), OriginSeq: 10, TreeHash: baseTree.Prefixed()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := fileplugin.Scope{TenantID: tenant, SessionID: child}
	readCtx := fileplugin.WithScope(ctx, scope)

	read, err := service.Read(readCtx, mustJSON(t, map[string]any{"path": "shared.txt"}))
	if err != nil {
		t.Fatalf("read inherited: %v", err)
	}
	if read.Content != "base\n" || read.Hash != baseHash.Prefixed() {
		t.Fatalf("inherited read = %+v", read)
	}
	if read.Seq != 10 {
		t.Fatalf("inherited seq = %d, want the fork origin", read.Seq)
	}
	if entries, err := service.List(readCtx, scope); err != nil || len(entries) != 1 || entries[0].Path != "shared.txt" {
		t.Fatalf("list = %+v err=%v", entries, err)
	}

	// The child writes the inherited path: the precondition check must see the
	// inherited hash, and the new row must win over the overlay.
	writeCtx := fileplugin.WithCallID(fileplugin.WithScope(ctx, scope), "call-write")
	written, err := service.Write(writeCtx, mustJSON(t, map[string]any{
		"path": "shared.txt", "content": "child\n", "expectedHash": baseHash.Prefixed(),
	}))
	if err != nil {
		t.Fatalf("write inherited: %v", err)
	}
	if written.Created {
		t.Fatalf("writing an inherited file reported Created: %+v", written)
	}
	read, err = service.Read(readCtx, mustJSON(t, map[string]any{"path": "shared.txt"}))
	if err != nil || read.Content != "child\n" || read.Hash == baseHash.Prefixed() {
		t.Fatalf("read after write = %+v err=%v", read, err)
	}
	own, err := manifest.List(ctx, tenant, child)
	if err != nil || len(own) != 1 || own[0].Path != "shared.txt" || own[0].Hash == baseHash {
		t.Fatalf("child manifest after write = %+v err=%v", own, err)
	}

	// A second child sees the same inherited view, independent of the first.
	second := uuid.New()
	mem.CreateSession(tenant, second)
	secondScope := fileplugin.Scope{TenantID: tenant, SessionID: second}
	service2, err := fileplugin.New(fileplugin.Options{
		Store:    store,
		Manifest: fileplugin.NewMemoryManifest(),
		Forks:    fakeForks{child: second, fact: sessionfork.Fact{ParentSessionID: uuid.New(), OriginSeq: 10, TreeHash: baseTree.Prefixed()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	secondRead, err := service2.Read(fileplugin.WithScope(ctx, secondScope), mustJSON(t, map[string]any{"path": "shared.txt"}))
	if err != nil || secondRead.Content != "base\n" {
		t.Fatalf("second fork read = %+v err=%v", secondRead, err)
	}
}

// TestForkCaptureDoesNotCopyInheritedManifestRows pins the O(1) rule at the
// capture boundary: an unchanged inherited file is referenced by the child's
// snapshot tree and retained as a ref, but no file_manifest row is written for
// it.
func TestForkCaptureDoesNotCopyInheritedManifestRows(t *testing.T) {
	store, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	baseHash, _, err := store.PutBytes([]byte("base\n"))
	if err != nil {
		t.Fatal(err)
	}
	ownHash, _, err := store.PutBytes([]byte("own\n"))
	if err != nil {
		t.Fatal(err)
	}
	baseTree, err := store.PutTree([]layerfs.TreeEntry{
		{Name: "shared.txt", Kind: layerfs.TreeFile, Hash: baseHash, Mode: 0o644, Size: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshotTree, err := store.PutTree([]layerfs.TreeEntry{
		{Name: "own.txt", Kind: layerfs.TreeFile, Hash: ownHash, Mode: 0o644, Size: 4},
		{Name: "shared.txt", Kind: layerfs.TreeFile, Hash: baseHash, Mode: 0o644, Size: 5},
	})
	if err != nil {
		t.Fatal(err)
	}

	tenant, child := uuid.New(), uuid.New()
	mem := kernel.NewMemStore()
	mem.CreateSession(tenant, child)
	mem.EnqueueRun(tenant, child)
	kern := kernel.New(mem, kernel.Options{})
	ctx := context.Background()
	if _, err := kern.ClaimScope(ctx, kernel.Scope{TenantID: tenant, SessionID: child}, "fork-capture-test", time.Hour); err != nil {
		t.Fatal(err)
	}
	manifest := fileplugin.NewMemoryManifest()
	service, err := fileplugin.New(fileplugin.Options{
		Store:     store,
		Manifest:  manifest,
		Committer: kern,
		Leases:    kern,
		Forks:     fakeForks{child: child, fact: sessionfork.Fact{ParentSessionID: uuid.New(), OriginSeq: 10, TreeHash: baseTree.Prefixed()}},
		SnapshotSource: staticSnapshots{snapshot: layerfs.Snapshot{
			Tree: snapshotTree, Count: 2, Bytes: 9,
			Files: []layerfs.FileEntry{
				{Path: "own.txt", Hash: ownHash, Mode: 0o644, Size: 4},
				{Path: "shared.txt", Hash: baseHash, Mode: 0o644, Size: 5},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := fileplugin.Scope{TenantID: tenant, SessionID: child}
	if _, err := service.CaptureWorkspace(ctx, scope, "call-cap", 0, ""); err != nil {
		t.Fatalf("capture: %v", err)
	}
	own, err := manifest.List(ctx, tenant, child)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 || own[0].Path != "own.txt" {
		t.Fatalf("capture copied inherited rows: %+v", own)
	}
	view, err := service.List(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range view {
		paths = append(paths, entry.Path)
	}
	if strings.Join(paths, ",") != "own.txt,shared.txt" {
		t.Fatalf("merged view = %v", paths)
	}
	// The child snapshot references the inherited bytes (GC must not free
	// them), and the ref proves it without a manifest row.
	refs := store.Refs(baseHash)
	foundChildRef := false
	for _, ref := range refs {
		if ref.Session == child.String() {
			foundChildRef = true
		}
	}
	if !foundChildRef {
		t.Fatalf("inherited blob refs = %+v, want a child snapshot ref", refs)
	}
}
