package servicehost

// Provider health probing: `keel model provider test` (and the `--refresh`
// mode of `keel model catalog`) asks the hosting process to make one real but
// minimal upstream request with the provider's resolved credential.
//
// Why the probe lives here: the credential never leaves the server. The CLI is
// a thin HTTP client and the probe endpoint answers a measured result, not a
// secret; a client-side probe would have to hold the key and would duplicate
// the credential ordering this package already owns.
//
// The request is the provider's model-catalog GET (`/models`, or `/v1/models`
// for Anthropic): it is the cheapest request every wire protocol exposes, it
// authenticates the same way a generation would, and it returns the live
// lineup the catalog command can print. A provider whose endpoint does not
// implement it fails honestly (unreachable) instead of a fabricated success.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/provider"
)

// ProbeProvider implements the harnessapi.ProviderProber seam.
//
// The ordering is deliberate: a provider that is incomplete on paper is
// reported as invalid_config without a request; a provider whose credential
// cannot be resolved is needs_auth without a request (the credential ordering
// is the same credentialDecision an attempt uses); only then is one GET sent.
// Every answer carries references and field paths, never a credential value.
func (m *modelsService) ProbeProvider(ctx context.Context, providerID string) (modelcfg.ProviderProbe, bool, error) {
	id := strings.TrimSpace(providerID)
	var view modelcfg.ProviderView
	found := false
	for _, candidate := range m.ProviderViews() {
		if candidate.ProviderID == id {
			view = candidate
			found = true
			break
		}
	}
	if !found {
		return modelcfg.ProviderProbe{}, false, nil
	}
	probe := modelcfg.ProviderProbe{
		ProviderID:   view.ProviderID,
		ProviderType: view.ProviderType,
		APIType:      view.API.Type,
		Endpoint:     view.API.BaseURL,
	}
	if !view.Executable {
		probe.Status = modelcfg.ProbeStatusInvalidConfig
		probe.Error = "the provider configuration is incomplete; no request was sent"
		probe.Issues = append([]modelcfg.Issue{}, view.Issues...)
		return probe, true, nil
	}
	decision := m.credentialDecision(view.Access)
	if !decision.found {
		issue, _ := m.credentialIssue(view.Access)
		probe.Status = modelcfg.ProbeStatusNeedsAuth
		probe.Error = "no credential could be resolved; no request was sent"
		probe.Issues = []modelcfg.Issue{issue}
		return probe, true, nil
	}
	if view.API.Type == modelcfg.APITypeDemo {
		// The demo protocol is in-process by construction: there is no endpoint
		// to reach, and inventing a network request would be the fake success
		// this probe exists to prevent.
		probe.OK = true
		probe.Status = modelcfg.ProbeStatusReady
		return probe, true, nil
	}
	protocol, err := probeProtocol(view.API.Type)
	if err != nil {
		probe.Status = modelcfg.ProbeStatusInvalidConfig
		probe.Error = err.Error()
		probe.Issues = []modelcfg.Issue{{Field: "api.type", Message: err.Error()}}
		return probe, true, nil
	}
	if strings.TrimSpace(view.API.BaseURL) == "" {
		probe.Status = modelcfg.ProbeStatusInvalidConfig
		probe.Error = "the provider declares no upstream base URL; no request was sent"
		probe.Issues = []modelcfg.Issue{{Field: "api.baseUrl", Message: "a probe needs an upstream endpoint"}}
		return probe, true, nil
	}

	started := time.Now()
	models, err := provider.FetchRemoteModels(ctx, view.API.BaseURL, protocol, decision.value)
	probe.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		probe.Error = probeFailureMessage(err)
		probe.Status = modelcfg.ProbeStatusUnreachable
		if probeAuthRejected(probe.Error) {
			// The request reached the endpoint and the endpoint rejected the
			// credential. That is the health vocabulary's needs_auth, not a
			// transport failure: the fix is the reference or the key.
			probe.Status = modelcfg.ProbeStatusNeedsAuth
			probe.Issues = []modelcfg.Issue{{
				Field:   "access",
				Message: "the provider rejected the resolved credential; update the reference or the key",
			}}
		}
		return probe, true, nil
	}
	probe.OK = true
	probe.Status = modelcfg.ProbeStatusReady
	probe.Models = models
	probe.ModelCount = len(models)
	return probe, true, nil
}

// probeProtocol maps the document's canonical API type onto the wire
// protocol provider.FetchRemoteModels understands.
func probeProtocol(apiType string) (string, error) {
	switch apiType {
	case modelcfg.APITypeOpenAIChatCompletions:
		return provider.ProtocolOpenAI, nil
	case modelcfg.APITypeOpenAIResponses:
		return provider.ProtocolOpenAIResponses, nil
	case modelcfg.APITypeAnthropicMessages:
		return provider.ProtocolAnthropic, nil
	default:
		return "", fmt.Errorf("api type %q has no probe request", apiType)
	}
}

// probeFailureMessage renders a discovery failure without the request URL.
// A *url.Error repeats the full URL (which may carry query parameters), and
// the probe's contract is that no configured value leaks through the failure
// text; the transport cause alone is enough to act on.
func probeFailureMessage(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return "model discovery request failed: " + urlErr.Err.Error()
	}
	return err.Error()
}

// probeAuthRejected reports whether the discovery failure was the endpoint
// refusing the credential (401/403). FetchRemoteModels reports a non-200 as
// "model discovery: GET /models returned N"; the status code is the only
// signal it exposes, and matching it is what lets the probe answer needs_auth
// instead of a generic unreachable.
func probeAuthRejected(message string) bool {
	return strings.Contains(message, "returned 401") || strings.Contains(message, "returned 403")
}
