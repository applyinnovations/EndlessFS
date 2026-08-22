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

	StepOperationAfterPrepared  = "operation:after-prepared"
	StepOperationAfterCommitted = "operation:after-committed"
	StepOperationAfterFinalized = "operation:after-finalized"

	StepUploadCompletionAfterPrepared  = "upload-completion:after-prepared"
	StepUploadCompletionAfterCommitted = "upload-completion:after-committed"
	StepUploadCompletionAfterFinalized = "upload-completion:after-finalized"

	StepCreateDirectoryAfterPrepared  = "create-directory:after-prepared"
	StepCreateDirectoryAfterCommitted = "create-directory:after-committed"
	StepCreateDirectoryAfterFinalized = "create-directory:after-finalized"
)

type treePreparation struct {
	entry         storageformat.DirectoryEntry
	prerequisites []storageformat.MutationObject
	copies        []storageformat.MutationCopy
	occurrences   []relativeCatalogEntry
}

type relativeCatalogEntry struct {
	segments      []string
	entry         storageformat.DirectoryEntry
	manifestID    string
	contentSketch []string
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

	sourceTrail, err := s.resolveDirectoryMetadataTrail(ctx, from, request.Source.Parent())
	if err != nil {
		return domain.Operation{}, err
	}
	sourceParentNode := sourceTrail[len(sourceTrail)-1]
	sourceParentID, sourceParent := sourceParentNode.directoryID, sourceParentNode.snapshot
	if sourceParent.pending {
		return domain.Operation{}, domain.NewError(domain.ErrorUnavailable, "source directory has a pending operation")
	}
	sourceEntry, err := s.directoryIndexEntry(ctx, from, sourceParentID, sourceParent.manifest, request.Source.Name())
	if err != nil {
		return domain.Operation{}, err
	}
	if request.ExpectedSource != "" && request.ExpectedSource != domain.Version(sourceEntry.LogicalVersion) {
		return domain.Operation{}, domain.NewError(domain.ErrorPreconditionFailed, "source version does not match")
	}
	if sourceEntry.Kind == domain.EntryDirectory {
		sourceDirectory, err := s.readDirectoryMetadata(ctx, from, sourceEntry.DirectoryID, false)
		if err != nil {
			return domain.Operation{}, err
		}
		if sourceDirectory.pending {
			return domain.Operation{}, domain.NewError(domain.ErrorUnavailable, "source tree has a pending operation")
		}
		if sourceDirectory.recursiveBytes != sourceEntry.Size || sourceDirectory.recursiveFileCount != sourceEntry.FileCount || sourceDirectory.contentDigest != sourceEntry.ContentDigest {
			return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "source directory recursive aggregate mismatch")
		}
	}
	destinationTrail, err := s.resolveDirectoryMetadataTrail(ctx, to, request.Destination.Parent())
	if err != nil {
		return domain.Operation{}, err
	}
	destinationParentNode := destinationTrail[len(destinationTrail)-1]
	destinationParentID, destinationParent := destinationParentNode.directoryID, destinationParentNode.snapshot
	if destinationParent.pending {
		return domain.Operation{}, domain.NewError(domain.ErrorUnavailable, "destination directory has a pending operation")
	}
	resolved, destinationExisting, err := s.resolveIndexedDirectoryDestination(ctx, to, destinationParentID, destinationParent.manifest, request.Destination, conflict, request.ExpectedTarget)
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
	if len(preparation.occurrences) != 0 {
		preparation.occurrences[0].entry = preparation.entry
	}
	if move && from.Area() == to.Area() {
		preparation.occurrences, err = s.collectCatalogTree(ctx, from, sourceEntry)
		if err != nil {
			return domain.Operation{}, err
		}
		preparation.occurrences[0].entry = preparation.entry
	}
	sourceOccurrences := []relativeCatalogEntry(nil)
	if move {
		sourceOccurrences, err = s.collectCatalogTree(ctx, from, sourceEntry)
		if err != nil {
			return domain.Operation{}, err
		}
	}
	destinationOccurrences := []relativeCatalogEntry(nil)
	if destinationExisting != nil {
		destinationOccurrences, err = s.collectCatalogTree(ctx, to, *destinationExisting)
		if err != nil {
			return domain.Operation{}, err
		}
	}
	sourceContent := relativeDirectoryContentFiles(sourceOccurrences)
	destinationContent := relativeDirectoryContentFiles(destinationOccurrences)
	preparedContent := relativeDirectoryContentFiles(preparation.occurrences)

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
		if move {
			if err := applyDirectoryEntryChangeWithContent(rootUpdates, sourceTrail, &sourceEntry, nil, sourceContent, nil); err != nil {
				return domain.Operation{}, err
			}
		}
		if destinationExisting != nil && (!move || destinationExisting.Name != sourceEntry.Name) {
			if err := applyDirectoryEntryChangeWithContent(rootUpdates, destinationTrail, destinationExisting, nil, destinationContent, nil); err != nil {
				return domain.Operation{}, err
			}
		}
		if err := applyDirectoryEntryChangeWithContent(rootUpdates, sourceTrail, nil, &preparation.entry, nil, preparedContent); err != nil {
			return domain.Operation{}, err
		}
	} else {
		if move {
			if err := applyDirectoryEntryChangeWithContent(rootUpdates, sourceTrail, &sourceEntry, nil, sourceContent, nil); err != nil {
				return domain.Operation{}, err
			}
		}
		if destinationExisting != nil {
			if err := applyDirectoryEntryChangeWithContent(rootUpdates, destinationTrail, destinationExisting, nil, destinationContent, nil); err != nil {
				return domain.Operation{}, err
			}
		}
		if err := applyDirectoryEntryChangeWithContent(rootUpdates, destinationTrail, nil, &preparation.entry, nil, preparedContent); err != nil {
			return domain.Operation{}, err
		}
	}

	catalogChanges := make([]catalogChange, 0, len(sourceOccurrences)+len(destinationOccurrences)+len(preparation.occurrences))
	appendTree := func(scope domain.Scope, base domain.UserPath, entries []relativeCatalogEntry, remove bool) error {
		for _, item := range entries {
			path := base
			var joinErr error
			for _, segment := range item.segments {
				path, joinErr = path.Join(segment)
				if joinErr != nil {
					return joinErr
				}
			}
			occurrence, occurrenceErr := catalogOccurrence(scope, path, item.entry)
			if occurrenceErr != nil {
				return occurrenceErr
			}
			change := catalogChange{}
			if item.entry.Kind == domain.EntryDirectory && item.entry.FileCount > 0 {
				postings, postingErr := duplicateSimilarityPostings(scope, path, item.entry.DirectoryID, item.contentSketch)
				if postingErr != nil {
					return postingErr
				}
				if remove {
					change.similarityPre = postings
				} else {
					change.similarityPost = postings
				}
			}
			if remove {
				change.pre = &occurrence
			} else {
				change.post = &occurrence
			}
			catalogChanges = append(catalogChanges, change)
		}
		return nil
	}
	if err := appendTree(from, request.Source, sourceOccurrences, true); err != nil {
		return domain.Operation{}, err
	}
	if destinationExisting != nil {
		if err := appendTree(to, resolved, destinationOccurrences, true); err != nil {
			return domain.Operation{}, err
		}
	}
	if err := appendTree(to, resolved, preparation.occurrences, false); err != nil {
		return domain.Operation{}, err
	}
	operation, body, err := s.buildFileOperation(ctx, from.UserID(), operationID, ownerID, kind, rootUpdates, preparation.prerequisites, preparation.copies, catalogChanges)
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
	parentTrail, err := s.resolveDirectoryMetadataTrail(ctx, scope, request.Path.Parent())
	if err != nil {
		return domain.Operation{}, err
	}
	parentNode := parentTrail[len(parentTrail)-1]
	parent := parentNode.snapshot
	if parent.pending {
		return domain.Operation{}, domain.NewError(domain.ErrorUnavailable, "directory has a pending operation")
	}
	entry, err := s.directoryIndexEntry(ctx, scope, parentNode.directoryID, parent.manifest, request.Path.Name())
	if err != nil {
		return domain.Operation{}, err
	}
	if request.ExpectedVersion != "" && request.ExpectedVersion != domain.Version(entry.LogicalVersion) {
		return domain.Operation{}, domain.NewError(domain.ErrorPreconditionFailed, "entry version does not match")
	}
	if entry.Kind == domain.EntryDirectory {
		directory, err := s.readDirectoryMetadata(ctx, scope, entry.DirectoryID, false)
		if err != nil {
			return domain.Operation{}, err
		}
		if directory.pending {
			return domain.Operation{}, domain.NewError(domain.ErrorUnavailable, "directory has a pending operation")
		}
		if directory.recursiveBytes != entry.Size || directory.recursiveFileCount != entry.FileCount || directory.contentDigest != entry.ContentDigest {
			return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "directory recursive aggregate mismatch")
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
	entries, err := s.collectCatalogTree(ctx, scope, entry)
	if err != nil {
		return domain.Operation{}, err
	}
	updates := make(map[string]directoryUpdate)
	if err := applyDirectoryEntryChangeWithContent(updates, parentTrail, &entry, nil, relativeDirectoryContentFiles(entries), nil); err != nil {
		return domain.Operation{}, err
	}
	catalogChanges := make([]catalogChange, 0, len(entries))
	for _, item := range entries {
		path := request.Path
		for _, segment := range item.segments {
			path, err = path.Join(segment)
			if err != nil {
				return domain.Operation{}, err
			}
		}
		occurrence, err := catalogOccurrence(scope, path, item.entry)
		if err != nil {
			return domain.Operation{}, err
		}
		change := catalogChange{pre: &occurrence}
		if item.entry.Kind == domain.EntryDirectory && item.entry.FileCount > 0 {
			change.similarityPre, err = duplicateSimilarityPostings(scope, path, item.entry.DirectoryID, item.contentSketch)
			if err != nil {
				return domain.Operation{}, err
			}
		}
		catalogChanges = append(catalogChanges, change)
	}
	operation, body, err := s.buildFileOperation(ctx, scope.UserID(), operationID, ownerID, operationDelete, updates, nil, nil, catalogChanges)
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
			result.copies = append(result.copies, storageformat.MutationCopy{
				SourceKey: sourceKey.String(), DestinationKey: destinationKey.String(), Size: source.Size,
				MD5: source.MD5, CRC32C: source.CRC32C,
			})
			result.entry.BlobID = blobID
			result.entry.ModifiedAt = now
		}
		version, versionErr := directoryEntryVersion(result.entry)
		if versionErr != nil {
			return treePreparation{}, versionErr
		}
		result.entry.LogicalVersion = version
		result.occurrences = []relativeCatalogEntry{{entry: result.entry}}
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
		for _, occurrence := range cloned.occurrences {
			segments := make([]string, 0, len(occurrence.segments)+1)
			segments = append(segments, child.Name)
			segments = append(segments, occurrence.segments...)
			result.occurrences = append(result.occurrences, relativeCatalogEntry{segments: segments, entry: occurrence.entry, manifestID: occurrence.manifestID, contentSketch: append([]string(nil), occurrence.contentSketch...)})
		}
	}
	contentEntries := make([]storageformat.DirectoryContentIndexEntry, 0, source.FileCount)
	for _, occurrence := range result.occurrences {
		if occurrence.entry.Kind != domain.EntryFile {
			continue
		}
		path := domain.MustParseUserPath("/")
		for _, segment := range occurrence.segments {
			path, err = path.Join(segment)
			if err != nil {
				return treePreparation{}, err
			}
		}
		value, contentErr := directoryContentIndexEntry(path, occurrence.entry)
		if contentErr != nil {
			return treePreparation{}, contentErr
		}
		contentEntries = append(contentEntries, value)
	}
	prepared, err := s.prepareDirectoryWithContentEntries(to, directoryID, children, contentEntries, 1)
	if err != nil {
		return treePreparation{}, err
	}
	rootKey := storageformat.DirectoryRootKey(to.UserID().String(), areaName(to.Area()), directoryID)
	result.prerequisites = append(result.prerequisites, prepared.prerequisites...)
	result.prerequisites = append(result.prerequisites, storageformat.MutationObject{Key: rootKey.String(), Body: prepared.rootBody})
	result.entry.DirectoryID = directoryID
	if prepared.recursiveBytes != source.Size || prepared.recursiveFileCount != source.FileCount || prepared.contentDigest != source.ContentDigest {
		return treePreparation{}, domain.NewError(domain.ErrorInvalid, "source directory recursive aggregate mismatch")
	}
	result.entry.ContentDigest = prepared.contentDigest
	result.entry.ModifiedAt = now
	result.entry.LogicalVersion, err = directoryEntryVersion(result.entry)
	if err != nil {
		return treePreparation{}, err
	}
	result.occurrences = append([]relativeCatalogEntry{{entry: result.entry, manifestID: prepared.manifestID, contentSketch: append([]string(nil), prepared.contentSketch...)}}, result.occurrences...)
	return result, nil
}

