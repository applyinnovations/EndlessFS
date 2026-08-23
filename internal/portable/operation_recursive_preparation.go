package portable

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func storedOperationScope(userID domain.UserID, area string) (domain.Scope, error) {
	value := domain.AreaLive
	if area == "trash" {
		value = domain.AreaTrash
	} else if area != "live" {
		return domain.Scope{}, domain.NewError(domain.ErrorInvalid, "invalid prepared operation area")
	}
	return domain.NewScope(userID, value)
}

func validatePreparedOperationParent(trail []directoryTrailNode, pin storageformat.FileOperationDirectoryPin) error {
	if len(trail) == 0 {
		return domain.NewError(domain.ErrorInvalid, "prepared operation has no parent trail")
	}
	parent := trail[len(trail)-1]
	if pin.DirectoryID == "" || parent.directoryID != pin.DirectoryID || parent.snapshot.exists != pin.PreExisted || parent.snapshot.manifestID != pin.ManifestID || parent.snapshot.envelope.LogicalVersion != pin.LogicalVersion || parent.snapshot.pending || pin.PreExisted && (pin.ManifestID == "" || pin.LogicalVersion == "") || !pin.PreExisted && (pin.ManifestID != "" || pin.LogicalVersion != "") {
		return domain.NewError(domain.ErrorPreconditionFailed, "prepared operation parent changed before traversal")
	}
	return nil
}

func joinRelativeCatalogPath(base domain.UserPath, item relativeCatalogEntry) (domain.UserPath, error) {
	path := base
	var err error
	for _, segment := range item.segments {
		path, err = path.Join(segment)
		if err != nil {
			return domain.UserPath{}, err
		}
	}
	return path, nil
}

func (s *FileStore) addPreparedCatalogOccurrence(ctx context.Context, collector *operationPreparationRunCollector, userID domain.UserID, scope domain.Scope, base domain.UserPath, item relativeCatalogEntry, remove bool) error {
	path, err := joinRelativeCatalogPath(base, item)
	if err != nil {
		return err
	}
	occurrence, err := catalogOccurrence(scope, path, item.entry)
	if err != nil {
		return err
	}
	if remove {
		key := duplicateOccurrenceKey(userID.String(), occurrence)
		object, getErr := s.engine.backend.Get(ctx, key)
		if errors.Is(getErr, domain.ErrNotFound) {
			return nil
		}
		if getErr != nil {
			return getErr
		}
		var envelope storageformat.Envelope
		var root storageformat.DuplicateOccurrenceRoot
		if err := storageformat.DecodeEnvelope(object.Body, key, duplicateOccurrenceSchema, &envelope, &root); err != nil {
			return err
		}
		if root.SchemaVersion != 1 || root.UserID != userID.String() || root.Pending != nil {
			return domain.NewError(domain.ErrorUnavailable, "duplicate occurrence is changing concurrently")
		}
		if root.Current == nil {
			return nil
		}
		occurrence = *root.Current
	}
	change := catalogChange{}
	if remove {
		change.pre = &occurrence
	} else {
		change.post = &occurrence
	}
	if item.entry.Kind == domain.EntryDirectory && item.entry.FileCount > 0 && validateDirectoryContentSketch(item.contentSketch) == nil {
		postings, err := duplicateSimilarityPostings(scope, path, item.entry.DirectoryID, item.contentSketch)
		if err != nil {
			return err
		}
		if remove {
			for _, posting := range postings {
				key := duplicateSimilarityPostingKey(userID.String(), posting)
				object, getErr := s.engine.backend.Get(ctx, key)
				if errors.Is(getErr, domain.ErrNotFound) {
					continue
				}
				if getErr != nil {
					return getErr
				}
				var envelope storageformat.Envelope
				var root storageformat.DuplicateSimilarityPostingRoot
				if err := storageformat.DecodeEnvelope(object.Body, key, duplicateSimilaritySchema, &envelope, &root); err != nil {
					return err
				}
				if root.SchemaVersion != 1 || root.UserID != userID.String() || root.Pending != nil {
					return domain.NewError(domain.ErrorUnavailable, "duplicate similarity posting is changing concurrently")
				}
				if root.Current != nil && *root.Current == posting {
					change.similarityPre = append(change.similarityPre, posting)
				}
			}
		} else {
			for _, posting := range postings {
				key := duplicateSimilarityPostingKey(userID.String(), posting)
				object, getErr := s.engine.backend.Get(ctx, key)
				if errors.Is(getErr, domain.ErrNotFound) {
					change.similarityPost = append(change.similarityPost, posting)
					continue
				}
				if getErr != nil {
					return getErr
				}
				var envelope storageformat.Envelope
				var root storageformat.DuplicateSimilarityPostingRoot
				if err := storageformat.DecodeEnvelope(object.Body, key, duplicateSimilaritySchema, &envelope, &root); err != nil {
					return err
				}
				if root.SchemaVersion != 1 || root.UserID != userID.String() || root.Pending != nil {
					return domain.NewError(domain.ErrorUnavailable, "duplicate similarity posting is changing concurrently")
				}
				if root.Current == nil {
					change.similarityPost = append(change.similarityPost, posting)
				}
			}
		}
	}
	return s.addCatalogChangePreparationItems(ctx, collector, userID, change)
}

