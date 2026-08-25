package portable

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// directOperationPreparationCollector keeps the bounded plan for an ordinary
// namespace mutation in process until it has been encoded as the durable
// operation record. No provider object is written before write admission. A
// plan which does not fit the canonical record limit is discarded and rebuilt
// by the resumable external preparation path.
type directOperationPreparationCollector struct {
	items   []storageformat.FileOperationPreparationItem
	bodies  map[string][]byte
	maximum int
}

func newDirectOperationPreparationCollector() *directOperationPreparationCollector {
	return &directOperationPreparationCollector{bodies: make(map[string][]byte), maximum: maxOperationPreparationPageItems}
}

func (collector *directOperationPreparationCollector) Add(_ context.Context, item storageformat.FileOperationPreparationItem) error {
	if collector == nil || len(collector.items) == collector.maximum {
		return errDirectOperationTooLarge
	}
	if err := validateOperationPreparationItem(item); err != nil {
		return err
	}
	collector.items = append(collector.items, item)
	return nil
}

func (collector *directOperationPreparationCollector) addObject(ctx context.Context, object storageformat.MutationObject) error {
	if collector == nil || object.Key == "" || len(object.Body) == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid direct operation prerequisite")
	}
	if prior, exists := collector.bodies[object.Key]; exists {
		if !reflect.DeepEqual(prior, object.Body) {
			return domain.NewError(domain.ErrorInvalid, "conflicting direct operation prerequisite")
		}
		return nil
	}
	collector.bodies[object.Key] = append([]byte(nil), object.Body...)
	reference := storageformat.MutationObjectReference{Key: object.Key, BodyDigest: storageformat.Digest(object.Body)}
	return collector.Add(ctx, storageformat.FileOperationPreparationItem{
		SortKey: "prerequisite\x00" + object.Key, Kind: storageformat.FileOperationPreparationPrerequisite, Prerequisite: &reference,
	})
}

var errDirectOperationTooLarge = errors.New("direct operation plan exceeds the bounded inline limit")

