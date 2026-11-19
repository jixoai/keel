package layerfs

import "errors"

// Typed failures. Callers (the file-plugin and the tool surface) map these onto
// the contract's error vocabulary; layerfs itself never invents a fourth shape.
var (
	// ErrNotFound is returned when an object hash is absent.
	ErrNotFound = errors.New("layerfs: object not found")
	// ErrQuotaExceeded is returned when a Put would exceed the store quota. The
	// temporary object is discarded at the commit point, so no partial object
	// becomes visible.
	ErrQuotaExceeded = errors.New("layerfs: quota exceeded")
	// ErrIO wraps a storage failure.
	ErrIO = errors.New("layerfs: io")
	// ErrUnavailable reports a store that cannot serve (closed, misconfigured).
	ErrUnavailable = errors.New("layerfs: unavailable")
	// ErrInvalid reports a malformed hash, tree entry or path.
	ErrInvalid = errors.New("layerfs: invalid")
)
