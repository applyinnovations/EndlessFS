package portable

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// This test-only candidate measures a physical packing alternative over the
// same already-built final projection. It does not claim bounded construction
// memory, crash recovery, head-publication or full lifecycle qualification.
type reviewRollingProjectionPacker struct {
	engine  *Engine
	owner   domain.UserID
	id      string
	binding string
	ordinal int
	current *consistencyDomainTreeSession
}

func (packer *reviewRollingProjectionPacker) next(t *testing.T) {
	t.Helper()
	packer.current = duplicateProjectionSession008(packer.engine, packer.owner, packer.id)
	if err := packer.current.bindPackedMutation(fmt.Sprintf("%s/%d", packer.binding, packer.ordinal)); err != nil {
		t.Fatal(err)
	}
	packer.ordinal++
}

func (packer *reviewRollingProjectionPacker) write(t *testing.T, ctx context.Context, page storageformat.DomainPage) domainPageRef {
	t.Helper()
	page.DomainID = packer.id
	encoded, err := storageformat.EncodeCanonical(page)
	if err != nil {
		t.Fatal(err)
	}
	digest := storageformat.Digest(encoded)
	for {
		if packer.current == nil {
			packer.next(t)
		}
		session := packer.current
		packID, err := session.ensurePackID(digest)
		if err != nil {
			t.Fatal(err)
		}
		pages := make([]storageformat.DomainPackedPage, 0, len(session.pendingPack)+1)
		for key, value := range session.pendingPack {
			if key != digest {
				pages = append(pages, storageformat.DomainPackedPage{Digest: key, Page: value})
			}
		}
		pages = append(pages, storageformat.DomainPackedPage{Digest: digest, Page: page})
		_, err = storageformat.EncodeDomainPagePack(storageformat.DomainPagePack{SchemaVersion: 1, DomainID: packer.id, Kind: session.reference.Kind, PackID: packID, Pages: pages})
		if errors.Is(err, storageformat.ErrDomainPagePackTooLarge) && len(session.pendingPack) > 0 {
			if err := session.flushPack(ctx); err != nil {
				t.Fatal(err)
			}
			packer.next(t)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		result, err := session.writePage(ctx, page)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
}

func reviewRepackProjection(t *testing.T, ctx context.Context, source *consistencyDomainTreeSession, root storageformat.DomainTreeRoot, engine *Engine, owner domain.UserID) (storageformat.DomainTreeRoot, string) {
	t.Helper()
	projectionID := storageformat.Digest([]byte("independent-review-rolling-projection"))
	packer := &reviewRollingProjectionPacker{engine: engine, owner: owner, id: projectionID, binding: root.Digest}
	var copyPage func(domainPageRef) domainPageRef
	copyPage = func(reference domainPageRef) domainPageRef {
		page, err := source.readPage(ctx, reference)
		if err != nil {
			t.Fatal(err)
		}
		page.Children = append([]storageformat.DomainPageChild(nil), page.Children...)
		for index, child := range page.Children {
			copied := copyPage(domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey, leafKeyFilter: child.LeafKeyFilter})
			page.Children[index] = storageformat.DomainPageChild{Digest: copied.root.Digest, PackID: copied.root.PackID, Level: copied.root.Level, EntryCount: copied.root.EntryCount, ByteCount: copied.root.ByteCount, FirstKey: copied.firstKey, LastKey: copied.lastKey, LeafKeyFilter: copied.leafKeyFilter}
		}
		return packer.write(t, ctx, page)
	}
	result := copyPage(domainPageRef{root: root})
	if err := packer.current.flushPack(ctx); err != nil {
		t.Fatal(err)
	}
	return result.root, projectionID
}
