package boardintegration

// Automation supervisors (keel-board-v1 task 12.1 / review P1-1).
//
// The watcher and schedule workers are consumers of the board log. They must
// follow the same tenant switches as the HTTP surface and the sync workers:
// when board.enabled or board.sync.enabled is off they pause, and they resume
// when the operator turns the switches back on. A supervisor owns one
// start/stop pair and reconciles it on the same short interval as the sync
// monitor.

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// automationSupervisor runs one automation consumer group only while the
// tenant's switches are fully enabled.
type automationSupervisor struct {
	ctx      context.Context
	cancel   context.CancelFunc
	flags    BoardFlagReader
	tenant   uuid.UUID
	interval time.Duration
	logger   *slog.Logger
	start    func(context.Context) (stop func(), err error)

	mu      sync.Mutex
	stop    func()
	running bool
	done    chan struct{}
}

func newAutomationSupervisor(ctx context.Context, flags BoardFlagReader, tenant uuid.UUID, interval time.Duration, logger *slog.Logger, start func(context.Context) (func(), error)) *automationSupervisor {
	if interval <= 0 {
		interval = defaultSyncMonitor
	}
	supCtx, cancel := context.WithCancel(ctx)
	sup := &automationSupervisor{
		ctx: supCtx, cancel: cancel, flags: flags, tenant: tenant, interval: interval,
		logger: logger, start: start, done: make(chan struct{}),
	}
	go sup.run()
	return sup
}

func (s *automationSupervisor) run() {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.reconcile()
	for {
		select {
		case <-s.ctx.Done():
			s.mu.Lock()
			s.stopLocked()
			s.mu.Unlock()
			return
		case <-ticker.C:
			s.reconcile()
		}
	}
}

func (s *automationSupervisor) reconcile() {
	if s.flags == nil {
		return
	}
	flags, err := s.flags.Get(s.ctx, s.tenant)
	enabled := err == nil && flags.FullyEnabled()
	if err != nil {
		s.logger.Warn("automation monitor could not read tenant flags; pausing", "tenant", s.tenant, "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if enabled && !s.running {
		stop, startErr := s.start(s.ctx)
		if startErr != nil {
			s.logger.Warn("automation workers could not start", "tenant", s.tenant, "error", startErr)
			return
		}
		s.stop = stop
		s.running = true
		return
	}
	if !enabled && s.running {
		s.stopLocked()
	}
}

func (s *automationSupervisor) stopLocked() {
	if s.stop != nil {
		s.stop()
	}
	s.stop = nil
	s.running = false
}

// Stop pauses the consumers and waits for the supervisor loop to exit.
func (s *automationSupervisor) Stop() {
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Lock()
	s.stopLocked()
	s.mu.Unlock()
}

// Wait blocks until the supervisor loop has stopped.
func (s *automationSupervisor) Wait() {
	if s == nil {
		return
	}
	<-s.done
}
