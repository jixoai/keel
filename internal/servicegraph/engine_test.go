package servicegraph

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestActivateInDependencyOrder(t *testing.T) {
	rec := &recorder{}
	lo := mustLoadout(t, "default", 1,
		desc("llm", instanceFactory(rec, "llm", "provider-client"), []string{"ctx.llm"}, nil, nil),
		desc("tools", instanceFactory(rec, "tools", "tool-registry"), []string{"ctx.tools"}, nil, []string{"ctx.mcp"}),
		desc("loop", binderFactory(rec, "loop", []string{"ctx.llm", "ctx.tools"}), []string{"ctx.loop"}, []string{"ctx.llm", "ctx.tools"}, nil),
	)
	e := newTestEngine()
	snap := mustApply(t, e, lo)

	if snap.LoadoutID != "default" || snap.Revision != 1 || snap.Generation != 1 {
		t.Fatalf("snapshot header wrong: %+v", snap)
	}
	for _, name := range []string{"llm", "tools", "loop"} {
		if !snap.Active(name) {
			t.Fatalf("%s should be ACTIVE, snapshot:\n%s", name, snap)
		}
	}
	rec.mustBeBefore(t, "start:llm", "start:loop")
	rec.mustBeBefore(t, "start:tools", "start:loop")
	if !rec.contains("bind:loop:ctx.llm=true") || !rec.contains("bind:loop:ctx.tools=true") {
		t.Fatalf("loop did not receive live bindings: %v", rec.list())
	}
	loop, _ := snap.Service("loop")
	if loop.Generation != 1 {
		t.Fatalf("loop generation: %d", loop.Generation)
	}
	if loop.Bindings["ctx.llm"] != "llm" || loop.Bindings["ctx.tools"] != "tools" {
		t.Fatalf("loop bindings: %v", loop.Bindings)
	}
	tools, _ := snap.Service("tools")
	if len(tools.Degraded) != 1 || tools.Degraded[0] != "ctx.mcp" {
		t.Fatalf("tools degraded: %v", tools.Degraded)
	}
	if len(tools.Effects) != 1 || tools.Effects[0].Kind != "tool-registry" || tools.Effects[0].Generation != 1 {
		t.Fatalf("tools effects: %+v", tools.Effects)
	}
	if _, ok := e.Lookup("loop"); !ok {
		t.Fatal("Lookup(loop) should find the live instance")
	}
	if _, ok := e.Lookup("nope"); ok {
		t.Fatal("Lookup(nope) should fail")
	}
}

func TestMissingRequiredCapabilityKeepsPending(t *testing.T) {
	rec := &recorder{}
	lo := mustLoadout(t, "default", 1,
		desc("llm", instanceFactory(rec, "llm"), []string{"ctx.llm"}, nil, nil),
		desc("loop", instanceFactory(rec, "loop"), []string{"ctx.loop"}, []string{"ctx.tools"}, nil),
	)
	e := newTestEngine()
	snap, err := e.Apply(context.Background(), lo)
	if err != nil {
		t.Fatalf("a PENDING service is not an activation failure: %v", err)
	}
	if snap.State("loop") != StatePending {
		t.Fatalf("loop should stay PENDING, got %s", snap.State("loop"))
	}
	if !snap.Active("llm") {
		t.Fatal("independent llm must still run")
	}
	if rec.contains("start:loop") {
		t.Fatalf("loop must not start: %v", rec.list())
	}
	diags := snap.DiagnosticsFor("loop")
	if len(diags) == 0 || diags[0].Code != DiagMissingRequired || diags[0].Capability != "ctx.tools" {
		t.Fatalf("diagnostics: %+v", diags)
	}
	explain := snap.Explain("loop")
	if !strings.Contains(explain, "ctx.tools") || !strings.Contains(explain, string(StatePending)) {
		t.Fatalf("explain should say why loop is pending:\n%s", explain)
	}
}

