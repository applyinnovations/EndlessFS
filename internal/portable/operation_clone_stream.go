package portable

import (
	"context"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type streamedTreePreparation struct {
	entry         storageformat.DirectoryEntry
	manifestID    string
	contentSketch []string
}

func deterministicCloneID(operationID, kind, sourceID string) string {
	return storageformat.Digest([]byte("endlessfs-operation-clone-v1\x00" + operationID + "\x00" + kind + "\x00" + sourceID))
}

func (s *FileStore) transformedCloneEntry(operationID string, modifiedAt time.Time, source storageformat.DirectoryEntry, copyBlobs bool) (storageformat.DirectoryEntry, error) {
	if err := validateDirectoryIndexEntry(source); err != nil {
		return storageformat.DirectoryEntry{}, err
	}
	result := source
	result.ModifiedAt = modifiedAt.UTC()
	switch result.Kind {
	case domain.EntryFile:
		if copyBlobs {
			result.BlobID = deterministicCloneID(operationID, "blob", source.BlobID)
		}
	case domain.EntryDirectory:
		result.DirectoryID = deterministicCloneID(operationID, "directory", source.DirectoryID)
	default:
		return storageformat.DirectoryEntry{}, domain.NewError(domain.ErrorInvalid, "invalid clone entry kind")
	}
	version, err := directoryEntryVersion(result)
	if err != nil {
		return storageformat.DirectoryEntry{}, err
	}
	result.LogicalVersion = version
	return result, nil
}

func (s *FileStore) directoryEntryIterator(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest, field domain.SortField, transform func(storageformat.DirectoryEntry) (storageformat.DirectoryEntry, error)) func() (storageformat.DirectoryEntry, bool, error) {
	after := ""
	var page []storageformat.DirectoryEntry
	index := 0
	return func() (storageformat.DirectoryEntry, bool, error) {
		if index == len(page) {
			var err error
			if field == domain.SortName {
				page, err = s.collectDirectoryIndexEntries(ctx, scope, directoryID, manifest, after, false, maxDirectoryIndexItems)
			} else {
				page, err = s.collectDirectorySortIndexEntries(ctx, scope, directoryID, manifest, field, after, false, maxDirectoryIndexItems)
			}
			if err != nil {
				return storageformat.DirectoryEntry{}, false, err
			}
			index = 0
			if len(page) == 0 {
				return storageformat.DirectoryEntry{}, false, nil
			}
			last := page[len(page)-1]
			if field == domain.SortName {
				after = last.Name
			} else {
				after, err = directorySortKey(field, last)
				if err != nil {
					return storageformat.DirectoryEntry{}, false, err
				}
			}
		}
		entry := page[index]
		index++
		if transform == nil {
			return entry, true, nil
		}
		transformed, err := transform(entry)
		return transformed, err == nil, err
	}
}

func (s *FileStore) directoryContentIterator(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest) func() (storageformat.DirectoryContentIndexEntry, bool, error) {
	after := ""
	var page []storageformat.DirectoryContentIndexEntry
	index := 0
	return func() (storageformat.DirectoryContentIndexEntry, bool, error) {
		if index == len(page) {
			var err error
			page, err = s.collectDirectoryContentIndexEntries(ctx, scope, directoryID, manifest, after, maxDirectoryIndexItems)
			if err != nil {
				return storageformat.DirectoryContentIndexEntry{}, false, err
			}
			index = 0
			if len(page) == 0 {
				return storageformat.DirectoryContentIndexEntry{}, false, nil
			}
			after, err = directoryContentIndexKey(page[len(page)-1])
			if err != nil {
				return storageformat.DirectoryContentIndexEntry{}, false, err
			}
		}
		entry := page[index]
		index++
		return entry, true, nil
	}
}

func (s *FileStore) cloneTreeStream(
	ctx context.Context,
	operationID string,
	modifiedAt time.Time,
	from, to domain.Scope,
	source storageformat.DirectoryEntry,
	copyBlobs bool,
	emitObject func(storageformat.MutationObject) error,
	emitCopy func(storageformat.MutationCopy) error,
	visitOccurrence func(relativeCatalogEntry) error,
) (streamedTreePreparation, error) {
	if operationID == "" || !from.Valid() || !to.Valid() || emitObject == nil || emitCopy == nil || visitOccurrence == nil {
		return streamedTreePreparation{}, domain.NewError(domain.ErrorInvalid, "invalid streaming tree clone")
	}
	return s.cloneTreeStreamAt(ctx, operationID, modifiedAt, from, to, source, copyBlobs, nil, emitObject, emitCopy, visitOccurrence)
}

func (s *FileStore) collectCatalogTreeStream(ctx context.Context, scope domain.Scope, source storageformat.DirectoryEntry, visit func(relativeCatalogEntry) error) error {
	if !scope.Valid() || visit == nil {
		return domain.NewError(domain.ErrorInvalid, "invalid streaming catalog traversal")
	}
	return s.collectCatalogTreeStreamAt(ctx, scope, source, nil, visit)
}

func (s *FileStore) collectCatalogTreeStreamAt(ctx context.Context, scope domain.Scope, source storageformat.DirectoryEntry, segments []string, visit func(relativeCatalogEntry) error) error {
	item := relativeCatalogEntry{segments: append([]string(nil), segments...), entry: source}
	if source.Kind == domain.EntryFile {
		return visit(item)
	}
	directory, err := s.readDirectoryMetadata(ctx, scope, source.DirectoryID, false)
	if err != nil {
		return err
	}
	if directory.pending {
		return domain.NewError(domain.ErrorUnavailable, "source tree changed during streaming duplicate catalog preparation")
	}
	if directory.recursiveBytes != source.Size || directory.recursiveFileCount != source.FileCount || directory.contentDigest != source.ContentDigest {
		return domain.NewError(domain.ErrorInvalid, "source tree aggregate is inconsistent during streaming duplicate catalog preparation")
	}
	item.manifestID = directory.manifestID
	item.contentSketch = append([]string(nil), directory.manifest.ContentSketch...)
	if err := visit(item); err != nil {
		return err
	}
	children := s.directoryEntryIterator(ctx, scope, source.DirectoryID, directory.manifest, domain.SortName, nil)
	for {
		child, ok, err := children()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		childSegments := append(append([]string(nil), segments...), child.Name)
		if err := s.collectCatalogTreeStreamAt(ctx, scope, child, childSegments, visit); err != nil {
			return err
		}
	}
}

func (s *FileStore) cloneTreeStreamAt(
	ctx context.Context,
	operationID string,
	modifiedAt time.Time,
	from, to domain.Scope,
	source storageformat.DirectoryEntry,
	copyBlobs bool,
	segments []string,
	emitObject func(storageformat.MutationObject) error,
	emitCopy func(storageformat.MutationCopy) error,
	visitOccurrence func(relativeCatalogEntry) error,
) (streamedTreePreparation, error) {
	entry, err := s.transformedCloneEntry(operationID, modifiedAt, source, copyBlobs)
	if err != nil {
		return streamedTreePreparation{}, err
	}
	if source.Kind == domain.EntryFile {
		if copyBlobs {
			if err := emitCopy(storageformat.MutationCopy{
				SourceKey:      storageformat.BlobKey(from.UserID().String(), source.BlobID).String(),
				DestinationKey: storageformat.BlobKey(to.UserID().String(), entry.BlobID).String(),
				Size:           source.Size, MD5: source.MD5, CRC32C: source.CRC32C,
			}); err != nil {
				return streamedTreePreparation{}, err
			}
		}
		if err := visitOccurrence(relativeCatalogEntry{segments: append([]string(nil), segments...), entry: entry}); err != nil {
			return streamedTreePreparation{}, err
		}
		return streamedTreePreparation{entry: entry}, nil
	}
	sourceDirectory, err := s.readDirectoryMetadata(ctx, from, source.DirectoryID, false)
	if err != nil {
		return streamedTreePreparation{}, err
	}
	if sourceDirectory.pending {
		return streamedTreePreparation{}, domain.NewError(domain.ErrorUnavailable, "source tree changed during streaming clone preparation")
	}
	if sourceDirectory.recursiveBytes != source.Size || sourceDirectory.recursiveFileCount != source.FileCount || sourceDirectory.contentDigest != source.ContentDigest {
		return streamedTreePreparation{}, domain.NewError(domain.ErrorInvalid, "source tree aggregate is inconsistent during streaming clone preparation")
	}
	directoryID := entry.DirectoryID
	transform := func(value storageformat.DirectoryEntry) (storageformat.DirectoryEntry, error) {
		return s.transformedCloneEntry(operationID, modifiedAt, value, copyBlobs)
	}
	indexRoot, err := s.buildDirectoryIndexStream(to, directoryID, s.directoryEntryIterator(ctx, from, source.DirectoryID, sourceDirectory.manifest, domain.SortName, transform), emitObject)
	if err != nil {
		return streamedTreePreparation{}, err
	}
	sortRoots := make([]storageformat.DirectorySortIndexRoot, 0, len(directorySecondarySorts))
	for _, field := range directorySecondarySorts {
		sourceField := field
		if field == domain.SortModified {
			// Every cloned entry receives the operation timestamp, so complete
			// modified ordering is identical to name ordering.
			sourceField = domain.SortName
		}
		root, buildErr := s.buildDirectorySortIndexStream(to, directoryID, field, s.directoryEntryIterator(ctx, from, source.DirectoryID, sourceDirectory.manifest, sourceField, transform), emitObject)
		if buildErr != nil {
			return streamedTreePreparation{}, buildErr
		}
		if sourceDirectory.manifest.EntryCount > 0 {
			sortRoots = append(sortRoots, storageformat.DirectorySortIndexRoot{Sort: field, NodeID: root.NodeID, NodeDigest: root.NodeDigest})
		}
	}
	contentRoot, err := s.buildDirectoryContentIndexStream(to, directoryID, s.directoryContentIterator(ctx, from, source.DirectoryID, sourceDirectory.manifest), emitObject)
	if err != nil {
		return streamedTreePreparation{}, err
	}
	manifestID := deterministicCloneID(operationID, "manifest", source.DirectoryID+"\x00"+sourceDirectory.manifestID)
	prepared, err := s.prepareClonedDirectoryRoots(to, directoryID, manifestID, modifiedAt, sourceDirectory, indexRoot, sortRoots, contentRoot, emitObject)
	if err != nil {
		return streamedTreePreparation{}, err
	}
	entry.ContentDigest = prepared.contentDigest
	entry.LogicalVersion, err = directoryEntryVersion(entry)
	if err != nil {
		return streamedTreePreparation{}, err
	}
	if err := visitOccurrence(relativeCatalogEntry{segments: append([]string(nil), segments...), entry: entry, manifestID: prepared.manifestID, contentSketch: append([]string(nil), prepared.contentSketch...)}); err != nil {
		return streamedTreePreparation{}, err
	}
	children := s.directoryEntryIterator(ctx, from, source.DirectoryID, sourceDirectory.manifest, domain.SortName, nil)
	for {
		child, ok, childErr := children()
		if childErr != nil {
			return streamedTreePreparation{}, childErr
		}
		if !ok {
			break
		}
		childSegments := append(append([]string(nil), segments...), child.Name)
		if _, childErr = s.cloneTreeStreamAt(ctx, operationID, modifiedAt, from, to, child, copyBlobs, childSegments, emitObject, emitCopy, visitOccurrence); childErr != nil {
			return streamedTreePreparation{}, childErr
		}
	}
	return streamedTreePreparation{entry: entry, manifestID: prepared.manifestID, contentSketch: append([]string(nil), prepared.contentSketch...)}, nil
}

func (s *FileStore) prepareClonedDirectoryRoots(
	scope domain.Scope,
	directoryID, manifestID string,
	createdAt time.Time,
	source directorySnapshot,
	indexRoot storageformat.DirectoryIndexChild,
	sortRoots []storageformat.DirectorySortIndexRoot,
	contentRoot storageformat.DirectoryContentIndexChild,
	emit func(storageformat.MutationObject) error,
) (preparedDirectory, error) {
	entryCount := source.manifest.EntryCount
	if entryCount < 0 || source.recursiveBytes < 0 || source.recursiveFileCount < 0 || entryCount == 0 && indexRoot.NodeID != "" || entryCount > 0 && (indexRoot.NodeID == "" || indexRoot.EntryCount != uint64(entryCount) || indexRoot.RecursiveBytes != source.recursiveBytes || indexRoot.RecursiveFileCount != source.recursiveFileCount) || validateDirectorySortIndexRoots(sortRoots, entryCount) != nil || source.recursiveFileCount == 0 && contentRoot.NodeID != "" || source.recursiveFileCount > 0 && (contentRoot.NodeID == "" || contentRoot.EntryCount != uint64(source.recursiveFileCount)) {
		return preparedDirectory{}, domain.NewError(domain.ErrorInvalid, "invalid streamed clone directory indexes")
	}
	manifest := storageformat.DirectoryManifest{
		SchemaVersion: 2, DirectoryID: directoryID, ManifestID: manifestID, EntryCount: entryCount,
		RecursiveBytes: source.recursiveBytes, RecursiveFileCount: source.recursiveFileCount,
		IndexRootID: indexRoot.NodeID, IndexRootDigest: indexRoot.NodeDigest,
		SortIndexes:        append([]storageformat.DirectorySortIndexRoot(nil), sortRoots...),
		ContentIndexRootID: contentRoot.NodeID, ContentIndexRootDigest: contentRoot.NodeDigest,
		ContentSketch:      append([]string(nil), contentRoot.Sketch...),
		ContentAccumulator: source.contentAccumulator, ContentDigest: source.contentDigest,
		CreatedAt: createdAt.UTC(),
	}
	manifestKey := storageformat.DirectoryManifestKey(scope.UserID().String(), areaName(scope.Area()), directoryID, manifestID)
	manifestBody, err := storageformat.EncodeEnvelope(directoryManifestSchema, manifestKey, 1, manifest)
	if err != nil {
		return preparedDirectory{}, err
	}
	if err := emit(storageformat.MutationObject{Key: manifestKey.String(), Body: manifestBody}); err != nil {
		return preparedDirectory{}, err
	}
	rootKey := storageformat.DirectoryRootKey(scope.UserID().String(), areaName(scope.Area()), directoryID)
	rootBody, err := storageformat.EncodeEnvelope(directoryRootSchema, rootKey, 1, storageformat.DirectoryRoot{
		SchemaVersion: 1, DirectoryID: directoryID, ManifestID: manifestID,
		RecursiveBytes: source.recursiveBytes, RecursiveFileCount: source.recursiveFileCount,
		ContentAccumulator: source.contentAccumulator, ContentDigest: source.contentDigest,
	})
	if err != nil {
		return preparedDirectory{}, err
	}
	if err := emit(storageformat.MutationObject{Key: rootKey.String(), Body: rootBody}); err != nil {
		return preparedDirectory{}, err
	}
	return preparedDirectory{
		manifestID: manifestID, recursiveBytes: source.recursiveBytes, recursiveFileCount: source.recursiveFileCount,
		contentAccumulator: source.contentAccumulator, contentDigest: source.contentDigest,
		contentSketch: append([]string(nil), contentRoot.Sketch...), rootBody: rootBody,
	}, nil
}