func (s *FileStore) collectCatalogTree(ctx context.Context, scope domain.Scope, source storageformat.DirectoryEntry) ([]relativeCatalogEntry, error) {
	result := []relativeCatalogEntry{{entry: source}}
	if source.Kind == domain.EntryFile {
		return result, nil
	}
	directory, err := s.readDirectory(ctx, scope, source.DirectoryID, false)
	if err != nil {
		return nil, err
	}
	if directory.pending || directory.recursiveBytes != source.Size || directory.recursiveFileCount != source.FileCount || directory.contentDigest != source.ContentDigest {
		return nil, domain.NewError(domain.ErrorUnavailable, "source tree changed during duplicate catalog preparation")
	}
	result[0].manifestID = directory.manifestID
	result[0].contentSketch = append([]string(nil), directory.manifest.ContentSketch...)
	for _, child := range directory.entries {
		children, err := s.collectCatalogTree(ctx, scope, child)
		if err != nil {
			return nil, err
		}
		for _, occurrence := range children {
			segments := make([]string, 0, len(occurrence.segments)+1)
			segments = append(segments, child.Name)
			segments = append(segments, occurrence.segments...)
			result = append(result, relativeCatalogEntry{segments: segments, entry: occurrence.entry, manifestID: occurrence.manifestID, contentSketch: append([]string(nil), occurrence.contentSketch...)})
		}
	}
	return result, nil
}

