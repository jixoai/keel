package superagentplugin_test

import (
	"context"
	"testing"
	"time"

	boardlog "github.com/jixoai/keel-board/log"
	boardstore "github.com/jixoai/keel-board/store"

	"github.com/jixoai/keel/internal/boardplugin"
	"github.com/jixoai/keel/internal/pluginruntime"
	"github.com/jixoai/keel/internal/superagentplugin"
)

type memoryDomain struct{}

func (memoryDomain) Domain() string                { return "superagent" }
func (memoryDomain) Migrate(context.Context) error { return nil }
func (memoryDomain) Ready(context.Context) error   { return nil }

func boardPluginForTest() *pluginruntime.Plugin {
	clock := func() time.Time { return time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC) }
	service := &boardplugin.LogBoard{
		Board: boardlog.New(boardlog.Options{Store: boardstore.NewMemory(), Now: clock}),
		Now:   clock,
	}
	return boardplugin.NewPlugin(boardplugin.Options{Service: service, Domain: memoryDomain{}})
}

func TestSuperagentIsPendingWithoutBoard(t *testing.T) {
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins: []*pluginruntime.Plugin{superagentplugin.NewPlugin(superagentplugin.Options{
			Service: struct{ Name string }{Name: "supervisor"},
			Domain:  memoryDomain{},
		})},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := runtime.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	status, _ := runtime.Status(superagentplugin.PluginID)
	if status.State != pluginruntime.StatePending || status.Reason != "waiting for: board" {
		t.Fatalf("superagent = %+v", status)
	}
}

func TestSuperagentStartsAfterBoardAndStopsFirst(t *testing.T) {
	events := make([]string, 0, 4)
	services := pluginruntime.NewServices()
	stopped := make([]string, 0, 2)
	boardPlugin := boardPluginForTest()
	boardStart := boardPlugin.Start
	boardPlugin.Start = func(ctx context.Context, env *pluginruntime.Env) error {
		if err := boardStart(ctx, env); err != nil {
			return err
		}
		events = append(events, "start:board")
		return nil
	}
	boardStop := boardPlugin.Stop
	boardPlugin.Stop = func(ctx context.Context) error {
		stopped = append(stopped, "stop:board")
		return boardStop(ctx)
	}
	superagentPlugin := superagentplugin.NewPlugin(superagentplugin.Options{
		Service: struct{ Name string }{Name: "supervisor"},
		Domain:  memoryDomain{},
	})
	superStart := superagentPlugin.Start
	superagentPlugin.Start = func(ctx context.Context, env *pluginruntime.Env) error {
		if err := superStart(ctx, env); err != nil {
			return err
		}
		events = append(events, "start:superagent")
		return nil
	}
	superStop := superagentPlugin.Stop
	superagentPlugin.Stop = func(ctx context.Context) error {
		stopped = append(stopped, "stop:superagent")
		return superStop(ctx)
	}
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins:  []*pluginruntime.Plugin{superagentPlugin, boardPlugin},
		Services: services,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := runtime.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := services.Get(boardplugin.Capability); !ok {
		t.Fatal("board service missing")
	}
	if _, ok := services.Get(superagentplugin.Capability); !ok {
		t.Fatal("superagent service missing")
	}
	if len(events) != 2 || events[0] != "start:board" || events[1] != "start:superagent" {
		t.Fatalf("start order = %v", events)
	}
	if err := runtime.Unload(context.Background()); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if len(stopped) != 2 || stopped[0] != "stop:superagent" || stopped[1] != "stop:board" {
		t.Fatalf("stop order = %v", stopped)
	}
}

// TestPluginReloadStartsAFreshSupervisor pins the stop/re-arm contract: the
// plugin's Stop must release the first generation and a later Load must start
// and later stop a second one. The old sync.Once stopped the first generation
// and silently leaked every later one (services stayed published and the run
// context was never cancelled).
func TestPluginReloadStartsAFreshSupervisor(t *testing.T) {
	services := pluginruntime.NewServices()
	stopped := 0
	superagentPlugin := superagentplugin.NewPlugin(superagentplugin.Options{
		Service: struct{ Name string }{Name: "supervisor"},
		Domain:  memoryDomain{},
		Stop: func(context.Context) error {
			stopped++
			return nil
		},
	})
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins:  []*pluginruntime.Plugin{boardPluginForTest(), superagentPlugin},
		Services: services,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	load := func() {
		t.Helper()
		if err := runtime.Load(context.Background()); err != nil {
			t.Fatalf("load: %v", err)
		}
		if _, ok := services.Get(superagentplugin.Capability); !ok {
			t.Fatal("superagent service is missing after load")
		}
	}
	unload := func() {
		t.Helper()
		if err := runtime.Unload(context.Background()); err != nil {
			t.Fatalf("unload: %v", err)
		}
		if _, ok := services.Get(superagentplugin.Capability); ok {
			t.Fatal("superagent service survived unload")
		}
	}
	load()
	unload()
	load()
	unload()
	if stopped != 2 {
		t.Fatalf("Stop callback ran %d times, want 2", stopped)
	}
}
