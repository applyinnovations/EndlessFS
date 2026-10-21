package portable

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

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
	return repackOwnedDomainClosure(ctx, source, roots)
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
