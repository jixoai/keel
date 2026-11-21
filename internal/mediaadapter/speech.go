package mediaadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SynthesizeRequest is text-to-speech input. Voice and Language are passed to
// the provider as configuration; the adapter never claims to honor a voice the
// backend ignores.
type SynthesizeRequest struct {
	Model    string
	Text     string
	Voice    string
	Language string
	Format   string
}

// SpeechAudio is synthesized audio before persistence.
type SpeechAudio struct {
	Data           []byte
	Mime           string
	Model          string
	DurationMillis int64
}

// TranscribeRequest is speech-to-text input. Audio is the raw bytes the caller
// already resolved (a layerfs object or an upload); it is never a remote URL.
type TranscribeRequest struct {
	Model    string
	Filename string
	Mime     string
	Language string
	Audio    []byte
}

// TranscriptSegment is one timestamped span. Times are milliseconds from the
// start of the audio.
type TranscriptSegment struct {
	StartMillis int64  `json:"startMillis"`
	EndMillis   int64  `json:"endMillis"`
	Text        string `json:"text"`
}

// SpeechTranscript is a transcription result.
type SpeechTranscript struct {
	Text           string              `json:"text"`
	Language       string              `json:"language,omitempty"`
	Model          string              `json:"model,omitempty"`
	DurationMillis int64               `json:"durationMillis,omitempty"`
	Segments       []TranscriptSegment `json:"segments,omitempty"`
	Usage          Usage               `json:"usage"`
}

// SpeechProvider is the capability interface a speech backend implements.
type SpeechProvider interface {
	Synthesize(ctx context.Context, req SynthesizeRequest) (SpeechAudio, error)
	Transcribe(ctx context.Context, req TranscribeRequest) (SpeechTranscript, error)
}

// SpeechResult is a synthesis result: one durable audio reference plus usage.
type SpeechResult struct {
	Model string   `json:"model"`
	Audio Artifact `json:"audio"`
	Usage Usage    `json:"usage"`
}

// TranscriptResult is a transcription result. Audio is the durable reference
// for the input bytes when Store is configured; it gives the ledger provenance
// without inlining the recording.
type TranscriptResult struct {
	Audio      *Artifact        `json:"audio,omitempty"`
	Transcript SpeechTranscript `json:"transcript"`
}

// SpeechAdapter validates a call, invokes the provider behind the panic
// boundary, persists audio and counts objective usage.
type SpeechAdapter struct {
	Provider     SpeechProvider
	Store        ArtifactStore
	MaxAudioSize int64
}

// MaxSpeechCharacters bounds one synthesis request. Providers may cap lower.
const MaxSpeechCharacters = 10000

// DefaultAudioBytes bounds one transcription upload (64 MiB).
const DefaultAudioBytes = 64 << 20

// Synthesize implements text-to-speech.
func (a *SpeechAdapter) Synthesize(ctx context.Context, session string, req SynthesizeRequest) (SpeechResult, error) {
	if a == nil || a.Provider == nil {
		return SpeechResult{}, configError(FamilyTTS, "synthesize", "speech provider is not configured")
	}
	if a.Store == nil {
		return SpeechResult{}, configError(FamilyTTS, "synthesize", "artifact store is not configured")
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		return SpeechResult{}, requestError(FamilyTTS, "synthesize", "text is required", []FieldError{{
			Field: "text", Message: "text is required",
		}})
	}
	if utf8.RuneCountInString(req.Text) > MaxSpeechCharacters {
		return SpeechResult{}, requestError(FamilyTTS, "synthesize",
			fmt.Sprintf("text exceeds %d characters", MaxSpeechCharacters), []FieldError{{
				Field: "text", Message: fmt.Sprintf("at most %d characters", MaxSpeechCharacters),
			}})
	}
	audio, err := guard(FamilyTTS, "synthesize", func() (SpeechAudio, error) {
		return a.Provider.Synthesize(ctx, req)
	})
	if err != nil {
		return SpeechResult{}, providerError(FamilyTTS, "synthesize", err)
	}
	if len(audio.Data) == 0 {
		return SpeechResult{}, &Error{Family: FamilyTTS, Op: "synthesize", Kind: KindUpstream, Message: "provider returned no audio"}
	}
	limit := a.MaxAudioSize
	if limit <= 0 {
		limit = DefaultAudioBytes
	}
	if int64(len(audio.Data)) > limit {
		return SpeechResult{}, &Error{
			Family:  FamilyTTS,
			Op:      "synthesize",
			Kind:    KindUpstream,
			Message: fmt.Sprintf("provider audio exceeds the %d-byte limit", limit),
		}
	}
	artifact, err := a.Store.Put(ctx, ArtifactInput{
		Session:        session,
		Kind:           ArtifactAudio,
		Mime:           audio.Mime,
		Data:           audio.Data,
		DurationMillis: audio.DurationMillis,
	})
	if err != nil {
		var adapterErr *Error
		if asAdapterError(err, &adapterErr) {
			return SpeechResult{}, adapterErr
		}
		return SpeechResult{}, artifactError(FamilyTTS, "store", err)
	}
	return SpeechResult{
		Model: firstNonEmpty(audio.Model, req.Model),
		Audio: artifact,
		Usage: Usage{
			InputCharacters: utf8.RuneCountInString(req.Text),
			AudioBytes:      artifact.Size,
			AudioMillis:     audio.DurationMillis,
		},
	}, nil
}

