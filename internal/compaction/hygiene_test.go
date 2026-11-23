package compaction

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/spill"
)

// memSpill is the in-memory spill.Store used by the hygiene tests. It is
// content-addressed and idempotent like the production blob store, so a test
// can prove both the reference and recovery.
type memSpill struct {
	mu     sync.Mutex
	byID   map[string][]byte
	refs   map[string]spill.Ref
	spills int
	fail   error
}

func newMemSpill() *memSpill {
	return &memSpill{byID: map[string][]byte{}, refs: map[string]spill.Ref{}}
}

func (m *memSpill) Spill(_ context.Context, _ spill.Scope, mime string, data []byte) (spill.Ref, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return spill.Ref{}, m.fail
	}
	digest := spill.Digest(data)
	id := "mem-" + digest[:16]
	if existing, ok := m.byID[id]; ok {
		if !bytes.Equal(existing, data) {
			return spill.Ref{}, errors.New("mem spill: digest collision")
		}
		return m.refs[id], nil
	}
	m.spills++
	m.byID[id] = append([]byte(nil), data...)
	ref := spill.Ref{ID: id, MIME: mime, SHA256: digest, Size: int64(len(data))}
	m.refs[id] = ref
	return ref, nil
}

func (m *memSpill) Load(_ context.Context, _ spill.Scope, ref spill.Ref) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.byID[ref.ID]
	if !ok {
		return nil, errors.New("mem spill: not found")
	}
	return append([]byte(nil), data...), nil
}

func toolResultTextMessage(callID, name, text string) agentcore.ToolResultMessage {
	return agentcore.ToolResultMessage{
		RoleField:  agentcore.RoleToolResult,
		ToolCallID: callID,
		ToolName:   name,
		Content:    agentcore.ContentList{agentcore.NewTextContent(text)},
	}
}

func userText(text string) agentcore.UserMessage {
	return agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent(text)}}
}

func assistantReply(text string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent(text)}}
}

func hygieneSettings() HygieneSettings {
	return HygieneSettings{
		Enabled:            true,
		KeepRecentTokens:   40,
		MinToolResultBytes: 256,
		PreviewBytes:       64,
		OffloadImages:      true,
		ImageRecentTokens:  40,
	}
}

