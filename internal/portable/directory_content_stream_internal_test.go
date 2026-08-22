package portable

import (
	"fmt"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestDirectoryContentIndexStreamingBuilderEmitsBoundedNodes(t *testing.T) {
	user, err := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := domain.NewScope(user, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	const count = maxDirectoryIndexItems*maxDirectoryIndexItems + 1
	groupID := storageformat.Digest([]byte("streaming-content-group"))
	nextIndex := 0
	next := func() (storageformat.DirectoryContentIndexEntry, bool, error) {
		if nextIndex == count {
			return storageformat.DirectoryContentIndexEntry{}, false, nil
		}
		value := storageformat.DirectoryContentIndexEntry{
			GroupID: groupID, RelativePath: fmt.Sprintf("/%08d", nextIndex), Size: 1,
		}
		nextIndex++
		return value, true, nil
	}
	emitted := 0
	root, err := (&FileStore{}).buildDirectoryContentIndexStream(scope, "stream-directory", next, func(object storageformat.MutationObject) error {
		emitted++
		if object.Key == "" || len(object.Body) == 0 || len(object.Body) > storageformat.MaxCanonicalBytes {
			t.Fatalf("invalid streamed node: key=%q bytes=%d", object.Key, len(object.Body))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if root.EntryCount != count || emitted <= count/maxDirectoryIndexItems {
		t.Fatalf("streamed root/count = %d, emitted=%d; want %d entries and branch nodes", root.EntryCount, emitted, count)
	}
}

func TestDirectoryEntryIndexesStreamingBuildersEmitBoundedNodes(t *testing.T) {
	user, err := domain.ParseUserID("WVhXWVhXWVhXWVhXWVhXWQ")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := domain.NewScope(user, domain.AreaLive)
	if err != nil {
		t.Fatal(err)
	}
	const count = maxDirectoryIndexItems*maxDirectoryIndexItems + 1
	now := time.Date(2047, 2, 3, 4, 5, 6, 0, time.UTC)
	entryAt := func(index int) storageformat.DirectoryEntry {
		name := fmt.Sprintf("file-%08d", index)
		return withCurrentTestFingerprint(storageformat.DirectoryEntry{
			Name: name, NameDigest: storageformat.NameDigest(name), Kind: domain.EntryFile,
			BlobID: fmt.Sprintf("blob-%08d", index), Size: int64(index), MediaType: "application/octet-stream", ModifiedAt: now,
		})
	}
	for name, build := range map[string]func(func(storageformat.MutationObject) error) (uint64, error){
		"name": func(emit func(storageformat.MutationObject) error) (uint64, error) {
			index := 0
			root, err := (&FileStore{}).buildDirectoryIndexStream(scope, "stream-directory", func() (storageformat.DirectoryEntry, bool, error) {
				if index == count {
					return storageformat.DirectoryEntry{}, false, nil
				}
				value := entryAt(index)
				index++
				return value, true, nil
			}, emit)
			return root.EntryCount, err
		},
		"size": func(emit func(storageformat.MutationObject) error) (uint64, error) {
			index := 0
			root, err := (&FileStore{}).buildDirectorySortIndexStream(scope, "stream-directory", domain.SortSize, func() (storageformat.DirectoryEntry, bool, error) {
				if index == count {
					return storageformat.DirectoryEntry{}, false, nil
				}
				value := entryAt(index)
				index++
				return value, true, nil
			}, emit)
			return root.EntryCount, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			emitted := 0
			entries, err := build(func(object storageformat.MutationObject) error {
				emitted++
				if object.Key == "" || len(object.Body) == 0 || len(object.Body) > storageformat.MaxCanonicalBytes {
					t.Fatalf("invalid streamed node: key=%q bytes=%d", object.Key, len(object.Body))
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if entries != count || emitted <= count/maxDirectoryIndexItems {
				t.Fatalf("streamed root/count = %d, emitted=%d; want %d entries and branch nodes", entries, emitted, count)
			}
		})
	}
}
