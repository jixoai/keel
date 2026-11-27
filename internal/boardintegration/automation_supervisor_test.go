package boardintegration

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeFlagReader struct {
	mu    sync.Mutex
	flags BoardFlags
	err   error
}

func (f *fakeFlagReader) Get(context.Context, uuid.UUID) (BoardFlags, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flags, f.err
}

func (f *fakeFlagReader) Set(flags BoardFlags) {
	f.mu.Lock()
	f.flags = flags
	f.mu.Unlock()
}

// TestAutomationSupervisorFollowsFlags proves the watcher/schedule consumers
// pause when the tenant's board/sync switches close and resume when they open.
func TestAutomationSupervisorFollowsFlags(t *testing.T) {
	flags := &fakeFlagReader{flags: BoardFlags{Enabled: true, Sync: true}}
	var starts, stops atomic.Int64
	sup := newAutomationSupervisor(context.Background(), flags, uuid.New(), 10*time.Millisecond, testDiscardLogger(),
		func(context.Context) (func(), error) {
			starts.Add(1)
			return func() { stops.Add(1) }, nil
		})
	t.Cleanup(sup.Stop)

	waitCount := func(what string, get func() int64, want int64) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if get() >= want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("%s = %d, want >= %d", what, get(), want)
	}
	waitCount("starts", starts.Load, 1)

	flags.Set(BoardFlags{Enabled: true})
	waitCount("stops", stops.Load, 1)

	flags.Set(BoardFlags{Enabled: true, Sync: true})
	waitCount("starts", starts.Load, 2)

	flags.Set(BoardFlags{})
	waitCount("stops", stops.Load, 2)
}

func testDiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
