package servicehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/servicegraph"
	"github.com/jixoai/keel/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeMirror is the run-row mirror seam; the worker only reads it for logs.
type fakeMirror struct{}

func (fakeMirror) GetRun(context.Context, uuid.UUID, uuid.UUID) (store.Run, error) {
	return store.Run{}, store.ErrNoRun
}

func bootForTest(t *testing.T, tenant host.TenantConfig, opts loadout.Options) *Runtime {
	t.Helper()
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	rt, err := Boot(context.Background(), Config{
		Seams:     Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:    tenant,
		Options:   opts,
		RuntimeID: "test-runtime",
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() {
		if err := rt.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return rt
}

// TestDefaultAssemblyMatchesDirectHostAssembly is the wiring-equivalence test:
// the host the graph assembles is exactly the host the pre-graph boot code built
// for the same configuration (same prompt, same tools).
func TestDefaultAssemblyMatchesDirectHostAssembly(t *testing.T) {
	tenant := host.TenantConfig{
		Agent: host.AgentConfig{SystemPrompt: "You are the test agent."},
	}
	rt := bootForTest(t, tenant, loadout.Options{})

	want, _, err := host.New(host.Config{
		Preamble:        tenant.Agent.SystemPrompt,
		IncludeBuiltins: true,
	})
	if err != nil {
		t.Fatalf("direct host: %v", err)
	}
	got := rt.Host()
	if got == nil {
		t.Fatal("prompt service did not activate")
	}
	if got.SystemPrompt() != want.SystemPrompt() {
		t.Fatalf("system prompt differs:\n got: %q\nwant: %q", got.SystemPrompt(), want.SystemPrompt())
	}
	gotTools := toolNames(got)
	wantTools := toolNames(want)
	if strings.Join(gotTools, ",") != strings.Join(wantTools, ",") {
		t.Fatalf("tool set differs: got %v want %v", gotTools, wantTools)
	}
	// The tools service is a view over the same host: same prompt, same tools.
	assembly := rt.Assembly()
	if assembly == nil {
		t.Fatal("tools service did not activate")
	}
	if assembly.SystemPrompt() != got.SystemPrompt() {
		t.Fatal("tools service prompt must be the host prompt")
	}
	if strings.Join(registryNames(assembly.Tools()), ",") != strings.Join(wantTools, ",") {
		t.Fatal("tools service registry must be the host registry")
	}
	if rt.Projection() == nil {
		t.Fatal("session-projection did not activate")
	}
}

func toolNames(h *host.Host) []string { return registryNames(h.Tools()) }

func registryNames(reg *agenttool.ToolRegistry) []string {
	if reg == nil {
		return nil
	}
	tools := reg.List()
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name())
	}
	return out
}

// TestBootDefaultLoadoutServices pins the default service set and its
// diagnostics: the same components the pre-graph cmd/runtime assembled, with
// no sandbox factory and no second registry.
func TestBootDefaultLoadoutServices(t *testing.T) {
	rt := bootForTest(t, host.TenantConfig{}, loadout.Options{Revision: 4})
	snap := rt.Snapshot()
	if snap.LoadoutID != loadout.DefaultID || snap.Revision != 4 || snap.Generation != 1 {
		t.Fatalf("snapshot header: %+v", snap)
	}
	for _, name := range []string{
		loadout.ServiceStore, loadout.ServiceLLM, loadout.ServicePrompt,
		loadout.ServiceSessionProjection, loadout.ServiceTools, loadout.ServicePiLoop,
	} {
		if !snap.Active(name) {
			t.Fatalf("%s should be ACTIVE:\n%s", name, snap)
		}
	}
	for _, absent := range []string{loadout.ServiceMCP, loadout.ServiceCompaction, loadout.ServiceTelemetry, loadout.ServiceWorker, loadout.ServiceSandbox} {
		if _, ok := snap.Service(absent); ok {
			t.Fatalf("%s must be absent from the default boot", absent)
		}
	}
	if rt.ProviderName() != "fake" || rt.Model() != "fake" {
		t.Fatalf("default provider: %s/%s", rt.ProviderName(), rt.Model())
	}
	if rt.MCPEntries() != nil {
		t.Fatal("no MCP config: entries must be empty")
	}
}

func TestBootWorkerStartsOnlyWhenEnabled(t *testing.T) {
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	rt, err := Boot(context.Background(), Config{
		Seams:     Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:    host.TenantConfig{},
		Options:   loadout.Options{Worker: true, RunConcurrency: 2},
		RuntimeID: "test-runtime",
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer rt.Close(context.Background())
	if !rt.Snapshot().Active(loadout.ServiceWorker) {
		t.Fatalf("worker should be ACTIVE:\n%s", rt.Snapshot())
	}
	rt.StopWorker()
}

// TestReloadSharesUnchangedServicesAndPinsRevision covers the next-run rule:
// a reload produces a new generation and revision, unchanged services are
// shared (not rebuilt), and in-flight leases keep their pinned revision.
func TestReloadSharesUnchangedServicesAndPinsRevision(t *testing.T) {
	rt := bootForTest(t, host.TenantConfig{}, loadout.Options{Revision: 1})
	first, err := rt.Binding().Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if first.Loadout().Revision != 1 || first.Loadout().Generation != 1 {
		t.Fatalf("first pin: %+v", first.Loadout())
	}
	loopBefore, ok := rt.engine.Lookup(loadout.ServicePiLoop)
	if !ok {
		t.Fatal("pi-loop should be live")
	}
	promptBefore, ok := rt.engine.Lookup(loadout.ServicePrompt)
	if !ok {
		t.Fatal("prompt should be live")
	}

	next := host.TenantConfig{Telemetry: host.TelemetryConfig{Adapter: "memory"}}
	snap, err := rt.Reload(context.Background(), next, loadout.Options{Revision: 2})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if snap.Revision != 2 || snap.Generation != 2 {
		t.Fatalf("reload snapshot: %+v", snap)
	}
	if len(snap.Retiring) != 0 {
		t.Fatalf("unchanged services must be shared, not retired: %+v", snap.Retiring)
	}
	if loopAfter, _ := rt.engine.Lookup(loadout.ServicePiLoop); loopAfter != loopBefore {
		t.Fatal("pi-loop instance must be shared across generations")
	}
	if promptAfter, _ := rt.engine.Lookup(loadout.ServicePrompt); promptAfter != promptBefore {
		t.Fatal("prompt instance must be shared across generations")
	}
	if !snap.Active(loadout.ServiceTelemetry) {
		t.Fatalf("telemetry should be active after reload:\n%s", snap)
	}

	// The in-flight pin keeps revision 1; a new run pins revision 2.
	if first.Loadout().Revision != 1 {
		t.Fatalf("in-flight pin changed: %+v", first.Loadout())
	}
	second, err := rt.Binding().Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire new: %v", err)
	}
	if second.Loadout().Revision != 2 || second.Loadout().Generation != 2 {
		t.Fatalf("new pin: %+v", second.Loadout())
	}
	if second.Runner() != first.Runner() {
		t.Fatal("an unchanged pi-loop instance must be shared across generations")
	}
	if err := first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReloadRejectsInvalidDocumentAndKeepsGeneration(t *testing.T) {
	rt := bootForTest(t, host.TenantConfig{}, loadout.Options{Revision: 1})
	before := rt.Snapshot()
	bad := host.TenantConfig{Quota: host.QuotaConfig{RunConcurrency: -1}}
	if _, err := rt.Reload(context.Background(), bad, loadout.Options{Revision: 2}); err == nil {
		t.Fatal("invalid document must be rejected")
	}
	after := rt.Snapshot()
	if after.Generation != before.Generation || after.Revision != before.Revision {
		t.Fatalf("rejected reload changed the generation: before %+v after %+v", before, after)
	}
}

// --- collector ------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type countedInstance struct {
	stopped atomic.Int32
}

func (c *countedInstance) Start(context.Context, *servicegraph.EffectScope) error { return nil }
func (c *countedInstance) Stop(context.Context) error {
	c.stopped.Add(1)
	return nil
}

func factoryFor(inst *countedInstance) servicegraph.Factory {
	return func(context.Context, servicegraph.Resolved) (servicegraph.Instance, error) { return inst, nil }
}

func loadoutWith(t *testing.T, version string, inst *countedInstance) *servicegraph.Loadout {
	t.Helper()
	lo, err := servicegraph.NewLoadout(servicegraph.LoadoutConfig{
		ID: "default", Revision: 1,
		Services: []servicegraph.ServiceDescriptor{{
			Name: "svc", Version: version, Provides: []string{"ctx.svc"},
			Effects: []string{"listener"}, Factory: factoryFor(inst),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return lo
}

// TestHostCollectorFinishesRetiringGenerationsWhileIdle is the "idle process
// still finishes RETIRING" gate: the engine alone only collects on Apply or
// Acquire, the host ticker must do it on its own.
func TestHostCollectorFinishesRetiringGenerationsWhileIdle(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	engine := servicegraph.NewEngine(
		servicegraph.WithLogger(discardLogger()),
		servicegraph.WithRetireTimeout(time.Second),
		servicegraph.WithClock(clock.Now),
	)
	old := &countedInstance{}
	cur := &countedInstance{}
	ctx := context.Background()
	if _, err := engine.Apply(ctx, loadoutWith(t, "v1", old)); err != nil {
		t.Fatal(err)
	}
	lease, err := engine.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, loadoutWith(t, "v2", cur)); err != nil {
		t.Fatal(err)
	}
	if old.stopped.Load() != 0 {
		t.Fatal("the retired instance must survive its in-flight reference")
	}

	rt := &Runtime{
		engine: engine,
		cfg:    Config{CollectInterval: 5 * time.Millisecond, Seams: Seams{Logger: discardLogger()}},
	}
	rt.startCollector(ctx)
	defer func() {
		rt.collectCancel()
		<-rt.collectDone
	}()

	// No Apply/Acquire happens from here on; only the host ticker runs.
	clock.Advance(2 * time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(engine.Snapshot().Retiring) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(engine.Snapshot().Retiring) != 0 {
		t.Fatalf("collector did not finish the retiring generation: %+v", engine.Snapshot().Retiring)
	}
	if old.stopped.Load() != 1 {
		t.Fatalf("old instance stopped %d times, want 1", old.stopped.Load())
	}
	if !lease.Broken() {
		t.Fatal("the outstanding lease must be invalidated after the drain deadline")
	}
	if err := rt.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// --- audit sink -----------------------------------------------------------

func TestAuditSinkRateLimitsAndHidesToken(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	sink := NewAuditSink(log, AuditOptions{Interval: time.Second, Now: clock.Now})
	tenant := uuid.New()
	session := uuid.New()
	violation := struct {
		reason, op, owner string
		epoch             int64
	}{
		reason: "stale-epoch", op: "commit", owner: "runtime-7", epoch: 42,
	}
	emit := func(reason string) {
		sink(kernel.Violation{
			Reason: kernel.Reason(reason),
			Op:     violation.op,
			Scope:  kernel.Scope{TenantID: tenant, SessionID: session},
			Kind:   kernel.KindAssistantMessage,
			Token:  kernel.OwnerToken{Owner: violation.owner, Epoch: violation.epoch},
			Detail: "test rejection",
		})
	}
	emit(violation.reason)
	emit(violation.reason) // suppressed
	emit(violation.reason) // suppressed
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want one rate-limited record, got %d: %s", len(lines), buf.String())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatal(err)
	}
	if record["reason"] != "stale-epoch" || record["op"] != "commit" || record["tenant"] != tenant.String() {
		t.Fatalf("audit fields: %v", record)
	}
	if record["owner"] != "runtime-7" || record["epoch"] != float64(42) {
		t.Fatalf("owner/epoch must be separate fields: %v", record)
	}
	if _, ok := record["token"]; ok {
		t.Fatalf("the full fencing token must not be logged: %v", record)
	}
	if !strings.Contains(lines[0], "kernel audit") {
		t.Fatalf("record message: %s", lines[0])
	}
	buf.Reset()
	clock.Advance(2 * time.Second)
	emit(violation.reason)
	var next map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &next); err != nil {
		t.Fatal(err)
	}
	if next["suppressed"] != float64(2) {
		t.Fatalf("suppressed count must be reported after the window: %v", next)
	}
}

func TestAuditSinkSeparatesReasons(t *testing.T) {
	var buf bytes.Buffer
	sink := NewAuditSink(slog.New(slog.NewJSONHandler(&buf, nil)), AuditOptions{Interval: time.Hour, Now: time.Now})
	scope := kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}
	sink(kernel.Violation{Reason: kernel.ReasonStaleEpoch, Op: kernel.OpCommit, Scope: scope})
	sink(kernel.Violation{Reason: kernel.ReasonQuota, Op: kernel.OpCommit, Scope: scope})
	if lines := strings.Count(buf.String(), "\n"); lines != 2 {
		t.Fatalf("distinct reasons must each log once, got %d lines: %s", lines, buf.String())
	}
}

// TestKernelStaleCommitIsAudited wires the sink through the kernel: a stale
// owner commit produces exactly one audit record.
func TestKernelStaleCommitIsAudited(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{Audit: NewAuditSink(log, AuditOptions{Interval: -1})})
	ctx := context.Background()
	tenant, session := uuid.New(), uuid.New()
	mem.CreateSession(tenant, session)
	mem.EnqueueRun(tenant, session)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	lease, err := k.Claim(ctx, "owner-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	stale := lease
	// A second claim after the first is released issues a new epoch; the old
	// token then commits nothing and is audited.
	if err := k.Release(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := k.ClaimScope(ctx, scope, "owner-b", time.Minute); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"text": "hi"})
	_, err = k.Commit(ctx, kernel.CommitRequest{
		Kind: kernel.KindAssistantMessage, Scope: scope, Payload: body,
		GenerationID: "g-1", Token: stale.Token,
	})
	if !errors.Is(err, kernel.ErrStaleEpoch) {
		t.Fatalf("want ErrStaleEpoch, got %v", err)
	}
	if !strings.Contains(buf.String(), "stale-epoch") || !strings.Contains(buf.String(), "kernel audit") {
		t.Fatalf("stale commit must be audited: %s", buf.String())
	}
}

// TestExternalServicesAssembleFromDocument covers the W5 wiring: the tenant
// document's external section selects the loadout services, a broken provider
// degrades without failing boot, and the runtime exposes the services to the
// HTTP edge.
func TestExternalServicesAssembleFromDocument(t *testing.T) {
	tenant := host.TenantConfig{External: host.ExternalConfig{
		Backends: map[string]host.ExternalProviderConfig{
			"dsh": {Command: "/nonexistent/keel-sidecar-for-tests"},
		},
		Tools: map[string]host.ExternalProviderConfig{
			"tools": {Command: "/nonexistent/keel-tool-provider-for-tests"},
		},
		Quota: host.ExternalQuotaConfig{Unmetered: host.ExternalUnmeteredAllow},
	}}
	rt := bootForTest(t, tenant, loadout.Options{})
	snap := rt.Snapshot()
	for _, name := range []string{loadout.ServiceExternalAgent, loadout.ServiceExternalTools} {
		if !snap.Active(name) {
			t.Fatalf("%s should be ACTIVE (degraded), snapshot:\n%s", name, snap)
		}
	}
	agents := rt.ExternalAgents()
	if agents == nil {
		t.Fatal("runtime did not expose the external agent service")
	}
	backends := agents.Backends()
	if len(backends) != 1 || backends[0].Name != "dsh" || backends[0].State != "degraded" {
		t.Fatalf("backend status = %+v", backends)
	}
	tools := rt.ExternalTools()
	if tools == nil {
		t.Fatal("runtime did not expose the external tool service")
	}
	if got := tools.Tools(); len(got) != 0 {
		t.Fatalf("a failed tool provider published tools: %v", got)
	}
}
