package toolsurface

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
)

// ErrorResult builds the one error shape every aggregate tool returns: a text
// block plus structured content the UI can render without parsing prose. A tool
// failure is a result the model reads and can react to, never a Go error that
// aborts the batch.
func ErrorResult(code, tool, message string, extra map[string]any) (agentcore.AgentToolResult, error) {
	document := map[string]any{
		"code":    code,
		"tool":    tool,
		"message": strings.TrimSpace(message),
	}
	for key, value := range extra {
		document[key] = value
	}
	encoded, _ := json.Marshal(map[string]any{"error": document})
	isError := true
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(encoded))},
		StructuredContent: map[string]any{"error": document},
		IsError:           &isError,
	}, nil
}

// NotImplemented is the typed refusal for an action whose wave has not landed.
// The tool name and action already exist so the model's surface is stable; the
// backend arrives in a later wave.
func NotImplemented(tool, action string) (agentcore.AgentToolResult, error) {
	label := tool
	if action != "" {
		label = tool + "." + action
	}
	return ErrorResult(CodeNotImplemented, tool,
		fmt.Sprintf("%s is not implemented in this harness slice; the tool and action are reserved", label), nil)
}

func unknownAction(tool, action string, available []string) (agentcore.AgentToolResult, error) {
	return ErrorResult(CodeUnknownAction, tool,
		fmt.Sprintf("unknown action %q", action),
		map[string]any{"availableActions": append([]string(nil), available...)})
}

func invalidArguments(tool, message string) (agentcore.AgentToolResult, error) {
	return ErrorResult(CodeInvalidArguments, tool, message, nil)
}

func notFound(tool, message string) (agentcore.AgentToolResult, error) {
	return ErrorResult(CodeNotFound, tool, message, nil)
}

func notWired(tool, message string) (agentcore.AgentToolResult, error) {
	return ErrorResult(CodeNotWired, tool, message, nil)
}
