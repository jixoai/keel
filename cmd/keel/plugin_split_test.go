package main

import (
	"context"
	"errors"
	"testing"
	"time"

	boardlog "github.com/jixoai/keel-board/log"
	boardstore "github.com/jixoai/keel-board/store"

	"github.com/jixoai/keel/internal/boardplugin"
	"github.com/jixoai/keel/internal/pluginruntime"
	"github.com/jixoai/keel/internal/superagent"
	"github.com/jixoai/keel/internal/superagentplugin"
)

// pluginSplitDomain is a domain-store stub: Ready decides whether the board
// plugin's Start succeeds, which is how the assembly test injects a board that
// fails after the surface was built.
type pluginSplitDomain struct {
	readyErr error
}

func (d pluginSplitDomain) Domain() string                { return "board" }
func (d pluginSplitDomain) Migrate(context.Context) error { return nil }
func (d pluginSplitDomain) Ready(context.Context) error   { return d.readyErr }

func pluginSplitBoardService() boardplugin.Service {
	clock := func() time.Time { return time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC) }
	return &boardplugin.LogBoard{Board: boardlog.New(boardlog.Options{Store: boardstore.NewMemory(), Now: clock}), Now: clock}
}

// TestBoardStartFailureLeavesSuperagentPendingAndUnbuilt pins the P1-3
// assembly contract: the supervisor is built by the superagent plugin's Start
// (after the board published `board`), not before the graph loads. A board
// that fails must leave superagent PENDING and must not create an orchestrator
// or writer, and the product exposure gate must return no supervisor even if a
// pointer existed.
func TestBoardStartFailureLeavesSuperagentPendingAndUnbuilt(t *testing.T) {
	buildCalls := 0
	candidate := &superagent.Orchestrator{}
	superagentPlugin := superagentplugin.NewPlugin(superagentplugin.Options{
		Domain: pluginSplitDomain{},
		Build: func(context.Context, boardplugin.Service) (*superagentplugin.Built, error) {
			buildCalls++
			return &superagentplugin.Built{Service: struct{ Name string }{Name: "supervisor"}}, nil
		},
	})
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins: []*pluginruntime.Plugin{
			boardplugin.NewPlugin(boardplugin.Options{
				Service: pluginSplitBoardService(),
				Domain:  pluginSplitDomain{readyErr: errors.New("board pool unavailable")},
			}),
			superagentPlugin,
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	if err := runtime.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if status, _ := runtime.Status(boardplugin.PluginID); status.State != pluginruntime.StateFailed {
		t.Fatalf("board = %+v, want failed", status)
	}
	status, _ := runtime.Status(superagentplugin.PluginID)
	if status.State != pluginruntime.StatePending || status.Reason != "waiting for: board" {
		t.Fatalf("superagent = %+v, want PENDING waiting for: board", status)
	}
	if buildCalls != 0 {
		t.Fatalf("supervisor builder ran %d times under a failed board", buildCalls)
	}
	if got := activeSupervisor(runtime, candidate); got != nil {
		t.Fatalf("product exposed a supervisor for a non-active plugin: %+v", got)
	}
}

// TestDisabledBoardLeavesSuperagentPending is the board-absent half: an
// unconfigured board is not a reason to mark superagent Stopped. The dependency
// answer must stay PENDING so an operator sees the missing requirement.
func TestDisabledBoardLeavesSuperagentPending(t *testing.T) {
	buildCalls := 0
	board := boardplugin.NewPlugin(boardplugin.Options{Disabled: true})
	superagentPlugin := superagentplugin.NewPlugin(superagentplugin.Options{
		Domain: pluginSplitDomain{},
		Build: func(context.Context, boardplugin.Service) (*superagentplugin.Built, error) {
			buildCalls++
			return &superagentplugin.Built{Service: struct{}{}}, nil
		},
	})
	runtime, err := pluginruntime.New(pluginruntime.Options{Plugins: []*pluginruntime.Plugin{board, superagentPlugin}})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	if err := runtime.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	status, _ := runtime.Status(superagentplugin.PluginID)
	if status.State != pluginruntime.StatePending || status.Reason != "waiting for: board" {
		t.Fatalf("superagent = %+v, want PENDING waiting for: board", status)
	}
	if buildCalls != 0 {
		t.Fatalf("supervisor builder ran %d times without a board", buildCalls)
	}
}

// TestSupervisorBuildsAfterBoardPublishes is the success half: the builder sees
// the published board service, and only then does the product gate expose the
// supervisor.
func TestSupervisorBuildsAfterBoardPublishes(t *testing.T) {
	buildCalls := 0
	var published bool
	superagentPlugin := superagentplugin.NewPlugin(superagentplugin.Options{
		Domain: pluginSplitDomain{},
		Build: func(_ context.Context, board boardplugin.Service) (*superagentplugin.Built, error) {
			buildCalls++
			published = board != nil
			return &superagentplugin.Built{Service: struct{ Name string }{Name: "supervisor"}}, nil
		},
	})
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins: []*pluginruntime.Plugin{
			boardplugin.NewPlugin(boardplugin.Options{Service: pluginSplitBoardService(), Domain: pluginSplitDomain{}}),
			superagentPlugin,
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	if err := runtime.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if buildCalls != 1 || !published {
		t.Fatalf("build calls = %d published=%v, want 1/true", buildCalls, published)
	}
	if status, _ := runtime.Status(superagentplugin.PluginID); status.State != pluginruntime.StateActive {
		t.Fatalf("superagent = %+v, want active", status)
	}
	candidate := &superagent.Orchestrator{}
	if got := activeSupervisor(runtime, candidate); got != candidate {
		t.Fatalf("product gate = %+v, want the active supervisor", got)
	}
}
