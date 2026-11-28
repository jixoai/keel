package boardplugin_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	boardlog "github.com/jixoai/keel-board/log"
	boardstore "github.com/jixoai/keel-board/store"

	"github.com/jixoai/keel/internal/boardplugin"
	"github.com/jixoai/keel/internal/pluginruntime"
)

func newBoard(t *testing.T) *boardplugin.LogBoard {
	t.Helper()
	clock := func() time.Time {
		return time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	}
	return &boardplugin.LogBoard{
		Board: boardlog.New(boardlog.Options{Store: boardstore.NewMemory(), Now: clock}),
		Now:   clock,
	}
}

func TestServiceTaskChannelThreadAndAppend(t *testing.T) {
	service := newBoard(t)
	ctx := context.Background()
	tenantID := uuid.New()
	task, err := service.EnsureTicketThread(ctx, tenantID, boardplugin.TicketThread{
		TicketID: "ticket-1", Channel: "ops", Title: "Fix the build", Request: "CI fails.",
		Labels: map[string]string{"source": "test"}, Actor: boardplugin.ServiceActor(tenantID),
	})
	if err != nil {
		t.Fatalf("ensure thread: %v", err)
	}
	if task.ID == 0 || task.State != boardplugin.StateReady || task.Channel != "ops" {
		t.Fatalf("task = %+v", task)
	}
	// Idempotent: the same ticket answers the first task.
	again, err := service.EnsureTicketThread(ctx, tenantID, boardplugin.TicketThread{
		TicketID: "ticket-1", Channel: "ops", Title: "Fix the build", Request: "CI fails.",
		Labels: map[string]string{"source": "test"}, Actor: boardplugin.ServiceActor(tenantID),
	})
	if err != nil || again.ID != task.ID {
		t.Fatalf("repeat = %+v err=%v", again, err)
	}
	tasks, err := service.Tasks(ctx, tenantID, boardplugin.TaskQuery{Channel: "ops"})
	if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Fatalf("tasks = %+v err=%v", tasks, err)
	}
	if filtered, err := service.Tasks(ctx, tenantID, boardplugin.TaskQuery{Channel: "ops", State: boardplugin.StateCompleted}); err != nil || len(filtered) != 0 {
		t.Fatalf("completed filter = %+v err=%v", filtered, err)
	}
	channels, err := service.Channels(ctx, tenantID)
	if err != nil {
		t.Fatalf("channels: %v", err)
	}
	found := false
	for _, channel := range channels {
		if channel.Path == "ops" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ops missing from channels: %+v", channels)
	}
	messageID, err := service.Reply(ctx, tenantID, boardplugin.ReplyRequest{
		TaskID: task.ID, Text: "progress", Actor: boardplugin.ServiceActor(tenantID),
		RequestKey: "reply-1",
	})
	if err != nil || messageID == 0 {
		t.Fatalf("reply = %d err=%v", messageID, err)
	}
	thread, err := service.Thread(ctx, tenantID, task.ID, 0, 100)
	if err != nil || len(thread) != 2 {
		t.Fatalf("thread = %+v err=%v", thread, err)
	}
}

func TestServiceClaimCompleteRoundTrip(t *testing.T) {
	service := newBoard(t)
	ctx := context.Background()
	tenantID := uuid.New()
	task, err := service.EnsureTicketThread(ctx, tenantID, boardplugin.TicketThread{
		TicketID: "ticket-2", Channel: "ops", Title: "Ship it", Actor: boardplugin.ServiceActor(tenantID),
	})
	if err != nil {
		t.Fatalf("ensure thread: %v", err)
	}
	worker := boardplugin.WorkerActor("worker")
	claimed, changed, err := service.Claim(ctx, tenantID, boardplugin.ClaimRequest{
		TaskID: task.ID, Actor: worker, Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err != nil || !changed {
		t.Fatalf("claim = %+v changed=%v err=%v", claimed, changed, err)
	}
	if claimed.State != boardplugin.StateClaimed || claimed.ClaimActor != worker.ID {
		t.Fatalf("claimed = %+v", claimed)
	}
	// A second actor cannot steal the live claim.
	if _, _, err := service.Claim(ctx, tenantID, boardplugin.ClaimRequest{
		TaskID: task.ID, Actor: boardplugin.WorkerActor("other"),
		Token: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}); err == nil {
		t.Fatal("a live claim was stolen")
	}
	if err := service.Complete(ctx, tenantID, boardplugin.SettleRequest{
		TaskID: task.ID, Epoch: claimed.Epoch, Result: "done", Actor: worker,
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	done, err := service.Task(ctx, tenantID, task.ID)
	if err != nil || done.State != boardplugin.StateCompleted {
		t.Fatalf("task after complete = %+v err=%v", done, err)
	}
}

type fakeDomain struct{ migrated, ready bool }

func (d *fakeDomain) Domain() string { return "board" }
func (d *fakeDomain) Migrate(context.Context) error {
	d.migrated = true
	return nil
}
func (d *fakeDomain) Ready(context.Context) error {
	d.ready = true
	return nil
}

func TestBoardPluginLoadUnloadAndDisable(t *testing.T) {
	service := newBoard(t)
	domain := &fakeDomain{}
	services := pluginruntime.NewServices()
	runtime, err := pluginruntime.New(pluginruntime.Options{
		Plugins: []*pluginruntime.Plugin{boardplugin.NewPlugin(boardplugin.Options{
			Service: service, Domain: domain,
		})},
		Services: services,
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	if err := runtime.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !runtime.Active(boardplugin.PluginID) || !domain.migrated || !domain.ready {
		t.Fatalf("board plugin did not activate: %+v", runtime.Snapshot())
	}
	if _, ok := services.Get(boardplugin.Capability); !ok {
		t.Fatal("board service was not published")
	}
	if err := runtime.StopPlugin(context.Background(), boardplugin.PluginID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, ok := services.Get(boardplugin.Capability); ok {
		t.Fatal("board service survived unload")
	}

	disabledDomain := &fakeDomain{}
	disabled, err := pluginruntime.New(pluginruntime.Options{
		Plugins: []*pluginruntime.Plugin{boardplugin.NewPlugin(boardplugin.Options{
			Service: service, Domain: disabledDomain, Disabled: true,
		})},
	})
	if err != nil {
		t.Fatalf("new disabled runtime: %v", err)
	}
	if err := disabled.Load(context.Background()); err != nil {
		t.Fatalf("load disabled: %v", err)
	}
	status, _ := disabled.Status(boardplugin.PluginID)
	if status.State != pluginruntime.StateStopped || disabledDomain.migrated {
		t.Fatalf("disabled board = %+v", status)
	}
}
