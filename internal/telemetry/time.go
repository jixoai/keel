// Ported from pi/packages/telemetry/src/memory.ts @ 55a5c6914570e2ae31ca36297654483bbbf46883.
// PI stamps wall-clock milliseconds at microsecond resolution and measures
// durations with a monotonic clock (memory.ts:97-126). Go's time.Time carries
// a monotonic reading, so the same split is preserved.

package telemetry

import (
	"math"
	"time"
)

// milliseconds converts a wall-clock instant to epoch milliseconds, rounded to
// the microsecond as PI's telemetryNow does.
func milliseconds(instant time.Time) float64 {
	return roundMicroseconds(float64(instant.UnixNano()) / 1e6)
}

// durationMilliseconds converts a monotonic duration to milliseconds, rounded
// to the microsecond as PI's event/span durations are.
func durationMilliseconds(duration time.Duration) float64 {
	return roundMicroseconds(float64(duration) / 1e6)
}

func roundMicroseconds(milliseconds float64) float64 {
	return math.Round(milliseconds*1000) / 1000
}
