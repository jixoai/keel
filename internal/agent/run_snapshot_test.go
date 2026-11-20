package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/toolsurface"
)

// TestRunSnapshotPinsTheInjectedSurface proves the three run snapshot values
// are recorded from the attempt itself: the config identity the host pinned,
// the log position the attempt starts at, and the hash of the exact tool slice
// injected into the loop.
func TestRunSnapshotPinsTheInjectedSurface(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	r, _ := newTestRunner(t, st)
	const configHash = "sha256:config-one"
	r.ConfigHash = configHash

	inputSeq := commitInput(t, k, scope, "r1", "hello", "", nil)
	lease := claimScope(t, k, tenant, session)
	res, err := r.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor, ReadThrough: res.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if res.Summary.ConfigHashBefore != configHash || res.Summary.ConfigHashAfter != configHash {
		t.Fatalf("config snapshot = %q/%q, want %q", res.Summary.ConfigHashBefore, res.Summary.ConfigHashAfter, configHash)
	}
	if res.Summary.AnchorEventSeq != inputSeq {
		t.Fatalf("anchorEventSeq = %d, want the run-start input seq %d", res.Summary.AnchorEventSeq, inputSeq)
	}
	wantManifest, err := toolsurface.ManifestHash(r.Tools.List())
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.ToolManifestHash != wantManifest {
		t.Fatalf("toolManifestHash = %q, want %q", res.Summary.ToolManifestHash, wantManifest)
	}
	if res.Summary.ToolManifestHash == "" {
		t.Fatal("toolManifestHash is empty")
	}
}

// TestRunSnapshotIgnoresAMidRunToolSurfaceChange is the spec's "adding an MCP
// server during a run does not change this run's manifest" scenario at the
// runner seam: the hash is taken from the slice handed to the loop, so a
// registration that lands after injection only moves the next attempt's hash.
func TestRunSnapshotIgnoresAMidRunToolSurfaceChange(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	r, _ := newTestRunner(t, st)

	registry := agenttool.NewToolRegistry()
	for _, name := range []string{"read", "write"} {
		if err := registry.Register(stubTool{name: name}); err != nil {
			t.Fatal(err)
		}
	}
	r.Assembly = staticAssembly{prompt: "snapshot", tools: registry}
	base := FakeStream()
	var once sync.Once
	r.Stream = func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		// A host refresh/registration lands while the attempt is running. It
		// must not be visible in this attempt's manifest hash (nor its tools).
		once.Do(func() {
			if err := registry.Register(stubTool{name: "mcp__late"}); err != nil {
				t.Errorf("register late tool: %v", err)
			}
		})
		return base(ctx, model, llm, cfg)
	}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	lease := claimScope(t, k, tenant, session)
	res, err := r.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor, ReadThrough: res.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	wantManifest, err := toolsurface.ManifestHash([]agentcore.AgentTool{stubTool{name: "read"}, stubTool{name: "write"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.ToolManifestHash != wantManifest {
		t.Fatalf("toolManifestHash = %q, want the pre-injection manifest %q", res.Summary.ToolManifestHash, wantManifest)
	}
	afterManifest, err := toolsurface.RegistryHash(registry)
	if err != nil {
		t.Fatal(err)
	}
	if afterManifest == res.Summary.ToolManifestHash {
		t.Fatal("the late registration did not change the registry, so the test proves nothing")
	}
}

// TestRunSnapshotAnchorFollowsConfigRotation proves the next run after a
// rotation uses the new config identity and anchors at the rotation event;
// without a rotation event the anchor is the run-start input sequence.
func TestRunSnapshotAnchorFollowsConfigRotation(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	first, _ := newTestRunner(t, st)
	first.ConfigHash = "sha256:config-one"
	firstSeq := commitInput(t, k, scope, "r1", "one", "", nil)
	lease := claimScope(t, k, tenant, session)
	res, err := first.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor, ReadThrough: res.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish first: %v", err)
	}
	if res.Summary.AnchorEventSeq != firstSeq {
		t.Fatalf("first anchor = %d, want %d", res.Summary.AnchorEventSeq, firstSeq)
	}
	if res.Summary.ConfigHashBefore != "sha256:config-one" {
		t.Fatalf("first config hash = %q", res.Summary.ConfigHashBefore)
	}

	rotated, err := k.Commit(ctx, kernel.CommitRequest{
		Scope: scope, Kind: kernel.KindConfigRotated, System: true,
		ConfigKey: "sha256:config-two",
		Payload:   mustJSON(map[string]string{"configHash": "sha256:config-two"}),
	})
	if err != nil {
		t.Fatalf("commit config rotation: %v", err)
	}

	second, _ := newTestRunner(t, st)
	second.ConfigHash = "sha256:config-two"
	commitInput(t, k, scope, "r2", "two", "", nil)
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	lease = claimScope(t, k, tenant, session)
	res, err = second.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor, ReadThrough: res.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish second: %v", err)
	}
	if res.Summary.AnchorEventSeq != rotated.Seq {
		t.Fatalf("second anchor = %d, want the rotation seq %d", res.Summary.AnchorEventSeq, rotated.Seq)
	}
	if res.Summary.ConfigHashBefore != "sha256:config-two" {
		t.Fatalf("second config hash = %q, want the rotated identity", res.Summary.ConfigHashBefore)
	}
	if res.Summary.ToolManifestHash == "" {
		t.Fatal("second run has no tool manifest hash")
	}
}

