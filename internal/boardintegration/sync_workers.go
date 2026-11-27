package boardintegration

// Sync and automation workers (keel-board-v1 tasks 12.1-12.3).
//
// The board module exposes the consumers but does not start them: a deployment
// owns the process that serves a connection. This file is that ownership. One
// inbound and one outbound worker per enabled connection, the board-event
// subscription, and the watcher/schedule workers all run under the Surface's
// context and stop with it.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jixoai/keel-board/automation"
	boardlog "github.com/jixoai/keel-board/log"
	boardserver "github.com/jixoai/keel-board/server"
	boardsync "github.com/jixoai/keel-board/sync"
	boardsyncstore "github.com/jixoai/keel-board/syncstore"
)

// syncWorkerPool runs one inbound and one outbound worker per connection key.
// A key can be stopped and restarted independently (an enable/disable PATCH),
// while the board-event subscription itself is process-wide.
type syncWorkerPool struct {
	server   *boardserver.Server
	store    boardsyncstore.Store
	registry *boardsync.Registry
	tenant   string
	logger   *slog.Logger
	// poll is the inbound/outbound worker's idle wait. Zero keeps the board
	// module's 1s default.
	poll time.Duration
	// actionTimeout bounds one automation action; it must be shorter than the
	// automation store's lease.
	actionTimeout time.Duration

	mu      sync.Mutex
	handles map[boardsync.Key]*workerHandle
}

type workerHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func newSyncWorkerPool(server *boardserver.Server, store boardsyncstore.Store, registry *boardsync.Registry, tenant string, poll time.Duration, logger *slog.Logger) *syncWorkerPool {
	return &syncWorkerPool{
		server: server, store: store, registry: registry, tenant: tenant,
		logger: logger, poll: poll, actionTimeout: 2 * time.Minute,
		handles: make(map[boardsync.Key]*workerHandle),
	}
}

// Start starts one key's workers. Starting a running key is a no-op.
func (p *syncWorkerPool) Start(ctx context.Context, key boardsync.Key) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, running := p.handles[key]; running {
		return nil
	}
	runner, ok := p.server.SyncRunnerFor()
	if !ok {
		return errors.New("boardintegration: sync is not configured on the board server")
	}
	host := p.server.SyncHost()
	if host == nil {
		return errors.New("boardintegration: sync host is not configured")
	}
	hooks := p.server.SyncWorkerHooks()
	inbound, err := boardsyncstore.NewInboundWorker(boardsyncstore.InboundWorkerConfig{
		Store: p.store, Registry: p.registry, Runner: runner,
		Tenant: p.tenant, Key: key, Owner: "keel-inbound-" + string(key),
		PollInterval: p.poll, Hooks: hooks, Logger: p.logger,
	})
	if err != nil {
		return err
	}
	outbound, err := boardsyncstore.NewOutboundWorker(boardsyncstore.OutboundWorkerConfig{
		Store: p.store, Registry: p.registry, Runner: runner, Host: host,
		Tenant: p.tenant, Keys: []boardsync.Key{key},
		Files: boardsync.FileLoader(host.LoadFiles), Owner: "keel-outbound-" + string(key),
		PollInterval: p.poll, Hooks: hooks, Logger: p.logger,
	})
	if err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	handle := &workerHandle{cancel: cancel, done: make(chan struct{})}
	p.handles[key] = handle
	go func() {
		defer close(handle.done)
		var wg sync.WaitGroup
		run := func(name string, fn func(context.Context) error) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := fn(workerCtx); err != nil && workerCtx.Err() == nil {
					p.logger.Error("sync worker stopped", "worker", name, "key", key,
						"error", boardsyncstore.ErrorCode(err))
				}
			}()
		}
		run("inbound", inbound.Run)
		run("outbound", outbound.Run)
		wg.Wait()
	}()
	return nil
}

// Stop stops one key's workers and waits for them to return. Stopping a key
// that isn't running is a no-op.
func (p *syncWorkerPool) Stop(key boardsync.Key) {
	p.mu.Lock()
	handle := p.handles[key]
	delete(p.handles, key)
	p.mu.Unlock()
	if handle == nil {
		return
	}
	handle.cancel()
	select {
	case <-handle.done:
	case <-time.After(10 * time.Second):
		p.logger.Warn("sync workers did not stop in time", "key", key)
	}
}

// StopAll stops every key's workers.
func (p *syncWorkerPool) StopAll() {
	p.mu.Lock()
	keys := make([]boardsync.Key, 0, len(p.handles))
	for key := range p.handles {
		keys = append(keys, key)
	}
	p.mu.Unlock()
	for _, key := range keys {
		p.Stop(key)
	}
}

// RunBoardSubscription follows board events for every owned connection. It
// must run after PrepareSync. One goroutine; a per-connection failure is
// retried inside the board server.
func (p *syncWorkerPool) RunBoardSubscription(ctx context.Context) {
	if err := p.server.RunSync(ctx); err != nil && ctx.Err() == nil {
		p.logger.Error("sync board subscription stopped", "error", boardsyncstore.ErrorCode(err))
	}
}

// startAutomationWorkers runs the watcher and schedule consumers against the
// board log with the same authorizer the HTTP surface uses.
func startAutomationWorkers(ctx context.Context, logger *slog.Logger, store automation.Store, board *boardlog.Board, authorize automation.Authorizer, tenant string) (stop func(), err error) {
	// The post action is the only built-in; the board worker enforces the
	// watcher's configured action name against this registry.
	actions, err := automation.NewRegistryWith(automation.PostAction{})
	if err != nil {
		return nil, err
	}
	watchers, err := automation.NewWatcherWorker(automation.WatcherWorkerConfig{
		Store: store, Board: board, Actions: actions,
		Authorize: authorize, Tenant: tenant, Owner: "keel-watcher",
		ActionTimeout: 2 * time.Minute, Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	schedules, err := automation.NewScheduleWorker(automation.ScheduleWorkerConfig{
		Store: store, Board: board, Authorize: authorize,
		Tenant: tenant, Owner: "keel-schedule", Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	run := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(workerCtx); err != nil && workerCtx.Err() == nil {
				logger.Error("automation worker stopped", "worker", name,
					"error", automation.ErrorCode(err))
			}
		}()
	}
	run("watchers", watchers.Run)
	run("schedules", schedules.Run)
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				logger.Warn("automation workers did not stop in time")
			}
		})
	}
	return stop, nil
}
