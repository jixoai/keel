package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// compactionTestWindow makes a small history compactable. The knobs are the
// production ones (the tenant document's compaction section): a tiny retained
// budget forces the cut to the newest candidate, so a checkpoint has both a
// summarized prefix and a retained tail.
const compactionTestWindow = 1000

// checkpointRecordingStream records the exact message list each provider
// request carried. The replay assertions read the recorded transcript, not the
// log: the point of a checkpoint is what the model is asked next.
type checkpointRecordingStream struct {
	mu    sync.Mutex
	calls []agentcore.MessageList
}

func (s *checkpointRecordingStream) stream(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
	s.mu.Lock()
	s.calls = append(s.calls, append(agentcore.MessageList(nil), llm.Messages...))
	s.mu.Unlock()
	return FakeStream()(ctx, model, llm, cfg)
}

func (s *checkpointRecordingStream) last(t *testing.T) agentcore.MessageList {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("provider was never called")
	}
	return s.calls[len(s.calls)-1]
}

// failingCheckpoints is a checkpoint seam that always fails to save. It proves
// the projection is best-effort: the committed compaction event must survive.
type failingCheckpoints struct{}

func (failingCheckpoints) SaveCheckpoint(context.Context, kernel.Scope, int64, int64, string) error {
	return errors.New("checkpoint store down")
}

func (failingCheckpoints) LoadCheckpoint(context.Context, kernel.Scope) (kernel.Checkpoint, bool, error) {
	return kernel.Checkpoint{}, false, nil
}

func newCompactionRunner(t *testing.T, st *store.Store) *Runner {
	t.Helper()
	runner, _ := newTestRunner(t, st)
	runner.Compaction = &CompactionOptions{
		ContextWindow: compactionTestWindow,
		Settings: compaction.CompactionSettings{
			Enabled:          true,
			ReserveTokens:    0,
			KeepRecentTokens: 1,
		},
	}
	return runner
}

// seedCompaction builds the canonical history: a small exchange, then a long
// input that crosses the window and compacts after its answer. It returns the
// newest checkpoint and the long input's text (the summarized prefix's marker).
func seedCompaction(t *testing.T, st *store.Store) (uuid.UUID, uuid.UUID, *kernel.Kernel, kernel.Checkpoint, string) {
	t.Helper()
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "remember the word ALPHA", "", nil)
	runner := newCompactionRunner(t, st)
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("seed first exchange: %v", err)
	}
	if got := countKind(eventsOf(t, st, tenant, session), store.EventCompaction); got != 0 {
		t.Fatalf("small exchange compacted (%d events), the fixture needs a quiet first turn", got)
	}

	long := strings.Repeat("context filler about the color BLUE. ", 400)
	commitInput(t, k, scope, "r2", long, "", nil)
	requeue(t, st, tenant, session)
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("seed compaction: %v", err)
	}
	cp, found, err := k.LoadCheckpoint(ctx, scope)
	if err != nil || !found {
		t.Fatalf("seed checkpoint = (found=%v, err=%v), want found", found, err)
	}
	return tenant, session, k, cp, long
}

