package mediaadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// VideoRequest is one video generation call. The shape mirrors Hermes
// video_generate: prompt is required; the rest are capability-gated options
// that a provider may clamp or ignore.
type VideoRequest struct {
	Model              string
	Prompt             string
	NegativePrompt     string
	DurationSeconds    int
	AspectRatio        string
	Resolution         string
	ImageURL           string
	ReferenceImageURLs []string
	Seed               *int
	Audio              *bool
}

// VideoBatch is a provider response before persistence.
type VideoBatch struct {
	Model  string
	Assets []ProviderAsset
	Usage  Usage
}

// VideoProvider is the capability interface a video backend implements.
type VideoProvider interface {
	GenerateVideo(ctx context.Context, req VideoRequest) (VideoBatch, error)
}

// VideoResult is the adapter result: references only.
type VideoResult struct {
	Model  string     `json:"model"`
	Videos []Artifact `json:"videos"`
	Usage  Usage      `json:"usage"`
}

// VideoAdapter validates a call, invokes the provider behind the panic
// boundary, persists every asset and counts objective usage.
type VideoAdapter struct {
	Provider      VideoProvider
	Store         ArtifactStore
	Fetcher       AssetFetcher
	MaxAssetBytes int64
}

// MaxVideosPerCall bounds one call; providers normally return one.
const MaxVideosPerCall = 4

// Generate implements the video family.
func (a *VideoAdapter) Generate(ctx context.Context, session string, req VideoRequest) (VideoResult, error) {
	if a == nil || a.Provider == nil {
		return VideoResult{}, configError(FamilyVideo, "generate", "video provider is not configured")
	}
	if a.Store == nil {
		return VideoResult{}, configError(FamilyVideo, "generate", "artifact store is not configured")
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return VideoResult{}, requestError(FamilyVideo, "generate", "prompt is required", []FieldError{{
			Field: "prompt", Message: "prompt is required",
		}})
	}
	if err := validatePrompt(FamilyVideo, req.Prompt); err != nil {
		return VideoResult{}, err
	}
	if req.DurationSeconds < 0 || req.DurationSeconds > 300 {
		return VideoResult{}, requestError(FamilyVideo, "generate",
			"duration must be between 0 and 300 seconds", []FieldError{{
				Field: "duration", Message: "must be between 0 and 300 seconds",
			}})
	}
	if len(req.ReferenceImageURLs) > MaxReferenceImages {
		return VideoResult{}, requestError(FamilyVideo, "generate",
			fmt.Sprintf("at most %d reference images are accepted", MaxReferenceImages), []FieldError{{
				Field:   "reference_image_urls",
				Message: fmt.Sprintf("at most %d reference images are accepted", MaxReferenceImages),
			}})
	}

	batch, err := guard(FamilyVideo, "generate", func() (VideoBatch, error) {
		return a.Provider.GenerateVideo(ctx, req)
	})
	if err != nil {
		return VideoResult{}, providerError(FamilyVideo, "generate", err)
	}
	if len(batch.Assets) == 0 {
		return VideoResult{}, &Error{
			Family:  FamilyVideo,
			Op:      "generate",
			Kind:    KindUpstream,
			Message: "provider returned no videos",
		}
	}
	if len(batch.Assets) > MaxVideosPerCall {
		batch.Assets = batch.Assets[:MaxVideosPerCall]
	}

	result := VideoResult{Model: firstNonEmpty(batch.Model, req.Model), Usage: batch.Usage}
	// The persisted assets are the objective count; a provider-reported video
	// count (or one from a batch that was truncated above) must not double-count.
	result.Usage.Videos = 0
	result.Usage.Bytes = 0
	for _, asset := range batch.Assets {
		data, mime, err := a.assetBytes(ctx, asset)
		if err != nil {
			return VideoResult{}, err
		}
		artifact, err := a.Store.Put(ctx, ArtifactInput{
			Session:        session,
			Kind:           ArtifactVideo,
			Mime:           firstNonEmpty(asset.Mime, mime),
			Data:           data,
			Width:          asset.Width,
			Height:         asset.Height,
			DurationMillis: asset.DurationMillis,
		})
		if err != nil {
			var adapterErr *Error
			if asAdapterError(err, &adapterErr) {
				return VideoResult{}, adapterErr
			}
			return VideoResult{}, artifactError(FamilyVideo, "store", err)
		}
		result.Videos = append(result.Videos, artifact)
		result.Usage.Videos++
		result.Usage.Bytes += artifact.Size
	}
	return result, nil
}