func (s *FileStore) buildDirectMoveOrDelete(
	ctx context.Context,
	operation storageformat.FileOperation,
	userID domain.UserID,
	from, to domain.Scope,
	sourcePath, resolved domain.UserPath,
	sourceTrail, destinationTrail []directoryTrailNode,
	sourceEntry storageformat.DirectoryEntry,
	destinationEntry *storageformat.DirectoryEntry,
	move bool,
) (storageformat.FileOperation, []byte, error) {
	collector := newDirectOperationPreparationCollector()
	emitObject := func(object storageformat.MutationObject) error { return collector.addObject(ctx, object) }
	updates := make(map[string]directoryUpdate)
	contentDeltas := make(map[string][]directoryContentDelta)
	preparedEntry := sourceEntry

	if operation.Kind != operationDelete {
		sourceCatalog := relativeCatalogEntry{entry: sourceEntry}
		if sourceEntry.Kind == domain.EntryDirectory {
			sourceScope, err := directoryEntryStorageScope(sourceTrail[len(sourceTrail)-1].scope, sourceEntry)
			if err != nil {
				return storageformat.FileOperation{}, nil, err
			}
			directory, err := s.readDirectoryEntryMetadata(ctx, sourceScope, sourceEntry)
			if err != nil {
				return storageformat.FileOperation{}, nil, err
			}
			if directory.pending || directory.recursiveBytes != sourceEntry.Size || directory.recursiveFileCount != sourceEntry.FileCount || directory.contentDigest != sourceEntry.ContentDigest {
				return storageformat.FileOperation{}, nil, domain.NewError(domain.ErrorPreconditionFailed, "direct operation source snapshot changed")
			}
			preparedEntry.ManifestID = directory.manifestID
			preparedEntry.StorageArea = areaName(sourceScope.Area())
			sourceCatalog.manifestID = directory.manifestID
			sourceCatalog.contentSketch = append([]string(nil), directory.manifest.ContentSketch...)
		}
		preparedEntry.Name, preparedEntry.NameDigest = resolved.Name(), storageformat.NameDigest(resolved.Name())
		if preparedEntry.Kind == domain.EntryDirectory {
			preparedEntry.ModifiedAt = operation.StartedAt.UTC()
		}
		var err error
		preparedEntry.LogicalVersion, err = directoryEntryVersion(preparedEntry)
		if err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		if move {
			if err := s.addPreparedCatalogOccurrence(ctx, collector, userID, from, sourcePath, sourceCatalog, true); err != nil {
				return storageformat.FileOperation{}, nil, err
			}
		}
		destinationCatalog := sourceCatalog
		destinationCatalog.entry = preparedEntry
		if move || preparedEntry.Kind == domain.EntryDirectory {
			if err := s.addPreparedCatalogOccurrence(ctx, collector, userID, to, resolved, destinationCatalog, false); err != nil {
				return storageformat.FileOperation{}, nil, err
			}
		}
		if destinationEntry != nil {
			if err := s.addPreparedCatalogOccurrence(ctx, collector, userID, to, resolved, relativeCatalogEntry{entry: *destinationEntry}, true); err != nil {
				return storageformat.FileOperation{}, nil, err
			}
		}
	} else if err := s.addPreparedCatalogOccurrence(ctx, collector, userID, from, sourcePath, relativeCatalogEntry{entry: sourceEntry}, true); err != nil {
		return storageformat.FileOperation{}, nil, err
	}

	if operation.Kind == operationDelete || move {
		if err := applyDirectoryEntryChangeWithContent(updates, sourceTrail, &sourceEntry, nil, nil, nil); err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		delta, err := s.contentDeltaForEntry(ctx, from, sourceEntry, []string{sourceEntry.Name}, true)
		if err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		if err := appendDirectoryContentDelta(contentDeltas, sourceTrail, delta); err != nil {
			return storageformat.FileOperation{}, nil, err
		}
	}
	if operation.Kind != operationDelete {
		if destinationEntry != nil && (!move || destinationEntry.Name != sourceEntry.Name || from.Area() != to.Area()) {
			if err := applyDirectoryEntryChangeWithContent(updates, destinationTrail, destinationEntry, nil, nil, nil); err != nil {
				return storageformat.FileOperation{}, nil, err
			}
			delta, err := s.contentDeltaForEntry(ctx, to, *destinationEntry, []string{destinationEntry.Name}, true)
			if err != nil {
				return storageformat.FileOperation{}, nil, err
			}
			if err := appendDirectoryContentDelta(contentDeltas, destinationTrail, delta); err != nil {
				return storageformat.FileOperation{}, nil, err
			}
		}
		if err := applyDirectoryEntryChangeWithContent(updates, destinationTrail, nil, &preparedEntry, nil, nil); err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		delta, err := s.contentDeltaForEntry(ctx, from, sourceEntry, []string{preparedEntry.Name}, false)
		if err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		if err := appendDirectoryContentDelta(contentDeltas, destinationTrail, delta); err != nil {
			return storageformat.FileOperation{}, nil, err
		}
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
			return storageformat.FileOperation{}, nil, err
		}
		if err := pinPreparedDirectoryInParent(updates, update, prepared); err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		if err := s.addPreparedDirectoryCatalogChange(ctx, collector, userID, update, prepared); err != nil {
			return storageformat.FileOperation{}, nil, err
		}
		if !update.path.IsRoot() {
			continue
		}
		key := objectstore.MustKey(keyValue)
		pendingBody, err := storageformat.EncodeEnvelope(directoryRootSchema, key, currentRevision+1, storageformat.DirectoryRoot{SchemaVersion: 1, DirectoryID: update.directoryID, ManifestID: preManifest, RecursiveBytes: update.snapshot.recursiveBytes, RecursiveFileCount: update.snapshot.recursiveFileCount, ContentAccumulator: update.snapshot.contentAccumulator, ContentDigest: update.snapshot.contentDigest, Pending: &storageformat.DirectoryTransition{
			OperationID: operation.OperationID, Fence: 1, PreManifestID: preManifest, PostManifestID: prepared.manifestID, PostRecursiveBytes: prepared.recursiveBytes, PostRecursiveFileCount: prepared.recursiveFileCount, PostContentAccumulator: prepared.contentAccumulator, PostContentDigest: prepared.contentDigest,
		}})
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
		root := storageformat.FileOperationRoot{Key: keyValue, ExpectedLogicalVersion: expected, PreExisted: update.snapshot.exists, PendingBody: pendingBody, FinalBody: finalBody, RollbackBody: rollbackBody}
		if err := collector.Add(ctx, storageformat.FileOperationPreparationItem{SortKey: "root\x00" + keyValue, Kind: storageformat.FileOperationPreparationRoot, Root: &root}); err != nil {
			return storageformat.FileOperation{}, nil, err
		}
	}
	return s.finishDirectFileOperation(ctx, userID, operation, collector)
}

