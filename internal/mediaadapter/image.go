package mediaadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ImageRequest is one image generation (or reference-guided edit) call. Model
// is provider configuration, not a model-chosen value: the Hermes image tool
// shape keeps model selection with the user's configured backend.
type ImageRequest struct {
	Model              string
	Prompt             string
	NegativePrompt     string
	Size               string
	N                  int
	ImageURL           string
	ReferenceImageURLs []string
}

// ProviderAsset is one asset a provider returned: exactly one of Data and URL
// is set. The adapter persists the bytes; a URL is fetched by the adapter, not
// handed to the model.
type ProviderAsset struct {
	Data           []byte
	URL            string
	Mime           string
	Width          int
	Height         int
	DurationMillis int64
}

// ImageBatch is a provider response before persistence.
type ImageBatch struct {
	Model  string
	Assets []ProviderAsset
	Usage  Usage
}

// ImageProvider is the capability interface an image backend implements.
type ImageProvider interface {
	GenerateImage(ctx context.Context, req ImageRequest) (ImageBatch, error)
}

// ImageResult is the adapter result: references only.
type ImageResult struct {
	Model  string     `json:"model"`
	Images []Artifact `json:"images"`
	Usage  Usage      `json:"usage"`
}

// ImageAdapter validates a call, invokes the provider behind the panic
// boundary, persists every asset and counts objective usage.
type ImageAdapter struct {
	Provider      ImageProvider
	Store         ArtifactStore
	Fetcher       AssetFetcher
	MaxAssetBytes int64
}

// MaxImagesPerCall bounds one tool call. Providers may cap lower; this is the
// harness-side ceiling.
const MaxImagesPerCall = 8

// MaxReferenceImages bounds reference inputs, matching the Hermes surface.
const MaxReferenceImages = 8

// Generate implements the image family.
func (a *ImageAdapter) Generate(ctx context.Context, session string, req ImageRequest) (ImageResult, error) {
	if a == nil || a.Provider == nil {
		return ImageResult{}, configError(FamilyImage, "generate", "image provider is not configured")
	}
	if a.Store == nil {
		return ImageResult{}, configError(FamilyImage, "generate", "artifact store is not configured")
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return ImageResult{}, requestError(FamilyImage, "generate", "prompt is required", []FieldError{{
			Field: "prompt", Message: "prompt is required",
		}})
	}
	if req.N == 0 {
		req.N = 1
	}
	if req.N < 1 || req.N > MaxImagesPerCall {
		return ImageResult{}, requestError(FamilyImage, "generate",
			fmt.Sprintf("n must be between 1 and %d", MaxImagesPerCall), []FieldError{{
				Field: "n", Message: fmt.Sprintf("must be between 1 and %d", MaxImagesPerCall),
			}})
	}
	if len(req.ReferenceImageURLs) > MaxReferenceImages {
		return ImageResult{}, requestError(FamilyImage, "generate",
			fmt.Sprintf("at most %d reference images are accepted", MaxReferenceImages), []FieldError{{
				Field:   "reference_image_urls",
				Message: fmt.Sprintf("at most %d reference images are accepted", MaxReferenceImages),
			}})
	}
	if strings.TrimSpace(req.Size) != "" && !validSize(req.Size) {
		return ImageResult{}, requestError(FamilyImage, "generate",
			"size must look like WIDTHxHEIGHT (for example 1024x1024)", []FieldError{{
				Field: "size", Message: "expected WIDTHxHEIGHT",
			}})
	}

	batch, err := guard(FamilyImage, "generate", func() (ImageBatch, error) {
		return a.Provider.GenerateImage(ctx, req)
	})
	if err != nil {
		return ImageResult{}, providerError(FamilyImage, "generate", err)
	}
	if len(batch.Assets) == 0 {
		return ImageResult{}, &Error{
			Family:  FamilyImage,
			Op:      "generate",
			Kind:    KindUpstream,
			Message: "provider returned no images",
		}
	}

	result := ImageResult{Model: firstNonEmpty(batch.Model, req.Model), Usage: batch.Usage}
	for _, asset := range batch.Assets {
		data, mime, err := a.assetBytes(ctx, asset, FamilyImage)
		if err != nil {
			return ImageResult{}, err
		}
		artifact, err := a.Store.Put(ctx, ArtifactInput{
			Session: session,
			Kind:    ArtifactImage,
			Mime:    firstNonEmpty(asset.Mime, mime),
			Data:    data,
			Width:   asset.Width,
			Height:  asset.Height,
		})
		if err != nil {
			var adapterErr *Error
			if ok := asAdapterError(err, &adapterErr); ok {
				return ImageResult{}, adapterErr
			}
			return ImageResult{}, artifactError(FamilyImage, "store", err)
		}
		result.Images = append(result.Images, artifact)
		result.Usage.Images++
		result.Usage.Bytes += artifact.Size
	}
	return result, nil
}

