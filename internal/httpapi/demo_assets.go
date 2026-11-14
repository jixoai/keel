// The demo asset surface: GET /v1/demo/assets/{name}.png.
//
// An image turn has to return a URL a gallery can render, and the demo has no
// vendor CDN to point at, so the assets are served from this process. The route is
// registered unconditionally and answers 404 when the demo provider is off: a
// missing asset is the honest answer there, and an unregistered path would be
// indistinguishable from a routing bug.
//
// # Why not a static directory
//
// The set of assets is a compiled-in closed set (internal/demo). Serving them from
// a directory would put a filesystem path in the request's hands; the handler
// resolves a name through demo.AssetByName and never touches the file system.

package httpapi

import (
	"net/http"
	"strings"

	"github.com/jixoai/keel/internal/demo"
)

// registerDemoAssetRoutes wires the hosted demo assets.
func (s *Server) registerDemoAssetRoutes() {
	s.mux.HandleFunc("GET /v1/demo/assets/{name}", s.demoAsset)
}

// demoAsset serves one compiled-in tile.
//
// The name is matched against the closed set, not joined onto a path, so a name
// carrying ".." or a slash simply is not found. A cacheable response is
// deliberate: the bytes never change, so a gallery that re-renders does not refetch.
func (s *Server) demoAsset(w http.ResponseWriter, r *http.Request) {
	if !audioDemoEnabled() {
		// Not enabled: the asset does not exist. 404 (not 503) because the caller
		// asked for a resource, and no resource is published here.
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSuffix(strings.TrimSpace(r.PathValue("name")), ".png")
	asset, ok := demo.AssetByName(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", itoa(len(asset.Bytes)))
	// The bytes are compiled in and immutable, so a long cache is safe and correct.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(asset.Bytes)
}

// itoa formats a non-negative int without pulling strconv into a file that only
// needs it once.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	pos := len(digits)
	for value > 0 {
		pos--
		digits[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[pos:])
}
