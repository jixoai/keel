package boardintegration

// Tenant-level board switches (keel-board-v1 design §20 / tasks 10.6, 12.1).
//
// A deployment opts the board in with KEEL_BOARD_ENABLED; a tenant opts in with
// the `board.enabled` and `board.sync.enabled` fields of its keel tenant
// configuration document (internal/host TenantConfig.Board). Both default
// false. The flags are read per request (short cache) so an operator can turn
// the board or its sync surface off without a restart; a read failure is a 503
// and never "allow".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

// BoardFlags is one tenant's effective switches after the deployment switch.
type BoardFlags struct {
	// Enabled gates every /v1/board/* route and every platform webhook.
	Enabled bool
	// Sync gates /v1/board/sync/*, /v1/board/sources/* and /webhooks/*. It
	// requires Enabled.
	Sync bool
}

// FullyEnabled reports whether both switches are on.
func (f BoardFlags) FullyEnabled() bool { return f.Enabled && f.Sync }

// tenantFlagsDocument is the subset of the tenant configuration this package
// reads. It deliberately does not import internal/host: the stored document is
// opaque JSON at the store layer, and only these two switches matter here.
type tenantFlagsDocument struct {
	Board struct {
		Enabled bool `json:"enabled"`
		Sync    struct {
			Enabled bool `json:"enabled"`
		} `json:"sync"`
	} `json:"board"`
}

// ParseBoardFlags reads the board switches from a stored tenant document. An
// empty document is both switches off; malformed JSON is an error (fail
// closed, never a silent allow).
func ParseBoardFlags(document []byte) (BoardFlags, error) {
	if len(strings.TrimSpace(string(document))) == 0 {
		return BoardFlags{}, nil
	}
	var parsed tenantFlagsDocument
	if err := json.Unmarshal(document, &parsed); err != nil {
		return BoardFlags{}, fmt.Errorf("boardintegration: tenant config is not valid JSON: %w", err)
	}
	flags := BoardFlags{Enabled: parsed.Board.Enabled, Sync: parsed.Board.Sync.Enabled}
	if flags.Sync && !flags.Enabled {
		// Sync alone cannot enable the board surface. This mirrors the
		// deployment switch: sync is a narrowing, never an escape hatch.
		flags.Sync = false
	}
	return flags, nil
}

// TenantFlags reads and caches tenant switch documents.
type TenantFlags struct {
	Store *store.Store
	// TTL is how long a read is reused. A negative TTL disables caching (the
	// tests use it to observe a write immediately); zero uses five seconds.
	TTL time.Duration
	// Now is a test seam. Nil uses time.Now.
	Now func() time.Time

	mu    sync.Mutex
	cache map[uuid.UUID]cachedFlags
}

type cachedFlags struct {
	flags   BoardFlags
	expires time.Time
}

// NewTenantFlags builds the reader.
func NewTenantFlags(st *store.Store, ttl time.Duration) *TenantFlags {
	if ttl == 0 {
		ttl = 5 * time.Second
	}
	return &TenantFlags{Store: st, TTL: ttl, cache: make(map[uuid.UUID]cachedFlags)}
}

// Get returns a tenant's switches. A tenant row that doesn't exist is treated
// as "both off": the board is not available for an unknown tenant, and no
// caller can infer existence from a flag read.
func (t *TenantFlags) Get(ctx context.Context, tenantID uuid.UUID) (BoardFlags, error) {
	if t == nil || t.Store == nil {
		return BoardFlags{}, errors.New("boardintegration: tenant flags are not wired")
	}
	if tenantID == uuid.Nil {
		return BoardFlags{}, nil
	}
	if t.TTL >= 0 {
		t.mu.Lock()
		cached, ok := t.cache[tenantID]
		t.mu.Unlock()
		if ok && t.now().Before(cached.expires) {
			return cached.flags, nil
		}
	}
	record, err := t.Store.GetTenantConfig(ctx, tenantID)
	if err != nil {
		if errors.Is(err, store.ErrNoTenant) {
			return BoardFlags{}, nil
		}
		return BoardFlags{}, fmt.Errorf("boardintegration: read tenant flags: %w", err)
	}
	flags, err := ParseBoardFlags(record.Config)
	if err != nil {
		return BoardFlags{}, err
	}
	if t.TTL >= 0 {
		t.mu.Lock()
		t.cache[tenantID] = cachedFlags{flags: flags, expires: t.now().Add(t.TTL)}
		t.mu.Unlock()
	}
	return flags, nil
}

// Invalidate drops one tenant's cached flags after a local write.
func (t *TenantFlags) Invalidate(tenantID uuid.UUID) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.cache, tenantID)
	t.mu.Unlock()
}

func (t *TenantFlags) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// requestTenant returns the authenticated tenant of a board API request.
// ok=false means the request carries no usable principal; the board itself
// answers 401, so the flag gate passes it through rather than inventing a
// tenant.
func requestTenant(r *http.Request) (uuid.UUID, bool) {
	principal, ok := auth.PrincipalFromContext(r.Context())
	if !ok || principal.TenantID == uuid.Nil {
		return uuid.Nil, false
	}
	return principal.TenantID, true
}

// boardSyncPath reports whether path belongs to the sync/source half of the
// board surface, which board.sync.enabled gates.
func boardSyncPath(path string) bool {
	for _, prefix := range []string{"/v1/board/sync", "/v1/board/sources"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// boardBridgePath is the Chat SDK sidecar's signed event route. It carries no
// keel session (the sidecar authenticates with HMAC), so the deployment
// tenant's switches are checked instead of a principal's.
func boardBridgePath(path string) bool {
	const prefix = "/v1/board/sync/bridge/"
	return strings.HasPrefix(path, prefix) && strings.HasSuffix(path, "/events")
}

// syncDisabledHandler answers 404 sync_disabled. Like board_disabled it reveals
// nothing about channels, entries or their existence.
func syncDisabledHandler() http.Handler { return syncDisabled{} }

type syncDisabled struct{}

func (syncDisabled) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	writeDisabled(w, "the board sync is disabled for this tenant", "sync_disabled")
}

func writeDisabled(w http.ResponseWriter, message, code string) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"` + message + `","code":"` + code + `"}`))
}
