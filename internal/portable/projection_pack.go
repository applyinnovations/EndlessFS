package portable

import (
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// prunePendingProjectionPages removes only unreachable, unpublished derived
// pages. It never rewrites references or discards pages in an existing pack.
func (session *consistencyDomainTreeSession) prunePendingProjectionPages(root storageformat.DomainTreeRoot) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.packID == "" {
		return nil
	}
	keep := make(map[string]storageformat.DomainPage)
	var visit func(domainPageRef) error
	visit = func(reference domainPageRef) error {
		root := reference.root
		if root.Digest == "" || root.PackID != session.packID {
			return nil
		}
		if page, found := keep[root.Digest]; found {
			return verifyConsistencyDomainPageRef(page, reference)
		}
		page, found := session.pendingPack[root.Digest]
		if !found {
			return domain.NewError(domain.ErrorInvalid, "projection closure omitted a staged page")
		}
		if err := storageformat.ValidateDomainPage(page, root.Digest); err != nil {
			return err
		}
		if err := verifyConsistencyDomainPageRef(page, reference); err != nil {
			return err
		}
		keep[root.Digest] = page
		for _, child := range page.Children {
			if err := visit(domainPageRef{root: storageformat.DomainTreeRoot{Digest: child.Digest, PackID: child.PackID, Level: child.Level, EntryCount: child.EntryCount, ByteCount: child.ByteCount}, firstKey: child.FirstKey, lastKey: child.LastKey, leafKeyFilter: child.LeafKeyFilter}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(domainPageRef{root: root}); err != nil {
		return err
	}
	session.pendingPack = keep
	return nil
}
