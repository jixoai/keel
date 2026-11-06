// Conformance tests for context compaction.
//
// Upstream: pi (commit 55a5c6914570e2ae31ca36297654483bbbf46883)
//   - packages/durable/src/harness/compaction.ts          (contract)
//   - packages/durable/src/harness/agent.ts:28-33,73      (DEFAULT_COMPACTION_POLICY)
//   - packages/durable/src/harness/generation.ts:448-468  (threshold decision)
//   - packages/durable/src/entries.ts:31                  (CompactionEntry shape)
//   - packages/durable/src/harness/context.ts             (orderToolResults)
//   - packages/ai/src/utils/estimate.ts                   (token estimation)
//   - packages/durable/test/harness-compaction.test.ts:234-374,749-960,1066-1120
//     (the it(...) titles below are the upstream ones, verbatim)
//
// Representation differences (keel's flat message list vs pi's entries +
// checkpoints). Each is deliberate and asserted where it changes a fixture:
//
//  1. pi selects cuts over context *entries* (each carrying 0..n messages and
//     an entry id); keel selects over a flat message slice. A pi entry index
//     maps 1:1 to a keel message index when the entry carries one message, so
//     the range-selection fixtures keep their exact expected indices.
//  2. pi has system entries in the view; keel's system prompt is out of band
//     (AgentContext.SystemPrompt), so it is never a message and never a cut
//     candidate. Tests that depend on system-entry tokens are adapted and say
//     so.
//  3. pi has display-only entries (e.g. chat.message) and an edit layer that can
//     omit entries; keel's flat list has neither, so those fixtures are modeled
//     by removing the message and the indices are documented.
//  4. pi's summary request can carry `instructions` ("Additional focus: ...")
//     from Conversation.compact(text); keel's Compact has no instructions
//     channel (see the uncovered list in the task report).
//  5. pi's CompactionEntry stores `head` (first kept entry id) and `data.reason`;
//     keel's CompactionResult stores FirstKeptIndex (the watermark) and the
//     CompactionMessage carries the summary, tokens-before and file details.
//     The summary prefix/suffix rendering is identical and asserted verbatim.
package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/provider"
)

// piText is pi's test fixture generator (harness-compaction.test.ts:47-49):
// about tokens*4 characters starting with label.
func piText(label string, tokens int) string {
	n := tokens*4 - len(label) - 1
	if n < 0 {
		n = 0
	}
	return label + " " + strings.Repeat("x", n)
}

// piSummaryPrefix / piSummarySuffix are verbatim from compaction.ts:69-71.
const (
	piSummaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	piSummarySuffix = "\n</summary>"
)

func piUser(content string) agentcore.UserMessage {
	return agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(content)},
	}
}

// piAssistant mirrors pi's assistant(content, calls, stopReason) fixture
// (harness-compaction.test.ts:196-203): text plus read tool calls whose
// arguments serialize as {}.
func piAssistant(content string, callIDs ...string) agentcore.AssistantMessage {
	return piAssistantWith(content, agentcore.StopReasonEndTurn, callIDs...)
}

func piAssistantWith(content, stopReason string, callIDs ...string) agentcore.AssistantMessage {
	blocks := agentcore.ContentList{}
	if content != "" {
		blocks = append(blocks, agentcore.NewTextContent(content))
	}
	for _, id := range callIDs {
		blocks = append(blocks, agentcore.NewToolCallContent(id, "read", json.RawMessage(`{}`)))
	}
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    blocks,
		StopReason: stopReason,
	}
}

func piToolResult(callID, content string) agentcore.ToolResultMessage {
	return agentcore.ToolResultMessage{
		RoleField:  agentcore.RoleToolResult,
		ToolCallID: callID,
		ToolName:   "read",
		Content:    agentcore.ContentList{agentcore.NewTextContent(content)},
		IsError:    false,
	}
}

// piSettings mirrors pi's MANUAL policy fixture with an explicit budget.
func piSettings(keepRecentTokens, reserveTokens int) CompactionSettings {
	return CompactionSettings{
		Enabled:          true,
		ReserveTokens:    reserveTokens,
		KeepRecentTokens: keepRecentTokens,
	}
}

// piModel is the faux model the upstream tests configure (maxTokens 900,
// contextWindow 100000).
func piModel() provider.Model {
	return provider.Model{Provider: "faux", ID: "faux-1", ContextWindow: 100_000, MaxOutputTokens: 900}
}

type piSummaryRequest struct {
	model string
	llm   provider.LlmContext
	cfg   provider.StreamConfig
}

// piFakeStream is pi's scripted faux provider: every summarization request is
// recorded and answered from the scripted queue.
type piFakeStream struct {
	t        *testing.T
	replies  []agentcore.AssistantMessage
	buildErr error
	requests []piSummaryRequest
}

func (f *piFakeStream) streamFn() provider.StreamFn {
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		f.requests = append(f.requests, piSummaryRequest{model: model, llm: llm, cfg: cfg})
		if f.buildErr != nil {
			return nil, f.buildErr
		}
		if len(f.replies) == 0 {
			f.t.Fatalf("unexpected summarization request #%d", len(f.requests))
		}
		msg := f.replies[0]
		f.replies = f.replies[1:]
		s := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}
}