// TestWorkerRunFinishedCarriesThePinnedSnapshot proves the real worker commit
// path: the attempt summary in the log carries all three values, with the config
// hash derived from the loadout the run pinned (not from process memory).
func TestWorkerRunFinishedCarriesThePinnedSnapshot(t *testing.T) {
	ctx := context.Background()
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	tenant, session := uuid.New(), uuid.New()
	mem.CreateSession(tenant, session)
	mem.EnqueueRun(tenant, session)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	input, err := k.Commit(ctx, kernel.CommitRequest{
		Scope: scope, Kind: kernel.KindUserMessage, RequestID: "r1",
		Payload: mustJSON(map[string]string{"text": "hello"}),
	})
	if err != nil {
		t.Fatalf("commit input: %v", err)
	}
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	registerTestkitTools(t, runner)
	binding := &bindingStub{
		runner: runner,
		ref: LoadoutRef{
			ID: "default", Revision: 7, Generation: 3,
			ConfigHash: ConfigHashForLoadout("default", 7),
		},
	}
	worker := &Worker{
		Leases: k, Runs: mirrorStub{}, Binding: binding, Owner: "snapshot-test",
		Lease: 30 * time.Second, MaxAttempts: 3, Logger: discardLog(), Committer: k,
	}
	lease, err := k.ClaimScope(ctx, scope, "snapshot-test", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(ctx, sem, lease)

	events, err := k.LoadEvents(ctx, scope, 0, 500)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var summary map[string]any
	for _, ev := range events {
		if ev.Kind != kernel.KindRunFinished {
			continue
		}
		if err := json.Unmarshal(ev.Payload, &summary); err != nil {
			t.Fatalf("decode run.finished: %v", err)
		}
	}
	if summary == nil {
		t.Fatal("no run.finished event")
	}
	wantConfig := ConfigHashForLoadout("default", 7)
	if summary["configHashBefore"] != wantConfig || summary["configHashAfter"] != wantConfig {
		t.Fatalf("config hashes = %v/%v, want %q", summary["configHashBefore"], summary["configHashAfter"], wantConfig)
	}
	anchor, ok := summary["anchorEventSeq"].(float64)
	if !ok {
		t.Fatalf("anchorEventSeq = %v, want a JSON number", summary["anchorEventSeq"])
	}
	if got := int64(anchor); got != input.Seq {
		t.Fatalf("anchorEventSeq = %d, want %d", got, input.Seq)
	}
	wantManifest, err := toolsurface.ManifestHash(runner.Tools.List())
	if err != nil {
		t.Fatal(err)
	}
	if summary["toolManifestHash"] != wantManifest {
		t.Fatalf("toolManifestHash = %v, want %q", summary["toolManifestHash"], wantManifest)
	}
}

// TestConfigHashForLoadoutIsRevisionScoped pins the digest contract: a
// configuration revision moves the hash, the same revision is stable, and the
// loadout id is part of the identity.
func TestConfigHashForLoadoutIsRevisionScoped(t *testing.T) {
	first := ConfigHashForLoadout("default", 7)
	if again := ConfigHashForLoadout("default", 7); again != first {
		t.Fatalf("config hash is not stable: %q != %q", first, again)
	}
	if next := ConfigHashForLoadout("default", 8); next == first {
		t.Fatalf("revision 8 kept hash %q", first)
	}
	if other := ConfigHashForLoadout("tenant-b", 7); other == first {
		t.Fatalf("another loadout kept hash %q", first)
	}
}