// Transcribe implements speech-to-text.
func (a *SpeechAdapter) Transcribe(ctx context.Context, session string, req TranscribeRequest) (TranscriptResult, error) {
	if a == nil || a.Provider == nil {
		return TranscriptResult{}, configError(FamilySTT, "transcribe", "speech provider is not configured")
	}
	if len(req.Audio) == 0 {
		return TranscriptResult{}, requestError(FamilySTT, "transcribe", "audio is required", []FieldError{{
			Field: "audio", Message: "audio bytes are required",
		}})
	}
	limit := a.MaxAudioSize
	if limit <= 0 {
		limit = DefaultAudioBytes
	}
	if int64(len(req.Audio)) > limit {
		return TranscriptResult{}, requestError(FamilySTT, "transcribe",
			fmt.Sprintf("audio exceeds the %d-byte limit", limit), []FieldError{{
				Field: "audio", Message: fmt.Sprintf("at most %d bytes", limit),
			}})
	}
	transcript, err := guard(FamilySTT, "transcribe", func() (SpeechTranscript, error) {
		return a.Provider.Transcribe(ctx, req)
	})
	if err != nil {
		return TranscriptResult{}, providerError(FamilySTT, "transcribe", err)
	}
	transcript.Text = strings.TrimSpace(transcript.Text)
	if transcript.Text == "" {
		return TranscriptResult{}, &Error{Family: FamilySTT, Op: "transcribe", Kind: KindUpstream, Message: "provider returned an empty transcript"}
	}
	result := TranscriptResult{Transcript: transcript}
	usage := transcript.Usage
	usage.TranscriptCharacters = utf8.RuneCountInString(transcript.Text)
	usage.AudioBytes = int64(len(req.Audio))
	usage.AudioMillis = transcript.DurationMillis
	result.Transcript.Usage = usage
	if a.Store != nil {
		artifact, err := a.Store.Put(ctx, ArtifactInput{
			Session:        session,
			Kind:           ArtifactAudio,
			Mime:           req.Mime,
			Data:           req.Audio,
			DurationMillis: transcript.DurationMillis,
			Name:           req.Filename,
		})
		if err != nil {
			var adapterErr *Error
			if asAdapterError(err, &adapterErr) {
				return TranscriptResult{}, adapterErr
			}
			return TranscriptResult{}, artifactError(FamilySTT, "store", err)
		}
		result.Audio = &artifact
	}
	return result, nil
}

// HTTPSpeechProvider is the OpenAI-compatible audio wire client used by both
// speech families.
type HTTPSpeechProvider struct {
	wire           *wireClient
	model          string
	speechPath     string
	transcribePath string
	name           string
}

// HTTPSpeechConfig configures one speech endpoint.
type HTTPSpeechConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	// SpeechPath defaults to /audio/speech; TranscribePath to
	// /audio/transcriptions.
	SpeechPath       string
	TranscribePath   string
	Client           *http.Client
	MaxResponseBytes int64
}

