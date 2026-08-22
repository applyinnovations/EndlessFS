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
	change := catalogChange{}
	if remove {
		change.pre = &occurrence
	} else {
		change.post = &occurrence
	}
	if item.entry.Kind == domain.EntryDirectory && item.entry.FileCount > 0 {
		postings, err := duplicateSimilarityPostings(scope, path, item.entry.DirectoryID, item.contentSketch)
		if err != nil {
			return err
		}
		if remove {
			change.similarityPre = postings
		} else {
			change.similarityPost = postings
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
	directory, err := s.readDirectoryMetadata(ctx, scope, entry.DirectoryID, false)
	if err != nil {
		return directoryContentDelta{}, err
	}
	if directory.pending || directory.recursiveBytes != entry.Size || directory.recursiveFileCount != entry.FileCount || directory.contentDigest != entry.ContentDigest {
		return directoryContentDelta{}, domain.NewError(domain.ErrorPreconditionFailed, "prepared operation content source changed")
	}
	delta.directoryID, delta.manifest = entry.DirectoryID, directory.manifest
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
	if operation.Kind != operationDelete {
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
	sourceEntry, err := s.directoryIndexEntry(ctx, from, sourceParent.directoryID, sourceParent.snapshot.manifest, sourcePath.Name())
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
	emitCopy := func(value storageformat.MutationCopy) error {
		return s.addPreparedOperationCopy(ctx, collector, value)
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
		currentDestination, lookupErr := s.directoryIndexEntry(ctx, to, destinationParent.directoryID, destinationParent.snapshot.manifest, resolved.Name())
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
		if !request.Move || from.Area() != to.Area() {
			prepared, cloneErr := s.cloneTreeStream(ctx, operation.OperationID, operation.StartedAt, from, to, sourceEntry, !request.Move, emitObject, emitCopy, func(item relativeCatalogEntry) error {
				if len(item.segments) == 0 {
					item.entry.Name, item.entry.NameDigest = resolved.Name(), storageformat.NameDigest(resolved.Name())
					version, versionErr := directoryEntryVersion(item.entry)
					if versionErr != nil {
						return versionErr
					}
					item.entry.LogicalVersion = version
				}
				return s.addPreparedCatalogOccurrence(ctx, collector, userID, to, resolved, item, false)
			})
			if cloneErr != nil {
				return cloneErr
			}
			preparedEntry = prepared.entry
		}
		preparedEntry.Name, preparedEntry.NameDigest = resolved.Name(), storageformat.NameDigest(resolved.Name())
		if !request.Move || preparedEntry.Kind == domain.EntryDirectory {
			preparedEntry.ModifiedAt = operation.StartedAt.UTC()
		}
		preparedEntry.LogicalVersion, err = directoryEntryVersion(preparedEntry)
		if err != nil {
			return err
		}
		if request.Move {
			if err := s.collectCatalogTreeStream(ctx, from, sourceEntry, func(item relativeCatalogEntry) error {
				if err := s.addPreparedCatalogOccurrence(ctx, collector, userID, from, sourcePath, item, true); err != nil {
					return err
				}
				if from.Area() == to.Area() {
					if len(item.segments) == 0 {
						item.entry = preparedEntry
					}
					return s.addPreparedCatalogOccurrence(ctx, collector, userID, to, resolved, item, false)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if request.DestinationEntry != nil {
			if err := s.collectCatalogTreeStream(ctx, to, *request.DestinationEntry, func(item relativeCatalogEntry) error {
				return s.addPreparedCatalogOccurrence(ctx, collector, userID, to, resolved, item, true)
			}); err != nil {
				return err
			}
		}
	}
	if operation.Kind == operationDelete {
		if err := s.collectCatalogTreeStream(ctx, from, sourceEntry, func(item relativeCatalogEntry) error {
			return s.addPreparedCatalogOccurrence(ctx, collector, userID, from, sourcePath, item, true)
		}); err != nil {
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

	keys := make([]string, 0, len(updates))
	for key := range updates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
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
		if err := s.addPreparedDirectoryCatalogChange(ctx, collector, userID, update, prepared); err != nil {
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
	if !update.path.Valid() {
		return nil
	}
	change := catalogChange{}
	var err error
	if !update.path.IsRoot() {
		before, err := catalogOccurrence(update.scope, update.path, update.entry)
		if err != nil {
			return err
		}
		change.pre = &before
	}
	if update.snapshot.recursiveFileCount > 0 {
		change.similarityPre, err = duplicateSimilarityPostings(update.scope, update.path, update.directoryID, update.snapshot.manifest.ContentSketch)
		if err != nil {
			return err
		}
	}
	afterEntry := update.entry
	afterEntry.Size, afterEntry.FileCount, afterEntry.ContentDigest = prepared.recursiveBytes, prepared.recursiveFileCount, prepared.contentDigest
	afterEntry.LogicalVersion, err = directoryEntryVersion(afterEntry)
	if err != nil {
		return err
	}
	if !update.path.IsRoot() {
		after, err := catalogOccurrence(update.scope, update.path, afterEntry)
		if err != nil {
			return err
		}
		change.post = &after
	}
	if prepared.recursiveFileCount > 0 {
		change.similarityPost, err = duplicateSimilarityPostings(update.scope, update.path, update.directoryID, prepared.contentSketch)
		if err != nil {
			return err
		}
	}
	return s.addCatalogChangePreparationItems(ctx, collector, userID, change)
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
	contentRoot, err := s.rebuildDirectoryContentIndexWithDeltas(ctx, update, deltas, emit)
	if err != nil {
		return preparedDirectory{}, err
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
	manifestKey := storageformat.DirectoryManifestKey(update.scope.UserID().String(), areaName(update.scope.Area()), update.directoryID, manifestID)
	manifestBody, err := storageformat.EncodeEnvelope(directoryManifestSchema, manifestKey, 1, storageformat.DirectoryManifest{
		SchemaVersion: 2, DirectoryID: update.directoryID, ManifestID: manifestID,
		IndexRootID: indexRoot.NodeID, IndexRootDigest: indexRoot.NodeDigest, SortIndexes: sortRoots,
		ContentIndexRootID: contentRoot.NodeID, ContentIndexRootDigest: contentRoot.NodeDigest, ContentSketch: append([]string(nil), contentRoot.Sketch...),
		EntryCount: update.entryCount, RecursiveBytes: update.recursiveBytes, RecursiveFileCount: update.recursiveFileCount,
		ContentAccumulator: update.contentAccumulator, ContentDigest: update.contentDigest, CreatedAt: createdAt.UTC(),
	})
	if err != nil {
		return preparedDirectory{}, err
	}
	if err := emit(storageformat.MutationObject{Key: manifestKey.String(), Body: manifestBody}); err != nil {
		return preparedDirectory{}, err
	}
	rootKey := storageformat.DirectoryRootKey(update.scope.UserID().String(), areaName(update.scope.Area()), update.directoryID)
	rootBody, err := storageformat.EncodeEnvelope(directoryRootSchema, rootKey, revision, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: update.directoryID, ManifestID: manifestID, RecursiveBytes: update.recursiveBytes, RecursiveFileCount: update.recursiveFileCount, ContentAccumulator: update.contentAccumulator, ContentDigest: update.contentDigest})
	if err != nil {
		return preparedDirectory{}, err
	}
	return preparedDirectory{manifestID: manifestID, recursiveBytes: update.recursiveBytes, recursiveFileCount: update.recursiveFileCount, contentAccumulator: update.contentAccumulator, contentDigest: update.contentDigest, contentSketch: append([]string(nil), contentRoot.Sketch...), rootBody: rootBody}, nil
}
