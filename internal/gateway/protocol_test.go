// Wire-layer tests: classification parity with internal/mcp (the transport and
// lifecycle this protocol reuses) and JSON-RPC identifier semantics.

package gateway

import (
	"encoding/json"
	"testing"

	"github.com/jixoai/keel/internal/mcp"
)

func messageKind(message Message) string {
	switch message.(type) {
	case *Request:
		return "request"
	case *Notification:
		return "notification"
	case *SuccessResponse:
		return "success"
	case *ErrorResponse:
		return "error"
	default:
		return "unknown"
	}
}

func mcpMessageKind(message mcp.Message) string {
	switch message.(type) {
	case *mcp.Request:
		return "request"
	case *mcp.Notification:
		return "notification"
	case *mcp.SuccessResponse:
		return "success"
	case *mcp.ErrorResponse:
		return "error"
	default:
		return "unknown"
	}
}

// TestParseMessageClassifiesLikeMCP pins the shared wire contract: the gateway
// and the MCP client must accept and reject the same bytes, so a message that
// works on one transport works on the other.
func TestParseMessageClassifiesLikeMCP(t *testing.T) {
	corpus := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-11-07"}}`,
		`{"jsonrpc":"2.0","id":"abc","method":"tools/call"}`,
		`{"jsonrpc":"2.0","id":1.5,"method":"tools/call","params":null}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`{"jsonrpc":"2.0","method":"event.stream"}`,
		`{"jsonrpc":"2.0","id":2,"result":null}`,
		`{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`,
		`{"jsonrpc":"2.0","id":4,"error":{"code":-32601,"message":"nope"}}`,
		`{"jsonrpc":"2.0","id":5,"error":{"code":-32601,"message":"nope","data":{"x":1}}}`,
		// Rejections.
		`{"jsonrpc":"1.0","id":1,"method":"x"}`,
		`{"jsonrpc":"2.0","id":null,"method":"x"}`,
		`{"jsonrpc":"2.0","id":6}`,
		`{"jsonrpc":"2.0","id":7,"error":{"code":"-1","message":"x"}}`,
		`{"jsonrpc":"2.0","id":8,"error":{"message":"no code"}}`,
		`[]`,
		`null`,
		`{"id":9,"method":"x"}`,
	}
	for _, data := range corpus {
		message, err := ParseMessage([]byte(data))
		mcpMessage, mcpErr := mcp.ParseMessage([]byte(data))
		if (err == nil) != (mcpErr == nil) {
			t.Errorf("%s: gateway err = %v, mcp err = %v", data, err, mcpErr)
			continue
		}
		if err != nil {
			continue
		}
		if messageKind(message) != mcpMessageKind(mcpMessage) {
			t.Errorf("%s: gateway says %s, mcp says %s", data, messageKind(message), mcpMessageKind(mcpMessage))
		}
	}
}

func TestIDKeyTreatsNumericallyEqualIDsAsOne(t *testing.T) {
	var integral, decimal ID
	if err := json.Unmarshal([]byte("1"), &integral); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte("1.0"), &decimal); err != nil {
		t.Fatal(err)
	}
	if integral.Key() != decimal.Key() {
		t.Errorf("1 and 1.0 must correlate as one id: %q vs %q", integral.Key(), decimal.Key())
	}
	if StringID("1").Key() == NumberID(1).Key() {
		t.Error("the string id \"1\" must not collide with the number 1")
	}
}

func TestIDRejectsNullAndZero(t *testing.T) {
	var id ID
	if err := json.Unmarshal([]byte("null"), &id); err == nil {
		t.Error("a null id must not parse")
	}
	var zero ID
	if _, err := zero.MarshalJSON(); err == nil {
		t.Error("the zero id must not marshal")
	}
}

func TestParseMessageCopiesPayloadBytes(t *testing.T) {
	data := []byte(`{"jsonrpc":"2.0","id":1,"result":{"text":"stable"}}`)
	message, err := ParseMessage(data)
	if err != nil {
		t.Fatal(err)
	}
	response := message.(*SuccessResponse)
	for index := range data {
		data[index] = 'x'
	}
	if string(response.Result) != `{"text":"stable"}` {
		t.Errorf("result changed with the source buffer: %s", response.Result)
	}
}
