package mediaadapter

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
)

type artifactSessionKey struct{}

// WithArtifactSession attaches the layerfs retention owner to a tool execution
// context. A serving runtime sets this once per tool call from the run scope;
// the adapter never guesses a session.
func WithArtifactSession(ctx context.Context, session string) context.Context {
	return context.WithValue(ctx, artifactSessionKey{}, strings.TrimSpace(session))
}

// ArtifactSessionFromContext reads the ambient session.
func ArtifactSessionFromContext(ctx context.Context) (string, bool) {
	session, ok := ctx.Value(artifactSessionKey{}).(string)
	return session, ok && session != ""
}

// artifactSession resolves the retention owner of one tool call. A serving
// runtime stamps the attempt's (tenant, session) scope on every tool context
// (agentcore.WithToolRunScope); an explicit WithArtifactSession wins for a
// caller outside a run. The adapter never guesses a session.
func artifactSession(ctx context.Context) (string, bool) {
	if session, ok := ArtifactSessionFromContext(ctx); ok {
		return session, true
	}
	if scope, ok := agentcore.ToolRunScopeFromContext(ctx); ok {
		return scope.SessionID, true
	}
	return "", false
}

// Tool error codes. They are stable strings a client can branch on.
const (
	ToolCodeInvalidArguments = "invalid_arguments"
	ToolCodeSessionMissing   = "session_missing"
)

// ToolFailure is the failure envelope shared by the media tools. Request is the
// input echo that makes a retry possible without reconstructing the call; it
// never contains image bytes or credentials.
type ToolFailure struct {
	Success   bool         `json:"success"`
	Error     string       `json:"error"`
	ErrorType string       `json:"error_type"`
	Retryable bool         `json:"retryable"`
	Fields    []FieldError `json:"fields,omitempty"`
	Request   any          `json:"request,omitempty"`
}

var (
	_ agentcore.AgentTool = (*ImageTool)(nil)
	_ agentcore.AgentTool = (*VideoTool)(nil)
)

func boolPtr(value bool) *bool { return &value }

func toolFailure(message, errorType string, retryable bool, fields []FieldError, request any) agentcore.AgentToolResult {
	payload := ToolFailure{
		Success:   false,
		Error:     message,
		ErrorType: errorType,
		Retryable: retryable,
		Fields:    fields,
		Request:   request,
	}
	raw, _ := json.Marshal(payload)
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: payload,
		IsError:           boolPtr(true),
		Details:           map[string]any{"errorType": errorType, "retryable": retryable},
	}
}

func toolFailureFromError(family string, err error, request any) agentcore.AgentToolResult {
	var typed *Error
	if asAdapterError(err, &typed) {
		kind := string(typed.Kind)
		if kind == "" {
			kind = string(KindProviderUnavailable)
		}
		return toolFailure(typed.Error(), kind, typed.Retryable, typed.Fields, request)
	}
	if err != nil {
		return toolFailure(err.Error(), string(KindProviderUnavailable), true, nil, request)
	}
	return toolFailure("media adapter failed", string(KindProviderUnavailable), true, nil, request)
}

// ImageToolArgs is the model-facing image_generate input.
//
// Prompt and aspect_ratio are the Hermes static surface. NegativePrompt, Size,
// N, ImageURL and ReferenceImageURLs are keel extensions: Size is a pixel size
// and wins over aspect_ratio when both are present.
type ImageToolArgs struct {
	Prompt             string   `json:"prompt"`
	AspectRatio        string   `json:"aspect_ratio,omitempty"`
	NegativePrompt     string   `json:"negative_prompt,omitempty"`
	Size               string   `json:"size,omitempty"`
	N                  int      `json:"n,omitempty"`
	ImageURL           string   `json:"image_url,omitempty"`
	ReferenceImageURLs []string `json:"reference_image_urls,omitempty"`
}

// ImageToolOutput is the success envelope. It carries references only.
type ImageToolOutput struct {
	Success bool       `json:"success"`
	Model   string     `json:"model,omitempty"`
	Images  []Artifact `json:"images"`
	Usage   Usage      `json:"usage"`
}

// ImageTool exposes the image adapter under the Hermes tool shape
// `image_generate`: prompt + aspect_ratio are the Hermes static surface; the
// remaining arguments are explicit keel extensions. The backend/model is
// deployment configuration: the model does not choose it.
type ImageTool struct {
	Adapter *ImageAdapter
}