func relativeDirectoryContentFiles(entries []relativeCatalogEntry) []relativeDirectoryContentFile {
	result := make([]relativeDirectoryContentFile, 0, len(entries))
	for _, item := range entries {
		if item.entry.Kind != domain.EntryFile {
			continue
		}
		result = append(result, relativeDirectoryContentFile{segments: append([]string(nil), item.segments...), entry: item.entry})
	}
	return result
}

func (s *FileStore) buildFileOperation(
	ctx context.Context,
	userID domain.UserID,
	operationID, ownerID, kind string,
	updates map[string]directoryUpdate,
	prerequisites []storageformat.MutationObject,
	copies []storageformat.MutationCopy,
	catalogChangeSets ...[]catalogChange,
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
		prepared, err := s.prepareDirectoryMutation(ctx, update, currentRevision+2)
		if err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		prerequisites = append(prerequisites, prepared.prerequisites...)
		key := objectstore.MustKey(keyValue)
		pendingRoot := storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: update.directoryID, ManifestID: preManifest, RecursiveBytes: update.snapshot.recursiveBytes, RecursiveFileCount: update.snapshot.recursiveFileCount, ContentAccumulator: update.snapshot.contentAccumulator, ContentDigest: update.snapshot.contentDigest, Pending: &storageformat.DirectoryTransition{
			OperationID: operationID, Fence: 1, PreManifestID: preManifest, PostManifestID: prepared.manifestID, PostRecursiveBytes: prepared.recursiveBytes, PostRecursiveFileCount: prepared.recursiveFileCount, PostContentAccumulator: prepared.contentAccumulator, PostContentDigest: prepared.contentDigest,
		}}
		pendingBody, err := storageformat.EncodeEnvelope(directoryRootSchema, key, currentRevision+1, pendingRoot)
		if err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		finalBody, err := storageformat.EncodeEnvelope(directoryRootSchema, key, currentRevision+2, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: update.directoryID, ManifestID: prepared.manifestID, RecursiveBytes: prepared.recursiveBytes, RecursiveFileCount: prepared.recursiveFileCount, ContentAccumulator: prepared.contentAccumulator, ContentDigest: prepared.contentDigest})
		if err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		var rollbackBody []byte
		if update.snapshot.exists {
			rollbackBody, err = storageformat.EncodeEnvelope(directoryRootSchema, key, currentRevision+2, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: update.directoryID, ManifestID: preManifest, RecursiveBytes: update.snapshot.recursiveBytes, RecursiveFileCount: update.snapshot.recursiveFileCount, ContentAccumulator: update.snapshot.contentAccumulator, ContentDigest: update.snapshot.contentDigest})
			if err != nil {
				return storageformat.FileOperation{}, nil, err
			}
		}
		roots = append(roots, storageformat.FileOperationRoot{
			Key: keyValue, ExpectedLogicalVersion: expected, PreExisted: update.snapshot.exists,
			PendingBody: pendingBody, FinalBody: finalBody, RollbackBody: rollbackBody,
		})
		if update.path.Valid() {
			change := catalogChange{}
			if !update.path.IsRoot() {
				before, err := catalogOccurrence(update.scope, update.path, update.entry)
				if err != nil {
					return storageformat.FileOperation{}, nil, err
				}
				change.pre = &before
			}
			if update.snapshot.recursiveFileCount > 0 {
				change.similarityPre, err = duplicateSimilarityPostings(update.scope, update.path, update.directoryID, update.snapshot.manifest.ContentSketch)
				if err != nil {
					return storageformat.FileOperation{}, nil, err
				}
			}
			afterEntry := update.entry
			afterEntry.Size = prepared.recursiveBytes
			afterEntry.FileCount = prepared.recursiveFileCount
			afterEntry.ContentDigest = prepared.contentDigest
			afterEntry.LogicalVersion, err = directoryEntryVersion(afterEntry)
			if err != nil {
				return storageformat.FileOperation{}, nil, err
			}
			if !update.path.IsRoot() {
				after, err := catalogOccurrence(update.scope, update.path, afterEntry)
				if err != nil {
					return storageformat.FileOperation{}, nil, err
				}
				change.post = &after
			}
			if prepared.recursiveFileCount > 0 {
				change.similarityPost, err = duplicateSimilarityPostings(update.scope, update.path, update.directoryID, prepared.contentSketch)
				if err != nil {
					return storageformat.FileOperation{}, nil, err
				}
			}
			catalogChangeSets = append(catalogChangeSets, []catalogChange{change})
		}
	}
	var catalogChanges []catalogChange
	for _, changes := range catalogChangeSets {
		catalogChanges = append(catalogChanges, changes...)
	}
	catalogRoots, err := s.buildCatalogOperationRoots(ctx, userID, operationID, catalogChanges)
	if err != nil {
		return storageformat.FileOperation{}, nil, err
	}
	roots = append(roots, catalogRoots...)
	sort.Slice(roots, func(i, j int) bool { return roots[i].Key < roots[j].Key })
	for index := 1; index < len(roots); index++ {
		if roots[index-1].Key == roots[index].Key {
			return storageformat.FileOperation{}, nil, domain.NewError(domain.ErrorInvalid, "file operation contains duplicate visibility roots")
		}
	}
	prerequisites, err = normalizeMutationObjects(prerequisites)
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
	if err := s.persistFileOperationStepPages(ctx, &operation); err != nil {
		return storageformat.FileOperation{}, nil, err
	}
	key := storageformat.OperationKey(userID.String(), operationID)
	body, err := storageformat.EncodeEnvelope(fileOperationSchema, key, 1, operation)
	return operation, body, err
}

