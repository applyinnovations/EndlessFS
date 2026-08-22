package portable

import (
	"container/heap"
	"context"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// directoryContentDelta describes one already-published directory content
// index being added to or removed from an ancestor. Keeping the source as a
// manifest pin lets operation preparation merge bounded index pages instead of
// materializing every descendant file.
type directoryContentDelta struct {
	scope       domain.Scope
	directoryID string
	manifest    storageformat.DirectoryManifest
	prefix      []string
	remove      bool
}

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

// rebuildDirectoryContentIndexWithDeltas performs a fixed-fan-in merge of the
// current immutable index and the added/removed subtree indexes. Memory is
// bounded by one provider page per source plus the streaming tree builder.
func (s *FileStore) rebuildDirectoryContentIndexWithDeltas(
	ctx context.Context,
	update directoryUpdate,
	deltas []directoryContentDelta,
	emit func(storageformat.MutationObject) error,
) (storageformat.DirectoryContentIndexChild, error) {
	if !update.scope.Valid() || update.directoryID == "" || update.recursiveFileCount < 0 || emit == nil {
		return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "invalid directory content delta merge")
	}
	sources := []directoryContentDeltaSource{{
		next: s.directoryContentIterator(ctx, update.scope, update.directoryID, update.snapshot.manifest),
		base: true,
	}}
	for _, delta := range deltas {
		if !delta.scope.Valid() || delta.directoryID == "" {
			return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "invalid directory content delta source")
		}
		sources = append(sources, directoryContentDeltaSource{
			next:   s.directoryContentIterator(ctx, delta.scope, delta.directoryID, delta.manifest),
			prefix: append([]string(nil), delta.prefix...), remove: delta.remove,
		})
	}
	values := make(directoryContentDeltaHeap, 0, len(sources))
	for index := range sources {
		value, ok, err := sources[index].advance()
		if err != nil {
			return storageformat.DirectoryContentIndexChild{}, err
		}
		if !ok {
			continue
		}
		key, err := directoryContentIndexKey(value)
		if err != nil {
			return storageformat.DirectoryContentIndexChild{}, err
		}
		values = append(values, directoryContentDeltaHeapItem{source: index, key: key, value: value})
	}
	heap.Init(&values)
	var next func() (storageformat.DirectoryContentIndexEntry, bool, error)
	next = func() (storageformat.DirectoryContentIndexEntry, bool, error) {
		for len(values) != 0 {
			key := values[0].key
			var base, removal, addition *storageformat.DirectoryContentIndexEntry
			for len(values) != 0 && values[0].key == key {
				item := heap.Pop(&values).(directoryContentDeltaHeapItem)
				source := sources[item.source]
				target := &addition
				if source.base {
					target = &base
				} else if source.remove {
					target = &removal
				}
				if *target != nil {
					return storageformat.DirectoryContentIndexEntry{}, false, domain.NewError(domain.ErrorInvalid, "duplicate directory content delta key")
				}
				value := item.value
				*target = &value
				following, ok, err := source.advance()
				if err != nil {
					return storageformat.DirectoryContentIndexEntry{}, false, err
				}
				if ok {
					followingKey, err := directoryContentIndexKey(following)
					if err != nil {
						return storageformat.DirectoryContentIndexEntry{}, false, err
					}
					heap.Push(&values, directoryContentDeltaHeapItem{source: item.source, key: followingKey, value: following})
				}
			}
			if removal != nil && (base == nil || !sameDirectoryContentIndexEntry(*base, *removal)) {
				return storageformat.DirectoryContentIndexEntry{}, false, domain.NewError(domain.ErrorInvalid, "directory content delta removal does not match snapshot")
			}
			if removal != nil {
				base = nil
			}
			if addition != nil {
				if base != nil && !sameDirectoryContentIndexEntry(*base, *addition) {
					return storageformat.DirectoryContentIndexEntry{}, false, domain.NewError(domain.ErrorInvalid, "directory content delta addition collides with snapshot")
				}
				base = addition
			}
			if base != nil {
				return *base, true, nil
			}
		}
		return storageformat.DirectoryContentIndexEntry{}, false, nil
	}
	root, err := s.buildDirectoryContentIndexStream(update.scope, update.directoryID, next, emit)
	if err != nil {
		return storageformat.DirectoryContentIndexChild{}, err
	}
	if root.EntryCount != uint64(update.recursiveFileCount) { // #nosec G115 -- negative update counts are rejected above.
		return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "directory content delta merge count mismatch")
	}
	if update.recursiveFileCount == 0 && (root.NodeID != "" || root.NodeDigest != "" || len(root.Sketch) != 0) {
		return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "non-empty directory content delta merge result")
	}
	return root, nil
}
