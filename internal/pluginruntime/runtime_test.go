package pluginruntime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/pluginruntime"
	"github.com/jixoai/keel/internal/pluginstore"
)

type recorder struct {
	events *[]string
}

func (r recorder) start(name string) func(context.Context, *pluginruntime.Env) error {
	return func(context.Context, *pluginruntime.Env) error {
		*r.events = append(*r.events, "start:"+name)
		return nil
	}
}

func (r recorder) stop(name string) func(context.Context) error {
	return func(context.Context) error {
		*r.events = append(*r.events, "stop:"+name)
		return nil
	}
}

func boardPlugin(events *[]string) *pluginruntime.Plugin {
	rec := recorder{events: events}
	return &pluginruntime.Plugin{
		Spec:  pluginruntime.Spec{ID: "keel.board", Version: "1.0.0", Provides: []string{"board"}},
		Start: rec.start("board"),
		Stop:  rec.stop("board"),
	}
}

func superagentPlugin(events *[]string) *pluginruntime.Plugin {
	rec := recorder{events: events}
	return &pluginruntime.Plugin{
		Spec: pluginruntime.Spec{
			ID: "keel.superagent", Version: "1.0.0",
			Requires: []string{"board"}, Provides: []string{"superagent"},
		},
		Start: rec.start("superagent"),
		Stop:  rec.stop("superagent"),
	}
}

func TestDependencyOrderAndReverseUnload(t *testing.T) {
	events := make([]string, 0, 4)
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins: []*pluginruntime.Plugin{superagentPlugin(&events), boardPlugin(&events)},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := runtime.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(events) != 2 || events[0] != "start:board" || events[1] != "start:superagent" {
		t.Fatalf("activation order = %v", events)
	}
	if !runtime.Active("keel.board") || !runtime.Active("keel.superagent") {
		t.Fatalf("snapshot = %+v", runtime.Snapshot())
	}
	if err := runtime.Unload(context.Background()); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if len(events) != 4 || events[2] != "stop:superagent" || events[3] != "stop:board" {
		t.Fatalf("unload order = %v", events)
	}
}

func TestMissingBoardLeavesSuperagentPending(t *testing.T) {
	events := make([]string, 0)
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins: []*pluginruntime.Plugin{superagentPlugin(&events)},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := runtime.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	status, _ := runtime.Status("keel.superagent")
	if status.State != pluginruntime.StatePending {
		t.Fatalf("state = %s", status.State)
	}
	if status.Reason != "waiting for: board" {
		t.Fatalf("reason = %q", status.Reason)
	}
	if len(events) != 0 {
		t.Fatalf("a pending plugin started: %v", events)
	}
}

func TestDisabledBoardLeavesSuperagentPending(t *testing.T) {
	events := make([]string, 0)
	board := boardPlugin(&events)
	board.Disabled = true
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins: []*pluginruntime.Plugin{board, superagentPlugin(&events)},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := runtime.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	status, _ := runtime.Status("keel.superagent")
	if status.State != pluginruntime.StatePending || status.Reason != "waiting for: board" {
		t.Fatalf("superagent = %+v", status)
	}
	if boardStatus, _ := runtime.Status("keel.board"); boardStatus.State != pluginruntime.StateStopped {
		t.Fatalf("board = %+v", boardStatus)
	}
	if len(events) != 0 {
		t.Fatalf("disabled plugin ran: %v", events)
	}
}

func TestFailedBoardLeavesSuperagentPending(t *testing.T) {
	events := make([]string, 0)
	board := boardPlugin(&events)
	board.Start = func(context.Context, *pluginruntime.Env) error { return errors.New("board pool unavailable") }
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins: []*pluginruntime.Plugin{board, superagentPlugin(&events)},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := runtime.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if status, _ := runtime.Status("keel.board"); status.State != pluginruntime.StateFailed {
		t.Fatalf("board = %+v", status)
	}
	if status, _ := runtime.Status("keel.superagent"); status.State != pluginruntime.StatePending {
		t.Fatalf("superagent = %+v", status)
	}
}

func TestBoardCannotStopUnderActiveSuperagent(t *testing.T) {
	events := make([]string, 0)
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins: []*pluginruntime.Plugin{boardPlugin(&events), superagentPlugin(&events)},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := runtime.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := runtime.StopPlugin(context.Background(), "keel.board"); err == nil {
		t.Fatal("board stopped while superagent was active")
	}
	if err := runtime.StopPlugin(context.Background(), "keel.superagent"); err != nil {
		t.Fatalf("stop superagent: %v", err)
	}
	if err := runtime.StopPlugin(context.Background(), "keel.board"); err != nil {
		t.Fatalf("stop board after superagent: %v", err)
	}
}

func TestDependencyStatusIsPersisted(t *testing.T) {
	tenantID := uuid.New()
	kernel := pluginstore.NewMemory(nil)
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins: []*pluginruntime.Plugin{boardPlugin(new([]string)), superagentPlugin(new([]string))},
		Kernel:  kernel, TenantID: tenantID,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := runtime.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	dependencies, err := kernel.ListDependencies(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("list dependencies: %v", err)
	}
	if len(dependencies) != 2 {
		t.Fatalf("dependencies = %+v", dependencies)
	}
	for _, dependency := range dependencies {
		if dependency.State != pluginstore.DependencyReady {
			t.Fatalf("%s state = %s", dependency.PluginID, dependency.State)
		}
	}
}

func TestCycleAndDuplicateCapabilityAreRejected(t *testing.T) {
	if _, err := pluginruntime.New(pluginruntime.Options{Plugins: []*pluginruntime.Plugin{
		{Spec: pluginruntime.Spec{ID: "a", Requires: []string{"b"}, Provides: []string{"a"}}},
		{Spec: pluginruntime.Spec{ID: "b", Requires: []string{"a"}, Provides: []string{"b"}}},
	}}); err == nil {
		t.Fatal("dependency cycle was accepted")
	}
	if _, err := pluginruntime.New(pluginruntime.Options{Plugins: []*pluginruntime.Plugin{
		{Spec: pluginruntime.Spec{ID: "a", Provides: []string{"board"}}},
		{Spec: pluginruntime.Spec{ID: "b", Provides: []string{"board"}}},
	}}); err == nil {
		t.Fatal("duplicate capability provider was accepted")
	}
}

func TestStatusTimesOutPendingDeterministically(t *testing.T) {
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins: []*pluginruntime.Plugin{superagentPlugin(new([]string))},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_ = runtime.Load(context.Background())
	deadline := time.Now().Add(time.Second)
	for {
		status, _ := runtime.Status("keel.superagent")
		if status.State == pluginruntime.StatePending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never observed: %+v", status)
		}
	}
}
