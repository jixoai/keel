package modelcfg

// ProviderProbe is the result of one health probe of a configured provider.
//
// It is the wire-independent answer of the hosting process's probe seam: the
// HTTP layer (POST /v1/providers/{id}/test) and `keel model provider test`
// render it, and the `--refresh` mode of `keel model catalog` reads its model
// list. The probe is a real but minimal request through the server's own
// credential resolution — it never carries a credential value and never
// mutates configuration, so it is safe to repeat.
//
// Status uses the health vocabulary of the provider registry (internal/
// modelprovider): ready|needs_auth|unreachable|invalid_config. OK is the one
// fact a caller acts on; Error/Issues explain a false one.
type ProviderProbe struct {
	// ProviderID is the resolved provider identity the probe ran against.
	ProviderID string `json:"providerId"`
	// ProviderType is the provider family (llm, image, tts, ...).
	ProviderType string `json:"providerType"`
	// APIType is the wire protocol the provider declares.
	APIType string `json:"apiType,omitempty"`
	// Endpoint is the resolved upstream base URL (empty for demo).
	Endpoint string `json:"endpoint,omitempty"`
	// OK reports that the probe reached the provider and authenticated.
	OK bool `json:"ok"`
	// Status is the measured health state (ready|needs_auth|unreachable|
	// invalid_config).
	Status string `json:"status"`
	// LatencyMS is the rounded round-trip time of the upstream request; zero
	// when no request was sent.
	LatencyMS int64 `json:"latencyMs"`
	// ModelCount is the number of models the upstream catalog answered with.
	ModelCount int `json:"modelCount,omitempty"`
	// Models is the upstream catalog (sorted ids), present on success. It is
	// the live lineup, not the configured member list.
	Models []string `json:"models,omitempty"`
	// Error names why OK is false, without a credential value.
	Error string `json:"error,omitempty"`
	// Issues are the field-path diagnostics the settings view uses.
	Issues []Issue `json:"issues,omitempty"`
}

// Provider probe statuses. They are the provider registry's health
// vocabulary, kept as plain strings here so this package stays the shared
// data vocabulary of both the API and the runtime.
const (
	// ProbeStatusReady: the provider answered and authenticated.
	ProbeStatusReady = "ready"
	// ProbeStatusNeedsAuth: no credential could be resolved; nothing was sent.
	ProbeStatusNeedsAuth = "needs_auth"
	// ProbeStatusUnreachable: a request was sent and failed (transport or a
	// non-200 answer).
	ProbeStatusUnreachable = "unreachable"
	// ProbeStatusInvalidConfig: the provider is incomplete on paper, so no
	// request was sent.
	ProbeStatusInvalidConfig = "invalid_config"
)