// NewImageTool returns the image_generate tool.
func NewImageTool(adapter *ImageAdapter) *ImageTool { return &ImageTool{Adapter: adapter} }

// Name implements agentcore.AgentTool.
func (t *ImageTool) Name() string { return "image_generate" }

// Description implements agentcore.AgentTool.
func (t *ImageTool) Description() string {
	return "Generate images from a text prompt. The backend and model are configured by the deployment. " +
		"aspect_ratio is landscape, portrait or square; size (WIDTHxHEIGHT), negative prompt, an image-to-image " +
		"source and reference images are keel extensions. Returns durable image references (blobId), never inline bytes."
}

// Schema implements agentcore.AgentTool.
func (t *ImageTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
		    "prompt": {"type": "string", "maxLength": 32768, "description": "The text prompt describing the desired image (or the edit to apply)."},
		    "aspect_ratio": {"type": "string", "enum": ["landscape", "portrait", "square"], "description": "Hermes static aspect ratio; mapped to a WIDTHxHEIGHT size. An explicit size wins."},
		    "negative_prompt": {"type": "string", "description": "Keel extension: content to avoid in the output, when the backend supports it."},
		    "size": {"type": "string", "description": "Keel extension: explicit output size as WIDTHxHEIGHT; wins over aspect_ratio."},
		    "n": {"type": "integer", "minimum": 1, "maximum": 8, "description": "Keel extension: number of images to generate."},
	    "image_url": {"type": "string", "description": "Optional source image for an image-to-image edit (data URL or provider URL)."},
	    "reference_image_urls": {"type": "array", "items": {"type": "string"}, "maxItems": 8, "description": "Optional reference images."}
	  },
	  "required": ["prompt"],
	  "additionalProperties": false
	}`)
}

// ExecutionMode implements agentcore.AgentTool.
func (t *ImageTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute implements agentcore.AgentTool.
func (t *ImageTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var input ImageToolArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return toolFailure("arguments are not valid JSON: "+err.Error(), ToolCodeInvalidArguments, false, nil, nil), nil
	}
	echo := newImageRequestEcho(input)
	session, ok := artifactSession(ctx)
	if !ok {
		return toolFailure("the image adapter has no session for artifact retention",
			ToolCodeSessionMissing, false, nil, echo), nil
	}
	if t.Adapter == nil {
		return toolFailure("image provider is not configured", string(KindConfig), false, nil, echo), nil
	}
	result, err := t.Adapter.Generate(ctx, session, ImageRequest{
		Prompt:             input.Prompt,
		AspectRatio:        input.AspectRatio,
		NegativePrompt:     input.NegativePrompt,
		Size:               input.Size,
		N:                  input.N,
		ImageURL:           input.ImageURL,
		ReferenceImageURLs: input.ReferenceImageURLs,
	})
	if err != nil {
		return toolFailureFromError(FamilyImage, err, echo), nil
	}
	output := ImageToolOutput{Success: true, Model: result.Model, Images: result.Images, Usage: result.Usage}
	raw, _ := json.Marshal(output)
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: output,
	}, nil
}

type imageRequestEcho struct {
	Prompt          string `json:"prompt"`
	AspectRatio     string `json:"aspect_ratio,omitempty"`
	NegativePrompt  string `json:"negative_prompt,omitempty"`
	Size            string `json:"size,omitempty"`
	N               int    `json:"n,omitempty"`
	HasImageURL     bool   `json:"has_image_url,omitempty"`
	ReferenceImages int    `json:"reference_images,omitempty"`
}

func newImageRequestEcho(input ImageToolArgs) imageRequestEcho {
	return imageRequestEcho{
		Prompt:          input.Prompt,
		AspectRatio:     input.AspectRatio,
		NegativePrompt:  input.NegativePrompt,
		Size:            input.Size,
		N:               input.N,
		HasImageURL:     strings.TrimSpace(input.ImageURL) != "",
		ReferenceImages: len(input.ReferenceImageURLs),
	}
}

// VideoToolArgs is the model-facing video_generate input.
type VideoToolArgs struct {
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

// VideoToolOutput is the success envelope. It carries references only.
type VideoToolOutput struct {
	Success bool       `json:"success"`
	Model   string     `json:"model,omitempty"`
	Videos  []Artifact `json:"videos"`
	Usage   Usage      `json:"usage"`
}

// VideoTool exposes the video adapter under the Hermes tool shape
// `video_generate`.
type VideoTool struct {
	Adapter *VideoAdapter
}

// NewVideoTool returns the video_generate tool.
func NewVideoTool(adapter *VideoAdapter) *VideoTool { return &VideoTool{Adapter: adapter} }

// Name implements agentcore.AgentTool.
func (t *VideoTool) Name() string { return "video_generate" }

// Description implements agentcore.AgentTool.
func (t *VideoTool) Description() string {
	return "Generate a video from a text prompt, or animate a still image. The backend and model are configured " +
		"by the deployment. Returns durable video references (blobId), never inline bytes."
}

// Schema implements agentcore.AgentTool.
func (t *VideoTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
		    "prompt": {"type": "string", "maxLength": 32768, "description": "Text instruction describing the desired video, motion, subject and style."},
	    "negative_prompt": {"type": "string", "description": "Content to avoid in the output, when the backend supports it."},
	    "duration": {"type": "integer", "minimum": 0, "maximum": 300, "description": "Desired duration in seconds; providers clamp to their supported range."},
	    "aspect_ratio": {"type": "string", "description": "Output aspect ratio, for example 16:9."},
	    "resolution": {"type": "string", "description": "Output resolution, for example 1080p."},
	    "image_url": {"type": "string", "description": "Optional source image for image-to-video."},
	    "reference_image_urls": {"type": "array", "items": {"type": "string"}, "maxItems": 8, "description": "Optional reference images."},
	    "seed": {"type": "integer", "description": "Reproducibility seed, when supported."},
	    "audio": {"type": "boolean", "description": "Request native audio, when supported."}
	  },
	  "required": ["prompt"],
	  "additionalProperties": false
	}`)
}

