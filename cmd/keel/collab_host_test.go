package main

// Production collaboration seams (change keel-collab-subagent) against a real
// PostgreSQL store: the SessionIndex recovers depth from the child ledger, the
// link projection round-trips, and the audit sink writes an auth_events row.
// Skipped without KEEL_TEST_DSN, like every store-backed test here.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

func openCollabTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := store.Open(context.Background(), store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func commitSpawnMark(t *testing.T, kern *kernel.Kernel, tenant, session, parent uuid.UUID, depth *int) {
	t.Helper()
	subagent := map[string]any{
		"mode":            "subagent",
		"parentSessionId": parent.String(),
		"origin":          "spawn",
	}
	if depth != nil {
		subagent["depth"] = *depth
	}
	payload := map[string]any{"text": "child", "requestId": "mark-" + uuid.NewString(), "subagent": subagent}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kern.Commit(context.Background(), kernel.CommitRequest{
		Scope:     kernel.Scope{TenantID: tenant, SessionID: session},
		Kind:      kernel.KindUserMessage,
		RequestID: payload["requestId"].(string),
		Payload:   raw,
	}); err != nil {
		t.Fatalf("commit spawn mark: %v", err)
	}
}

func TestCollabSessionIndexReadsDurableDepth(t *testing.T) {
	st := openCollabTestStore(t)
	ctx := context.Background()
	tenant, root, child := uuid.New(), uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "collab-index"); err != nil {
		t.Fatal(err)
	}
	for _, session := range []uuid.UUID{root, child} {
		if err := st.CreateSession(ctx, tenant, session); err != nil {
			t.Fatal(err)
		}
	}
	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(klog, kernel.Options{})
	depth := 1
	commitSpawnMark(t, kern, tenant, child, root, &depth)

	index := collabSessionIndex{store: st, events: kern}
	ref, err := index.Session(ctx, tenant, child)
	if err != nil {
		t.Fatalf("child session: %v", err)
	}
	if ref.ParentSessionID != root || ref.Depth != 1 || ref.ModeID != collab.ModeSubagent || ref.State != collab.SessionActive {
		t.Fatalf("child ref = %+v", ref)
	}
	rootRef, err := index.Session(ctx, tenant, root)
	if err != nil {
		t.Fatalf("root session: %v", err)
	}
	if rootRef.Depth != 0 || rootRef.ParentSessionID != uuid.Nil {
		t.Fatalf("root ref = %+v", rootRef)
	}
	// A pre-depth link fails closed, exactly like the runner's own guard.
	legacy := uuid.New()
	if err := st.CreateSession(ctx, tenant, legacy); err != nil {
		t.Fatal(err)
	}
	commitSpawnMark(t, kern, tenant, legacy, root, nil)
	if _, err := index.Session(ctx, tenant, legacy); !errors.Is(err, harnessext.ErrSubagentLinkInvalid) {
		t.Fatalf("legacy link session = %v, want ErrSubagentLinkInvalid", err)
	}
	// A fork is a ledger branch: depth 0, no parent.
	forked := uuid.New()
	if err := st.CreateSession(ctx, tenant, forked); err != nil {
		t.Fatal(err)
	}
	forkPayload := map[string]any{
		"text": "fork", "requestId": "fork-" + uuid.NewString(),
		"subagent": map[string]any{"parentSessionId": root.String(), "origin": "fork"},
		"fork":     map[string]any{"parentSessionId": root.String(), "originSeq": 3},
	}
	raw, _ := json.Marshal(forkPayload)
	if _, err := kern.Commit(ctx, kernel.CommitRequest{
		Scope: kernel.Scope{TenantID: tenant, SessionID: forked}, Kind: kernel.KindUserMessage,
		RequestID: forkPayload["requestId"].(string), Payload: raw,
	}); err != nil {
		t.Fatal(err)
	}
	forkRef, err := index.Session(ctx, tenant, forked)
	if err != nil {
		t.Fatalf("fork session: %v", err)
	}
	if forkRef.Depth != 0 || forkRef.ParentSessionID != uuid.Nil {
		t.Fatalf("fork ref = %+v", forkRef)
	}
	// A foreign tenant sees a missing session.
	if _, err := index.Session(ctx, uuid.New(), child); !errors.Is(err, collab.ErrSessionNotFound) {
		t.Fatalf("foreign session = %v, want ErrSessionNotFound", err)
	}
}

func TestCollabLinkProjectionAndAuditSink(t *testing.T) {
	st := openCollabTestStore(t)
	ctx := context.Background()
	tenant, root, child := uuid.New(), uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "collab-links"); err != nil {
		t.Fatal(err)
	}
	for _, session := range []uuid.UUID{root, child} {
		if err := st.CreateSession(ctx, tenant, session); err != nil {
			t.Fatal(err)
		}
	}
	links := collabSubagentLinks{store: st}
	if err := links.RecordSubagentLink(ctx, harnessext.SubagentLink{
		TenantID: tenant, SessionID: child, ParentSessionID: root,
		ModeID: collab.ModeSubagent, Depth: 1,
	}); err != nil {
		t.Fatalf("record link: %v", err)
	}
	got, ok, err := links.SubagentLink(ctx, tenant, child)
	if err != nil || !ok || got.Depth != 1 || got.ParentSessionID != root {
		t.Fatalf("link = %+v ok=%v err=%v", got, ok, err)
	}
	children, err := links.SubagentChildren(ctx, tenant, root)
	if err != nil || len(children) != 1 || children[0].SessionID != child {
		t.Fatalf("children = %+v err=%v", children, err)
	}
	tree, err := links.ListSubagentTree(ctx, tenant, root)
	if err != nil || len(tree) != 1 || tree[0].Depth != 1 {
		t.Fatalf("tree = %+v err=%v", tree, err)
	}

	audit := collabAuditSink{store: st, log: discardLogger()}
	if err := audit.Record(ctx, collab.AuditEvent{
		Kind: collab.AuditSessionCreated, Result: "accepted", Operation: "create_session",
		TenantID: tenant, ModeID: collab.ModeSubagent, SessionID: child, ParentSessionID: root,
		BotID: "bot-a", Field: "maxDepth", Limit: 3, Actual: 1,
	}); err != nil {
		t.Fatalf("audit: %v", err)
	}
	events, err := st.ListAuthEvents(ctx, tenant, store.AuthEventFilter{Kind: collab.AuditSessionCreated}, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("audit rows = %+v err=%v", events, err)
	}
	if events[0].Result != store.AuthResultOK {
		t.Fatalf("audit result = %q", events[0].Result)
	}
	if events[0].Detail["parent_session_id"] != root.String() || events[0].Detail["bot_id"] != "bot-a" {
		t.Fatalf("audit detail = %+v", events[0].Detail)
	}
	if _, leaked := events[0].Detail["task"]; leaked {
		t.Fatalf("audit detail carries task text: %+v", events[0].Detail)
	}
}
