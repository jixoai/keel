package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/jixoai/keel/internal/store"
)

// configHashDomain keeps the loadout digest from ever colliding with another
// sha256 use over the same fields.
const configHashDomain = "keel-config-v1"

// ConfigHashForLoadout derives the configuration identity of a pinned loadout:
// the run snapshot's configHash. Revision is the configuration version — it
// advances when an administrator changes the configuration (the semantics of a
// config.toml rotation), not on every process start — so hashing (ID,
// revision) answers "did this run execute under a different configuration?"
// without reading the document again.
//
// Generation is deliberately excluded: every reassembly increments it, even
// when the configuration document did not change, so including it would report
// a config rotation where none happened.
func ConfigHashForLoadout(id string, revision int64) string {
	sum := sha256.Sum256([]byte(configHashDomain + "\x00" + id + "\x00" + strconv.FormatInt(revision, 10)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// runSnapshotAnchor resolves the sequence this attempt's configuration
// snapshot is anchored at, plus a configHash read from the newest rotation
// event when the log carries one.
//
// A committed config.rotated event is the authoritative anchor: it is the point
// at which the configuration became active, and the spec requires the next run
// after a rotation to point at it. Without a rotation event the run-start
// read-through is used — the newest committed sequence the transcript was built
// from — because a run has no lifecycle event of its own today (run.started is
// reserved, not emitted), and the read-through is exactly the log position at
// which the attempt begins. Zero means the log was empty.
func runSnapshotAnchor(events []store.Event, readThrough int64) (int64, string) {
	anchor := readThrough
	var configHash string
	for _, ev := range events {
		if ev.Kind != store.EventConfigRotated {
			continue
		}
		anchor = ev.Seq
		var payload struct {
			ConfigHash string `json:"configHash"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err == nil && payload.ConfigHash != "" {
			configHash = payload.ConfigHash
		}
	}
	return anchor, configHash
}

// firstNonEmpty returns the first non-empty value, or "".
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
