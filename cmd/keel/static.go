package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// staticHandler serves the web build (NSH, `nsh package --target web`). When the
// build is absent it answers a JSON 404 that names what is missing instead of a
// blank page: `keel serve` must never look like a server with no UI by accident.
//
// NSH packages an app into a nested root (<out>/web/<app>/<App>/index.html), so
// the handler resolves the actual root once at boot: either <dir>/index.html
// directly or the shallowest index.html within the build directory (depth <= 4).
type staticHandler struct {
	dir  string // configured build directory (reported in diagnostics)
	root string // resolved serving root ("" when there is no build)
	file http.Handler
	log  *slog.Logger
}

// maxWebRootDepth bounds the search for a packaged index.html. NSH's layout is
// depth 3 (web/<app>/<App>/index.html); 4 leaves room for one wrapper level.
const maxWebRootDepth = 4

func newStaticHandler(dir string, log *slog.Logger) *staticHandler {
	h := &staticHandler{dir: dir, log: log}
	if dir == "" {
		return h
	}
	if abs, err := filepath.Abs(dir); err == nil {
		h.dir = abs
	}
	if root, ok := resolveWebRoot(h.dir); ok {
		h.root = root
		h.file = http.FileServer(http.Dir(root))
	}
	return h
}

// resolveWebRoot finds the directory that actually holds index.html.
func resolveWebRoot(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	if info, err := os.Stat(filepath.Join(dir, "index.html")); err == nil && !info.IsDir() {
		return dir, true
	}
	type node struct {
		path  string
		depth int
	}
	queue := []node{{path: dir}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.depth >= maxWebRootDepth {
			continue
		}
		entries, err := os.ReadDir(current.path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules" {
				continue
			}
			child := filepath.Join(current.path, entry.Name())
			if info, err := os.Stat(filepath.Join(child, "index.html")); err == nil && !info.IsDir() {
				return child, true
			}
			queue = append(queue, node{path: child, depth: current.depth + 1})
		}
	}
	return "", false
}

// available reports whether a servable build exists.
func (h *staticHandler) available() bool {
	return h != nil && h.file != nil && h.root != ""
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.available() {
		h.notBuilt(w)
		return
	}
	upath := r.URL.Path
	if upath == "" || upath == "/" {
		h.serveIndex(w, r)
		return
	}
	clean := filepath.Clean("/" + upath)
	target := filepath.Join(h.root, clean)
	if !strings.HasPrefix(target, h.root+string(os.PathSeparator)) && target != h.root {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		// SPA route: a deep link like /sessions/<id> is client-side state.
		h.serveIndex(w, r)
		return
	}
	base := filepath.Base(target)
	if base == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else if strings.Contains(upath, "/assets/") || hashedAsset(base) {
		// Content-hashed assets are immutable; index.html above stays no-cache.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	h.file.ServeHTTP(w, r)
}

func (h *staticHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	r.URL.Path = "/"
	h.file.ServeHTTP(w, r)
}

// hashedAsset heuristically recognizes a build-hashed filename (for example
// "index-C3x9f2.js" or "app.a5226dfa9b.js").
func hashedAsset(name string) bool {
	extension := filepath.Ext(name)
	switch extension {
	case ".js", ".css", ".woff", ".woff2", ".png", ".svg", ".webp", ".avif", ".map":
	default:
		return false
	}
	stem := strings.TrimSuffix(name, extension)
	// Accept both dash-hashed (index-abc123) and dot-hashed (app.abc123) names.
	hash := ""
	if dash := strings.LastIndexByte(stem, '-'); dash > 0 && dash < len(stem)-1 {
		hash = stem[dash+1:]
	} else if dot := strings.LastIndexByte(stem, '.'); dot > 0 && dot < len(stem)-1 {
		hash = stem[dot+1:]
	}
	if len(hash) < 6 || len(hash) > 40 {
		return false
	}
	for _, r := range hash {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func (h *staticHandler) notBuilt(w http.ResponseWriter) {
	dir := h.dir
	if dir == "" {
		dir = "web/dist"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":  "web build not found",
		"detail": "keel serve hosts the NSH web package (nsh package --target web) from " + dir + "; the AgentAPI is still available under /v1",
		"webDir": dir,
	})
}

// webBuildSummary describes the resolved build for boot logs and doctor.
func webBuildSummary(dir string) string {
	if dir == "" {
		return "not configured"
	}
	abs, _ := filepath.Abs(dir)
	if root, ok := resolveWebRoot(abs); ok {
		if root == abs {
			return abs
		}
		return root + " (from " + abs + ")"
	}
	return "missing: " + abs
}
