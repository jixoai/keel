package agentloop

import (
	"context"
	"fmt"

	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/servicegraph"
)

// Service identity of the default loop service. The values mirror
// internal/loadout's frozen blueprint (name, version, capabilities, effects);
// a test asserts they do not drift.
const (
	// ServiceName is the service instance name in a loadout.
	ServiceName = "pi-loop"
	// Version is the descriptor version of this implementation. It must be
	// bumped whenever the implementation changes: the graph treats (name,
	// version, provides, requires, optional, role) as reassembly identity.
	Version = "v1"
	// CapLoop is the capability the loop provides.
	CapLoop = "ctx.loop"
	// CapLLM is the capability the loop requires: the provider stream.
	CapLLM = "ctx.llm"
	// CapTools is the capability the loop requires: the tool set/executor
	// wiring. The loop itself only needs the tool objects (they arrive in
	// Input.Context.Tools); the binding is what proves the loadout has a tools
	// service at all.
	CapTools = "ctx.tools"
	// CapCompaction is the optional capability: with it the loop's run may
	// compact the context; without it the loop runs non-compacting and the
	// graph records the degraded diagnostic.
	CapCompaction = "ctx.compaction"
	// EffectLoopStream names the loop's cleanup scope. The default loop is
	// stateless, so nothing is registered under it; it documents the run
	// streams a loop instance is responsible for.
	EffectLoopStream = "loop-stream"
)

// StreamProvider is the shape the loop accepts for the CapLLM binding: an
// instance that yields the provider stream. A bare provider.StreamFn is also
// accepted (see BindStream), which is what tests and simple loadouts bind.
type StreamProvider interface {
	StreamFn() provider.StreamFn
}

// BindStream builds the default loop from a resolved loadout: it binds the
// CapLLM provider and constructs a PiLoop over it. It fails with ErrNoStream
// when the capability is not bound or the bound value is neither a
// provider.StreamFn nor a StreamProvider.
//
// It is the binding-aware factory Descriptor installs, so that reassembling a
// loadout with a new llm provider rebinds pi-loop to the new stream instead of
// keeping a captured one.
func BindStream(resolved servicegraph.Resolved) (*PiLoop, error) {
	bound, ok := resolved.Bind(CapLLM)
	if !ok {
		return nil, fmt.Errorf("%w: capability %q is not bound for service %q", ErrNoStream, CapLLM, resolved.Service)
	}
	stream, ok := streamFromBinding(bound)
	if !ok {
		return nil, fmt.Errorf("%w: capability %q is bound to %T, want provider.StreamFn or StreamProvider",
			ErrNoStream, CapLLM, bound)
	}
	return NewPiLoop(stream), nil
}

// streamFromBinding accepts the shapes an llm binding may have: the named
// provider.StreamFn, a StreamProvider instance, or an unnamed function with
// the provider stream signature (a method value or a hand-written func).
func streamFromBinding(bound any) (provider.StreamFn, bool) {
	switch value := bound.(type) {
	case provider.StreamFn:
		return value, value != nil
	case StreamProvider:
		stream := value.StreamFn()
		return stream, stream != nil
	case func(context.Context, string, provider.LlmContext, provider.StreamConfig) (*provider.AssistantMessageEventStream, error):
		return provider.StreamFn(value), value != nil
	default:
		return nil, false
	}
}

// Descriptor returns the "pi-loop" service descriptor for a loadout: it
// provides ctx.loop, requires ctx.llm and ctx.tools, optionally consumes
// ctx.compaction, and its factory builds a PiLoop from the CapLLM binding
// (BindStream) on every assembly.
//
// The descriptor is the canonical declaration of the default loop; the
// loadout blueprint in internal/loadout must describe the same service
// (asserted by test).
func Descriptor() servicegraph.ServiceDescriptor {
	return DescriptorWithFactory(func(_ context.Context, resolved servicegraph.Resolved) (servicegraph.Instance, error) {
		loop, err := BindStream(resolved)
		if err != nil {
			return nil, err
		}
		return loop, nil
	})
}

// DescriptorWithFactory is the escape hatch for a hosting process that builds
// the loop instance differently — for example a loadout that captures a fixed
// stream, or a test that installs a different loop implementation behind the
// same slot. The service identity (name, version, capabilities, effects) is
// unchanged; only the factory differs.
func DescriptorWithFactory(factory servicegraph.Factory) servicegraph.ServiceDescriptor {
	return servicegraph.ServiceDescriptor{
		Name:     ServiceName,
		Version:  Version,
		Provides: []string{CapLoop},
		Requires: []string{CapLLM, CapTools},
		Optional: []string{CapCompaction},
		Effects:  []string{EffectLoopStream},
		Factory:  factory,
	}
}
