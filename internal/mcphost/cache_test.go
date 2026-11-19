package mcphost

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/mcp"
)

// TestDocCacheByteBoundAndLRUEviction pins the two properties the document
// cache exists for: the bound is bytes (not entries) and eviction is least
// recently used.
func TestDocCacheByteBoundAndLRUEviction(t *testing.T) {
	cache := newDocCache(100)
	if !cache.put("a", sizedDoc(40), false) || !cache.put("b", sizedDoc(40), false) {
		t.Fatal("the first two documents did not fit")
	}
	if cache.totalBytes() != 80 {
		t.Fatalf("total = %d, want 80", cache.totalBytes())
	}
	// a is least recently used; inserting c (40) must reclaim it.
	if !cache.put("c", sizedDoc(40), false) {
		t.Fatal("c did not fit after eviction")
	}
	if cache.totalBytes() != 80 || cache.count() != 2 {
		t.Fatalf("total=%d count=%d, want 80/2", cache.totalBytes(), cache.count())
	}
	if cache.cached("a") {
		t.Fatal("a survived; the LRU victim was not the oldest entry")
	}
	if !cache.cached("b") || !cache.cached("c") {
		t.Fatal("b/c were evicted instead of a")
	}
	if cache.evictionCount() != 1 {
		t.Fatalf("evictions = %d, want 1", cache.evictionCount())
	}

	// Touch b, then insert d: c is now the LRU victim.
	if _, ok := cache.get("b"); !ok {
		t.Fatal("b is not cached")
	}
	if !cache.put("d", sizedDoc(40), false) {
		t.Fatal("d did not fit")
	}
	if cache.cached("c") {
		t.Fatal("c survived although it was the least recently used")
	}
	if !cache.cached("b") || !cache.cached("d") {
		t.Fatal("touching b did not protect it from the eviction")
	}
}

// TestDocCacheProtectsCurrentVersion pins that a pinned (current) document is
// never the eviction victim and that a document larger than the whole budget is
// refused instead of silently consuming it.
func TestDocCacheProtectsCurrentVersion(t *testing.T) {
	cache := newDocCache(100)
	if !cache.put("current", sizedDoc(60), true) {
		t.Fatal("current did not fit")
	}
	if !cache.put("stale", sizedDoc(30), false) {
		t.Fatal("stale did not fit")
	}
	// 60 + 30 = 90; inserting 30 more needs 30, so the unprotected stale entry
	// is the only candidate.
	if !cache.put("next", sizedDoc(30), false) {
		t.Fatal("next did not fit")
	}
	if !cache.cached("current") {
		t.Fatal("the protected current document was evicted")
	}
	if cache.cached("stale") {
		t.Fatal("the unprotected document was not evicted")
	}
	if cache.totalBytes() != 90 {
		t.Fatalf("total = %d, want 90", cache.totalBytes())
	}

	// A protected document that cannot fit even after reclaiming everything is
	// refused (the registry turns this into a "cache" diagnostic).
	big := newDocCache(100)
	if !big.put("a", sizedDoc(60), true) || !big.put("b", sizedDoc(30), true) {
		t.Fatal("setup did not fit")
	}
	if big.put("c", sizedDoc(40), true) {
		t.Fatal("a protected document over the budget was accepted")
	}
	if big.totalBytes() != 90 || big.cached("c") {
		t.Fatalf("refused insert changed the cache: total=%d cached=%v", big.totalBytes(), big.cached("c"))
	}
	if big.put("huge", sizedDoc(101), true) {
		t.Fatal("a document larger than the cache was accepted")
	}
}

// TestDocCacheRetainReleasesSupersededVersions pins the publish-step call: only
// the new current set stays pinned.
func TestDocCacheRetainReleasesSupersededVersions(t *testing.T) {
	cache := newDocCache(200)
	cache.put("old", sizedDoc(50), true)
	cache.put("new", sizedDoc(50), true)
	cache.retain(map[string]bool{"new": true})
	// old is now evictable; adding 150 must reclaim it.
	if !cache.put("other", sizedDoc(150), true) {
		t.Fatal("other did not fit")
	}
	if cache.cached("old") {
		t.Fatal("the superseded version stayed pinned")
	}
	if !cache.cached("new") {
		t.Fatal("the retained version was evicted")
	}
}

// sizedDoc builds an immutable document with n bytes of content.
func sizedDoc(size int) Document {
	return Document{Hash: strings.Repeat("0", 64) + string(rune('0'+size%10)), Bytes: bytes.Repeat([]byte{0x61}, size)}
}

// TestContentIdentity pins the derived identity rules the URL and the cache key
// depend on: the app id is derived from (server, uri), and the document URL
// carries the content hash.
func TestContentIdentity(t *testing.T) {
	appID := mcp.AppIDFor("weather", "ui://weather/dashboard")
	if !strings.HasPrefix(appID, "mcpapp_") || len(appID) != len("mcpapp_")+16 {
		t.Fatalf("appId = %q", appID)
	}
	if appID != mcp.AppIDFor("weather", "ui://weather/dashboard") {
		t.Fatal("appId is not stable")
	}
	if appID == mcp.AppIDFor("weather", "ui://weather/other") {
		t.Fatal("appId does not depend on the resource uri")
	}
	if appID == mcp.AppIDFor("other", "ui://weather/dashboard") {
		t.Fatal("appId does not depend on the server")
	}
	hashOne := contentHash([]byte("<html>one</html>"))
	hashTwo := contentHash([]byte("<html>two</html>"))
	if hashOne == hashTwo || len(hashOne) != 64 {
		t.Fatalf("content hash = %q / %q", hashOne, hashTwo)
	}
	if got := documentURL(appID, hashOne); got != "/v1/mcp/apps/"+appID+"/view?v="+hashOne {
		t.Fatalf("documentURL = %q", got)
	}
}
