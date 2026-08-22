package portable

import (
	"context"
	"errors"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func (s *FileStore) startPreparingFileOperation(ctx context.Context, operation storageformat.FileOperation, idempotencyKey, fingerprint string) (domain.Operation, error) {
	userID, err := domain.ParseUserID(operation.UserID)
	if err != nil || operation.SchemaVersion != 2 || operation.State != storageformat.FileOperationPreparing || operation.Preparation == nil || operation.Preparation.Phase != "build" || operation.Preparation.Request == nil {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid preparing file operation")
	}
	operationKey := storageformat.OperationKey(operation.UserID, operation.OperationID)
	operationBody, err := storageformat.EncodeEnvelope(fileOperationSchema, operationKey, 1, operation)
	if err != nil {
		return domain.Operation{}, err
	}
	intent := storageformat.MutationIntent{
		Action: storageformat.MutationCreate, TargetKey: operationKey.String(), TargetBody: operationBody,
		RecoverOperationKey: operationKey.String(),
	}
	if idempotencyKey != "" {
		intent.Prerequisites = []storageformat.MutationObject{{Key: operationKey.String(), Body: operationBody}}
		idempotencyObjectKey := storageformat.IdempotencyKey(operation.UserID, idempotencyKey)
		idempotencyBody, encodeErr := storageformat.EncodeEnvelope(idempotencySchema, idempotencyObjectKey, 1, storageformat.IdempotencyRecord{
			SchemaVersion: 1, UserID: operation.UserID, Kind: operation.Kind,
			KeyDigest: storageformat.Digest([]byte(idempotencyKey)), Fingerprint: fingerprint, OperationID: operation.OperationID,
		})
		if encodeErr != nil {
			return domain.Operation{}, encodeErr
		}
		intent.TargetKey, intent.TargetBody = idempotencyObjectKey.String(), idempotencyBody
	}
	err = s.engine.withAdmission(ctx, intent, func() error {
		if err := s.engine.ensureMutationPrerequisites(ctx, intent.Prerequisites); err != nil {
			return err
		}
		target := objectstore.MustKey(intent.TargetKey)
		if _, putErr := s.engine.backend.Put(ctx, target, intent.TargetBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); putErr != nil {
			if errors.Is(putErr, domain.ErrConflict) && idempotencyKey != "" {
				existing, found, lookupErr := s.lookupIdempotentOperation(ctx, userID, operation.Kind, idempotencyKey, fingerprint)
				if found && lookupErr == nil {
					operation.OperationID = string(existing.ID)
					return nil
				}
			}
			return putErr
		}
		if err := s.engine.step(ctx, StepOperationAfterHeader); err != nil {
			return err
		}
		return s.executeFileOperation(ctx, operationKey)
	})
	if err != nil {
		return domain.Operation{}, err
	}
	stored, err := s.readFileOperation(ctx, userID, operation.OperationID)
	if err != nil {
		return domain.Operation{}, err
	}
	return domainFileOperation(stored), nil
}

func (s *FileStore) checkpointFileOperationPreparationRun(ctx context.Context, key objectstore.Key, ownedFence uint64, ownedAttempt string, runCount uint64) error {
	object, envelope, operation, err := s.readFileOperationObject(ctx, key)
	if err != nil {
		return err
	}
	if operation.State != storageformat.FileOperationPreparing || operation.Preparation == nil || operation.Preparation.Phase != "build" || operation.Fence != ownedFence || operation.ReplicaAttemptID != ownedAttempt {
		return domain.NewError(domain.ErrorUnavailable, "file operation preparation ownership changed")
	}
	if operation.Preparation.RunCount == runCount {
		return nil
	}
	if runCount == 0 || operation.Preparation.RunCount != runCount-1 {
		return domain.NewError(domain.ErrorInvalid, "file operation preparation run checkpoint is not adjacent")
	}
	operation.Preparation.RunCount = runCount
	operation.UpdatedAt = s.engine.clock.Now().UTC()
	body, err := storageformat.EncodeEnvelope(fileOperationSchema, key, envelope.Revision+1, operation)
	if err != nil {
		return err
	}
	if _, err = s.engine.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound) {
			return domain.NewError(domain.ErrorUnavailable, "file operation preparation run was superseded")
		}
		return err
	}
	return s.engine.step(ctx, StepOperationAfterPreparationRun)
}

func (s *FileStore) sealBuiltFileOperationPreparation(ctx context.Context, key objectstore.Key, ownedFence uint64, ownedAttempt string, generatedRuns uint64) error {
	object, envelope, operation, err := s.readFileOperationObject(ctx, key)
	if err != nil {
		return err
	}
	if generatedRuns == 0 || operation.State != storageformat.FileOperationPreparing || operation.Preparation == nil || operation.Preparation.Phase != "build" || operation.Preparation.RunCount != generatedRuns || operation.Fence != ownedFence || operation.ReplicaAttemptID != ownedAttempt {
		return domain.NewError(domain.ErrorUnavailable, "file operation preparation cannot be sealed by this owner")
	}
	operation.Preparation.Phase = "seal"
	operation.Preparation.Request = nil
	operation.UpdatedAt = s.engine.clock.Now().UTC()
	body, err := storageformat.EncodeEnvelope(fileOperationSchema, key, envelope.Revision+1, operation)
	if err != nil {
		return err
	}
	if _, err = s.engine.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
		if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound) {
			return domain.NewError(domain.ErrorUnavailable, "file operation preparation seal was superseded")
		}
	}
	return err
}
