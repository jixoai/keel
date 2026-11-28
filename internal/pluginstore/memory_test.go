package pluginstore_test

import (
	"testing"
	"time"

	"github.com/jixoai/keel/internal/pluginstore"
	"github.com/jixoai/keel/internal/pluginstore/conformance"
)

func TestMemoryConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) pluginstore.Store {
		return pluginstore.NewMemory(func() time.Time {
			return time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
		})
	})
}
