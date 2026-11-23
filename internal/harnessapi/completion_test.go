package harnessapi

// The session manifest route's wire contract (change keel-harness-v2, W4
// follow-ups): each row carries the manifest's own content hash, and the list
// is a bounded path-cursor page instead of one capped answer the client cannot
// page. The fake manifest keeps the test honest about what the handler adds —
// nothing is invented for a row that carries no hash.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/layerfs"
)

type sessionFilesPage struct {
	Files      []SessionFileView `json:"files"`
	Count      int               `json:"count"`
	Limit      int               `json:"limit"`
	Truncated  bool              `json:"truncated"`
	HasMore    bool              `json:"hasMore"`
	NextCursor string            `json:"nextCursor"`
}

func manifestEntry(path string, hash layerfs.Hash, size int64, seq int64) fileplugin.Entry {
	return fileplugin.Entry{Path: path, Hash: hash, Size: size, UpdatedSeq: seq}
}

func TestSessionFilesRoutePagesByPathAndCarriesTheManifestHash(t *testing.T) {
	tenant := uuid.New()
	session := uuid.New()
	hashA := layerfs.Hash(strings.Repeat("a", 64))
	hashB := layerfs.Hash(strings.Repeat("b", 64))
	hashD := layerfs.Hash(strings.Repeat("d", 64))
	hashE := layerfs.Hash(strings.Repeat("e", 64))
	server := settingsServer(t, func(cfg *Config) {
		cfg.SessionFiles = fakeSessionFiles{entries: []fileplugin.Entry{
			manifestEntry("src/e.ts", hashE, 5, 9),
			manifestEntry("src/c.ts", "", 3, 7), // no hash: the route invents none
			manifestEntry("src/a.ts", hashA, 1, 5),
			manifestEntry("src/d.ts", hashD, 4, 8),
			manifestEntry("src/b.ts", hashB, 2, 6),
		}}
		cfg.SessionExists = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil }
	})
	client := server.Client()
	base := server.URL + "/v1/sessions/" + session.String() + "/files"

	var first sessionFilesPage
	if code := doJSON(t, client, http.MethodGet, base+"?limit=2", tenant, nil, &first); code != 200 {
		t.Fatalf("first page = %d", code)
	}
	if len(first.Files) != 2 || first.Files[0].Path != "src/a.ts" || first.Files[1].Path != "src/b.ts" {
		t.Fatalf("first page files = %+v", first.Files)
	}
	if first.Files[0].Hash != hashA.Prefixed() || first.Files[1].Hash != hashB.Prefixed() {
		t.Fatalf("hashes = %q %q", first.Files[0].Hash, first.Files[1].Hash)
	}
	if !first.HasMore || !first.Truncated || first.NextCursor != "src/b.ts" {
		t.Fatalf("first page bounds = %+v", first)
	}

	var second sessionFilesPage
	if code := doJSON(t, client, http.MethodGet, base+"?limit=2&cursor="+first.NextCursor, tenant, nil, &second); code != 200 {
		t.Fatalf("second page = %d", code)
	}
	if len(second.Files) != 2 || second.Files[0].Path != "src/c.ts" || second.Files[1].Path != "src/d.ts" {
		t.Fatalf("second page files = %+v", second.Files)
	}
	// A row with no manifest hash stays without one: the route does not mint a
	// plausible digest for it.
	if second.Files[0].Hash != "" {
		t.Fatalf("hash invented for a hashless row: %q", second.Files[0].Hash)
	}
	if second.NextCursor != "src/d.ts" || !second.HasMore {
		t.Fatalf("second page bounds = %+v", second)
	}

	var last sessionFilesPage
	if code := doJSON(t, client, http.MethodGet, base+"?limit=2&cursor="+second.NextCursor, tenant, nil, &last); code != 200 {
		t.Fatalf("last page = %d", code)
	}
	if len(last.Files) != 1 || last.Files[0].Path != "src/e.ts" {
		t.Fatalf("last page files = %+v", last.Files)
	}
	if last.HasMore || last.Truncated || last.NextCursor != "" {
		t.Fatalf("last page bounds = %+v", last)
	}

	// The cursor is exclusive: asking again past the last path is an empty
	// page, not a repeat of the final row.
	var beyond sessionFilesPage
	if code := doJSON(t, client, http.MethodGet, base+"?limit=2&cursor=src/e.ts", tenant, nil, &beyond); code != 200 {
		t.Fatalf("beyond page = %d", code)
	}
	if len(beyond.Files) != 0 || beyond.HasMore {
		t.Fatalf("beyond page = %+v", beyond)
	}

	// A cursor the manifest no longer holds is a valid boundary (a row was
	// deleted between pages): the page starts after it.
	var shifted sessionFilesPage
	if code := doJSON(t, client, http.MethodGet, base+"?limit=2&cursor=src/az.ts", tenant, nil, &shifted); code != 200 {
		t.Fatalf("shifted page = %d", code)
	}
	if len(shifted.Files) != 2 || shifted.Files[0].Path != "src/b.ts" {
		t.Fatalf("shifted page = %+v", shifted.Files)
	}

	// The bounds stay the route's: an overlong cursor or an out-of-range limit
	// is a 400, never a silent clamp or an unbounded read.
	var refusal map[string]any
	if code := doJSON(t, client, http.MethodGet, base+"?cursor="+strings.Repeat("x", MaxSessionFileCursorLen+1), tenant, nil, &refusal); code != http.StatusBadRequest {
		t.Fatalf("overlong cursor = %d %+v", code, refusal)
	}
	if code := doJSON(t, client, http.MethodGet, base+"?limit=0", tenant, nil, &refusal); code != http.StatusBadRequest {
		t.Fatalf("zero limit = %d %+v", code, refusal)
	}
	if code := doJSON(t, client, http.MethodGet, base+"?limit=1001", tenant, nil, &refusal); code != http.StatusBadRequest {
		t.Fatalf("oversized limit = %d %+v", code, refusal)
	}
}
