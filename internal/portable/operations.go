package portable

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"unicode"
	"unicode/utf8"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	fileOperationSchema           = "file-operation-v1"
	fileOperationStepPageSchema   = "file-operation-step-page-v1"
	idempotencySchema             = "idempotency-v1"
	maxOperationPageRoots         = 64
	maxOperationPagePrerequisites = 256
	maxOperationPageCopies        = 128

	operationCopy            = "copy"
	operationMove            = "move"
	operationDelete          = "delete"
	operationCreateDirectory = "create-directory"

	StepOperationAfterPrepared          = "operation:after-prepared"
	StepOperationAfterHeader            = "operation:after-header"
	StepOperationAfterPreparationRun    = "operation:after-preparation-run"
	StepOperationAfterPreparationSealed = "operation:after-preparation-sealed"
	StepOperationAfterCommitted         = "operation:after-committed"
	StepOperationAfterFinalized         = "operation:after-finalized"

	StepUploadCompletionAfterPrepared  = "upload-completion:after-prepared"
	StepUploadCompletionAfterCommitted = "upload-completion:after-committed"
	StepUploadCompletionAfterFinalized = "upload-completion:after-finalized"

	StepCreateDirectoryAfterPrepared  = "create-directory:after-prepared"
	StepCreateDirectoryAfterCommitted = "create-directory:after-committed"
	StepCreateDirectoryAfterFinalized = "create-directory:after-finalized"
)

func stagedFileOperationStepPageKey(operation storageformat.FileOperation, index uint64) objectstore.Key {
	artifactID := fmt.Sprintf("step-%s-%016x", storageformat.Digest([]byte(operation.StepSetID)), index)
	return storageformat.OperationStagingKey(operation.UserID, operation.OperationID, artifactID)
}

func fileOperationStepPageKey(operation storageformat.FileOperation, index uint64) objectstore.Key {
	if operation.StepsStaged {
		return stagedFileOperationStepPageKey(operation, index)
	}
	return storageformat.FileOperationStepPageKey(operation.UserID, operation.OperationID, operation.StepSetID, index)
}

func (s *FileStore) forEachFileOperationStepPage(ctx context.Context, operation storageformat.FileOperation, visit func(storageformat.FileOperationStepPage) error) error {
	if operation.StepPageCount == 0 {
		if operation.StepSetID != "" || operation.StepDigest != "" || len(operation.Roots) == 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid legacy file operation steps")
		}
		references := append([]storageformat.MutationObjectReference(nil), operation.PrerequisiteRefs...)
		if operation.SchemaVersion != 3 {
			references = make([]storageformat.MutationObjectReference, 0, len(operation.Prerequisites))
			for _, prerequisite := range operation.Prerequisites {
				references = append(references, storageformat.MutationObjectReference{Key: prerequisite.Key, BodyDigest: storageformat.Digest(prerequisite.Body)})
			}
		}
		return visit(storageformat.FileOperationStepPage{SchemaVersion: 1, UserID: operation.UserID, OperationID: operation.OperationID, Roots: operation.Roots, Prerequisites: references, Copies: operation.Copies})
	}
	if operation.StepSetID == "" || operation.StepDigest == "" || len(operation.Roots) != 0 || len(operation.Prerequisites) != 0 || len(operation.Copies) != 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid paged file operation steps")
	}
	previous := ""
	lastRoot, lastPrerequisite, lastCopy := "", "", ""
	for index := uint64(0); index < operation.StepPageCount; index++ {
		key := fileOperationStepPageKey(operation, index)
		object, err := s.engine.backend.Get(ctx, key)
		if err != nil {
			return err
		}
		var envelope storageformat.Envelope
		var page storageformat.FileOperationStepPage
		if err := storageformat.DecodeEnvelope(object.Body, key, fileOperationStepPageSchema, &envelope, &page); err != nil {
			return err
		}
		if page.SchemaVersion != 1 || page.UserID != operation.UserID || page.OperationID != operation.OperationID || page.StepSetID != operation.StepSetID || page.Index != index || page.PreviousDigest != previous || len(page.Roots) > maxOperationPageRoots || len(page.Prerequisites) > maxOperationPagePrerequisites || len(page.Copies) > maxOperationPageCopies || len(page.Roots)+len(page.Prerequisites)+len(page.Copies) == 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid file operation step page")
		}
		for _, root := range page.Roots {
			if root.Key == "" || root.Key <= lastRoot {
				return domain.NewError(domain.ErrorInvalid, "file operation roots are not uniquely ordered")
			}
			lastRoot = root.Key
		}
		for _, prerequisite := range page.Prerequisites {
			if prerequisite.Key == "" || prerequisite.BodyDigest == "" || prerequisite.Key <= lastPrerequisite {
				return domain.NewError(domain.ErrorInvalid, "file operation prerequisites are not uniquely ordered")
			}
			lastPrerequisite = prerequisite.Key
		}
		for _, copy := range page.Copies {
			if copy.DestinationKey == "" || copy.DestinationKey <= lastCopy {
				return domain.NewError(domain.ErrorInvalid, "file operation copies are not uniquely ordered")
			}
			lastCopy = copy.DestinationKey
		}
		if err := visit(page); err != nil {
			return err
		}
		previous = storageformat.Digest(object.Body)
	}
	if previous != operation.StepDigest {
		return domain.NewError(domain.ErrorInvalid, "file operation step-page chain digest mismatch")
	}
	return nil
}

