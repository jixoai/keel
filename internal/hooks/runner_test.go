package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// PI: "Handlers run in extension load order, then registration order."
func TestHandlersRunInRegistrationOrder(t *testing.T) {
	reg, _ := newTestRegistry(t)
	var log eventLog
	reg.On(EventAgentStart, func(context.Context, Event) (Decision, error) {
		log.add("A")
		return Allow(), nil
	}, WithName("A"))
	reg.On(EventAgentStart, func(context.Context, Event) (Decision, error) {
		log.add("B")
		return Allow(), nil
	}, WithName("B"))

	reg.Emit(context.Background(), &AgentStartEvent{})
	assertStrings(t, log.snapshot(), []string{"A", "B"})
}

// PI lifecycle: agent_start -> turn_start -> message_* -> tool_execution_* ->
// turn_end -> agent_end -> agent_before_settle -> agent_settled.
func TestLifecycleEventNamesAndOrder(t *testing.T) {
	reg, _ := newTestRegistry(t)
	var log eventLog
	events := []Event{
		&AgentStartEvent{},
		&TurnStartEvent{TurnIndex: 0},
		&MessageStartEvent{Message: textMessage(agentcore.RoleUser, "hi")},
		&MessageUpdateEvent{Message: textMessage(agentcore.RoleAssistant, "h")},
		&MessageEndEvent{Message: textMessage(agentcore.RoleAssistant, "hi")},
		&ToolExecutionStartEvent{ToolCallID: "1", ToolName: "bash"},
		&ToolExecutionUpdateEvent{ToolCallID: "1", ToolName: "bash"},
		&ToolExecutionEndEvent{ToolCallID: "1", ToolName: "bash"},
		&TurnEndEvent{},
		&AgentEndEvent{},
		&AgentBeforeSettleEvent{},
		&AgentSettledEvent{},
	}
	for _, event := range events {
		reg.On(event.EventType(), func(_ context.Context, event Event) (Decision, error) {
			log.add(event.EventType())
			return Allow(), nil
		})
	}
	for _, event := range events {
		reg.Emit(context.Background(), event)
	}
	assertStrings(t, log.snapshot(), []string{
		EventAgentStart,
		EventTurnStart,
		EventMessageStart,
		EventMessageUpdate,
		EventMessageEnd,
		EventToolExecutionStart,
		EventToolExecutionUpdate,
		EventToolExecutionEnd,
		EventTurnEnd,
		EventAgentEnd,
		EventAgentBeforeSettle,
		EventAgentSettled,
	})
}

// PI: "Allows self-removal without skipping neighboring handlers."
func TestAllowsSelfRemovalWithoutSkippingNeighbors(t *testing.T) {
	reg, _ := newTestRegistry(t)
	var log eventLog
	var unsubscribe func()
	unsubscribe = reg.On(EventAgentEnd, func(context.Context, Event) (Decision, error) {
		log.add("A")
		unsubscribe()
		return Allow(), nil
	})
	reg.On(EventAgentEnd, func(context.Context, Event) (Decision, error) {
		log.add("B")
		return Allow(), nil
	})

	reg.Emit(context.Background(), &AgentEndEvent{})
	assertStrings(t, log.snapshot(), []string{"A", "B"})
	reg.Emit(context.Background(), &AgentEndEvent{})
	assertStrings(t, log.snapshot(), []string{"A", "B", "B"})
}

// PI: "Keeps removed pending handlers in the current dispatch."
func TestKeepsRemovedPendingHandlersInCurrentDispatch(t *testing.T) {
	reg, _ := newTestRegistry(t)
	var log eventLog
	var stopB func()
	reg.On(EventAgentEnd, func(context.Context, Event) (Decision, error) {
		log.add("A")
		stopB()
		return Allow(), nil
	})
	stopB = reg.On(EventAgentEnd, func(context.Context, Event) (Decision, error) {
		log.add("B")
		return Allow(), nil
	})
	reg.On(EventAgentEnd, func(context.Context, Event) (Decision, error) {
		log.add("C")
		return Allow(), nil
	})

	reg.Emit(context.Background(), &AgentEndEvent{})
	assertStrings(t, log.snapshot(), []string{"A", "B", "C"})
	reg.Emit(context.Background(), &AgentEndEvent{})
	assertStrings(t, log.snapshot(), []string{"A", "B", "C", "A", "C"})
}

