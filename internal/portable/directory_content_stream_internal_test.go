package portable

import (
	"fmt"
	"testing"

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
