package portable

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/providerbudget"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// The architecture sensitivity run compared 16, 64, 256, and 1,024 entries
// per page at 4,096 entries. The project's documented priority order selects
// 256: it minimized provider requests, cost, and critical latency while 1,024
// was strictly dominated by the same request shape and materially more bytes.
// Exact canonical encoding remains the second bound, so large values split a
// page before this item limit and no provider-specific object limit leaks into
// the format.
const domainPageMaximumItems = 256

type domainPageRef struct {
	root     storageformat.DomainTreeRoot
	firstKey string
	lastKey  string
}

type consistencyDomainTreeSession struct {
	store     *consistencyDomainStore
	reference consistencyDomainRef
	mu        sync.RWMutex
	pages     map[string]storageformat.DomainPage
	known     map[string]storageformat.DomainTreeRoot
	pageKey   func(string) objectstore.Key
}

func newConsistencyDomainTreeSession(store *consistencyDomainStore, reference consistencyDomainRef) *consistencyDomainTreeSession {
	return &consistencyDomainTreeSession{
		store: store, reference: reference, pages: make(map[string]storageformat.DomainPage), known: make(map[string]storageformat.DomainTreeRoot),
		pageKey: func(digest string) objectstore.Key {
			return storageformat.DomainPageKey(reference.Kind, reference.ID, digest)
		},
	}
}

func (session *consistencyDomainTreeSession) markKnown(root storageformat.DomainTreeRoot) {
	if root.Digest != "" {
		session.mu.Lock()
		defer session.mu.Unlock()
		session.known[root.Digest] = root
	}
}

func (session *consistencyDomainTreeSession) forgetPage(digest string) {
	session.mu.Lock()
	defer session.mu.Unlock()
	delete(session.pages, digest)
}

func newDomainCatalogTreeSession(store *consistencyDomainStore) *consistencyDomainTreeSession {
	reference := consistencyDomainRef{Kind: storageformat.DomainAdmin, ID: "__catalog__"}
	session := newConsistencyDomainTreeSession(store, reference)
	session.pageKey = storageformat.DomainCatalogPageKey
	return session
}

func (session *consistencyDomainTreeSession) readPage(ctx context.Context, expected domainPageRef) (storageformat.DomainPage, error) {
	if expected.root.Digest == "" {
		return storageformat.DomainPage{}, domain.NewError(domain.ErrorInvalid, "empty consistency-domain page reference")
	}
	session.mu.RLock()
	cachedPage, found := session.pages[expected.root.Digest]
	session.mu.RUnlock()
	if found {
		if err := verifyConsistencyDomainPageRef(cachedPage, expected); err != nil {
			return storageformat.DomainPage{}, err
		}
		return cachedPage, nil
	}
	key := session.pageKey(expected.root.Digest)
	object, err := session.store.backend.Get(ctx, key)
	if err != nil {
		return storageformat.DomainPage{}, err
	}
	var page storageformat.DomainPage
	if err := decodeCanonicalValue(object.Body, &page); err != nil {
		return storageformat.DomainPage{}, err
	}
	if page.DomainID != session.reference.ID || page.Kind != session.reference.Kind {
		return storageformat.DomainPage{}, domain.NewError(domain.ErrorInvalid, "consistency-domain page key binding mismatch")
	}
	if err := storageformat.ValidateDomainPage(page, expected.root.Digest); err != nil {
		return storageformat.DomainPage{}, err
	}
	if err := verifyConsistencyDomainPageRef(page, expected); err != nil {
		return storageformat.DomainPage{}, err
	}
	session.mu.Lock()
	session.pages[expected.root.Digest] = page
	session.mu.Unlock()
	return page, nil
}

func verifyConsistencyDomainPageRef(page storageformat.DomainPage, expected domainPageRef) error {
	actual, err := consistencyDomainPageDescriptor(page, expected.root.Digest)
	if err != nil {
		return err
	}
	if actual.root != expected.root || expected.firstKey != "" && actual.firstKey != expected.firstKey || expected.lastKey != "" && actual.lastKey != expected.lastKey {
		return domain.NewError(domain.ErrorInvalid, "consistency-domain child descriptor mismatch")
	}
	return nil
}