func (a *VideoAdapter) assetBytes(ctx context.Context, asset ProviderAsset) ([]byte, string, error) {
	switch {
	case len(asset.Data) > 0:
		if err := checkAssetSize(FamilyVideo, len(asset.Data), a.MaxAssetBytes); err != nil {
			return nil, "", err
		}
		return asset.Data, asset.Mime, nil
	case strings.TrimSpace(asset.URL) != "":
		if a.Fetcher == nil {
			return nil, "", &Error{
				Family:  FamilyVideo,
				Op:      "fetch",
				Kind:    KindConfig,
				Message: "provider returned an asset URL but no asset fetcher is configured",
			}
		}
		fetched, err := a.Fetcher.Fetch(ctx, asset.URL)
		if err != nil {
			return nil, "", providerError(FamilyVideo, "fetch", err)
		}
		if err := checkAssetSize(FamilyVideo, len(fetched.Data), a.MaxAssetBytes); err != nil {
			return nil, "", err
		}
		return fetched.Data, fetched.Mime, nil
	default:
		return nil, "", &Error{
			Family:  FamilyVideo,
			Op:      "generate",
			Kind:    KindUpstream,
			Message: "provider returned an asset with no bytes and no URL",
		}
	}
}

func checkAssetSize(family string, size int, limit int64) error {
	if limit <= 0 {
		limit = DefaultAssetBytes
	}
	if int64(size) > limit {
		return &Error{
			Family:  family,
			Op:      "store",
			Kind:    KindUpstream,
			Message: fmt.Sprintf("generated asset exceeds the %d-byte limit", limit),
		}
	}
	return nil
}

// HTTPVideoProvider is the request/response wire client for a video backend.
// The wire is deliberately capability-shaped rather than vendor-shaped; a
// deployment points BaseURL at its gateway, and the gateway maps the fields it
// supports.
type HTTPVideoProvider struct {
	wire  *wireClient
	model string
	path  string
	name  string
}

// HTTPVideoConfig configures one video endpoint.
type HTTPVideoConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	// Path defaults to /videos/generations.
	Path             string
	Client           *http.Client
	MaxResponseBytes int64
}

// NewHTTPVideoProvider validates and builds one video wire client.
func NewHTTPVideoProvider(cfg HTTPVideoConfig) (*HTTPVideoProvider, error) {
	wire, err := newWireClient(cfg.BaseURL, cfg.APIKey, cfg.Client, cfg.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		path = "/videos/generations"
	}
	return &HTTPVideoProvider{wire: wire, model: strings.TrimSpace(cfg.Model), path: path, name: "http-videos"}, nil
}

var _ VideoProvider = (*HTTPVideoProvider)(nil)

// Name reports the wire family, for diagnostics.
func (p *HTTPVideoProvider) Name() string { return p.name }

type videoWireRequest struct {
	Model              string   `json:"model,omitempty"`
	Prompt             string   `json:"prompt"`
	NegativePrompt     string   `json:"negative_prompt,omitempty"`
	Duration           int      `json:"duration,omitempty"`
	AspectRatio        string   `json:"aspect_ratio,omitempty"`
	Resolution         string   `json:"resolution,omitempty"`
	ImageURL           string   `json:"image_url,omitempty"`
	ReferenceImageURLs []string `json:"reference_image_urls,omitempty"`
	Seed               *int     `json:"seed,omitempty"`
	Audio              *bool    `json:"audio,omitempty"`
}

