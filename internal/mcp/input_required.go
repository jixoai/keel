// MRTR wire types (change feat-mcp-apps-events, P5.1).
//
// 2026-07-28 replaces server-initiated requests with Multi-Round Tool Requests
// (MRTR): a `tools/call` (or `prompts/get` / `resources/read`) may answer with
// `resultType:"input_required"` and a map of input requests. The client executes
// them, retries the original request under a NEW JSON-RPC id with
// `inputResponses`, and echoes `requestState` byte-for-byte.
//
// Parsing rules that are protocol facts, not choices:
//
//   - a missing `resultType` is `"complete"` (old servers must stay readable);
//   - `"complete"` is a final result;
//   - `"input_required"` carries at least one of `inputRequests` / `requestState`;
//   - any other value is a protocol error (the call fails and is audited).
//
// `inputResponses` values are the result object itself (ElicitResult,
// CreateMessageResult, ListRootsResult). The schema has no per-key error
// variant: a refused request (roots, an unknown method, a disabled sampling
// capability) fails the whole round with a JSON-RPC error, never by silently
// omitting the key.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// ResultType values of the 2026-07-28 result envelope.
const (
	ResultTypeComplete      = "complete"
	ResultTypeInputRequired = "input_required"
)

// InputRequest is one entry of an InputRequiredResult. It is a method plus
// params, without a JSON-RPC id (the exchange is owned by the original call).
type InputRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// InputRequiredResult is a parsed `resultType:"input_required"` result.
type InputRequiredResult struct {
	// InputRequests maps a server-assigned key to the request to execute.
	InputRequests map[string]InputRequest
	// RequestState is the opaque value the server sent. It is kept as raw bytes
	// and echoed verbatim; the client must not parse, rewrite or reuse it.
	RequestState json.RawMessage
	// HasRequestState distinguishes "absent" from an explicit JSON value.
	HasRequestState bool
}

// InputRequestState is the per-call MRTR bookkeeping the client owns. It is
// transient: nothing here is persisted or written to the event ledger.
type InputRequestState struct {
	// CallID is the client's opaque identity of this tool call. It makes an
	// elicitation decision deterministic across retries of the same call while
	// keeping two parallel calls apart (the server's keys are per call).
	CallID string
	// ToolCallID is the progress token / tool call id of the call, when known.
	ToolCallID string
	// Round counts the input_required rounds processed so far (1-based).
	Round int
	// SamplingCalls counts the sampling/createMessage requests the server asked
	// for during this tool call (the per-call budget).
	SamplingCalls int
}

// InputRequestHandler executes one round of input requests. server is the
// configured server name; request carries the parsed round; state is the
// per-call bookkeeping. Returning an error fails the whole call (a refused
// request has no per-key error representation on the wire).
type InputRequestHandler func(ctx context.Context, server string, request InputRequiredResult, state *InputRequestState) (map[string]json.RawMessage, error)

// ClientInputRequestHandler is the per-connection form of InputRequestHandler
// (the configured server name is already bound).
type ClientInputRequestHandler func(ctx context.Context, request InputRequiredResult, state *InputRequestState) (map[string]json.RawMessage, error)

// MRTRBudgetExceededError reports a server that kept asking for input past the
// per-call round budget. The call fails with this error and an audit row.
type MRTRBudgetExceededError struct {
	Rounds int
}

func (e *MRTRBudgetExceededError) Error() string {
	return fmt.Sprintf("the server requested more than %d rounds of input", e.Rounds)
}

// MRTRUnsupportedError reports an input_required result when the host has no
// executor wired (no decision/sampling capability). It is a protocol error:
// the server asked for a capability the client did not declare.
type MRTRUnsupportedError struct {
	Method string
}

func (e *MRTRUnsupportedError) Error() string {
	if e.Method == "" {
		return "the server requires input but this host has no MRTR executor"
	}
	return "the server requested an input method this host cannot execute: " + e.Method
}

// parseResultType applies the three-state resultType rule. It returns the
// normalized resultType and, for input_required, the parsed requests.
func parseResultType(data json.RawMessage) (string, *InputRequiredResult, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return "", nil, &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP result envelope"}
	}
	rawType, hasType := fields["resultType"]
	if !hasType || isJSONNull(rawType) {
		return ResultTypeComplete, nil, nil
	}
	var resultType string
	if err := json.Unmarshal(rawType, &resultType); err != nil {
		return "", nil, &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP resultType"}
	}
	switch resultType {
	case ResultTypeComplete:
		return ResultTypeComplete, nil, nil
	case ResultTypeInputRequired:
		parsed, err := parseInputRequiredResult(fields)
		if err != nil {
			return "", nil, err
		}
		return ResultTypeInputRequired, parsed, nil
	default:
		return "", nil, &McpError{
			Code:    CodeInvalidRequest,
			Message: "Unknown MCP resultType: " + resultType,
		}
	}
}

// parseInputRequiredResult validates the two optional members. At least one must
// be present (2026-07-28: an input_required result with neither is malformed).
func parseInputRequiredResult(fields map[string]json.RawMessage) (*InputRequiredResult, error) {
	result := &InputRequiredResult{}
	invalid := func(message string) error {
		return &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP input_required result: " + message}
	}
	if raw, ok := fields["inputRequests"]; ok && !isJSONNull(raw) {
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, invalid("inputRequests must be an object")
		}
		result.InputRequests = make(map[string]InputRequest, len(entries))
		for key, entry := range entries {
			if key == "" {
				return nil, invalid("an input request key is empty")
			}
			var requestFields map[string]json.RawMessage
			if err := json.Unmarshal(entry, &requestFields); err != nil || requestFields == nil {
				return nil, invalid("input request " + boundedKey(key) + " must be an object")
			}
			var method string
			if err := json.Unmarshal(requestFields["method"], &method); err != nil || method == "" {
				return nil, invalid("input request " + boundedKey(key) + " needs a method")
			}
			params := requestFields["params"]
			if len(params) > 0 && !isJSONNull(params) {
				var object map[string]json.RawMessage
				if err := json.Unmarshal(params, &object); err != nil || object == nil {
					return nil, invalid("input request " + boundedKey(key) + " params must be an object")
				}
			} else {
				params = nil
			}
			result.InputRequests[key] = InputRequest{Method: method, Params: copyOptionalRaw(params)}
		}
	}
	if raw, ok := fields["requestState"]; ok && !isJSONNull(raw) {
		var state string
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, invalid("requestState must be a string")
		}
		result.RequestState = append(json.RawMessage(nil), raw...)
		result.HasRequestState = true
	}
	if len(result.InputRequests) == 0 && !result.HasRequestState {
		return nil, invalid("inputRequests and requestState are both absent")
	}
	return result, nil
}

// boundedKey bounds a server-supplied map key in an error message.
func boundedKey(key string) string {
	if len(key) > 64 {
		return key[:64] + "…"
	}
	return fmt.Sprintf("%q", key)
}