// NewHTTPSpeechProvider validates and builds one speech wire client.
func NewHTTPSpeechProvider(cfg HTTPSpeechConfig) (*HTTPSpeechProvider, error) {
	wire, err := newWireClient(cfg.BaseURL, cfg.APIKey, cfg.Client, cfg.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	speechPath := strings.TrimSpace(cfg.SpeechPath)
	if speechPath == "" {
		speechPath = "/audio/speech"
	}
	transcribePath := strings.TrimSpace(cfg.TranscribePath)
	if transcribePath == "" {
		transcribePath = "/audio/transcriptions"
	}
	return &HTTPSpeechProvider{
		wire:           wire,
		model:          strings.TrimSpace(cfg.Model),
		speechPath:     speechPath,
		transcribePath: transcribePath,
		name:           "http-speech",
	}, nil
}

var _ SpeechProvider = (*HTTPSpeechProvider)(nil)

// Name reports the wire family, for diagnostics.
func (p *HTTPSpeechProvider) Name() string { return p.name }

type speechWireRequest struct {
	Model          string `json:"model,omitempty"`
	Input          string `json:"input"`
	Voice          string `json:"voice,omitempty"`
	Language       string `json:"language,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

// Synthesize implements SpeechProvider.
func (p *HTTPSpeechProvider) Synthesize(ctx context.Context, req SynthesizeRequest) (SpeechAudio, error) {
	if p == nil || p.wire == nil {
		return SpeechAudio{}, configError(FamilyTTS, "synthesize", "speech wire is not configured")
	}
	payload := speechWireRequest{
		Model:          firstNonEmpty(req.Model, p.model),
		Input:          strings.TrimSpace(req.Text),
		Voice:          strings.TrimSpace(req.Voice),
		Language:       strings.TrimSpace(req.Language),
		ResponseFormat: strings.TrimSpace(req.Format),
	}
	endpoint, err := p.wire.endpoint(p.speechPath)
	if err != nil {
		return SpeechAudio{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return SpeechAudio{}, &Error{Family: FamilyTTS, Op: "synthesize", Kind: KindRequest, Message: "request is not encodable"}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return SpeechAudio{}, &Error{Family: FamilyTTS, Op: "synthesize", Kind: KindConfig, Message: "provider request could not be built", Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "audio/*")
	if p.wire.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.wire.apiKey)
	}
	resp, err := p.wire.client.Do(httpReq)
	if err != nil {
		return SpeechAudio{}, providerError(FamilyTTS, "synthesize", err)
	}
	defer func() { _ = resp.Body.Close() }()
	limit := int64(DefaultAudioBytes)
	if p.wire.maxResponse > limit {
		limit = p.wire.maxResponse
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return SpeechAudio{}, providerError(FamilyTTS, "synthesize", err)
	}
	if int64(len(raw)) > limit {
		return SpeechAudio{}, &Error{
			Family: FamilyTTS, Op: "synthesize", Kind: KindUpstream,
			Message: fmt.Sprintf("provider audio exceeds the %d-byte limit", limit),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return SpeechAudio{}, upstreamError(FamilyTTS, "synthesize", resp.StatusCode, upstreamMessage(raw), p.wire.apiKey)
	}
	if len(raw) == 0 {
		return SpeechAudio{}, &Error{Family: FamilyTTS, Op: "synthesize", Kind: KindUpstream, Message: "provider returned no audio"}
	}
	mime := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if mime == "" {
		mime = http.DetectContentType(raw)
	}
	duration := int64(0)
	if value := strings.TrimSpace(resp.Header.Get("X-Audio-Duration-Ms")); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed >= 0 {
			duration = parsed
		}
	}
	return SpeechAudio{
		Data:           raw,
		Mime:           mime,
		Model:          firstNonEmpty(payload.Model, p.model),
		DurationMillis: duration,
	}, nil
}

// Transcribe implements SpeechProvider.
func (p *HTTPSpeechProvider) Transcribe(ctx context.Context, req TranscribeRequest) (SpeechTranscript, error) {
	if p == nil || p.wire == nil {
		return SpeechTranscript{}, configError(FamilySTT, "transcribe", "speech wire is not configured")
	}
	filename := strings.TrimSpace(req.Filename)
	if filename == "" {
		filename = "audio"
		if ext := extensionForMime(req.Mime); ext != "" {
			filename += ext
		}
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeQuotes(filename)))
	partMime := strings.TrimSpace(req.Mime)
	if partMime == "" {
		partMime = "application/octet-stream"
	}
	header.Set("Content-Type", partMime)
	part, err := writer.CreatePart(header)
	if err != nil {
		return SpeechTranscript{}, &Error{Family: FamilySTT, Op: "transcribe", Kind: KindRequest, Message: "multipart request could not be built", Err: err}
	}
	if _, err := part.Write(req.Audio); err != nil {
		return SpeechTranscript{}, &Error{Family: FamilySTT, Op: "transcribe", Kind: KindRequest, Message: "audio could not be encoded", Err: err}
	}
	fields := map[string]string{
		"model":           firstNonEmpty(req.Model, p.model),
		"language":        strings.TrimSpace(req.Language),
		"response_format": "verbose_json",
	}
	for name, value := range fields {
		if value == "" {
			continue
		}
		if err := writer.WriteField(name, value); err != nil {
			return SpeechTranscript{}, &Error{Family: FamilySTT, Op: "transcribe", Kind: KindRequest, Message: "multipart request could not be built", Err: err}
		}
	}
	if err := writer.Close(); err != nil {
		return SpeechTranscript{}, &Error{Family: FamilySTT, Op: "transcribe", Kind: KindRequest, Message: "multipart request could not be built", Err: err}
	}
	endpoint, err := p.wire.endpoint(p.transcribePath)
	if err != nil {
		return SpeechTranscript{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body.Bytes()))
	if err != nil {
		return SpeechTranscript{}, &Error{Family: FamilySTT, Op: "transcribe", Kind: KindConfig, Message: "provider request could not be built", Err: err}
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	httpReq.Header.Set("Accept", "application/json")
	if p.wire.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.wire.apiKey)
	}
	resp, err := p.wire.client.Do(httpReq)
	if err != nil {
		return SpeechTranscript{}, providerError(FamilySTT, "transcribe", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, p.wire.maxResponse+1))
	if err != nil {
		return SpeechTranscript{}, providerError(FamilySTT, "transcribe", err)
	}
	if int64(len(raw)) > p.wire.maxResponse {
		return SpeechTranscript{}, &Error{Family: FamilySTT, Op: "transcribe", Kind: KindUpstream, Message: "provider response exceeds the response limit"}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return SpeechTranscript{}, upstreamError(FamilySTT, "transcribe", resp.StatusCode, upstreamMessage(raw), p.wire.apiKey)
	}
	return parseTranscript(raw, firstNonEmpty(req.Model, p.model))
}

func parseTranscript(raw []byte, model string) (SpeechTranscript, error) {
	var wire struct {
		Text     string  `json:"text"`
		Language string  `json:"language"`
		Model    string  `json:"model"`
		Duration float64 `json:"duration"`
		Segments []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		} `json:"segments"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		// Some deployments answer plain text; accept it as a transcript with no
		// timestamps rather than inventing segments.
		text := strings.TrimSpace(string(raw))
		if text == "" || strings.HasPrefix(text, "{") {
			return SpeechTranscript{}, &Error{Family: FamilySTT, Op: "transcribe", Kind: KindUpstream, Message: "provider returned invalid JSON", Err: err}
		}
		return SpeechTranscript{Text: text, Model: model}, nil
	}
	transcript := SpeechTranscript{
		Text:           strings.TrimSpace(wire.Text),
		Language:       wire.Language,
		Model:          firstNonEmpty(wire.Model, model),
		DurationMillis: int64(wire.Duration * 1000),
		Usage: Usage{
			InputTokens:  wire.Usage.InputTokens,
			OutputTokens: wire.Usage.OutputTokens,
		},
	}
	for _, segment := range wire.Segments {
		transcript.Segments = append(transcript.Segments, TranscriptSegment{
			StartMillis: int64(segment.Start * 1000),
			EndMillis:   int64(segment.End * 1000),
			Text:        strings.TrimSpace(segment.Text),
		})
	}
	return transcript, nil
}

func extensionForMime(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/ogg", "audio/opus":
		return ".ogg"
	case "audio/webm":
		return ".webm"
	case "audio/flac":
		return ".flac"
	case "audio/mp4", "audio/m4a":
		return ".m4a"
	default:
		return ""
	}
}

func escapeQuotes(value string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, `\"`).Replace(value)
}
