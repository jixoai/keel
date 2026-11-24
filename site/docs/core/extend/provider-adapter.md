# Write a provider adapter

A **provider adapter** is the code that turns Keel's shaped request into one vendor's wire format
and streams the answer back as a single event type. This page documents the seam, how resolution
works today, what adding a provider actually involves, and how attachments (images and documents)
are carried outbound.

## 1. The seam

Everything the loop needs from a model is one function type (`internal/provider/provider.go:128`):

```go
type StreamFn func(ctx context.Context, model string, llm LlmContext, cfg StreamConfig) (*AssistantMessageEventStream, error)
```

A provider implements one interface (`internal/provider/provider_interface.go:65`):

```go
type Provider interface {
    Name() string                                  // identifier, matches Model.Provider
    Models() []Model                               // the models this provider can serve
    StreamCompletion(ctx context.Context, req CompletionRequest) (*AssistantMessageEventStream, error)
}
```

Two failure models coexist deliberately: `StreamCompletion` returns an `error` only for the earliest
"cannot build the stream" case (bad config, no credential); every runtime failure rides the returned
stream as a **terminal error event**. `StreamFnFromProvider` is a straight delegation between the
two shapes (`internal/provider/provider_interface.go:79`).

## 2. How a model gets resolved

The `llm` service builds its `StreamFn` from the tenant document
(`internal/servicehost/services.go:127`):

```go
svc := &llmService{stream: agent.FakeStream(), providerName: "fake", model: "fake"}
if strings.TrimSpace(agentCfg.Model) != "" {
    resolved, _, err := provider.ResolveProvider(agentCfg.Model, agentCfg.BaseURL, agentCfg.Protocol, agentCfg.Provider, lookup)
    svc.stream = provider.StreamFnFromProvider(resolved)
    svc.providerName = resolved.Name()
    svc.model = /* the provider's own model id (models[0].ID) */
}
```

Consequences worth knowing before you write an adapter:

- **An empty model means the deterministic fake provider.** That is what makes the quickstart and
  the hermetic tests possible — the whole path runs without credentials.
- **`ResolveProvider` precedence** (`internal/provider/resolve.go:50`): an explicit `--provider` name
  wins over everything; otherwise an explicit `--protocol` wins over model-id heuristics; otherwise
  presets, prefixes (`ollama/`, `nvidia/`), model-name inference, and finally the reference
  OpenAI-compatible gateway. An unknown protocol value is an **error**, never a silent fallback.
- **Protocols are normalized, not free-form** (`internal/provider/protocol.go:33`): `openai` and
  `openai/chat` collapse to Chat Completions, `openai/resp_api` routes to Responses, `anthropic`
  maps to Messages. The startup banner prints the label so the displayed wire format matches what is
  actually spoken.
- **Credentials are references.** `agent.apiKeyRef` and provider `env`/`headers` values accept
  `${NAME}` / `env:NAME` / `secret:NAME` only; a literal secret (including `Bearer <token>`) is
  rejected and never stored (`docs/HOST.md:61`). The `llm` service resolves references through
  `LLMOptions.LookupEnv` / `LookupSecret`; a `secret:` reference fails closed when the deployment
  has no secret storage, naming the secret instead of falling back to the environment
  (`internal/servicehost/host.go:109`).

Configuration inputs, for reference:

| Variable | Meaning |
|---|---|
| `KEEL_MODEL` | model id; empty = fake provider |
| `KEEL_PROVIDER` | built-in provider name from the registry (wins over protocol) |
| `KEEL_PROTOCOL` | `openai` (`openai/chat`), `openai/resp_api`, `anthropic` |
| `KEEL_BASE_URL` | endpoint prefix (OpenAI-compatible and Anthropic both use it) |
| `KEEL_API_KEY` | credential for the resolved driver |
| `KEEL_MODELS_CONFIG` | the tenant **models document** (providers/rules/selection), the newer path |

## 3. Recipe A — add a built-in registry entry

Most "new provider" requests are really "a new base URL, key variable and auth scheme for an
existing protocol". The registry is a list of specs (`internal/provider/registry.go:72`):

```go
var providerRegistry = []ProviderSpec{
    {
        Name:           "example-vendor",
        EnvVars:        []string{"EXAMPLE_API_KEY"},     // first non-empty wins
        DefaultBaseURL: "https://api.example.com/v1",
        Protocol:       ProtocolOpenAI,                  // or ProtocolAnthropic / responses
        AuthScheme:     AuthBearer,                      // bearer | x-api-key | aws | azure | special
        ExtraHeaders:   map[string]string{"X-Tenant": "keel"}, // optional
        BaseURLEnvVars: []string{"EXAMPLE_BASE_URL"},    // optional overrides, in precedence order
    },
    // … existing entries …
}
```

Then:

1. The provider is selectable by name (`--provider example-vendor`) and by model presets you add to
   the catalog if you want model-id inference.
2. Keep the registry **ordered and deterministic** — `--help` enumerates it.
3. Run the provider tests; the resolution matrix (explicit provider > explicit protocol > presets >
   prefixes > inference > default gateway) is covered there.

## 4. Recipe B — implement a whole protocol

