package agentloop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Case identifies one PI conformance case the exported suite runs against a
// Loop implementation.
//
// The manifest is the replaceable-loop gate's static contract: the default
// blueprint pins ManifestHash for pi-loop, and a loadout refuses a loop service
// that cannot present a matching manifest (see internal/loadout). Adding,
// removing, renaming or retitling a case changes the hash, which forces the pin
// to be updated deliberately — a silent weakening of the suite cannot pass.
//
// Titles are PI's upstream it(...) titles, copied verbatim from the pinned
// upstream commit (pi@55a5c69); the suite keeps the same title-preservation
// discipline as internal/loop/conformance_test.go and the Go test functions
// there. Source names the upstream file that owns the title, so the two suites
// can be diffed line by line.
type Case struct {
	// Name is the stable suite subtest path ("tool-call-results").
	Name string `json:"name"`
	// Title is the upstream PI it(...) title this case preserves.
	Title string `json:"title"`
	// Source is the upstream file the title comes from.
	Source string `json:"source"`
}

// Upstream PI sources referenced by the manifest (pi@55a5c69).
const (
	SourceAgentLoopTest = "packages/agent/test/agent-loop.test.ts"
	SourceAgentTest     = "packages/agent/test/agent.test.ts"
	SourceE2ETest       = "packages/agent/test/e2e.test.ts"
	SourceProxyTest     = "packages/agent/test/proxy.test.ts"
)

// manifest is the canonical PI conformance case list. It is kept sorted by
// Name: the canonical JSON encoding (and therefore ManifestHash) must be
// independent of declaration order.
var manifest = []Case{
	{Name: "abort-controller", Title: "should handle abort controller", Source: SourceAgentTest},
	{Name: "abort-during-streaming", Title: "handles abort during streaming", Source: SourceE2ETest},
	{Name: "agent-message-events", Title: "should emit events with AgentMessage types", Source: SourceAgentLoopTest},
	{Name: "continue-existing-context", Title: "should continue from existing context without emitting user message events", Source: SourceAgentLoopTest},
	{Name: "continue-no-messages", Title: "should throw when context has no messages", Source: SourceAgentLoopTest},
	{Name: "custom-message-convert-to-llm", Title: "should handle custom message types via convertToLlm", Source: SourceAgentLoopTest},
	{Name: "follow-up-queue", Title: "should support follow-up message queue", Source: SourceAgentTest},
	{Name: "prepare-next-turn-snapshot", Title: "should use prepareNextTurn snapshot before continuing", Source: SourceAgentLoopTest},
	{Name: "prepare-request-initial", Title: "prepares the initial request after pending messages and can replace request state", Source: SourceAgentLoopTest},
	{Name: "prepare-request-no-steering-poll", Title: "does not poll steering after prepareRequest", Source: SourceAgentLoopTest},
	{Name: "provider-context-from-transcript", Title: "should build provider context exclusively from transcript messages", Source: SourceAgentLoopTest},
	{Name: "provider-error-terminal", Title: "emits an error instead of hanging when the stream ends without a terminal event", Source: SourceProxyTest},
	{Name: "steering-after-tools", Title: "should inject queued messages after all tool calls complete", Source: SourceAgentLoopTest},
	{Name: "terminate-blocked-single", Title: "should stop after a blocked tool call when beforeToolCall sets terminate=true", Source: SourceAgentLoopTest},
	{Name: "terminate-mixed-batch", Title: "should continue after a mixed batch with one terminating blocked call", Source: SourceAgentLoopTest},
	{Name: "terminate-partial-batch", Title: "should continue after parallel tool calls when not all tool results terminate", Source: SourceAgentLoopTest},
	{Name: "terminate-whole-batch", Title: "should stop after a tool batch when every tool result sets terminate=true", Source: SourceAgentLoopTest},
	{Name: "tool-call-results", Title: "should handle tool calls and results", Source: SourceAgentLoopTest},
	{Name: "tool-completion-order", Title: "should emit tool_execution_end in completion order but persist tool results in source order", Source: SourceAgentLoopTest},
	{Name: "tool-duration-excludes-hooks", Title: "records how long execute() took on the tool result, excluding hooks", Source: SourceAgentLoopTest},
	{Name: "tool-mutated-args", Title: "should execute mutated beforeToolCall args without revalidation", Source: SourceAgentLoopTest},
	{Name: "tool-truncated-assistant", Title: "should not execute tool calls from a length-truncated assistant message", Source: SourceAgentLoopTest},
	{Name: "transform-before-convert", Title: "should apply transformContext before convertToLlm", Source: SourceAgentLoopTest},
}

// Manifest returns a copy of the canonical PI conformance case list, sorted by
// case name. Mutating the returned slice never changes the manifest.
func Manifest() []Case {
	return append([]Case(nil), manifest...)
}

// ManifestHash returns the lowercase hex SHA-256 of Manifest's canonical JSON
// encoding. It is stable across processes and declaration order; it changes
// whenever a case's name, PI title or upstream source changes.
func ManifestHash() string { return HashCases(manifest) }

// HashCases computes the canonical hash of an arbitrary declared case list
// (the shape a loadout carries in its conformance declaration). Order does not
// matter: the list is normalized by (Name, Title, Source) before encoding, so
// two declarations that enumerate the same cases hash alike.
func HashCases(cases []Case) string {
	canonical := append([]Case(nil), cases...)
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Name != canonical[j].Name {
			return canonical[i].Name < canonical[j].Name
		}
		if canonical[i].Title != canonical[j].Title {
			return canonical[i].Title < canonical[j].Title
		}
		return canonical[i].Source < canonical[j].Source
	})
	blob, err := json.Marshal(canonical)
	if err != nil {
		// Case is a plain struct of strings; encoding cannot fail.
		panic("agentloop: marshal conformance manifest: " + err.Error())
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}
