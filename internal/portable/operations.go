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
	fileOperationSchema = "file-operation-v1"
	idempotencySchema   = "idempotency-v1"

	operationCopy   = "copy"
	operationMove   = "move"
	operationDelete = "delete"

	StepOperationAfterPrepared  = "operation:after-prepared"
	StepOperationAfterCommitted = "operation:after-committed"
	StepOperationAfterFinalized = "operation:after-finalized"

	StepUploadCompletionAfterPrepared  = "upload-completion:after-prepared"
	StepUploadCompletionAfterCommitted = "upload-completion:after-committed"
	StepUploadCompletionAfterFinalized = "upload-completion:after-finalized"
)

type treePreparation struct {
	entry         storageformat.DirectoryEntry
	prerequisites []storageformat.MutationObject
	copies        []storageformat.MutationCopy
}

func (s *FileStore) Copy(ctx context.Context, from, to domain.Scope, request domain.CopyRequest) (domain.Operation, error) {
	return s.copyOrMove(ctx, false, from, to, request)
}

func (s *FileStore) Move(ctx context.Context, from, to domain.Scope, request domain.MoveRequest) (domain.Operation, error) {
	return s.copyOrMove(ctx, true, from, to, request)
}

func (s *FileStore) copyOrMove(ctx context.Context, move bool, from, to domain.Scope, request domain.CopyRequest) (domain.Operation, error) {
	if err := validateFileRequest(ctx, from); err != nil {
		return domain.Operation{}, err
	}
	if err := validateFileRequest(ctx, to); err != nil {
		return domain.Operation{}, err
	}
	if from.UserID() != to.UserID() {
		return domain.Operation{}, domain.NewError(domain.ErrorUnauthorized, "cross-user operations are forbidden")
	}
	if !request.Source.Valid() || request.Source.IsRoot() || !request.Destination.Valid() || request.Destination.IsRoot() {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "source and destination paths are required")
	}
	conflict, err := domain.NormalizeConflictMode(request.Conflict)
	if err != nil {
		return domain.Operation{}, err
	}
	if from == to && request.Source == request.Destination && conflict != domain.ConflictRename {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "source and destination are identical")
	}
	if from == to && request.Destination.IsDescendantOf(request.Source) {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "destination cannot be inside the source tree")
	}
	if err := validatePortableIdempotencyKey(request.IdempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	kind := operationCopy
	if move {
		kind = operationMove
	}
	fingerprint := storageformat.Digest([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s", kind, areaName(from.Area()), areaName(to.Area()), request.Source.String(), request.Destination.String(), conflict, request.ExpectedSource, request.ExpectedTarget)))
	if existing, found, err := s.lookupIdempotentOperation(ctx, from.UserID(), kind, request.IdempotencyKey, fingerprint); found || err != nil {
		return existing, err
	}

	sourceTrail, err := s.resolveDirectoryTrail(ctx, from, request.Source.Parent())
	if err != nil {
		return domain.Operation{}, err
	}
	sourceParentNode := sourceTrail[len(sourceTrail)-1]
	sourceParentID, sourceParent := sourceParentNode.directoryID, sourceParentNode.snapshot
	if sourceParent.pending {
		return domain.Operation{}, domain.NewError(domain.ErrorUnavailable, "source directory has a pending operation")
	}
	sourceEntry, found := findDirectoryEntry(sourceParent.entries, request.Source.Name())
	if !found {
		return domain.Operation{}, domain.NewError(domain.ErrorNotFound, "source not found")
	}
	if request.ExpectedSource != "" && request.ExpectedSource != domain.Version(sourceEntry.LogicalVersion) {
		return domain.Operation{}, domain.NewError(domain.ErrorPreconditionFailed, "source version does not match")
	}
	if sourceEntry.Kind == domain.EntryDirectory {
		sourceDirectory, err := s.readDirectory(ctx, from, sourceEntry.DirectoryID, false)
		if err != nil {
			return domain.Operation{}, err
		}
		if sourceDirectory.pending {
			return domain.Operation{}, domain.NewError(domain.ErrorUnavailable, "source tree has a pending operation")
		}
		if sourceDirectory.recursiveBytes != sourceEntry.Size {
			return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "source directory recursive byte aggregate mismatch")
		}
	}
	destinationTrail, err := s.resolveDirectoryTrail(ctx, to, request.Destination.Parent())
	if err != nil {
		return domain.Operation{}, err
	}
	destinationParentNode := destinationTrail[len(destinationTrail)-1]
	destinationParentID, destinationParent := destinationParentNode.directoryID, destinationParentNode.snapshot
	if destinationParent.pending {
		return domain.Operation{}, domain.NewError(domain.ErrorUnavailable, "destination directory has a pending operation")
	}
	resolved, destinationExisting, err := resolveDirectoryDestination(request.Destination, conflict, request.ExpectedTarget, destinationParent.entries)
	if err != nil {
		return domain.Operation{}, err
	}

	preparation := treePreparation{entry: sourceEntry}
	if !move || from.Area() != to.Area() {
		preparation, err = s.cloneTree(ctx, from, to, sourceEntry, !move)
		if err != nil {
			return domain.Operation{}, err
		}
	}
	preparation.entry.Name = resolved.Name()
	preparation.entry.NameDigest = storageformat.NameDigest(resolved.Name())
	if !move || preparation.entry.Kind == domain.EntryDirectory {
		preparation.entry.ModifiedAt = s.engine.clock.Now().UTC()
	}
	preparation.entry.LogicalVersion, err = directoryEntryVersion(preparation.entry)
	if err != nil {
		return domain.Operation{}, err
	}

	operationID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return domain.Operation{}, err
	}
	ownerID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return domain.Operation{}, err
	}
	rootUpdates := make(map[string]directoryUpdate)
	sourceRootKey := storageformat.DirectoryRootKey(from.UserID().String(), areaName(from.Area()), sourceParentID).String()
	destinationRootKey := storageformat.DirectoryRootKey(to.UserID().String(), areaName(to.Area()), destinationParentID).String()
	if sourceRootKey == destinationRootKey {
		entries := append([]storageformat.DirectoryEntry(nil), sourceParent.entries...)
		if move {
			entries = removeDirectoryEntry(entries, sourceEntry.Name)
		}
		if destinationExisting != nil && (!move || destinationExisting.Name != sourceEntry.Name) {
			entries = removeDirectoryEntry(entries, destinationExisting.Name)
		}
		entries = replaceDirectoryEntry(entries, nil, preparation.entry)
		if err := applyDirectoryChange(rootUpdates, sourceTrail, entries); err != nil {
			return domain.Operation{}, err
		}
	} else {
		if move {
			if err := applyDirectoryChange(rootUpdates, sourceTrail, removeDirectoryEntry(sourceParent.entries, sourceEntry.Name)); err != nil {
				return domain.Operation{}, err
			}
		}
		destinationEntries := currentDirectoryEntries(rootUpdates, destinationParentNode)
		if destinationExisting != nil {
			destinationEntries = removeDirectoryEntry(destinationEntries, destinationExisting.Name)
		}
		destinationEntries = replaceDirectoryEntry(destinationEntries, nil, preparation.entry)
		if err := applyDirectoryChange(rootUpdates, destinationTrail, destinationEntries); err != nil {
			return domain.Operation{}, err
		}
	}

	operation, body, err := s.buildFileOperation(ctx, from.UserID(), operationID, ownerID, kind, rootUpdates, preparation.prerequisites, preparation.copies)
	if err != nil {
		return domain.Operation{}, err
	}
	return s.startFileOperation(ctx, operation, body, request.IdempotencyKey, fingerprint)
}

