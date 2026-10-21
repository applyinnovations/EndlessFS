package portable

import (
	"bytes"
	"context"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// consistencyDomainTreeIterator walks one immutable tree while retaining at
// most one page per tree level. It is used by projection construction and
// closure verification where retaining every visited page would make memory
// proportional to the inventory.
type consistencyDomainTreeIterator struct {
	ctx     context.Context
	session *consistencyDomainTreeSession
	stack   []domainTreeIteratorFrame
}

// newConsistencyDomainTreeIteratorAfter starts at the first key strictly
// greater than after. It descends through aggregate key bounds instead of
// replaying the immutable prefix, keeping cursor continuation logarithmic in
// tree size.
func newConsistencyDomainTreeIteratorAfter(ctx context.Context, session *consistencyDomainTreeSession, root storageformat.DomainTreeRoot, after string) (*consistencyDomainTreeIterator, error) {
	iterator := &consistencyDomainTreeIterator{ctx: ctx, session: session}
	if root.Digest == "" {
		return iterator, nil
	}
	reference := domainPageRef{root: root}
	for {
		page, err := session.readPage(ctx, reference)
		if err != nil {
			return nil, err
		}
		if page.Level == 0 {
			index := sort.Search(len(page.Entries), func(index int) bool { return page.Entries[index].Key > after })
			iterator.stack = append(iterator.stack, domainTreeIteratorFrame{reference: reference, page: page, index: index})
			return iterator, nil
		}
		index := sort.Search(len(page.Children), func(index int) bool { return page.Children[index].LastKey > after })
		if index == len(page.Children) {
			return iterator, nil
		}
		iterator.stack = append(iterator.stack, domainTreeIteratorFrame{reference: reference, page: page, index: index})
		child := page.Children[index]
		reference = domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey}
	}
}

type domainTreeIteratorFrame struct {
	reference domainPageRef
	page      storageformat.DomainPage
	index     int
}

func newConsistencyDomainTreeIterator(ctx context.Context, session *consistencyDomainTreeSession, root storageformat.DomainTreeRoot) (*consistencyDomainTreeIterator, error) {
	iterator := &consistencyDomainTreeIterator{ctx: ctx, session: session}
	if root.Digest != "" {
		if err := iterator.descend(domainPageRef{root: root}); err != nil {
			return nil, err
		}
	}
	return iterator, nil
}

func (iterator *consistencyDomainTreeIterator) descend(reference domainPageRef) error {
	for {
		page, err := iterator.session.readPage(iterator.ctx, reference)
		if err != nil {
			return err
		}
		iterator.stack = append(iterator.stack, domainTreeIteratorFrame{reference: reference, page: page})
		if page.Level == 0 {
			return nil
		}
		child := page.Children[0]
		reference = domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey}
	}
}

func (iterator *consistencyDomainTreeIterator) Next() (storageformat.DomainEntry, bool, error) {
	for len(iterator.stack) > 0 {
		leaf := &iterator.stack[len(iterator.stack)-1]
		if leaf.page.Level == 0 && leaf.index < len(leaf.page.Entries) {
			entry := leaf.page.Entries[leaf.index]
			leaf.index++
			return storageformat.DomainEntry{Key: entry.Key, Value: append([]byte(nil), entry.Value...), LogicalVersion: entry.LogicalVersion}, true, nil
		}
		iterator.session.forgetPage(leaf.reference.root.Digest)
		iterator.stack = iterator.stack[:len(iterator.stack)-1]
		for len(iterator.stack) > 0 {
			branch := &iterator.stack[len(iterator.stack)-1]
			branch.index++
			if branch.index < len(branch.page.Children) {
				child := branch.page.Children[branch.index]
				reference := domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey}
				if err := iterator.descend(reference); err != nil {
					return storageformat.DomainEntry{}, false, err
				}
				break
			}
			iterator.session.forgetPage(branch.reference.root.Digest)
			iterator.stack = iterator.stack[:len(iterator.stack)-1]
		}
	}
	return storageformat.DomainEntry{}, false, nil
}

// consistencyDomainTreeBuilder creates a tree from an already sorted stream.
// Its buffers are bounded by one leaf and domainPageMaximumItems descriptors
// per active level; it never accumulates the complete result set.
type consistencyDomainTreeBuilder struct {
	ctx                context.Context
	session            *consistencyDomainTreeSession
	leaf               []storageformat.DomainEntry
	leafCanonicalBytes int // exact size of the privately owned leaf
	levels             [][]domainPageRef
}

