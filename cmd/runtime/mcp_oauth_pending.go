// MCP OAuth pending-step housekeeping. A browser sign-in is a transient row:
// once it is consumed or expires it can never complete, so an always-on process
// must not let those rows accumulate. The runtime prunes them at boot and then
// on a timer; the store keeps the "never delete an unexpired, unconsumed row"
// rule.

package main

import (
	"context"
	"log/slog"
	"time"
)

// mcpOAuthPendingPruneInterval bounds table growth without turning housekeeping
// into a busy loop; a browser step lives ten minutes by default, so a ten
// minute sweep lags a dead row by at most one interval.
const mcpOAuthPendingPruneInterval = 10 * time.Minute

// mcpOAuthPendingRetention keeps consumed/expired rows for an hour: duplicate
// callbacks stay diagnosable, and a long-running process still never grows the
// table without bound.
const mcpOAuthPendingRetention = time.Hour

// mcpOAuthPendingPruner is the store seam, kept narrow so the loop is testable
// without PostgreSQL. before is the retention cutoff.
type mcpOAuthPendingPruner interface {
	PruneMCPOAuthPending(ctx context.Context, before time.Time) (int64, error)
}

// runMCPOAuthPendingPruner prunes once immediately, then on every interval
// until ctx is cancelled. A failed sweep is logged and retried on the next
// tick; housekeeping must never take the runtime down.
func runMCPOAuthPendingPruner(ctx context.Context, pruner mcpOAuthPendingPruner, interval time.Duration, log *slog.Logger) {
	if pruner == nil || interval <= 0 {
		return
	}
	prune := func() {
		rows, err := pruner.PruneMCPOAuthPending(ctx, time.Now().Add(-mcpOAuthPendingRetention))
		if err != nil {
			log.Warn("mcp oauth pending prune", "err", err)
			return
		}
		if rows > 0 {
			log.Info("mcp oauth pending pruned", "rows", rows)
		}
	}
	prune()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