func (s *FileStore) Delete(ctx context.Context, scope domain.Scope, request domain.DeleteRequest) (domain.Operation, error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return domain.Operation{}, err
	}
	if !request.Path.Valid() || request.Path.IsRoot() {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "delete path is invalid")
	}
	if err := validatePortableIdempotencyKey(request.IdempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	fingerprint := storageformat.Digest([]byte(fmt.Sprintf("delete\x00%s\x00%s\x00%s", areaName(scope.Area()), request.Path.String(), request.ExpectedVersion)))
	if existing, found, err := s.lookupIdempotentOperation(ctx, scope.UserID(), operationDelete, request.IdempotencyKey, fingerprint); found || err != nil {
		return existing, err
	}
	parentTrail, err := s.resolveDirectoryTrail(ctx, scope, request.Path.Parent())
	if err != nil {
		return domain.Operation{}, err
	}
	parentNode := parentTrail[len(parentTrail)-1]
	parent := parentNode.snapshot
	if parent.pending {
		return domain.Operation{}, domain.NewError(domain.ErrorUnavailable, "directory has a pending operation")
	}
	entry, found := findDirectoryEntry(parent.entries, request.Path.Name())
	if !found {
		return domain.Operation{}, domain.NewError(domain.ErrorNotFound, "entry not found")
	}
	if request.ExpectedVersion != "" && request.ExpectedVersion != domain.Version(entry.LogicalVersion) {
		return domain.Operation{}, domain.NewError(domain.ErrorPreconditionFailed, "entry version does not match")
	}
	if entry.Kind == domain.EntryDirectory {
		directory, err := s.readDirectory(ctx, scope, entry.DirectoryID, false)
		if err != nil {
			return domain.Operation{}, err
		}
		if directory.pending {
			return domain.Operation{}, domain.NewError(domain.ErrorUnavailable, "directory has a pending operation")
		}
		if directory.recursiveBytes != entry.Size {
			return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "directory recursive byte aggregate mismatch")
		}
	}
	operationID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return domain.Operation{}, err
	}
	ownerID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return domain.Operation{}, err
	}
	updates := make(map[string]directoryUpdate)
	if err := applyDirectoryChange(updates, parentTrail, removeDirectoryEntry(parent.entries, entry.Name)); err != nil {
		return domain.Operation{}, err
	}
	operation, body, err := s.buildFileOperation(ctx, scope.UserID(), operationID, ownerID, operationDelete, updates, nil, nil)
	if err != nil {
		return domain.Operation{}, err
	}
	return s.startFileOperation(ctx, operation, body, request.IdempotencyKey, fingerprint)
}

