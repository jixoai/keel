package agent

// Session-loaded skills (`/skill:<name>`, change keel-plugin-commands).
//
// A `/skill:<name>` command does not send text and does not write a user or
// assistant message. It appends one durable `skill.loaded` event carrying the
// skill's name, content hash and body (the host read them from the installed
// skill library). This file is the runner's half: for the NEXT model turn, the
// loaded skill is a system-prompt section, referenced by name and hash, and
// after that turn it is history like any other event.
//
// "Next turn" is decided from the committed log, not process state: a
// skill.loaded event newer than the newest assistant.message is pending. That
// makes the rule survive a restart and a retry, and it never rewrites the
// session history (the loaded event is a new fact).
//
// A skill loaded through this path applies even when its SKILL.md declares
// `disable-model-invocation`: that flag hides a skill from the model's
// self-service index, while the user's explicit command is authority. The skills
// library does not expose the flag to this path by design.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/store"
)

// MaxLoadedSkillsBytes bounds the system-prompt section of explicitly loaded
// skills. A skill bundle can be large; a turn's injected skill text is capped so
// a user cannot accidentally pin an unbounded prompt.
const MaxLoadedSkillsBytes = 256 << 10

type loadedSkill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Hash        string `json:"hash,omitempty"`
	Body        string `json:"body"`
}

// pendingLoadedSkills returns the skills loaded since the newest committed
// assistant message, last load per name wins, in first-load order.
func pendingLoadedSkills(events []store.Event) []loadedSkill {
	lastAnswer := int64(0)
	for _, event := range events {
		if event.Kind != store.EventAssistantMessage || event.Seq <= lastAnswer {
			continue
		}
		// Only a terminal SUCCESS consumes the pin. A failed or aborted
		// assistant.message is a record of an attempt that did not deliver the
		// turn; the next successful turn must still see the explicitly loaded
		// skill.
		var outcome struct {
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal(event.Payload, &outcome); err == nil {
			if outcome.StopReason == agentcore.StopReasonError || outcome.StopReason == agentcore.StopReasonAborted {
				continue
			}
		}
		lastAnswer = event.Seq
	}
	order := make([]string, 0)
	byName := make(map[string]loadedSkill)
	for _, event := range events {
		if event.Kind != store.EventSkillLoaded || event.Seq <= lastAnswer {
			continue
		}
		var skill loadedSkill
		if err := json.Unmarshal(event.Payload, &skill); err != nil {
			continue
		}
		skill.Name = strings.TrimSpace(skill.Name)
		if skill.Name == "" || strings.TrimSpace(skill.Body) == "" {
			continue
		}
		if _, seen := byName[skill.Name]; !seen {
			order = append(order, skill.Name)
		}
		byName[skill.Name] = skill
	}
	out := make([]loadedSkill, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

// applySkillPreload appends the pending loaded-skill section to a host-assembled
// system prompt. It is the runner's single call site; keeping it a pure
// function lets the next-turn contract be tested without a database.
func applySkillPreload(systemPrompt string, events []store.Event) string {
	section := skillPreloadSection(events)
	if section == "" {
		return systemPrompt
	}
	if strings.TrimSpace(systemPrompt) == "" {
		return section
	}
	return systemPrompt + "\n\n" + section
}

// promptSummaryHash digests the prompt the attempt actually sends. A pending
// skill preload changes the wire prompt even when the binding carries its own
// PromptHash, so it must be part of the digest; taking the binding text alone
// would hide the preload from run.finished readers.
func promptSummaryHash(surface RunSurface, systemPrompt string, preloaded bool) string {
	if !preloaded && surface.PromptHash == "" && strings.TrimSpace(surface.Prompt) != "" {
		// No host renderer contributed sections and no preload: the wire prompt
		// IS the binding text, so its digest is the binding's.
		return digestPrompt(surface.Prompt)
	}
	if strings.TrimSpace(surface.Prompt) != "" || strings.TrimSpace(systemPrompt) != "" {
		return digestPrompt(systemPrompt)
	}
	return ""
}

// skillPreloadSection renders the loaded-skill section of one attempt. It
// returns "" when nothing is pending.
func skillPreloadSection(events []store.Event) string {
	skills := pendingLoadedSkills(events)
	if len(skills) == 0 {
		return ""
	}
	// Stable order independent of event order quirks: the load order is already
	// deterministic (the log order), but sort by name for replay equality across
	// a checkpoint/retry boundary.
	sort.SliceStable(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	var builder strings.Builder
	builder.WriteString("<loaded_skills>\n")
	builder.WriteString("The user explicitly loaded the following skills for this turn. Treat their instructions as active for this turn.\n")
	total := 0
	for _, skill := range skills {
		body := skill.Body
		remaining := MaxLoadedSkillsBytes - total
		if remaining <= 0 {
			break
		}
		if len(body) > remaining {
			body = boundSkillText(body, remaining)
		}
		total += len(body)
		hash := ""
		if skill.Hash != "" {
			hash = fmt.Sprintf(" hash=%q", skill.Hash)
		}
		builder.WriteString(fmt.Sprintf("<skill name=%q%s>\n%s\n</skill>\n", skill.Name, hash, body))
	}
	builder.WriteString("</loaded_skills>")
	return builder.String()
}

// boundSkillText cuts a skill body to max bytes without splitting a UTF-8 rune.
func boundSkillText(body string, max int) string {
	if len(body) <= max {
		return body
	}
	cut := body[:max]
	for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n[skill text truncated]"
}