// TestPruneToolResultsReplacesOldLargeResultWithDurableReference is the core
// contract: a large tool result outside the recent window leaves the context
// as a bounded reference, and the store returns the exact original message by
// that reference.
func TestPruneToolResultsReplacesOldLargeResultWithDurableReference(t *testing.T) {
	store := newMemSpill()
	large := strings.Repeat("line of tool output\n", 200) // 4000 bytes
	original := toolResultTextMessage("call-old", "bash", large)
	msgs := agentcore.MessageList{
		userText("run the command"),
		original,
		userText("next step"),
		assistantReply("done"),
		userText("what happened?"),
	}
	settings := hygieneSettings()
	before := EstimateContextTokensConservative(msgs)

	out, report := ApplyHygiene(context.Background(), msgs, settings, store, spill.Scope{TenantID: "t", SessionID: "s"})

	if !report.Changed() || len(report.Pruned) != 1 {
		t.Fatalf("report = %+v, want exactly one pruned result", report)
	}
	pruned := report.Pruned[0]
	if pruned.ToolCallID != "call-old" || pruned.ToolName != "bash" {
		t.Fatalf("pruned identity = %+v", pruned)
	}
	if pruned.Ref.ID == "" || pruned.Ref.SHA256 == "" || pruned.Ref.Size <= 0 {
		t.Fatalf("reference = %+v, want durable identity", pruned.Ref)
	}
	prunedMsg, ok := out[1].(agentcore.ToolResultMessage)
	if !ok {
		t.Fatalf("out[1] = %T, want ToolResultMessage", out[1])
	}
	reference := agentcore.ContentToText(prunedMsg.Content)
	if !strings.HasPrefix(reference, spill.ReferencePrefix) {
		t.Fatalf("replacement = %q, want the spill prefix", reference)
	}
	if !strings.Contains(reference, pruned.Ref.ID) || !strings.Contains(reference, pruned.Ref.SHA256) {
		t.Fatalf("replacement does not name the durable reference: %q", reference)
	}
	if strings.Contains(reference, large) {
		t.Fatal("the full payload leaked back into the context")
	}
	if len(reference) > 800 {
		t.Fatalf("replacement = %d bytes, want a bounded reference", len(reference))
	}
	// The preview is the tool's text, not the JSON recovery envelope.
	if !strings.Contains(reference, "line of tool output") || strings.Contains(reference, `"role":"toolResult"`) {
		t.Fatalf("preview should render the tool text, not the payload JSON: %q", reference)
	}
	// The original message identity survives: call id/name stay addressable.
	if prunedMsg.ToolCallID != "call-old" || prunedMsg.ToolName != "bash" {
		t.Fatalf("replacement lost the tool identity: %+v", prunedMsg)
	}
	// Recovery by reference returns the exact message.
	payload, err := store.Load(context.Background(), spill.Scope{}, pruned.Ref)
	if err != nil {
		t.Fatalf("load by reference: %v", err)
	}
	var recovered agentcore.ToolResultMessage
	if err := json.Unmarshal(payload, &recovered); err != nil {
		t.Fatalf("decode recovered message: %v", err)
	}
	if got := agentcore.ContentToText(recovered.Content); got != large {
		t.Fatalf("recovered content = %d bytes, want %d", len(got), len(large))
	}
	if recovered.ToolCallID != original.ToolCallID || recovered.ToolName != original.ToolName {
		t.Fatalf("recovered identity = %+v", recovered)
	}
	// Token accounting reflects the smaller request view.
	after := EstimateContextTokensConservative(out)
	if !(after < before) {
		t.Fatalf("tokens before=%d after=%d, want a reduction", before, after)
	}
	if report.TokensBefore != before || report.TokensAfter != after {
		t.Fatalf("report tokens = %d→%d, estimate = %d→%d", report.TokensBefore, report.TokensAfter, before, after)
	}
	if pruned.OriginalTokens <= pruned.ReferenceTokens {
		t.Fatalf("pruned tokens = %d→%d, want a reduction", pruned.OriginalTokens, pruned.ReferenceTokens)
	}
}

// TestPruneIsIdempotent: memory hygiene runs on every turn over the same
// transcript; an already-spilled result must not be spilled again.
func TestPruneIsIdempotent(t *testing.T) {
	store := newMemSpill()
	large := strings.Repeat("x", 2000)
	msgs := agentcore.MessageList{
		userText("run"),
		toolResultTextMessage("call-1", "bash", large),
		userText("next"),
		assistantReply("ok"),
		userText("and?"),
	}
	settings := hygieneSettings()
	once, report := ApplyHygiene(context.Background(), msgs, settings, store, spill.Scope{})
	if len(report.Pruned) != 1 {
		t.Fatalf("first pass pruned %d", len(report.Pruned))
	}
	twice, report2 := ApplyHygiene(context.Background(), once, settings, store, spill.Scope{})
	if report2.Changed() {
		t.Fatalf("second pass rewrote already-pruned content: %+v", report2)
	}
	if store.spills != 1 {
		t.Fatalf("spills = %d, want 1", store.spills)
	}
	if got := agentcore.ContentToText(twice[1].(agentcore.ToolResultMessage).Content); got != agentcore.ContentToText(once[1].(agentcore.ToolResultMessage).Content) {
		t.Fatal("second pass changed the reference text")
	}
}

// TestPruneFailureKeepsOriginal pins the failure rule: a store failure reports
// the message and leaves the original payload in context (no silent loss).
func TestPruneFailureKeepsOriginal(t *testing.T) {
	store := newMemSpill()
	store.fail = errors.New("store unavailable")
	large := strings.Repeat("y", 2000)
	original := toolResultTextMessage("call-1", "bash", large)
	msgs := agentcore.MessageList{
		userText("run"),
		original,
		userText("next"),
		assistantReply("ok"),
		userText("and?"),
	}
	before := EstimateContextTokensConservative(msgs)
	out, report := ApplyHygiene(context.Background(), msgs, hygieneSettings(), store, spill.Scope{})
	if report.Changed() {
		t.Fatal("a failed spill must not rewrite the message")
	}
	if len(report.Failures) != 1 || report.Failures[0].Kind != "tool_result_prune" {
		t.Fatalf("failures = %+v, want one pruner failure", report.Failures)
	}
	if got := agentcore.ContentToText(out[1].(agentcore.ToolResultMessage).Content); got != large {
		t.Fatal("the original payload must stay in context on failure")
	}
	if report.TokensAfter != before {
		t.Fatalf("tokens = %d, want unchanged %d", report.TokensAfter, before)
	}
}