func (s *FileStore) addPreparedOperationObject(ctx context.Context, collector *operationPreparationRunCollector, operation storageformat.FileOperation, object storageformat.MutationObject) error {
	key, err := objectstore.ParseKey(object.Key)
	if err != nil || len(object.Body) == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid prepared operation prerequisite")
	}
	artifactID := "prerequisite-" + storageformat.Digest([]byte(object.Key))
	stagingKey := storageformat.OperationStagingKey(operation.UserID, operation.OperationID, artifactID)
	if err := s.ensureImmutableOperationObject(ctx, stagingKey, object.Body); err != nil {
		return err
	}
	reference := storageformat.MutationObjectReference{Key: key.String(), BodyDigest: storageformat.Digest(object.Body), StagingKey: stagingKey.String()}
	return collector.Add(ctx, storageformat.FileOperationPreparationItem{
		SortKey: "prerequisite\x00" + object.Key, Kind: storageformat.FileOperationPreparationPrerequisite, Prerequisite: &reference,
	})
}

func (s *FileStore) addPreparedOperationCopy(ctx context.Context, collector *operationPreparationRunCollector, copy storageformat.MutationCopy) error {
	value := copy
	return collector.Add(ctx, storageformat.FileOperationPreparationItem{
		SortKey: "copy\x00" + copy.DestinationKey, Kind: storageformat.FileOperationPreparationCopy, Copy: &value,
	})
}

func (s *FileStore) contentDeltaForEntry(ctx context.Context, scope domain.Scope, entry storageformat.DirectoryEntry, prefix []string, remove bool) (directoryContentDelta, error) {
	delta := directoryContentDelta{scope: scope, prefix: append([]string(nil), prefix...), remove: remove}
	if entry.Kind == domain.EntryFile {
		delta.entry = cloneDirectoryEntry(&entry)
		return delta, nil
	}
	storageScope, err := directoryEntryStorageScope(scope, entry)
	if err != nil {
		return directoryContentDelta{}, err
	}
	directory, err := s.readDirectoryEntryMetadata(ctx, storageScope, entry)
	if err != nil {
		return directoryContentDelta{}, err
	}
	if directory.pending || directory.recursiveBytes != entry.Size || directory.recursiveFileCount != entry.FileCount || directory.contentDigest != entry.ContentDigest {
		return directoryContentDelta{}, domain.NewError(domain.ErrorPreconditionFailed, "prepared operation content source changed")
	}
	delta.scope, delta.directoryID, delta.manifest = storageScope, entry.DirectoryID, directory.manifest
	return delta, nil
}

