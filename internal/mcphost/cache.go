package mcphost

// The document cache: a byte-bounded LRU keyed by content identity
// (appID + content hash). Two properties are load-bearing:
//
//   - the bound is bytes, not entries: two apps with 2 MiB documents and two
//     with 2 KiB documents must not consume the same budget (design §3.2,
//     default 64 MiB);
//   - the cache owns the bytes. The registry keeps no second document map, so
//     the accounting is the truth: an evicted document's memory is released,
//     and a superseded version is reclaimed by eviction rather than lingering
//     as an unaccounted slice.
//
// The current version of every published app is protected: eviction may
// reclaim a superseded version but never the document a live catalogue points
// at, because the view route has no second copy to fall back on. A protected
// document that does not fit is refused (put returns false): the registry then
// drops that app from the catalogue with a diagnostic instead of publishing an
// app whose document cannot be served.

import (
	"container/list"
	"sync"
)

type docCache struct {
	mu       sync.Mutex
	capacity int64
	total    int64
	entries  map[string]*docCacheEntry
	order    *list.List // front = most recently used
	evicted  int
}

type docCacheEntry struct {
	key       string
	document  Document
	size      int64
	protected bool
	elem      *list.Element
}

func newDocCache(capacity int64) *docCache {
	if capacity < 0 {
		capacity = 0
	}
	return &docCache{
		capacity: capacity,
		entries:  make(map[string]*docCacheEntry),
		order:    list.New(),
	}
}

// get returns the cached document and marks it most recently used. The document
// value is immutable (a new version is a new Document), so the caller may keep
// and serve it.
func (c *docCache) get(key string) (Document, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return Document{}, false
	}
	c.order.MoveToFront(entry.elem)
	return entry.document, true
}

// put inserts or refreshes one document. It reports false when the document
// does not fit under the byte cap even after reclaiming every unprotected
// entry.
func (c *docCache) put(key string, document Document, protected bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	size := int64(len(document.Bytes))
	if size <= 0 || size > c.capacity {
		return false
	}
	if entry, ok := c.entries[key]; ok {
		// A key embeds the content hash, so a re-put carries the same content.
		entry.document = document
		entry.protected = entry.protected || protected
		c.order.MoveToFront(entry.elem)
		return true
	}
	if !c.makeRoom(size) {
		return false
	}
	entry := &docCacheEntry{key: key, document: document, size: size, protected: protected}
	entry.elem = c.order.PushFront(entry)
	c.entries[key] = entry
	c.total += size
	return true
}

// makeRoom evicts least recently used unprotected entries until size fits.
func (c *docCache) makeRoom(size int64) bool {
	for c.total+size > c.capacity {
		victim := c.leastRecentlyUsedUnprotected()
		if victim == nil {
			return false
		}
		c.remove(victim)
		c.evicted++
	}
	return true
}

func (c *docCache) leastRecentlyUsedUnprotected() *docCacheEntry {
	for element := c.order.Back(); element != nil; element = element.Prev() {
		entry := element.Value.(*docCacheEntry)
		if !entry.protected {
			return entry
		}
	}
	return nil
}

func (c *docCache) remove(entry *docCacheEntry) {
	c.order.Remove(entry.elem)
	delete(c.entries, entry.key)
	c.total -= entry.size
}

// protect marks one cached key as the current version of a published app (or
// releases it). A missing key is ignored: the registry protects after it puts,
// and a key that did not fit was never inserted.
func (c *docCache) protect(key string, protected bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[key]; ok {
		entry.protected = protected
	}
}

// retain protects exactly the given keys and releases every other entry. It is
// the publish step's one call: the set is the new current version of every
// app, so nothing outside it may stay pinned.
func (c *docCache) retain(keys map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		entry.protected = keys[key]
	}
}

func (c *docCache) cached(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[key]
	return ok
}

func (c *docCache) totalBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func (c *docCache) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *docCache) evictionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evicted
}
