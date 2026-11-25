package main

import (
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/pluginpkg"
)

// Plugin UI hosting (change keel-plugin-core, W1): /plugins/<id>/* serves the UI
// resource package of an ENABLED plugin, on the same origin as the product, so
// the page reaches the `keel.plugin.*` bridge (internal/harnessapi/plugin_bridge.go)
// without CORS.
//
// Authentication: /plugins/ is not the public static app. The auth middleware
// resolves the session cookie, replaces client identity headers and injects the
// principal; an unauthenticated request is 401 before this handler runs. The
// handler reads the tenant from that principal (never from X-Tenant-ID), so a
// forged header cannot reach another tenant's package.
//
// The gate is the lifecycle state, and only the enabled state opens it:
//
//   - not installed            → 404 (the id is not a fact of this deployment);
//   - installed/disabled/failed/invalid → 403 (the operator asked for it to be off);
//   - enabled                  → served, whether or not the plugin has a runtime
//     surface: a UI-only webview plugin is enabled but never assembled, and its
//     UI is exactly the thing that exists to be served.
//
// Containment: the requested path is refused as soon as it contains a ".."
// segment, the resolved file must stay inside the UI root *after* symlinks are
// resolved, and the manifest itself is never served. The UI root is the
// directory of the manifest's provides.ui.entry, which pluginpkg validates to
// be relative and inside the package.
type pluginUIHandler struct {
	manager *harnessext.Manager
	log     *slog.Logger
}

const (
	pluginUIPrefix = "/plugins/"
	// pluginUIManifestName is never served: the UI directory is for the plugin's
	// own assets, and the manifest has a structured API surface.
	pluginUIManifestName = "plugin.yaml"
)

func newPluginUIHandler(manager *harnessext.Manager, log *slog.Logger) *pluginUIHandler {
	return &pluginUIHandler{manager: manager, log: log}
}

// available reports whether plugin UIs can be served at all.
func (h *pluginUIHandler) available() bool { return h != nil && h.manager != nil }

func (h *pluginUIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.fail(w, r, http.StatusMethodNotAllowed, "plugin UI assets are read-only")
		return
	}
	if !h.available() {
		h.fail(w, r, http.StatusNotFound, "plugin management is not wired in this deployment")
		return
	}
	// The tenant is a fact of the authenticated session, never a request header:
	// the edge middleware injected the principal and replaced any client identity
	// header. A missing principal is 401; a foreign tenant gets the same 404 an
	// unknown plugin gets.
	principal, authenticated := principalFrom(r.Context())
	if !authenticated || principal.TenantID == uuid.Nil {
		h.fail(w, r, http.StatusUnauthorized, "authentication required")
		return
	}
	if principal.TenantID != h.manager.TenantID() {
		h.fail(w, r, http.StatusNotFound, "plugin is not installed")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, pluginUIPrefix)
	if rest == "" {
		h.fail(w, r, http.StatusNotFound, "a plugin id is required: /plugins/<id>/")
		return
	}
	id, sub, _ := strings.Cut(rest, "/")
	if !pluginpkg.ValidatePluginID(id) {
		h.fail(w, r, http.StatusNotFound, "no plugin UI at this path")
		return
	}
	// A traversal attempt is refused explicitly (403) rather than merely missed by
	// the join below: the refusal is the contract, and the test can hold onto it.
	if strings.Contains(rest, "..") || strings.Contains(r.URL.Path, "..") || strings.Contains(r.URL.RawPath, "..") {
		h.fail(w, r, http.StatusForbidden, "the path escapes the plugin package")
		return
	}
	view := h.plugin(id)
	if view == nil {
		h.fail(w, r, http.StatusNotFound, "plugin is not installed")
		return
	}
	if view.State != harnessext.LifecycleEnabled {
		h.fail(w, r, http.StatusForbidden,
			"plugin "+id+" is "+string(view.State)+": only an enabled plugin's UI is hosted")
		return
	}
	// A declared chunk entry is served from the package root first: a pure chunk
	// package has no page of its own, and a webview package may keep its chunk
	// outside the UI entry's directory.
	if target, ok := chunkEntryTarget(view, sub); ok {
		if view.Path == "" {
			h.fail(w, r, http.StatusNotFound, "plugin "+id+" has no managed package directory")
			return
		}
		file := filepath.Join(view.Path, filepath.FromSlash(target))
		if !pluginpkg.IsInside(view.Path, file) {
			h.fail(w, r, http.StatusForbidden, "the path escapes the plugin package")
			return
		}
		h.serveFile(w, r, file, view.Path, file)
		return
	}
	ui := view.Provides.UI
	if ui == nil || strings.TrimSpace(ui.Entry) == "" {
		h.fail(w, r, http.StatusNotFound, "plugin "+id+" declares no UI page; load one of its declared chunk entries")
		return
	}
	if view.Path == "" {
		// A bundled plugin has no package directory; its UI would have to be
		// embedded some other way, and today none is.
		h.fail(w, r, http.StatusNotFound, "plugin "+id+" has no managed package directory")
		return
	}
	entry, uiRoot, ok := resolvePluginUI(view.Path, ui.Entry)
	if !ok {
		h.fail(w, r, http.StatusForbidden, "the declared UI entry is outside the plugin package")
		return
	}
	target, ok := h.resolveTarget(w, r, id, sub, entry, uiRoot)
	if !ok {
		return
	}
	if target == "" {
		h.fail(w, r, http.StatusNotFound, "not found")
		return
	}
	h.serveFile(w, r, target, uiRoot, entry)
}

