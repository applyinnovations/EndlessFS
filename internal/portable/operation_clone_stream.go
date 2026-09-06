package portable

import (
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

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
