// Package agentlooptest is the exported PI conformance suite for
// agentloop.Loop service implementations.
//
// # Why a separate package
//
// The suite runs against the agentloop service interface, not against
// internal/loop internals: a replacement loop can import it, wire its provider
// stream into a Factory and run the same contract cases pi-loop runs. The case
// list is exported as a manifest (agentloop.Manifest / ManifestHash) and the
// default blueprint pins pi-loop's manifest hash; a loop service that cannot
// present a matching manifest is refused by the loadout (see internal/loadout).
//
// # Usage
//
//	func TestMyLoopConformance(t *testing.T) {
//		agentlooptest.Suite(t, func(t *testing.T, stream provider.StreamFn) agentloop.Loop {
//			return myloop.New(stream)
//		})
//	}
//
// Factory receives the scripted provider stream each case drives; a loop binds
// its provider at construction, exactly like the ctx.llm binding of a loadout
// generation, so the suite cannot pass it through agentloop.Input.
//
// # Discipline
//
// Every case keeps PI's upstream it(...) title (see agentloop.Manifest) and its
// scenario is an adaptation of internal/loop's conformance test of the same
// title. The suite asserts the service-interface contract — event order, tool
// call ↔ result pairing, cancellation, error paths and transcript shape — not
// pi-loop private details: sequences are checked structurally rather than for
// an exact event count, and additive Keel events (telemetry/compaction/progress)
// are ignored. The internal/loop suite remains the line-by-line PI diff for the
// default implementation; this suite is the gate a *replacement* must pass.
//
// All cases are deterministic: scripts are fixed, tool call ids come from the
// script, and no case depends on the wall clock (the one timing case mirrors
// PI's duration test and measures hook exclusion with a wide bound).
package agentlooptest

import (
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/provider"
)

// Factory builds a fresh loop bound to the scripted provider stream of one
// case. The suite calls it once per case (and more than once inside cases that
// compare two runs), so the factory must not share run state.
//
// The stream is a provider.StreamFn, exactly the shape a loadout binds as
// ctx.llm: requiring it is what makes a provider-driven loop the unit under
// test. A loop that never consults the model (for example the canned testloop)
// cannot pass the suite.
type Factory func(t *testing.T, stream provider.StreamFn) agentloop.Loop

// T is the subset of *testing.T the suite consumes. Suite receives a
// *testing.T; the interface exists so the suite's own tests can drive a case
// with a recorder and prove that a contract-violating loop fails on an
// assertion rather than at compile time.
type T interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// env carries one case's assertion sink and loop factory.
type env struct {
	t  T
	mk func(stream provider.StreamFn) agentloop.Loop
}

// scripted returns a provider double serving responses in order. It records
// every request and can block, fail or truncate a specific call.
func (e *env) scripted(responses ...agentcore.AssistantMessage) *scriptedProvider {
	return newScriptedProvider(responses...)
}

// loop builds the implementation under test over a scripted provider.
func (e *env) loop(prov *scriptedProvider) agentloop.Loop { return e.mk(prov.stream) }

// caseFunc is one manifest case. Cases report through T.
type caseFunc func(t T, e *env)

// cases maps a manifest case name to its implementation. Every manifest entry
// must have an implementation and vice versa (asserted by the suite's own
// tests), so the manifest can never advertise coverage the suite lacks.
var cases = map[string]caseFunc{
	"abort-controller":                 caseAbortController,
	"abort-during-streaming":           caseAbortDuringStreaming,
	"agent-message-events":             caseAgentMessageEvents,
	"continue-existing-context":        caseContinueExistingContext,
	"continue-no-messages":             caseContinueNoMessages,
	"custom-message-convert-to-llm":    caseCustomMessageConvertToLLM,
	"follow-up-queue":                  caseFollowUpQueue,
	"prepare-next-turn-snapshot":       casePrepareNextTurnSnapshot,
	"prepare-request-initial":          casePrepareRequestInitial,
	"prepare-request-no-steering-poll": casePrepareRequestNoSteeringPoll,
	"provider-context-from-transcript": caseProviderContextFromTranscript,
	"provider-error-terminal":          caseProviderErrorTerminal,
	"steering-after-tools":             caseSteeringAfterTools,
	"terminate-blocked-single":         caseTerminateBlockedSingle,
	"terminate-mixed-batch":            caseTerminateMixedBatch,
	"terminate-partial-batch":          caseTerminatePartialBatch,
	"terminate-whole-batch":            caseTerminateWholeBatch,
	"tool-call-results":                caseToolCallResults,
	"tool-completion-order":            caseToolCompletionOrder,
	"tool-duration-excludes-hooks":     caseToolDurationExcludesHooks,
	"tool-mutated-args":                caseToolMutatedArgs,
	"tool-truncated-assistant":         caseToolTruncatedAssistant,
	"transform-before-convert":         caseTransformBeforeConvert,
}

// Suite runs the PI conformance manifest against f. Each case is a subtest
// named by its manifest name; a failing case fails the suite.
func Suite(t *testing.T, f Factory) {
	t.Helper()
	if f == nil {
		t.Fatal("agentlooptest: nil Factory")
	}
	for _, c := range agentloop.Manifest() {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			mk := func(stream provider.StreamFn) agentloop.Loop { return f(t, stream) }
			runCase(t, mk, c.Name, c.Title)
		})
	}
}

// runCase executes one manifest case against mk, reporting through t.
func runCase(t T, mk func(stream provider.StreamFn) agentloop.Loop, name, title string) {
	t.Helper()
	impl, ok := cases[name]
	if !ok {
		t.Fatalf("agentlooptest: manifest case %q (%s) has no suite implementation", name, title)
	}
	impl(t, &env{t: t, mk: mk})
}
