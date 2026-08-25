package portable

import (
	"context"
	"errors"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// fixed recovery window. Application Trash has separate, indefinite retention.
const terminalOperationRetention = 30 * 24 * time.Hour

func (e *Engine) GateStatus(ctx context.Context) (storageformat.WriteGate, error) {
	_, _, gate, err := e.readGate(ctx)
	return gate, err
}

func (e *Engine) CloseWrites(ctx context.Context, checkpointID string) error {
	if checkpointID == "" {
		return domain.NewError(domain.ErrorInvalid, "checkpoint ID is required")
	}
	pending, err := e.storageMigrationPending(ctx)
	if err != nil {
		return err
	}
	if pending {
		return domain.NewError(domain.ErrorPreconditionFailed, "storage schema migration must converge before checkpoint closure")
	}
	gateObject, gateEnvelope, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	gateSchema, found := detectWriteGateSchema(gate.WriterFeatures, e.writer.RequiredFeatures)
	if !found || gateSchema.id != currentStorageSchema().id {
		return domain.NewError(domain.ErrorPreconditionFailed, "write gate is not bound to the current storage schema")
	}
	switch gate.Mode {
	case storageformat.GateOpen:
		gate.Mode = storageformat.GateClosing
		gate.CheckpointID = checkpointID
		body, encodeErr := storageformat.EncodeEnvelope(writeGateSchema, storageformat.WriteGateKey(), gateEnvelope.Revision+1, gate)
		if encodeErr != nil {
			return encodeErr
		}
		if _, err = e.backend.Put(ctx, storageformat.WriteGateKey(), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: gateObject.Version}); err != nil {
			return err
		}
	case storageformat.GateClosing:
		if gate.CheckpointID != checkpointID {
			return domain.NewError(domain.ErrorConflict, "write gate is closing for another checkpoint")
		}
	case storageformat.GateClosed:
		if gate.CheckpointID == checkpointID {
			return nil
		}
		return domain.NewError(domain.ErrorConflict, "write gate is closed for another checkpoint")
	}
	return e.finishClosingWrites(ctx, checkpointID)
}