// PI: "Defers registrations made during dispatch until the next dispatch."
func TestDefersRegistrationsMadeDuringDispatch(t *testing.T) {
	reg, _ := newTestRegistry(t)
	var log eventLog
	reg.On(EventAgentEnd, func(context.Context, Event) (Decision, error) {
		log.add("A")
		reg.On(EventAgentEnd, func(context.Context, Event) (Decision, error) {
			log.add("C")
			return Allow(), nil
		})
		return Allow(), nil
	})
	reg.On(EventAgentEnd, func(context.Context, Event) (Decision, error) {
		log.add("B")
		return Allow(), nil
	})

	reg.Emit(context.Background(), &AgentEndEvent{})
	assertStrings(t, log.snapshot(), []string{"A", "B"})
	reg.Emit(context.Background(), &AgentEndEvent{})
	assertStrings(t, log.snapshot(), []string{"A", "B", "A", "B", "C"})
}

// PI: "Uses a fresh handler list for nested dispatches."
func TestNestedDispatchUsesFreshHandlerList(t *testing.T) {
	reg, _ := newTestRegistry(t)
	var log eventLog
	var stopA, stopB func()
	stopA = reg.On(EventAgentEnd, func(ctx context.Context, _ Event) (Decision, error) {
		log.add("A")
		stopA()
		stopB()
		reg.On(EventAgentEnd, func(context.Context, Event) (Decision, error) {
			log.add("C")
			return Allow(), nil
		})
		reg.Emit(ctx, &AgentEndEvent{})
		return Allow(), nil
	})
	stopB = reg.On(EventAgentEnd, func(context.Context, Event) (Decision, error) {
		log.add("B")
		return Allow(), nil
	})

	reg.Emit(context.Background(), &AgentEndEvent{})
	assertStrings(t, log.snapshot(), []string{"A", "C", "B"})
}

// PI: tool_call can block; the first block wins and stops later handlers.
func TestToolCallDenyStopsDispatch(t *testing.T) {
	reg, rec := newTestRegistry(t)
	secondRan := false
	reg.On(EventToolCall, func(context.Context, Event) (Decision, error) {
		return Deny("dangerous command"), nil
	}, WithName("guard"))
	reg.On(EventToolCall, func(context.Context, Event) (Decision, error) {
		secondRan = true
		return Allow(), nil
	})

	event := &ToolCallEvent{ToolCallID: "1", ToolName: "bash", Input: json.RawMessage(`{"command":"rm -rf /"}`)}
	decision := reg.EmitToolCall(context.Background(), event)
	if decision.Action != ActionDeny || decision.Reason != "dangerous command" {
		t.Fatalf("decision = %+v", decision)
	}
	if secondRan {
		t.Fatal("later handler ran after a deny")
	}
	audits := rec.auditRecords()
	if len(audits) != 1 || audits[0].Decision != ActionDeny || audits[0].Hook != "guard" {
		t.Fatalf("audits = %+v", audits)
	}
	if audits[0].Duration < 0 {
		t.Fatalf("duration = %v", audits[0].Duration)
	}
}

// PI: tool_call input is mutable; later handlers see earlier mutations.
func TestToolCallModifyChains(t *testing.T) {
	reg, _ := newTestRegistry(t)
	var seen []string
	reg.On(EventToolCall, func(_ context.Context, event Event) (Decision, error) {
		call := event.(*ToolCallEvent)
		seen = append(seen, string(call.Input))
		return ModifyInput(json.RawMessage(`{"x":"first"}`)), nil
	})
	reg.On(EventToolCall, func(_ context.Context, event Event) (Decision, error) {
		call := event.(*ToolCallEvent)
		seen = append(seen, string(call.Input))
		return ModifyInput(json.RawMessage(`{"x":"second"}`)), nil
	})

	event := &ToolCallEvent{ToolCallID: "1", ToolName: "t", Input: json.RawMessage(`{"x":"base"}`)}
	decision := reg.EmitToolCall(context.Background(), event)
	if decision.Action != ActionModify || string(decision.Input) != `{"x":"second"}` {
		t.Fatalf("decision = %+v", decision)
	}
	if string(event.Input) != `{"x":"second"}` {
		t.Fatalf("event input = %s", event.Input)
	}
	assertStrings(t, seen, []string{`{"x":"base"}`, `{"x":"first"}`})
}