type videoWireAsset struct {
	URL      string `json:"url"`
	B64JSON  string `json:"b64_json"`
	Mime     string `json:"mime_type"`
	MimeAlt  string `json:"mime"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Duration int64  `json:"duration_ms"`
}

type videoWireResponse struct {
	Model  string            `json:"model"`
	Data   []json.RawMessage `json:"data"`
	Videos []json.RawMessage `json:"videos"`
	Video  string            `json:"video"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

// GenerateVideo implements VideoProvider.
func (p *HTTPVideoProvider) GenerateVideo(ctx context.Context, req VideoRequest) (VideoBatch, error) {
	if p == nil || p.wire == nil {
		return VideoBatch{}, configError(FamilyVideo, "generate", "video wire is not configured")
	}
	payload := videoWireRequest{
		Model:              firstNonEmpty(req.Model, p.model),
		Prompt:             strings.TrimSpace(req.Prompt),
		NegativePrompt:     strings.TrimSpace(req.NegativePrompt),
		Duration:           req.DurationSeconds,
		AspectRatio:        strings.TrimSpace(req.AspectRatio),
		Resolution:         strings.TrimSpace(req.Resolution),
		ImageURL:           strings.TrimSpace(req.ImageURL),
		ReferenceImageURLs: req.ReferenceImageURLs,
		Seed:               req.Seed,
		Audio:              req.Audio,
	}
	raw, _, err := p.wire.postJSON(ctx, FamilyVideo, "generate", p.path, payload, nil)
	if err != nil {
		return VideoBatch{}, err
	}
	assets, model, usage, err := parseVideoAssets(raw, p.wire.maxResponse)
	if err != nil {
		return VideoBatch{}, err
	}
	return VideoBatch{
		Model:  firstNonEmpty(model, payload.Model),
		Assets: assets,
		Usage:  usage,
	}, nil
}

func parseVideoAssets(raw []byte, maxBytes int64) ([]ProviderAsset, string, Usage, error) {
	var envelope videoWireResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		var asString string
		if json.Unmarshal(raw, &asString) == nil && strings.TrimSpace(asString) != "" {
			return []ProviderAsset{{URL: asString}}, "", Usage{}, nil
		}
		return nil, "", Usage{}, &Error{Family: FamilyVideo, Op: "generate", Kind: KindUpstream, Message: "provider returned invalid JSON", Err: err}
	}
	list := envelope.Data
	if len(list) == 0 {
		list = envelope.Videos
	}
	if len(list) == 0 && strings.TrimSpace(envelope.Video) != "" {
		return []ProviderAsset{{URL: envelope.Video}}, envelope.Model, Usage{}, nil
	}
	if len(list) == 0 {
		return nil, "", Usage{}, &Error{Family: FamilyVideo, Op: "generate", Kind: KindUpstream, Message: "provider returned no videos"}
	}
	assets := make([]ProviderAsset, 0, len(list))
	for _, item := range list {
		var asString string
		if json.Unmarshal(item, &asString) == nil {
			if strings.TrimSpace(asString) != "" {
				assets = append(assets, ProviderAsset{URL: asString})
			}
			continue
		}
		var wire videoWireAsset
		if err := json.Unmarshal(item, &wire); err != nil {
			continue
		}
		asset := ProviderAsset{
			URL:            strings.TrimSpace(wire.URL),
			Mime:           firstNonEmpty(wire.Mime, wire.MimeAlt),
			Width:          wire.Width,
			Height:         wire.Height,
			DurationMillis: wire.Duration,
		}
		if wire.B64JSON != "" {
			limit := maxBytes
			if limit <= 0 {
				limit = DefaultAssetBytes
			}
			if int64(len(wire.B64JSON)) > limit*4/3+4 {
				return nil, "", Usage{}, &Error{
					Family:  FamilyVideo,
					Op:      "generate",
					Kind:    KindUpstream,
					Message: fmt.Sprintf("provider returned an inline video over the %d-byte limit", limit),
				}
			}
			data, err := base64.StdEncoding.DecodeString(wire.B64JSON)
			if err != nil {
				return nil, "", Usage{}, &Error{Family: FamilyVideo, Op: "generate", Kind: KindUpstream, Message: "provider returned invalid base64 video data", Err: err}
			}
			asset.Data = data
		}
		if len(asset.Data) == 0 && asset.URL == "" {
			continue
		}
		assets = append(assets, asset)
	}
	if len(assets) == 0 {
		return nil, "", Usage{}, &Error{Family: FamilyVideo, Op: "generate", Kind: KindUpstream, Message: "provider returned no usable videos"}
	}
	usage := Usage{
		InputTokens:  firstPositive(envelope.Usage.InputTokens, envelope.Usage.PromptTokens),
		OutputTokens: envelope.Usage.OutputTokens,
	}
	return assets, envelope.Model, usage, nil
}