// piRunCompaction runs Compact against a scripted stream.
func piRunCompaction(t *testing.T, msgs []agentcore.Message, settings CompactionSettings, prevIndex int, replies ...agentcore.AssistantMessage) (*CompactionResult, error, *piFakeStream) {
	t.Helper()
	fake := &piFakeStream{t: t, replies: replies}
	res, err := Compact(context.Background(), fake.streamFn(), piModel(), msgs, settings, prevIndex, nil, "", provider.StreamConfig{})
	return res, err, fake
}

// ─── range selection (harness-compaction.test.ts:234-350) ──────────────────

func TestConformanceRangeSelection(t *testing.T) {
	t.Run("keeps about keepRecentTokens and cuts at the first candidate at or after the budget (spec §8.7 example)", func(t *testing.T) {
		msgs := []agentcore.Message{
			piUser(piText("1", 10)),
			piAssistant("2", "c1"),
			piToolResult("c1", piText("3", 3000)),
			piAssistant(piText("4", 10)),
			piUser(piText("5", 10)),
			piAssistant(piText("6", 10)),
		}
		if got := FindCutPoint(msgs, 2000).FirstKeptIndex; got != 3 {
			t.Fatalf("FirstKeptIndex = %d, want 3 (pi: selectCut(view, 2000) === 3)", got)
		}
	})

	t.Run("cuts at a user entry", func(t *testing.T) {
		msgs := []agentcore.Message{
			piUser(piText("u1", 100)),
			piAssistant(piText("a1", 100)),
			piUser(piText("u2", 100)),
			piAssistant(piText("a2", 100)),
		}
		if got := FindCutPoint(msgs, 150).FirstKeptIndex; got != 2 {
			t.Fatalf("FirstKeptIndex = %d, want 2 (pi: selectCut(view, 150) === 2)", got)
		}
	})

	t.Run("cuts at an assistant in the middle of one long run and never at a tool result", func(t *testing.T) {
		msgs := []agentcore.Message{piUser("do it")}
		for index := 0; index < 5; index++ {
			msgs = append(msgs,
				piAssistant("step "+string(rune('0'+index)), "c"+string(rune('0'+index))),
				piToolResult("c"+string(rune('0'+index)), piText("r"+string(rune('0'+index)), 100)),
			)
		}
		cut := FindCutPoint(msgs, 150).FirstKeptIndex
		// The budget fills at the fourth result; the cut is the last call, whose
		// result it keeps (pi: entries[9] is an assistant entry).
		if cut != 9 {
			t.Fatalf("FirstKeptIndex = %d, want 9", cut)
		}
		if got := msgs[cut].Role(); got != agentcore.RoleAssistant {
			t.Fatalf("cut role = %q, want assistant: a cut must never land on a toolResult", got)
		}
	})

	t.Run("never cuts at a display-only entry where the budget is reached (#10407)", func(t *testing.T) {
		// pi's fixture inserts a display-only chat.message entry between a1 and
		// a2 because its view has entry granularity. keel has no display-only
		// entries, so the message-level fixture drops it: pi's entry index 3
		// (a2) is keel's message index 2.
		msgs := []agentcore.Message{
			piUser(piText("u1", 100)),
			piAssistant(piText("a1", 100)),
			piAssistant(piText("a2", 100)),
		}
		if got := FindCutPoint(msgs, 150).FirstKeptIndex; got != 1 {
			t.Fatalf("FirstKeptIndex with keep=150 = %d, want 1 (pi: 1)", got)
		}
		if got := FindCutPoint(msgs, 50).FirstKeptIndex; got != 2 {
			t.Fatalf("FirstKeptIndex with keep=50 = %d, want 2 (pi: 3, the display entry removed)", got)
		}
	})

	t.Run("keeps a huge last tool result together with its assistant", func(t *testing.T) {
		msgs := []agentcore.Message{
			piUser("u"),
			piAssistant("a", "c"),
			piToolResult("c", piText("big", 5000)),
		}
		if got := FindCutPoint(msgs, 100).FirstKeptIndex; got != 1 {
			t.Fatalf("FirstKeptIndex = %d, want 1 (pi: selectCut(view, 100) === 1)", got)
		}
	})

	t.Run("never cuts at a system entry or an excluded error or aborted answer", func(t *testing.T) {
		// keel has no system entries (the system prompt is out of band), so pi's
		// 100-token system entry that carries the budget past the threshold is
		// absent and the cut differs from pi's entry index 5. What this pins is
		// the exclusion itself: the budget is reached inside the trailing
		// aborted/error answers, and neither may become the cut.
		msgs := []agentcore.Message{
			piUser(piText("u1", 100)),
			piAssistant(piText("a1", 100)),
			piAssistantWith(piText("err", 100), agentcore.StopReasonError),
			piAssistantWith(piText("stopped", 100), agentcore.StopReasonAborted),
		}
		cut := FindCutPoint(msgs, 50).FirstKeptIndex
		if cut != 1 {
			t.Fatalf("FirstKeptIndex = %d, want 1: an error/aborted answer is never a cut point", cut)
		}
		withTail := append(append([]agentcore.Message{}, msgs...), piAssistant(piText("a2", 100)))
		if got := FindCutPoint(withTail, 150).FirstKeptIndex; got != 1 {
			t.Fatalf("FirstKeptIndex with a trailing answer = %d, want 1 (a1), not the excluded answers", got)
		}
	})

	t.Run("follows edited contributions: an omitted entry adds nothing and is no candidate", func(t *testing.T) {
		// keel's flat list has no edit layer; "omitted" is modeled by removing
		// the message (pi: contributions[index] = []).
		plain := []agentcore.Message{
			piUser(piText("u1", 100)),
			piAssistant(piText("a1", 100)),
			piUser(piText("u2", 100)),
			piAssistant(piText("a2", 100)),
		}
		if got := FindCutPoint(plain, 150).FirstKeptIndex; got != 2 {
			t.Fatalf("plain FirstKeptIndex = %d, want 2", got)
		}
		omitted := []agentcore.Message{plain[0], plain[1], plain[3]}
		if got := FindCutPoint(omitted, 150).FirstKeptIndex; got != 1 {
			t.Fatalf("omitted FirstKeptIndex = %d, want 1 (pi: 1)", got)
		}
	})

	t.Run("keeps the newest user message when it tails the context", func(t *testing.T) {
		// The backward walk starts at the tail, so a trailing user message is
		// always at or after the cut and can never be summarized
		// (compaction.ts:293-299 walks `index >= start` from the newest entry).
		msgs := []agentcore.Message{
			piUser(piText("u1", 100)),
			piAssistant(piText("a1", 100)),
			piUser(piText("u2", 100)),
		}
		if cut := FindCutPoint(msgs, 20).FirstKeptIndex; cut != 2 {
			t.Fatalf("FirstKeptIndex = %d, want 2: the newest user message must be kept", cut)
		}
	})

	t.Run("does not cut at a user entry that a result of the preceding call still follows", func(t *testing.T) {
		msgs := []agentcore.Message{
			piUser(piText("u1", 100)),
			piAssistant("a", "c"),
			piUser(piText("steer", 100)),
			piToolResult("c", piText("r", 100)),
			piAssistant(piText("a2", 100)),
		}
		// The budget fills at the steer; its result follows it, so the cut moves
		// to the next assistant (pi compaction.ts:317-324).
		if got := FindCutPoint(msgs, 250).FirstKeptIndex; got != 4 {
			t.Fatalf("FirstKeptIndex = %d, want 4 (pi: selectCut(view, 250) === 4)", got)
		}
	})

	t.Run("finds nothing when the budget is never reached or only the marker precedes the cut", func(t *testing.T) {
		small := []agentcore.Message{piUser("hi"), piAssistant("hello")}
		if got := FindCutPoint(small, 150).FirstKeptIndex; got != 0 {
			t.Fatalf("FirstKeptIndex = %d, want 0 (no cut; pi: undefined)", got)
		}
		if res, err, _ := piRunCompaction(t, small, piSettings(150, 1000), -1); res != nil || err != nil {
			t.Fatalf("Compact = (%v, %v), want (nil, nil) like pi's completed-with-{}", res, err)
		}

		// Only the marker would precede the cut: pi returns undefined
		// (compaction.ts:301-302).
		marker := agentcore.CompactionMessage{RoleField: agentcore.RoleCompaction, Summary: "EARLIER"}
		onlyMarker := []agentcore.Message{marker, piUser(piText("u", 200))}
		if res, err, _ := piRunCompaction(t, onlyMarker, piSettings(150, 1000), 0); res != nil || err != nil {
			t.Fatalf("Compact = (%v, %v), want (nil, nil): only the marker precedes the cut", res, err)
		}
	})

	t.Run("summarizes an earlier summary marker first", func(t *testing.T) {
		marker := agentcore.CompactionMessage{RoleField: agentcore.RoleCompaction, Summary: "EARLIER"}
		msgs := []agentcore.Message{
			marker,
			piUser(piText("u1", 100)),
			piAssistant(piText("a1", 100)),
			piUser(piText("u2", 100)),
			piAssistant(piText("a2", 100)),
		}
		res, err, fake := piRunCompaction(t, msgs, piSettings(150, 1000), 0, piAssistant("SUMMARY"))
		if err != nil || res == nil {
			t.Fatalf("Compact = (%v, %v), want a result", res, err)
		}
		if res.FirstKeptIndex != 3 {
			t.Fatalf("FirstKeptIndex = %d, want 3 (pi: selectCut(selected, 150) === 3)", res.FirstKeptIndex)
		}
		// The folded-in marker counts as covered, like pi's entries.slice(0, cut).
		if res.CoveredMessages != 3 {
			t.Fatalf("CoveredMessages = %d, want 3 (marker + u1 + a1)", res.CoveredMessages)
		}
		prompt := piPromptOf(t, fake)
		// pi stores the marker as a user message carrying the wrapped summary,
		// so the newer summary sees it as the start of the conversation
		// (harness-compaction.test.ts:344-348).
		wantStart := "<conversation>\n[User]: " + piSummaryPrefix + "EARLIER" + piSummarySuffix
		if !strings.HasPrefix(prompt, wantStart) {
			t.Fatalf("summary prompt does not start with the earlier summary marker\n got: %.200q\nwant prefix: %.200q", prompt, wantStart)
		}
		if !strings.Contains(prompt, "[Assistant]: "+piText("a1", 100)) {
			t.Fatalf("summary prompt omits the summarized assistant turn")
		}
		if strings.Contains(prompt, piText("u2", 100)) {
			t.Fatalf("summary prompt includes a kept message (u2)")
		}
	})
}

