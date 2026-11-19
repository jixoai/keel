package mcphost

// The diagnostic budget itself (P1-B): an input with millions of offending
// members must cost a counter increment per member, not a formatted and
// retained string, and the output stays at the budget plus one summary line.

import (
	"fmt"
	"strings"
	"testing"
)

func TestDiagCollectorBoundsTwoMillionMessages(t *testing.T) {
	collector := newDiagCollector(maxAppDiagnostics)
	for index := 0; index < 2_000_000; index++ {
		collector.addf("unknown key %d", index)
	}
	out := collector.finish()
	if len(out) != maxAppDiagnostics+1 {
		t.Fatalf("diagnostics = %d, want %d + summary", len(out), maxAppDiagnostics)
	}
	want := fmt.Sprintf("…%d more dropped", 2_000_000-maxAppDiagnostics)
	if out[len(out)-1] != want {
		t.Fatalf("summary = %q, want %q", out[len(out)-1], want)
	}
	// The retained strings are the first budget's messages, not the later ones.
	for _, item := range out[:maxAppDiagnostics] {
		if !strings.HasPrefix(item, "unknown key ") {
			t.Fatalf("unexpected diagnostic %q", item)
		}
	}
	if collector.full() != true {
		t.Fatal("the collector does not report a spent budget")
	}
}

func TestBoundDiagnosticsTruncatesCombinedLists(t *testing.T) {
	items := make([]string, 0, 100)
	for index := 0; index < 100; index++ {
		items = append(items, fmt.Sprintf("note %d", index))
	}
	out := boundDiagnostics(items, 10)
	if len(out) != 11 || out[10] != "…90 more dropped" {
		t.Fatalf("out = %v", out)
	}
	if len(boundDiagnostics(items[:5], 10)) != 5 {
		t.Fatal("a list within the budget was truncated")
	}
}
