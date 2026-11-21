package mediawiring

import (
	"context"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/mediaadapter"
	"github.com/jixoai/keel/internal/modelcfg"
)

// failingSpeechProvider is the fail-closed stand-in for a configured speech
// provider whose credential could not be resolved. It never touches the wire.
type failingSpeechProvider struct {
	family string
	reason string
}

func (p *failingSpeechProvider) Synthesize(context.Context, mediaadapter.SynthesizeRequest) (mediaadapter.SpeechAudio, error) {
	return mediaadapter.SpeechAudio{}, missingCredential(p.family, "synthesize", p.reason)
}

func (p *failingSpeechProvider) Transcribe(context.Context, mediaadapter.TranscribeRequest) (mediaadapter.SpeechTranscript, error) {
	return mediaadapter.SpeechTranscript{}, missingCredential(p.family, "transcribe", p.reason)
}

// buildSpeechTool assembles one speech family. TTS and STT are distinct
// provider types and distinct tools; sharing a provider endpoint is allowed by
// configuration but never assumed. Missing configuration omits the tool and
// records a provider-scoped issue; a missing credential keeps the tool so the
// call fails with the typed missing_credentials error and no wire request.
func buildSpeechTool(resolved resolved, opts Options, family string) (agentcore.AgentTool, FamilyState, []modelcfg.Issue) {
	state := FamilyState{Family: family}
	candidate, ok := chooseModel(resolved, family)
	if !ok {
		return nil, state, nil
	}
	state.Configured = true
	state.ProviderID, state.ModelID = candidate.provider.ProviderID, candidate.model.ModelID
	if opts.Artifacts == nil && !opts.validateOnly {
		state.Reason = "no artifact store: speech audio could not be referenced"
		return nil, state, []modelcfg.Issue{issue(candidate.provider.ProviderID, "media.artifacts",
			"this deployment has no artifact store, so speech cannot return a layerfs reference")}
	}
	if strings.TrimSpace(candidate.provider.API.BaseURL) == "" {
		state.Reason = "provider has no base URL"
		return nil, state, []modelcfg.Issue{issue(candidate.provider.ProviderID, "api.baseUrl", "base URL is required")}
	}
	key, reason := credential(opts, candidate.provider)
	var provider mediaadapter.SpeechProvider
	if reason != "" {
		state.Reason = reason
		provider = &failingSpeechProvider{family: family, reason: reason}
	} else {
		wire, err := mediaadapter.NewHTTPSpeechProvider(mediaadapter.HTTPSpeechConfig{
			BaseURL: candidate.provider.API.BaseURL,
			APIKey:  key,
			Model:   candidate.model.ModelID,
			Client:  httpClient(opts),
		})
		if err != nil {
			state.Reason = err.Error()
			return nil, state, []modelcfg.Issue{issue(candidate.provider.ProviderID, "media."+family, err.Error())}
		}
		provider = wire
	}
	adapter := &mediaadapter.SpeechAdapter{
		Provider:     provider,
		Store:        store(opts),
		MaxAudioSize: opts.MaxAssetBytes,
	}
	state.Wired = true
	if reason == "" {
		state.Reason = ""
	}
	var tool agentcore.AgentTool
	if family == FamilyTTS {
		tool = mediaadapter.NewSpeechTTSTool(adapter)
	} else {
		tool = mediaadapter.NewSpeechSTTTool(adapter, opts.Artifacts, opts.MaxAssetBytes)
	}
	return tool, state, nil
}
