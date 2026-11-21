package mediaadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/layerfs"
)

// SpeechTTSToolArgs is the model-facing speech_tts input. Model/backend
// selection is deployment configuration, like image_generate.
type SpeechTTSToolArgs struct {
	Text     string `json:"text"`
	Voice    string `json:"voice,omitempty"`
	Language string `json:"language,omitempty"`
	Format   string `json:"format,omitempty"`
}

// SpeechTTSToolOutput is the success envelope. It carries a durable audio
// reference and objective usage only.
type SpeechTTSToolOutput struct {
	Success bool     `json:"success"`
	Model   string   `json:"model,omitempty"`
	Audio   Artifact `json:"audio"`
	Usage   Usage    `json:"usage"`
}

// SpeechTTSTool exposes text-to-speech as a product tool. The adapter writes
// the audio to layerfs; the model sees a reference, never audio bytes.
type SpeechTTSTool struct {
	Adapter *SpeechAdapter
}

// NewSpeechTTSTool returns the speech_tts tool.
func NewSpeechTTSTool(adapter *SpeechAdapter) *SpeechTTSTool {
	return &SpeechTTSTool{Adapter: adapter}
}

// Name implements agentcore.AgentTool.
func (t *SpeechTTSTool) Name() string { return "speech_tts" }

// Description implements agentcore.AgentTool.
func (t *SpeechTTSTool) Description() string {
	return "Synthesize speech from text. The backend, model and voice allowlist are configured by the deployment. " +
		"Returns a durable audio reference (blobId), never inline audio bytes."
}

// Schema implements agentcore.AgentTool.
func (t *SpeechTTSTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "text": {"type": "string", "minLength": 1, "maxLength": 10000, "description": "The text to speak."},
	    "voice": {"type": "string", "description": "Provider voice id, when the deployment supports more than the default."},
	    "language": {"type": "string", "description": "BCP-47 language tag, when the provider supports an explicit language."},
	    "format": {"type": "string", "description": "Requested audio format (for example wav or mp3); the provider may choose its default."}
	  },
	  "required": ["text"],
	  "additionalProperties": false
	}`)
}

// ExecutionMode implements agentcore.AgentTool.
func (t *SpeechTTSTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute implements agentcore.AgentTool.
func (t *SpeechTTSTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (result agentcore.AgentToolResult, err error) {
	defer recoverToolPanic(FamilyTTS, &result, &err)
	var input SpeechTTSToolArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return toolFailure("arguments are not valid JSON: "+err.Error(), ToolCodeInvalidArguments, false, nil, nil), nil
	}
	echo := map[string]any{
		"text":     input.Text,
		"voice":    input.Voice,
		"language": input.Language,
		"format":   input.Format,
	}
	session, ok := artifactSession(ctx)
	if !ok {
		return toolFailure("the speech adapter has no session for artifact retention",
			ToolCodeSessionMissing, false, nil, echo), nil
	}
	if t.Adapter == nil {
		return toolFailure("speech provider is not configured", string(KindConfig), false, nil, echo), nil
	}
	speech, err := t.Adapter.Synthesize(ctx, session, SynthesizeRequest{
		Text:     input.Text,
		Voice:    input.Voice,
		Language: input.Language,
		Format:   input.Format,
	})
	if err != nil {
		return toolFailureFromError(FamilyTTS, err, echo), nil
	}
	output := SpeechTTSToolOutput{Success: true, Model: speech.Model, Audio: speech.Audio, Usage: speech.Usage}
	raw, _ := json.Marshal(output)
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: output,
		Details:           mediaDetails(FamilyTTS, output.Usage, output.Audio),
	}, nil
}

// SpeechSTTToolArgs is the model-facing speech_stt input. The audio is named by
// a layerfs reference (normally produced by speech_tts or a file tool), never
// inlined into the model context.
type SpeechSTTToolArgs struct {
	BlobID   string `json:"blob_id"`
	Filename string `json:"filename,omitempty"`
	Mime     string `json:"mime,omitempty"`
	Language string `json:"language,omitempty"`
}

// SpeechSTTToolOutput is the success envelope: the transcript (with the
// provider's segments, if any) and the durable input-audio reference.
type SpeechSTTToolOutput struct {
	Success    bool             `json:"success"`
	Audio      *Artifact        `json:"audio,omitempty"`
	Transcript SpeechTranscript `json:"transcript"`
}

// SpeechSTTTool exposes speech-to-text as a product tool. It reads the input
// bytes from layerfs, persists the same bytes as a transcript-provenance
// reference, and returns transcript text/segments only.
type SpeechSTTTool struct {
	Adapter *SpeechAdapter
	Store   *layerfs.Store
	// MaxAudioBytes bounds one input object. Zero uses DefaultAudioBytes.
	MaxAudioBytes int64
}

// NewSpeechSTTTool returns the speech_stt tool.
func NewSpeechSTTTool(adapter *SpeechAdapter, store *layerfs.Store, maxAudioBytes int64) *SpeechSTTTool {
	return &SpeechSTTTool{Adapter: adapter, Store: store, MaxAudioBytes: maxAudioBytes}
}

// Name implements agentcore.AgentTool.
func (t *SpeechSTTTool) Name() string { return "speech_stt" }

// Description implements agentcore.AgentTool.
func (t *SpeechSTTTool) Description() string {
	return "Transcribe a layerfs audio reference into text, with provider timestamps when available. " +
		"Returns the transcript, optional segments and the durable audio reference; never inline audio."
}

// Schema implements agentcore.AgentTool.
func (t *SpeechSTTTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "blob_id": {"type": "string", "description": "Layerfs audio reference (sha256:...), normally returned by speech_tts."},
	    "filename": {"type": "string", "description": "Original filename, for providers that infer the container from it."},
	    "mime": {"type": "string", "description": "Audio MIME type, for example audio/wav or audio/webm."},
	    "language": {"type": "string", "description": "BCP-47 language hint, when the provider supports it."}
	  },
	  "required": ["blob_id"],
	  "additionalProperties": false
	}`)
}

