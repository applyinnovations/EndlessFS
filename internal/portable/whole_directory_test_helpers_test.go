package portable

// This file preserves whole-snapshot constructors used by corruption and
// structural-sharing tests. They are deliberately excluded from production:
// runtime path resolution and mutation must use the bounded persistent indexes.

import (
	"context"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func listAllFrom(ctx context.Context, backend objectstore.ListBackend, prefix string) ([]objectstore.ObjectInfo, error) {
	request := objectstore.ListRequest{Prefix: prefix, Limit: 1000}
	var result []objectstore.ObjectInfo
	for {
		page, err := backend.List(ctx, request)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Objects...)
		if page.NextCursor == "" {
			return result, nil
		}
		request.Cursor = page.NextCursor
	}
}

func (s *FileStore) resolveDirectory(ctx context.Context, scope domain.Scope, path domain.UserPath) (string, directorySnapshot, error) {
	if path.IsRoot() {
		snapshot, err := s.readDirectory(ctx, scope, storageformat.RootDirectoryID, true)
		return storageformat.RootDirectoryID, snapshot, err
	}
	entry, err := s.resolveEntry(ctx, scope, path)
	if err != nil {
		return "", directorySnapshot{}, err
	}
	if entry.Kind != domain.EntryDirectory || entry.DirectoryID == "" {
		return "", directorySnapshot{}, domain.NewError(domain.ErrorNotFound, "directory not found")
	}
	snapshot, err := s.readDirectory(ctx, scope, entry.DirectoryID, false)
	if err == nil && (snapshot.recursiveBytes != entry.Size || snapshot.recursiveFileCount != entry.FileCount || snapshot.contentDigest != entry.ContentDigest) {
		return "", directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "directory recursive aggregate mismatch")
	}
	return entry.DirectoryID, snapshot, err
}

func (s *FileStore) resolveDirectoryTrail(ctx context.Context, scope domain.Scope, path domain.UserPath) ([]directoryTrailNode, error) {
	root, err := s.readDirectory(ctx, scope, storageformat.RootDirectoryID, true)
	if err != nil {
		return nil, err
	}
	trail := []directoryTrailNode{{scope: scope, path: domain.MustParseUserPath("/"), directoryID: storageformat.RootDirectoryID, snapshot: root}}
	current := root
	currentPath := domain.MustParseUserPath("/")
	for _, segment := range path.Segments() {
		entry, found := findDirectoryEntry(current.entries, segment)
		if !found || entry.Kind != domain.EntryDirectory || entry.DirectoryID == "" {
			return nil, domain.NewError(domain.ErrorNotFound, "directory not found")
		}
		currentPath, err = currentPath.Join(segment)
		if err != nil {
			return nil, err
		}
		current, err = s.readDirectory(ctx, scope, entry.DirectoryID, false)
		if err != nil {
			return nil, err
		}
		if current.recursiveBytes != entry.Size || current.recursiveFileCount != entry.FileCount || current.contentDigest != entry.ContentDigest {
			return nil, s.classifyDirectoryTrailMismatch(ctx, trail[len(trail)-1], entry, current)
		}
		trail = append(trail, directoryTrailNode{scope: scope, path: currentPath, directoryID: entry.DirectoryID, entry: entry, snapshot: current})
	}
	return trail, nil
}

