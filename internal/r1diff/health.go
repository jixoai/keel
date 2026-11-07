package r1diff

import (
	"encoding/json"
	"errors"
	"fmt"
)

// VerifyHealthOwner checks that a /v1/health response was produced by pid.
//
// The runtime answers /v1/health with its own pid; matching it against the
// child the harness launched is what proves the scenario will talk to the
// build under test. A stale runtime holding the capture port would otherwise
// answer every request while the candidate never bound (or bound and lost the
// race), and the differential would silently compare two runs of the stale
// binary. A response without a pid cannot prove ownership, so it fails closed.
func VerifyHealthOwner(body []byte, pid int) error {
	var h struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return fmt.Errorf("r1diff: health response is not JSON: %w", err)
	}
	if h.PID == 0 {
		return errors.New("r1diff: health response carries no pid: cannot prove it is the process we started")
	}
	if h.PID != pid {
		return fmt.Errorf("r1diff: health answers with pid %d, not %d (stale runtime on the capture port?)", h.PID, pid)
	}
	return nil
}