func (s *FileStore) ensureOperationPrerequisiteReferences(ctx context.Context, operation storageformat.FileOperation, references []storageformat.MutationObjectReference) error {
	for _, reference := range references {
		key, err := objectstore.ParseKey(reference.Key)
		if err != nil || reference.BodyDigest == "" {
			return domain.NewError(domain.ErrorInvalid, "invalid operation prerequisite reference")
		}
		if object, getErr := s.engine.backend.Get(ctx, key); getErr == nil {
			if storageformat.Digest(object.Body) != reference.BodyDigest {
				return domain.NewError(domain.ErrorInvalid, "operation prerequisite digest mismatch")
			}
			continue
		} else if !errors.Is(getErr, domain.ErrNotFound) {
			return getErr
		}
		if !operation.StepsStaged || reference.StagingKey == "" {
			return domain.NewError(domain.ErrorNotFound, "operation prerequisite is missing")
		}
		expectedStaging := storageformat.OperationStagingKey(operation.UserID, operation.OperationID, "prerequisite-"+storageformat.Digest([]byte(reference.Key)))
		prerequisiteKey, parseErr := objectstore.ParseKey(reference.StagingKey)
		if parseErr != nil || prerequisiteKey != expectedStaging {
			return domain.NewError(domain.ErrorInvalid, "invalid operation prerequisite staging reference")
		}
		staged, getErr := s.engine.backend.Get(ctx, prerequisiteKey)
		if getErr != nil {
			return getErr
		}
		if storageformat.Digest(staged.Body) != reference.BodyDigest {
			return domain.NewError(domain.ErrorInvalid, "staged operation prerequisite digest mismatch")
		}
		if _, copyErr := s.engine.backend.Copy(ctx, prerequisiteKey, key, objectstore.CopyCondition{SourceVersion: staged.Version, Destination: objectstore.PutCondition{Mode: objectstore.PutCreateOnly}}); copyErr != nil {
			if !errors.Is(copyErr, domain.ErrConflict) && !errors.Is(copyErr, domain.ErrPreconditionFailed) {
				return copyErr
			}
			winner, winnerErr := s.engine.backend.Get(ctx, key)
			if winnerErr != nil || storageformat.Digest(winner.Body) != reference.BodyDigest {
				return domain.NewError(domain.ErrorInvalid, "operation prerequisite promotion collision")
			}
		}
	}
	return nil
}

func (s *FileStore) executeFileOperation(ctx context.Context, key objectstore.Key) error {
	return s.executeFileOperationReady(ctx, key, false)
}

