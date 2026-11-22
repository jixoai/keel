package oar

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ContractVersion is the OAR contract revision negotiated in initialize. It is
// date-free on purpose: the contract is versioned by its own number and frozen
// in docs/OAR.md. A runtime that does not declare this version is rejected
// before a session opens.
const ContractVersion = "1"

// CapabilitiesSchema is the schema id of the OAR declaration carried in
// initialize capabilities.experimental.oar.
const CapabilitiesSchema = "keel.oar.capabilities/1"

// ExperimentalKey is the key under capabilities.experimental that carries the
// OAR declaration. The gateway passes experimental capability blocks through
// untouched, so OAR does not need a gateway protocol revision.
const ExperimentalKey = "oar"

// Capability is one bit of the OAR runtime capability matrix. The vocabulary
// is closed: an unknown requirement is invalid_capability, never silently
// ignored.
type Capability string

const (
	// CapabilityRecords is the OAR record stream itself. Every OAR runtime MUST
	// declare it.
	CapabilityRecords Capability = "records"
	// CapabilityAgentPath reports that every record carries agentPath, not only
	// a session-level sequence.
	CapabilityAgentPath Capability = "agent_path"
	// CapabilityCalls reports call records (a tool or model call started).
	CapabilityCalls Capability = "calls"
	// CapabilityResults reports result records linked to a call by callId.
	CapabilityResults Capability = "results"
	// CapabilityStatus reports status records (turn/session lifecycle, degraded
	// conditions).
	CapabilityStatus Capability = "status"
	// CapabilityMessages reports assistant/status message records (including
	// reasoning and error text).
	CapabilityMessages Capability = "messages"
	// CapabilityUsage reports token usage on a usage record. Absence means "not
	// measurable", never zero.
	CapabilityUsage Capability = "usage"
	// CapabilityCancel reports turn cancellation.
	CapabilityCancel Capability = "cancel"
	// CapabilityResume reports that a session can be resumed from seq inside one
	// sidecar lifetime.
	CapabilityResume Capability = "resume"
	// CapabilityStreaming reports incremental message/reasoning records while a
	// turn runs (not only terminal messages).
	CapabilityStreaming Capability = "streaming"
	// CapabilitySubagents reports records on child agent paths (agentPath below
	// the root), i.e. the runtime fans work out to subagents.
	CapabilitySubagents Capability = "subagents"
	// CapabilityLiveModel reports that the runtime talks to a real model. A
	// simulated runtime MUST NOT declare it, and a caller that requires a live
	// model rejects a simulated runtime with simulated_runtime.
	CapabilityLiveModel Capability = "live_model"
)

// capabilityOrder is the closed vocabulary in stable documentation order.
var capabilityOrder = []Capability{
	CapabilityRecords,
	CapabilityAgentPath,
	CapabilityCalls,
	CapabilityResults,
	CapabilityStatus,
	CapabilityMessages,
	CapabilityUsage,
	CapabilityCancel,
	CapabilityResume,
	CapabilityStreaming,
	CapabilitySubagents,
	CapabilityLiveModel,
}

// KnownCapabilities returns the closed vocabulary in stable order.
func KnownCapabilities() []Capability {
	out := make([]Capability, len(capabilityOrder))
	copy(out, capabilityOrder)
	return out
}

// ParseCapability validates a capability name.
func ParseCapability(raw string) (Capability, bool) {
	for _, known := range capabilityOrder {
		if string(known) == raw {
			return known, true
		}
	}
	return "", false
}

// joinCapabilities renders a capability list for errors and docs.
func joinCapabilities(capabilities []Capability) string {
	if len(capabilities) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		parts = append(parts, string(capability))
	}
	return strings.Join(parts, ", ")
}

// BaseCapabilities is the minimum every OAR runtime must declare: a record
// stream with seq, agentPath, and call/result/status records. An adapter that
// cannot provide these is not an OAR runtime.
var BaseCapabilities = []Capability{
	CapabilityRecords,
	CapabilityAgentPath,
	CapabilityCalls,
	CapabilityResults,
	CapabilityStatus,
}