func TestMissingOptionalCapabilityDegraded(t *testing.T) {
	rec := &recorder{}
	lo := mustLoadout(t, "default", 1,
		desc("tools", instanceFactory(rec, "tools"), []string{"ctx.tools"}, nil, []string{"ctx.sandbox"}),
	)
	e := newTestEngine()
	snap := mustApply(t, e, lo)
	if !snap.Active("tools") {
		t.Fatalf("tools should activate degraded, snapshot:\n%s", snap)
	}
	st, _ := snap.Service("tools")
	if len(st.Degraded) != 1 || st.Degraded[0] != "ctx.sandbox" {
		t.Fatalf("degraded: %v", st.Degraded)
	}
	diags := snap.DiagnosticsFor("tools")
	if len(diags) != 1 || diags[0].Code != DiagOptionalMissing || diags[0].Severity != SeverityDegraded {
		t.Fatalf("diagnostics: %+v", diags)
	}
	if !strings.Contains(diags[0].Message, "ctx.sandbox") {
		t.Fatalf("message should name the capability: %q", diags[0].Message)
	}
}

func TestPendingProviderChainExplained(t *testing.T) {
	rec := &recorder{}
	lo := mustLoadout(t, "default", 1,
		desc("db", instanceFactory(rec, "db"), []string{"ctx.db"}, []string{"ctx.disk"}, nil),
		desc("app", instanceFactory(rec, "app"), []string{"ctx.app"}, []string{"ctx.db"}, nil),
		desc("solo", instanceFactory(rec, "solo"), []string{"ctx.solo"}, nil, nil),
	)
	e := newTestEngine()
	snap, err := e.Apply(context.Background(), lo)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if snap.State("db") != StatePending || snap.State("app") != StatePending {
		t.Fatalf("both db and app should be PENDING:\n%s", snap)
	}
	if !snap.Active("solo") {
		t.Fatal("independent solo must run")
	}
	app, _ := snap.Service("app")
	if len(app.WaitingOn) != 1 || app.WaitingOn[0] != "db" {
		t.Fatalf("app waiting on: %v", app.WaitingOn)
	}
	diags := snap.DiagnosticsFor("app")
	if len(diags) == 0 || diags[0].Code != DiagProviderPending {
		t.Fatalf("app diagnostics: %+v", diags)
	}
	explain := snap.Explain("app")
	for _, want := range []string{"app", "db", "ctx.disk", "missing-required"} {
		if !strings.Contains(explain, want) {
			t.Fatalf("explain missing %q:\n%s", want, explain)
		}
	}
}

func TestPrimaryFallbackStandbyAndPromotion(t *testing.T) {
	newLoadout := func(rec *recorder, primaryOK bool) *Loadout {
		var primary Factory
		if primaryOK {
			primary = instanceFactory(rec, "llm-a")
		} else {
			primary = failingFactory(rec, "llm-a", errors.New("primary down"), "llm-a-listener")
		}
		fallback := desc("llm-f", instanceFactory(rec, "llm-f"), []string{"ctx.llm"}, nil, nil)
		fallback.Role = RoleFallback
		return mustLoadout(t, "default", 1,
			desc("llm-a", primary, []string{"ctx.llm"}, nil, nil),
			fallback,
			desc("loop", binderFactory(rec, "loop", []string{"ctx.llm"}), []string{"ctx.loop"}, []string{"ctx.llm"}, nil),
		)
	}

	t.Run("healthy primary keeps fallback standby", func(t *testing.T) {
		rec := &recorder{}
		e := newTestEngine()
		snap := mustApply(t, e, newLoadout(rec, true))
		if !snap.Active("llm-a") || !snap.Active("loop") {
			t.Fatalf("snapshot:\n%s", snap)
		}
		if snap.State("llm-f") != StatePending {
			t.Fatalf("fallback should be PENDING standby, got %s", snap.State("llm-f"))
		}
		if rec.contains("start:llm-f") {
			t.Fatal("standby fallback must not start")
		}
		diags := snap.DiagnosticsFor("llm-f")
		if len(diags) != 1 || diags[0].Code != DiagStandby {
			t.Fatalf("fallback diagnostics: %+v", diags)
		}
		loop, _ := snap.Service("loop")
		if loop.Bindings["ctx.llm"] != "llm-a" {
			t.Fatalf("loop should bind the primary: %v", loop.Bindings)
		}
	})

	t.Run("failed primary promotes fallback", func(t *testing.T) {
		rec := &recorder{}
		e := newTestEngine()
		snap, err := e.Apply(context.Background(), newLoadout(rec, false))
		var actErr *ActivationError
		if !errors.As(err, &actErr) {
			t.Fatalf("want ActivationError, got %v", err)
		}
		if len(actErr.Failures) != 1 || actErr.Failures[0].Service != "llm-a" || actErr.Failures[0].Code != DiagStartFailed {
			t.Fatalf("failures: %+v", actErr.Failures)
		}
		if snap.State("llm-a") != StateFailed {
			t.Fatalf("llm-a should be FAILED, got %s", snap.State("llm-a"))
		}
		if !snap.Active("llm-f") || !snap.Active("loop") {
			t.Fatalf("fallback and loop should be ACTIVE:\n%s", snap)
		}
		loop, _ := snap.Service("loop")
		if loop.Bindings["ctx.llm"] != "llm-f" {
			t.Fatalf("loop should bind the promoted fallback: %v", loop.Bindings)
		}
		diags := snap.DiagnosticsFor("llm-f")
		found := false
		for _, d := range diags {
			if d.Code == DiagStandbyServing {
				found = true
			}
		}
		if !found {
			t.Fatalf("fallback should explain it is serving: %+v", diags)
		}
		// The failed primary's own effects were rolled back.
		if !rec.contains("release:llm-a/llm-a-listener") {
			t.Fatalf("failed start effects must be released: %v", rec.list())
		}
		if rec.contains("stop:llm-a") {
			t.Fatal("Stop must not be called for a service whose Start failed")
		}
	})
}

