// Package routemux is an http.ServeMux that remembers the patterns registered
// on it. The permission-coverage gate enumerates those patterns and refuses to
// start when a /v1 route has no declaration (change keel-bot-identity-access,
// A4.2a).
//
// It is a thin recorder, not a router: matching, precedence and PathValue all
// remain net/http.ServeMux behavior. Registering through the wrapper is the
// only way a pattern can be forgotten, so the two servers register every route
// through it.
package routemux

import (
	"net/http"
	"sync"
)

// Mux wraps http.ServeMux and records every pattern passed to Handle/HandleFunc.
// It is safe for concurrent registration (the constructors do not need it, but
// plugins may register routes later).
type Mux struct {
	*http.ServeMux

	mu       sync.Mutex
	patterns []string
}

// New returns an empty recording mux.
func New() *Mux {
	return &Mux{ServeMux: http.NewServeMux()}
}

// HandleFunc registers one handler and records its pattern.
func (m *Mux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.record(pattern)
	m.ServeMux.HandleFunc(pattern, handler)
}

// Handle registers one handler and records its pattern.
func (m *Mux) Handle(pattern string, handler http.Handler) {
	m.record(pattern)
	m.ServeMux.Handle(pattern, handler)
}

// Patterns returns a copy of the registered patterns in registration order.
func (m *Mux) Patterns() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.patterns...)
}

func (m *Mux) record(pattern string) {
	m.mu.Lock()
	m.patterns = append(m.patterns, pattern)
	m.mu.Unlock()
}