// ─── serialization (harness-compaction.test.ts:351-374) ────────────────────

func TestConformanceSerialization(t *testing.T) {
	t.Run("writes a transcript, truncates tool results, and omits system messages and thinking", func(t *testing.T) {
		call := agentcore.NewToolCallContent("c", "read", json.RawMessage(`{"path":"a.ts"}`))
		thinking := agentcore.NewThinkingContent("hmm")
		msgs := []agentcore.Message{
			// pi's fixture also has a system message with sections; keel's system
			// prompt never enters the message list, which is the same omission.
			piUser("hello"),
			agentcore.AssistantMessage{
				RoleField: agentcore.RoleAssistant,
				Content:   agentcore.ContentList{thinking, agentcore.NewTextContent("sure"), call},
			},
			piToolResult("c", strings.Repeat("y", 2500)),
		}
		serialized := serializeConversation(msgs)
		if strings.Contains(serialized, "hmm") || strings.Contains(serialized, "[Assistant thinking]") {
			t.Fatalf("thinking must be omitted (compaction.ts:381-385):\n%s", serialized)
		}
		for _, want := range []string{
			"[User]: hello",
			"[Assistant]: sure",
			`[Assistant tool calls]: read(path="a.ts")`,
			"[Tool result]: " + strings.Repeat("y", 2000) + "\n\n[... 500 more characters truncated]",
		} {
			if !strings.Contains(serialized, want) {
				t.Fatalf("serialized transcript missing %q:\n%s", want, serialized)
			}
		}
	})
}