func (e *Engine) finishClosingWrites(ctx context.Context, checkpointID string) error {
	_, initialEnvelope, initialGate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if initialGate.Mode == storageformat.GateClosed && initialGate.CheckpointID == checkpointID {
		return nil
	}
	if initialGate.Mode != storageformat.GateClosing || initialGate.CheckpointID != checkpointID {
		return domain.NewError(domain.ErrorPreconditionFailed, "write gate changed before closure drain")
	}
	if !writeGateSchemaAtLeast(initialGate.WriterFeatures, storageSchema008, e.writer.RequiredFeatures) {
		return e.finishLegacyClosingWrites(ctx, checkpointID, initialGate, initialEnvelope)
	}
	// Schema 008 pays no global write-admission tax on ordinary mutations.
	// Checkpoint closure instead freezes the catalog first (preventing a new
	// domain from escaping enumeration) and then conditionally freezes every
	// registered domain. Each domain-head freeze CAS totally orders with its
	// final publication. No schema-007 admissions, mutable operation records,
	// synchronous duplicate roots, or state-version objects participate.
	catalog := newDomainCatalog(e.backend, e.scheduler)
	if err := e.resolveAllTransitions009(ctx); err != nil {
		return err
	}
	entries, err := catalog.freeze(ctx, initialGate.Epoch)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		reference := consistencyDomainRef{Kind: entry.Kind, ID: entry.DomainID}
		for attempts := 0; attempts < 32; attempts++ {
			freezeErr := e.stateDomainStore().freeze(ctx, reference, initialGate.Epoch)
			if freezeErr == nil {
				break
			}
			if !errors.Is(freezeErr, errTransitionPending009) {
				return domain.WrapError(domain.KindOf(freezeErr), "freeze registered consistency domain "+entry.DomainID, freezeErr)
			}
			if resolveErr := e.resolveStateTransition009(ctx, reference); resolveErr != nil {
				return resolveErr
			}
			if attempts == 31 {
				return domain.NewError(domain.ErrorUnavailable, "consistency-domain transition remained freeze-contended")
			}
		}
	}
	if err := e.drainExpiredSchema008Uploads(ctx); err != nil {
		if errors.Is(err, domain.ErrUnavailable) {
			if rollbackErr := catalog.unfreeze(ctx, initialGate.Epoch); rollbackErr != nil {
				return rollbackErr
			}
			if rollbackErr := e.cancelClosingWriteGate(ctx, checkpointID); rollbackErr != nil {
				return rollbackErr
			}
		}
		return err
	}
	gateObject, gateEnvelope, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if gate.Mode == storageformat.GateClosed && gate.CheckpointID == checkpointID {
		return nil
	}
	if gate.Mode != storageformat.GateClosing || gate.CheckpointID != checkpointID {
		return domain.NewError(domain.ErrorPreconditionFailed, "write gate changed while closing")
	}
	gate.Mode = storageformat.GateClosed
	body, err := storageformat.EncodeEnvelope(writeGateSchema, storageformat.WriteGateKey(), gateEnvelope.Revision+1, gate)
	if err != nil {
		return err
	}
	_, err = e.backend.Put(ctx, storageformat.WriteGateKey(), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: gateObject.Version})
	if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) {
		_, _, winner, readErr := e.readGate(ctx)
		if readErr == nil && winner.Mode == storageformat.GateClosed && winner.CheckpointID == checkpointID {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
	return err
}

// finishLegacyClosingWrites is retained exclusively for the forward migration
// reader. Schema-008 runtime mutations never enter this admission/operation/
// manifest maintenance protocol.
func (e *Engine) cancelClosingWriteGate(ctx context.Context, checkpointID string) error {
	for {
		object, envelope, gate, err := e.readGate(ctx)
		if err != nil {
			return err
		}
		if gate.Mode == storageformat.GateOpen {
			return nil
		}
		if gate.Mode != storageformat.GateClosing || gate.CheckpointID != checkpointID {
			return domain.NewError(domain.ErrorPreconditionFailed, "write gate changed while checkpoint closure was cancelled")
		}
		gate.Mode, gate.CheckpointID = storageformat.GateOpen, ""
		body, err := storageformat.EncodeEnvelope(writeGateSchema, storageformat.WriteGateKey(), envelope.Revision+1, gate)
		if err != nil {
			return err
		}
		if _, err := e.backend.Put(ctx, storageformat.WriteGateKey(), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err == nil {
			return nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return err
		}
	}
}

func (e *Engine) OpenWrites(ctx context.Context, checkpointID string) error {
	checkpoint, err := e.readCheckpoint(ctx, checkpointID)
	if err != nil {
		return err
	}
	_, _, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if gate.Mode == storageformat.GateClosed {
		if err := e.VerifyCheckpoint(ctx, checkpointID); err != nil {
			return err
		}
		if err := e.openClosedWriteGate(ctx, checkpointID); err != nil {
			return err
		}
	} else if gate.Mode != storageformat.GateOpen || gate.Epoch != checkpoint.GateEpoch+1 {
		return domain.NewError(domain.ErrorPreconditionFailed, "write gate cannot resume checkpoint reopen")
	}
	return newDomainCatalog(e.backend, e.scheduler).unfreeze(ctx, checkpoint.GateEpoch)
}

func (e *Engine) openClosedWriteGate(ctx context.Context, checkpointID string) error {
	gateObject, gateEnvelope, gate, err := e.readGate(ctx)
	if err != nil {
		return err
	}
	if gate.Mode != storageformat.GateClosed || checkpointID == "" || gate.CheckpointID != checkpointID {
		return domain.NewError(domain.ErrorPreconditionFailed, "write gate is not closed for this checkpoint")
	}
	gate.Mode = storageformat.GateOpen
	gate.Epoch++
	gate.CheckpointID = ""
	body, err := storageformat.EncodeEnvelope(writeGateSchema, storageformat.WriteGateKey(), gateEnvelope.Revision+1, gate)
	if err != nil {
		return err
	}
	_, err = e.backend.Put(ctx, storageformat.WriteGateKey(), body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: gateObject.Version})
	return err
}

func visitObjectPages(ctx context.Context, backend objectstore.ListBackend, prefix string, visit func(objectstore.ObjectInfo) error) error {
	request := objectstore.ListRequest{Prefix: prefix, Limit: 256}
	for {
		page, err := backend.List(ctx, request)
		if err != nil {
			return err
		}
		for _, info := range page.Objects {
			if err := visit(info); err != nil {
				return err
			}
			request.After = info.Key.String()
		}
		if len(page.Objects) == 0 || page.NextCursor == "" {
			return nil
		}
	}
}