// PI: "tool_call errors block the tool (fail-safe)".
func TestToolCallHandlerErrorFailsClosed(t *testing.T) {
	reg, rec := newTestRegistry(t)
	handlerErr := errors.New("guard unavailable")
	reg.On(EventToolCall, func(context.Context, Event) (Decision, error) {
		return Allow(), handlerErr
	}, WithName("guard"))

	decision := reg.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "bash", Input: json.RawMessage(`{}`)})
	if decision.Action != ActionDeny {
		t.Fatalf("decision = %+v", decision)
	}
	if !strings.Contains(decision.Reason, "guard unavailable") {
		t.Fatalf("reason = %q", decision.Reason)
	}
	errs := rec.errors()
	if len(errs) != 1 || errs[0].Event != EventToolCall || !errors.Is(errs[0].Err, handlerErr) {
		t.Fatalf("errors = %+v", errs)
	}
	audits := rec.auditRecords()
	if len(audits) != 1 || audits[0].Decision != ActionDeny || !strings.Contains(audits[0].Error, "guard unavailable") {
		t.Fatalf("audits = %+v", audits)
	}
}

// PI: tool_result handlers chain partial patches and preserve omitted fields.
func TestToolResultChaining(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventToolResult, func(_ context.Context, event Event) (Decision, error) {
		result := event.(*ToolResultEvent)
		content := append(agentcore.ContentList{}, result.Content...)
		content = append(content, agentcore.NewTextContent("ext1"))
		return ModifyResult(ToolResultPatch{
			Content:    &content,
			Details:    map[string]any{"source": "ext1"},
			DetailsSet: true,
		}), nil
	})
	reg.On(EventToolResult, func(_ context.Context, event Event) (Decision, error) {
		onlyError := true
		return ModifyResult(ToolResultPatch{IsError: &onlyError}), nil
	})

	patched, modified := reg.EmitToolResult(context.Background(), &ToolResultEvent{
		ToolName:   "t",
		ToolCallID: "1",
		Content:    agentcore.ContentList{agentcore.NewTextContent("base")},
		Details:    map[string]any{"initial": true},
		DetailsSet: true,
	})
	if !modified {
		t.Fatal("result was not marked modified")
	}
	if len(patched.Content) != 2 || agentcore.ContentToText(patched.Content) != "baseext1" {
		t.Fatalf("content = %+v", patched.Content)
	}
	details, ok := patched.Details.(map[string]any)
	if !ok || details["source"] != "ext1" {
		t.Fatalf("details = %+v", patched.Details)
	}
	if !patched.IsError {
		t.Fatal("isError patch not applied")
	}
}

// PI: message_end handlers replace the finalized message; role must match.
func TestMessageEndReplacementAndRoleCheck(t *testing.T) {
	t.Run("replaces with same role", func(t *testing.T) {
		reg, _ := newTestRegistry(t)
		reg.On(EventMessageEnd, func(_ context.Context, event Event) (Decision, error) {
			return ReplaceMessage(textMessage(agentcore.RoleAssistant, "replaced")), nil
		})
		final, modified := reg.EmitMessageEnd(context.Background(), MessageEndEvent{
			Message: textMessage(agentcore.RoleAssistant, "original"),
		})
		if !modified || messageText(final) != "replaced" {
			t.Fatalf("final = %q modified=%v", messageText(final), modified)
		}
	})

	t.Run("rejects mismatched role and reports", func(t *testing.T) {
		reg, rec := newTestRegistry(t)
		reg.On(EventMessageEnd, func(context.Context, Event) (Decision, error) {
			return ReplaceMessage(textMessage(agentcore.RoleUser, "wrong role")), nil
		})
		final, modified := reg.EmitMessageEnd(context.Background(), MessageEndEvent{
			Message: textMessage(agentcore.RoleAssistant, "original"),
		})
		if modified || messageText(final) != "original" {
			t.Fatalf("final = %q modified=%v", messageText(final), modified)
		}
		if len(rec.errors()) != 1 {
			t.Fatalf("errors = %+v", rec.errors())
		}
	})
}