func (s *FileStore) finishDirectFileOperation(ctx context.Context, userID domain.UserID, operation storageformat.FileOperation, collector *directOperationPreparationCollector) (storageformat.FileOperation, []byte, error) {
	sort.Slice(collector.items, func(i, j int) bool { return operationPreparationItemLess(collector.items[i], collector.items[j]) })
	var roots []storageformat.FileOperationRoot
	var prerequisites []storageformat.MutationObject
	previous := ""
	emitDirect := func(item storageformat.FileOperationPreparationItem) error {
		if item.SortKey == previous {
			return domain.NewError(domain.ErrorInvalid, "duplicate direct operation plan item")
		}
		previous = item.SortKey
		switch item.Kind {
		case storageformat.FileOperationPreparationRoot:
			roots = append(roots, *item.Root)
		case storageformat.FileOperationPreparationPrerequisite:
			body, ok := collector.bodies[item.Prerequisite.Key]
			if !ok || storageformat.Digest(body) != item.Prerequisite.BodyDigest {
				return domain.NewError(domain.ErrorInvalid, "direct operation prerequisite body is missing")
			}
			prerequisites = append(prerequisites, storageformat.MutationObject{Key: item.Prerequisite.Key, Body: body})
		default:
			return domain.NewError(domain.ErrorInvalid, "unreduced direct operation item")
		}
		return nil
	}
	if err := s.reduceDirectCatalogItems(ctx, userID, operation.OperationID, collector.items, emitDirect); err != nil {
		return storageformat.FileOperation{}, nil, err
	}
	if len(roots) == 0 {
		return storageformat.FileOperation{}, nil, domain.NewError(domain.ErrorInvalid, "direct file operation has no visibility roots")
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Key < roots[j].Key })
	for index := 1; index < len(roots); index++ {
		if roots[index-1].Key == roots[index].Key {
			return storageformat.FileOperation{}, nil, domain.NewError(domain.ErrorInvalid, "direct file operation has duplicate visibility roots")
		}
	}
	var err error
	prerequisites, err = normalizeMutationObjects(prerequisites)
	if err != nil {
		return storageformat.FileOperation{}, nil, err
	}
	operation.SchemaVersion = 3
	operation.State = storageformat.FileOperationRunning
	operation.Preparation = nil
	operation.Roots = roots
	operation.Prerequisites = prerequisites
	operation.PrerequisiteRefs = make([]storageformat.MutationObjectReference, 0, len(prerequisites))
	for _, prerequisite := range prerequisites {
		operation.PrerequisiteRefs = append(operation.PrerequisiteRefs, storageformat.MutationObjectReference{
			Key: prerequisite.Key, BodyDigest: storageformat.Digest(prerequisite.Body),
		})
	}
	key := storageformat.OperationKey(operation.UserID, operation.OperationID)
	persisted := operation
	persisted.Prerequisites = nil
	body, err := storageformat.EncodeEnvelope(fileOperationSchema, key, 1, persisted)
	if err != nil {
		return storageformat.FileOperation{}, nil, errDirectOperationTooLarge
	}
	return operation, body, nil
}

