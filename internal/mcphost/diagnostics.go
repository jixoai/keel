package mcphost

// Diagnostic budgets. Every diagnostic is derived from untrusted server input
// (`_meta`), so the number produced per app must be bounded independently of
// the input size: an oversized or hostile block must not turn into thousands of
// retained strings, a huge `AppSpec.Diagnostics` array, and an equally huge
// `/v1/mcp/apps` response.

import "fmt"

// maxAppDiagnostics bounds the diagnostics of one app (metadata parsing plus
// CSP sanitizing). The registry truncates the combined list to this many
// entries and appends one "...N more dropped" summary line.
const maxAppDiagnostics = 32

// diagCollector accumulates a bounded diagnostic list. Once the budget is
// spent, further messages only increment the dropped counter: no string is
// formatted or retained, so a hostile block cannot grow memory through
// diagnostics.
type diagCollector struct {
	limit   int
	items   []string
	dropped int
}

func newDiagCollector(limit int) *diagCollector {
	if limit <= 0 {
		limit = maxAppDiagnostics
	}
	return &diagCollector{limit: limit}
}

// addf records one formatted diagnostic, or counts it as dropped once full.
func (d *diagCollector) addf(format string, args ...any) {
	if d == nil {
		return
	}
	if len(d.items) >= d.limit {
		d.dropped++
		return
	}
	d.items = append(d.items, fmt.Sprintf(format, args...))
}

// addDropped counts already-known drops (a truncated list) without formatting.
func (d *diagCollector) addDropped(count int) {
	if d == nil || count <= 0 {
		return
	}
	d.dropped += count
}

// full reports whether the budget is spent: callers scanning a long list stop
// early instead of validating millions of entries whose diagnostics would only
// be counted.
func (d *diagCollector) full() bool {
	return d != nil && len(d.items) >= d.limit
}

// finish returns the bounded list, with one summary line for the drops.
func (d *diagCollector) finish() []string {
	if d == nil || (len(d.items) == 0 && d.dropped == 0) {
		return nil
	}
	out := append([]string(nil), d.items...)
	if d.dropped > 0 {
		out = append(out, fmt.Sprintf("…%d more dropped", d.dropped))
	}
	return out
}

// boundDiagnostics truncates a combined list to limit entries and summarizes
// the remainder. It is the last gate before a list reaches AppSpec.Diagnostics.
func boundDiagnostics(items []string, limit int) []string {
	if limit <= 0 {
		limit = maxAppDiagnostics
	}
	if len(items) <= limit {
		return items
	}
	out := append([]string(nil), items[:limit]...)
	out = append(out, fmt.Sprintf("…%d more dropped", len(items)-limit))
	return out
}