func appendDirectoryContentDelta(deltas map[string][]directoryContentDelta, trail []directoryTrailNode, delta directoryContentDelta) error {
	if len(trail) == 0 || len(delta.prefix) == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid prepared operation content trail")
	}
	prefix := append([]string(nil), delta.prefix...)
	for index := len(trail) - 1; index >= 0; index-- {
		node := trail[index]
		key := storageformat.DirectoryRootKey(node.scope.UserID().String(), areaName(node.scope.Area()), node.directoryID).String()
		copy := delta
		copy.prefix = append([]string(nil), prefix...)
		deltas[key] = append(deltas[key], copy)
		if index != 0 {
			prefix = append([]string{trail[index].entry.Name}, prefix...)
		}
	}
	return nil
}

func (s *FileStore) buildRecursiveFileOperationPreparation(ctx context.Context, object objectstore.Object, _ storageformat.Envelope, operation storageformat.FileOperation) error {
	request := operation.Preparation.Request
	userID, err := domain.ParseUserID(operation.UserID)
	if err != nil || request == nil || request.Fingerprint == "" {
		return domain.NewError(domain.ErrorInvalid, "invalid recursive operation preparation request")
	}
	from, err := storedOperationScope(userID, request.FromArea)
	if err != nil {
		return err
	}
	to := from
	if operation.Kind != operationDelete && operation.Kind != operationCreateDirectory {
		to, err = storedOperationScope(userID, request.ToArea)
		if err != nil {
			return err
		}
	}
	sourcePath, err := domain.ParseUserPath(request.Source)
	if err != nil || sourcePath.IsRoot() {
		return domain.NewError(domain.ErrorInvalid, "invalid prepared operation source")
	}
	sourceTrail, err := s.resolveDirectoryMetadataTrail(ctx, from, sourcePath.Parent())
	if err != nil {
		return err
	}
	if err := validatePreparedOperationParent(sourceTrail, request.SourceParent); err != nil {
		return err
	}
	sourceParent := sourceTrail[len(sourceTrail)-1]
	sourceEntry, err := s.directoryIndexEntry(ctx, sourceParent.scope, sourceParent.directoryID, sourceParent.snapshot.manifest, sourcePath.Name())
	if err != nil {
		return err
	}
	if sourceEntry != request.SourceEntry {
		return domain.NewError(domain.ErrorPreconditionFailed, "prepared operation source changed before traversal")
	}
	operationKey := object.Key
	collector, err := newResumableOperationPreparationRunCollector(s, operation, func(runCount uint64) error {
		return s.checkpointFileOperationPreparationRun(ctx, operationKey, operation.Fence, operation.ReplicaAttemptID, runCount)
	})
	if err != nil {
		return err
	}
	emitObject := func(value storageformat.MutationObject) error {
		return s.addPreparedOperationObject(ctx, collector, operation, value)
	}
	if operation.Kind == operationCreateDirectory {
		return s.buildCreateDirectoryReplacementPreparation(ctx, operationKey, operation, userID, from, sourcePath, sourceTrail, sourceEntry, collector, emitObject)
	}

	updates := make(map[string]directoryUpdate)
	contentDeltas := make(map[string][]directoryContentDelta)
	preparedEntry := sourceEntry
	var destinationTrail []directoryTrailNode
	var resolved domain.UserPath
	if operation.Kind != operationDelete {
		if request.DestinationParent == nil {
			return domain.NewError(domain.ErrorInvalid, "prepared operation destination pin is missing")
		}
		resolved, err = domain.ParseUserPath(request.ResolvedDestination)
		if err != nil || resolved.IsRoot() {
			return domain.NewError(domain.ErrorInvalid, "invalid prepared operation destination")
		}
		destinationTrail, err = s.resolveDirectoryMetadataTrail(ctx, to, resolved.Parent())
		if err != nil {
			return err
		}
		if err := validatePreparedOperationParent(destinationTrail, *request.DestinationParent); err != nil {
			return err
		}
		destinationParent := destinationTrail[len(destinationTrail)-1]
		currentDestination, lookupErr := s.directoryIndexEntry(ctx, destinationParent.scope, destinationParent.directoryID, destinationParent.snapshot.manifest, resolved.Name())
		if request.DestinationEntry == nil {
			if lookupErr != nil && !errors.Is(lookupErr, domain.ErrNotFound) {
				return lookupErr
			}
			if lookupErr == nil {
				return domain.NewError(domain.ErrorPreconditionFailed, "prepared operation destination changed before traversal")
			}
		} else if lookupErr != nil {
			return lookupErr
		} else if currentDestination != *request.DestinationEntry {
			return domain.NewError(domain.ErrorPreconditionFailed, "prepared operation destination changed before traversal")
		}
		var sourceCatalog relativeCatalogEntry
		if sourceEntry.Kind == domain.EntryDirectory {
			sourceStorageScope, scopeErr := directoryEntryStorageScope(sourceParent.scope, sourceEntry)
			if scopeErr != nil {
				return scopeErr
			}
			sourceDirectory, readErr := s.readDirectoryEntryMetadata(ctx, sourceStorageScope, sourceEntry)
			if readErr != nil {
				return readErr
			}
			if sourceDirectory.pending || sourceDirectory.recursiveBytes != sourceEntry.Size || sourceDirectory.recursiveFileCount != sourceEntry.FileCount || sourceDirectory.contentDigest != sourceEntry.ContentDigest {
				return domain.NewError(domain.ErrorPreconditionFailed, "prepared operation source snapshot changed")
			}
			preparedEntry.ManifestID = sourceDirectory.manifestID
			preparedEntry.StorageArea = areaName(sourceStorageScope.Area())
			sourceCatalog.manifestID = sourceDirectory.manifestID
			sourceCatalog.contentSketch = append([]string(nil), sourceDirectory.manifest.ContentSketch...)
		}
		sourceCatalog.entry = sourceEntry
		preparedEntry.Name, preparedEntry.NameDigest = resolved.Name(), storageformat.NameDigest(resolved.Name())
		if !request.Move || preparedEntry.Kind == domain.EntryDirectory {
			preparedEntry.ModifiedAt = operation.StartedAt.UTC()
		}
		preparedEntry.LogicalVersion, err = directoryEntryVersion(preparedEntry)
		if err != nil {
			return err
		}
		if request.Move {
			if err := s.addPreparedCatalogOccurrence(ctx, collector, userID, from, sourcePath, sourceCatalog, true); err != nil {
				return err
			}
		}
		destinationCatalog := sourceCatalog
		destinationCatalog.entry = preparedEntry
		// A same-owner file copy is another logical name for the same immutable
		// blob, not another physical duplicate. Directory aliases still publish
		// their exact root occurrence so structurally identical trees remain
		// discoverable without expanding descendant occurrences.
		if request.Move || preparedEntry.Kind == domain.EntryDirectory {
			if err := s.addPreparedCatalogOccurrence(ctx, collector, userID, to, resolved, destinationCatalog, false); err != nil {
				return err
			}
		}
		if request.DestinationEntry != nil {
			if err := s.addPreparedCatalogOccurrence(ctx, collector, userID, to, resolved, relativeCatalogEntry{entry: *request.DestinationEntry}, true); err != nil {
				return err
			}
		}
	}
	if operation.Kind == operationDelete {
		if err := s.addPreparedCatalogOccurrence(ctx, collector, userID, from, sourcePath, relativeCatalogEntry{entry: sourceEntry}, true); err != nil {
			return err
		}
	}

	if operation.Kind == operationDelete || request.Move {
		if err := applyDirectoryEntryChangeWithContent(updates, sourceTrail, &sourceEntry, nil, nil, nil); err != nil {
			return err
		}
		delta, err := s.contentDeltaForEntry(ctx, from, sourceEntry, []string{sourceEntry.Name}, true)
		if err != nil {
			return err
		}
		if err := appendDirectoryContentDelta(contentDeltas, sourceTrail, delta); err != nil {
			return err
		}
	}
	if operation.Kind != operationDelete {
		if request.DestinationEntry != nil && (!request.Move || request.DestinationEntry.Name != sourceEntry.Name || from.Area() != to.Area()) {
			if err := applyDirectoryEntryChangeWithContent(updates, destinationTrail, request.DestinationEntry, nil, nil, nil); err != nil {
				return err
			}
			delta, err := s.contentDeltaForEntry(ctx, to, *request.DestinationEntry, []string{request.DestinationEntry.Name}, true)
			if err != nil {
				return err
			}
			if err := appendDirectoryContentDelta(contentDeltas, destinationTrail, delta); err != nil {
				return err
			}
		}
		if err := applyDirectoryEntryChangeWithContent(updates, destinationTrail, nil, &preparedEntry, nil, nil); err != nil {
			return err
		}
		delta, err := s.contentDeltaForEntry(ctx, from, sourceEntry, []string{preparedEntry.Name}, false)
		if err != nil {
			return err
		}
		if err := appendDirectoryContentDelta(contentDeltas, destinationTrail, delta); err != nil {
			return err
		}
	}

	return s.finishRecursiveFileOperationPreparation(ctx, operationKey, operation, userID, collector, updates, contentDeltas, emitObject)
}

