package portable

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// Schema 012 gives independent upload intents independent opaque identifiers.
// Existing batch membership, upload records, blobs and outcomes remain intact.
// The required feature fences predecessor writers before new identities exist.
func (e *Engine) runStorageMigration011To012(ctx context.Context, transition storageMigration, superblockObject objectstore.Object, superblock storageformat.Superblock) error {
	return e.runFeatureOnlyStorageMigration(ctx, transition, superblockObject, superblock)
}

func (e *Engine) verifySchema012Authority(ctx context.Context, transition storageMigration) error {
	if transition.id != storageMigration011To012 || transition.from != storageSchema011 || transition.to != storageSchema012 {
		return domain.NewError(domain.ErrorInvalid, "schema-012 authority verifier received another migration")
	}
	_, _, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if gate.Mode != storageformat.GateClosed || gate.CheckpointID != transition.checkpointID {
		return domain.NewError(domain.ErrorPreconditionFailed, "schema-012 authority verification requires its closed write gate")
	}
	return e.validateSchema009CheckpointClosure(ctx, gate.Epoch)
}
