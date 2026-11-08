package agentapi

import (
	"encoding/json"
	"sync"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/httpapi"
)

// PartialSource adapts the in-process partial hub to the SSE seam of
// internal/httpapi. The adapter owns the slow-consumer policy: the hub already
// drops when its subscriber buffer is full, and the adapter does the same while
// converting to the transport frame type.
func PartialSource(hub *agentext.PartialHub) httpapi.PartialSource {
	if hub == nil {
		return nil
	}
	return &partialSource{hub: hub}
}

type partialSource struct {
	hub *agentext.PartialHub
}

func (p *partialSource) SubscribePartials(tenantID, sessionID uuid.UUID) (<-chan httpapi.PartialFrame, func()) {
	in, cancelHub := p.hub.Subscribe(tenantID, sessionID)
	out := make(chan httpapi.PartialFrame, partialBufferFrames)
	var once sync.Once
	stop := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-stop:
				return
			case partial, ok := <-in:
				if !ok {
					return
				}
				raw, err := json.Marshal(partial)
				if err != nil {
					continue
				}
				select {
				case out <- httpapi.PartialFrame{Event: "partial", Data: raw}:
				case <-stop:
					return
				default:
					// Slow client: drop the frame rather than block the run.
				}
			}
		}
	}()
	cancel := func() {
		once.Do(func() {
			close(stop)
			cancelHub()
		})
	}
	return out, cancel
}

const partialBufferFrames = 16