func consistencyDomainPageDescriptor(page storageformat.DomainPage, digest string) (domainPageRef, error) {
	reference := domainPageRef{root: storageformat.DomainTreeRoot{Digest: digest, Level: page.Level}}
	if page.Level == 0 {
		reference.root.EntryCount = uint64(len(page.Entries))
		if len(page.Entries) > 0 {
			reference.firstKey = page.Entries[0].Key
			reference.lastKey = page.Entries[len(page.Entries)-1].Key
		}
		for _, entry := range page.Entries {
			if uint64(len(entry.Value)) > math.MaxUint64-reference.root.ByteCount {
				return domainPageRef{}, domain.NewError(domain.ErrorInvalid, "consistency-domain byte count overflows")
			}
			reference.root.ByteCount += uint64(len(entry.Value))
		}
		return reference, nil
	}
	if len(page.Children) == 0 {
		return domainPageRef{}, domain.NewError(domain.ErrorInvalid, "consistency-domain branch has no children")
	}
	reference.firstKey = page.Children[0].FirstKey
	reference.lastKey = page.Children[len(page.Children)-1].LastKey
	for _, child := range page.Children {
		if child.EntryCount > math.MaxUint64-reference.root.EntryCount || child.ByteCount > math.MaxUint64-reference.root.ByteCount {
			return domainPageRef{}, domain.NewError(domain.ErrorInvalid, "consistency-domain aggregate overflows")
		}
		reference.root.EntryCount += child.EntryCount
		reference.root.ByteCount += child.ByteCount
	}
	return reference, nil
}

func (session *consistencyDomainTreeSession) lookup(ctx context.Context, root storageformat.DomainTreeRoot, key string) (consistencyDomainValue, bool, error) {
	if root.Digest == "" {
		return consistencyDomainValue{}, false, nil
	}
	reference := domainPageRef{root: root}
	for {
		page, err := session.readPage(ctx, reference)
		if err != nil {
			return consistencyDomainValue{}, false, err
		}
		if page.Level == 0 {
			index := sort.Search(len(page.Entries), func(index int) bool { return page.Entries[index].Key >= key })
			if index == len(page.Entries) || page.Entries[index].Key != key {
				return consistencyDomainValue{}, false, nil
			}
			entry := page.Entries[index]
			return consistencyDomainValue{Data: append([]byte(nil), entry.Value...), LogicalVersion: entry.LogicalVersion}, true, nil
		}
		index := consistencyDomainChildIndex(page.Children, key)
		if index < 0 || page.Children[index].FirstKey > key || page.Children[index].LastKey < key {
			return consistencyDomainValue{}, false, nil
		}
		child := page.Children[index]
		reference = domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey}
	}
}

