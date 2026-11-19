package fileplugin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/layerfs"
)

type harness struct {
	store    *layerfs.Store
	kernel   *kernel.Kernel
	mem      *kernel.MemStore
	manifest *fileplugin.MemoryManifest
	service  *fileplugin.Service
	surface  *fileplugin.DirSurface
	scope    fileplugin.Scope
	lease    kernel.Lease
	ctx      context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mem := kernel.NewMemStore()
	tenantID, sessionID := uuid.New(), uuid.New()
	mem.CreateSession(tenantID, sessionID)
	mem.EnqueueRun(tenantID, sessionID)
	ctx := context.Background()
	kern := kernel.New(mem, kernel.Options{})
	// The 100k-file capture test creates its fixture while holding this lease;
	// an hour keeps the fence live without weakening the production check.
	lease, err := kern.ClaimScope(ctx, kernel.Scope{TenantID: tenantID, SessionID: sessionID}, "fileplugin-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	manifest := fileplugin.NewMemoryManifest()
	surface := &fileplugin.DirSurface{Root: t.TempDir()}
	service, err := fileplugin.New(fileplugin.Options{
		Store:       store,
		Manifest:    manifest,
		Committer:   kern,
		Leases:      kern,
		Surface:     surface,
		CaptureRoot: surface.Root,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{
		store: store, kernel: kern, mem: mem, manifest: manifest, service: service,
		surface: surface, scope: fileplugin.Scope{TenantID: tenantID, SessionID: sessionID}, lease: lease, ctx: ctx,
	}
}

func (h *harness) call(callID string) context.Context {
	ctx := fileplugin.WithScope(h.ctx, h.scope)
	return fileplugin.WithCallID(ctx, callID)
}

func (h *harness) write(t *testing.T, callID, path, content string) fileplugin.WriteResult {
	t.Helper()
	result, err := h.service.Write(h.call(callID), mustJSON(t, map[string]any{"path": path, "content": content}))
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return result
}

func (h *harness) read(t *testing.T, callID, path string, offset, limit int) fileplugin.ReadResult {
	t.Helper()
	result, err := h.service.Read(h.call(callID), mustJSON(t, map[string]any{"path": path, "offset": offset, "limit": limit}))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return result
}

func (h *harness) events(t *testing.T) []kernel.Event {
	t.Helper()
	events, err := h.kernel.LoadEvents(h.ctx, kernel.Scope{TenantID: h.scope.TenantID, SessionID: h.scope.SessionID}, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWriteReadPagingAndPreconditions(t *testing.T) {
	h := newHarness(t)
	var builder strings.Builder
	for line := 0; line < 1000; line++ {
		fmt.Fprintf(&builder, "line-%04d\n", line)
	}
	first := h.write(t, "call-w1", "notes/long.txt", builder.String())
	if !first.Created || first.Seq == 0 {
		t.Fatalf("first write = %+v", first)
	}
	if first.Hash != layerfs.HashBytes([]byte(builder.String())).Prefixed() {
		t.Fatalf("hash = %s", first.Hash)
	}
	// Write-through: the execution space already has the bytes.
	surfaceBytes, err := os.ReadFile(filepath.Join(h.surface.Root, "notes", "long.txt"))
	if err != nil || string(surfaceBytes) != builder.String() {
		t.Fatalf("write-through = %d bytes, err=%v", len(surfaceBytes), err)
	}
	defaultPage := h.read(t, "call-r0", "notes/long.txt", 0, 0)
	if defaultPage.ReturnedLines != fileplugin.DefaultReadLines || defaultPage.TotalLines != 1000 || !defaultPage.Truncated {
		t.Fatalf("default page = %+v", defaultPage)
	}
	page := h.read(t, "call-r1", "notes/long.txt", 100, 10)
	if page.TotalLines != 1000 || page.ReturnedLines != 10 || page.NextOffset != 110 || !page.Truncated {
		t.Fatalf("page = %+v", page)
	}
	if !strings.HasPrefix(page.Content, "line-0100\n") || !strings.Contains(page.Content, "line-0109\n") {
		t.Fatalf("page content = %q", page.Content)
	}
	last := h.read(t, "call-r2", "notes/long.txt", 990, 100)
	if last.ReturnedLines != 10 || last.NextOffset != 1000 || last.Truncated {
		t.Fatalf("last page = %+v", last)
	}
	// A final line without a trailing newline is returned verbatim.
	h.write(t, "call-w-nl", "no-newline.txt", "abc")
	if got := h.read(t, "call-r-nl", "no-newline.txt", 0, 10).Content; got != "abc" {
		t.Fatalf("no-newline read = %q", got)
	}
	// A write to an existing file needs the hash the model read.
	if _, err := h.service.Write(h.call("call-w2"), mustJSON(t, map[string]any{"path": "notes/long.txt", "content": "x"})); fileplugin.CodeOf(err) != fileplugin.CodeConflict {
		t.Fatalf("write without expectedHash = %v", err)
	}
	if _, err := h.service.Write(h.call("call-w3"), mustJSON(t, map[string]any{
		"path": "notes/long.txt", "content": "x", "expectedHash": layerfs.HashBytes([]byte("other")).Prefixed(),
	})); fileplugin.CodeOf(err) != fileplugin.CodeConflict {
		t.Fatalf("write with a stale hash = %v", err)
	}
	updated, err := h.service.Write(h.call("call-w4"), mustJSON(t, map[string]any{
		"path": "notes/long.txt", "content": "replaced\n", "expectedHash": first.Hash,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Hash == first.Hash {
		t.Fatal("write did not change the hash")
	}
	if _, err := h.service.Write(h.call("call-w5"), mustJSON(t, map[string]any{
		"path": "new.txt", "content": "x", "expectedHash": updated.Hash,
	})); fileplugin.CodeOf(err) != fileplugin.CodeConflict {
		t.Fatalf("create with expectedHash = %v", err)
	}
	if _, err := h.service.Read(h.call("call-r3"), mustJSON(t, map[string]any{"path": "absent.txt"})); fileplugin.CodeOf(err) != fileplugin.CodeNotFound {
		t.Fatalf("read missing = %v", err)
	}
}

func TestEditExactUniqueAndReplaceAll(t *testing.T) {
	h := newHarness(t)
	created := h.write(t, "call-w1", "src/a.go", "alpha beta alpha\n")
	// No precondition is a conflict.
	if _, err := h.service.Edit(h.call("call-e1"), mustJSON(t, map[string]any{
		"path": "src/a.go", "oldText": "alpha", "newText": "gamma",
	})); fileplugin.CodeOf(err) != fileplugin.CodeConflict {
		t.Fatalf("edit without expectedHash = %v", err)
	}
	// Ambiguous oldText is a conflict.
	if _, err := h.service.Edit(h.call("call-e2"), mustJSON(t, map[string]any{
		"path": "src/a.go", "oldText": "alpha", "newText": "gamma", "expectedHash": created.Hash,
	})); fileplugin.CodeOf(err) != fileplugin.CodeConflict {
		t.Fatalf("ambiguous edit = %v", err)
	}
	// A unique snippet succeeds.
	unique, err := h.service.Edit(h.call("call-e3"), mustJSON(t, map[string]any{
		"path": "src/a.go", "oldText": "beta", "newText": "BETA", "expectedHash": created.Hash,
	}))
	if err != nil || unique.Replacements != 1 {
		t.Fatalf("unique edit = %+v/%v", unique, err)
	}
	if got := h.read(t, "call-r1", "src/a.go", 0, 10).Content; got != "alpha BETA alpha\n" {
		t.Fatalf("after unique edit = %q", got)
	}
	// replaceAll succeeds and reports both replacements.
	all, err := h.service.Edit(h.call("call-e4"), mustJSON(t, map[string]any{
		"path": "src/a.go", "oldText": "alpha", "newText": "gamma", "expectedHash": unique.Hash, "replaceAll": true,
	}))
	if err != nil || all.Replacements != 2 {
		t.Fatalf("replaceAll = %+v/%v", all, err)
	}
	if got := h.read(t, "call-r2", "src/a.go", 0, 10).Content; got != "gamma BETA gamma\n" {
		t.Fatalf("after replaceAll = %q", got)
	}
	// The execution space saw both edits.
	surfaceBytes, err := os.ReadFile(filepath.Join(h.surface.Root, "src", "a.go"))
	if err != nil || string(surfaceBytes) != "gamma BETA gamma\n" {
		t.Fatalf("write-through after edit = %q/%v", surfaceBytes, err)
	}
	// Missing oldText is a conflict.
	if _, err := h.service.Edit(h.call("call-e5"), mustJSON(t, map[string]any{
		"path": "src/a.go", "oldText": "absent", "newText": "x", "expectedHash": all.Hash,
	})); fileplugin.CodeOf(err) != fileplugin.CodeConflict {
		t.Fatalf("missing oldText = %v", err)
	}
}

func TestWriteIsIdempotentForTheSameToolCall(t *testing.T) {
	h := newHarness(t)
	first := h.write(t, "call-1", "a.txt", "one")
	before := len(h.events(t))
	replay, err := h.service.Write(h.call("call-1"), mustJSON(t, map[string]any{"path": "a.txt", "content": "one"}))
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.Seq != first.Seq {
		t.Fatalf("replay = %+v, first = %+v", replay, first)
	}
	if after := len(h.events(t)); after != before {
		t.Fatalf("replay appended an event: %d -> %d", before, after)
	}
	// A different tool call is a new write fact and needs the new precondition.
	second, err := h.service.Write(h.call("call-2"), mustJSON(t, map[string]any{
		"path": "a.txt", "content": "two", "expectedHash": first.Hash,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if second.Seq <= first.Seq {
		t.Fatalf("second seq = %d, first = %d", second.Seq, first.Seq)
	}
}

func TestMaterializeFromManifestAndSnapshot(t *testing.T) {
	h := newHarness(t)
	h.write(t, "call-w1", "a.txt", "alpha")
	h.write(t, "call-w2", "dir/b.txt", "beta")
	manifestDir := t.TempDir()
	if err := h.service.Materialize(h.ctx, h.scope, manifestDir); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"a.txt": "alpha", "dir/b.txt": "beta"} {
		got, err := os.ReadFile(filepath.Join(manifestDir, filepath.FromSlash(path)))
		if err != nil || string(got) != want {
			t.Fatalf("materialized %s = %q/%v", path, got, err)
		}
	}
	capture, err := h.service.CaptureWorkspace(h.ctx, h.scope, "call-c1", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshotDir := t.TempDir()
	if err := h.service.MaterializeSnapshot(h.ctx, h.scope, capture.Seq, snapshotDir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(snapshotDir, "dir", "b.txt"))
	if err != nil || string(got) != "beta" {
		t.Fatalf("snapshot materialize = %q/%v", got, err)
	}
}

func TestEditLargeFileIsStreamed(t *testing.T) {
	h := newHarness(t)
	payload := strings.Repeat("0123456789abcdef", (2<<20)/16)
	created := h.write(t, "call-w1", "big.txt", payload+"NEEDLE")
	edited, err := h.service.Edit(h.call("call-e1"), mustJSON(t, map[string]any{
		"path": "big.txt", "oldText": "NEEDLE", "newText": "DONE", "expectedHash": created.Hash,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if edited.Size != int64(len(payload)+len("DONE")) {
		t.Fatalf("edited size = %d", edited.Size)
	}
	surfaceBytes, err := os.ReadFile(filepath.Join(h.surface.Root, "big.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(surfaceBytes), "DONE") {
		t.Fatalf("edited content tail = %q", string(surfaceBytes[len(surfaceBytes)-20:]))
	}
}