// TestCompactionPersistsEventAndCheckpoint is the real-database integration
// contract: crossing the window appends one compaction event and writes one
// checkpoints row keyed by that event, while the original events stay readable
// by seq. A replay of the same compaction upserts the same row.
func TestCompactionPersistsEventAndCheckpoint(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, cp, _ := seedCompaction(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	events := eventsOf(t, st, tenant, session)
	var compactionSeq int64
	for _, ev := range events {
		if ev.Kind == store.EventCompaction {
			compactionSeq = ev.Seq
			break
		}
	}
	if compactionSeq == 0 {
		t.Fatal("no compaction event committed")
	}
	if countKind(events, store.EventCompaction) != 1 {
		t.Fatalf("compaction events = %d, want 1", countKind(events, store.EventCompaction))
	}
	if cp.UptoSeq != compactionSeq {
		t.Fatalf("checkpoint upto_seq = %d, want compaction seq %d", cp.UptoSeq, compactionSeq)
	}
	if cp.KeptFromSeq <= 0 || cp.KeptFromSeq >= compactionSeq {
		t.Fatalf("checkpoint kept_from_seq = %d, want an anchored tail inside (0, %d)", cp.KeptFromSeq, compactionSeq)
	}
	if strings.TrimSpace(cp.Summary) == "" {
		t.Fatal("checkpoint summary is empty")
	}
	// The legacy store read sees the same row (both layers point at one table).
	stored, found, err := st.LoadCheckpoint(ctx, tenant, session)
	if err != nil || !found {
		t.Fatalf("store checkpoint = (found=%v, err=%v), want found", found, err)
	}
	if stored.UptoSeq != cp.UptoSeq || stored.KeptFromSeq != cp.KeptFromSeq || stored.Summary != cp.Summary {
		t.Fatalf("store checkpoint = %+v, kernel checkpoint = %+v", stored, cp)
	}
	if n := checkpointRows(t, st, tenant, session); n != 1 {
		t.Fatalf("checkpoint rows = %d, want 1", n)
	}

	// The original events are untouched: the summarized user input is still
	// readable at its own seq, below the checkpoint's retained tail.
	var summarizedSeq int64
	for _, ev := range events {
		if ev.Kind == store.EventUserMessage && strings.Contains(eventText(ev), "ALPHA") {
			summarizedSeq = ev.Seq
			break
		}
	}
	if summarizedSeq == 0 || summarizedSeq >= cp.KeptFromSeq {
		t.Fatalf("summarized input seq = %d, kept_from_seq = %d", summarizedSeq, cp.KeptFromSeq)
	}
	page, err := st.LoadEvents(ctx, tenant, session, summarizedSeq-1, 1)
	if err != nil || len(page) != 1 || page[0].Seq != summarizedSeq {
		t.Fatalf("raw event read = %+v (err=%v), want seq %d", page, err, summarizedSeq)
	}

	// A replay of the same compaction updates the projection instead of
	// appending a second row, and does not clear the anchor.
	if err := k.SaveCheckpoint(ctx, scope, cp.UptoSeq, 0, "replayed summary"); err != nil {
		t.Fatalf("replay checkpoint: %v", err)
	}
	got, found, err := k.LoadCheckpoint(ctx, scope)
	if err != nil || !found {
		t.Fatalf("reloaded checkpoint = (found=%v, err=%v)", found, err)
	}
	if got.UptoSeq != cp.UptoSeq || got.KeptFromSeq != cp.KeptFromSeq || got.Summary != "replayed summary" {
		t.Fatalf("replayed checkpoint = %+v, want seq/anchor preserved and summary updated", got)
	}
	if n := checkpointRows(t, st, tenant, session); n != 1 {
		t.Fatalf("checkpoint rows after replay = %d, want 1", n)
	}
}

// TestCompactionCheckpointFailureKeepsEvent pins the best-effort contract: the
// checkpoint is a projection, so a failed save never rolls the committed
// compaction event back (and never fails the run).
func TestCompactionCheckpointFailureKeepsEvent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "remember the word ALPHA", "", nil)
	runner := newCompactionRunner(t, st)
	runner.Checkpoints = failingCheckpoints{}
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	long := strings.Repeat("context filler about the color BLUE. ", 400)
	commitInput(t, k, scope, "r2", long, "", nil)
	requeue(t, st, tenant, session)
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("compaction run with a failing checkpoint store: %v", err)
	}

	events := eventsOf(t, st, tenant, session)
	if got := countKind(events, store.EventCompaction); got != 1 {
		t.Fatalf("compaction events = %d, want 1 even when the checkpoint save failed", got)
	}
	if _, found, err := k.LoadCheckpoint(ctx, scope); err != nil || found {
		t.Fatalf("checkpoint = (found=%v, err=%v), want none", found, err)
	}
}

