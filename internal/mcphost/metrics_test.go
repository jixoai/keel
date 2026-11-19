// MCP metric tests (P4.7): the series set is fixed at boot and does not grow
// with sessions, app instances or traffic.

package mcphost

import (
	"context"
	"testing"

	keelmetrics "github.com/jixoai/keel/internal/metrics"
)

func familyNames(t *testing.T, reg *keelmetrics.Registry) map[string]keelmetrics.FamilySnapshot {
	t.Helper()
	out := make(map[string]keelmetrics.FamilySnapshot)
	for _, family := range reg.Snapshot(context.Background()) {
		out[family.Name] = family
	}
	return out
}

func sampleCount(family keelmetrics.FamilySnapshot) int {
	return len(family.Samples)
}

func TestMCPMetricsRegisterTheFixedSet(t *testing.T) {
	reg := keelmetrics.NewRegistry()
	NewMetrics(reg)
	families := familyNames(t, reg)
	for _, name := range []string{
		"mcp_servers",
		"mcp_surface_generation",
		"mcp_app_fetch_total",
		"mcp_app_bridge_frames_total",
		"mcp_listen_reconnects_total",
		"mcp_mrtr_rounds",
	} {
		if _, ok := families[name]; !ok {
			t.Fatalf("metric family %q is not registered", name)
		}
	}
}

// TestMCPMetricsSeriesDoNotGrowWithSessions: exercising the instruments from
// 1000 simulated sessions (the labels carry no identity, so the callers look
// identical to the collector) leaves the series count unchanged.
func TestMCPMetricsSeriesDoNotGrowWithSessions(t *testing.T) {
	reg := keelmetrics.NewRegistry()
	m := NewMetrics(reg)
	// One pass establishes every series the fixed vocabulary can create.
	m.BridgeFrame("tools/call", "in")
	m.BridgeFrame("tools/call", "out")
	m.AppFetch("ok")
	m.ServerStates(1, 0)
	m.SurfaceGeneration(1)
	m.ListenReconnect()
	m.MRTRRound(1)
	before := familyNames(t, reg)
	beforeTotal := 0
	for _, family := range before {
		beforeTotal += sampleCount(family)
		if family.Name == "mcp_servers" || family.Name == "mcp_app_bridge_frames_total" || family.Name == "mcp_app_fetch_total" {
			for _, label := range family.Labels {
				if label == "session" || label == "tenant" || label == "app" || label == "toolCallId" {
					t.Fatalf("%s has an identity label %q", family.Name, label)
				}
			}
		}
	}
	for session := 0; session < 1000; session++ {
		m.BridgeFrame("tools/call", "in")
		m.BridgeFrame("tools/call", "out")
		m.AppFetch("ok")
		m.ServerStates(session%3, session%2)
		m.SurfaceGeneration(int64(session))
		m.ListenReconnect()
		m.MRTRRound(session % 6)
	}
	after := familyNames(t, reg)
	afterTotal := 0
	for _, family := range after {
		afterTotal += sampleCount(family)
	}
	if afterTotal != beforeTotal {
		t.Fatalf("series count grew with sessions: %d -> %d", beforeTotal, afterTotal)
	}
	// Cardinality upper bound: 2 server states + 1 generation + 4 fetch results
	// + 15 bridge (5 methods × 3 directions) + 1 reconnect + 1 histogram = 24.
	all := keelmetrics.NewRegistry()
	full := NewMetrics(all)
	for _, state := range []string{"connected", "disconnected"} {
		_ = state
	}
	full.ServerStates(1, 1)
	for _, result := range []string{"ok", "error", "reused", "other"} {
		full.AppFetch(result)
	}
	for _, method := range []string{"tools/call", "resources/read", "ui/message", "ui/open-link", "other"} {
		for _, direction := range []string{"in", "out", "other"} {
			full.BridgeFrame(method, direction)
		}
	}
	full.SurfaceGeneration(1)
	full.ListenReconnect()
	full.MRTRRound(1)
	total := 0
	for _, family := range familyNames(t, all) {
		total += sampleCount(family)
	}
	if total > 24 {
		t.Fatalf("MCP metric series = %d, over the documented upper bound 24", total)
	}
}

func TestMCPMetricsVocabularyFoldsUnknownValues(t *testing.T) {
	reg := keelmetrics.NewRegistry()
	m := NewMetrics(reg)
	m.AppFetch("weird")
	m.BridgeFrame("something/else", "sideways")
	families := familyNames(t, reg)
	fetch := families["mcp_app_fetch_total"]
	if len(fetch.Samples) != 1 || fetch.Samples[0].Labels["result"] != "other" {
		t.Fatalf("app fetch fold = %+v", fetch.Samples)
	}
	bridge := families["mcp_app_bridge_frames_total"]
	if len(bridge.Samples) != 1 || bridge.Samples[0].Labels["method"] != "other" || bridge.Samples[0].Labels["direction"] != "other" {
		t.Fatalf("bridge fold = %+v", bridge.Samples)
	}
}

func TestMCPMetricsNilIsNoOp(t *testing.T) {
	var m *Metrics
	m.ServerStates(1, 0)
	m.SurfaceGeneration(1)
	m.AppFetch("ok")
	m.BridgeFrame("tools/call", "in")
	m.ListenReconnect()
	m.MRTRRound(1)
}