func (s *FileStore) executeFileOperationReady(ctx context.Context, key objectstore.Key, prerequisitesReady bool) error {
	object, envelope, operation, err := s.readFileOperationObject(ctx, key)
	if err != nil {
		return err
	}
	if operation.State == storageformat.FileOperationSucceeded || operation.State == storageformat.FileOperationFailed {
		return nil
	}
	if operation.State == storageformat.FileOperationRunning {
		ownedFence := operation.Fence
		ownedAttempt := operation.ReplicaAttemptID
		if prerequisitesReady {
			// startFileOperation authenticated and published every immutable
			// prerequisite immediately before creating this operation record.
			// Recovery enters through executeFileOperation and revalidates them.
		} else if operation.SchemaVersion == 3 {
			if err := s.engine.ensureMutationPrerequisiteReferences(ctx, operation.PrerequisiteRefs); err != nil {
				return err
			}
			if err := s.engine.ensureMutationCopies(ctx, operation.Copies); err != nil {
				return err
			}
		} else if operation.StepPageCount == 0 {
			if err := s.engine.ensureMutationPrerequisites(ctx, operation.Prerequisites); err != nil {
				return err
			}
			if err := s.engine.ensureMutationCopies(ctx, operation.Copies); err != nil {
				return err
			}
		} else if err := s.forEachFileOperationStepPage(ctx, operation, func(page storageformat.FileOperationStepPage) error {
			if err := s.ensureOperationPrerequisiteReferences(ctx, operation, page.Prerequisites); err != nil {
				return err
			}
			return s.engine.ensureMutationCopies(ctx, page.Copies)
		}); err != nil {
			return err
		}
		if err := s.forEachFileOperationStepPage(ctx, operation, func(page storageformat.FileOperationStepPage) error {
			for _, root := range page.Roots {
				if err := s.prepareOperationRoot(ctx, root); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) {
				return s.failFileOperation(ctx, object, envelope, operation, domain.ErrorPreconditionFailed, "visibility root changed before operation prepare")
			}
			return err
		}
		preparedStep := StepOperationAfterPrepared
		switch operation.Kind {
		case "upload-complete":
			preparedStep = StepUploadCompletionAfterPrepared
		case operationCreateDirectory:
			preparedStep = StepCreateDirectoryAfterPrepared
		}
		if err := s.engine.step(ctx, preparedStep); err != nil {
			return err
		}
		object, envelope, operation, err = s.readFileOperationObject(ctx, key)
		if err != nil {
			return err
		}
		if operation.State != storageformat.FileOperationRunning {
			return s.executeFileOperationReady(ctx, key, false)
		}
		if operation.Fence != ownedFence || operation.ReplicaAttemptID != ownedAttempt {
			return domain.NewError(domain.ErrorUnavailable, "file operation ownership was superseded")
		}
		operation.State = storageformat.FileOperationCommitted
		operation.UpdatedAt = s.engine.clock.Now().UTC()
		body, err := storageformat.EncodeEnvelope(fileOperationSchema, key, envelope.Revision+1, operation)
		if err != nil {
			return err
		}
		if _, err = s.engine.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
			return err
		}
		committedStep := StepOperationAfterCommitted
		switch operation.Kind {
		case "upload-complete":
			committedStep = StepUploadCompletionAfterCommitted
		case operationCreateDirectory:
			committedStep = StepCreateDirectoryAfterCommitted
		}
		if err := s.engine.step(ctx, committedStep); err != nil {
			return err
		}
	}
	if err := s.forEachFileOperationStepPage(ctx, operation, func(page storageformat.FileOperationStepPage) error {
		for _, root := range page.Roots {
			if err := s.finalizeOperationRoot(ctx, root); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	finalizedStep := StepOperationAfterFinalized
	switch operation.Kind {
	case "upload-complete":
		finalizedStep = StepUploadCompletionAfterFinalized
	case operationCreateDirectory:
		finalizedStep = StepCreateDirectoryAfterFinalized
	}
	if err := s.engine.step(ctx, finalizedStep); err != nil {
		return err
	}
	object, envelope, operation, err = s.readFileOperationObject(ctx, key)
	if err != nil {
		return err
	}
	if operation.State == storageformat.FileOperationSucceeded {
		return nil
	}
	if operation.State != storageformat.FileOperationCommitted {
		return domain.NewError(domain.ErrorPreconditionFailed, "file operation is not committed")
	}
	operation.State = storageformat.FileOperationSucceeded
	operation.UpdatedAt = s.engine.clock.Now().UTC()
	body, err := storageformat.EncodeEnvelope(fileOperationSchema, key, envelope.Revision+1, operation)
	if err != nil {
		return err
	}
	_, err = s.engine.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
	return err
}

func (s *FileStore) failPreparingFileOperation(ctx context.Context, key objectstore.Key, message string) error {
	object, envelope, operation, err := s.readFileOperationObject(ctx, key)
	if err != nil {
		return err
	}
	if operation.State == storageformat.FileOperationFailed {
		return nil
	}
	if operation.State != storageformat.FileOperationPreparing {
		return domain.NewError(domain.ErrorUnavailable, "file operation preparation state changed concurrently")
	}
	operation.State = storageformat.FileOperationFailed
	operation.ErrorKind = domain.ErrorPreconditionFailed
	operation.Error = message
	operation.UpdatedAt = s.engine.clock.Now().UTC()
	body, err := storageformat.EncodeEnvelope(fileOperationSchema, key, envelope.Revision+1, operation)
	if err != nil {
		return err
	}
	_, err = s.engine.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
	if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
		return domain.NewError(domain.ErrorUnavailable, "file operation preparation failure was superseded")
	}
	return err
}

func (s *FileStore) recoverFileOperation(ctx context.Context, key objectstore.Key) error {
	object, envelope, operation, err := s.readFileOperationObject(ctx, key)
	if err != nil {
		return err
	}
	switch operation.State {
	case storageformat.FileOperationSucceeded, storageformat.FileOperationFailed:
		return nil
	case storageformat.FileOperationCommitted:
		return s.executeFileOperation(ctx, key)
	case storageformat.FileOperationPreparing:
		return s.failPreparingFileOperation(ctx, key, "operation was safely cancelled while closing the storage write gate; retry the mutation")
	case storageformat.FileOperationRunning:
		if !expired(s.engine.clock.Now(), operation.ExpiresAt) {
			return domain.NewError(domain.ErrorUnavailable, "file operation owner is still active")
		}
		ownerID, err := s.engine.ids.OpaqueID()
		if err != nil {
			return err
		}
		operation.Attempt++
		operation.Fence++
		operation.ReplicaAttemptID = ownerID
		operation.ExpiresAt = s.engine.clock.Now().UTC().Add(s.engine.leaseTTL)
		operation.UpdatedAt = s.engine.clock.Now().UTC()
		body, err := storageformat.EncodeEnvelope(fileOperationSchema, key, envelope.Revision+1, operation)
		if err != nil {
			return err
		}
		if _, err = s.engine.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version}); err != nil {
			if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
				return domain.NewError(domain.ErrorUnavailable, "another replica won file operation takeover")
			}
			return err
		}
		return s.executeFileOperation(ctx, key)
	default:
		return domain.NewError(domain.ErrorInvalid, "invalid file operation state")
	}
}

