// Kernel audit sink: the production implementation of kernel.Options.Audit.
//
// The contract kernel calls the sink for rejections that matter (stale epoch,
// lost lease, quota refusal, store failure, ...). The sink projects them onto
// structured logs, which is the only durable trace such a rejection has: the
// caller receives an error without extra detail, and no metric is allowed to
// carry tenant/session labels.
//
// Two rules govern the projection:
//
//   - Rate limiting: a rejection storm (a drained runtime retrying, a broken
//     client hammering the edge) must not flood the log. Each key — tenant, op,
//     reason — logs at most once per interval; suppressed occurrences are
//     counted and reported with the next record.
//   - No full fencing token: the audit carries owner and epoch as separate
//     low-sensitivity fields. The token string is never logged (logsafe would
//     redact credential-shaped values, but a token is not a credential: it is
//     an identity, so only its parts go in).

package servicehost

import (
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
)

// AuditOptions tunes the audit sink. The zero value uses DefaultAuditInterval
// and time.Now.
type AuditOptions struct {
	// Interval is the per-key suppression window. 0 selects
	// DefaultAuditInterval; a negative value disables suppression.
	Interval time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

// DefaultAuditInterval is the suppression window for one audit key.
const DefaultAuditInterval = 5 * time.Second

type auditEntry struct {
	last       time.Time
	suppressed int
}

// NewAuditSink builds the kernel audit sink. It is safe for concurrent use and
// never blocks on I/O semantics beyond the logger itself.
func NewAuditSink(log *slog.Logger, opts AuditOptions) kernel.AuditFunc {
	if log == nil {
		log = slog.Default()
	}
	interval := opts.Interval
	if interval == 0 {
		interval = DefaultAuditInterval
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	var mu sync.Mutex
	seen := map[string]*auditEntry{}

	return func(v kernel.Violation) {
		key := string(v.Reason) + "|" + v.Op + "|" + v.Scope.TenantID.String()
		when := now()
		mu.Lock()
		entry := seen[key]
		if entry == nil {
			entry = &auditEntry{}
			seen[key] = entry
		}
		if interval > 0 && !entry.last.IsZero() && when.Sub(entry.last) < interval {
			entry.suppressed++
			mu.Unlock()
			return
		}
		suppressed := entry.suppressed
		entry.suppressed = 0
		entry.last = when
		mu.Unlock()

		attrs := []any{
			"reason", string(v.Reason),
			"op", v.Op,
			"tenant", v.Scope.TenantID.String(),
		}
		if v.Scope.SessionID != uuid.Nil {
			attrs = append(attrs, "session", v.Scope.SessionID.String())
		}
		if v.Kind != "" {
			attrs = append(attrs, "kind", string(v.Kind))
		}
		if v.Token.Owner != "" {
			attrs = append(attrs, "owner", v.Token.Owner)
		}
		if v.Token.Epoch != 0 {
			attrs = append(attrs, "epoch", v.Token.Epoch)
		}
		if v.Detail != "" {
			attrs = append(attrs, "detail", v.Detail)
		}
		if suppressed > 0 {
			attrs = append(attrs, "suppressed", suppressed)
		}
		log.Warn("kernel audit: rejection", attrs...)
	}
}