func (s *FileStore) GetOperation(ctx context.Context, userID domain.UserID, operationID domain.OperationID) (domain.Operation, error) {
	if err := ctx.Err(); err != nil {
		return domain.Operation{}, domain.WrapError(domain.ErrorUnavailable, "provider request canceled", err)
	}
	if !userID.Valid() || operationID == "" {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "user and operation IDs are required")
	}
	operation, err := s.readFileOperation(ctx, userID, string(operationID))
	if err != nil {
		return domain.Operation{}, err
	}
	return domainFileOperation(operation), nil
}

func (s *FileStore) cloneTree(ctx context.Context, from, to domain.Scope, source storageformat.DirectoryEntry, copyBlobs bool) (treePreparation, error) {
	result := treePreparation{entry: source}
	now := s.engine.clock.Now().UTC()
	if source.Kind == domain.EntryFile {
		if copyBlobs {
			blobID, err := s.engine.ids.OpaqueID()
			if err != nil {
				return treePreparation{}, err
			}
			sourceKey := storageformat.BlobKey(from.UserID().String(), source.BlobID)
			destinationKey := storageformat.BlobKey(to.UserID().String(), blobID)
			result.copies = append(result.copies, storageformat.MutationCopy{SourceKey: sourceKey.String(), DestinationKey: destinationKey.String(), Size: source.Size, SHA256: source.SHA256})
			result.entry.BlobID = blobID
			result.entry.ModifiedAt = now
		}
		return result, nil
	}
	sourceDirectory, err := s.readDirectory(ctx, from, source.DirectoryID, false)
	if err != nil {
		return treePreparation{}, err
	}
	if sourceDirectory.pending {
		return treePreparation{}, domain.NewError(domain.ErrorUnavailable, "source tree has a pending operation")
	}
	directoryID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return treePreparation{}, err
	}
	children := make([]storageformat.DirectoryEntry, 0, len(sourceDirectory.entries))
	for _, child := range sourceDirectory.entries {
		cloned, cloneErr := s.cloneTree(ctx, from, to, child, copyBlobs)
		if cloneErr != nil {
			return treePreparation{}, cloneErr
		}
		cloned.entry.LogicalVersion, cloneErr = directoryEntryVersion(cloned.entry)
		if cloneErr != nil {
			return treePreparation{}, cloneErr
		}
		children = append(children, cloned.entry)
		result.prerequisites = append(result.prerequisites, cloned.prerequisites...)
		result.copies = append(result.copies, cloned.copies...)
	}
	prepared, err := s.prepareDirectory(ctx, to, directoryID, children, 1)
	if err != nil {
		return treePreparation{}, err
	}
	rootKey := storageformat.DirectoryRootKey(to.UserID().String(), areaName(to.Area()), directoryID)
	result.prerequisites = append(result.prerequisites, prepared.prerequisites...)
	result.prerequisites = append(result.prerequisites, storageformat.MutationObject{Key: rootKey.String(), Body: prepared.rootBody})
	result.entry.DirectoryID = directoryID
	if prepared.recursiveBytes != source.Size {
		return treePreparation{}, domain.NewError(domain.ErrorInvalid, "source directory recursive byte aggregate mismatch")
	}
	result.entry.ModifiedAt = now
	return result, nil
}

