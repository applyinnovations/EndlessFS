package portable

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
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
	pages     map[string]storageformat.DomainPage
}

func newConsistencyDomainTreeSession(store *consistencyDomainStore, reference consistencyDomainRef) *consistencyDomainTreeSession {
	return &consistencyDomainTreeSession{store: store, reference: reference, pages: make(map[string]storageformat.DomainPage)}
}

func (session *consistencyDomainTreeSession) readPage(ctx context.Context, expected domainPageRef) (storageformat.DomainPage, error) {
	if expected.root.Digest == "" {
		return storageformat.DomainPage{}, domain.NewError(domain.ErrorInvalid, "empty consistency-domain page reference")
	}
	if page, found := session.pages[expected.root.Digest]; found {
		if err := verifyConsistencyDomainPageRef(page, expected); err != nil {
			return storageformat.DomainPage{}, err
		}
		return page, nil
	}
	key := storageformat.DomainPageKey(session.reference.Kind, session.reference.ID, expected.root.Digest)
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
	session.pages[expected.root.Digest] = page
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
	result := make([]domainPageRef, 0, len(groups))
	for _, group := range groups {
		page := storageformat.DomainPage{SchemaVersion: 1, DomainID: session.reference.ID, Kind: session.reference.Kind, Level: 0, Entries: append([]storageformat.DomainEntry(nil), group...)}
		reference, err := session.writePage(ctx, page)
		if err != nil {
			return nil, err
		}
		result = append(result, reference)
	}
	return result, nil
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
	result := make([]domainPageRef, 0, len(groups))
	for _, group := range groups {
		pageChildren := make([]storageformat.DomainPageChild, len(group))
		for index, child := range group {
			pageChildren[index] = storageformat.DomainPageChild{FirstKey: child.firstKey, LastKey: child.lastKey, Digest: child.root.Digest, Level: child.root.Level, EntryCount: child.root.EntryCount, ByteCount: child.root.ByteCount}
		}
		page := storageformat.DomainPage{SchemaVersion: 1, DomainID: session.reference.ID, Kind: session.reference.Kind, Level: level, Children: pageChildren}
		reference, err := session.writePage(ctx, page)
		if err != nil {
			return nil, err
		}
		result = append(result, reference)
	}
	return result, nil
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
	key := storageformat.DomainPageKey(session.reference.Kind, session.reference.ID, digest)
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
	reference, err := consistencyDomainPageDescriptor(page, digest)
	if err == nil {
		session.pages[digest] = page
	}
	return reference, err
}