func (s *FileStore) persistFileOperationStepPages(ctx context.Context, operation *storageformat.FileOperation) error {
	if operation == nil || operation.UserID == "" || operation.OperationID == "" || operation.ReplicaAttemptID == "" || operation.StepSetID != "" || operation.StepPageCount != 0 || operation.StepDigest != "" {
		return domain.NewError(domain.ErrorInvalid, "invalid operation step-page input")
	}
	if len(operation.Roots) == 0 {
		return domain.NewError(domain.ErrorInvalid, "file operation has no visibility roots")
	}
	references := make([]storageformat.MutationObjectReference, 0, len(operation.Prerequisites))
	for _, prerequisite := range operation.Prerequisites {
		artifactID := "prerequisite-" + storageformat.Digest([]byte(prerequisite.Key))
		stagingKey := storageformat.OperationStagingKey(operation.UserID, operation.OperationID, artifactID)
		if err := s.ensureImmutableOperationObject(ctx, stagingKey, prerequisite.Body); err != nil {
			return err
		}
		references = append(references, storageformat.MutationObjectReference{Key: prerequisite.Key, BodyDigest: storageformat.Digest(prerequisite.Body), StagingKey: stagingKey.String()})
	}
	pages := make([]storageformat.FileOperationStepPage, 0)
	for start := 0; start < len(operation.Roots); start += maxOperationPageRoots {
		end := min(start+maxOperationPageRoots, len(operation.Roots))
		pages = append(pages, storageformat.FileOperationStepPage{Roots: append([]storageformat.FileOperationRoot(nil), operation.Roots[start:end]...)})
	}
	for start := 0; start < len(references); start += maxOperationPagePrerequisites {
		end := min(start+maxOperationPagePrerequisites, len(references))
		pages = append(pages, storageformat.FileOperationStepPage{Prerequisites: append([]storageformat.MutationObjectReference(nil), references[start:end]...)})
	}
	for start := 0; start < len(operation.Copies); start += maxOperationPageCopies {
		end := min(start+maxOperationPageCopies, len(operation.Copies))
		pages = append(pages, storageformat.FileOperationStepPage{Copies: append([]storageformat.MutationCopy(nil), operation.Copies[start:end]...)})
	}
	previous := ""
	operation.StepSetID = operation.ReplicaAttemptID
	operation.StepsStaged = true
	for index := range pages {
		pages[index].SchemaVersion = 1
		pages[index].UserID = operation.UserID
		pages[index].OperationID = operation.OperationID
		pages[index].StepSetID = operation.StepSetID
		pages[index].Index = uint64(index)
		pages[index].PreviousDigest = previous
		key := stagedFileOperationStepPageKey(*operation, uint64(index))
		body, err := storageformat.EncodeEnvelope(fileOperationStepPageSchema, key, 1, pages[index])
		if err != nil {
			return domain.WrapError(domain.ErrorInvalid, "operation step page exceeds the bounded record limit", err)
		}
		if err := s.ensureImmutableOperationObject(ctx, key, body); err != nil {
			return err
		}
		previous = storageformat.Digest(body)
	}
	operation.StepPageCount = uint64(len(pages))
	operation.StepDigest = previous
	operation.Roots = nil
	operation.Prerequisites = nil
	operation.Copies = nil
	return nil
}

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