func (s *FileStore) buildFileOperation(
	ctx context.Context,
	userID domain.UserID,
	operationID, ownerID, kind string,
	updates map[string]directoryUpdate,
	prerequisites []storageformat.MutationObject,
	copies []storageformat.MutationCopy,
) (storageformat.FileOperation, []byte, error) {
	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	roots := make([]storageformat.FileOperationRoot, 0, len(keys))
	for _, keyValue := range keys {
		update := updates[keyValue]
		if update.snapshot.pending {
			return storageformat.FileOperation{}, nil, domain.NewError(domain.ErrorUnavailable, "directory has a pending operation")
		}
		currentRevision := uint64(0)
		expected := ""
		preManifest := update.snapshot.root.ManifestID
		if update.snapshot.exists {
			currentRevision = update.snapshot.envelope.Revision
			expected = update.snapshot.envelope.LogicalVersion
		}
		prepared, err := s.prepareDirectory(ctx, update.scope, update.directoryID, update.entries, currentRevision+2)
		if err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		prerequisites = append(prerequisites, prepared.prerequisites...)
		key := objectstore.MustKey(keyValue)
		pendingRoot := storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: update.directoryID, ManifestID: preManifest, RecursiveBytes: update.snapshot.recursiveBytes, Pending: &storageformat.DirectoryTransition{
			OperationID: operationID, Fence: 1, PreManifestID: preManifest, PostManifestID: prepared.manifestID, PostRecursiveBytes: prepared.recursiveBytes,
		}}
		pendingBody, err := storageformat.EncodeEnvelope(directoryRootSchema, key, currentRevision+1, pendingRoot)
		if err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		finalBody, err := storageformat.EncodeEnvelope(directoryRootSchema, key, currentRevision+2, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: update.directoryID, ManifestID: prepared.manifestID, RecursiveBytes: prepared.recursiveBytes})
		if err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		var rollbackBody []byte
		if update.snapshot.exists {
			rollbackBody, err = storageformat.EncodeEnvelope(directoryRootSchema, key, currentRevision+2, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: update.directoryID, ManifestID: preManifest, RecursiveBytes: update.snapshot.recursiveBytes})
			if err != nil {
				return storageformat.FileOperation{}, nil, err
			}
		}
		roots = append(roots, storageformat.FileOperationRoot{
			Key: keyValue, ExpectedLogicalVersion: expected, PreExisted: update.snapshot.exists,
			PendingBody: pendingBody, FinalBody: finalBody, RollbackBody: rollbackBody,
		})
	}
	prerequisites, err := normalizeMutationObjects(prerequisites)
	if err != nil {
		return storageformat.FileOperation{}, nil, err
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i].DestinationKey < copies[j].DestinationKey })
	now := s.engine.clock.Now().UTC()
	operation := storageformat.FileOperation{
		SchemaVersion: 1, OperationID: operationID, UserID: userID.String(), Kind: kind,
		State: storageformat.FileOperationRunning, Attempt: 1, Fence: 1, ReplicaAttemptID: ownerID,
		ExpiresAt: now.Add(s.engine.leaseTTL), StartedAt: now, UpdatedAt: now,
		Roots: roots, Prerequisites: prerequisites, Copies: copies,
	}
	key := storageformat.OperationKey(userID.String(), operationID)
	body, err := storageformat.EncodeEnvelope(fileOperationSchema, key, 1, operation)
	return operation, body, err
}