// Declaration is the machine-readable capability declaration carried in
// initialize capabilities.experimental.oar.
type Declaration struct {
	// Schema is CapabilitiesSchema.
	Schema string `json:"schema"`
	// ContractVersion is the OAR contract revision (ContractVersion).
	ContractVersion string `json:"contractVersion"`
	// Runtime is the adapter id ("codex", "fake", ...).
	Runtime string `json:"runtime"`
	// RuntimeVersion is the underlying runtime's own version, when known.
	RuntimeVersion string `json:"runtimeVersion,omitempty"`
	// Simulated marks a deterministic test runtime. A simulated runtime can
	// never claim live_model.
	Simulated bool `json:"simulated,omitempty"`
	// Capabilities is the declared capability set.
	Capabilities []Capability `json:"capabilities"`
}

// Matrix returns the full declared capability set; absent bits read false.
func (d Declaration) Matrix() map[Capability]bool {
	out := make(map[Capability]bool, len(capabilityOrder))
	for _, capability := range d.Capabilities {
		out[capability] = true
	}
	return out
}

// Has reports whether the declaration carries one capability. Unknown names
// report false; callers that need the typed rejection use Require.
func (d Declaration) Has(capability Capability) bool {
	for _, declared := range d.Capabilities {
		if declared == capability {
			return true
		}
	}
	return false
}

// Validate checks the declaration against the closed contract: exact schema and
// contract version, a runtime id, unique known capabilities, no simulated
// runtime claiming a live model, and the base capability set.
func (d Declaration) Validate() error {
	invalid := func(format string, args ...any) error {
		return &Error{Code: CodeInvalidCapability, Runtime: d.Runtime, Detail: fmt.Sprintf(format, args...)}
	}
	if d.Schema != CapabilitiesSchema {
		return invalid("schema is %q, want %q", d.Schema, CapabilitiesSchema)
	}
	if d.ContractVersion != ContractVersion {
		return &Error{
			Code:    CodeUnsupportedCapability,
			Runtime: d.Runtime,
			Detail:  fmt.Sprintf("contract version %q is not supported (want %q)", d.ContractVersion, ContractVersion),
		}
	}
	if strings.TrimSpace(d.Runtime) == "" {
		return invalid("runtime id is required")
	}
	if len(d.Capabilities) == 0 {
		return invalid("capability list is empty")
	}
	seen := make(map[Capability]bool, len(d.Capabilities))
	for _, capability := range d.Capabilities {
		if _, ok := ParseCapability(string(capability)); !ok {
			return invalid("unknown capability %q", capability)
		}
		if seen[capability] {
			return invalid("capability %q is declared twice", capability)
		}
		seen[capability] = true
	}
	if d.Simulated && seen[CapabilityLiveModel] {
		return invalid("a simulated runtime must not claim live_model")
	}
	var missing []Capability
	for _, capability := range BaseCapabilities {
		if !seen[capability] {
			missing = append(missing, capability)
		}
	}
	if len(missing) > 0 {
		return &Error{
			Code:     CodeUnsupportedCapability,
			Runtime:  d.Runtime,
			Detail:   "the declaration is missing the base capability set",
			Required: missing,
			Declared: append([]Capability(nil), d.Capabilities...),
		}
	}
	return nil
}

