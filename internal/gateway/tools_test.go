// Tool-surface tests: the MCP-shaped half of the protocol (tools/list and
// tools/call), its validated results and its bounded page walk.

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestListToolsFollowsPagination(t *testing.T) {
	group := newToolFixture(t)
	pages := 0
	group.provider.setHandler(MethodToolsList, func(params json.RawMessage) (any, *ErrorObject) {
		pages++
		cursor := ""
		if len(params) > 0 {
			var decoded struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(params, &decoded)
			cursor = decoded.Cursor
		}
		entry := func(name string) map[string]any {
			return map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}}
		}
		switch cursor {
		case "":
			return map[string]any{"tools": []any{entry("alpha")}, "nextCursor": "page-2"}, nil
		case "page-2":
			return map[string]any{"tools": []any{entry("beta")}}, nil
		default:
			return nil, &ErrorObject{Code: CodeInvalidParams, Message: "unexpected cursor " + cursor}
		}
	})
	tools, err := group.tool.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "alpha" || tools[1].Name != "beta" {
		t.Fatalf("tools = %+v, want alpha and beta", tools)
	}
	if pages != 2 {
		t.Errorf("pages = %d, want 2", pages)
	}
}

func TestListToolsRejectsRepeatedCursor(t *testing.T) {
	group := newToolFixture(t)
	group.provider.setHandler(MethodToolsList, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"tools":      []any{map[string]any{"name": "loop", "inputSchema": map[string]any{}}},
			"nextCursor": "same",
		}, nil
	})
	if _, err := group.tool.ListTools(context.Background()); err == nil {
		t.Fatal("a provider looping on one cursor was not rejected")
	} else if !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("error = %v, want it to mention the cursor", err)
	}
}

func TestListToolsRejectsInvalidEntries(t *testing.T) {
	group := newToolFixture(t)
	group.provider.setHandler(MethodToolsList, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"tools": []any{map[string]any{"name": "no-schema"}},
		}, nil
	})
	_, err := group.tool.ListTools(context.Background())
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("error = %v, want *ProtocolError", err)
	}
}

func TestCallToolValidatesResult(t *testing.T) {
	group := newToolFixture(t)

	if _, err := group.tool.CallTool(context.Background(), "", nil); err == nil {
		t.Error("CallTool with an empty tool name was accepted")
	}

	group.provider.setHandler(MethodToolsCall, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"content": nil}, nil
	})
	if _, err := group.tool.CallTool(context.Background(), "echo", nil); err == nil {
		t.Error("a null content array was accepted")
	}

	group.provider.setHandler(MethodToolsCall, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"content":           []any{map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"}},
			"structuredContent": []any{1, 2, 3},
			"isError":           true,
		}, nil
	})
	result, err := group.tool.CallTool(context.Background(), "echo", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "image" || result.Content[0].Data != "AAAA" {
		t.Errorf("content = %+v, want the image block", result.Content)
	}
	if string(result.StructuredContent) != "[1,2,3]" {
		t.Errorf("structuredContent = %s, want the array", result.StructuredContent)
	}
	if !result.IsError {
		t.Error("isError = false, want the provider's flag")
	}
}

func TestToolProviderErrorsMapToRPCError(t *testing.T) {
	group := newToolFixture(t)
	group.provider.setHandler(MethodToolsList, func(json.RawMessage) (any, *ErrorObject) {
		return nil, &ErrorObject{Code: CodeInvalidParams, Message: "bad request"}
	})
	_, err := group.tool.ListTools(context.Background())
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want *RPCError", err)
	}
	if rpcErr.Code != CodeInvalidParams || !rpcErr.IsCode(CodeInvalidParams) {
		t.Errorf("rpc error = %+v, want code %d", rpcErr, CodeInvalidParams)
	}
}