// TestPruneSkipsRecentAndNonTextResults: the recent window is protected, and a
// result with a non-text block is never partially spilled.
func TestPruneSkipsRecentAndNonTextResults(t *testing.T) {
	store := newMemSpill()
	large := strings.Repeat("z", 2000)
	recent := toolResultTextMessage("call-recent", "bash", large)
	msgs := agentcore.MessageList{
		userText("run"),
		toolResultTextMessage("call-old-mixed", "bash", large),
		recent,
		userText("and?"),
	}
	// Add a non-text block to the old result so it is ineligible.
	mixed := msgs[1].(agentcore.ToolResultMessage)
	mixed.Content = append(mixed.Content, agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "blob-1"})
	msgs[1] = mixed
	// A large recent window keeps everything verbatim.
	settings := hygieneSettings()
	settings.KeepRecentTokens = 1_000_000
	settings.MinToolResultBytes = 1
	settings.ImageRecentTokens = 1_000_000
	out, report := ApplyHygiene(context.Background(), msgs, settings, store, spill.Scope{})
	if report.Changed() {
		t.Fatalf("nothing should qualify: %+v", report)
	}
	if got := agentcore.ContentToText(out[1].(agentcore.ToolResultMessage).Content); got != large {
		t.Fatal("the mixed result must keep its text block")
	}
}

// TestOffloadImagesOutsideRecentWindow: an old image reference becomes text
// naming its durable blob; a recent reference stays resolvable.
func TestOffloadImagesOutsideRecentWindow(t *testing.T) {
	old := agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "blob-old", MimeType: "image/png", Size: 4096, Name: "before.png"}
	recent := agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "blob-recent", MimeType: "image/jpeg", Size: 2048, Name: "after.jpg"}
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("look"), old}},
		userText("describe it"),
		assistantReply("done"),
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("now this"), recent}},
	}
	settings := hygieneSettings()
	settings.KeepRecentTokens = 20 // only the trailing user message is protected
	out, report := ApplyHygiene(context.Background(), msgs, settings, nil, spill.Scope{})
	if len(report.Offloaded) != 1 {
		t.Fatalf("offloaded = %+v, want one image", report.Offloaded)
	}
	if report.Offloaded[0].BlobID != "blob-old" {
		t.Fatalf("offloaded blob = %q", report.Offloaded[0].BlobID)
	}
	first := out[0].(agentcore.UserMessage)
	text := agentcore.ContentToText(first.Content)
	if !strings.Contains(text, ImageReferencePrefix) || !strings.Contains(text, "blob-old") || !strings.Contains(text, "recoverable") {
		t.Fatalf("offloaded text = %q", text)
	}
	if len(text) > 500 {
		t.Fatalf("offloaded text = %d bytes, want bounded", len(text))
	}
	last := out[3].(agentcore.UserMessage)
	if _, ok := last.Content[1].(agentcore.ImageRefContent); !ok {
		t.Fatalf("recent image block = %T, want the reference preserved", last.Content[1])
	}
}

// TestOffloadImageWithoutBlobIDIsKept: a reference without an id is not
// recoverable, so it stays and the failure is reported.
func TestOffloadImageWithoutBlobIDIsKept(t *testing.T) {
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{
			agentcore.NewTextContent("look"),
			agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, Name: "orphan.png"},
		}},
		userText("next"),
	}
	settings := hygieneSettings()
	settings.KeepRecentTokens = 1
	out, report := ApplyHygiene(context.Background(), msgs, settings, nil, spill.Scope{})
	if report.Changed() {
		t.Fatal("an unrecoverable reference must not be replaced")
	}
	if len(report.Failures) != 1 || report.Failures[0].Kind != "image_offload" {
		t.Fatalf("failures = %+v", report.Failures)
	}
	if _, ok := out[0].(agentcore.UserMessage).Content[1].(agentcore.ImageRefContent); !ok {
		t.Fatal("the original reference must stay")
	}
}