// ExecutionMode implements agentcore.AgentTool.
func (t *VideoTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute implements agentcore.AgentTool.
func (t *VideoTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var input VideoToolArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return toolFailure("arguments are not valid JSON: "+err.Error(), ToolCodeInvalidArguments, false, nil, nil), nil
	}
	echo := newVideoRequestEcho(input)
	session, ok := artifactSession(ctx)
	if !ok {
		return toolFailure("the video adapter has no session for artifact retention",
			ToolCodeSessionMissing, false, nil, echo), nil
	}
	if t.Adapter == nil {
		return toolFailure("video provider is not configured", string(KindConfig), false, nil, echo), nil
	}
	result, err := t.Adapter.Generate(ctx, session, VideoRequest{
		Prompt:             input.Prompt,
		NegativePrompt:     input.NegativePrompt,
		DurationSeconds:    input.Duration,
		AspectRatio:        input.AspectRatio,
		Resolution:         input.Resolution,
		ImageURL:           input.ImageURL,
		ReferenceImageURLs: input.ReferenceImageURLs,
		Seed:               input.Seed,
		Audio:              input.Audio,
	})
	if err != nil {
		return toolFailureFromError(FamilyVideo, err, echo), nil
	}
	output := VideoToolOutput{Success: true, Model: result.Model, Videos: result.Videos, Usage: result.Usage}
	raw, _ := json.Marshal(output)
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: output,
	}, nil
}

type videoRequestEcho struct {
	Prompt          string `json:"prompt"`
	NegativePrompt  string `json:"negative_prompt,omitempty"`
	Duration        int    `json:"duration,omitempty"`
	AspectRatio     string `json:"aspect_ratio,omitempty"`
	Resolution      string `json:"resolution,omitempty"`
	HasImageURL     bool   `json:"has_image_url,omitempty"`
	ReferenceImages int    `json:"reference_images,omitempty"`
}

func newVideoRequestEcho(input VideoToolArgs) videoRequestEcho {
	return videoRequestEcho{
		Prompt:          input.Prompt,
		NegativePrompt:  input.NegativePrompt,
		Duration:        input.Duration,
		AspectRatio:     input.AspectRatio,
		Resolution:      input.Resolution,
		HasImageURL:     strings.TrimSpace(input.ImageURL) != "",
		ReferenceImages: len(input.ReferenceImageURLs),
	}
}
