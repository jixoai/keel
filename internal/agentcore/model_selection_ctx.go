// Tool model-selection context (change feat-mcp-apps-events, P5.2 review fix).
//
// The runner resolves the attempt's model selection fact once (the run's
// selection merged with the execution binding's default) and stamps the same
// fact on the tool-execution context. A tool that performs an out-of-band model
// call (MCP sampling/createMessage) must run on the model the session is
// configured for, not on the deployment default: the sampling executor reads
// this key and resolves through the same ModelService the runner used.
//
// The value is a transient transport fact, never persisted. Its absence means
// "no selection was made for this attempt" and the deployment default applies.

package agentcore

import (
	"context"
	"encoding/json"
)

type toolModelSelectionKey struct{}

// WithToolModelSelection returns a context carrying the attempt's selection
// fact. A nil/empty selection leaves the context unchanged: absence is the
// deployment default, not an empty choice.
func WithToolModelSelection(ctx context.Context, selection json.RawMessage) context.Context {
	if len(selection) == 0 {
		return ctx
	}
	return context.WithValue(ctx, toolModelSelectionKey{}, append(json.RawMessage(nil), selection...))
}

// ToolModelSelectionFromContext returns the attempt's selection fact, nil when
// the attempt named none.
func ToolModelSelectionFromContext(ctx context.Context) json.RawMessage {
	selection, _ := ctx.Value(toolModelSelectionKey{}).(json.RawMessage)
	if len(selection) == 0 {
		return nil
	}
	return selection
}