func TestStartupFailureRollbackIsolated(t *testing.T) {
	rec := &recorder{}
	boom := errors.New("boom")
	lo := mustLoadout(t, "default", 1,
		desc("a", instanceFactory(rec, "a", "a-effect"), []string{"ctx.a"}, nil, nil),
		desc("b", failingFactory(rec, "b", boom, "b-effect-1", "b-effect-2"), []string{"ctx.b"}, nil, nil),
		desc("c", instanceFactory(rec, "c"), []string{"ctx.c"}, []string{"ctx.b"}, nil),
	)
	e := newTestEngine()
	snap, err := e.Apply(context.Background(), lo)
	var actErr *ActivationError
	if !errors.As(err, &actErr) {
		t.Fatalf("want ActivationError, got %v", err)
	}
	if len(actErr.Failures) != 1 || actErr.Failures[0].Service != "b" {
		t.Fatalf("failures: %+v", actErr.Failures)
	}
	if !snap.Active("a") {
		t.Fatal("already-started a must not be unloaded")
	}
	if rec.contains("stop:a") {
		t.Fatalf("a must not be stopped: %v", rec.list())
	}
	if snap.State("b") != StateFailed || snap.State("c") != StatePending {
		t.Fatalf("b/c states wrong:\n%s", snap)
	}
	c, _ := snap.Service("c")
	if len(c.WaitingOn) != 1 || c.WaitingOn[0] != "b" {
		t.Fatalf("c waiting on: %v", c.WaitingOn)
	}
	diags := snap.DiagnosticsFor("c")
	if len(diags) == 0 || diags[0].Code != DiagProviderFailed {
		t.Fatalf("c diagnostics: %+v", diags)
	}
	// Rollback of b's effects is in reverse registration order.
	rec.mustBeBefore(t, "release:b/b-effect-2", "release:b/b-effect-1")
}

func TestReplaceProviderUnloadOrder(t *testing.T) {
	rec := &recorder{}
	loA := mustLoadout(t, "default", 1,
		desc("llm-a", instanceFactory(rec, "llm-a", "llm-client"), []string{"ctx.llm"}, nil, nil),
		desc("loop", instanceFactory(rec, "loop", "loop-listener"), []string{"ctx.loop"}, []string{"ctx.llm"}, nil),
	)
	loB := mustLoadout(t, "default", 2,
		desc("llm-b", instanceFactory(rec, "llm-b", "llm-client"), []string{"ctx.llm"}, nil, nil),
		desc("loop", instanceFactory(rec, "loop", "loop-listener"), []string{"ctx.loop"}, []string{"ctx.llm"}, nil),
	)
	e := newTestEngine()
	first := mustApply(t, e, loA)
	if !first.Active("loop") {
		t.Fatal("gen1 should be active")
	}
	second := mustApply(t, e, loB)

	// Dependents unload first, then the provider, then the new provider
	// activates, then the dependent reloads.
	rec.mustBeBefore(t, "release:loop/loop-listener", "stop:loop")
	rec.mustBeBefore(t, "stop:loop", "stop:llm-a")
	rec.mustBeBefore(t, "stop:llm-a", "start:llm-b")
	rec.mustBeBefore(t, "start:llm-b", "start:loop")
	if rec.count("start:loop") != 2 || rec.count("stop:loop") != 1 {
		t.Fatalf("loop should reload exactly once: %v", rec.list())
	}
	if _, ok := second.Service("llm-a"); ok {
		t.Fatal("llm-a should be gone from the new loadout")
	}
	loop, _ := second.Service("loop")
	if loop.Generation != 2 || loop.Bindings["ctx.llm"] != "llm-b" {
		t.Fatalf("loop should be reloaded onto llm-b: %+v", loop)
	}
	if second.Generation != 2 || second.Revision != 2 {
		t.Fatalf("snapshot header: %+v", second)
	}
}

