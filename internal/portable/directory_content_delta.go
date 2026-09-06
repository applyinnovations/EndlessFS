package portable

import (
	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

type directoryContentDeltaSource struct {
	next   func() (storageformat.DirectoryContentIndexEntry, bool, error)
	prefix []string
	remove bool
	base   bool
}

func prefixDirectoryContentIndexPath(prefix []string, value storageformat.DirectoryContentIndexEntry) (storageformat.DirectoryContentIndexEntry, error) {
	path, err := domain.ParseUserPath(value.RelativePath)
	if err != nil || path.IsRoot() {
		return storageformat.DirectoryContentIndexEntry{}, domain.NewError(domain.ErrorInvalid, "invalid directory content delta path")
	}
	result := domain.MustParseUserPath("/")
	for _, segment := range prefix {
		result, err = result.Join(segment)
		if err != nil {
			return storageformat.DirectoryContentIndexEntry{}, err
		}
	}
	for _, segment := range path.Segments() {
		result, err = result.Join(segment)
		if err != nil {
			return storageformat.DirectoryContentIndexEntry{}, err
		}
	}
	value.RelativePath = result.String()
	if _, err := directoryContentIndexKey(value); err != nil {
		return storageformat.DirectoryContentIndexEntry{}, err
	}
	return value, nil
}

func (source directoryContentDeltaSource) advance() (storageformat.DirectoryContentIndexEntry, bool, error) {
	value, ok, err := source.next()
	if err != nil || !ok || len(source.prefix) == 0 {
		return value, ok, err
	}
	value, err = prefixDirectoryContentIndexPath(source.prefix, value)
	return value, err == nil, err
}

type directoryContentDeltaHeapItem struct {
	source int
	key    string
	value  storageformat.DirectoryContentIndexEntry
}

type directoryContentDeltaHeap []directoryContentDeltaHeapItem

func (values directoryContentDeltaHeap) Len() int           { return len(values) }
func (values directoryContentDeltaHeap) Less(i, j int) bool { return values[i].key < values[j].key }
func (values directoryContentDeltaHeap) Swap(i, j int)      { values[i], values[j] = values[j], values[i] }
func (values *directoryContentDeltaHeap) Push(value any) {
	*values = append(*values, value.(directoryContentDeltaHeapItem))
}
func (values *directoryContentDeltaHeap) Pop() any {
	prior := *values
	last := prior[len(prior)-1]
	*values = prior[:len(prior)-1]
	return last
}

func sameDirectoryContentIndexEntry(left, right storageformat.DirectoryContentIndexEntry) bool {
	return left.GroupID == right.GroupID && left.RelativePath == right.RelativePath && left.Size == right.Size
}