If the vendor does not speak OpenAI Chat/Responses or Anthropic Messages, implement `Provider`:

```go
type myProvider struct { apiKey, baseURL string; client *http.Client }

func (p *myProvider) Name() string   { return "example-vendor" }
func (p *myProvider) Models() []Model { return []Model{{ID: "example-large", Provider: p.Name()} } }

func (p *myProvider) StreamCompletion(ctx context.Context, req CompletionRequest) (*AssistantMessageEventStream, error) {
    stream := provider.NewAssistantMessageEventStream(ctx)   // or the package's shape
    go func() {
        defer stream.Close()
        // 1. shape the request from req.Context (system prompt, messages, tools)
        // 2. parse the vendor's SSE/JSON frames incrementally
        // 3. emit assistant message events (text/thinking/toolCall deltas, then the message)
        // 4. ANY runtime failure becomes a terminal error event, not a returned error
    }()
    return stream, nil
}
```

Rules the rest of the runtime relies on:

- **Stream, do not buffer.** The loop consumes deltas; a provider that returns one whole message is
  still correct but loses token-level UX (partials are a bypass, see
  [Events and protocol](/core/events)).
- **Tool calls are part of the message**, with `id` and `name` on every call — the kernel refuses an
  assistant payload with an unnamed or duplicated call id at commit time
  (`internal/kernel/validate.go:168`).
- **Failure rides the stream.** `stopReason: error` becomes an attempt failure; the runner retries
  per policy, and the terminal message never enters the model context
  ([Events and protocol](/core/events) §5).
- **Do not put credentials in logs or error strings.** The audit path only records
  provider/method/target/duration/status (`docs/SERVICE-GRAPH.md:168`); the HTTP shell redacts.

## 5. Swapping the whole `llm` service

If you want the resolution logic itself to be different (for example a multi-region router, or a
provider that needs request-scoped credentials), the right shape is a **service replacement**, not a
patch: the `llm` service provides `ctx.llm`, so you supply your own factory through
`loadout.Config.Provide` and keep the descriptor's `Version` bumped. That is the assembly seam
described in [Write a service descriptor](/core/extend/service-descriptor); the loop sees only a
`StreamFn`.

## 6. Attachments: `file_id` > `url` > `base64`

The file/image protocol is implemented, and the order is fixed. Two shapes matter:

- **What the ledger stores** is a reference only: `ImageRefContent{blobId, mimeType, size…}` (and
  `MediaRefContent` for non-images). Base64 never enters the events table, so the same image
  referenced by many turns is one blob row and one copy of the bytes
  (`internal/agentcore/content.go:190`, `openspec/specs/provider-files/spec.md:5`).
- **What the provider wire carries** is a transport form: `ImageContent` with exactly one of
  `fileId` / `url` / `data` populated (`internal/agentcore/content.go:156`).

Resolution happens in the worker, before the outbound request, against the target's declared
capabilities:

1. **`file_id`** — a provider-side handle from the upload path. Cheapest form: the bytes cross the
   wire once and again only when the handle expires. Handles are cached by
   `(sha256, provider, workspace)` with a TTL and lazy refresh
   (`internal/provider/imagefiles.go:324`).
2. **`url`** — a minted URL (default TTL 10 minutes) so the bytes stay out of the request body
   (`internal/provider/imagefiles.go:163`).
3. **`base64`** — universal, and the bound: the inline path allocates exactly one encoded buffer,
   capped by the provider's `MaxInlineBytes` before a byte is read
   (`internal/provider/imagefiles.go:299`).

The capability declaration is per **wire protocol**, not per model name: `ImageCapsForAPI`
(`internal/provider/imagecaps.go:82`) and `FileCaps` for documents
(`internal/provider/media.go:51`), each with `BestForm()` following the same order
(`internal/provider/imagecaps.go:63`, `internal/provider/media.go:80`). A driver that declares no
capabilities is treated as base64-only, which every vendor accepts.

Degradation is honest, never silent: a failed upload/URL falls through with a recorded `Note`, and
an image over the declared ceiling that no cheaper form can carry fails closed as
`ErrImageTooLarge` naming the size and the limit — the run continues without the image instead of
an OOM or a rejected request (`internal/provider/imagefiles.go:42`).

So the work an adapter author has is narrow: declare the caps for the protocol and encode the three
forms. Everything above the encoder — capability selection, the handle cache, the bounded inline
buffer — is already in `internal/provider`.

## 7. Testing a provider adapter

| Gate | What to run |
|---|---|
| Hermetic unit tests | provider package tests with recorded/wire-level streams; no network |
| Fake-provider path | `KEEL_MODEL` unset — the loop, kernel and HTTP faces are exercised without credentials |
| Real-model E2E | `docs/REAL-MODEL-E2E.md` (§1–§2 config, §3 script usage, §4 assertion and evidence rules; §7 lists the known boundaries) |
| Redaction | the failure path is part of the E2E: a deliberately wrong key must be visible as 401 **and** must not leak the key in logs or responses |
| Regression | if the change touches the default assembly, the R1 differential gate applies ([Test and accept](/core/extend/testing)) |