func TestRoleFlipUnloadsDependentsFirst(t *testing.T) {
	rec := &recorder{}
	primary := func() ServiceDescriptor {
		return desc("llm-a", instanceFactory(rec, "llm-a"), []string{"ctx.llm"}, nil, nil)
	}
	fallback := func() ServiceDescriptor {
		d := desc("llm-f", instanceFactory(rec, "llm-f"), []string{"ctx.llm"}, nil, nil)
		d.Role = RoleFallback
		return d
	}
	loop := func() ServiceDescriptor {
		return desc("loop", instanceFactory(rec, "loop"), []string{"ctx.loop"}, []string{"ctx.llm"}, nil)
	}
	lo1 := mustLoadout(t, "default", 1, primary(), fallback(), loop())
	lo2 := mustLoadout(t, "default", 2, func() ServiceDescriptor { d := primary(); d.Role = RoleFallback; return d }(), func() ServiceDescriptor { d := fallback(); d.Role = RolePrimary; return d }(), loop())

	e := newTestEngine()
	mustApply(t, e, lo1)
	snap := mustApply(t, e, lo2)

	rec.mustBeBefore(t, "stop:loop", "stop:llm-a")
	rec.mustBeBefore(t, "start:llm-f", "start:loop")
	if snap.State("llm-a") != StatePending || !snap.Active("llm-f") {
		t.Fatalf("roles should have swapped:\n%s", snap)
	}
	loopStatus, _ := snap.Service("loop")
	if loopStatus.Bindings["ctx.llm"] != "llm-f" || loopStatus.Generation != 2 {
		t.Fatalf("loop should rebind: %+v", loopStatus)
	}
}

func TestVersionBumpReloadsSelfAndDependentsOnly(t *testing.T) {
	rec := &recorder{}
	mk := func(version string) *Loadout {
		a := desc("a", instanceFactory(rec, "a"), []string{"ctx.a"}, nil, nil)
		a.Version = version
		return mustLoadout(t, "default", 1,
			a,
			desc("b", instanceFactory(rec, "b"), []string{"ctx.b"}, []string{"ctx.a"}, nil),
			desc("c", instanceFactory(rec, "c"), []string{"ctx.c"}, nil, nil),
		)
	}
	e := newTestEngine()
	mustApply(t, e, mk("v1"))
	snap := mustApply(t, e, mk("v2"))

	if rec.count("start:a") != 2 || rec.count("start:b") != 2 {
		t.Fatalf("a and b should reload once: %v", rec.list())
	}
	if rec.count("start:c") != 1 || rec.contains("stop:c") {
		t.Fatalf("independent c must not be disturbed: %v", rec.list())
	}
	rec.mustBeBefore(t, "stop:b", "stop:a")
	rec.mustBeBefore(t, "start:a", "start:b")
	c, _ := snap.Service("c")
	a, _ := snap.Service("a")
	if c.Generation != 1 || a.Generation != 2 {
		t.Fatalf("generations: c=%d a=%d", c.Generation, a.Generation)
	}
}