func (s *FileStore) buildCreateDirectoryReplacementPreparation(
	ctx context.Context,
	operationKey objectstore.Key,
	operation storageformat.FileOperation,
	userID domain.UserID,
	scope domain.Scope,
	path domain.UserPath,
	parentTrail []directoryTrailNode,
	existing storageformat.DirectoryEntry,
	collector *operationPreparationRunCollector,
	emitObject func(storageformat.MutationObject) error,
) error {
	if existing.Kind != domain.EntryDirectory || operation.Kind != operationCreateDirectory || collector == nil || emitObject == nil {
		return domain.NewError(domain.ErrorInvalid, "invalid create-directory replacement preparation")
	}
	emptyAccumulator, emptyDigest, err := directoryContentIdentity(nil)
	if err != nil {
		return err
	}
	directoryID := deterministicCloneID(operation.OperationID, "directory-create", path.String())
	manifestID := deterministicCloneID(operation.OperationID, "manifest-create", path.String())
	preparedChild, err := s.prepareClonedDirectoryRoots(scope, directoryID, manifestID, operation.StartedAt, directorySnapshot{
		manifest:           storageformat.DirectoryManifest{SchemaVersion: 2, DirectoryID: directoryID},
		contentAccumulator: emptyAccumulator,
		contentDigest:      emptyDigest,
	}, storageformat.DirectoryIndexChild{}, nil, storageformat.DirectoryContentIndexChild{}, emitObject)
	if err != nil {
		return err
	}
	entry := storageformat.DirectoryEntry{
		Name: path.Name(), NameDigest: storageformat.NameDigest(path.Name()), Kind: domain.EntryDirectory,
		DirectoryID: directoryID, ContentDigest: emptyDigest, ModifiedAt: operation.StartedAt.UTC(),
	}
	entry.LogicalVersion, err = directoryEntryVersion(entry)
	if err != nil {
		return err
	}
	if err := s.addPreparedCatalogOccurrence(ctx, collector, userID, scope, path, relativeCatalogEntry{entry: existing}, true); err != nil {
		return err
	}
	if err := s.addPreparedCatalogOccurrence(ctx, collector, userID, scope, path, relativeCatalogEntry{
		entry: entry, manifestID: preparedChild.manifestID, contentSketch: append([]string(nil), preparedChild.contentSketch...),
	}, false); err != nil {
		return err
	}
	updates := make(map[string]directoryUpdate)
	if err := applyDirectoryEntryChangeWithContent(updates, parentTrail, &existing, &entry, nil, nil); err != nil {
		return err
	}
	delta, err := s.contentDeltaForEntry(ctx, scope, existing, []string{existing.Name}, true)
	if err != nil {
		return err
	}
	contentDeltas := make(map[string][]directoryContentDelta)
	if err := appendDirectoryContentDelta(contentDeltas, parentTrail, delta); err != nil {
		return err
	}
	return s.finishRecursiveFileOperationPreparation(ctx, operationKey, operation, userID, collector, updates, contentDeltas, emitObject)
}

