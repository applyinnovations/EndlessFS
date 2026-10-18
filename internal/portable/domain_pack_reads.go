package portable

import (
	"container/list"
	"context"
	"errors"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// The coordinator belongs to one engine/backend. In-flight work shares decoded
// immutable pages; the bounded idle cache retains only authenticated compressed
// bytes and an opaque prior-validation memo. Fresh sessions verify exact wire
// identity and decode into their own pages with the same binding.
// Mutable heads, leases, errors and publication-presence checks never enter it.
type consistencyDomainPackReads struct {
	mu                sync.Mutex
	loads             map[consistencyDomainPackReadKey]*consistencyDomainPackLoad
	cacheMaximumBytes int
	cacheBytes        int
	cache             map[consistencyDomainPackCacheKey]*list.Element
	lru               list.List
	stats             consistencyDomainPackCacheSnapshot
}

func (reads *consistencyDomainPackReads) read(ctx context.Context, key consistencyDomainPackCacheKey, fetch func() (map[string]storageformat.DomainPage, error)) (map[string]storageformat.DomainPage, error) {
	readKey := consistencyDomainPackReadKey{key: key, fresh: metadataPackCacheBypassed(ctx)}
	for {
		if err := ctx.Err(); err != nil {
			return nil, domain.WrapError(domain.ErrorUnavailable, "consistency-domain pack read canceled", err)
		}
		reads.mu.Lock()
		if load := reads.loads[readKey]; load != nil {
			reads.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, domain.WrapError(domain.ErrorUnavailable, "consistency-domain pack read canceled", ctx.Err())
			case <-load.done:
				// A canceled leader does not cancel independent callers. Retry
				// through their own context; all-waiter cancellation leaves no
				// detached provider work or background goroutine.
				if errors.Is(load.err, context.Canceled) || errors.Is(load.err, context.DeadlineExceeded) {
					continue
				}
				return load.pages, load.err
			}
		}
		load := &consistencyDomainPackLoad{done: make(chan struct{})}
		if reads.loads == nil {
			reads.loads = make(map[consistencyDomainPackReadKey]*consistencyDomainPackLoad)
		}
		reads.loads[readKey] = load
		reads.mu.Unlock()
		load.pages, load.err = fetch()
		reads.mu.Lock()
		delete(reads.loads, readKey)
		close(load.done)
		reads.mu.Unlock()
		return load.pages, load.err
	}
}

// Cache-owned wire bytes and entries are bounded independently. Active requests
// can still pin an evicted body and own decoded pages; this is not a global RSS
// bound. No background work or TTL is required for immutable authenticated data.
const metadataPackCacheMaximumBytes = 128 << 20
const metadataPackCacheMaximumEntries = 256

type consistencyDomainPackReadKey struct {
	key   consistencyDomainPackCacheKey
	fresh bool
}
type consistencyDomainPackCacheKey struct {
	key      objectstore.Key
	domainID string
	kind     storageformat.ConsistencyDomainKind
	packID   string
}
type consistencyDomainPackCacheEntry struct {
	key   consistencyDomainPackCacheKey
	body  []byte
	proof storageformat.DomainPagePackWireProof
}
type consistencyDomainPackCacheSnapshot struct {
	Hits, Misses, Bypasses, Evictions uint64
	Bytes, Entries                    int
}
type metadataPackCacheBypassKey struct{}

func withoutMetadataPackCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, metadataPackCacheBypassKey{}, true)
}
func metadataPackCacheBypassed(ctx context.Context) bool {
	value, _ := ctx.Value(metadataPackCacheBypassKey{}).(bool)
	return value
}

func newConsistencyDomainPackReads(maximumBytes int) *consistencyDomainPackReads {
	return &consistencyDomainPackReads{cacheMaximumBytes: maximumBytes}
}

func (reads *consistencyDomainPackReads) cachedBody(ctx context.Context, key consistencyDomainPackCacheKey) ([]byte, storageformat.DomainPagePackWireProof, bool) {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	if metadataPackCacheBypassed(ctx) || reads.cacheMaximumBytes <= 0 {
		reads.stats.Bypasses++
		return nil, storageformat.DomainPagePackWireProof{}, false
	}
	if element := reads.cache[key]; element != nil {
		reads.lru.MoveToFront(element)
		reads.stats.Hits++
		entry := element.Value.(consistencyDomainPackCacheEntry)
		return entry.body, entry.proof, true
	}
	reads.stats.Misses++
	return nil, storageformat.DomainPagePackWireProof{}, false
}

// Only the strict decoder's successful owned provider wire/proof may be admitted.
// Copying prevents a backend from mutating its returned buffer after validation.
func (reads *consistencyDomainPackReads) retainBody(ctx context.Context, key consistencyDomainPackCacheKey, body []byte, proof storageformat.DomainPagePackWireProof) {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	if !reads.cacheable(ctx, len(body)) {
		return
	}
	if reads.cache == nil {
		reads.cache = make(map[consistencyDomainPackCacheKey]*list.Element)
	}
	if element := reads.cache[key]; element != nil {
		reads.removeCachedBody(element)
	}
	for reads.cacheBytes+len(body) > reads.cacheMaximumBytes || reads.lru.Len() >= metadataPackCacheMaximumEntries {
		reads.removeCachedBody(reads.lru.Back())
	}
	entry := consistencyDomainPackCacheEntry{key: key, body: append([]byte(nil), body...), proof: proof}
	reads.cache[key] = reads.lru.PushFront(entry)
	reads.cacheBytes += len(body)
}

// The configured byte limit is immutable after coordinator construction.
func (reads *consistencyDomainPackReads) cacheable(ctx context.Context, size int) bool {
	return ctx.Err() == nil && !metadataPackCacheBypassed(ctx) && size > 0 && size <= storageformat.MaxDomainPagePackBytes && size <= reads.cacheMaximumBytes
}

func (reads *consistencyDomainPackReads) removeCachedBody(element *list.Element) {
	entry := element.Value.(consistencyDomainPackCacheEntry)
	reads.cacheBytes -= len(entry.body)
	delete(reads.cache, entry.key)
	reads.lru.Remove(element)
	reads.stats.Evictions++
}
func (reads *consistencyDomainPackReads) discardBody(key consistencyDomainPackCacheKey) {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	if element := reads.cache[key]; element != nil {
		reads.removeCachedBody(element)
	}
}
func (reads *consistencyDomainPackReads) snapshot() consistencyDomainPackCacheSnapshot {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	result := reads.stats
	result.Bytes = reads.cacheBytes
	result.Entries = reads.lru.Len()
	return result
}