func newConsistencyDomainTreeBuilder(ctx context.Context, session *consistencyDomainTreeSession) *consistencyDomainTreeBuilder {
	return &consistencyDomainTreeBuilder{ctx: ctx, session: session}
}

func (builder *consistencyDomainTreeBuilder) Add(entry storageformat.DomainEntry) error {
	for {
		if err := builder.ctx.Err(); err != nil {
			return domain.WrapError(domain.ErrorUnavailable, "consistency-domain stream canceled", err)
		}
		if len(builder.leaf) == 0 {
			page := storageformat.DomainPage{SchemaVersion: 1, DomainID: builder.session.reference.ID, Kind: builder.session.reference.Kind, Level: 0, Entries: []storageformat.DomainEntry{entry}}
			body, err := storageformat.EncodeCanonical(page)
			if err != nil {
				return domain.NewError(domain.ErrorInvalid, "consistency-domain entry cannot fit in a canonical page")
			}
			entry.Value = bytes.Clone(entry.Value)
			builder.leaf = append(builder.leaf, entry)
			builder.leafCanonicalBytes = len(body)
			return nil
		}
		// The closed canonical entries array grows by one comma and the
		// member encoding. Earlier accepted values are owned, so their
		// byte contribution cannot drift between admissions.
		if len(builder.leaf) < domainPageMaximumItems {
			member, err := storageformat.EncodeCanonical(entry)
			if err == nil && len(member) <= storageformat.MaxCanonicalBytes-builder.leafCanonicalBytes-1 {
				entry.Value = bytes.Clone(entry.Value)
				builder.leaf = append(builder.leaf, entry)
				builder.leafCanonicalBytes += 1 + len(member)
				return nil
			}
		}
		// Preserve the prior valid-prefix publication/error precedence. A
		// following oversized entry is rejected only after this leaf flush.
		if err := builder.flushLeaf(); err != nil {
			return err
		}
	}
}

func (builder *consistencyDomainTreeBuilder) flushLeaf() error {
	if len(builder.leaf) == 0 {
		return nil
	}
	page := storageformat.DomainPage{SchemaVersion: 1, DomainID: builder.session.reference.ID, Kind: builder.session.reference.Kind, Level: 0, Entries: builder.leaf}
	reference, err := builder.session.writePage(builder.ctx, page)
	if err != nil {
		return err
	}
	builder.session.forgetPage(reference.root.Digest)
	builder.leaf = nil
	builder.leafCanonicalBytes = 0
	return builder.addReference(0, reference)
}

func (builder *consistencyDomainTreeBuilder) addReference(level int, reference domainPageRef) error {
	for len(builder.levels) <= level {
		builder.levels = append(builder.levels, nil)
	}
	builder.levels[level] = append(builder.levels[level], reference)
	if len(builder.levels[level]) < domainPageMaximumItems {
		return nil
	}
	return builder.flushLevel(level)
}

func (builder *consistencyDomainTreeBuilder) flushLevel(level int) error {
	references := builder.levels[level]
	if len(references) == 0 {
		return nil
	}
	children := make([]storageformat.DomainPageChild, len(references))
	for index, child := range references {
		var err error
		children[index], err = builder.session.pageChild(child)
		if err != nil {
			return err
		}
	}
	page := storageformat.DomainPage{SchemaVersion: 1, DomainID: builder.session.reference.ID, Kind: builder.session.reference.Kind, Level: level + 1, Children: children}
	reference, err := builder.session.writePage(builder.ctx, page)
	if err != nil {
		return err
	}
	builder.session.forgetPage(reference.root.Digest)
	builder.levels[level] = nil
	return builder.addReference(level+1, reference)
}

func (builder *consistencyDomainTreeBuilder) Finish() (storageformat.DomainTreeRoot, error) {
	if err := builder.flushLeaf(); err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	for {
		lowest, total, only := -1, 0, domainPageRef{}
		for level, references := range builder.levels {
			if len(references) == 0 {
				continue
			}
			if lowest < 0 {
				lowest = level
			}
			total += len(references)
			if len(references) == 1 {
				only = references[0]
			}
		}
		if total == 0 {
			return storageformat.DomainTreeRoot{}, nil
		}
		if total == 1 {
			return only.root, nil
		}
		if err := builder.flushLevel(lowest); err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
	}
}