func (s *FileStore) finishRecursiveFileOperationPreparation(
	ctx context.Context,
	operationKey objectstore.Key,
	operation storageformat.FileOperation,
	userID domain.UserID,
	collector *operationPreparationRunCollector,
	updates map[string]directoryUpdate,
	contentDeltas map[string][]directoryContentDelta,
	emitObject func(storageformat.MutationObject) error,
) error {
	if collector == nil || emitObject == nil || len(updates) == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid recursive operation visibility preparation")
	}
	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := updates[keys[i]], updates[keys[j]]
		if len(left.path.Segments()) != len(right.path.Segments()) {
			return len(left.path.Segments()) > len(right.path.Segments())
		}
		return keys[i] < keys[j]
	})
	for _, keyValue := range keys {
		update := updates[keyValue]
		currentRevision := uint64(0)
		expected, preManifest := "", update.snapshot.root.ManifestID
		if update.snapshot.exists {
			currentRevision, expected = update.snapshot.envelope.Revision, update.snapshot.envelope.LogicalVersion
		}
		manifestID := deterministicCloneID(operation.OperationID, "manifest-update", keyValue+"\x00"+preManifest)
		prepared, err := s.prepareDirectoryMutationWithContentDeltas(ctx, update, currentRevision+2, manifestID, operation.StartedAt, contentDeltas[keyValue], emitObject)
		if err != nil {
			return err
		}
		if err := pinPreparedDirectoryInParent(updates, update, prepared); err != nil {
			return err
		}
		if err := s.addPreparedDirectoryCatalogChange(ctx, collector, userID, update, prepared); err != nil {
			return err
		}
		if !update.path.IsRoot() {
			continue
		}
		key := objectstore.MustKey(keyValue)
		pendingBody, err := storageformat.EncodeEnvelope(directoryRootSchema, key, currentRevision+1, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: update.directoryID, ManifestID: preManifest, RecursiveBytes: update.snapshot.recursiveBytes, RecursiveFileCount: update.snapshot.recursiveFileCount, ContentAccumulator: update.snapshot.contentAccumulator, ContentDigest: update.snapshot.contentDigest, Pending: &storageformat.DirectoryTransition{
			OperationID: operation.OperationID, Fence: 1, PreManifestID: preManifest, PostManifestID: prepared.manifestID,
			PostRecursiveBytes: prepared.recursiveBytes, PostRecursiveFileCount: prepared.recursiveFileCount, PostContentAccumulator: prepared.contentAccumulator, PostContentDigest: prepared.contentDigest,
		}})
		if err != nil {
			return err
		}
		finalBody, err := storageformat.EncodeEnvelope(directoryRootSchema, key, currentRevision+2, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: update.directoryID, ManifestID: prepared.manifestID, RecursiveBytes: prepared.recursiveBytes, RecursiveFileCount: prepared.recursiveFileCount, ContentAccumulator: prepared.contentAccumulator, ContentDigest: prepared.contentDigest})
		if err != nil {
			return err
		}
		var rollbackBody []byte
		if update.snapshot.exists {
			rollbackBody, err = storageformat.EncodeEnvelope(directoryRootSchema, key, currentRevision+2, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: update.directoryID, ManifestID: preManifest, RecursiveBytes: update.snapshot.recursiveBytes, RecursiveFileCount: update.snapshot.recursiveFileCount, ContentAccumulator: update.snapshot.contentAccumulator, ContentDigest: update.snapshot.contentDigest})
			if err != nil {
				return err
			}
		}
		root := storageformat.FileOperationRoot{Key: keyValue, ExpectedLogicalVersion: expected, PreExisted: update.snapshot.exists, PendingBody: pendingBody, FinalBody: finalBody, RollbackBody: rollbackBody}
		if err := collector.Add(ctx, storageformat.FileOperationPreparationItem{SortKey: "root\x00" + keyValue, Kind: storageformat.FileOperationPreparationRoot, Root: &root}); err != nil {
			return err
		}
	}
	runCount, err := collector.Close(ctx)
	if err != nil {
		return err
	}
	return s.sealBuiltFileOperationPreparation(ctx, operationKey, operation.Fence, operation.ReplicaAttemptID, runCount)
}