// ExecutionMode implements agentcore.AgentTool.
func (t *SpeechSTTTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute implements agentcore.AgentTool.
func (t *SpeechSTTTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (result agentcore.AgentToolResult, err error) {
	defer recoverToolPanic(FamilySTT, &result, &err)
	var input SpeechSTTToolArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return toolFailure("arguments are not valid JSON: "+err.Error(), ToolCodeInvalidArguments, false, nil, nil), nil
	}
	echo := map[string]any{
		"blob_id":  input.BlobID,
		"filename": input.Filename,
		"mime":     input.Mime,
		"language": input.Language,
	}
	session, ok := artifactSession(ctx)
	if !ok {
		return toolFailure("the speech adapter has no session for artifact retention",
			ToolCodeSessionMissing, false, nil, echo), nil
	}
	if t.Adapter == nil || t.Store == nil {
		return toolFailure("speech transcription is not configured", string(KindConfig), false, nil, echo), nil
	}
	hash, parseErr := layerfs.ParseHash(input.BlobID)
	if parseErr != nil {
		return toolFailure("blob_id is not a layerfs audio reference: "+parseErr.Error(),
			ToolCodeInvalidArguments, false, nil, echo), nil
	}
	limit := t.MaxAudioBytes
	if limit <= 0 {
		limit = DefaultAudioBytes
	}
	object, openErr := t.Store.Open(hash)
	if openErr != nil {
		return toolFailure(fmt.Sprintf("audio object %s cannot be opened: %v", hash, openErr),
			ToolCodeInvalidArguments, false, nil, echo), nil
	}
	data, readErr := io.ReadAll(io.LimitReader(object, limit+1))
	closeErr := object.Close()
	if readErr != nil {
		return toolFailure("audio object could not be read: "+readErr.Error(),
			string(KindArtifact), true, nil, echo), nil
	}
	if closeErr != nil {
		return toolFailure("audio object could not be closed: "+closeErr.Error(),
			string(KindArtifact), true, nil, echo), nil
	}
	if int64(len(data)) > limit {
		return toolFailure(fmt.Sprintf("audio exceeds the %d-byte limit", limit),
			ToolCodeInvalidArguments, false, nil, echo), nil
	}
	transcript, transcribeErr := t.Adapter.Transcribe(ctx, session, TranscribeRequest{
		Filename: input.Filename,
		Mime:     input.Mime,
		Language: input.Language,
		Audio:    data,
	})
	if transcribeErr != nil {
		return toolFailureFromError(FamilySTT, transcribeErr, echo), nil
	}
	output := SpeechSTTToolOutput{Success: true, Audio: transcript.Audio, Transcript: transcript.Transcript}
	raw, _ := json.Marshal(output)
	var audio []Artifact
	if output.Audio != nil {
		audio = append(audio, *output.Audio)
	}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: output,
		Details:           mediaDetails(FamilySTT, output.Transcript.Usage, audio...),
	}, nil
}

var (
	_ agentcore.AgentTool = (*SpeechTTSTool)(nil)
	_ agentcore.AgentTool = (*SpeechSTTTool)(nil)
)