// PI: context handlers can filter and chain message lists; the host input is
// not mutated.
func TestContextRewriteChainsAndDoesNotMutateInput(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventContext, func(_ context.Context, event Event) (Decision, error) {
		contextEvent := event.(*ContextEvent)
		var filtered []agentcore.Message
		for _, message := range contextEvent.Messages {
			if messageText(message) != "drop" {
				filtered = append(filtered, message)
			}
		}
		return ReplaceMessages(filtered), nil
	})
	reg.On(EventContext, func(_ context.Context, event Event) (Decision, error) {
		contextEvent := event.(*ContextEvent)
		messages := append(agentcore.MessageList{}, contextEvent.Messages...)
		messages = append(messages, textMessage(agentcore.RoleUser, "added"))
		return ReplaceMessages(messages), nil
	})

	input := []agentcore.Message{
		textMessage(agentcore.RoleUser, "keep"),
		textMessage(agentcore.RoleUser, "drop"),
	}
	output := reg.EmitContext(context.Background(), input)
	if len(output) != 2 || messageText(output[0]) != "keep" || messageText(output[1]) != "added" {
		t.Fatalf("output = %+v", output)
	}
	if len(input) != 2 {
		t.Fatalf("host input mutated: %+v", input)
	}
}

func TestContextInPlaceMutationIsHonored(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventContext, func(_ context.Context, event Event) (Decision, error) {
		contextEvent := event.(*ContextEvent)
		contextEvent.Messages = append(contextEvent.Messages, textMessage(agentcore.RoleUser, "in-place"))
		return Allow(), nil
	})
	output := reg.EmitContext(context.Background(), []agentcore.Message{textMessage(agentcore.RoleUser, "base")})
	if len(output) != 2 || messageText(output[1]) != "in-place" {
		t.Fatalf("output = %+v", output)
	}
}

// PI: before_agent_start chains the system prompt and injects guidance.
func TestBeforeAgentStartChainsSystemPromptAndGuidance(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventBeforeAgentStart, func(_ context.Context, event Event) (Decision, error) {
		start := event.(*BeforeAgentStartEvent)
		return Decision{
			Action:       ActionModify,
			SystemPrompt: stringPtr(start.SystemPrompt + "\nfirst"),
			Guidance:     []GuidanceMessage{{CustomType: "first", Content: "first guidance", Display: false}},
		}, nil
	})
	var secondSaw string
	reg.On(EventBeforeAgentStart, func(_ context.Context, event Event) (Decision, error) {
		start := event.(*BeforeAgentStartEvent)
		secondSaw = start.SystemPrompt
		return Decision{
			Action:       ActionModify,
			SystemPrompt: stringPtr(start.SystemPrompt + "\nsecond"),
			Guidance:     []GuidanceMessage{{CustomType: "second", Content: "second guidance"}},
		}, nil
	})

	result := reg.EmitBeforeAgentStart(context.Background(), &BeforeAgentStartEvent{
		MessageType:  MessageTypeUser,
		Prompt:       "hello",
		SystemPrompt: "base",
	})
	if result.SystemPrompt != "base\nfirst\nsecond" {
		t.Fatalf("system prompt = %q", result.SystemPrompt)
	}
	if secondSaw != "base\nfirst" {
		t.Fatalf("second handler saw %q", secondSaw)
	}
	if len(result.Guidance) != 2 || result.Guidance[0].CustomType != "first" || result.Guidance[1].CustomType != "second" {
		t.Fatalf("guidance = %+v", result.Guidance)
	}
}

// PI: observer failures are logged and never affect the run; observer decisions
// are ignored.
func TestObserverFailureDoesNotBlock(t *testing.T) {
	reg, rec := newTestRegistry(t)
	observerErr := errors.New("metrics backend down")
	reg.On(EventAgentEnd, func(context.Context, Event) (Decision, error) {
		return Deny("observer must not deny"), observerErr
	}, WithName("metrics"), AsObserver())
	reg.On(EventToolCall, func(context.Context, Event) (Decision, error) {
		return Deny("observer must not deny"), observerErr
	}, WithName("metrics-tool"), AsObserver())

	reg.Emit(context.Background(), &AgentEndEvent{})
	decision := reg.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "t", Input: json.RawMessage(`{}`)})
	if decision.Action.Normalized() != ActionAllow {
		t.Fatalf("observer denial was applied: %+v", decision)
	}
	errs := rec.errors()
	if len(errs) != 2 {
		t.Fatalf("errors = %+v", errs)
	}
	audits := rec.auditRecords()
	if len(audits) != 2 {
		t.Fatalf("audits = %+v", audits)
	}
	for _, audit := range audits {
		if !audit.Observer || audit.Error == "" || audit.Decision != ActionAllow {
			t.Fatalf("audit = %+v", audit)
		}
	}
}

