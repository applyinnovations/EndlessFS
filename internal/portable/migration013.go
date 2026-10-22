package portable

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// Schema 013 adds durable per-artifact thumbnail jobs and their ordering and
// worker-lane records. Existing authority is unchanged; the feature fences
// predecessor writers before the first queue record is created.
func (e *Engine) runStorageMigration012To013(ctx context.Context, transition storageMigration, superblockObject objectstore.Object, superblock storageformat.Superblock) error {
	return e.runFeatureOnlyStorageMigration(ctx, transition, superblockObject, superblock)
}

func (e *Engine) verifySchema013Authority(ctx context.Context, transition storageMigration) error {
	if transition.id != storageMigration012To013 || transition.from != storageSchema012 || transition.to != storageSchema013 {
		return domain.NewError(domain.ErrorInvalid, "schema-013 authority verifier received another migration")
	}
	_, _, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if gate.Mode != storageformat.GateClosed || gate.CheckpointID != transition.checkpointID {
		return domain.NewError(domain.ErrorPreconditionFailed, "schema-013 authority verification requires its closed write gate")
	}
	return e.validateSchema009CheckpointClosure(ctx, gate.Epoch)
}