func (session *consistencyDomainTreeSession) collect(ctx context.Context, root storageformat.DomainTreeRoot, prefix, after string, limit int) ([]storageformat.DomainEntry, error) {
	if root.Digest == "" || limit <= 0 {
		return nil, nil
	}
	result := make([]storageformat.DomainEntry, 0, limit)
	var visit func(domainPageRef) error
	visit = func(reference domainPageRef) error {
		if len(result) >= limit || reference.lastKey != "" && reference.lastKey <= after {
			return nil
		}
		page, err := session.readPage(ctx, reference)
		if err != nil {
			return err
		}
		if page.Level == 0 {
			for _, entry := range page.Entries {
				if entry.Key <= after {
					continue
				}
				if prefix != "" && !strings.HasPrefix(entry.Key, prefix) {
					if entry.Key > prefix {
						return nil
					}
					continue
				}
				result = append(result, storageformat.DomainEntry{Key: entry.Key, Value: append([]byte(nil), entry.Value...), LogicalVersion: entry.LogicalVersion})
				if len(result) == limit {
					return nil
				}
			}
			return nil
		}
		for _, child := range page.Children {
			if len(result) == limit {
				break
			}
			if child.LastKey <= after || prefix != "" && child.LastKey < prefix {
				continue
			}
			if prefix != "" && child.FirstKey > prefix && !strings.HasPrefix(child.FirstKey, prefix) {
				break
			}
			if err := visit(domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(domainPageRef{root: root}); err != nil {
		return nil, err
	}
	return result, nil
}

func (session *consistencyDomainTreeSession) collectOrdered(ctx context.Context, root storageformat.DomainTreeRoot, bound string, limit int, descending bool) ([]storageformat.DomainEntry, error) {
	if !descending {
		return session.collect(ctx, root, "", bound, limit)
	}
	if root.Digest == "" || limit <= 0 {
		return nil, nil
	}
	result := make([]storageformat.DomainEntry, 0, limit)
	var visit func(domainPageRef) error
	visit = func(reference domainPageRef) error {
		if len(result) >= limit || bound != "" && reference.firstKey != "" && reference.firstKey >= bound {
			return nil
		}
		page, err := session.readPage(ctx, reference)
		if err != nil {
			return err
		}
		if page.Level == 0 {
			for index := len(page.Entries) - 1; index >= 0; index-- {
				entry := page.Entries[index]
				if bound != "" && entry.Key >= bound {
					continue
				}
				result = append(result, storageformat.DomainEntry{Key: entry.Key, Value: append([]byte(nil), entry.Value...), LogicalVersion: entry.LogicalVersion})
				if len(result) == limit {
					return nil
				}
			}
			return nil
		}
		for index := len(page.Children) - 1; index >= 0; index-- {
			child := page.Children[index]
			if bound != "" && child.FirstKey >= bound {
				continue
			}
			if err := visit(domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey}); err != nil {
				return err
			}
			if len(result) == limit {
				break
			}
		}
		return nil
	}
	if err := visit(domainPageRef{root: root}); err != nil {
		return nil, err
	}
	return result, nil
}

func consistencyDomainChildIndex(children []storageformat.DomainPageChild, key string) int {
	if len(children) == 0 {
		return -1
	}
	index := sort.Search(len(children), func(index int) bool { return children[index].LastKey >= key })
	if index == len(children) {
		return len(children) - 1
	}
	return index
}

func (session *consistencyDomainTreeSession) apply(ctx context.Context, root storageformat.DomainTreeRoot, changes []storageformat.DomainChange) (storageformat.DomainTreeRoot, error) {
	if len(changes) == 0 {
		return root, nil
	}
	changes = append([]storageformat.DomainChange(nil), changes...)
	sort.Slice(changes, func(left, right int) bool { return changes[left].Key < changes[right].Key })
	if root.Digest == "" {
		entries := make([]storageformat.DomainEntry, 0, len(changes))
		for _, change := range changes {
			if change.Delete {
				continue
			}
			entries = append(entries, storageformat.DomainEntry{Key: change.Key, Value: append([]byte(nil), change.Value...), LogicalVersion: change.LogicalVersion})
		}
		return session.buildTree(ctx, entries)
	}
	page, err := session.readPage(ctx, domainPageRef{root: root})
	if err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	replacements, err := session.rewrite(ctx, page, changes)
	if err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	if len(replacements) == 0 {
		return storageformat.DomainTreeRoot{}, nil
	}
	for len(replacements) > 1 {
		replacements, err = session.writeBranchGroups(ctx, replacements[0].root.Level+1, replacements)
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
	}
	return replacements[0].root, nil
}

func (session *consistencyDomainTreeSession) buildTree(ctx context.Context, entries []storageformat.DomainEntry) (storageformat.DomainTreeRoot, error) {
	if len(entries) == 0 {
		return storageformat.DomainTreeRoot{}, nil
	}
	pages, err := session.writeLeafGroups(ctx, entries)
	if err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	for len(pages) > 1 {
		pages, err = session.writeBranchGroups(ctx, pages[0].root.Level+1, pages)
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
	}
	return pages[0].root, nil
}

func (session *consistencyDomainTreeSession) rewrite(ctx context.Context, page storageformat.DomainPage, changes []storageformat.DomainChange) ([]domainPageRef, error) {
	if page.Level == 0 {
		entries := append([]storageformat.DomainEntry(nil), page.Entries...)
		for _, change := range changes {
			index := sort.Search(len(entries), func(index int) bool { return entries[index].Key >= change.Key })
			found := index < len(entries) && entries[index].Key == change.Key
			if change.Delete {
				if found {
					entries = append(entries[:index], entries[index+1:]...)
				}
				continue
			}
			entry := storageformat.DomainEntry{Key: change.Key, Value: append([]byte(nil), change.Value...), LogicalVersion: change.LogicalVersion}
			if found {
				entries[index] = entry
				continue
			}
			entries = append(entries, storageformat.DomainEntry{})
			copy(entries[index+1:], entries[index:])
			entries[index] = entry
		}
		return session.writeLeafGroups(ctx, entries)
	}
	groups := make(map[int][]storageformat.DomainChange)
	indices := make([]int, 0)
	for _, change := range changes {
		index := consistencyDomainChildIndex(page.Children, change.Key)
		if index < 0 {
			return nil, domain.NewError(domain.ErrorInvalid, "consistency-domain branch has no child for edit")
		}
		if _, found := groups[index]; !found {
			indices = append(indices, index)
		}
		groups[index] = append(groups[index], change)
	}
	sort.Ints(indices)
	children := make([]domainPageRef, len(page.Children))
	for index, child := range page.Children {
		children[index] = domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey}
	}
	offset := 0
	for _, originalIndex := range indices {
		index := originalIndex + offset
		childPage, err := session.readPage(ctx, children[index])
		if err != nil {
			return nil, err
		}
		replacements, err := session.rewrite(ctx, childPage, groups[originalIndex])
		if err != nil {
			return nil, err
		}
		next := make([]domainPageRef, 0, len(children)-1+len(replacements))
		next = append(next, children[:index]...)
		next = append(next, replacements...)
		next = append(next, children[index+1:]...)
		children = next
		offset += len(replacements) - 1
	}
	return session.writeBranchGroups(ctx, page.Level, children)
}

func (session *consistencyDomainTreeSession) writeLeafGroups(ctx context.Context, entries []storageformat.DomainEntry) ([]domainPageRef, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	groups, err := session.leafGroups(entries)
	if err != nil {
		return nil, err
	}
	pages := make([]storageformat.DomainPage, len(groups))
	for index, group := range groups {
		pages[index] = storageformat.DomainPage{SchemaVersion: 1, DomainID: session.reference.ID, Kind: session.reference.Kind, Level: 0, Entries: append([]storageformat.DomainEntry(nil), group...)}
	}
	return session.writePages(ctx, pages)
}

func (session *consistencyDomainTreeSession) leafGroups(entries []storageformat.DomainEntry) ([][]storageformat.DomainEntry, error) {
	var groups [][]storageformat.DomainEntry
	for offset := 0; offset < len(entries); {
		end := offset
		for end < len(entries) && end-offset < domainPageMaximumItems {
			candidate := storageformat.DomainPage{SchemaVersion: 1, DomainID: session.reference.ID, Kind: session.reference.Kind, Level: 0, Entries: entries[offset : end+1]}
			if _, err := storageformat.EncodeCanonical(candidate); err != nil {
				break
			}
			end++
		}
		if end == offset {
			return nil, domain.NewError(domain.ErrorInvalid, "consistency-domain entry cannot fit in a canonical page")
		}
		groups = append(groups, append([]storageformat.DomainEntry(nil), entries[offset:end]...))
		offset = end
	}
	return groups, nil
}

func (session *consistencyDomainTreeSession) writeBranchGroups(ctx context.Context, level int, children []domainPageRef) ([]domainPageRef, error) {
	if len(children) == 0 {
		return nil, nil
	}
	groups, err := session.branchGroups(level, children)
	if err != nil {
		return nil, err
	}
	pages := make([]storageformat.DomainPage, len(groups))
	for groupIndex, group := range groups {
		pageChildren := make([]storageformat.DomainPageChild, len(group))
		for index, child := range group {
			pageChildren[index] = storageformat.DomainPageChild{FirstKey: child.firstKey, LastKey: child.lastKey, Digest: child.root.Digest, Level: child.root.Level, EntryCount: child.root.EntryCount, ByteCount: child.root.ByteCount}
		}
		pages[groupIndex] = storageformat.DomainPage{SchemaVersion: 1, DomainID: session.reference.ID, Kind: session.reference.Kind, Level: level, Children: pageChildren}
	}
	return session.writePages(ctx, pages)
}

const domainPageWriteParallelism = 32

// writePages publishes independent immutable pages concurrently. Publication
// order is irrelevant because no page is authoritative until the later head
// CAS; result order remains deterministic for parent construction.
func (session *consistencyDomainTreeSession) writePages(ctx context.Context, pages []storageformat.DomainPage) ([]domainPageRef, error) {
	if len(pages) == 0 {
		return nil, nil
	}
	if len(pages) == 1 {
		reference, err := session.writePage(ctx, pages[0])
		return []domainPageRef{reference}, err
	}
	workerCount := min(len(pages), domainPageWriteParallelism)
	parallel := providerbudget.TraceFromContext(ctx)
	parallel.ParallelGroup = "immutable-domain-pages"
	parallelContext, cancel := context.WithCancel(providerbudget.WithTrace(ctx, parallel))
	defer cancel()
	type job struct{ index int }
	jobs := make(chan job)
	results := make([]domainPageRef, len(pages))
	var wait sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for range workerCount {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for item := range jobs {
				reference, err := session.writePage(parallelContext, pages[item.index])
				if err != nil {
					errOnce.Do(func() { firstErr = err; cancel() })
					continue
				}
				results[item.index] = reference
			}
		}()
	}
	for index := range pages {
		if parallelContext.Err() != nil {
			break
		}
		jobs <- job{index: index}
	}
	close(jobs)
	wait.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

func (session *consistencyDomainTreeSession) branchGroups(level int, children []domainPageRef) ([][]domainPageRef, error) {
	var groups [][]domainPageRef
	for offset := 0; offset < len(children); {
		end := offset
		for end < len(children) && end-offset < domainPageMaximumItems {
			pageChildren := make([]storageformat.DomainPageChild, end-offset+1)
			for index, child := range children[offset : end+1] {
				pageChildren[index] = storageformat.DomainPageChild{FirstKey: child.firstKey, LastKey: child.lastKey, Digest: child.root.Digest, Level: child.root.Level, EntryCount: child.root.EntryCount, ByteCount: child.root.ByteCount}
			}
			candidate := storageformat.DomainPage{SchemaVersion: 1, DomainID: session.reference.ID, Kind: session.reference.Kind, Level: level, Children: pageChildren}
			if _, err := storageformat.EncodeCanonical(candidate); err != nil {
				break
			}
			end++
		}
		if end == offset {
			return nil, domain.NewError(domain.ErrorInvalid, "consistency-domain child cannot fit in a canonical page")
		}
		groups = append(groups, append([]domainPageRef(nil), children[offset:end]...))
		offset = end
	}
	return groups, nil
}

func (session *consistencyDomainTreeSession) writePage(ctx context.Context, page storageformat.DomainPage) (domainPageRef, error) {
	body, err := storageformat.EncodeCanonical(page)
	if err != nil {
		return domainPageRef{}, err
	}
	digest := storageformat.Digest(body)
	if err := storageformat.ValidateDomainPage(page, digest); err != nil {
		return domainPageRef{}, err
	}
	session.mu.RLock()
	cached, cachedFound := session.pages[digest]
	known, knownFound := session.known[digest]
	session.mu.RUnlock()
	if cachedFound {
		cachedBody, encodeErr := storageformat.EncodeCanonical(cached)
		if encodeErr != nil || !bytes.Equal(cachedBody, body) {
			return domainPageRef{}, domain.NewError(domain.ErrorInvalid, "consistency-domain page digest collision")
		}
		return consistencyDomainPageDescriptor(page, digest)
	}
	reference, err := consistencyDomainPageDescriptor(page, digest)
	if err != nil {
		return domainPageRef{}, err
	}
	if knownFound {
		if known != reference.root {
			return domainPageRef{}, domain.NewError(domain.ErrorInvalid, "consistency-domain known page descriptor mismatch")
		}
		validated, readErr := session.readPage(ctx, reference)
		if readErr != nil {
			return domainPageRef{}, readErr
		}
		validatedBody, encodeErr := storageformat.EncodeCanonical(validated)
		if encodeErr != nil || !bytes.Equal(validatedBody, body) {
			return domainPageRef{}, domain.NewError(domain.ErrorInvalid, "consistency-domain page digest collision")
		}
		return reference, nil
	}
	key := session.pageKey(digest)
	if _, err := session.store.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
		if !errors.Is(err, domain.ErrConflict) {
			return domainPageRef{}, err
		}
		existing, getErr := session.store.backend.Get(ctx, key)
		if getErr != nil {
			return domainPageRef{}, getErr
		}
		if !bytes.Equal(existing.Body, body) {
			return domainPageRef{}, domain.NewError(domain.ErrorInvalid, "consistency-domain page digest collision")
		}
	}
	session.mu.Lock()
	session.pages[digest] = page
	session.mu.Unlock()
	return reference, nil
}
