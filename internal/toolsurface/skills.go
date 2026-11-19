package toolsurface

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/skills"
)

var skillsActions = []string{ActionLs, ActionView, ActionManage, ActionSources}

const skillsToolDescription = "Skill knowledge base. Actions: ls (name/description/path of visible skills); " +
	"view {name} (load the full SKILL.md body on demand). manage and sources are reserved for a later wave."

func skillsSchema() json.RawMessage {
	return schema(`, "name":{"type":"string"}, "path":{"type":"string"}, "source":{"type":"string"}`)
}

func newSkillsTool(discovered []skills.Skill) *actionTool {
	visible := make([]skills.Skill, 0, len(discovered))
	for _, skill := range discovered {
		if !skill.DisableModelInvocation {
			visible = append(visible, skill)
		}
	}
	return &actionTool{
		name:        Skills,
		description: skillsToolDescription,
		schema:      skillsSchema(),
		actions:     skillsActions,
		run: func(_ context.Context, action string, raw json.RawMessage) (agentcore.AgentToolResult, error) {
			switch action {
			case ActionLs:
				return skillsList(visible)
			case ActionView:
				return skillsView(visible, raw)
			case ActionManage, ActionSources:
				return NotImplemented(Skills, action)
			default:
				return unknownAction(Skills, action, skillsActions)
			}
		},
	}
}

func skillsList(discovered []skills.Skill) (agentcore.AgentToolResult, error) {
	items := make([]map[string]string, 0, len(discovered))
	for _, skill := range discovered {
		items = append(items, map[string]string{
			"name":        skill.Name,
			"description": skill.Description,
			"path":        skill.Path,
		})
	}
	document := map[string]any{"count": len(items), "skills": items}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(marshalDocument(document))},
		StructuredContent: document,
	}, nil
}

func skillsView(discovered []skills.Skill, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	var input struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return invalidArguments(Skills, err.Error())
	}
	if input.Name == "" {
		return invalidArguments(Skills, "view requires name")
	}
	for _, skill := range discovered {
		if skill.Name != input.Name {
			continue
		}
		document := map[string]any{
			"name":        skill.Name,
			"description": skill.Description,
			"path":        skill.Path,
			"body":        skill.Body,
		}
		return agentcore.AgentToolResult{
			Content: agentcore.ContentList{agentcore.NewTextContent(
				fmt.Sprintf("Skill %s (%s)\n\n%s", skill.Name, skill.Path, skill.Body),
			)},
			StructuredContent: document,
		}, nil
	}
	return notFound(Skills, fmt.Sprintf("unknown skill %q", input.Name))
}