func (s *FileStore) ensureImmutableOperationObject(ctx context.Context, key objectstore.Key, body []byte) error {
	if _, err := s.engine.backend.Put(ctx, key, body, objectstore.PutCondition{Mode: objectstore.PutCreateOnly}); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
		return err
	}
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return err
	}
	if string(object.Body) != string(body) {
		return domain.NewError(domain.ErrorInvalid, "immutable operation step object conflicts with different bytes")
	}
	return nil
}

func (s *FileStore) forEachFileOperationStepPage(ctx context.Context, operation storageformat.FileOperation, visit func(storageformat.FileOperationStepPage) error) error {
	if operation.StepPageCount == 0 {
		if operation.StepSetID != "" || operation.StepDigest != "" || len(operation.Roots) == 0 {
			return domain.NewError(domain.ErrorInvalid, "invalid legacy file operation steps")
		}
		references := make([]storageformat.MutationObjectReference, 0, len(operation.Prerequisites))
		for _, prerequisite := range operation.Prerequisites {
			references = append(references, storageformat.MutationObjectReference{Key: prerequisite.Key, BodyDigest: storageformat.Digest(prerequisite.Body)})
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

func (s *FileStore) startFileOperation(ctx context.Context, operation storageformat.FileOperation, operationBody []byte, idempotencyKey, fingerprint string) (domain.Operation, error) {
	userID, err := domain.ParseUserID(operation.UserID)
	if err != nil {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "stored operation user is invalid")
	}
	operationKey := storageformat.OperationKey(operation.UserID, operation.OperationID)
	intent := storageformat.MutationIntent{
		Action: storageformat.MutationCreate, TargetKey: operationKey.String(), TargetBody: operationBody,
		RecoverOperationKey: operationKey.String(),
	}
	if operation.StepPageCount == 0 {
		intent.Prerequisites = operation.Prerequisites
		intent.Copies = operation.Copies
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
		if operation.StepPageCount == 0 {
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
	legacySteps := operation.StepSetID == "" && operation.StepPageCount == 0 && operation.StepDigest == "" && len(operation.Roots) != 0
	pagedSteps := operation.StepSetID != "" && operation.StepPageCount != 0 && operation.StepDigest != "" && len(operation.Roots) == 0 && len(operation.Prerequisites) == 0 && len(operation.Copies) == 0
	if operation.SchemaVersion != 1 || operation.OperationID == "" || operation.UserID == "" || operation.Fence == 0 || operation.Attempt == 0 || operation.StartedAt.IsZero() || operation.UpdatedAt.IsZero() || operation.ExpiresAt.IsZero() || !legacySteps && !pagedSteps {
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
