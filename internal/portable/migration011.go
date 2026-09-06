package portable

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// Schema 011 activates packed consistency-domain pages and product-scale
// upload transactions. Schema-010 heads and individual immutable pages remain
// valid inputs; the first schema-011 mutation replaces them with a packed
// content-addressed representation. No predecessor application object is
// rewritten at the boundary.
func (e *Engine) runStorageMigration010To011(ctx context.Context, transition storageMigration, superblockObject objectstore.Object, superblock storageformat.Superblock) error {
	return e.runFeatureOnlyStorageMigration(ctx, transition, superblockObject, superblock)
}

// verifySchema011Authority authenticates the complete current domain catalog
// under the canonical checkpoint freeze. A storage set initialized directly
// at schema 010 legitimately has no 009-to-010 conservation receipt, while a
// migrated set has already made the receipt targets authoritative. Therefore
// the successor edge verifies the current typed authority itself instead of
// making historical migration residue part of the live schema contract.
func (e *Engine) verifySchema011Authority(ctx context.Context, transition storageMigration) error {
	if transition.id != storageMigration010To011 || transition.from != storageSchema010 || transition.to != storageSchema011 {
		return domain.NewError(domain.ErrorInvalid, "schema-011 authority verifier received another migration")
	}
	_, _, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if gate.Mode != storageformat.GateClosed || gate.CheckpointID != transition.checkpointID {
		return domain.NewError(domain.ErrorPreconditionFailed, "schema-011 authority verification requires its closed write gate")
	}
	return e.validateSchema009CheckpointClosure(ctx, gate.Epoch)
}