func (s *FileStore) addPreparedDirectoryCatalogChange(ctx context.Context, collector *operationPreparationRunCollector, userID domain.UserID, update directoryUpdate, prepared preparedDirectory) error {
	change, present, err := preparedDirectoryCatalogChange(update, prepared)
	if err != nil || !present {
		return err
	}
	if err := s.filterPreparedSimilarityChange(ctx, userID, &change); err != nil {
		return err
	}
	return s.addCatalogChangePreparationItems(ctx, collector, userID, change)
}

func (s *FileStore) filterPreparedSimilarityChange(ctx context.Context, userID domain.UserID, change *catalogChange) error {
	if change == nil || !userID.Valid() {
		return domain.NewError(domain.ErrorInvalid, "invalid duplicate similarity change")
	}
	read := func(posting storageformat.DuplicateSimilarityPosting) (*storageformat.DuplicateSimilarityPosting, error) {
		key := duplicateSimilarityPostingKey(userID.String(), posting)
		object, err := s.engine.backend.Get(ctx, key)
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var envelope storageformat.Envelope
		var root storageformat.DuplicateSimilarityPostingRoot
		if err := storageformat.DecodeEnvelope(object.Body, key, duplicateSimilaritySchema, &envelope, &root); err != nil {
			return nil, err
		}
		if root.SchemaVersion != 1 || root.UserID != userID.String() || root.Pending != nil {
			return nil, domain.NewError(domain.ErrorUnavailable, "duplicate similarity posting is changing concurrently")
		}
		return root.Current, nil
	}
	keptPre := make(map[string]storageformat.DuplicateSimilarityPosting)
	filteredPre := change.similarityPre[:0]
	for _, posting := range change.similarityPre {
		current, err := read(posting)
		if err != nil {
			return err
		}
		if current != nil && *current == posting {
			filteredPre = append(filteredPre, posting)
			keptPre[duplicateSimilarityPostingKey(userID.String(), posting).String()] = posting
		}
	}
	change.similarityPre = filteredPre
	filteredPost := change.similarityPost[:0]
	for _, posting := range change.similarityPost {
		current, err := read(posting)
		if err != nil {
			return err
		}
		key := duplicateSimilarityPostingKey(userID.String(), posting).String()
		_, replacing := keptPre[key]
		if current == nil || replacing {
			filteredPost = append(filteredPost, posting)
		}
	}
	change.similarityPost = filteredPost
	return nil
}