// Require enforces a caller's required capability set against the declaration.
// An unknown name is invalid_capability; a known bit the runtime did not
// declare is unsupported_capability. Requiring live_model on a simulated
// runtime is simulated_runtime, so a fake can never satisfy a live-model gate.
func (d Declaration) Require(required []Capability) error {
	var unknown []Capability
	for _, capability := range required {
		if _, ok := ParseCapability(string(capability)); !ok {
			unknown = append(unknown, capability)
		}
	}
	if len(unknown) > 0 {
		return &Error{
			Code:     CodeInvalidCapability,
			Runtime:  d.Runtime,
			Detail:   "required capability is outside the closed vocabulary",
			Required: append([]Capability(nil), unknown...),
		}
	}
	var missing []Capability
	for _, capability := range required {
		if !d.Has(capability) {
			missing = append(missing, capability)
		}
	}
	if len(missing) > 0 {
		code := CodeUnsupportedCapability
		if d.Simulated && containsCapability(missing, CapabilityLiveModel) {
			code = CodeSimulatedRuntime
		}
		return &Error{
			Code:     code,
			Runtime:  d.Runtime,
			Detail:   "the runtime did not declare a required capability",
			Required: missing,
			Declared: append([]Capability(nil), d.Capabilities...),
		}
	}
	return nil
}

func containsCapability(capabilities []Capability, want Capability) bool {
	for _, capability := range capabilities {
		if capability == want {
			return true
		}
	}
	return false
}

// DeclarationFor returns the capability matrix of a known runtime. The matrix
// is static on purpose: an adapter declares what it can actually do, and a
// caller that needs more is rejected before the runtime is used. "codex" is
// the first real adapter; "fake" is the deterministic test runtime and is
// always simulated=true, live_model=false.
func DeclarationFor(runtime, runtimeVersion string) Declaration {
	declaration := Declaration{
		Schema:          CapabilitiesSchema,
		ContractVersion: ContractVersion,
		Runtime:         runtime,
		RuntimeVersion:  runtimeVersion,
	}
	switch runtime {
	case RuntimeCodex:
		declaration.Capabilities = []Capability{
			CapabilityRecords,
			CapabilityAgentPath,
			CapabilityCalls,
			CapabilityResults,
			CapabilityStatus,
			CapabilityMessages,
			CapabilityUsage,
			CapabilityCancel,
			CapabilityLiveModel,
		}
	case RuntimeFake:
		declaration.Simulated = true
		declaration.Capabilities = []Capability{
			CapabilityRecords,
			CapabilityAgentPath,
			CapabilityCalls,
			CapabilityResults,
			CapabilityStatus,
			CapabilityMessages,
			CapabilityUsage,
			CapabilityCancel,
			CapabilityResume,
			CapabilitySubagents,
		}
	default:
		// Unknown runtimes get a declaration that Validate rejects; the caller
		// must name a runtime in the matrix instead of silently trusting one.
		declaration.Capabilities = nil
	}
	return declaration
}

// Runtime ids of the capability matrix.
const (
	// RuntimeCodex is the Codex CLI adapter (live model, JSONL event stream).
	RuntimeCodex = "codex"
	// RuntimeFake is the deterministic test runtime. It is always simulated.
	RuntimeFake = "fake"
)

// KnownRuntimes returns the adapter ids with a frozen capability matrix.
func KnownRuntimes() []string {
	out := []string{RuntimeCodex, RuntimeFake}
	sort.Strings(out)
	return out
}

// MarshalDeclaration is the wire form of a declaration, used by the sidecar
// and by tests that build a handshake fixture.
func MarshalDeclaration(declaration Declaration) (json.RawMessage, error) {
	raw, err := json.Marshal(declaration)
	if err != nil {
		return nil, fmt.Errorf("oar: encode capability declaration: %w", err)
	}
	return raw, nil
}

// ParseDeclaration decodes a declaration from the initialize experimental
// block. Unknown fields are tolerated for forward compatibility; validation is
// still strict about the fields this contract owns.
func ParseDeclaration(raw json.RawMessage) (Declaration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return Declaration{}, newError(CodeProtocol, "initialize did not declare capabilities.experimental.%s", ExperimentalKey)
	}
	var declaration Declaration
	if err := json.Unmarshal(raw, &declaration); err != nil {
		return Declaration{}, &Error{Code: CodeProtocol, Detail: "capability declaration is not a JSON object", Err: err}
	}
	return declaration, nil
}
