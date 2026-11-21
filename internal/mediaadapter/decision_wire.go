package mediaadapter

import (
	"context"
	"encoding/json"
	"strings"
)

type systemOneRequest struct {
	State     json.RawMessage             `json:"state"`
	Model     string                      `json:"model,omitempty"`
	Questions map[string]DecisionQuestion `json:"questions"`
	SessionID string                      `json:"session_id,omitempty"`
	User      string                      `json:"user,omitempty"`
}

type systemOneResponse struct {
	ID      string                     `json:"id"`
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   struct {
		InputTokens       int `json:"input_tokens"`
		OutputTokens      int `json:"output_tokens"`
		InputTokensCamel  int `json:"inputTokens"`
		OutputTokensCamel int `json:"outputTokens"`
	} `json:"usage"`
}

func (c *DecisionClient) send(ctx context.Context, body systemOneRequest, questions map[string]DecisionQuestion) ([]byte, error) {
	if c.descriptor.Adapter == DecisionAdapterCloudflare {
		// Workers AI names the model in the run path, not the body; sending a
		// second model field would be an unrequested extension.
		body.Model = ""
	}
	headers := map[string]string{}
	for _, question := range questions {
		if strings.EqualFold(strings.TrimSpace(question.Type), QuestionBox) {
			// Box is an experimental TypeSafe capability gated behind this header.
			// Only the TypeSafe adapter exposes box in its descriptor, so a box
			// question cannot reach another adapter.
			headers["TypeSafe-Danger-Zone-Images"] = "true"
			break
		}
	}
	raw, _, err := c.wire.postJSON(ctx, FamilyDecision, "decide", c.endpoint, body, headers)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// parseSystemOneResponse accepts both the direct System One body
// ({model, answers, usage}) and Cloudflare's Workers AI envelope
// ({result, success, errors, messages}), unwrapping the latter. The answers
// map is returned exactly as the provider wrote it.
func parseSystemOneResponse(raw []byte) (systemOneResponse, error) {
	var envelope struct {
		Result   json.RawMessage   `json:"result"`
		Success  *bool             `json:"success"`
		Errors   []json.RawMessage `json:"errors"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return systemOneResponse{}, &Error{
			Family: FamilyDecision, Op: "decide", Kind: KindUpstream,
			Message: "provider returned invalid JSON", Err: err,
		}
	}
	payload := raw
	if len(envelope.Result) > 0 || envelope.Success != nil || len(envelope.Errors) > 0 || len(envelope.Messages) > 0 {
		if envelope.Success != nil && !*envelope.Success {
			return systemOneResponse{}, &Error{
				Family:  FamilyDecision,
				Op:      "decide",
				Kind:    KindUpstream,
				Message: "clef envelope reported failure: " + envelopeMessages(envelope.Errors, envelope.Messages),
			}
		}
		if len(envelope.Errors) > 0 {
			return systemOneResponse{}, &Error{
				Family:  FamilyDecision,
				Op:      "decide",
				Kind:    KindUpstream,
				Message: "clef envelope reported errors: " + envelopeMessages(envelope.Errors, nil),
			}
		}
		if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
			return systemOneResponse{}, &Error{
				Family: FamilyDecision, Op: "decide", Kind: KindUpstream,
				Message: "clef envelope carried no result",
			}
		}
		payload = envelope.Result
	}
	var response systemOneResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return systemOneResponse{}, &Error{
			Family: FamilyDecision, Op: "decide", Kind: KindUpstream,
			Message: "provider returned an invalid System One body", Err: err,
		}
	}
	if response.Answers == nil {
		return systemOneResponse{}, &Error{
			Family: FamilyDecision, Op: "decide", Kind: KindUpstream,
			Message: "provider response has no answers object",
		}
	}
	response.Usage.InputTokens = firstPositive(response.Usage.InputTokens, response.Usage.InputTokensCamel)
	response.Usage.OutputTokens = firstPositive(response.Usage.OutputTokens, response.Usage.OutputTokensCamel)
	return response, nil
}

func envelopeMessages(errors, messages []json.RawMessage) string {
	parts := make([]string, 0, len(errors)+len(messages))
	for _, item := range errors {
		parts = append(parts, jsonText(item))
	}
	for _, item := range messages {
		parts = append(parts, jsonText(item))
	}
	if len(parts) == 0 {
		return "no detail"
	}
	return strings.Join(parts, "; ")
}

// decisionUsage converts the parsed wire usage. The cost field some gateways
// return is intentionally not modeled: usage records objective counts only.
func decisionUsage(response systemOneResponse, questions int) Usage {
	return Usage{
		InputTokens:  response.Usage.InputTokens,
		OutputTokens: response.Usage.OutputTokens,
		Questions:    questions,
	}
}