func (s *FileStore) readDirectory(ctx context.Context, scope domain.Scope, directoryID string, allowVirtualRoot bool) (directorySnapshot, error) {
	snapshot, err := s.readDirectoryMetadata(ctx, scope, directoryID, allowVirtualRoot)
	if err != nil || snapshot.manifestID == "" {
		return snapshot, err
	}
	entries, err := s.readManifestPageEntries(ctx, scope, directoryID, snapshot.manifest)
	if err != nil {
		return directorySnapshot{}, err
	}
	computedBytes, err := recursiveByteSize(entries)
	if err != nil || computedBytes != snapshot.recursiveBytes {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "directory manifest entries recursive byte aggregate mismatch")
	}
	computedFiles, err := recursiveFileCount(entries)
	if err != nil || computedFiles != snapshot.recursiveFileCount {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "directory manifest entries recursive file count mismatch")
	}
	computedAccumulator, computedDigest, err := directoryContentIdentity(entries)
	if err != nil || computedAccumulator != snapshot.contentAccumulator || computedDigest != snapshot.contentDigest {
		return directorySnapshot{}, domain.NewError(domain.ErrorInvalid, "directory manifest entries content digest mismatch")
	}
	snapshot.entries = entries
	return snapshot, nil
}

func (s *FileStore) readManifestSnapshot(ctx context.Context, scope domain.Scope, directoryID, manifestID string) (storageformat.DirectoryManifest, []storageformat.DirectoryEntry, error) {
	manifest, err := s.readDirectoryManifest(ctx, scope, directoryID, manifestID)
	if err != nil {
		return storageformat.DirectoryManifest{}, nil, err
	}
	entries, err := s.readManifestPageEntries(ctx, scope, directoryID, manifest)
	if err != nil {
		return storageformat.DirectoryManifest{}, nil, err
	}
	computedBytes, err := recursiveByteSize(entries)
	if err != nil || computedBytes != manifest.RecursiveBytes {
		return storageformat.DirectoryManifest{}, nil, domain.NewError(domain.ErrorInvalid, "directory manifest entries recursive byte aggregate mismatch")
	}
	computedFiles, err := recursiveFileCount(entries)
	if err != nil || computedFiles != manifest.RecursiveFileCount {
		return storageformat.DirectoryManifest{}, nil, domain.NewError(domain.ErrorInvalid, "directory manifest entries recursive file count mismatch")
	}
	computedAccumulator, computedDigest, err := directoryContentIdentity(entries)
	if err != nil || computedAccumulator != manifest.ContentAccumulator || computedDigest != manifest.ContentDigest {
		return storageformat.DirectoryManifest{}, nil, domain.NewError(domain.ErrorInvalid, "directory manifest content digest mismatch")
	}
	return manifest, entries, nil
}

func (s *FileStore) prepareDirectoryUpdate(ctx context.Context, scope domain.Scope, directoryID string, snapshot directorySnapshot, entries []storageformat.DirectoryEntry, revision uint64) (preparedDirectory, error) {
	if err := validateDirectoryEntries(entries); err != nil {
		return preparedDirectory{}, err
	}
	if snapshot.manifestID == "" && len(snapshot.entries) == 0 {
		return s.prepareDirectory(ctx, scope, directoryID, entries, revision)
	}
	if !snapshot.exists || snapshot.manifest.SchemaVersion != 2 || snapshot.manifest.EntryCount != len(snapshot.entries) {
		return preparedDirectory{}, domain.NewError(domain.ErrorInvalid, "directory update has no current index snapshot")
	}
	indexRoot, nodes, err := s.mutateDirectoryIndex(ctx, scope, directoryID, snapshot.manifest, snapshot.entries, entries)
	if err != nil {
		return preparedDirectory{}, err
	}
	names, replacements := directoryEntryChanges(snapshot.entries, entries)
	changes := make(map[string]directoryEntryMutation, len(names))
	old := make(map[string]storageformat.DirectoryEntry, len(snapshot.entries))
	for _, entry := range snapshot.entries {
		old[entry.Name] = entry
	}
	for _, name := range names {
		change := directoryEntryMutation{after: replacements[name]}
		if entry, found := old[name]; found {
			before := entry
			change.before = &before
		}
		changes[name] = change
	}
	sortRoots, sortNodes, err := s.mutateDirectorySortIndexes(ctx, scope, directoryID, snapshot.manifest, changes, len(entries))
	if err != nil {
		return preparedDirectory{}, err
	}
	nodes = append(nodes, sortNodes...)
	contentEntries, err := s.directoryContentIndexEntries(ctx, scope, entries)
	if err != nil {
		return preparedDirectory{}, err
	}
	contentRoot, contentNodes, err := s.buildDirectoryContentIndex(scope, directoryID, contentEntries)
	if err != nil {
		return preparedDirectory{}, err
	}
	nodes = append(nodes, contentNodes...)
	contentAccumulator, contentDigest, err := updateDirectoryContentIdentity(snapshot.contentAccumulator, snapshot.entries, entries)
	if err != nil {
		return preparedDirectory{}, err
	}
	return s.prepareDirectoryWithIndex(scope, directoryID, entries, revision, indexRoot, sortRoots, contentRoot, nodes, contentAccumulator, contentDigest)
}

