package toolsurface_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/toolsurface"
)

func destructiveServer(tools ...mcp.Tool) *fakeMCP {
	return &fakeMCP{
		servers: []*mcp.Server{mcpServer("files", tools...)},
		result:  mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "done"}}},
	}
}

// TestMCPDestructiveToolFailsClosedWithoutApproval pins the default: a
// destructive tool on the model path is never executed without a human
// approval seam.
func TestMCPDestructiveToolFailsClosedWithoutApproval(t *testing.T) {
	backend := destructiveServer(mcp.Tool{
		Name: "delete_file", Annotations: json.RawMessage(`{"destructiveHint":true}`),
	})
	registry := builtinRegistry(t, toolsurface.Backends{MCP: backend})
	result := runAction(t, registry[toolsurface.MCP],
		`{"action":"call","server":"files","tool":"delete_file","arguments":{"path":"/tmp/x"}}`)
	problem := errorDocument(t, result)
	if problem["code"] != toolsurface.CodeMCPApprovalRequired {
		t.Fatalf("problem = %v, want %s", problem, toolsurface.CodeMCPApprovalRequired)
	}
	if backend.calls != 0 {
		t.Fatalf("backend calls = %d, want 0 (no approval)", backend.calls)
	}
}

// TestMCPDestructiveToolRequiresAndHonoursApproval pins the gate: approval is
// asked with the exact call, a refusal is typed and never executes, and an
// approval runs the call once.
func TestMCPDestructiveToolRequiresAndHonoursApproval(t *testing.T) {
	backend := destructiveServer(mcp.Tool{
		Name: "write_file", Annotations: json.RawMessage(`{"destructiveHint":true}`),
	})
	var requests []toolsurface.MCPApprovalRequest
	registry := builtinRegistry(t, toolsurface.Backends{
		MCP: backend,
		MCPApproval: func(_ context.Context, request toolsurface.MCPApprovalRequest) error {
			requests = append(requests, request)
			return nil
		},
	})
	tool := registry[toolsurface.MCP]
	result := runAction(t, tool,
		`{"action":"call","server":"files","tool":"write_file","arguments":{"path":"/tmp/x","text":"hi"}}`)
	if got := agentcore.ContentToText(result.Content); got != "done" {
		t.Fatalf("approved call content = %q", got)
	}
	if backend.calls != 1 || len(requests) != 1 {
		t.Fatalf("calls=%d requests=%d, want 1/1", backend.calls, len(requests))
	}
	if requests[0].Server != "files" || requests[0].Tool != "write_file" || !requests[0].Destructive {
		t.Fatalf("approval request = %+v", requests[0])
	}
	var arguments map[string]string
	if err := json.Unmarshal(requests[0].Arguments, &arguments); err != nil {
		t.Fatal(err)
	}
	if arguments["path"] != "/tmp/x" || arguments["text"] != "hi" {
		t.Fatalf("approval arguments = %+v", arguments)
	}

	// A refusal is typed and the backend stays untouched.
	denied := builtinRegistry(t, toolsurface.Backends{
		MCP: backend,
		MCPApproval: func(context.Context, toolsurface.MCPApprovalRequest) error {
			return errors.New("user said no")
		},
	})
	before := backend.calls
	refused := runAction(t, denied[toolsurface.MCP],
		`{"action":"call","server":"files","tool":"write_file","arguments":{"path":"/tmp/x"}}`)
	problem := errorDocument(t, refused)
	if problem["code"] != toolsurface.CodeMCPApprovalDenied {
		t.Fatalf("problem = %v, want %s", problem, toolsurface.CodeMCPApprovalDenied)
	}
	if backend.calls != before {
		t.Fatalf("denied call executed: calls %d -> %d", before, backend.calls)
	}
}

// TestMCPDestructiveClassification pins the classifier: an explicit annotation
// (true or false) wins over the name-family fallback.
func TestMCPDestructiveClassification(t *testing.T) {
	cases := []struct {
		name string
		tool mcp.Tool
		want bool
	}{
		{"annotation true", mcp.Tool{Name: "read_file", Annotations: json.RawMessage(`{"destructiveHint":true}`)}, true},
		{"annotation false wins over name", mcp.Tool{Name: "delete_file", Annotations: json.RawMessage(`{"destructiveHint":false}`)}, false},
		{"name fallback write", mcp.Tool{Name: "write_file"}, true},
		{"name fallback delete", mcp.Tool{Name: "delete_directory"}, true},
		{"read stays free", mcp.Tool{Name: "read_file"}, false},
		{"list stays free", mcp.Tool{Name: "list_directory"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := destructiveServer(tc.tool)
			approvals := 0
			registry := builtinRegistry(t, toolsurface.Backends{
				MCP: backend,
				MCPApproval: func(context.Context, toolsurface.MCPApprovalRequest) error {
					approvals++
					return nil
				},
			})
			runAction(t, registry[toolsurface.MCP], `{"action":"call","server":"files","tool":"`+tc.tool.Name+`"}`)
			if got := approvals > 0; got != tc.want {
				t.Fatalf("approval asked = %v, want %v", got, tc.want)
			}
			if backend.calls != 1 {
				t.Fatalf("backend calls = %d, want 1", backend.calls)
			}
		})
	}
}
