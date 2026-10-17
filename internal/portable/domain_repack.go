package portable

import (
	"context"
	"errors"
	"strconv"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// boundedDomainPacker is used only when a staged tree cannot fit
// one optimization pack. It preserves the existing canonical format, checks
// both actual encoding bounds before accepting a page, and flushes all packs
// before the caller may publish its original conditional head.
type boundedDomainPacker struct {
	source  *consistencyDomainTreeSession
	binding string
	ordinal int
	current *consistencyDomainTreeSession
}

func (packer *boundedDomainPacker) next() error {
	source := packer.source
	packer.current = newConsistencyDomainTreeSession(source.store, source.reference)
	packer.current.pageKey, packer.current.packKey = source.pageKey, source.packKey
	packer.current.enablePackedWrites(source.packSeed)
	err := packer.current.bindPackedMutation(packer.binding + "/" + strconv.Itoa(packer.ordinal))
	packer.ordinal++
	return err
}

func (packer *boundedDomainPacker) write(ctx context.Context, page storageformat.DomainPage) (domainPageRef, error) {
	if err := ctx.Err(); err != nil {
		return domainPageRef{}, domain.WrapError(domain.ErrorUnavailable, "metadata packing canceled", err)
	}
	encoded, err := storageformat.EncodeCanonical(page)
	if err != nil {
		return domainPageRef{}, err
	}
	digest := storageformat.Digest(encoded)
	for {
		if packer.current == nil {
			if err := packer.next(); err != nil {
				return domainPageRef{}, err
			}
		}
		session := packer.current
		packID, err := session.ensurePackID(digest)
		if err != nil {
			return domainPageRef{}, err
		}
		pages := make([]storageformat.DomainPackedPage, 0, len(session.pendingPack)+1)
		for key, value := range session.pendingPack {
			if key != digest {
				pages = append(pages, storageformat.DomainPackedPage{Digest: key, Page: value})
			}
		}
		pages = append(pages, storageformat.DomainPackedPage{Digest: digest, Page: page})
		_, err = storageformat.EncodeDomainPagePack(storageformat.DomainPagePack{SchemaVersion: 1, DomainID: session.reference.ID, Kind: session.reference.Kind, PackID: packID, Pages: pages})
		if errors.Is(err, storageformat.ErrDomainPagePackTooLarge) && len(session.pendingPack) > 0 {
			if err := session.flushPack(ctx); err != nil {
				return domainPageRef{}, err
			}
			if err := packer.next(); err != nil {
				return domainPageRef{}, err
			}
			continue
		}
		if err != nil {
			return domainPageRef{}, err
		}
		return session.writePage(ctx, page)
	}
}

func repackProjectionClosure(ctx context.Context, source *consistencyDomainTreeSession, root storageformat.DomainTreeRoot) (storageformat.DomainTreeRoot, error) {
	roots, err := repackStagedRoots(ctx, source, []storageformat.DomainTreeRoot{root})
	if err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	return roots[0], nil
}

// All logical roots are bound together before assigning a physical pack ID.
// Opaque leaf values are preserved exactly; their staged dependencies must
// already be flushed by the caller under their original descriptors.
func repackStagedRoots(ctx context.Context, source *consistencyDomainTreeSession, roots []storageformat.DomainTreeRoot) ([]storageformat.DomainTreeRoot, error) {
	body, err := storageformat.EncodeCanonical(roots)
	if err != nil {
		return nil, err
	}
	packer := &boundedDomainPacker{source: source, binding: storageformat.Digest(body)}
	var copyPage func(domainPageRef) (domainPageRef, error)
	copyPage = func(reference domainPageRef) (domainPageRef, error) {
		if err := ctx.Err(); err != nil {
			return domainPageRef{}, domain.WrapError(domain.ErrorUnavailable, "metadata packing canceled", err)
		}
		// Only the unpublished staged pack needs changing. Prior immutable
		// references remain bound to the authenticated projection/source head.
		if reference.root.Digest == "" || reference.root.PackID != source.packID {
			return reference, nil
		}
		page, err := source.readPage(ctx, reference)
		if err != nil {
			return domainPageRef{}, err
		}
		page.Children = append([]storageformat.DomainPageChild(nil), page.Children...)
		for index, child := range page.Children {
			copied, err := copyPage(domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey, leafKeyFilter: child.LeafKeyFilter})
			if err != nil {
				return domainPageRef{}, err
			}
			page.Children[index] = storageformat.DomainPageChild{Digest: copied.root.Digest, PackID: copied.root.PackID, Level: copied.root.Level, EntryCount: copied.root.EntryCount, ByteCount: copied.root.ByteCount, FirstKey: copied.firstKey, LastKey: copied.lastKey, LeafKeyFilter: copied.leafKeyFilter}
		}
		return packer.write(ctx, page)
	}
	result := make([]storageformat.DomainTreeRoot, len(roots))
	for index, root := range roots {
		reference, err := copyPage(domainPageRef{root: root})
		if err != nil {
			return nil, err
		}
		result[index] = reference.root
	}
	if packer.current != nil {
		if err := packer.current.flushPack(ctx); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// publishBoundedMaterializedPacks separates already-staged dependencies from
// materialized state pages without changing an opaque reference. Capacity
// failure happens before a pack PUT, so the original ID can safely publish
// exactly those dependencies. Every new state root is then rewritten to bounded
// packs; the original head CAS remains the sole authority/visibility point.
func publishBoundedMaterializedPacks(ctx context.Context, session *consistencyDomainTreeSession, prepared map[string]storageformat.DomainPage, roots []storageformat.DomainTreeRoot) ([]storageformat.DomainTreeRoot, error) {
	source := newConsistencyDomainTreeSession(session.store, session.reference)
	source.pageKey, source.packKey = session.pageKey, session.packKey
	session.mu.RLock()
	source.packID, source.packSeed = session.packID, session.packSeed
	source.pendingPack = make(map[string]storageformat.DomainPage, len(session.pendingPack))
	for digest, page := range session.pendingPack {
		source.pendingPack[digest] = page
	}
	session.mu.RUnlock()
	if len(prepared) > 0 {
		children := newConsistencyDomainTreeSession(session.store, session.reference)
		children.pageKey, children.packKey = source.pageKey, source.packKey
		children.packID, children.pendingPack = source.packID, prepared
		if err := children.flushPack(ctx); err != nil {
			return nil, err
		}
	}
	return repackStagedRoots(ctx, source, roots)
}