func preparedDirectoryCatalogChange(update directoryUpdate, prepared preparedDirectory) (catalogChange, bool, error) {
	if !update.path.Valid() {
		return catalogChange{}, false, nil
	}
	change := catalogChange{}
	var err error
	if !update.path.IsRoot() {
		before, err := catalogOccurrence(update.scope, update.path, update.entry)
		if err != nil {
			return catalogChange{}, false, err
		}
		change.pre = &before
	}
	if update.snapshot.recursiveFileCount > 0 && validateDirectoryContentSketch(update.snapshot.manifest.ContentSketch) == nil {
		change.similarityPre, err = duplicateSimilarityPostings(update.scope, update.path, update.directoryID, update.snapshot.manifest.ContentSketch)
		if err != nil {
			return catalogChange{}, false, err
		}
	}
	afterEntry := update.entry
	afterEntry.Size, afterEntry.FileCount, afterEntry.ContentDigest = prepared.recursiveBytes, prepared.recursiveFileCount, prepared.contentDigest
	if afterEntry.Kind == domain.EntryDirectory && !update.path.IsRoot() {
		afterEntry.ManifestID = prepared.manifestID
		afterEntry.StorageArea = areaName(update.scope.Area())
	}
	afterEntry.LogicalVersion, err = directoryEntryVersion(afterEntry)
	if err != nil {
		return catalogChange{}, false, err
	}
	if !update.path.IsRoot() {
		after, err := catalogOccurrence(update.scope, update.path, afterEntry)
		if err != nil {
			return catalogChange{}, false, err
		}
		change.post = &after
	}
	if prepared.recursiveFileCount > 0 && validateDirectoryContentSketch(prepared.contentSketch) == nil {
		change.similarityPost, err = duplicateSimilarityPostings(update.scope, update.path, update.directoryID, prepared.contentSketch)
		if err != nil {
			return catalogChange{}, false, err
		}
	}
	return change, true, nil
}

