// Package mediawiring assembles internal/mediaadapter into the product runtime.
//
// # What this package owns
//
// mediaadapter is a library: it takes an injected provider, endpoint, credential
// and artifact store. This package is the seam that reads the tenant's model
// configuration (the same modelcfg resolution the llm service runs), resolves a
// media provider's credential with the same ordering an attempt uses, builds the
// per-family adapter, and hands the model-facing tools back to the host
// assembly. cmd/keel passes Build as servicehost.ProductOptions.MediaTools so
// the tools are rebuilt with every generation (a settings write that adds or
// changes a media provider applies to the next run).
//
// # Families
//
// image and video are wired as the image_generate and video_generate tools. The
// tool name is the capability; the backend/model is deployment configuration
// and never a model-chosen argument.
//
// TTS and STT are wired as speech_tts and speech_stt. TTS returns a layerfs
// audio reference. STT resolves a layerfs input reference, sends the bytes to
// the tenant's transcription provider, persists the same bytes as provenance
// and returns transcript/segments plus the reference. Neither tool inlines
// audio. The human-facing demo routes under /v1/media/audio remain a separate
// UI/demo surface, not the tenant runtime.
//
// The decision family has two surfaces. ValidateModels runs the DecisionClient
// Preflight over a decision provider's saved binding, so an unsupported question
// type, an over-limit rubric, a remote image URL or an invalid policy is a field
// error before it can reach a run. At runtime decision_ask executes the saved
// binding (or a call-supplied state/questions) with the binding's policy; the
// model cannot switch enforce to shadow or fail-open. It never calls a provider
// during validation and never fabricates an answer.
//
// # Failure shape
//
// A configured family with no usable credential still gets its tool, backed by
// a provider that fails with the typed missing_credentials error: the model can
// call the capability and sees a retryable/typed failure instead of a silently
// missing tool, and no unauthenticated request leaves the process. Missing
// channel configuration (no base URL/model/artifact store) omits the tool and
// records a provider-scoped issue for the settings write path.
//
// # Media references
//
// Successful media tools carry a details.media payload naming layerfs
// references and objective usage. This reuses the existing tool.result event;
// no media-specific kernel kind is required. The payload has no price field,
// and kernel blob_refs only counts platform UUID blobs, so a layerfs sha256
// reference stays owned by layerfs retention.
package mediawiring