// TestSecondCompactionAnchorsFromCheckpoint covers an iterative compaction: the
// run starts from checkpoint #1's summary + tail, compacts again, and must
// anchor checkpoint #2 against the same transcript view (the previous summary
// marker is a message in that view, the tool-loadout declaration is not).
func TestSecondCompactionAnchorsFromCheckpoint(t *testing.T) {
	st := testStore(t)
	tenant, session, k, first, _ := seedCompaction(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	long := strings.Repeat("another context filler about GREEN. ", 400)
	commitInput(t, k, scope, "r3", long, "", nil)
	requeue(t, st, tenant, session)
	runner := newCompactionRunner(t, st)
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("second compaction run: %v", err)
	}
	second, found, err := k.LoadCheckpoint(context.Background(), scope)
	if err != nil || !found {
		t.Fatalf("second checkpoint = (found=%v, err=%v), want found", found, err)
	}
	if second.UptoSeq <= first.UptoSeq {
		t.Fatalf("second checkpoint upto_seq = %d, want > %d", second.UptoSeq, first.UptoSeq)
	}
	if second.KeptFromSeq <= 0 || second.KeptFromSeq >= second.UptoSeq {
		t.Fatalf("second checkpoint kept_from_seq = %d, want an anchored tail inside (0, %d)", second.KeptFromSeq, second.UptoSeq)
	}
	if got := countKind(eventsOf(t, st, tenant, session), store.EventCompaction); got != 2 {
		t.Fatalf("compaction events = %d, want 2", got)
	}

	commitInput(t, k, scope, "r4", "what came before?", "", nil)
	requeue(t, st, tenant, session)
	recorder := &checkpointRecordingStream{}
	replay, _ := newTestRunner(t, st)
	replay.Stream = recorder.stream
	if _, err := execRun(t, k, replay, tenant, session); err != nil {
		t.Fatalf("replay after second compaction: %v", err)
	}
	msgs := recorder.last(t)
	checkpoint, ok := msgs[0].(agentcore.CompactionMessage)
	if !ok {
		t.Fatalf("replay starts with %T, want the newest checkpoint summary", msgs[0])
	}
	if checkpoint.Summary != second.Summary {
		t.Fatal("replay used a checkpoint other than the newest")
	}
	if _, ok := firstUserMessageWith(msgs, "GREEN"); ok {
		t.Fatal("replay re-sent input summarized by the second compaction")
	}
}

// TestNewRunReplaysFromCheckpoint is the restart contract: a fresh runner reads
// the newest anchored checkpoint and presents the model its summary plus the
// retained tail, not the summarized history. The raw log still holds every
// original event.
func TestNewRunReplaysFromCheckpoint(t *testing.T) {
	st := testStore(t)
	tenant, session, k, cp, long := seedCompaction(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r3", "what color did I mention?", "", nil)
	requeue(t, st, tenant, session)

	recorder := &checkpointRecordingStream{}
	runner, _ := newTestRunner(t, st)
	runner.Stream = recorder.stream
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("replay run: %v", err)
	}
	msgs := recorder.last(t)
	if len(msgs) < 3 {
		t.Fatalf("replay transcript = %d messages, want checkpoint + retained tail + new input", len(msgs))
	}
	checkpoint, ok := msgs[0].(agentcore.CompactionMessage)
	if !ok {
		t.Fatalf("replay transcript starts with %T, want the checkpoint summary", msgs[0])
	}
	if checkpoint.Summary != cp.Summary {
		t.Fatalf("replay checkpoint summary = %q, want the stored summary", checkpoint.Summary)
	}
	// The summarized prefix is gone as messages (the summary is not a user
	// message), while the retained tail is still there.
	for _, msg := range msgs[1:] {
		if user, ok := msg.(agentcore.UserMessage); ok {
			text := ContentText(user.Content)
			if strings.Contains(text, "ALPHA") || strings.Contains(text, long) {
				t.Fatalf("replay re-sent summarized input: %.60q", text)
			}
		}
	}
	var sawTail, sawNew bool
	for _, msg := range msgs[1:] {
		switch m := msg.(type) {
		case agentcore.AssistantMessage:
			if strings.Contains(ContentText(m.Content), "context filler") {
				sawTail = true
			}
		case agentcore.UserMessage:
			if strings.Contains(ContentText(m.Content), "what color") {
				sawNew = true
			}
		}
	}
	if !sawTail {
		t.Fatal("replay transcript lost the retained assistant tail")
	}
	if !sawNew {
		t.Fatal("replay transcript lost the new input")
	}

	// The raw log is still complete: the summarized user input remains readable
	// at its seq, below the checkpoint anchor.
	events := eventsOf(t, st, tenant, session)
	decoded, _ := DecodeMessages(events, nil)
	if len(decoded) <= len(msgs) {
		t.Fatalf("full log decoded %d messages, replay used %d: no trim happened", len(decoded), len(msgs))
	}
	if !strings.Contains(ContentText(firstUserContent(decoded)), "ALPHA") {
		t.Fatal("raw log lost the summarized user input")
	}
}