func (s *FileStore) reduceDirectCatalogItems(ctx context.Context, userID domain.UserID, operationID string, items []storageformat.FileOperationPreparationItem, emit func(storageformat.FileOperationPreparationItem) error) error {
	var pending operationPreparationReduction
	flush := func() error {
		if pending.sortKey == "" {
			return nil
		}
		var root storageformat.FileOperationRoot
		var err error
		switch pending.kind {
		case storageformat.FileOperationPreparationOccurrence:
			if sameDuplicateOccurrence(pending.occurrencePre, pending.occurrencePost) {
				pending = operationPreparationReduction{}
				return nil
			}
			value := pending.occurrencePre
			if value == nil {
				value = pending.occurrencePost
			}
			change, prepareErr := s.prepareOccurrenceRootChange(ctx, userID, operationID, duplicateOccurrenceKey(userID.String(), *value), pending.occurrencePre, pending.occurrencePost)
			if prepareErr != nil {
				return prepareErr
			}
			root = storageformat.FileOperationRoot{Key: change.key.String(), ExpectedLogicalVersion: change.expected, PreExisted: change.preExisted, PendingBody: change.pendingBody, FinalBody: change.finalBody, RollbackBody: change.rollbackBody}
		case storageformat.FileOperationPreparationSummary:
			if pending.summary.Delta == 0 {
				pending = operationPreparationReduction{}
				return nil
			}
			value := pending.summary
			key := storageformat.DuplicateSummaryKey(userID.String(), string(value.Kind), value.GroupID, value.Shard)
			change, prepareErr := s.prepareSummaryRootChange(ctx, userID, operationID, key, struct {
				groupID                string
				kind                   domain.DuplicateKind
				shard                  string
				size, fileCount, delta int64
			}{value.GroupID, value.Kind, value.Shard, value.Size, value.FileCount, value.Delta})
			if prepareErr != nil {
				return prepareErr
			}
			root = storageformat.FileOperationRoot{Key: change.key.String(), ExpectedLogicalVersion: change.expected, PreExisted: change.preExisted, PendingBody: change.pendingBody, FinalBody: change.finalBody, RollbackBody: change.rollbackBody}
		case storageformat.FileOperationPreparationSimilarity:
			if reflect.DeepEqual(pending.similarityPre, pending.similarityPost) {
				pending = operationPreparationReduction{}
				return nil
			}
			value := pending.similarityPre
			if value == nil {
				value = pending.similarityPost
			}
			root, err = s.prepareSimilarityPostingRootChange(ctx, userID, operationID, duplicateSimilarityPostingKey(userID.String(), *value), pending.similarityPre, pending.similarityPost)
		default:
			return domain.NewError(domain.ErrorInvalid, "invalid direct operation reduction")
		}
		if err != nil {
			return err
		}
		pending = operationPreparationReduction{}
		copy := root
		return emit(storageformat.FileOperationPreparationItem{SortKey: "root\x00" + root.Key, Kind: storageformat.FileOperationPreparationRoot, Root: &copy})
	}
	for _, item := range items {
		switch item.Kind {
		case storageformat.FileOperationPreparationRoot, storageformat.FileOperationPreparationPrerequisite, storageformat.FileOperationPreparationCopy:
			if err := flush(); err != nil {
				return err
			}
			if err := emit(item); err != nil {
				return err
			}
			continue
		case storageformat.FileOperationPreparationOccurrence, storageformat.FileOperationPreparationSummary, storageformat.FileOperationPreparationSimilarity:
		default:
			return domain.NewError(domain.ErrorInvalid, "invalid direct operation preparation item")
		}
		if pending.sortKey != "" && pending.sortKey != item.SortKey {
			if err := flush(); err != nil {
				return err
			}
		}
		if pending.sortKey == "" {
			pending.sortKey, pending.kind = item.SortKey, item.Kind
		}
		if pending.kind != item.Kind {
			return domain.NewError(domain.ErrorInvalid, "direct operation sort key mixes item kinds")
		}
		switch item.Kind {
		case storageformat.FileOperationPreparationOccurrence:
			value := item.Occurrence.Value
			target := &pending.occurrencePost
			if item.Occurrence.Before {
				target = &pending.occurrencePre
			}
			if *target != nil && !sameDuplicateOccurrence(*target, &value) {
				return domain.NewError(domain.ErrorInvalid, "conflicting direct duplicate occurrence")
			}
			*target = &value
		case storageformat.FileOperationPreparationSummary:
			value := item.Summary
			if pending.summary == nil {
				copy := *value
				pending.summary = &copy
			} else {
				if pending.summary.GroupID != value.GroupID || pending.summary.Kind != value.Kind || pending.summary.Shard != value.Shard || pending.summary.Size != value.Size || pending.summary.FileCount != value.FileCount || value.Delta > 0 && pending.summary.Delta > math.MaxInt64-value.Delta || value.Delta < 0 && pending.summary.Delta < math.MinInt64-value.Delta {
					return domain.NewError(domain.ErrorInvalid, "conflicting direct duplicate summary")
				}
				pending.summary.Delta += value.Delta
			}
		case storageformat.FileOperationPreparationSimilarity:
			value := item.Similarity.Value
			target := &pending.similarityPost
			if item.Similarity.Before {
				target = &pending.similarityPre
			}
			if *target != nil && !reflect.DeepEqual(*target, &value) {
				return domain.NewError(domain.ErrorInvalid, "conflicting direct similarity posting")
			}
			*target = &value
		}
	}
	return flush()
}