func (s *FileStore) startFileOperation(ctx context.Context, operation storageformat.FileOperation, operationBody []byte, idempotencyKey, fingerprint string) (domain.Operation, error) {
	userID, err := domain.ParseUserID(operation.UserID)
	if err != nil {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "stored operation user is invalid")
	}
	operationKey := storageformat.OperationKey(operation.UserID, operation.OperationID)
	intent := storageformat.MutationIntent{
		Action: storageformat.MutationCreate, TargetKey: operationKey.String(), TargetBody: operationBody,
		Prerequisites: operation.Prerequisites, Copies: operation.Copies, RecoverOperationKey: operationKey.String(),
	}
	if idempotencyKey != "" {
		intent.Prerequisites = append(intent.Prerequisites, storageformat.MutationObject{Key: operationKey.String(), Body: operationBody})
		intent.Prerequisites, _ = normalizeMutationObjects(intent.Prerequisites)
		idempotencyObjectKey := storageformat.IdempotencyKey(operation.UserID, idempotencyKey)
		idempotencyBody, err := storageformat.EncodeEnvelope(idempotencySchema, idempotencyObjectKey, 1, storageformat.IdempotencyRecord{
			SchemaVersion: 1, UserID: operation.UserID, Kind: operation.Kind,
			KeyDigest: storageformat.Digest([]byte(idempotencyKey)), Fingerprint: fingerprint, OperationID: operation.OperationID,
		})
		if err != nil {
			return domain.Operation{}, err
		}
		intent.TargetKey = idempotencyObjectKey.String()
		intent.TargetBody = idempotencyBody
	}
	err = s.engine.withAdmission(ctx, intent, func() error {
		if err := s.engine.ensureMutationPrerequisites(ctx, intent.Prerequisites); err != nil {
			return err
		}
		if err := s.engine.ensureMutationCopies(ctx, operation.Copies); err != nil {
			return err
		}
		target := objectstore.MustKey(intent.TargetKey)
		if _, err := s.engine.backend.Put(ctx, target, intent.TargetBody, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err != nil {
			if errors.Is(err, domain.ErrConflict) && idempotencyKey != "" {
				existing, found, lookupErr := s.lookupIdempotentOperation(ctx, userID, operation.Kind, idempotencyKey, fingerprint)
				if found && lookupErr == nil {
					operation.OperationID = string(existing.ID)
					return nil
				}
			}
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

func (s *FileStore) executeFileOperation(ctx context.Context, key objectstore.Key) error {
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
		if err := s.engine.ensureMutationPrerequisites(ctx, operation.Prerequisites); err != nil {
			return err
		}
		if err := s.engine.ensureMutationCopies(ctx, operation.Copies); err != nil {
			return err
		}
		for _, root := range operation.Roots {
			if err := s.prepareOperationRoot(ctx, root); err != nil {
				if errors.Is(err, domain.ErrPreconditionFailed) || errors.Is(err, domain.ErrConflict) {
					return s.failFileOperation(ctx, object, envelope, operation, domain.ErrorPreconditionFailed, "directory changed before operation prepare")
				}
				return err
			}
		}
		preparedStep := StepOperationAfterPrepared
		if operation.Kind == "upload-complete" {
			preparedStep = StepUploadCompletionAfterPrepared
		}
		if err := s.engine.step(ctx, preparedStep); err != nil {
			return err
		}
		object, envelope, operation, err = s.readFileOperationObject(ctx, key)
		if err != nil {
			return err
		}
		if operation.State != storageformat.FileOperationRunning {
			return s.executeFileOperation(ctx, key)
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
		if operation.Kind == "upload-complete" {
			committedStep = StepUploadCompletionAfterCommitted
		}
		if err := s.engine.step(ctx, committedStep); err != nil {
			return err
		}
	}
	for _, root := range operation.Roots {
		if err := s.finalizeOperationRoot(ctx, root); err != nil {
			return err
		}
	}
	finalizedStep := StepOperationAfterFinalized
	if operation.Kind == "upload-complete" {
		finalizedStep = StepUploadCompletionAfterFinalized
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
	for _, root := range operation.Roots {
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
	if operation.SchemaVersion != 1 || operation.OperationID == "" || operation.UserID == "" || operation.Fence == 0 || operation.Attempt == 0 || operation.StartedAt.IsZero() || operation.UpdatedAt.IsZero() || operation.ExpiresAt.IsZero() || len(operation.Roots) == 0 {
		return objectstore.Object{}, storageformat.Envelope{}, storageformat.FileOperation{}, domain.NewError(domain.ErrorInvalid, "invalid file operation")
	}
	return object, envelope, operation, nil
}

func (s *FileStore) lookupIdempotentOperation(ctx context.Context, userID domain.UserID, kind, keyValue, fingerprint string) (domain.Operation, bool, error) {
	if keyValue == "" {
		return domain.Operation{}, false, nil
	}
	key := storageformat.IdempotencyKey(userID.String(), keyValue)
	object, err := s.engine.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Operation{}, false, nil
	}
	if err != nil {
		return domain.Operation{}, false, err
	}
	var envelope storageformat.Envelope
	var record storageformat.IdempotencyRecord
	if err := storageformat.DecodeEnvelope(object.Body, key, idempotencySchema, &envelope, &record); err != nil {
		return domain.Operation{}, false, err
	}
	if record.SchemaVersion != 1 || record.UserID != userID.String() || record.Kind != kind || record.KeyDigest != storageformat.Digest([]byte(keyValue)) || record.OperationID == "" {
		return domain.Operation{}, false, domain.NewError(domain.ErrorInvalid, "invalid idempotency record")
	}
	if record.Fingerprint != fingerprint {
		return domain.Operation{}, false, domain.NewError(domain.ErrorConflict, "idempotency key was used for a different request")
	}
	operation, err := s.readFileOperation(ctx, userID, record.OperationID)
	return domainFileOperation(operation), true, err
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

func removeDirectoryEntry(entries []storageformat.DirectoryEntry, name string) []storageformat.DirectoryEntry {
	result := make([]storageformat.DirectoryEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Name != name {
			result = append(result, entry)
		}
	}
	return result
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