// plugin returns the inventory row of id, or nil.
func (h *pluginUIHandler) plugin(id string) *harnessext.PluginView {
	for _, view := range h.manager.Snapshot().Plugins {
		if view.ID == id {
			copied := view
			return &copied
		}
	}
	return nil
}

// chunkEntryTarget resolves a request remainder to a declared chunk/package
// entry (package-relative). Only the exact declared entries are exposed: a
// chunk that needs siblings must be bundled into one file, so the hosting
// surface stays the declaration, not the package tree.
func chunkEntryTarget(view *harnessext.PluginView, sub string) (string, bool) {
	if view == nil {
		return "", false
	}
	clean := path.Clean("/" + sub)
	if clean == "/" {
		return "", false
	}
	requested := strings.TrimPrefix(clean, "/")
	for _, contribution := range view.Contributes {
		if contribution.Form != "chunk" && contribution.Form != "package" {
			continue
		}
		entry := path.Clean(strings.TrimPrefix(filepath.ToSlash(contribution.Entry), "./"))
		if entry == requested {
			return entry, true
		}
	}
	return "", false
}

// resolvePluginUI resolves the declared entry to a file and its hosting root.
// Both are required to stay inside the package (the manifest's own validation
// already refuses "..", so a failure here is a package edited on disk behind
// the manager's back).
func resolvePluginUI(packageDir, entry string) (string, string, bool) {
	entryPath := filepath.Join(packageDir, filepath.FromSlash(entry))
	uiRoot := filepath.Dir(entryPath)
	if !pluginpkg.IsInside(packageDir, entryPath) || !pluginpkg.IsInside(packageDir, uiRoot) {
		return "", "", false
	}
	return entryPath, uiRoot, true
}

// resolveTarget maps the request remainder onto a file inside the UI root.
// It answers false when it has already written the response (a redirect), and
// an empty target when there is nothing to serve.
func (h *pluginUIHandler) resolveTarget(w http.ResponseWriter, r *http.Request, id, sub, entry, uiRoot string) (target string, ok bool) {
	if sub == "" && !strings.HasSuffix(r.URL.Path, "/") {
		// /plugins/<id> → /plugins/<id>/ : relative asset URLs in the page need
		// the trailing slash to resolve under the plugin, not the site root.
		http.Redirect(w, r, pluginUIPrefix+id+"/"+querySuffix(r), http.StatusMovedPermanently)
		return "", false
	}
	if sub == "" {
		return entry, true
	}
	clean := path.Clean("/" + sub)
	if clean == "/" {
		return entry, true
	}
	candidate := filepath.Join(uiRoot, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	if !pluginpkg.IsInside(uiRoot, candidate) {
		h.fail(w, r, http.StatusForbidden, "the path escapes the plugin package")
		return "", false
	}
	if filepath.Base(candidate) == pluginUIManifestName {
		h.fail(w, r, http.StatusForbidden, "the plugin manifest is not part of the UI")
		return "", false
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", true
	}
	if info.IsDir() {
		// A directory is served by its index.html (when it has one); there is no
		// listing.
		index := filepath.Join(candidate, "index.html")
		if info, err := os.Stat(index); err != nil || info.IsDir() {
			return "", true
		}
		return index, true
	}
	return candidate, true
}

// serveFile sends one file after resolving symlinks and re-checking containment,
// so a link planted inside the package cannot read the host filesystem.
func (h *pluginUIHandler) serveFile(w http.ResponseWriter, r *http.Request, target, uiRoot, entry string) {
	resolvedRoot, err := filepath.EvalSymlinks(uiRoot)
	if err != nil {
		h.fail(w, r, http.StatusNotFound, "not found")
		return
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		h.fail(w, r, http.StatusNotFound, "not found")
		return
	}
	if !pluginpkg.IsInside(resolvedRoot, resolvedTarget) {
		h.log.Warn("plugin UI request refused: the file resolves outside the package", "plugin_root", resolvedRoot, "target", resolvedTarget)
		h.fail(w, r, http.StatusForbidden, "the file resolves outside the plugin package")
		return
	}
	file, err := os.Open(resolvedTarget)
	if err != nil {
		h.fail(w, r, http.StatusNotFound, "not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		h.fail(w, r, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// The CSP keeps a plugin page on this origin: it may fetch the bridge and its
	// own assets, and nothing else (a plugin that needs the network needs a
	// sidecar with a declared permission, per the spec's permission model).
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; "+
			"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'self'")
	switch {
	case filepath.Clean(resolvedTarget) == filepath.Clean(entry) || filepath.Base(resolvedTarget) == "index.html":
		w.Header().Set("Cache-Control", "no-cache")
	case hashedAsset(filepath.Base(resolvedTarget)):
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		w.Header().Set("Cache-Control", "no-cache")
	}
	if contentType := pluginUIContentType(resolvedTarget); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

// fail writes the JSON 404/403/405 of the plugin UI route. A UI host never
// renders an HTML error page: the caller is either a page's fetch or an
// operator's curl.
func (h *pluginUIHandler) fail(w http.ResponseWriter, r *http.Request, status int, detail string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": detail, "path": r.URL.Path})
}

// pluginUIContentType resolves an explicit content type for the asset types a
// plugin UI ships. An unknown extension falls through to ServeContent's sniffing.
func pluginUIContentType(name string) string {
	extension := strings.ToLower(filepath.Ext(name))
	switch extension {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".wasm":
		return "application/wasm"
	case ".txt":
		return "text/plain; charset=utf-8"
	}
	return mime.TypeByExtension(extension)
}

// querySuffix preserves the query string of a redirect.
func querySuffix(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}
