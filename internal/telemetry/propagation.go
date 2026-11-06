// Ported from pi/packages/telemetry/src/memory.ts @ 55a5c6914570e2ae31ca36297654483bbbf46883
// (extract/inject) and index.ts:12-17 (the propagation contract).

package telemetry

import "strconv"

// Carrier keys are owned by the memory adapter, not by the contract. They stay
// identical to PI's reference adapter (memory.ts:93-95) so recorded identities
// and carriers compare one-to-one with PI.
const (
	carrierScopeKey = "pi-memory-scope"
	carrierSpanKey  = "pi-memory-span"
	carrierTraceKey = "pi-memory-trace"
)

// Inject returns a detached carrier for a known span or extracted parent,
// including settled spans (memory.ts:332-336). Unknown contexts report false.
func (m *Memory) Inject(context Context) (Carrier, bool) {
	provider, ok := context.(carrierProvider)
	if !ok {
		return nil, false
	}
	carrier := provider.telemetryCarrier()
	if carrier == nil {
		return nil, false
	}
	return copyCarrier(carrier), true
}

// Extract resumes a remote parent from a carrier. A nil or unrecognized carrier
// falls back to this recorder's root (memory.ts:338-354). Recognized identifiers
// are opaque strings and are not validated beyond the three required keys.
func (m *Memory) Extract(carrier Carrier) Context {
	if carrier == nil {
		return m
	}
	scope, hasScope := carrier[carrierScopeKey]
	span, hasSpan := carrier[carrierSpanKey]
	trace, hasTrace := carrier[carrierTraceKey]
	if !hasScope || !hasSpan || !hasTrace {
		return m
	}
	return &extractedContext{
		memory: m,
		carrier: Carrier{
			carrierScopeKey: scope,
			carrierSpanKey:  span,
			carrierTraceKey: trace,
		},
	}
}

type extractedContext struct {
	memory  *Memory
	carrier Carrier
}

func (c *extractedContext) Metrics() Metrics         { return c.memory }
func (c *extractedContext) Propagation() Propagation { return c.memory }

func (c *extractedContext) StartSpan(options SpanOptions, callback func(Span) error) error {
	var localID *int64
	if c.carrier[carrierScopeKey] == c.memory.scope {
		if id, err := strconv.ParseInt(c.carrier[carrierSpanKey], 10, 64); err == nil {
			localID = &id
		}
	}
	return c.memory.startSpan(&spanParent{
		carrier: c.carrier,
		localID: localID,
	}, options, callback)
}

func (c *extractedContext) telemetryCarrier() Carrier {
	return copyCarrier(c.carrier)
}