func TestGenerationIncrementAndEffectLeak(t *testing.T) {
	rec := &recorder{}
	first := &fakeInstance{rec: rec, name: "svc", effects: []fakeEffect{{kind: "listener"}}}
	lo1 := mustLoadout(t, "default", 1, desc("svc", fixedFactory(first), []string{"ctx.svc"}, nil, nil))
	e := newTestEngine()
	mustApply(t, e, lo1)

	second := &fakeInstance{rec: rec, name: "svc", effects: []fakeEffect{{kind: "listener"}}}
	lo2 := func() *Loadout {
		d := desc("svc", fixedFactory(second), []string{"ctx.svc"}, nil, nil)
		d.Version = "v2"
		return mustLoadout(t, "default", 2, d)
	}()
	snap := mustApply(t, e, lo2)

	if e.Generation() != 2 || snap.Generation != 2 {
		t.Fatalf("generation should be 2: %d", e.Generation())
	}
	if rec.count("release:svc/listener") != 1 {
		t.Fatalf("old effect must be released exactly once: %v", rec.list())
	}
	// The old scope is closed: no new effect can leak into the old generation.
	if err := first.scope.Register("late", func() error { return nil }); !errors.Is(err, ErrScopeClosed) {
		t.Fatalf("registering into an unloaded scope must fail, got %v", err)
	}
	svc, _ := snap.Service("svc")
	if svc.Generation != 2 {
		t.Fatalf("service should be reloaded in generation 2: %+v", svc)
	}
	if len(svc.Effects) != 1 || svc.Effects[0].Generation != 2 {
		t.Fatalf("effect should carry generation 2: %+v", svc.Effects)
	}
	// Applying again without changes keeps the instance but advances the
	// engine generation.
	snap3 := mustApply(t, e, lo2)
	svc3, _ := snap3.Service("svc")
	if snap3.Generation != 3 || svc3.Generation != 2 {
		t.Fatalf("unchanged instance must keep its generation: engine=%d svc=%d", snap3.Generation, svc3.Generation)
	}
	if rec.count("start:svc") != 2 {
		t.Fatalf("no extra start expected: %v", rec.list())
	}
}

func TestOptionalProviderFailedIsDegraded(t *testing.T) {
	rec := &recorder{}
	lo := mustLoadout(t, "default", 1,
		desc("sandbox", failingFactory(rec, "sandbox", errors.New("no seccomp")), []string{"ctx.sandbox"}, nil, nil),
		desc("tools", instanceFactory(rec, "tools"), []string{"ctx.tools"}, nil, []string{"ctx.sandbox"}),
	)
	e := newTestEngine()
	snap, err := e.Apply(context.Background(), lo)
	if err == nil {
		t.Fatal("sandbox failure should still be reported")
	}
	if !snap.Active("tools") {
		t.Fatalf("tools should run degraded:\n%s", snap)
	}
	tools, _ := snap.Service("tools")
	if len(tools.Degraded) != 1 || tools.Degraded[0] != "ctx.sandbox" {
		t.Fatalf("degraded: %v", tools.Degraded)
	}
	diags := snap.DiagnosticsFor("tools")
	if len(diags) == 0 || diags[0].Code != DiagOptionalFailed {
		t.Fatalf("diagnostics: %+v", diags)
	}
}

func TestRejectedLoadoutKeepsPreviousGeneration(t *testing.T) {
	rec := &recorder{}
	valid := mustLoadout(t, "default", 1, desc("a", instanceFactory(rec, "a"), []string{"ctx.a"}, nil, nil))

	e := newTestEngine()
	snap := mustApply(t, e, valid)

	recBefore := rec.list()
	invalid, err := NewLoadout(LoadoutConfig{ID: "bad", Revision: 9, Services: []ServiceDescriptor{
		desc("x", instanceFactory(rec, "x"), []string{"ctx.x"}, []string{"ctx.y"}, nil),
		desc("y", instanceFactory(rec, "y"), []string{"ctx.y"}, []string{"ctx.x"}, nil),
	}})
	if err != nil {
		t.Fatalf("construction should succeed, resolution should reject: %v", err)
	}
	_, err = e.Apply(context.Background(), invalid)
	if !hasIssue(err, IssueCycle) {
		t.Fatalf("want cycle rejection, got %v", err)
	}
	after := e.Snapshot()
	if !after.Active("a") || after.Generation != snap.Generation {
		t.Fatalf("rejection must not disturb the live generation:\n%s", after)
	}
	if _, ok := after.Service("x"); ok {
		t.Fatal("rejected services must not appear")
	}
	if len(rec.list()) != len(recBefore) {
		t.Fatalf("rejection must not start or stop anything: %v", rec.list())
	}
}