// quiesceFileOperation is used only while the canonical write gate is closing.
// A preparing operation has not published visibility yet, so failing it is the
// only safe cross-version action: recomputing predecessor preparation runs with
// a newer namespace algorithm could otherwise mix incompatible durable intent.
// Running and committed operations already carry sealed immutable steps and are
// recovered normally.
func (s *FileStore) quiesceFileOperation(ctx context.Context, key objectstore.Key) error {
	_, _, operation, err := s.readFileOperationObject(ctx, key)
	if err != nil {
		return err
	}
	if operation.State == storageformat.FileOperationPreparing {
		return s.failPreparingFileOperation(ctx, key, "operation was safely cancelled while closing the storage write gate; retry the mutation")
	}
	return s.recoverFileOperation(ctx, key)
}

func (s *FileStore) prepareOperationRoot(ctx context.Context, root storageformat.FileOperationRoot) error {
	key, err := objectstore.ParseKey(root.Key)
	if err != nil {
		return err
	}
	object, err := s.engine.backend.Get(ctx, key)
	if err == nil {
		if string(object.Body) == string(root.PendingBody) || string(object.Body) == string(root.FinalBody) {
			return nil
		}
		logical, logicalErr := canonicalLogicalVersion(object.Body)
		if logicalErr != nil {
			return logicalErr
		}
		if !root.PreExisted || logical != root.ExpectedLogicalVersion {
			return domain.NewError(domain.ErrorPreconditionFailed, "directory root changed")
		}
		_, err = s.engine.backend.Put(ctx, key, root.PendingBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
		if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) {
			return s.acceptConcurrentOperationRoot(ctx, key, root, err)
		}
		return err
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if root.PreExisted || root.ExpectedLogicalVersion != "" {
		return domain.NewError(domain.ErrorPreconditionFailed, "directory root disappeared")
	}
	_, err = s.engine.backend.Put(ctx, key, root.PendingBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly})
	if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) {
		return s.acceptConcurrentOperationRoot(ctx, key, root, err)
	}
	return err
}

