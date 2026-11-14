package mediagw

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jixoai/keel/internal/demo"
)

// The demo media provider: image generation, image editing, structured decisions
// and the narrow chat proxy answered in-process when KEEL_DEMO_PROVIDER is on.
//
// # Why it is a mode of the same Gateway
//
// Every caller (the HTTP media routes, the /v1/decisions/ask edge) already goes
// through the Gateway methods, and those methods own the size allowlist, the model
// allowlist, the validation messages and the error taxonomy. Answering the demo
// through the same methods means a review deployment exercises the real request
// path: the same 400s for a bad size, the same model check, the same response
// envelope. A separate demo HTTP surface would be a second implementation that
// could disagree with the real one.
//
// # Routing
//
// Two rules, and the difference matters because the catalog lists the demo models
// whenever the switch is on (so they are selectable):
//
//   - an explicit demo-* model is served by the demo provider — the client can pick
//     it from /v1/models, so it must be runnable in every deployment, including one
//     that also holds vendor credentials;
//   - otherwise the vendor wins when configured, and with no vendor the demo answers
//     as the fallback. When neither holds, every operation keeps returning
//     ErrNotConfigured (HTTP 503) exactly as before.
//
// A demo model id with the switch OFF is not served: the vendor allowlist answers
// its usual 400 when credentials exist, and the endpoint keeps its 503 when not.
//
// # Determinism
//
// The image answer is a server-hosted tile selected by the request's aspect ratio,
// and the decision answer is a fixed function of the question type and the option
// order. Nothing reads the clock, the prompt or any user data, so two identical
// requests produce identical bodies.

// demoEnabled reports whether the switch is on for this gateway. It is the
// switch alone: it says nothing about which provider answers a given request.
//
// The switch is read through the gateway's own Getenv seam rather than the process
// environment directly, so a test can assert both states without mutating shared
// state and a host can inject its configuration.
func (g *Gateway) demoEnabled() bool {
	if g.cfg.DemoDisabled {
		return false
	}
	return demo.EnabledFromGetenv(g.cfg.Getenv)
}

// demoActive reports whether the demo provider is the DEFAULT answer for this
// gateway: the switch is on and no vendor provider is configured.
//
// This is the fallback rule, not the routing rule. A request that explicitly names
// a demo model is answered by the demo provider even in a deployment that also
// holds vendor credentials — see demoRequested. The default (no model named) stays
// vendor-first, so turning the demo switch on in a working deployment changes
// nothing about what its own credentials serve.
func (g *Gateway) demoActive() bool {
	if g.cfg.Configured() {
		return false
	}
	return g.demoEnabled()
}

// demoRequested reports whether this request explicitly asks for a demo model of
// the given family and the switch is on.
//
// Why this exists: the model catalog lists the demo models whenever the switch is
// on, and a listed model is selectable. If a deployment also held vendor
// credentials, vendor-first routing used to reject that selection with
// "model \"demo-image\" is not allowed" — a model the client could pick but never
// run. Routing on the explicit id keeps the catalog honest: anything selectable is
// servable, while an unnamed request still goes to the vendor.
//
// An empty model is never "requested": it means "the deployment's default model",
// which is the vendor's when credentials exist. An unknown or vendor model id is
// not requested either, so the vendor path keeps producing its own 400 for it.
func (g *Gateway) demoRequested(model, family string) bool {
	requested := strings.TrimSpace(model)
	if requested == "" {
		return false
	}
	requestedFamily, ok := demo.Family(requested)
	if !ok || requestedFamily != family {
		return false
	}
	return g.demoEnabled()
}

// demoGenerate answers an image generation request. Validation is the same code
// the vendor path runs, so a demo request that would be a 400 is a 400 here too.
func (g *Gateway) demoGenerate(req ImageRequest) (*ImageResult, error) {
	model, err := g.resolveDemoModel(req.Model, demo.ModelImage)
	if err != nil {
		return nil, err
	}
	n, err := validateImageRequest(req)
	if err != nil {
		return nil, err
	}
	asset := demo.AssetForSize(req.Size)
	assets := make([]Asset, 0, n)
	for i := 0; i < n; i++ {
		assets = append(assets, Asset{
			// Every asset of one request renders the same tile (the demo does not
			// pretend to hold four different pictures), but the URLs stay distinct:
			// `?i=<index>` makes the nth asset addressable on its own, so a gallery
			// keeps n separate entries, an edit can name one of them, and a reviewer
			// can see which of the n it came from. The query does not change the
			// bytes served.
			URL:    demo.AssetURL(asset.Name) + "?i=" + strconv.Itoa(i),
			Width:  asset.Width,
			Height: asset.Height,
			Model:  model,
		})
	}
	return &ImageResult{Assets: assets, Model: model}, nil
}