// ─── threshold policy (compaction.ts:337-352, generation.ts:448-468) ───────

func TestConformanceThresholdPolicy(t *testing.T) {
	t.Run("DEFAULT_COMPACTION_POLICY matches pi", func(t *testing.T) {
		// harness/agent.ts:28-33
		want := CompactionSettings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 20000, BackgroundTokens: 32768}
		if DefaultCompactionSettings != want {
			t.Fatalf("DefaultCompactionSettings = %+v, want %+v", DefaultCompactionSettings, want)
		}
	})

	t.Run("below threshold: no compaction", func(t *testing.T) {
		settings := CompactionSettings{Enabled: true, ReserveTokens: 300}
		const window = 1000
		blocking := window - settings.ReserveTokens // 700
		for _, tokens := range []int{0, 1, blocking - 1, blocking} {
			if ShouldCompact(tokens, window, settings) {
				t.Fatalf("ShouldCompact(%d) = true, want false: pi only compacts strictly above %d", tokens, blocking)
			}
			if kind, ok := CompactionTrigger(tokens, window, settings); ok {
				t.Fatalf("CompactionTrigger(%d) = %q, want none", tokens, kind)
			}
		}
		if ShouldCompact(100_000, window, CompactionSettings{Enabled: false, ReserveTokens: 300}) {
			t.Fatal("disabled compaction must never trigger")
		}
		if ShouldCompact(100_000, 0, settings) {
			t.Fatal("an unknown context window must never trigger")
		}
	})

	t.Run("at threshold: compacts once", func(t *testing.T) {
		settings := CompactionSettings{Enabled: true, ReserveTokens: 300, KeepRecentTokens: 150}
		const window = 1000
		if !ShouldCompact(701, window, settings) {
			t.Fatal("ShouldCompact(701) = false, want true: 701 > contextWindow-reserveTokens")
		}
		if kind, ok := CompactionTrigger(701, window, settings); !ok || kind != TriggerBlocking {
			t.Fatalf("CompactionTrigger(701) = (%q, %v), want (blocking, true)", kind, ok)
		}

		// pi starts a background compaction above blocking-backgroundTokens
		// (generation.ts:465-467).
		withBackground := settings
		withBackground.BackgroundTokens = 200
		if kind, ok := CompactionTrigger(501, window, withBackground); !ok || kind != TriggerBackground {
			t.Fatalf("CompactionTrigger(501) = (%q, %v), want (background, true)", kind, ok)
		}
		if _, ok := CompactionTrigger(500, window, withBackground); ok {
			t.Fatal("CompactionTrigger(500) = triggered, want none: the background threshold is strict")
		}
		if ShouldCompact(501, window, withBackground) {
			t.Fatal("a background-level context must not satisfy the blocking ShouldCompact")
		}
	})
}

