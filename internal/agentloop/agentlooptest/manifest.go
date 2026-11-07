package agentlooptest

import "github.com/jixoai/keel/internal/agentloop"

// Case is agentloop.Case: one PI conformance case in the normalized manifest.
type Case = agentloop.Case

// Manifest returns the canonical PI conformance case list this suite runs.
// It is the same list agentloop.Manifest exposes (the production contract the
// loadout gate hashes); the alias keeps the conformance vocabulary reachable
// from the package loop authors import.
func Manifest() []Case { return agentloop.Manifest() }

// ManifestHash returns the SHA-256 of Manifest's canonical JSON encoding. A
// loop service declares this hash (plus the manifest) to enter a loadout; see
// internal/loadout.PinnedPiLoopManifestHash and the fail-closed gate.
func ManifestHash() string { return agentloop.ManifestHash() }

// HashCases computes the canonical hash of an arbitrary declared case list.
func HashCases(cases []Case) string { return agentloop.HashCases(cases) }