func (s *FileStore) prepareDirectoryMutationWithContentDeltas(ctx context.Context, update directoryUpdate, revision uint64, manifestID string, createdAt time.Time, deltas []directoryContentDelta, emit func(storageformat.MutationObject) error) (preparedDirectory, error) {
	if !update.scope.Valid() || update.directoryID == "" || manifestID == "" || createdAt.IsZero() || len(update.changes) == 0 || update.entryCount < 0 || update.recursiveBytes < 0 || update.recursiveFileCount < 0 || emit == nil {
		return preparedDirectory{}, domain.NewError(domain.ErrorInvalid, "invalid streamed directory mutation")
	}
	manifest := update.snapshot.manifest
	if update.snapshot.manifestID == "" {
		manifest = storageformat.DirectoryManifest{SchemaVersion: 2, DirectoryID: update.directoryID, ContentAccumulator: update.snapshot.contentAccumulator, ContentDigest: update.snapshot.contentDigest}
	}
	indexRoot, nodes, err := s.mutateDirectoryIndexChanges(ctx, update.scope, update.directoryID, manifest, update.changes)
	if err != nil {
		return preparedDirectory{}, err
	}
	for _, node := range nodes {
		if err := emit(node); err != nil {
			return preparedDirectory{}, err
		}
	}
	sortRoots, sortNodes, err := s.mutateDirectorySortIndexes(ctx, update.scope, update.directoryID, manifest, update.changes, update.entryCount)
	if err != nil {
		return preparedDirectory{}, err
	}
	for _, node := range sortNodes {
		if err := emit(node); err != nil {
			return preparedDirectory{}, err
		}
	}
	if update.entryCount > 0 && (indexRoot.EntryCount != uint64(update.entryCount) || indexRoot.RecursiveBytes != update.recursiveBytes || indexRoot.RecursiveFileCount != update.recursiveFileCount) { // #nosec G115 -- negative aggregates are rejected by the mutation functions.
		return preparedDirectory{}, domain.NewError(domain.ErrorInvalid, "streamed directory mutation aggregate mismatch")
	}
	accumulator, err := decodeDirectoryContentAccumulator(update.contentAccumulator)
	if err != nil {
		return preparedDirectory{}, err
	}
	expectedDigest, err := directoryContentAccumulatorDigest(accumulator, update.entryCount)
	if err != nil || expectedDigest != update.contentDigest || validateDirectorySortIndexRoots(sortRoots, update.entryCount) != nil {
		return preparedDirectory{}, domain.NewError(domain.ErrorInvalid, "streamed directory mutation content identity mismatch")
	}
	storedDeltas, err := storedDirectoryContentDeltas(deltas)
	if err != nil {
		return preparedDirectory{}, err
	}
	contentSketch := lazyDirectoryContentSketch(update, storedDeltas, deltas)
	contentBase, contentDeltas := lazyDirectoryContentForUpdate(update, storedDeltas)
	prepared, err := s.prepareDirectoryWithLazyContent(update.scope, update.directoryID, update.entryCount, update.recursiveBytes, update.recursiveFileCount, revision, indexRoot, sortRoots, nil, contentBase, contentDeltas, contentSketch, update.contentAccumulator, update.contentDigest, createdAt, manifestID)
	if err != nil {
		return preparedDirectory{}, err
	}
	for _, object := range prepared.prerequisites {
		if err := emit(object); err != nil {
			return preparedDirectory{}, err
		}
	}
	prepared.prerequisites = nil
	return prepared, nil
}
