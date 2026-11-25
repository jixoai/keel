package harnessapi

// HTTP edge of the sub-agent depth contract (change keel-collab-subagent): both
// entries (this route and the `agent` tool) build the same SpawnRequest, and a
// depth/width refusal answers a typed code before any child session exists.
// The durable tree read is the post-restart projection the UI consumes.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/harnessext"
)

func depthPtr(value int) *int { return &value }

func TestSubagentRouteDepthRefusalIsTyped(t *testing.T) {
	tenant, parent := uuid.New(), uuid.New()
	committer := &fakeCommitter{}
	runner := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: newFakeGate(parent), Edge: committer, Events: &fakeEvents{},
		Runs: &fakeRuns{state: harnessext.RunState{Status: "done"}}, Logger: testLogger(),
		MaxDepth: depthPtr(0),
	})
	server := httptest.NewServer(New(Config{Subagents: runner, Access: permitAllAccess(), Logger: testLogger()}))
	defer server.Close()

	body, code := postJSON(t, server, "/v1/sessions/"+parent.String()+"/subagents", tenant, map[string]any{"task": "nope"})
	if code != http.StatusConflict {
		t.Fatalf("depth refusal = %d (%s), want 409", code, body)
	}
	var refusal struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Code != "subagent_depth_exceeded" {
		t.Fatalf("refusal = %+v", refusal)
	}
	committer.mu.Lock()
	commits := len(committer.commits)
	committer.mu.Unlock()
	if commits != 0 {
		t.Fatalf("depth refusal committed %d input(s)", commits)
	}
}

func TestSubagentRouteWidthRefusalIsTyped(t *testing.T) {
	tenant, parent := uuid.New(), uuid.New()
	runner := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: newFakeGate(parent), Edge: &fakeCommitter{}, Events: &fakeEvents{},
		Runs: &fakeRuns{state: harnessext.RunState{Status: "done"}}, Logger: testLogger(),
		MaxChildren: 1,
	})
	server := httptest.NewServer(New(Config{Subagents: runner, Access: permitAllAccess(), Logger: testLogger()}))
	defer server.Close()

	if body, code := postJSON(t, server, "/v1/sessions/"+parent.String()+"/subagents", tenant, map[string]any{"task": "first"}); code != http.StatusAccepted {
		t.Fatalf("first spawn = %d (%s)", code, body)
	}
	body, code := postJSON(t, server, "/v1/sessions/"+parent.String()+"/subagents", tenant, map[string]any{"task": "second"})
	if code != http.StatusConflict {
		t.Fatalf("width refusal = %d (%s), want 409", code, body)
	}
	var refusal struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Code != "subagent_children_exceeded" {
		t.Fatalf("refusal = %+v", refusal)
	}
}

func TestSubagentRouteRejectsWideningMaxDepth(t *testing.T) {
	tenant, parent := uuid.New(), uuid.New()
	runner := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: newFakeGate(parent), Edge: &fakeCommitter{}, Events: &fakeEvents{},
		Runs: &fakeRuns{state: harnessext.RunState{Status: "done"}}, Logger: testLogger(),
		MaxDepth: depthPtr(3),
	})
	server := httptest.NewServer(New(Config{Subagents: runner, Access: permitAllAccess(), Logger: testLogger()}))
	defer server.Close()

	for _, maxDepth := range []int{4, -1} {
		body, code := postJSON(t, server, "/v1/sessions/"+parent.String()+"/subagents", tenant,
			map[string]any{"task": "wider", "maxDepth": maxDepth})
		if code != http.StatusBadRequest {
			t.Fatalf("maxDepth=%d = %d (%s), want 400", maxDepth, code, body)
		}
	}
}

// fakeSubagentLinks is the durable projection seam.
type fakeSubagentLinks struct {
	tree []harnessext.SubagentLink
	one  map[uuid.UUID]harnessext.SubagentLink
}

func (f fakeSubagentLinks) ListSubagentTree(context.Context, uuid.UUID, uuid.UUID) ([]harnessext.SubagentLink, error) {
	return f.tree, nil
}

func (f fakeSubagentLinks) SubagentLink(_ context.Context, _ uuid.UUID, childID uuid.UUID) (harnessext.SubagentLink, bool, error) {
	link, ok := f.one[childID]
	return link, ok, nil
}

// TestSubagentDurableTreeRead is the post-restart projection: the registry is
// cold, but the durable links still answer the tree and one record.
func TestSubagentDurableTreeRead(t *testing.T) {
	tenant, root, child, grandchild := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	runner := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: newFakeGate(root), Edge: &fakeCommitter{}, Events: &fakeEvents{},
		Runs: &fakeRuns{state: harnessext.RunState{Status: "done"}}, Logger: testLogger(),
	})
	links := fakeSubagentLinks{
		tree: []harnessext.SubagentLink{
			{TenantID: tenant, SessionID: child, ParentSessionID: root, ModeID: "subagent", Depth: 1},
			{TenantID: tenant, SessionID: grandchild, ParentSessionID: child, ModeID: "subagent", Depth: 2},
		},
		one: map[uuid.UUID]harnessext.SubagentLink{
			child: {TenantID: tenant, SessionID: child, ParentSessionID: root, ModeID: "subagent", Depth: 1},
		},
	}
	server := httptest.NewServer(New(Config{
		Subagents: runner, SubagentLinks: links, Access: permitAllAccess(), Logger: testLogger(),
	}))
	defer server.Close()

	var tree struct {
		Subagents []harnessext.SubagentRecord `json:"subagents"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/subagents?rootSessionId="+root.String(), tenant, nil, &tree); code != http.StatusOK {
		t.Fatalf("tree read: %d", code)
	}
	if len(tree.Subagents) != 2 || tree.Subagents[0].Depth != 1 || tree.Subagents[1].Depth != 2 {
		t.Fatalf("tree = %+v", tree.Subagents)
	}
	if tree.Subagents[1].ParentSessionID != child.String() {
		t.Fatalf("grandchild parent = %q", tree.Subagents[1].ParentSessionID)
	}

	var single struct {
		Subagent harnessext.SubagentRecord `json:"subagent"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/subagents/"+child.String(), tenant, nil, &single); code != http.StatusOK {
		t.Fatalf("durable single read: %d", code)
	}
	if single.Subagent.Depth != 1 || single.Subagent.ParentSessionID != root.String() {
		t.Fatalf("durable single = %+v", single.Subagent)
	}
	if code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/subagents/"+uuid.NewString(), tenant, nil, nil); code != http.StatusNotFound {
		t.Fatalf("unknown durable child: %d", code)
	}
}

func postJSON(t *testing.T, server *httptest.Server, path string, tenant uuid.UUID, body any) ([]byte, int) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenant.String())
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return payload, res.StatusCode
}