func updateDirectoryContentIdentity(encoded string, before, after []storageformat.DirectoryEntry) (string, string, error) {
	return updateDirectoryContentIdentityAtCount(encoded, before, after, len(after))
}

func applyDirectoryChange(updates map[string]directoryUpdate, trail []directoryTrailNode, entries []storageformat.DirectoryEntry) error {
	if len(trail) == 0 {
		return domain.NewError(domain.ErrorInvalid, "directory aggregate trail is empty")
	}
	leaf := trail[len(trail)-1]
	before := append([]storageformat.DirectoryEntry(nil), leaf.snapshot.entries...)
	names, replacements := directoryEntryChanges(before, entries)
	old := make(map[string]storageformat.DirectoryEntry, len(before))
	for _, entry := range before {
		old[entry.Name] = entry
	}
	for _, name := range names {
		var beforeEntry *storageformat.DirectoryEntry
		if value, ok := old[name]; ok {
			copy := value
			beforeEntry = &copy
		}
		if err := applyDirectoryEntryChange(updates, trail, beforeEntry, replacements[name]); err != nil {
			return err
		}
	}
	return nil
}

func directoryEntryChanges(before, after []storageformat.DirectoryEntry) ([]string, map[string]*storageformat.DirectoryEntry) {
	old := append([]storageformat.DirectoryEntry(nil), before...)
	current := append([]storageformat.DirectoryEntry(nil), after...)
	sort.Slice(old, func(left, right int) bool { return old[left].Name < old[right].Name })
	sort.Slice(current, func(left, right int) bool { return current[left].Name < current[right].Name })
	changes := make(map[string]*storageformat.DirectoryEntry)
	var names []string
	for left, right := 0, 0; left < len(old) || right < len(current); {
		switch {
		case right == len(current) || left < len(old) && old[left].Name < current[right].Name:
			names = append(names, old[left].Name)
			changes[old[left].Name] = nil
			left++
		case left == len(old) || current[right].Name < old[left].Name:
			entry := current[right]
			names = append(names, entry.Name)
			changes[entry.Name] = &entry
			right++
		default:
			if old[left] != current[right] {
				entry := current[right]
				names = append(names, entry.Name)
				changes[entry.Name] = &entry
			}
			left++
			right++
		}
	}
	return names, changes
}

func (s *FileStore) mutateDirectoryIndex(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest, before, after []storageformat.DirectoryEntry) (storageformat.DirectoryIndexChild, []storageformat.MutationObject, error) {
	names, replacements := directoryEntryChanges(before, after)
	changes := make(map[string]directoryEntryMutation, len(names))
	old := make(map[string]storageformat.DirectoryEntry, len(before))
	for _, entry := range before {
		old[entry.Name] = entry
	}
	for _, name := range names {
		change := directoryEntryMutation{after: replacements[name]}
		if entry, ok := old[name]; ok {
			entryCopy := entry
			change.before = &entryCopy
		}
		changes[name] = change
	}
	return s.mutateDirectoryIndexChanges(ctx, scope, directoryID, manifest, changes)
}