// TestPruneIncompleteReferenceKeepsOriginal: a store that returns an empty id
// or hash cannot prove recoverability, so the message is kept.
func TestPruneIncompleteReferenceKeepsOriginal(t *testing.T) {
	store := &incompleteSpill{}
	large := strings.Repeat("k", 2000)
	original := toolResultTextMessage("call-1", "bash", large)
	msgs := agentcore.MessageList{
		userText("run"),
		original,
		userText("next"),
		assistantReply("ok"),
		userText("and?"),
	}
	out, report := ApplyHygiene(context.Background(), msgs, hygieneSettings(), store, spill.Scope{})
	if report.Changed() {
		t.Fatal("an incomplete reference must not replace the message")
	}
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Reason, "incomplete reference") {
		t.Fatalf("failures = %+v", report.Failures)
	}
	if got := agentcore.ContentToText(out[1].(agentcore.ToolResultMessage).Content); got != large {
		t.Fatal("the original payload must stay")
	}
}

// incompleteSpill returns a reference with no id and no hash.
type incompleteSpill struct{}

func (incompleteSpill) Spill(context.Context, spill.Scope, string, []byte) (spill.Ref, error) {
	return spill.Ref{}, nil
}

func (incompleteSpill) Load(context.Context, spill.Scope, spill.Ref) ([]byte, error) {
	return nil, errors.New("not found")
}

// TestHygieneReportDoesNotTrustStaleUsage pins the reviewer's 50122→50122 bug:
// a large tool result is pruned, but EstimateContextTokens still trusts the
// newest assistant Usage measured over the pre-prune prefix. The hygiene report
// must use full estimates on both sides so the reduction is visible.
func TestHygieneReportDoesNotTrustStaleUsage(t *testing.T) {
	store := newMemSpill()
	large := strings.Repeat("x", 8000)
	withUsage := assistantReply("done")
	withUsage.Usage = &agentcore.Usage{InputTokens: 50000, OutputTokens: 100}
	msgs := agentcore.MessageList{
		userText("run"),
		toolResultTextMessage("call-1", "bash", large),
		withUsage,
		userText("and?"),
	}
	settings := hygieneSettings()
	settings.KeepRecentTokens = 20

	out, report := ApplyHygiene(context.Background(), msgs, settings, store, spill.Scope{})
	if !report.Changed() {
		t.Fatal("the large result should have been pruned")
	}
	// The bug premise: the ordinary estimator still reports the pre-prune Usage.
	if got := EstimateContextTokens(out).Tokens; got < 50000 {
		t.Fatalf("test premise: EstimateContextTokens = %d, want the stale usage figure", got)
	}
	if report.TokensBefore < 2000 {
		t.Fatalf("report.TokensBefore = %d, want a full estimate of the large result", report.TokensBefore)
	}
	if report.TokensAfter >= report.TokensBefore {
		t.Fatalf("report = %d→%d, want a visible post-prune reduction", report.TokensBefore, report.TokensAfter)
	}
	if report.TokensAfter != EstimateContextTokensConservative(out) {
		t.Fatalf("report.TokensAfter = %d, conservative estimate = %d", report.TokensAfter, EstimateContextTokensConservative(out))
	}
}

// TestApplyHygieneDisabled does nothing when the master switch is off.
func TestApplyHygieneDisabled(t *testing.T) {
	store := newMemSpill()
	msgs := agentcore.MessageList{
		userText("run"),
		toolResultTextMessage("call-1", "bash", strings.Repeat("q", 4000)),
		userText("next"),
	}
	before := EstimateContextTokensConservative(msgs)
	out, report := ApplyHygiene(context.Background(), msgs, HygieneSettings{Enabled: false}, store, spill.Scope{})
	if report.Changed() || store.spills != 0 {
		t.Fatalf("disabled hygiene wrote or rewrote: %+v", report)
	}
	if report.TokensBefore != before || report.TokensAfter != before {
		t.Fatalf("disabled hygiene report = %d→%d, want %d", report.TokensBefore, report.TokensAfter, before)
	}
	if EstimateContextTokensConservative(out) != before {
		t.Fatal("disabled hygiene changed the token estimate")
	}
}