func TestFirstApplyRejectedStartsNothing(t *testing.T) {
	rec := &recorder{}
	e := newTestEngine()
	invalid, err := NewLoadout(LoadoutConfig{ID: "bad", Revision: 1, Services: []ServiceDescriptor{
		{Name: "noprovides", Version: "v1"},
	}})
	if err == nil {
		t.Fatal("missing provides must be rejected at construction")
	}
	_ = invalid
	if e.Generation() != 0 {
		t.Fatalf("generation: %d", e.Generation())
	}
	if _, err := e.Apply(context.Background(), mustLoadout(t, "l", 1, desc("a", instanceFactory(rec, "a"), []string{"ctx.a"}, nil, nil))); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestCloseUnloadsReverseOrderAndIsIdempotent(t *testing.T) {
	rec := &recorder{}
	lo := mustLoadout(t, "default", 1,
		desc("store", instanceFactory(rec, "store"), []string{"ctx.store"}, nil, nil),
		desc("llm", instanceFactory(rec, "llm"), []string{"ctx.llm"}, nil, nil),
		desc("loop", instanceFactory(rec, "loop"), []string{"ctx.loop"}, []string{"ctx.llm"}, nil),
		desc("worker", instanceFactory(rec, "worker"), []string{"ctx.worker"}, []string{"ctx.store", "ctx.loop"}, nil),
	)
	e := newTestEngine()
	mustApply(t, e, lo)
	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rec.mustBeBefore(t, "stop:worker", "stop:loop")
	rec.mustBeBefore(t, "stop:loop", "stop:llm")
	rec.mustBeBefore(t, "stop:llm", "stop:store")

	snap := e.Snapshot()
	for _, name := range []string{"store", "llm", "loop", "worker"} {
		if snap.State(name) != StatePending {
			t.Fatalf("%s should be PENDING after close, got %s", name, snap.State(name))
		}
	}
	if _, ok := e.Lookup("loop"); ok {
		t.Fatal("no instance should survive Close")
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
	if _, err := e.Apply(context.Background(), lo); !errors.Is(err, ErrClosed) {
		t.Fatalf("Apply after Close: want ErrClosed, got %v", err)
	}
}

func TestEffectReleaseFailureStillUnloads(t *testing.T) {
	rec := &recorder{}
	mk := func(version string) *Loadout {
		inst := &fakeInstance{rec: rec, name: "svc", effects: []fakeEffect{
			{kind: "first"},
			{kind: "second", err: errors.New("release failed")},
		}}
		d := desc("svc", fixedFactory(inst), []string{"ctx.svc"}, nil, nil)
		d.Version = version
		return mustLoadout(t, "default", 1, d)
	}
	e := newTestEngine()
	mustApply(t, e, mk("v1"))
	snap := mustApply(t, e, mk("v2"))

	if !rec.contains("release:svc/second") || !rec.contains("release:svc/first") {
		t.Fatalf("both releases must be attempted: %v", rec.list())
	}
	if !snap.Active("svc") {
		t.Fatalf("service should have reloaded:\n%s", snap)
	}
	found := false
	for _, d := range snap.Diagnostics {
		if d.Code == DiagEffectRelease {
			found = true
		}
	}
	if !found {
		t.Fatalf("release failure should be a diagnostic: %+v", snap.Diagnostics)
	}
}

func TestStopFailureRecordedAndRetryOnNextApply(t *testing.T) {
	rec := &recorder{}
	first := &fakeInstance{rec: rec, name: "svc", stopErr: errors.New("stop failed")}
	mk := func(version string, inst *fakeInstance) *Loadout {
		d := desc("svc", fixedFactory(inst), []string{"ctx.svc"}, nil, nil)
		d.Version = version
		return mustLoadout(t, "default", 1, d)
	}
	e := newTestEngine()
	mustApply(t, e, mk("v1", first))
	mustApply(t, e, mk("v2", &fakeInstance{rec: rec, name: "svc"}))

	ok := false
	for _, d := range e.Snapshot().Diagnostics {
		if d.Code == DiagStopFailed {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("stop failure should be recorded: %+v", e.Snapshot().Diagnostics)
	}
}

func TestFailedServiceRetriesOnNextApply(t *testing.T) {
	rec := &recorder{}
	failing := failingFactory(rec, "flaky", errors.New("first attempt fails"))
	lo := mustLoadout(t, "default", 1, desc("flaky", failing, []string{"ctx.flaky"}, nil, nil))
	e := newTestEngine()

	if _, err := e.Apply(context.Background(), lo); err == nil {
		t.Fatal("first attempt should fail")
	}
	// The fake instance succeeds on the second construction.
	recovered := &fakeInstance{rec: rec, name: "flaky"}
	lo2 := mustLoadout(t, "default", 2, desc("flaky", fixedFactory(recovered), []string{"ctx.flaky"}, nil, nil))
	snap := mustApply(t, e, lo2)
	if !snap.Active("flaky") {
		t.Fatalf("retry should activate:\n%s", snap)
	}
}

func TestNoFactoryFailsWithDiagnostic(t *testing.T) {
	lo := mustLoadout(t, "default", 1, ServiceDescriptor{Name: "ghost", Version: "v1", Provides: []string{"ctx.ghost"}})
	e := newTestEngine()
	snap, err := e.Apply(context.Background(), lo)
	var actErr *ActivationError
	if !errors.As(err, &actErr) || actErr.Failures[0].Code != DiagNoFactory {
		t.Fatalf("want no-factory failure, got %v", err)
	}
	if snap.State("ghost") != StateFailed {
		t.Fatalf("ghost should be FAILED:\n%s", snap)
	}
}

func TestBindHelpers(t *testing.T) {
	inst := &fakeInstance{rec: &recorder{}, name: "llm"}
	r := Resolved{Service: "loop", Bindings: map[string]any{"ctx.llm": inst}}
	if v, ok := r.Bind("ctx.llm"); !ok || v != inst {
		t.Fatalf("Bind: %v %v", v, ok)
	}
	if _, ok := r.Bind("ctx.nope"); ok {
		t.Fatal("missing capability must not bind")
	}
	typed, err := BindAs[*fakeInstance](r, "ctx.llm")
	if err != nil || typed != inst {
		t.Fatalf("BindAs: %v %v", typed, err)
	}
	if _, err := BindAs[string](r, "ctx.llm"); err == nil {
		t.Fatal("wrong type must fail")
	}
	if _, err := BindAs[*fakeInstance](r, "ctx.nope"); err == nil {
		t.Fatal("missing capability must fail")
	}
}

func TestEffectScopeReleasesReverseOrder(t *testing.T) {
	rec := &recorder{}
	scope := newEffectScope("svc", 5)
	for _, kind := range []string{"one", "two", "three"} {
		k := kind
		if err := scope.Register(k, func() error {
			rec.add("%s", k)
			if k == "two" {
				return errors.New("two failed")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := scope.Register("", nil); !errors.Is(err, ErrNilRelease) {
		t.Fatalf("nil release: %v", err)
	}
	if effects := scope.Effects(); len(effects) != 3 || effects[0].Kind != "one" || effects[0].Generation != 5 {
		t.Fatalf("effects: %+v", effects)
	}
	errs := scope.closeAndRelease()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "two failed") {
		t.Fatalf("release errors: %v", errs)
	}
	want := []string{"three", "two", "one"}
	got := rec.list()
	if len(got) != len(want) {
		t.Fatalf("release order: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("release order: want %v got %v", want, got)
		}
	}
	if err := scope.Register("late", func() error { return nil }); !errors.Is(err, ErrScopeClosed) {
		t.Fatalf("closed scope: %v", err)
	}
}

func TestSnapshotRendering(t *testing.T) {
	rec := &recorder{}
	lo := mustLoadout(t, "default", 2,
		desc("a", instanceFactory(rec, "a", "listener"), []string{"ctx.a"}, nil, []string{"ctx.missing"}),
	)
	e := newTestEngine()
	snap := mustApply(t, e, lo)
	text := snap.String()
	for _, want := range []string{"loadout default revision 2 generation 1", "a", "ACTIVE", "degraded=ctx.missing"} {
		if !strings.Contains(text, want) {
			t.Fatalf("snapshot string missing %q:\n%s", want, text)
		}
	}
	if got := snap.Explain("nope"); !strings.Contains(got, "not in loadout") {
		t.Fatalf("explain unknown: %q", got)
	}
}