// demoEdit answers an image edit request. The reference rules are the vendor
// ones (an http(s) URL, never a data URL); the demo additionally accepts its own
// hosted asset path, which is what a gallery asset URL resolves to before the
// client absolutizes it.
func (g *Gateway) demoEdit(req ImageEditRequest) (*ImageResult, error) {
	model, err := g.resolveDemoModel(req.Model, demo.ModelImage)
	if err != nil {
		return nil, err
	}
	if err := validateEditRequest(req); err != nil {
		return nil, err
	}
	// A reference to a hosted demo asset produces a visibly different tile (so a
	// multi-turn chain changes image); any other accepted reference yields the
	// square tile. Deterministic in both cases.
	asset := demo.AssetForSize("1x1")
	if isDemoAssetReference(req.Image) {
		asset = demo.AssetForSize("1x2")
	}
	return &ImageResult{
		Assets: []Asset{{
			// An edit answers one image, so it carries no `?i=` marker: the index
			// distinguishes the members of one generation batch, not turns.
			URL:    demo.AssetURL(asset.Name),
			Width:  asset.Width,
			Height: asset.Height,
			Model:  model,
		}},
		Model: model,
	}, nil
}

// isDemoAssetReference reports whether an edit reference points at the
// server-hosted demo assets, either as the bare path Generate returns or as an
// absolute URL carrying that path.
//
// It is a string test, never a fetch: the demo must not become an SSRF surface by
// dereferencing a client-supplied URL (the vendor path forwards the URL to a
// provider that fetches it; the demo has nobody to forward to).
func isDemoAssetReference(raw string) bool {
	reference := strings.TrimSpace(raw)
	prefix := demo.AssetPathPrefix
	if strings.HasPrefix(reference, prefix) {
		return true
	}
	idx := strings.Index(reference, prefix)
	if idx < 0 {
		return false
	}
	scheme := reference[:idx]
	return strings.EqualFold(scheme, "http://") || strings.EqualFold(scheme, "https://")
}

// demoDecision answers a structured decision request with deterministic answers.
func (g *Gateway) demoDecision(req DecisionRequest) (*DecisionResult, error) {
	model, err := g.resolveDemoModel(req.Model, demo.ModelDecision)
	if err != nil {
		return nil, err
	}
	if err := validateDecisionRequest(req); err != nil {
		return nil, err
	}
	answers := make([]any, 0, len(req.Questions))
	for i, question := range req.Questions {
		answer, err := demoAnswer(question, i)
		if err != nil {
			return nil, err
		}
		answers = append(answers, answer)
	}
	raw, err := json.Marshal(answers)
	if err != nil {
		return nil, &Error{Status: http.StatusInternalServerError, Message: "failed to encode decision answers"}
	}
	usage, err := json.Marshal(map[string]any{
		"input_tokens":  0,
		"output_tokens": 0,
		"total_tokens":  0,
	})
	if err != nil {
		return nil, &Error{Status: http.StatusInternalServerError, Message: "failed to encode decision usage"}
	}
	return &DecisionResult{Answers: raw, Model: model, Usage: usage}, nil
}

// demoAnswer produces one deterministic answer in the shape the vendor gateway
// returns: choice names an option with a probability distribution, predicate
// reports a fixed probability, score reports a level over the declared criteria.
//
// index is the question's position; it only affects the "which option" choice, so
// a question set that repeats the same question gets distinct (but reproducible)
// answers instead of a wall of identical ones.
func demoAnswer(question Question, index int) (map[string]any, error) {
	switch question.Type {
	case "choice":
		if len(question.Choices) == 0 {
			return nil, &Error{Status: http.StatusBadRequest, Message: "a choice question needs at least one choice"}
		}
		// The probability mass is spread evenly; the selected option rotates by
		// question index so a multi-question request reads as a real answer set.
		probabilities := make([]float64, len(question.Choices))
		share := 1.0 / float64(len(question.Choices))
		for i := range probabilities {
			probabilities[i] = share
		}
		selected := question.Choices[index%len(question.Choices)].Value
		return map[string]any{
			"type":          "choice",
			"choice":        selected,
			"confidence":    share,
			"probabilities": probabilities,
		}, nil
	case "predicate":
		return map[string]any{
			"type":        "predicate",
			"probability": 0.5,
		}, nil
	case "score":
		// The score answer carries the legend the vendor returns: the declared
		// criteria become the level names, and a middle level avoids an extreme.
		levels := decodeCriteriaLevels(question.Criteria)
		legend := make(map[string]string, len(levels))
		probabilities := make([]float64, len(levels))
		if len(levels) > 0 {
			share := 1.0 / float64(len(levels))
			for i, level := range levels {
				legend[fmt.Sprintf("%d", i)] = level
				probabilities[i] = share
			}
		}
		score := 0.0
		if len(levels) > 1 {
			score = float64(len(levels)-1) / 2
		}
		return map[string]any{
			"type":          "score",
			"score":         score,
			"confidence":    1.0,
			"legend":        legend,
			"probabilities": probabilities,
		}, nil
	default:
		return nil, &Error{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("question type %q is not supported; use choice, predicate or score", question.Type),
		}
	}
}