func TestAuditRecordsEveryHookExecution(t *testing.T) {
	reg, rec := newTestRegistry(t)
	reg.On(EventAgentStart, func(context.Context, Event) (Decision, error) {
		return Allow(), nil
	}, WithName("start-hook"))
	reg.On(EventToolCall, func(context.Context, Event) (Decision, error) {
		return ModifyInput(json.RawMessage(`{"x":1}`)), nil
	}, WithName("input-hook"))

	reg.Emit(context.Background(), &AgentStartEvent{})
	reg.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "t", Input: json.RawMessage(`{}`)})

	audits := rec.auditRecords()
	if len(audits) != 2 {
		t.Fatalf("audits = %+v", audits)
	}
	if audits[0].Hook != "start-hook" || audits[0].Event != EventAgentStart || audits[0].Decision != ActionAllow {
		t.Fatalf("audit[0] = %+v", audits[0])
	}
	if audits[1].Hook != "input-hook" || audits[1].Event != EventToolCall || audits[1].Decision != ActionModify {
		t.Fatalf("audit[1] = %+v", audits[1])
	}
	if audits[0].Timestamp.IsZero() || audits[0].Duration < 0 {
		t.Fatalf("audit[0] = %+v", audits[0])
	}
}

func TestSessionBeforeCompactCancelWinsAndCustomResult(t *testing.T) {
	t.Run("custom compaction result", func(t *testing.T) {
		reg, _ := newTestRegistry(t)
		summary := "custom summary"
		reg.On(EventSessionBeforeCompact, func(context.Context, Event) (Decision, error) {
			return CustomCompaction(CompactionResult{Summary: summary, FirstKeptEntryID: "e1"}), nil
		})
		result := reg.EmitSessionBeforeCompact(context.Background(), &SessionBeforeCompactEvent{Reason: CompactReasonManual})
		if result.Cancel || result.Compaction == nil || result.Compaction.Summary != summary {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("first cancel wins", func(t *testing.T) {
		reg, _ := newTestRegistry(t)
		thirdRan := false
		reg.On(EventSessionBeforeCompact, func(context.Context, Event) (Decision, error) {
			return CustomCompaction(CompactionResult{Summary: "earlier"}), nil
		})
		reg.On(EventSessionBeforeCompact, func(context.Context, Event) (Decision, error) {
			return Deny("user cancelled"), nil
		}, WithName("cancel"))
		reg.On(EventSessionBeforeCompact, func(context.Context, Event) (Decision, error) {
			thirdRan = true
			return Allow(), nil
		})
		result := reg.EmitSessionBeforeCompact(context.Background(), &SessionBeforeCompactEvent{Reason: CompactReasonManual})
		if !result.Cancel {
			t.Fatalf("result = %+v", result)
		}
		if result.Compaction != nil {
			t.Fatalf("PI replaces the result on cancel, got compaction %+v", result.Compaction)
		}
		if thirdRan {
			t.Fatal("dispatch continued after cancel")
		}
	})
}

func TestHasHandlers(t *testing.T) {
	reg := NewRegistry()
	if reg.HasHandlers(EventToolCall) {
		t.Fatal("empty registry reported handlers")
	}
	reg.On(EventToolCall, func(context.Context, Event) (Decision, error) { return Allow(), nil })
	if !reg.HasHandlers(EventToolCall) {
		t.Fatal("registered handler not reported")
	}
	if reg.HasHandlers(EventAgentEnd) {
		t.Fatal("unrelated event reported handlers")
	}
}

func TestOnRejectsUnknownEvent(t *testing.T) {
	reg := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for unknown event")
		}
	}()
	reg.On("not_a_pi_event", func(context.Context, Event) (Decision, error) { return Allow(), nil })
}

func TestRegistryIsSafeForConcurrentDispatch(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventToolResult, func(_ context.Context, event Event) (Decision, error) {
		result := event.(*ToolResultEvent)
		content := append(agentcore.ContentList{}, result.Content...)
		content = append(content, agentcore.NewTextContent("extra"))
		return ModifyResult(ToolResultPatch{Content: &content}), nil
	})

	const workers = 8
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			reg.EmitToolResult(context.Background(), &ToolResultEvent{
				ToolCallID: string(rune('a' + id)),
				Content:    agentcore.ContentList{agentcore.NewTextContent("base")},
			})
			done <- struct{}{}
		}(i)
	}
	timeout := time.After(5 * time.Second)
	for i := 0; i < workers; i++ {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("concurrent dispatch timed out")
		}
	}
}

func stringPtr(value string) *string { return &value }