// ─── manual compaction: summary request + checkpoint (harness-compaction.test.ts:375-432) ──

func TestConformanceSummaryRequestAndCheckpoint(t *testing.T) {
	// Three turns of about 100-token messages (harness-compaction.test.ts:139-144).
	history := []agentcore.Message{
		piUser(piText("u1", 100)),
		piAssistant(piText("a1", 100)),
		piUser(piText("u2", 100)),
		piAssistant(piText("a2", 100)),
		piUser(piText("u3", 100)),
		piAssistant(piText("a3", 100)),
	}

	t.Run("places the summary at once when idle and keeps raw history", func(t *testing.T) {
		before := append([]agentcore.Message{}, history...)
		now := int64(1_700_000_000_000)

		res, err, fake := piRunCompaction(t, history, piSettings(150, 1000), -1, piAssistant("SUMMARY"))
		if err != nil || res == nil {
			t.Fatalf("Compact = (%v, %v), want a result", res, err)
		}
		if res.FirstKeptIndex != 4 {
			t.Fatalf("FirstKeptIndex = %d, want 4 (u3 heads the kept range)", res.FirstKeptIndex)
		}
		if res.CoveredMessages != 4 {
			t.Fatalf("CoveredMessages = %d, want 4 (u1, a1, u2, a2)", res.CoveredMessages)
		}
		if got, want := res.Summary, "SUMMARY"; got != want {
			t.Fatalf("Summary = %q, want %q", got, want)
		}
		if res.TokensBefore != EstimateContextTokens(history).Tokens {
			t.Fatalf("TokensBefore = %d, want the pre-compaction estimate %d", res.TokensBefore, EstimateContextTokens(history).Tokens)
		}

		// Raw history is unchanged: compaction appends a checkpoint, never
		// rewrites the transcript.
		if len(history) != len(before) {
			t.Fatalf("message count changed: %d -> %d", len(before), len(history))
		}
		for i := range before {
			if history[i].Role() != before[i].Role() {
				t.Fatalf("history[%d] role = %q, want %q", i, history[i].Role(), before[i].Role())
			}
		}

		// The checkpoint carries the summary and the watermark.
		entry := res.Message(now)
		if entry.Role() != agentcore.RoleCompaction || entry.Summary != "SUMMARY" || entry.Timestamp != now {
			t.Fatalf("checkpoint = %+v, want a CompactionMessage carrying SUMMARY", entry)
		}
		if entry.TokensBefore != res.TokensBefore {
			t.Fatalf("checkpoint TokensBefore = %d, want %d", entry.TokensBefore, res.TokensBefore)
		}

		// The rebuilt context is the summary followed by the kept messages
		// (pi: view.messages = [marker, u3, a3]).
		rebuilt := res.RebuildContext(history, now)
		if len(rebuilt) != 3 {
			t.Fatalf("rebuilt length = %d, want 3", len(rebuilt))
		}
		if rebuilt[0].Role() != agentcore.RoleCompaction {
			t.Fatalf("rebuilt[0] role = %q, want compaction", rebuilt[0].Role())
		}
		rendered := agentcore.ContentToText(rebuilt[0].(agentcore.CompactionMessage).AsUserMessage().Content)
		wantRendered := "The conversation history before this point was compacted into the following summary:\n\n<summary>\nSUMMARY\n</summary>"
		if rendered != wantRendered {
			t.Fatalf("rendered checkpoint = %q, want %q", rendered, wantRendered)
		}
		if text := agentcore.ContentToText(rebuilt[1].(agentcore.UserMessage).Content); text != piText("u3", 100) {
			t.Fatalf("rebuilt[1] = %.40q, want u3", text)
		}
		if _, ok := rebuilt[2].(agentcore.AssistantMessage); !ok {
			t.Fatalf("rebuilt[2] = %T, want the kept assistant answer", rebuilt[2])
		}

		// The summarizer saw exactly the collapsed prefix: pi's prompt is
		// <conversation> + serializeConversation(range) + </conversation> + the
		// template (compaction.ts:374-378).
		prompt := piPromptOf(t, fake)
		wantPrompt := "<conversation>\n" + serializeConversation(history[:4]) + "\n</conversation>\n\n" + summarizationPrompt
		if prompt != wantPrompt {
			t.Fatalf("summary prompt mismatch\n got: %.300q\nwant: %.300q", prompt, wantPrompt)
		}
		if strings.Contains(prompt, piText("u3", 100)) {
			t.Fatal("summary prompt includes the kept turn u3")
		}
		if !strings.Contains(prompt, "## Goal") {
			t.Fatal("summary prompt omits the structured template")
		}

		request := fake.requests[0]
		if request.model != "faux-1" {
			t.Fatalf("summarization model = %q, want faux-1", request.model)
		}
		if request.llm.SystemPrompt != SUMMARIZATION_SYSTEM_PROMPT {
			t.Fatalf("system prompt = %q, want the summarization system prompt", request.llm.SystemPrompt)
		}
		// pi's request is [system, user]; keel shapes the system prompt as
		// LlmContext.SystemPrompt, so the message list holds the one user turn.
		if len(request.llm.Messages) != 1 {
			t.Fatalf("summary request messages = %d, want 1 user message", len(request.llm.Messages))
		}
		if _, ok := request.llm.Messages[0].(agentcore.UserMessage); !ok {
			t.Fatalf("summary request message = %T, want UserMessage", request.llm.Messages[0])
		}
		if len(request.llm.Tools) != 0 {
			t.Fatalf("summary request offered %d tools, want none", len(request.llm.Tools))
		}
		if got := request.cfg.Extra["max_tokens"]; got != 800 {
			t.Fatalf("max_tokens = %v, want 800 = min(floor(0.8*1000), 900)", got)
		}
	})

	t.Run("caps maxTokens at the model's output limit and sends no tools", func(t *testing.T) {
		// pi: reserveTokens 2000 -> 0.8*2000 = 1600, capped to the model's 900.
		res, err, fake := piRunCompaction(t, history, piSettings(150, 2000), -1, piAssistant("SUMMARY"))
		if err != nil || res == nil {
			t.Fatalf("Compact = (%v, %v), want a result", res, err)
		}
		if got := fake.requests[0].cfg.Extra["max_tokens"]; got != 900 {
			t.Fatalf("max_tokens = %v, want the model limit 900", got)
		}
		if len(fake.requests[0].llm.Tools) != 0 {
			t.Fatal("summary request must not offer tools (pi sends no tools)")
		}
	})

	t.Run("completes without a summary when there is nothing to compact", func(t *testing.T) {
		msgs := []agentcore.Message{piUser("hi"), piAssistant("hello")}
		res, err, fake := piRunCompaction(t, msgs, piSettings(150, 1000), -1)
		if res != nil || err != nil {
			t.Fatalf("Compact = (%v, %v), want (nil, nil)", res, err)
		}
		if len(fake.requests) != 0 {
			t.Fatalf("summarization requests = %d, want 0", len(fake.requests))
		}
	})

	t.Run("leaves excluded answers out of the summary source", func(t *testing.T) {
		// pi's contributions drop error/aborted answers before serialization
		// (harness/context.ts:283-289), so they never reach the summary request.
		msgs := []agentcore.Message{
			piUser(piText("u1", 100)),
			piAssistantWith(piText("err", 100), agentcore.StopReasonError),
			piAssistantWith(piText("a1", 100), agentcore.StopReasonAborted),
			piUser(piText("u2", 100)),
			piAssistant(piText("a2", 100)),
		}
		if _, err, fake := piRunCompaction(t, msgs, piSettings(150, 1000), -1, piAssistant("SUMMARY")); err != nil {
			t.Fatalf("Compact error = %v", err)
		} else {
			prompt := piPromptOf(t, fake)
			if strings.Contains(prompt, piText("err", 100)) || strings.Contains(prompt, piText("a1", 100)) {
				t.Fatalf("summary request serialized an excluded answer:\n%.200q", prompt)
			}
			if !strings.Contains(prompt, piText("u1", 100)) {
				t.Fatal("summary request dropped a normal message")
			}
		}
	})

	t.Run("summarizes the previous summary in a second compaction", func(t *testing.T) {
		// pi: a second compaction folds the first summary in through the
		// transcript (harness-compaction.test.ts:2105-2120).
		first := agentcore.CompactionMessage{RoleField: agentcore.RoleCompaction, Summary: "FIRST"}
		msgs := []agentcore.Message{
			first,
			piUser(piText("u2", 100)),
			piAssistant(piText("a2", 100)),
			piUser(piText("u3", 100)),
			piAssistant(piText("a3", 100)),
		}
		res, err, fake := piRunCompaction(t, msgs, piSettings(150, 1000), 0, piAssistant("SECOND"))
		if err != nil || res == nil {
			t.Fatalf("Compact = (%v, %v), want a result", res, err)
		}
		if !strings.Contains(res.Summary, "SECOND") {
			t.Fatalf("Summary = %q, want the second summary", res.Summary)
		}
		if res.CoveredMessages != 3 {
			t.Fatalf("CoveredMessages = %d, want 3 (first marker + u2 + a2)", res.CoveredMessages)
		}
		prompt := piPromptOf(t, fake)
		if !strings.Contains(prompt, "FIRST") {
			t.Fatal("the second summary request must include the first summary")
		}
	})
}

