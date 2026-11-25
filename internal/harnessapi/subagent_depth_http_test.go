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
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/collab"
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
			{TenantID: tenant, SessionID: child, ParentSessionID: root, ModeID: "subagent", Depth: 1,
				MaxDepth: 3, MaxChildren: 8, RunStatus: "running", Budget: &harnessext.Budget{MaxTurns: 25}},
			{TenantID: tenant, SessionID: grandchild, ParentSessionID: child, ModeID: "subagent", Depth: 2,
				MaxDepth: 3, MaxChildren: 8, RunStatus: "done"},
		},
		one: map[uuid.UUID]harnessext.SubagentLink{
			child: {TenantID: tenant, SessionID: child, ParentSessionID: root, ModeID: "subagent", Depth: 1,
				MaxDepth: 3, MaxChildren: 8, RunStatus: "running", Budget: &harnessext.Budget{MaxTurns: 25}},
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
	if tree.Subagents[0].Status != "running" || tree.Subagents[1].Status != "done" {
		t.Fatalf("tree statuses = %q/%q", tree.Subagents[0].Status, tree.Subagents[1].Status)
	}
	if tree.Subagents[0].MaxDepth != 3 || tree.Subagents[0].MaxChildren != 8 || tree.Subagents[0].Budget == nil ||
		tree.Subagents[0].Budget.MaxTurns != 25 {
		t.Fatalf("tree caps/budget = %+v", tree.Subagents[0])
	}
	// The registry is cold; a parent-filtered list merges the durable tree.
	var merged struct {
		Subagents []harnessext.SubagentRecord `json:"subagents"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/subagents?parentSessionId="+root.String(), tenant, nil, &merged); code != http.StatusOK {
		t.Fatalf("merged list: %d", code)
	}
	if len(merged.Subagents) != 1 || merged.Subagents[0].SessionID != child.String() {
		t.Fatalf("merged list = %+v, want the one durable direct child", merged.Subagents)
	}

	var single struct {
		Subagent harnessext.SubagentRecord `json:"subagent"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet,
		server.URL+"/v1/subagents/"+child.String(), tenant, nil, &single); code != http.StatusOK {
		t.Fatalf("durable single read: %d", code)
	}
	if single.Subagent.Depth != 1 || single.Subagent.ParentSessionID != root.String() ||
		single.Subagent.Status != "running" || single.Subagent.MaxDepth != 3 {
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

// TestSubagentSpawnHTTPIdempotencyKey is the P2 guard: the HTTP spawn form now
// forwards the idempotency key, so a retried request answers the same child and
// commits one input.
func TestSubagentSpawnHTTPIdempotencyKey(t *testing.T) {
	tenant, parent := uuid.New(), uuid.New()
	committer := &fakeCommitter{}
	runner := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: newFakeGate(parent), Edge: committer, Events: &fakeEvents{},
		Runs: &fakeRuns{state: harnessext.RunState{Status: "done"}}, Logger: testLogger(),
	})
	server := httptest.NewServer(New(Config{Subagents: runner, Access: permitAllAccess(), Logger: testLogger()}))
	defer server.Close()

	post := func() harnessext.SubagentRecord {
		body, code := postJSON(t, server, "/v1/sessions/"+parent.String()+"/subagents", tenant, map[string]any{
			"task": "delegate", "idempotencyKey": "http-key",
		})
		if code != http.StatusAccepted {
			t.Fatalf("spawn = %d (%s)", code, body)
		}
		var decoded struct {
			Subagent harnessext.SubagentRecord `json:"subagent"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded.Subagent
	}
	first, second := post(), post()
	if first.SessionID != second.SessionID {
		t.Fatalf("retry sessions = %s / %s", first.SessionID, second.SessionID)
	}
	committer.mu.Lock()
	commits := len(committer.commits)
	committer.mu.Unlock()
	if commits != 1 {
		t.Fatalf("idempotent HTTP spawn committed %d inputs, want 1", commits)
	}
}

// TestWriteSubagentErrorMapping pins the Host error codes to HTTP: permission
// 403, concurrency 429, widening 400, missing parent 404, audit failure 503.
func TestWriteSubagentErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "permission",
			err:        collab.ErrPermissionDenied,
			wantStatus: http.StatusForbidden,
			wantCode:   "permission_denied",
		},
		{
			name:       "concurrency",
			err:        &collab.Error{Kind: collab.ErrConcurrencyExceeded, Code: collab.CodeConcurrencyExceeded},
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "concurrency_exceeded",
		},
		{
			name:       "widening",
			err:        &harnessext.SubagentDepthPolicyError{Requested: 4, Effective: 3},
			wantStatus: http.StatusBadRequest,
			wantCode:   "subagent_max_depth_widened",
		},
		{
			name:       "parent missing",
			err:        &collab.Error{Kind: collab.ErrParentSessionNotFound, Code: collab.CodeParentNotFound},
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "audit unavailable",
			err:        collab.ErrAuditUnavailable,
			wantStatus: http.StatusServiceUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeSubagentError(recorder, tc.err)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("%s = %d, want %d", tc.name, recorder.Code, tc.wantStatus)
			}
			if tc.wantCode == "" {
				return
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != tc.wantCode {
				t.Fatalf("%s code = %q, want %q", tc.name, body.Code, tc.wantCode)
			}
		})
	}
}

type httpCanceller struct {
	mu   sync.Mutex
	seen []uuid.UUID
}

func (c *httpCanceller) CancelQueuedRun(_ context.Context, _ uuid.UUID, sessionID uuid.UUID, _ string) (bool, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, sessionID)
	return true, "failed", nil
}

// TestSubagentCancelRouteVerifiesParentAndStops covers the unified cancel
// entry: the child must belong to the authorized parent, and a queued run is
// really cancelled (Cancelled true) with the subtree result lists.
func TestSubagentCancelRouteVerifiesParentAndStops(t *testing.T) {
	tenant, parent := uuid.New(), uuid.New()
	canceller := &httpCanceller{}
	runner := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: newFakeGate(parent), Edge: &fakeCommitter{}, Events: &fakeEvents{},
		Runs: &fakeRuns{state: harnessext.RunState{Status: "pending"}}, Logger: testLogger(),
		Control: harnessext.NewRunControlWithCanceller(&fakeRuns{state: harnessext.RunState{Status: "pending"}}, canceller, testLogger()),
	})
	server := httptest.NewServer(New(Config{Subagents: runner, Access: permitAllAccess(), Logger: testLogger()}))
	defer server.Close()

	body, code := postJSON(t, server, "/v1/sessions/"+parent.String()+"/subagents", tenant, map[string]any{"task": "child"})
	if code != http.StatusAccepted {
		t.Fatalf("spawn = %d (%s)", code, body)
	}
	var spawned struct {
		Subagent harnessext.SubagentRecord `json:"subagent"`
	}
	if err := json.Unmarshal(body, &spawned); err != nil {
		t.Fatal(err)
	}
	childID := spawned.Subagent.SessionID

	// A parent that does not own the child cannot cancel it.
	raw, status := postJSON(t, server, "/v1/subagents/"+childID+"/cancel?parentSessionId="+uuid.NewString(), tenant, map[string]any{})
	if status != http.StatusNotFound {
		t.Fatalf("foreign-parent cancel = %d (%s), want 404", status, raw)
	}
	raw, status = postJSON(t, server, "/v1/subagents/"+childID+"/cancel?parentSessionId="+parent.String(), tenant, map[string]any{"reason": "stop"})
	if status != http.StatusOK {
		t.Fatalf("cancel = %d (%s)", status, raw)
	}
	var cancelled struct {
		Cancel harnessext.CancelResult `json:"cancel"`
	}
	if err := json.Unmarshal(raw, &cancelled); err != nil {
		t.Fatal(err)
	}
	if !cancelled.Cancel.Cancelled || len(cancelled.Cancel.CancelledIDs) != 1 {
		t.Fatalf("cancel result = %+v", cancelled.Cancel)
	}
	canceller.mu.Lock()
	seen := len(canceller.seen)
	canceller.mu.Unlock()
	if seen != 1 {
		t.Fatalf("durable canceller calls = %d, want 1", seen)
	}
}
