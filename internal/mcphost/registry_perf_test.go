package mcphost

// G5-scale cache bound: 20 app documents of ~1.5 MiB each, generated 10 times.
// The registry may keep superseded versions only while the byte-bounded LRU
// allows it; after 300 MiB of document content has passed through, the cache
// must still be at or under DefaultCacheBytes (64 MiB) and the published
// catalogue must stay at 20 apps. This is the exact-number half of the G5 bar;
// the live harness measures the same property through a heap profile.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/mcp"
)

func TestRegistryCacheBoundAtG5Scale(t *testing.T) {
	if testing.Short() {
		t.Skip("G5 scale test allocates ~300 MiB of document content")
	}
	const (
		appCount    = 20
		docBytes    = 1536 * 1024
		generations = 10
	)
	server := &mcp.Server{Entry: mcp.ServerEntry{Name: "perf-fleet", Config: map[string]any{"url": "https://mcp.example.com/mcp"}}}
	apps := make([]mcp.App, 0, appCount)
	for index := 0; index < appCount; index++ {
		uri := fmt.Sprintf("ui://perf/app-%d", index)
		apps = append(apps, mcp.App{
			AppID:       mcp.AppIDFor("perf-fleet", uri),
			Server:      "perf-fleet",
			ResourceURI: uri,
			Tools: []mcp.AppTool{{
				Server:     "perf-fleet",
				Tool:       fmt.Sprintf("ping_%d", index),
				Visibility: []string{"app"},
			}},
		})
	}
	fleet := &fakeFleet{servers: []*mcp.Server{server}, apps: apps}

	revision := 0
	registry := newTestRegistry(t, Options{})
	read := func(_ context.Context, _ *mcp.Server, uri string) (mcp.AppResource, error) {
		prefix := fmt.Sprintf("<!doctype html><html><body><!-- revision=%d uri=%s -->", revision, uri)
		suffix := "</body></html>"
		pad := docBytes - len(prefix) - len(suffix)
		if pad < 0 {
			pad = 0
		}
		return mcp.AppResource{
			URI:      uri,
			MIMEType: mcp.AppMIMEType,
			Text:     prefix + strings.Repeat("x", pad) + suffix,
		}, nil
	}
	registry.options.ReadApp = read

	for generation := 0; generation < generations; generation++ {
		revision = generation + 1
		surface, err := registry.Refresh(context.Background(), fleet)
		if err != nil {
			t.Fatalf("Refresh generation %d: %v", revision, err)
		}
		if len(surface.Apps) != appCount {
			t.Fatalf("generation %d published %d apps, want %d", revision, len(surface.Apps), appCount)
		}
		total := registry.cache.totalBytes()
		if total > DefaultCacheBytes {
			t.Fatalf("generation %d cache total = %d bytes, over the %d-byte bound",
				revision, total, DefaultCacheBytes)
		}
		if entries := registry.cache.count(); entries < appCount {
			t.Fatalf("generation %d cache holds %d entries, want at least the %d current apps",
				revision, entries, appCount)
		}
	}
	if registry.cache.evictionCount() == 0 {
		t.Fatal("no superseded document was ever evicted; the cache did not exercise its bound")
	}
	// The current version of every app stays pinned: the catalogue is servable
	// after the churn, not just present in the map.
	surface, _ := registry.Surface()
	for _, app := range surface.Apps {
		if _, ok := registry.Document(app.AppID, app.Hash); !ok {
			t.Fatalf("the current document of %s is not servable after %d generations", app.AppID, generations)
		}
	}
}