// piPromptOf returns the single user prompt a summarization request carried.
func piPromptOf(t *testing.T, fake *piFakeStream) string {
	t.Helper()
	if len(fake.requests) == 0 {
		t.Fatal("no summarization request was made")
	}
	req := fake.requests[len(fake.requests)-1]
	if len(req.llm.Messages) != 1 {
		t.Fatalf("summary request messages = %d, want 1", len(req.llm.Messages))
	}
	msg, ok := req.llm.Messages[0].(agentcore.UserMessage)
	if !ok {
		t.Fatalf("summary request message = %T, want UserMessage", req.llm.Messages[0])
	}
	return agentcore.ContentToText(msg.Content)
}

// ─── failure outcomes (harness-compaction.test.ts:905-955) ────────────────

func TestConformanceSummaryFailure(t *testing.T) {
	history := []agentcore.Message{
		piUser(piText("u1", 100)),
		piAssistant(piText("a1", 100)),
		piUser(piText("u2", 100)),
		piAssistant(piText("a2", 100)),
		piUser(piText("u3", 100)),
		piAssistant(piText("a3", 100)),
	}

	for _, tc := range []struct {
		name  string
		reply agentcore.AssistantMessage
		want  string
	}{
		{
			name:  "a non-retryable error",
			reply: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonError, ErrorMessage: "bad request"},
			want:  "Summarization failed: bad request",
		},
		{
			name:  "a length stop",
			reply: piAssistantWith("partial", agentcore.StopReasonLength),
			want:  "Summarization hit the token limit; the summary is incomplete",
		},
		{
			name:  "a tool call",
			reply: piAssistantWith("", agentcore.StopReasonToolUse, "read"),
			want:  "Summarization attempted to call a tool",
		},
		{
			name:  "empty text",
			reply: piAssistant("  "),
			want:  "Summarization produced no text",
		},
		{
			name:  "an aborted answer",
			reply: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonAborted},
			want:  "Summarization failed: aborted",
		},
	} {
		t.Run("fails with model_error on "+tc.name, func(t *testing.T) {
			before := append([]agentcore.Message{}, history...)
			res, err, _ := piRunCompaction(t, history, piSettings(150, 1000), -1, tc.reply)
			if err == nil {
				t.Fatalf("Compact error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Fatalf("Compact error = %q, want %q", err.Error(), tc.want)
			}
			if res != nil {
				t.Fatalf("Compact result = %+v, want nil: a failed summary writes no checkpoint", res)
			}
			// The original context is preserved: compaction never rewrites the
			// messages it could not summarize.
			if len(history) != len(before) {
				t.Fatalf("message count changed: %d -> %d", len(before), len(history))
			}
			for i := range before {
				if history[i].Role() != before[i].Role() {
					t.Fatalf("history[%d] role changed", i)
				}
			}
		})
	}

	t.Run("summary failure preserves the original context", func(t *testing.T) {
		if res, err, _ := piRunCompaction(t, history, piSettings(150, 1000), -1,
			agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, StopReason: agentcore.StopReasonError, ErrorMessage: "overloaded"}); res != nil || err == nil {
			t.Fatalf("Compact = (%v, %v), want (nil, error)", res, err)
		}
		// The loop's contract (US-004): on failure it keeps agentCtx.Messages and
		// only emits the error, so the rebuilt context is never used. This is
		// asserted at the loop level in internal/loop; here the nil result is the
		// proof that no rebuild can happen.
	})

	t.Run("fails when the summarization stream cannot be built", func(t *testing.T) {
		fake := &piFakeStream{t: t, buildErr: errors.New("missing api key")}
		res, err := Compact(context.Background(), fake.streamFn(), piModel(), history, piSettings(150, 1000), -1, nil, "", provider.StreamConfig{})
		if err == nil || res != nil {
			t.Fatalf("Compact = (%v, %v), want (nil, error)", res, err)
		}
		if !strings.Contains(err.Error(), "missing api key") {
			t.Fatalf("error = %q, want it to carry the stream build failure", err)
		}
	})
}