func (a *ImageAdapter) assetBytes(ctx context.Context, asset ProviderAsset, family string) ([]byte, string, error) {
	switch {
	case len(asset.Data) > 0:
		if err := a.checkAssetSize(len(asset.Data)); err != nil {
			return nil, "", err
		}
		return asset.Data, asset.Mime, nil
	case strings.TrimSpace(asset.URL) != "":
		if a.Fetcher == nil {
			return nil, "", &Error{
				Family:  family,
				Op:      "fetch",
				Kind:    KindConfig,
				Message: "provider returned an asset URL but no asset fetcher is configured",
			}
		}
		fetched, err := a.Fetcher.Fetch(ctx, asset.URL)
		if err != nil {
			return nil, "", providerError(family, "fetch", err)
		}
		if err := a.checkAssetSize(len(fetched.Data)); err != nil {
			return nil, "", err
		}
		return fetched.Data, fetched.Mime, nil
	default:
		return nil, "", &Error{
			Family:  family,
			Op:      "generate",
			Kind:    KindUpstream,
			Message: "provider returned an asset with no bytes and no URL",
		}
	}
}

func (a *ImageAdapter) checkAssetSize(size int) error {
	limit := a.MaxAssetBytes
	if limit <= 0 {
		limit = DefaultAssetBytes
	}
	if int64(size) > limit {
		return &Error{
			Family:  FamilyImage,
			Op:      "store",
			Kind:    KindUpstream,
			Message: fmt.Sprintf("generated asset exceeds the %d-byte limit", limit),
		}
	}
	return nil
}

func validSize(size string) bool {
	size = strings.TrimSpace(size)
	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func asAdapterError(err error, target **Error) bool {
	if err == nil {
		return false
	}
	if typed, ok := err.(*Error); ok {
		*target = typed
		return true
	}
	return false
}

// HTTPImageProvider is the OpenAI-images-compatible wire client. The wire
// accepts the Hermes-shaped optional fields (negative prompt, references) and
// both asset forms providers return: an inline b64_json or a URL the adapter
// fetches.
type HTTPImageProvider struct {
	wire  *wireClient
	model string
	path  string

	name string
}

// HTTPImageConfig configures one image endpoint.
type HTTPImageConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	// Path defaults to /images/generations.
	Path string
	// Client and MaxResponseBytes are optional overrides.
	Client           *http.Client
	MaxResponseBytes int64
}

// NewHTTPImageProvider validates and builds one image wire client.
func NewHTTPImageProvider(cfg HTTPImageConfig) (*HTTPImageProvider, error) {
	wire, err := newWireClient(cfg.BaseURL, cfg.APIKey, cfg.Client, cfg.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		path = "/images/generations"
	}
	return &HTTPImageProvider{
		wire:  wire,
		model: strings.TrimSpace(cfg.Model),
		path:  path,
		name:  "http-images",
	}, nil
}

var _ ImageProvider = (*HTTPImageProvider)(nil)

// Name reports the wire family, for diagnostics.
func (p *HTTPImageProvider) Name() string { return p.name }

type imageWireRequest struct {
	Model              string   `json:"model,omitempty"`
	Prompt             string   `json:"prompt"`
	N                  int      `json:"n,omitempty"`
	Size               string   `json:"size,omitempty"`
	NegativePrompt     string   `json:"negative_prompt,omitempty"`
	ImageURL           string   `json:"image_url,omitempty"`
	ReferenceImageURLs []string `json:"reference_image_urls,omitempty"`
}

