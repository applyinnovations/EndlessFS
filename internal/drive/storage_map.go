package drive

import (
	"context"
	"errors"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

const (
	storageMapRootEntries            = 180
	storageMapExpandedDirectoryLimit = 8
	storageMapChildrenPerDirectory   = 60
	storageMapNodeLimit              = 420
)

// StorageMapEntry is one immediate child of the requested directory. Children
// are populated only for a bounded number of the largest directories and are
// verified against the version and recursive aggregates in the parent page.
type StorageMapEntry struct {
	domain.Entry
	Children             []domain.Entry `json:"children,omitempty"`
	RemainingMaximumSize *int64         `json:"remainingMaximumSize,omitempty"`
}

// StorageMapPage is a bounded two-level hierarchy for the owner storage map.
// It deliberately has no cursor: omitted bytes and files are represented by
// the exact recursive aggregates on Current and each directory entry.
type StorageMapPage struct {
	Current              domain.Entry      `json:"current"`
	Entries              []StorageMapEntry `json:"entries"`
	RemainingMaximumSize *int64            `json:"remainingMaximumSize,omitempty"`
}

func validStorageMapDirectory(entry domain.Entry) bool {
	return entry.Path.Valid() && entry.Kind == domain.EntryDirectory && entry.Size >= 0 && entry.FileCount >= 0 && entry.MediaType == "" && entry.Version != ""
}

func validStorageMapEntry(directory domain.UserPath, entry domain.Entry) bool {
	if !entry.Path.Valid() || entry.Path.Parent() != directory || entry.Name != entry.Path.Name() || entry.Size < 0 || entry.FileCount < 0 || entry.Version == "" {
		return false
	}
	if entry.Kind == domain.EntryDirectory {
		return entry.MediaType == ""
	}
	return entry.Kind == domain.EntryFile && entry.FileCount == 1
}

func matchingStorageMapSnapshot(expected, current domain.Entry) bool {
	return validStorageMapDirectory(current) &&
		current.Path == expected.Path &&
		current.Version == expected.Version &&
		current.Size == expected.Size &&
		current.FileCount == expected.FileCount
}

// StorageMap returns one bounded response instead of requiring the browser to
// crawl directories. A concurrently changed child is left unexpanded; other
// provider failures still fail closed.
func (s *Service) StorageMap(ctx context.Context, userID domain.UserID, directory domain.UserPath) (StorageMapPage, error) {
	scope, err := liveScope(userID)
	if err != nil {
		return StorageMapPage{}, err
	}
	root, err := s.storage.List(ctx, scope, domain.ListRequest{Directory: directory, PageSize: storageMapRootEntries + 1, Sort: domain.SortSize, Descending: true})
	if err != nil {
		return StorageMapPage{}, err
	}
	if !validStorageMapDirectory(root.Current) || root.Current.Path != directory || len(root.Entries) > storageMapRootEntries+1 {
		return StorageMapPage{}, domain.NewError(domain.ErrorInvalid, "storage map root is invalid")
	}
	for _, entry := range root.Entries {
		if !validStorageMapEntry(directory, entry) {
			return StorageMapPage{}, domain.NewError(domain.ErrorInvalid, "storage map entry is invalid")
		}
	}
	var rootRemainingMaximumSize *int64
	if len(root.Entries) > storageMapRootEntries {
		maximum := root.Entries[storageMapRootEntries].Size
		rootRemainingMaximumSize = &maximum
		root.Entries = root.Entries[:storageMapRootEntries]
	}

	result := StorageMapPage{Current: root.Current, Entries: make([]StorageMapEntry, len(root.Entries)), RemainingMaximumSize: rootRemainingMaximumSize}
	for index, entry := range root.Entries {
		result.Entries[index].Entry = entry
	}

	remainingNodes := storageMapNodeLimit - len(result.Entries)
	expandedDirectories := 0
	for index := range result.Entries {
		entry := result.Entries[index].Entry
		if remainingNodes == 0 || expandedDirectories == storageMapExpandedDirectoryLimit {
			break
		}
		if entry.Kind != domain.EntryDirectory || entry.Size == 0 || entry.FileCount == 0 {
			continue
		}
		expandedDirectories++
		childLimit := min(storageMapChildrenPerDirectory, remainingNodes)
		children, listErr := s.storage.List(ctx, scope, domain.ListRequest{Directory: entry.Path, PageSize: childLimit + 1, Sort: domain.SortSize, Descending: true})
		if listErr != nil {
			if errors.Is(listErr, domain.ErrNotFound) {
				continue
			}
			return StorageMapPage{}, listErr
		}
		if !matchingStorageMapSnapshot(entry, children.Current) {
			continue
		}
		if len(children.Entries) > childLimit+1 {
			return StorageMapPage{}, domain.NewError(domain.ErrorInvalid, "storage map child page is invalid")
		}
		for _, child := range children.Entries {
			if !validStorageMapEntry(entry.Path, child) {
				return StorageMapPage{}, domain.NewError(domain.ErrorInvalid, "storage map child entry is invalid")
			}
		}
		if len(children.Entries) > childLimit {
			maximum := children.Entries[childLimit].Size
			result.Entries[index].RemainingMaximumSize = &maximum
			children.Entries = children.Entries[:childLimit]
		}
		result.Entries[index].Children = children.Entries
		remainingNodes -= len(children.Entries)
	}
	return result, nil
}