// ─── token estimation (ai/src/utils/estimate.ts) ──────────────────────────

func TestConformanceTokenEstimation(t *testing.T) {
	t.Run("estimates tokens as ceil(chars / 3.5)", func(t *testing.T) {
		// pi: CHARS_PER_TOKEN = 3.5 (estimate.ts:15) and Math.ceil.
		for _, tc := range []struct {
			chars int
			want  int
		}{
			{0, 0},
			{1, 1},
			{3, 1},
			{4, 2},
			{7, 2},
			{8, 3},
			{350, 100},
			{400, 115},
			{12000, 3429},
		} {
			msg := piUser(strings.Repeat("x", tc.chars))
			if got := EstimateTokens(msg); got != tc.want {
				t.Fatalf("EstimateTokens(%d chars) = %d, want %d (pi: Math.ceil(%d/3.5))", tc.chars, got, tc.want, tc.chars)
			}
		}
	})

	t.Run("counts an image block as ESTIMATED_IMAGE_CHARS", func(t *testing.T) {
		msg := agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewImageContent("data", "image/png")},
		}
		if got, want := EstimateTokens(msg), 1372; got != want {
			t.Fatalf("EstimateTokens(image) = %d, want %d (pi: ceil(4800/3.5))", got, want)
		}
	})

	t.Run("counts a tool call by name plus serialized arguments", func(t *testing.T) {
		// pi counts name.length + JSON.stringify(arguments).length
		// (estimate.ts:59-67).
		msg := piAssistant("", "c") // read + {}
		if got, want := EstimateTokens(msg), 2; got != want {
			t.Fatalf("EstimateTokens(tool call) = %d, want %d", got, want)
		}
	})

	t.Run("estimates a checkpoint by its rendered summary text", func(t *testing.T) {
		msg := agentcore.CompactionMessage{RoleField: agentcore.RoleCompaction, Summary: "SUMMARY"}
		want := (len(piSummaryPrefix+"SUMMARY"+piSummarySuffix)*2 + 6) / 7
		if got := EstimateTokens(msg); got != want {
			t.Fatalf("EstimateTokens(checkpoint) = %d, want %d", got, want)
		}
	})

	t.Run("prefers the newest usable usage and estimates only the trailing tail", func(t *testing.T) {
		withUsage := piAssistant(piText("a1", 100))
		withUsage.Usage = &agentcore.Usage{InputTokens: 100, OutputTokens: 20}
		msgs := []agentcore.Message{piUser("hi"), withUsage, piUser(piText("tail", 100))}
		est := EstimateContextTokens(msgs)
		if est.LastUsageIndex != 1 {
			t.Fatalf("LastUsageIndex = %d, want 1", est.LastUsageIndex)
		}
		if est.UsageTokens != 120 {
			t.Fatalf("UsageTokens = %d, want 120", est.UsageTokens)
		}
		if est.TrailingTokens != 115 {
			t.Fatalf("TrailingTokens = %d, want 115", est.TrailingTokens)
		}
		if est.Tokens != 235 {
			t.Fatalf("Tokens = %d, want 235", est.Tokens)
		}
	})

	t.Run("ignores usage measured before a summary placed mid-run", func(t *testing.T) {
		measured := piAssistant(piText("a1", 100))
		measured.Timestamp = 10
		measured.Usage = &agentcore.Usage{InputTokens: 1000, OutputTokens: 0}
		marker := agentcore.CompactionMessage{RoleField: agentcore.RoleCompaction, Summary: "SUMMARY", Timestamp: 20}

		// The newest marker postdates the measured answer, so that usage cannot
		// describe the compacted prefix (estimate.ts:79-92).
		beforeMarker := []agentcore.Message{measured, marker}
		est := EstimateContextTokens(beforeMarker)
		if est.LastUsageIndex != -1 || est.UsageTokens != 0 {
			t.Fatalf("usage before the marker was used: %+v, want all-estimated", est)
		}
		// Usage measured at or before the newest checkpoint is also skipped by
		// pi's estimateContext head rule (compaction.ts:337-352).
		if want := EstimateTokens(measured) + EstimateTokens(marker); est.Tokens != want {
			t.Fatalf("Tokens = %d, want the full estimate %d", est.Tokens, want)
		}

		// A newer assistant after the marker is measured normally.
		fresh := piAssistant(piText("a2", 100))
		fresh.Timestamp = 30
		fresh.Usage = &agentcore.Usage{InputTokens: 500, OutputTokens: 25}
		est = EstimateContextTokens([]agentcore.Message{measured, marker, fresh})
		if est.LastUsageIndex != 2 || est.UsageTokens != 525 || est.Tokens != 525 {
			t.Fatalf("estimate = %+v, want usage 525 from the post-marker answer", est)
		}
	})

	t.Run("never counts an error or aborted answer", func(t *testing.T) {
		msgs := []agentcore.Message{
			piUser("hi"),
			piAssistantWith(piText("err", 100), agentcore.StopReasonError),
			piAssistantWith(piText("stopped", 100), agentcore.StopReasonAborted),
		}
		if got, want := EstimateContextTokens(msgs).Tokens, EstimateTokens(msgs[0]); got != want {
			t.Fatalf("Tokens = %d, want %d: excluded answers add nothing", got, want)
		}
	})
}