// TestReplayFallsBackWithoutCheckpoint deletes the projection row after a
// compaction and proves replay falls back to the full log: the summarized input
// and the inline compaction marker are both present again.
func TestReplayFallsBackWithoutCheckpoint(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, _, _ := seedCompaction(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	if _, err := st.Pool().Exec(ctx, `DELETE FROM checkpoints WHERE tenant_id = $1 AND session_id = $2`, tenant, session); err != nil {
		t.Fatalf("delete checkpoint: %v", err)
	}
	commitInput(t, k, scope, "r3", "continue", "", nil)
	requeue(t, st, tenant, session)

	recorder := &checkpointRecordingStream{}
	runner, _ := newTestRunner(t, st)
	runner.Stream = recorder.stream
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("fallback run: %v", err)
	}
	msgs := recorder.last(t)
	if _, ok := firstUserMessageWith(msgs, "ALPHA"); !ok {
		t.Fatal("fallback replay lost the summarized user input")
	}
	var sawMarker bool
	for _, msg := range msgs {
		if _, ok := msg.(agentcore.CompactionMessage); ok {
			sawMarker = true
			break
		}
	}
	if !sawMarker {
		t.Fatal("fallback replay lost the inline compaction marker")
	}
}

// TestReplayFallsBackWithoutAnchor pins the kept_from_seq=0 contract: a row
// exists but the host never anchored the tail, so replay must not guess a
// truncation point and uses the full log.
func TestReplayFallsBackWithoutAnchor(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, cp, _ := seedCompaction(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	if _, err := st.Pool().Exec(ctx,
		`UPDATE checkpoints SET kept_from_seq = NULL WHERE tenant_id = $1 AND session_id = $2 AND upto_seq = $3`,
		tenant, session, cp.UptoSeq); err != nil {
		t.Fatalf("unanchor checkpoint: %v", err)
	}
	commitInput(t, k, scope, "r3", "continue", "", nil)
	requeue(t, st, tenant, session)

	recorder := &checkpointRecordingStream{}
	runner, _ := newTestRunner(t, st)
	runner.Stream = recorder.stream
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("unanchored run: %v", err)
	}
	msgs := recorder.last(t)
	if _, ok := firstUserMessageWith(msgs, "ALPHA"); !ok {
		t.Fatal("unanchored replay lost the summarized user input")
	}
}

func checkpointRows(t *testing.T, st *store.Store, tenant, session uuid.UUID) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM checkpoints WHERE tenant_id = $1 AND session_id = $2`,
		tenant, session).Scan(&n); err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	return n
}

func eventText(ev store.Event) string {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return ""
	}
	return payload.Text
}

func firstUserContent(msgs agentcore.MessageList) agentcore.ContentList {
	for _, msg := range msgs {
		if user, ok := msg.(agentcore.UserMessage); ok {
			return user.Content
		}
	}
	return nil
}

func firstUserMessageWith(msgs agentcore.MessageList, needle string) (agentcore.UserMessage, bool) {
	for _, msg := range msgs {
		if user, ok := msg.(agentcore.UserMessage); ok && strings.Contains(ContentText(user.Content), needle) {
			return user, true
		}
	}
	return agentcore.UserMessage{}, false
}