func (s *FileStore) acceptConcurrentOperationRoot(ctx context.Context, key objectstore.Key, root storageformat.FileOperationRoot, original error) error {
	current, err := s.engine.backend.Get(ctx, key)
	if err == nil && (string(current.Body) == string(root.PendingBody) || string(current.Body) == string(root.FinalBody)) {
		return nil
	}
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	return original
}

func (s *FileStore) finalizeOperationRoot(ctx context.Context, root storageformat.FileOperationRoot) error {
	key := objectstore.MustKey(root.Key)
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return err
	}
	if string(object.Body) == string(root.FinalBody) {
		return nil
	}
	if string(object.Body) != string(root.PendingBody) {
		return domain.NewError(domain.ErrorPreconditionFailed, "directory pending transition changed")
	}
	_, err = s.engine.backend.Put(ctx, key, root.FinalBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
	if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) {
		current, getErr := s.engine.backend.Get(ctx, key)
		if getErr == nil && string(current.Body) == string(root.FinalBody) {
			return nil
		}
		if getErr != nil && !errors.Is(getErr, domain.ErrNotFound) {
			return getErr
		}
	}
	return err
}

func (s *FileStore) failFileOperation(ctx context.Context, object objectstore.Object, envelope storageformat.Envelope, operation storageformat.FileOperation, kind domain.ErrorKind, message string) error {
	if err := s.forEachFileOperationStepPage(ctx, operation, func(page storageformat.FileOperationStepPage) error {
		for _, root := range page.Roots {
			key := objectstore.MustKey(root.Key)
			current, getErr := s.engine.backend.Get(ctx, key)
			if errors.Is(getErr, domain.ErrNotFound) {
				continue
			}
			if getErr != nil {
				return getErr
			}
			if string(current.Body) != string(root.PendingBody) {
				continue
			}
			var rollbackErr error
			if root.PreExisted {
				_, rollbackErr = s.engine.backend.Put(ctx, key, root.RollbackBody, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: current.Version})
			} else {
				rollbackErr = s.engine.backend.Delete(ctx, key, objectstore.DeleteCondition{Version: current.Version})
			}
			if rollbackErr != nil {
				return domain.WrapError(domain.ErrorUnavailable, "file operation rollback was interrupted", rollbackErr)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	operation.State = storageformat.FileOperationFailed
	operation.ErrorKind = kind
	operation.Error = message
	operation.UpdatedAt = s.engine.clock.Now().UTC()
	body, err := storageformat.EncodeEnvelope(fileOperationSchema, object.Key, envelope.Revision+1, operation)
	if err != nil {
		return err
	}
	_, err = s.engine.backend.Put(ctx, object.Key, body, objectstore.PutCondition{Mode: objectstore.PutMatch, Version: object.Version})
	if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrNotFound) {
		return domain.NewError(domain.ErrorUnavailable, "file operation failure state changed concurrently")
	}
	return err
}

func (s *FileStore) readFileOperation(ctx context.Context, userID domain.UserID, operationID string) (storageformat.FileOperation, error) {
	key := storageformat.OperationKey(userID.String(), operationID)
	_, _, operation, err := s.readFileOperationObject(ctx, key)
	return operation, err
}

func (s *FileStore) readFileOperationObject(ctx context.Context, key objectstore.Key) (objectstore.Object, storageformat.Envelope, storageformat.FileOperation, error) {
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.FileOperation{}, err
	}
	var envelope storageformat.Envelope
	var operation storageformat.FileOperation
	if err := storageformat.DecodeEnvelope(object.Body, key, fileOperationSchema, &envelope, &operation); err != nil {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.FileOperation{}, err
	}
	legacySteps := operation.SchemaVersion == 1 && operation.Preparation == nil && operation.StepSetID == "" && operation.StepPageCount == 0 && operation.StepDigest == "" && len(operation.Roots) != 0 && len(operation.PrerequisiteRefs) == 0
	inlineSteps := operation.SchemaVersion == 3 && operation.Preparation == nil && operation.StepSetID == "" && operation.StepPageCount == 0 && operation.StepDigest == "" && len(operation.Roots) != 0 && len(operation.Prerequisites) == 0
	pagedSteps := operation.Preparation == nil && operation.StepSetID != "" && operation.StepPageCount != 0 && operation.StepDigest != "" && len(operation.Roots) == 0 && len(operation.Prerequisites) == 0 && len(operation.PrerequisiteRefs) == 0 && len(operation.Copies) == 0
	preparationState := operation.State == storageformat.FileOperationPreparing || operation.State == storageformat.FileOperationFailed
	preparingBuild := operation.SchemaVersion == 2 && preparationState && operation.Preparation != nil && operation.Preparation.SchemaVersion == 1 && operation.Preparation.RunSetID != "" && operation.Preparation.Phase == "build" && operation.Preparation.Request != nil && operation.Preparation.GateVersion != "" && operation.StepSetID == "" && operation.StepPageCount == 0 && operation.StepDigest == "" && len(operation.Roots) == 0 && len(operation.Prerequisites) == 0 && len(operation.Copies) == 0
	preparingSeal := operation.SchemaVersion == 2 && preparationState && operation.Preparation != nil && operation.Preparation.SchemaVersion == 1 && operation.Preparation.RunSetID != "" && operation.Preparation.Phase == "seal" && operation.Preparation.Request == nil && operation.Preparation.RunCount != 0 && operation.StepSetID == "" && operation.StepPageCount == 0 && operation.StepDigest == "" && len(operation.Roots) == 0 && len(operation.Prerequisites) == 0 && len(operation.Copies) == 0
	validInlineRefs := true
	previousReference := ""
	for _, reference := range operation.PrerequisiteRefs {
		if reference.Key <= previousReference || reference.BodyDigest == "" || reference.StagingKey != "" {
			validInlineRefs = false
			break
		}
		previousReference = reference.Key
	}
	validState := operation.State == storageformat.FileOperationPreparing || operation.State == storageformat.FileOperationRunning || operation.State == storageformat.FileOperationCommitted || operation.State == storageformat.FileOperationSucceeded || operation.State == storageformat.FileOperationFailed
	if operation.SchemaVersion != 1 && operation.SchemaVersion != 2 && operation.SchemaVersion != 3 || operation.SchemaVersion == 2 && operation.IntentFingerprint == "" || operation.OperationID == "" || operation.UserID == "" || operation.Fence == 0 || operation.Attempt == 0 || operation.StartedAt.IsZero() || operation.UpdatedAt.IsZero() || operation.ExpiresAt.IsZero() || !validState || !validInlineRefs || !legacySteps && !inlineSteps && !pagedSteps && !preparingBuild && !preparingSeal {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.FileOperation{}, domain.NewError(domain.ErrorInvalid, "invalid file operation")
	}
	return object, envelope, operation, nil
}

func domainFileOperation(operation storageformat.FileOperation) domain.Operation {
	stateValue := domain.OperationRunning
	switch operation.State {
	case storageformat.FileOperationSucceeded:
		stateValue = domain.OperationSucceeded
	case storageformat.FileOperationFailed:
		stateValue = domain.OperationFailed
	}
	return domain.Operation{
		ID: domain.OperationID(operation.OperationID), State: stateValue,
		ErrorKind: operation.ErrorKind, Error: operation.Error,
		StartedAt: operation.StartedAt, UpdatedAt: operation.UpdatedAt,
	}
}

func normalizeMutationObjects(objects []storageformat.MutationObject) ([]storageformat.MutationObject, error) {
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	result := objects[:0]
	for _, object := range objects {
		if object.Key == "" || len(object.Body) == 0 {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid operation prerequisite")
		}
		if len(result) > 0 && result[len(result)-1].Key == object.Key {
			if string(result[len(result)-1].Body) != string(object.Body) {
				return nil, domain.NewError(domain.ErrorInvalid, "conflicting operation prerequisite")
			}
			continue
		}
		result = append(result, object)
	}
	return result, nil
}

func validatePortableIdempotencyKey(value string) error {
	if value == "" {
		return nil
	}
	if !utf8.ValidString(value) || len(value) > 128 {
		return domain.NewError(domain.ErrorInvalid, "invalid idempotency key")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return domain.NewError(domain.ErrorInvalid, "invalid idempotency key")
		}
	}
	return nil
}

var _ state.Store = (*Engine)(nil)