// decodeCriteriaLevels reads the ordered level names a score question declares.
// A score question forwards its raw criteria, which is an array of strings; an
// unreadable document yields no levels (the answer then carries an empty legend
// rather than a fabricated one).
func decodeCriteriaLevels(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var levels []string
	if err := json.Unmarshal(raw, &levels); err != nil {
		return nil
	}
	out := make([]string, 0, len(levels))
	for _, level := range levels {
		if trimmed := strings.TrimSpace(level); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// demoChat answers the narrow diagnostics chat proxy with the fixed completion.
func (g *Gateway) demoChat(req ChatRequest) (json.RawMessage, error) {
	var messages []json.RawMessage
	if err := json.Unmarshal(req.Messages, &messages); err != nil || len(messages) == 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "messages must be a non-empty array"}
	}
	if req.MaxTokens < 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "max_tokens must not be negative"}
	}
	return demo.Completion(demo.ModelLLM), nil
}

// resolveDemoModel applies the same "only the configured model" policy the vendor
// path uses, with the demo model as the configured one.
func (g *Gateway) resolveDemoModel(requested, demoModel string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return demoModel, nil
	}
	if requested != demoModel {
		return "", &Error{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("model %q is not allowed; use %q", requested, demoModel),
		}
	}
	return requested, nil
}

// The validators below are shared by both paths. The vendor methods call them
// after resolving the effective model (they need the model id for the payload);
// the demo methods call them before producing the answer. One implementation
// means the two can never disagree about what a valid request is.

// validateImageRequest checks a generation request and returns the effective
// asset count.
func validateImageRequest(req ImageRequest) (int, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return 0, &Error{Status: http.StatusBadRequest, Message: "prompt is required"}
	}
	n := req.N
	if n == 0 {
		n = 1
	}
	if n < 1 || n > 4 {
		return 0, &Error{Status: http.StatusBadRequest, Message: "n must be between 1 and 4"}
	}
	if req.Size != "" && !SizeAllowed(req.Size) {
		return 0, &Error{Status: http.StatusBadRequest, Message: sizeMessage()}
	}
	return n, nil
}

// validateEditRequest checks an edit request's prompt and reference shape.
func validateEditRequest(req ImageEditRequest) error {
	if strings.TrimSpace(req.Prompt) == "" {
		return &Error{Status: http.StatusBadRequest, Message: "prompt is required"}
	}
	image := strings.TrimSpace(req.Image)
	if image == "" {
		return &Error{Status: http.StatusBadRequest, Message: "image is required"}
	}
	if strings.HasPrefix(image, "data:") {
		return &Error{
			Status:  http.StatusBadRequest,
			Message: "data URLs are not supported for image edits; pass the provider http(s) URL returned by /v1/media/images/generate (or upload the image first)",
		}
	}
	if !isHTTPURL(image) && !isDemoAssetReference(image) {
		return &Error{Status: http.StatusBadRequest, Message: "image must be an http(s) URL"}
	}
	return nil
}

// validateDecisionRequest checks a decision request's input, question count and
// question types.
func validateDecisionRequest(req DecisionRequest) error {
	input := strings.TrimSpace(string(req.Input))
	if input == "" || input == "null" {
		return &Error{Status: http.StatusBadRequest, Message: "input is required"}
	}
	if len(req.Questions) < 1 || len(req.Questions) > 400 {
		return &Error{Status: http.StatusBadRequest, Message: "questions must contain between 1 and 400 entries"}
	}
	for i, question := range req.Questions {
		switch question.Type {
		case "choice", "predicate", "score":
		default:
			return &Error{
				Status:  http.StatusBadRequest,
				Message: fmt.Sprintf("questions[%d].type %q is not supported; use choice, predicate or score", i, question.Type),
			}
		}
	}
	return nil
}
