package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/store"
)

func skillLoadedEvent(seq int64, name, hash, body string) store.Event {
	payload, _ := json.Marshal(map[string]any{
		"name": name, "hash": hash, "body": body,
	})
	return store.Event{Seq: seq, Kind: store.EventSkillLoaded, Payload: payload}
}

func TestSkillPreloadSectionAppliesToNextTurnOnly(t *testing.T) {
	events := []store.Event{
		{Seq: 1, Kind: store.EventUserMessage, Payload: json.RawMessage(`{"text":"hi"}`)},
		{Seq: 2, Kind: store.EventAssistantMessage, Payload: json.RawMessage(`{}`)},
		skillLoadedEvent(3, "release-checklist", "sha256:abc", "1. Tag\n2. Ship\n"),
	}
	section := skillPreloadSection(events)
	if section == "" {
		t.Fatal("a skill loaded after the last answer did not reach the next turn")
	}
	for _, want := range []string{"<loaded_skills>", `name="release-checklist"`, "sha256:abc", "1. Tag", "2. Ship"} {
		if !strings.Contains(section, want) {
			t.Fatalf("section lacks %q: %s", want, section)
		}
	}

	// Once the next assistant answer is committed, the pin is history and no
	// longer part of the prompt.
	events = append(events, store.Event{Seq: 4, Kind: store.EventAssistantMessage, Payload: json.RawMessage(`{}`)})
	if got := skillPreloadSection(events); got != "" {
		t.Fatalf("a consumed skill pin still applies: %q", got)
	}
}

func TestSkillPreloadSectionLatestLoadPerNameWins(t *testing.T) {
	events := []store.Event{
		skillLoadedEvent(1, "beta", "", "beta old"),
		skillLoadedEvent(2, "alpha", "", "alpha"),
		skillLoadedEvent(3, "beta", "", "beta new"),
	}
	section := skillPreloadSection(events)
	if strings.Contains(section, "beta old") || !strings.Contains(section, "beta new") {
		t.Fatalf("latest load did not win: %s", section)
	}
	if strings.Index(section, "alpha") > strings.Index(section, "beta") {
		t.Fatalf("skills are not ordered deterministically: %s", section)
	}
}

func TestSkillPreloadSectionIgnoresMalformedAndEmpty(t *testing.T) {
	events := []store.Event{
		{Seq: 1, Kind: store.EventSkillLoaded, Payload: json.RawMessage(`not json`)},
		skillLoadedEvent(2, "", "", "no name"),
		skillLoadedEvent(3, "empty", "", "   "),
	}
	if got := skillPreloadSection(events); got != "" {
		t.Fatalf("malformed/empty skill events rendered: %q", got)
	}
}

func TestSkillPreloadSectionBoundsContent(t *testing.T) {
	body := strings.Repeat("x", MaxLoadedSkillsBytes+1024)
	section := skillPreloadSection([]store.Event{skillLoadedEvent(1, "big", "", body)})
	if len(section) > MaxLoadedSkillsBytes+4096 {
		t.Fatalf("section is not bounded: %d bytes", len(section))
	}
	if !strings.Contains(section, "[skill text truncated]") {
		t.Fatal("bounded skill is not marked truncated")
	}
}

func TestApplySkillPreloadKeepsHostPrompt(t *testing.T) {
	events := []store.Event{skillLoadedEvent(1, "checklist", "", "do it")}
	prompt := applySkillPreload("HOST PROMPT", events)
	if !strings.HasPrefix(prompt, "HOST PROMPT\n\n<loaded_skills>") {
		t.Fatalf("host prompt was not preserved: %q", prompt)
	}
	if got := applySkillPreload("HOST PROMPT", nil); got != "HOST PROMPT" {
		t.Fatalf("no pending skill changed the prompt: %q", got)
	}
	if got := applySkillPreload("", events); !strings.HasPrefix(got, "<loaded_skills>") {
		t.Fatalf("empty host prompt did not receive the section: %q", got)
	}
}

// TestCommandFactsStayOutOfTheModelContext pins the projection boundary: a
// command.invoked line is a UI/audit row, and a skill.loaded event is consumed
// by the system prompt — neither may become a user/assistant message.
func TestCommandFactsStayOutOfTheModelContext(t *testing.T) {
	events := []store.Event{
		{Seq: 1, Kind: store.EventCommandInvoked, Payload: json.RawMessage(`{"command":"compact","status":"ok"}`)},
		skillLoadedEvent(2, "alpha", "", "body"),
	}
	messages, _ := DecodeMessages(events, nil)
	if len(messages) != 0 {
		t.Fatalf("command facts entered the model transcript: %+v", messages)
	}
}
