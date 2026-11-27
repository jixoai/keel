package boardintegration

import (
	"net/http"
	"testing"
)

// TestAutomationSurfaceAndWorkersMount proves the board mount wires the
// watcher/schedule management store and starts their consumers (New fails if
// either cannot be built). An empty list is the expected initial state.
func TestAutomationSurfaceAndWorkersMount(t *testing.T) {
	env := newTestEnv(t, func(options *Options) {
		options.DisableAutomation = false
	})
	server := env.server()
	resp := env.do(t, server, http.MethodGet, "/v1/board/watchers", "alice", nil)
	var body struct {
		Watchers []map[string]any `json:"watchers"`
	}
	env.decode(t, resp, &body)
	if len(body.Watchers) != 0 {
		t.Fatalf("watchers = %+v", body.Watchers)
	}
	resp = env.do(t, server, http.MethodGet, "/v1/board/schedules", "alice", nil)
	env.decode(t, resp, &body)
}
