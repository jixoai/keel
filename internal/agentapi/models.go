package agentapi

import (
	"net/http"

	"github.com/jixoai/keel/internal/modelcfg"
)

// ModelCatalog is the product-layer seam between AgentAPI and the resolved
// provider configuration (internal/modelcfg). The implementation lives in the
// hosting process (cmd/keel), so this package never depends on the service
// assembly. Both projections MUST come from the same resolution, or the
// settings view and the picker list drift apart.
type ModelCatalog interface {
	// ProviderViews returns the resolved provider settings for visible
	// providers only (a hidden provider is not part of the API surface). Every
	// visible provider keeps all of its member models, including incomplete or
	// disabled ones — the settings view is where their issues are read. A
	// provider whose credential this process cannot resolve stays visible with
	// an issue; only the picker list drops it.
	ProviderViews() []modelcfg.ProviderView
	// ModelViews returns the flat, selectable model list in stable order
	// (models of visible providers whose credential this process can resolve;
	// the executable registry projection).
	ModelViews() []modelcfg.ModelView
	// ResolutionIssues reports resolution-level diagnostics that no single
	// provider view can carry (an empty resolution, every provider hidden).
	ResolutionIssues() []modelcfg.Issue
	// DefaultSelection is the current generation's default selection — the
	// model a run that carries no selection of its own resolves. It is the
	// document's declared default (zero when it names none), reported by the
	// settings read view so a client can show the effective default even when
	// the deployment document itself is empty.
	DefaultSelection() modelcfg.Selection
}

// providersResponse is the GET /v1/providers body. Top-level issues report
// resolution-level diagnostics; per-provider and per-model issues travel
// inside their views.
type providersResponse struct {
	Providers []modelcfg.ProviderView `json:"providers"`
	Issues    []modelcfg.Issue        `json:"issues"`
}

// modelsResponse is the GET /v1/models body.
type modelsResponse struct {
	Models []modelcfg.ModelView `json:"models"`
	Issues []modelcfg.Issue     `json:"issues"`
	// Enforcement says which published capability facts the server actually
	// enforces. A client MUST NOT promise behaviour from a declared fact alone:
	// v1 enforces the tool gate (a model declaring supportsToolCall=false never
	// receives tools) while the media gate is a recorded deferral, so an
	// attachment policy may not be derived from properties.inputFormat yet.
	// Keeping the answer in the response means a client reads it instead of
	// assuming it.
	Enforcement capabilityEnforcement `json:"enforcement"`
}

// capabilityEnforcement is the enforcement ledger of the published capabilities.
type capabilityEnforcement struct {
	ToolCall bool `json:"toolCall"`
	Media    bool `json:"media"`
}

// noProvidersIssue is the resolution-level diagnostic of a deployment with no
// provider configuration at all (an empty or builtin-less resolution): an
// empty list without a reason would look like a client bug. A resolution whose
// diagnostics the catalog supplied (for example every provider hidden) uses
// those instead, so the two cases never read alike.
func noProvidersIssue() modelcfg.Issue {
	return modelcfg.Issue{Field: "providers", Message: "no provider configuration is loaded"}
}

// resolutionIssues is the settings/list diagnostic set: the catalog's own
// resolution-level issues, with the generic "nothing loaded" message only when
// it explains an empty provider list.
func resolutionIssues(catalog ModelCatalog, providers []modelcfg.ProviderView) []modelcfg.Issue {
	issues := append([]modelcfg.Issue{}, catalog.ResolutionIssues()...)
	if len(providers) == 0 && len(issues) == 0 {
		issues = append(issues, noProvidersIssue())
	}
	if issues == nil {
		issues = []modelcfg.Issue{}
	}
	return issues
}

// notWired answers 501 in the shape the cancel seam established: the endpoint
// exists, but this deployment did not wire the capability. message is the
// complete error sentence.
func notWired(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error":  message,
		"status": "not_wired",
	})
}

// visibleProviderViews drops hidden providers. The catalog contract already
// excludes them; the filter is repeated here so the response is correct no
// matter which side of the seam made the mistake (a hidden provider leaking
// into a settings response is a credential/inventory leak, not a cosmetic
// one).
func visibleProviderViews(views []modelcfg.ProviderView) []modelcfg.ProviderView {
	out := make([]modelcfg.ProviderView, 0, len(views))
	for _, view := range views {
		if view.Visibility == modelcfg.VisibilityHidden {
			continue
		}
		out = append(out, view)
	}
	return out
}

// catalog resolves the model catalog the registry endpoints read: ModelsLive
// wins when set — it re-reads the runtime's current generation on every call,
// so a settings write is visible without a restart — and Models is the static
// seam used by tests and embedded hosts. A nil result means this deployment has
// no provider configuration capability to report, which the endpoints answer
// with 501 rather than an empty list.
func (s *Server) catalog() ModelCatalog {
	if s.cfg.ModelsLive != nil {
		return s.cfg.ModelsLive()
	}
	return s.cfg.Models
}

// listProviders implements GET /v1/providers: the resolved settings view
// (Effective configurations, issues, executable/selectable flags). It is not a
// picker projection — incomplete and disabled entries stay visible with their
// issues so an operator can see why something cannot be chosen.
func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantFrom(w, r); !ok {
		return
	}
	catalog := s.catalog()
	if catalog == nil {
		notWired(w, "provider settings are not wired")
		return
	}
	providers := visibleProviderViews(catalog.ProviderViews())
	writeJSON(w, http.StatusOK, providersResponse{
		Providers: providers,
		Issues:    resolutionIssues(catalog, providers),
	})
}

// listModels implements GET /v1/models: the flat selectable list a picker
// consumes, derived from the same resolution as /v1/providers. Selectability
// is the seam's business (complete + enabled + usable provider); this handler
// only enforces the visibility boundary, so a model of a hidden provider can
// never be picked.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantFrom(w, r); !ok {
		return
	}
	catalog := s.catalog()
	if catalog == nil {
		notWired(w, "model listing is not wired")
		return
	}
	providers := visibleProviderViews(catalog.ProviderViews())
	visible := make(map[string]struct{}, len(providers))
	for _, view := range providers {
		visible[view.ProviderID] = struct{}{}
	}
	all := catalog.ModelViews()
	models := make([]modelcfg.ModelView, 0, len(all))
	for _, model := range all {
		if _, ok := visible[model.ProviderID]; !ok {
			continue
		}
		models = append(models, model)
	}
	writeJSON(w, http.StatusOK, modelsResponse{
		Models: models,
		Issues: resolutionIssues(catalog, providers),
		// v1: the tool gate is enforced in the runtime, the media gate is not
		// (see the spec's capability scenario and the change's deferred list).
		Enforcement: capabilityEnforcement{ToolCall: true, Media: false},
	})
}
