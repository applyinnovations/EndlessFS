package portable

// This file preserves whole-snapshot constructors used by corruption and
// structural-sharing tests. They are deliberately excluded from production:
// runtime path resolution and mutation must use the bounded persistent indexes.

import "github.com/applyinnovations/endlessfs/internal/storageformat"

func removeDirectoryEntry(entries []storageformat.DirectoryEntry, name string) []storageformat.DirectoryEntry {
	result := make([]storageformat.DirectoryEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Name != name {
			result = append(result, entry)
		}
	}
	return result
}

func updateDirectoryContentIdentity(encoded string, before, after []storageformat.DirectoryEntry) (string, string, error) {
	return updateDirectoryContentIdentityAtCount(encoded, before, after, len(after))
}