type imageWireAsset struct {
	URL      string `json:"url"`
	B64JSON  string `json:"b64_json"`
	Mime     string `json:"mime_type"`
	MimeAlt  string `json:"mime"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Duration int64  `json:"duration_ms"`
}

type imageWireResponse struct {
	Model  string            `json:"model"`
	Data   []json.RawMessage `json:"data"`
	Images []json.RawMessage `json:"images"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// GenerateImage implements ImageProvider.
func (p *HTTPImageProvider) GenerateImage(ctx context.Context, req ImageRequest) (ImageBatch, error) {
	if p == nil || p.wire == nil {
		return ImageBatch{}, configError(FamilyImage, "generate", "image wire is not configured")
	}
	payload := imageWireRequest{
		Model:              firstNonEmpty(req.Model, p.model),
		Prompt:             strings.TrimSpace(req.Prompt),
		N:                  req.N,
		Size:               strings.TrimSpace(req.Size),
		NegativePrompt:     strings.TrimSpace(req.NegativePrompt),
		ImageURL:           strings.TrimSpace(req.ImageURL),
		ReferenceImageURLs: req.ReferenceImageURLs,
	}
	raw, _, err := p.wire.postJSON(ctx, FamilyImage, "generate", p.path, payload, nil)
	if err != nil {
		return ImageBatch{}, err
	}
	assets, err := parseImageAssets(raw, p.wire.maxResponse)
	if err != nil {
		return ImageBatch{}, err
	}
	var decoded imageWireResponse
	_ = json.Unmarshal(raw, &decoded)
	batch := ImageBatch{
		Model:  firstNonEmpty(decoded.Model, payload.Model),
		Assets: assets,
		Usage: Usage{
			InputTokens:  firstPositive(decoded.Usage.InputTokens, decoded.Usage.PromptTokens),
			OutputTokens: decoded.Usage.OutputTokens,
		},
	}
	return batch, nil
}

// parseImageAssets accepts either an OpenAI images envelope (data[] entries
// with url/b64_json) or a Hermes-shaped images[] list (strings or objects).
func parseImageAssets(raw []byte, maxBytes int64) ([]ProviderAsset, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, &Error{Family: FamilyImage, Op: "generate", Kind: KindUpstream, Message: "provider returned an empty body"}
	}
	if strings.HasPrefix(trimmed, "\"") || strings.HasPrefix(trimmed, "[") {
		// A bare JSON string (one URL) or a bare array of assets.
		var asString string
		if json.Unmarshal(raw, &asString) == nil {
			return []ProviderAsset{{URL: asString}}, nil
		}
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, &Error{Family: FamilyImage, Op: "generate", Kind: KindUpstream, Message: "provider returned invalid JSON", Err: err}
		}
		return parseImageAssetList(list, maxBytes)
	}
	var envelope imageWireResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &Error{Family: FamilyImage, Op: "generate", Kind: KindUpstream, Message: "provider returned invalid JSON", Err: err}
	}
	list := envelope.Data
	if len(list) == 0 {
		list = envelope.Images
	}
	if len(list) == 0 {
		return nil, &Error{Family: FamilyImage, Op: "generate", Kind: KindUpstream, Message: "provider returned no images"}
	}
	return parseImageAssetList(list, maxBytes)
}

func parseImageAssetList(list []json.RawMessage, maxBytes int64) ([]ProviderAsset, error) {
	assets := make([]ProviderAsset, 0, len(list))
	for _, item := range list {
		var asString string
		if json.Unmarshal(item, &asString) == nil {
			if strings.TrimSpace(asString) == "" {
				continue
			}
			assets = append(assets, ProviderAsset{URL: asString})
			continue
		}
		var wire imageWireAsset
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
				return nil, &Error{
					Family:  FamilyImage,
					Op:      "generate",
					Kind:    KindUpstream,
					Message: fmt.Sprintf("provider returned an inline image over the %d-byte limit", limit),
				}
			}
			data, err := base64.StdEncoding.DecodeString(wire.B64JSON)
			if err != nil {
				return nil, &Error{Family: FamilyImage, Op: "generate", Kind: KindUpstream, Message: "provider returned invalid base64 image data", Err: err}
			}
			asset.Data = data
		}
		if len(asset.Data) == 0 && asset.URL == "" {
			continue
		}
		assets = append(assets, asset)
	}
	if len(assets) == 0 {
		return nil, &Error{Family: FamilyImage, Op: "generate", Kind: KindUpstream, Message: "provider returned no usable images"}
	}
	return assets, nil
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
