package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestSubagentLinkProjection covers the durable spawn-link projection: write,
// idempotent repeat, re-point refusal, direct-children and active-run counting,
// the tree read and the session state mark.
func TestSubagentLinkProjection(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, root := runTestSession(t, st)
	child, sibling, fork := uuid.New(), uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{child, sibling, fork} {
		if err := st.CreateSession(ctx, tenant, session); err != nil {
			t.Fatal(err)
		}
	}
	link := SubagentLinkRow{
		TenantID: tenant, SessionID: child, ParentSessionID: root,
		ModeID: "subagent", Depth: 1, MaxDepth: 3, MaxChildren: 8,
		Budget: json.RawMessage(`{"maxTurns":7,"maxTokens":11,"timeoutMs":9000}`),
	}
	if err := st.RecordSubagentLink(ctx, link); err != nil {
		t.Fatalf("record link: %v", err)
	}
	// Idempotent for the same fact.
	if err := st.RecordSubagentLink(ctx, link); err != nil {
		t.Fatalf("record same link again: %v", err)
	}
	got, found, err := st.SubagentLink(ctx, tenant, child)
	if err != nil || !found {
		t.Fatalf("read link: found=%v err=%v", found, err)
	}
	if got.ParentSessionID != root || got.Depth != 1 || got.ModeID != "subagent" ||
		got.MaxDepth != 3 || got.MaxChildren != 8 || len(got.Budget) == 0 {
		t.Fatalf("link = %+v", got)
	}
	// A deterministic child id cannot be re-pointed at another origin.
	if err := st.RecordSubagentLink(ctx, SubagentLinkRow{
		TenantID: tenant, SessionID: child, ParentSessionID: sibling, ModeID: "subagent", Depth: 2, MaxDepth: 3,
	}); err == nil {
		t.Fatal("re-pointing a sub-agent child must fail")
	}
	// A missing depth and mode are refused before SQL.
	if err := st.RecordSubagentLink(ctx, SubagentLinkRow{
		TenantID: tenant, SessionID: sibling, ParentSessionID: root, ModeID: "subagent", MaxDepth: 3,
	}); err == nil {
		t.Fatal("depth 0 link must be refused")
	}
	// A link without a maxDepth is refused (the subtree cap cannot be guessed).
	if err := st.RecordSubagentLink(ctx, SubagentLinkRow{
		TenantID: tenant, SessionID: sibling, ParentSessionID: root, ModeID: "subagent", Depth: 1,
	}); err == nil {
		t.Fatal("link without maxDepth must be refused")
	}
	// A plain session and a fork are not delegation links.
	plain := uuid.New()
	if err := st.CreateSession(ctx, tenant, plain); err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.SubagentLink(ctx, tenant, plain); err != nil || found {
		t.Fatalf("plain session link = found=%v err=%v", found, err)
	}
	// Forks never write these columns; create one with only the fork projection.
	if err := st.CreateSession(ctx, tenant, fork); err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.SubagentLink(ctx, tenant, fork); err != nil || found {
		t.Fatalf("fork session link = found=%v err=%v", found, err)
	}

	// A queued run is active; a second child keeps the width countable.
	if err := st.EnqueueRun(ctx, tenant, child); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSubagentLink(ctx, SubagentLinkRow{
		TenantID: tenant, SessionID: sibling, ParentSessionID: root, ModeID: "subagent", Depth: 1, MaxDepth: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueRun(ctx, tenant, sibling); err != nil {
		t.Fatal(err)
	}
	active, err := st.CountActiveSubagentChildren(ctx, tenant, root)
	if err != nil || active != 2 {
		t.Fatalf("active children = %d, err=%v; want 2", active, err)
	}
	inMode, err := st.CountActiveSubagentSessions(ctx, tenant, "subagent")
	if err != nil || inMode != 2 {
		t.Fatalf("active in mode = %d, err=%v; want 2", inMode, err)
	}
	children, err := st.SubagentChildren(ctx, tenant, root)
	if err != nil || len(children) != 2 {
		t.Fatalf("children = %+v, err=%v", children, err)
	}
	tree, err := st.SubagentTree(ctx, tenant, root)
	if err != nil || len(tree) != 2 {
		t.Fatalf("tree = %+v, err=%v", tree, err)
	}
	for _, row := range tree {
		if row.Depth != 1 || row.ParentSessionID != root || row.ModeID != "subagent" {
			t.Fatalf("tree row = %+v", row)
		}
		if row.RunStatus == "" {
			t.Fatalf("tree row lost its run status: %+v", row)
		}
	}

	// A finished run frees the width slot.
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status = 'done' WHERE tenant_id = $1 AND session_id = $2`, tenant, child); err != nil {
		t.Fatal(err)
	}
	active, err = st.CountActiveSubagentChildren(ctx, tenant, root)
	if err != nil || active != 1 {
		t.Fatalf("active children after done = %d, err=%v; want 1", active, err)
	}

	// The session state mark and the session info projection.
	info, err := st.GetSessionInfo(ctx, tenant, child)
	if err != nil || info.Archived || info.State == "cancelled" {
		t.Fatalf("session info = %+v, err=%v", info, err)
	}
	if err := st.SetSessionState(ctx, tenant, child, "cancelled"); err != nil {
		t.Fatalf("set session state: %v", err)
	}
	info, err = st.GetSessionInfo(ctx, tenant, child)
	if err != nil || info.State != "cancelled" {
		t.Fatalf("session info after cancel = %+v, err=%v", info, err)
	}
	if err := st.SetSessionState(ctx, tenant, uuid.New(), "cancelled"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("missing session state = %v, want ErrNoSession", err)
	}

	// Tenant isolation: the row is invisible through another tenant.
	if _, found, err := st.SubagentLink(ctx, uuid.New(), child); err != nil || found {
		t.Fatalf("cross-tenant link = found=%v err=%v", found, err)
	}
	if children, err := st.SubagentChildren(ctx, uuid.New(), root); err != nil || len(children) != 0 {
		t.Fatalf("cross-tenant children = %+v, err=%v", children, err)
	}
}

// TestSubagentLinkCapsBudgetAndCancel covers the P1/P2 projection additions:
// maxDepth/maxChildren/budget survive the round trip, the tree carries the run
// status, a ghost link can be cleared, and a queued run is durably cancelled.
func TestSubagentLinkCapsBudgetAndCancel(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, root := runTestSession(t, st)
	child := uuid.New()
	if err := st.CreateSession(ctx, tenant, child); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSubagentLink(ctx, SubagentLinkRow{
		TenantID: tenant, SessionID: child, ParentSessionID: root, ModeID: "subagent",
		Depth: 1, MaxDepth: 1, MaxChildren: 4, Budget: json.RawMessage(`{"maxTurns":7}`),
	}); err != nil {
		t.Fatalf("record link: %v", err)
	}
	got, found, err := st.SubagentLink(ctx, tenant, child)
	if err != nil || !found {
		t.Fatalf("read link: found=%v err=%v", found, err)
	}
	if got.MaxDepth != 1 || got.MaxChildren != 4 || len(got.Budget) == 0 {
		t.Fatalf("link caps/budget = %+v", got)
	}
	tree, err := st.SubagentTree(ctx, tenant, root)
	if err != nil || len(tree) != 1 {
		t.Fatalf("tree = %+v err=%v", tree, err)
	}
	if tree[0].MaxDepth != 1 || tree[0].MaxChildren != 4 || len(tree[0].Budget) == 0 {
		t.Fatalf("tree caps/budget = %+v", tree[0])
	}
	// A queued run is durably cancelled.
	if err := st.EnqueueRun(ctx, tenant, child); err != nil {
		t.Fatal(err)
	}
	cancelled, status, err := st.CancelQueuedRun(ctx, tenant, child, "operator stop")
	if err != nil || !cancelled || status != "failed" {
		t.Fatalf("cancel queued = cancelled=%v status=%q err=%v", cancelled, status, err)
	}
	run, err := st.GetRun(ctx, tenant, child)
	if err != nil || run.Status != "failed" || run.LastError != "operator stop" {
		t.Fatalf("cancelled run = %+v err=%v", run, err)
	}
	// A terminal row is settled, not cancelled again.
	cancelled, status, err = st.CancelQueuedRun(ctx, tenant, child, "again")
	if err != nil || cancelled || status != "failed" {
		t.Fatalf("repeat cancel = cancelled=%v status=%q err=%v", cancelled, status, err)
	}
	// A ghost link can be cleared.
	if err := st.DeleteSubagentLink(ctx, tenant, child); err != nil {
		t.Fatalf("delete link: %v", err)
	}
	if _, found, err := st.SubagentLink(ctx, tenant, child); err != nil || found {
		t.Fatalf("deleted link = found=%v err=%v", found, err)
	}
}