// ─── keel extensions with no PI counterpart ───────────────────────────────

func TestConformanceKeelExtensions(t *testing.T) {
	t.Run("appends file operations metadata to the summary", func(t *testing.T) {
		// pigo's file-operation bookkeeping has no counterpart in pi's
		// compaction.ts (which stores the bare summary text). It must not change
		// the summary request, only the stored summary.
		readCall := agentcore.NewToolCallContent("r1", "read", json.RawMessage(`{"path":"a.ts"}`))
		assistant := agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent(piText("a1", 100)), readCall},
			StopReason: agentcore.StopReasonEndTurn,
		}
		msgs := []agentcore.Message{piUser(piText("u1", 100)), assistant, piUser(piText("u2", 100)), piAssistant(piText("a2", 100))}
		res, err, _ := piRunCompaction(t, msgs, piSettings(150, 1000), -1, piAssistant("SUMMARY"))
		if err != nil || res == nil {
			t.Fatalf("Compact = (%v, %v), want a result", res, err)
		}
		if len(res.Details.ReadFiles) != 1 || res.Details.ReadFiles[0] != "a.ts" {
			t.Fatalf("Details.ReadFiles = %v, want [a.ts]", res.Details.ReadFiles)
		}
		wantSuffix := "\n\n<read-files>\na.ts\n</read-files>"
		if !strings.HasSuffix(res.Summary, wantSuffix) {
			t.Fatalf("Summary = %q, want the file metadata suffix %q", res.Summary, wantSuffix)
		}
	})
}
