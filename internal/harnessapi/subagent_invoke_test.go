package harnessapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/harnessext"
)

// TestSpawnBotIDRequiresBotInvoke pins the HTTP half of A4.2b1's botId rule:
// the sub-agent routes accept a botId/selection target only
// when the caller holds bot.invoke on it. A call naming no target spawns the
// parent's own Bot and stays available under the route's session.write gate.
func TestSpawnBotIDRequiresBotInvoke(t *testing.T) {
	tenant, parent := uuid.New(), uuid.New()
	committer := &fakeCommitter{}
	runner := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: newFakeGate(parent), Edge: committer, Events: &fakeEvents{},
		Runs: &fakeRuns{state: harnessext.RunState{Status: "done"}}, Logger: testLogger(),
		Poll: time.Millisecond,
	})
	seam := permitAllAccess()
	seam.decisions = map[string]access.Decision{
		access.KeyBotInvoke: {
			Allow: false, Key: access.KeyBotInvoke, Reason: access.ReasonMissingRelation,
			Detail: "no relation satisfies viewer",
		},
	}
	server := httptest.NewServer(New(Config{Subagents: runner, Access: seam, Logger: testLogger()}))
	defer server.Close()

	cases := []struct {
		name string
		body map[string]any
	}{
		{"botId", map[string]any{"task": "delegate", "botId": "member-1"}},
		{"selection botId", map[string]any{"task": "delegate", "selection": map[string]any{"botId": "member-1"}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var refusal map[string]any
			code := doJSON(t, server.Client(), http.MethodPost,
				server.URL+"/v1/sessions/"+parent.String()+"/subagents", tenant, test.body, &refusal)
			if code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%v", code, refusal)
			}
			if refusal["requiredKey"] != access.KeyBotInvoke || refusal["reason"] != string(access.ReasonMissingRelation) {
				t.Fatalf("403 body = %v", refusal)
			}
		})
	}
	committer.mu.Lock()
	commits := len(committer.commits)
	committer.mu.Unlock()
	if commits != 0 {
		t.Fatalf("a refused spawn committed %d turns", commits)
	}
	// No target: the child runs as the parent's Bot and no extra capability is
	// required.
	var accepted struct {
		Subagent harnessext.SubagentRecord `json:"subagent"`
	}
	code := doJSON(t, server.Client(), http.MethodPost,
		server.URL+"/v1/sessions/"+parent.String()+"/subagents", tenant,
		map[string]any{"task": "delegate"}, &accepted)
	if code != http.StatusAccepted {
		t.Fatalf("target-free spawn = %d, want 202", code)
	}
	committer.mu.Lock()
	defer committer.mu.Unlock()
	if len(committer.commits) != 1 {
		t.Fatalf("commits = %d, want 1", len(committer.commits))
	}
}

// TestForkBotIDRequiresBotInvoke mirrors the spawn check on the fork route.
func TestForkBotIDRequiresBotInvoke(t *testing.T) {
	tenant, parent := uuid.New(), uuid.New()
	runner := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: newFakeGate(parent), Edge: &fakeCommitter{}, Events: &fakeEvents{},
		Runs: &fakeRuns{state: harnessext.RunState{Status: "done"}}, Logger: testLogger(),
	})
	seam := permitAllAccess()
	seam.decisions = map[string]access.Decision{
		access.KeyBotInvoke: {Allow: false, Key: access.KeyBotInvoke, Reason: access.ReasonMissingGrant},
	}
	server := httptest.NewServer(New(Config{Subagents: runner, Access: seam, Logger: testLogger()}))
	defer server.Close()
	var body map[string]any
	code := doJSON(t, server.Client(), http.MethodPost,
		server.URL+"/v1/sessions/"+parent.String()+"/fork", tenant,
		map[string]any{"eventSeq": 1, "task": "fork", "botId": "member-1"}, &body)
	if code != http.StatusForbidden {
		t.Fatalf("fork with botId = %d, want 403; body=%v", code, body)
	}
}
