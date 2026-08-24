package portable

import (
	"container/heap"
	"context"
	"math"
	"slices"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	directoryContentIndexNodeSchema = "directory-content-index-node-v1"
	directoryContentSketchSize      = 16
)

type directoryContentMergeSource struct {
	store       *FileStore
	ctx         context.Context
	scope       domain.Scope
	prefix      string
	directoryID string
	manifest    storageformat.DirectoryManifest
	direct      []storageformat.DirectoryContentIndexEntry
	page        []storageformat.DirectoryContentIndexEntry
	index       int
	after       string
	done        bool
}

func (source *directoryContentMergeSource) next() (storageformat.DirectoryContentIndexEntry, bool, error) {
	if len(source.direct) != 0 {
		if source.index == len(source.direct) {
			return storageformat.DirectoryContentIndexEntry{}, false, nil
		}
		value := source.direct[source.index]
		source.index++
		return value, true, nil
	}
	for source.index == len(source.page) {
		if source.done {
			return storageformat.DirectoryContentIndexEntry{}, false, nil
		}
		page, err := source.store.collectDirectoryContentIndexEntries(source.ctx, source.scope, source.directoryID, source.manifest, source.after, maxEntriesPerPage)
		if err != nil {
			return storageformat.DirectoryContentIndexEntry{}, false, err
		}
		source.page, source.index = page, 0
		if len(page) < maxEntriesPerPage {
			source.done = true
		}
		if len(page) == 0 {
			return storageformat.DirectoryContentIndexEntry{}, false, nil
		}
		source.after, _ = directoryContentIndexKey(page[len(page)-1])
	}
	value, err := prefixDirectoryContentIndexEntry(source.prefix, source.page[source.index])
	if err != nil {
		return storageformat.DirectoryContentIndexEntry{}, false, err
	}
	source.index++
	return value, true, nil
}

type directoryContentMergeItem struct {
	source int
	key    string
	value  storageformat.DirectoryContentIndexEntry
}

type directoryContentMergeHeap []directoryContentMergeItem

func (values directoryContentMergeHeap) Len() int           { return len(values) }
func (values directoryContentMergeHeap) Less(i, j int) bool { return values[i].key < values[j].key }
func (values directoryContentMergeHeap) Swap(i, j int)      { values[i], values[j] = values[j], values[i] }
func (values *directoryContentMergeHeap) Push(value any) {
	*values = append(*values, value.(directoryContentMergeItem))
}
func (values *directoryContentMergeHeap) Pop() any {
	prior := *values
	last := prior[len(prior)-1]
	*values = prior[:len(prior)-1]
	return last
}

func directoryContentIndexEntry(relativePath domain.UserPath, entry storageformat.DirectoryEntry) (storageformat.DirectoryContentIndexEntry, error) {
	if relativePath.IsRoot() || entry.Kind != domain.EntryFile {
		return storageformat.DirectoryContentIndexEntry{}, domain.NewError(domain.ErrorInvalid, "directory content index requires a relative file")
	}
	groupID, err := duplicateFileGroupID(entry)
	if err != nil {
		return storageformat.DirectoryContentIndexEntry{}, err
	}
	value := storageformat.DirectoryContentIndexEntry{GroupID: groupID, RelativePath: relativePath.String(), Size: entry.Size}
	_, err = directoryContentIndexKey(value)
	return value, err
}

func prefixDirectoryContentIndexEntry(name string, value storageformat.DirectoryContentIndexEntry) (storageformat.DirectoryContentIndexEntry, error) {
	path, err := domain.ParseUserPath(value.RelativePath)
	if err != nil || path.IsRoot() {
		return storageformat.DirectoryContentIndexEntry{}, domain.NewError(domain.ErrorInvalid, "invalid child directory content path")
	}
	prefixed, err := domain.MustParseUserPath("/").Join(name)
	if err != nil {
		return storageformat.DirectoryContentIndexEntry{}, err
	}
	for _, segment := range path.Segments() {
		prefixed, err = prefixed.Join(segment)
		if err != nil {
			return storageformat.DirectoryContentIndexEntry{}, err
		}
	}
	value.RelativePath = prefixed.String()
	_, err = directoryContentIndexKey(value)
	return value, err
}

// mergedDirectoryContentIndexEntries returns the parent directory's content
// entries in index-key order without collecting every descendant file. Direct
// files form one small sorted run and each child contributes bounded pages from
// its already-published immutable content tree.
func (s *FileStore) mergedDirectoryContentIndexEntries(ctx context.Context, scope domain.Scope, entries []storageformat.DirectoryEntry) (func() (storageformat.DirectoryContentIndexEntry, bool, error), error) {
	direct := make([]storageformat.DirectoryContentIndexEntry, 0, len(entries))
	sources := make([]*directoryContentMergeSource, 0, len(entries)+1)
	for _, entry := range entries {
		if entry.Kind == domain.EntryFile {
			path, err := domain.MustParseUserPath("/").Join(entry.Name)
			if err != nil {
				return nil, err
			}
			value, err := directoryContentIndexEntry(path, entry)
			if err != nil {
				return nil, err
			}
			direct = append(direct, value)
			continue
		}
		child, err := s.readDirectoryMetadata(ctx, scope, entry.DirectoryID, false)
		if err != nil {
			return nil, err
		}
		if child.pending || child.recursiveFileCount != entry.FileCount || child.recursiveBytes != entry.Size || child.contentDigest != entry.ContentDigest {
			return nil, domain.NewError(domain.ErrorInvalid, "child directory content index has stale aggregates")
		}
		if child.recursiveFileCount == 0 {
			continue
		}
		sources = append(sources, &directoryContentMergeSource{
			store: s, ctx: ctx, scope: scope, prefix: entry.Name,
			directoryID: entry.DirectoryID, manifest: child.manifest,
		})
	}
	if len(direct) != 0 {
		sort.Slice(direct, func(i, j int) bool {
			left, _ := directoryContentIndexKey(direct[i])
			right, _ := directoryContentIndexKey(direct[j])
			return left < right
		})
		sources = append(sources, &directoryContentMergeSource{direct: direct})
	}
	values := make(directoryContentMergeHeap, 0, len(sources))
	for index, source := range sources {
		value, ok, err := source.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		key, err := directoryContentIndexKey(value)
		if err != nil {
			return nil, err
		}
		values = append(values, directoryContentMergeItem{source: index, key: key, value: value})
	}
	heap.Init(&values)
	return func() (storageformat.DirectoryContentIndexEntry, bool, error) {
		if len(values) == 0 {
			return storageformat.DirectoryContentIndexEntry{}, false, nil
		}
		item := heap.Pop(&values).(directoryContentMergeItem)
		next, ok, err := sources[item.source].next()
		if err != nil {
			return storageformat.DirectoryContentIndexEntry{}, false, err
		}
		if ok {
			key, err := directoryContentIndexKey(next)
			if err != nil {
				return storageformat.DirectoryContentIndexEntry{}, false, err
			}
			heap.Push(&values, directoryContentMergeItem{source: item.source, key: key, value: next})
		}
		return item.value, true, nil
	}, nil
}

func directoryContentIndexKey(value storageformat.DirectoryContentIndexEntry) (string, error) {
	if err := validateDuplicateGroupID(value.GroupID); err != nil || value.Size < 0 {
		return "", domain.NewError(domain.ErrorInvalid, "invalid directory content-index entry")
	}
	path, err := domain.ParseUserPath(value.RelativePath)
	if err != nil || path.IsRoot() {
		return "", domain.NewError(domain.ErrorInvalid, "invalid directory content-index relative path")
	}
	return value.GroupID + "\x00" + path.String(), nil
}

func validateDirectoryContentIndexChild(child storageformat.DirectoryContentIndexChild) error {
	if child.NodeID == "" || child.NodeDigest == "" || child.FirstKey == "" || child.LastKey < child.FirstKey || child.EntryCount == 0 || validateDirectoryContentSketch(child.Sketch) != nil {
		return domain.NewError(domain.ErrorInvalid, "invalid directory content-index child")
	}
	return nil
}

func sameDirectoryContentIndexChild(left, right storageformat.DirectoryContentIndexChild) bool {
	return left.NodeID == right.NodeID && left.NodeDigest == right.NodeDigest && left.FirstKey == right.FirstKey && left.LastKey == right.LastKey && left.EntryCount == right.EntryCount && slices.Equal(left.Sketch, right.Sketch)
}

func validateDirectoryContentSketch(sketch []string) error {
	if len(sketch) != directoryContentSketchSize {
		return domain.NewError(domain.ErrorInvalid, "invalid directory content sketch")
	}
	for _, value := range sketch {
		if validateDuplicateGroupID(value) != nil {
			return domain.NewError(domain.ErrorInvalid, "invalid directory content sketch")
		}
	}
	return nil
}

func directoryContentSketch(entries []storageformat.DirectoryContentIndexEntry, children []storageformat.DirectoryContentIndexChild) ([]string, error) {
	result := make([]string, directoryContentSketchSize)
	merge := func(position int, value string) {
		if result[position] == "" || value < result[position] {
			result[position] = value
		}
	}
	for _, entry := range entries {
		if validateDuplicateGroupID(entry.GroupID) != nil {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid directory content sketch source")
		}
		for position := range directoryContentSketchSize {
			material := make([]byte, 0, len("endlessfs-directory-content-sketch-v1\x00")+2+len(entry.GroupID))
			material = append(material, "endlessfs-directory-content-sketch-v1\x00"...)
			material = append(material, byte(position), 0)
			material = append(material, entry.GroupID...)
			merge(position, storageformat.Digest(material))
		}
	}
	for _, child := range children {
		if err := validateDirectoryContentSketch(child.Sketch); err != nil {
			return nil, err
		}
		for position, value := range child.Sketch {
			merge(position, value)
		}
	}
	if len(entries)+len(children) == 0 || validateDirectoryContentSketch(result) != nil {
		return nil, domain.NewError(domain.ErrorInvalid, "empty directory content sketch source")
	}
	return result, nil
}

func validateDirectoryContentIndexNode(directoryID string, node storageformat.DirectoryContentIndexNode) error {
	if node.SchemaVersion != 1 || node.DirectoryID != directoryID || node.NodeID == "" || node.Leaf == (len(node.Entries) == 0) || node.Leaf && len(node.Children) != 0 || !node.Leaf && len(node.Entries) != 0 || !node.Leaf && len(node.Children) == 0 || len(node.Entries) > maxDirectoryIndexItems || len(node.Children) > maxDirectoryIndexItems {
		return domain.NewError(domain.ErrorInvalid, "invalid directory content-index node")
	}
	previous := ""
	if node.Leaf {
		for _, value := range node.Entries {
			key, err := directoryContentIndexKey(value)
			if err != nil || key <= previous {
				return domain.NewError(domain.ErrorInvalid, "directory content-index leaf is not uniquely ordered")
			}
			previous = key
		}
		return nil
	}
	for _, child := range node.Children {
		if err := validateDirectoryContentIndexChild(child); err != nil || child.FirstKey <= previous {
			return domain.NewError(domain.ErrorInvalid, "directory content-index branch is not uniquely ordered")
		}
		previous = child.LastKey
	}
	return nil
}

func directoryContentIndexNodeChild(node storageformat.DirectoryContentIndexNode, digest string) (storageformat.DirectoryContentIndexChild, error) {
	child := storageformat.DirectoryContentIndexChild{NodeID: node.NodeID, NodeDigest: digest}
	sketch, err := directoryContentSketch(node.Entries, node.Children)
	if err != nil {
		return storageformat.DirectoryContentIndexChild{}, err
	}
	child.Sketch = sketch
	if node.Leaf {
		if len(node.Entries) == 0 {
			return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "empty directory content-index leaf")
		}
		first, err := directoryContentIndexKey(node.Entries[0])
		if err != nil {
			return storageformat.DirectoryContentIndexChild{}, err
		}
		last, err := directoryContentIndexKey(node.Entries[len(node.Entries)-1])
		if err != nil {
			return storageformat.DirectoryContentIndexChild{}, err
		}
		child.FirstKey, child.LastKey, child.EntryCount = first, last, uint64(len(node.Entries))
		return child, nil
	}
	if len(node.Children) == 0 {
		return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "empty directory content-index branch")
	}
	child.FirstKey, child.LastKey = node.Children[0].FirstKey, node.Children[len(node.Children)-1].LastKey
	for _, nested := range node.Children {
		if nested.EntryCount > ^uint64(0)-child.EntryCount {
			return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "directory content-index count overflows")
		}
		child.EntryCount += nested.EntryCount
	}
	return child, nil
}

func (s *FileStore) makeDirectoryContentIndexNode(scope domain.Scope, directoryID string, leaf bool, entries []storageformat.DirectoryContentIndexEntry, children []storageformat.DirectoryContentIndexChild) (storageformat.DirectoryContentIndexChild, storageformat.MutationObject, storageformat.DirectoryContentIndexNode, error) {
	identity, err := storageformat.EncodeCanonical(struct {
		DirectoryID string                                     `json:"directoryID"`
		Leaf        bool                                       `json:"leaf"`
		Entries     []storageformat.DirectoryContentIndexEntry `json:"entries,omitempty"`
		Children    []storageformat.DirectoryContentIndexChild `json:"children,omitempty"`
	}{directoryID, leaf, entries, children})
	if err != nil {
		return storageformat.DirectoryContentIndexChild{}, storageformat.MutationObject{}, storageformat.DirectoryContentIndexNode{}, err
	}
	nodeID := storageformat.Digest(append([]byte("endlessfs-directory-content-index-node-v1\x00"), identity...))
	node := storageformat.DirectoryContentIndexNode{SchemaVersion: 1, DirectoryID: directoryID, NodeID: nodeID, Leaf: leaf, Entries: entries, Children: children}
	if err := validateDirectoryContentIndexNode(directoryID, node); err != nil {
		return storageformat.DirectoryContentIndexChild{}, storageformat.MutationObject{}, storageformat.DirectoryContentIndexNode{}, err
	}
	key := storageformat.DirectoryContentIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, nodeID)
	body, err := storageformat.EncodeEnvelope(directoryContentIndexNodeSchema, key, 1, node)
	if err != nil {
		return storageformat.DirectoryContentIndexChild{}, storageformat.MutationObject{}, storageformat.DirectoryContentIndexNode{}, err
	}
	child, err := directoryContentIndexNodeChild(node, storageformat.Digest(body))
	return child, storageformat.MutationObject{Key: key.String(), Body: body}, node, err
}

// buildDirectoryContentIndexStream bulk-builds an index from an already sorted
// source while retaining at most one leaf and one bounded child buffer per tree
// level. Every completed immutable node is handed to emit immediately, so a
// migration or durable preparation phase does not need a subtree-sized object
// or entry slice merely to publish the resulting root.
func (s *FileStore) buildDirectoryContentIndexStream(
	scope domain.Scope,
	directoryID string,
	next func() (storageformat.DirectoryContentIndexEntry, bool, error),
	emit func(storageformat.MutationObject) error,
) (storageformat.DirectoryContentIndexChild, error) {
	if !scope.Valid() || directoryID == "" || next == nil || emit == nil {
		return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "invalid streaming directory content-index builder")
	}
	levels := make([][]storageformat.DirectoryContentIndexChild, 1)
	var addChild func(int, storageformat.DirectoryContentIndexChild) error
	addChild = func(level int, child storageformat.DirectoryContentIndexChild) error {
		for len(levels) <= level {
			levels = append(levels, nil)
		}
		levels[level] = append(levels[level], child)
		if len(levels[level]) < maxDirectoryIndexItems {
			return nil
		}
		children := append([]storageformat.DirectoryContentIndexChild(nil), levels[level]...)
		levels[level] = levels[level][:0]
		parent, object, _, err := s.makeDirectoryContentIndexNode(scope, directoryID, false, nil, children)
		if err != nil {
			return err
		}
		if err := emit(object); err != nil {
			return err
		}
		return addChild(level+1, parent)
	}
	emitLeaf := func(entries []storageformat.DirectoryContentIndexEntry) error {
		leaf, object, _, err := s.makeDirectoryContentIndexNode(scope, directoryID, true, append([]storageformat.DirectoryContentIndexEntry(nil), entries...), nil)
		if err != nil {
			return err
		}
		if err := emit(object); err != nil {
			return err
		}
		return addChild(0, leaf)
	}

	leaf := make([]storageformat.DirectoryContentIndexEntry, 0, maxDirectoryIndexItems)
	previous := ""
	for {
		value, ok, err := next()
		if err != nil {
			return storageformat.DirectoryContentIndexChild{}, err
		}
		if !ok {
			break
		}
		key, err := directoryContentIndexKey(value)
		if err != nil {
			return storageformat.DirectoryContentIndexChild{}, err
		}
		if previous != "" && key <= previous {
			return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "streaming directory content index is not uniquely ordered")
		}
		previous = key
		leaf = append(leaf, value)
		if len(leaf) == maxDirectoryIndexItems {
			if err := emitLeaf(leaf); err != nil {
				return storageformat.DirectoryContentIndexChild{}, err
			}
			leaf = leaf[:0]
		}
	}
	if len(leaf) != 0 {
		if err := emitLeaf(leaf); err != nil {
			return storageformat.DirectoryContentIndexChild{}, err
		}
	}
	if previous == "" {
		return storageformat.DirectoryContentIndexChild{}, nil
	}
	for {
		lowest, highest := -1, -1
		for level, children := range levels {
			if len(children) == 0 {
				continue
			}
			if lowest == -1 {
				lowest = level
			}
			highest = level
		}
		if lowest == highest && len(levels[lowest]) == 1 {
			return levels[lowest][0], nil
		}
		children := append([]storageformat.DirectoryContentIndexChild(nil), levels[lowest]...)
		levels[lowest] = levels[lowest][:0]
		parent, object, _, err := s.makeDirectoryContentIndexNode(scope, directoryID, false, nil, children)
		if err != nil {
			return storageformat.DirectoryContentIndexChild{}, err
		}
		if err := emit(object); err != nil {
			return storageformat.DirectoryContentIndexChild{}, err
		}
		if err := addChild(lowest+1, parent); err != nil {
			return storageformat.DirectoryContentIndexChild{}, err
		}
	}
}

func directoryContentIndexManifestRoot(manifest storageformat.DirectoryManifest) (storageformat.DirectoryContentIndexChild, error) {
	if manifest.RecursiveFileCount < 0 {
		return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "negative directory recursive file count")
	}
	if manifest.RecursiveFileCount == 0 {
		if manifest.ContentIndexRootID != "" || manifest.ContentIndexRootDigest != "" || len(manifest.ContentSketch) != 0 {
			return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "empty directory has a content index")
		}
		return storageformat.DirectoryContentIndexChild{}, nil
	}
	if manifest.ContentIndexRootID == "" || manifest.ContentIndexRootDigest == "" || validateDirectoryContentSketch(manifest.ContentSketch) != nil {
		return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "directory content index is missing")
	}
	return storageformat.DirectoryContentIndexChild{NodeID: manifest.ContentIndexRootID, NodeDigest: manifest.ContentIndexRootDigest, Sketch: append([]string(nil), manifest.ContentSketch...)}, nil
}

func (s *FileStore) readDirectoryContentIndexNode(ctx context.Context, scope domain.Scope, directoryID string, reference storageformat.DirectoryContentIndexChild) (storageformat.DirectoryContentIndexNode, error) {
	if err := validateDirectoryContentIndexChild(reference); err != nil {
		return storageformat.DirectoryContentIndexNode{}, err
	}
	key := storageformat.DirectoryContentIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, reference.NodeID)
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return storageformat.DirectoryContentIndexNode{}, err
	}
	if storageformat.Digest(object.Body) != reference.NodeDigest {
		return storageformat.DirectoryContentIndexNode{}, domain.NewError(domain.ErrorInvalid, "directory content-index node digest mismatch")
	}
	var envelope storageformat.Envelope
	var node storageformat.DirectoryContentIndexNode
	if err := storageformat.DecodeEnvelope(object.Body, key, directoryContentIndexNodeSchema, &envelope, &node); err != nil {
		return storageformat.DirectoryContentIndexNode{}, err
	}
	if err := validateDirectoryContentIndexNode(directoryID, node); err != nil {
		return storageformat.DirectoryContentIndexNode{}, err
	}
	derived, err := directoryContentIndexNodeChild(node, reference.NodeDigest)
	if err != nil || !sameDirectoryContentIndexChild(derived, reference) {
		return storageformat.DirectoryContentIndexNode{}, domain.NewError(domain.ErrorInvalid, "directory content-index child metadata mismatch")
	}
	return node, nil
}

func (s *FileStore) directoryContentIndexRoot(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest) (storageformat.DirectoryContentIndexChild, error) {
	reference, err := directoryContentIndexManifestRoot(manifest)
	if err != nil || manifest.RecursiveFileCount == 0 {
		return reference, err
	}
	key := storageformat.DirectoryContentIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, reference.NodeID)
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return storageformat.DirectoryContentIndexChild{}, err
	}
	var envelope storageformat.Envelope
	var node storageformat.DirectoryContentIndexNode
	if storageformat.Digest(object.Body) != reference.NodeDigest || storageformat.DecodeEnvelope(object.Body, key, directoryContentIndexNodeSchema, &envelope, &node) != nil || validateDirectoryContentIndexNode(directoryID, node) != nil {
		return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "invalid directory content-index root")
	}
	derived, err := directoryContentIndexNodeChild(node, storageformat.Digest(object.Body))
	if err != nil || derived.NodeID != reference.NodeID || derived.NodeDigest != reference.NodeDigest || !slices.Equal(derived.Sketch, reference.Sketch) || derived.EntryCount != uint64(manifest.RecursiveFileCount) { // #nosec G115 -- directoryContentIndexManifestRoot rejects negative counts.
		return storageformat.DirectoryContentIndexChild{}, domain.NewError(domain.ErrorInvalid, "directory content-index root count mismatch")
	}
	return derived, nil
}

func (s *FileStore) collectDirectoryContentIndexEntries(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest, after string, limit int) ([]storageformat.DirectoryContentIndexEntry, error) {
	return s.collectDirectoryContentIndexEntriesAtDepth(ctx, scope, directoryID, manifest, after, limit, 0)
}

func (s *FileStore) collectDirectoryContentIndexEntriesAtDepth(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest, after string, limit, depth int) ([]storageformat.DirectoryContentIndexEntry, error) {
	if manifest.RecursiveFileCount == 0 {
		return nil, nil
	}
	if depth > 256 {
		return nil, domain.NewError(domain.ErrorInvalid, "directory content expression is too deep")
	}
	if manifest.SchemaVersion == 3 {
		next, err := s.lazyDirectoryContentIterator(ctx, scope.UserID(), manifest, depth+1)
		if err != nil {
			return nil, err
		}
		result := make([]storageformat.DirectoryContentIndexEntry, 0, limit)
		for len(result) < limit {
			value, ok, err := next()
			if err != nil {
				return nil, err
			}
			if !ok {
				break
			}
			key, err := directoryContentIndexKey(value)
			if err != nil {
				return nil, err
			}
			if after == "" || key > after {
				result = append(result, value)
			}
		}
		return result, nil
	}
	root, err := s.directoryContentIndexRoot(ctx, scope, directoryID, manifest)
	if err != nil {
		return nil, err
	}
	result := make([]storageformat.DirectoryContentIndexEntry, 0, limit)
	var walk func(storageformat.DirectoryContentIndexChild) error
	walk = func(reference storageformat.DirectoryContentIndexChild) error {
		if len(result) >= limit || after != "" && reference.LastKey <= after {
			return nil
		}
		node, err := s.readDirectoryContentIndexNode(ctx, scope, directoryID, reference)
		if err != nil {
			return err
		}
		if node.Leaf {
			for _, value := range node.Entries {
				key, _ := directoryContentIndexKey(value)
				if after == "" || key > after {
					result = append(result, value)
					if len(result) == limit {
						break
					}
				}
			}
			return nil
		}
		for _, child := range node.Children {
			if err := walk(child); err != nil {
				return err
			}
			if len(result) == limit {
				break
			}
		}
		return nil
	}
	return result, walk(root)
}

func (s *FileStore) lazyDirectoryContentIterator(ctx context.Context, userID domain.UserID, manifest storageformat.DirectoryManifest, depth int) (func() (storageformat.DirectoryContentIndexEntry, bool, error), error) {
	if !userID.Valid() || manifest.SchemaVersion != 3 || validateDirectoryManifestContent(manifest) != nil {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid lazy directory content iterator")
	}
	pageSource := func(scope domain.Scope, directoryID string, sourceManifest storageformat.DirectoryManifest) func() (storageformat.DirectoryContentIndexEntry, bool, error) {
		after := ""
		var page []storageformat.DirectoryContentIndexEntry
		index := 0
		done := false
		return func() (storageformat.DirectoryContentIndexEntry, bool, error) {
			for index == len(page) {
				if done {
					return storageformat.DirectoryContentIndexEntry{}, false, nil
				}
				var err error
				page, err = s.collectDirectoryContentIndexEntriesAtDepth(ctx, scope, directoryID, sourceManifest, after, maxEntriesPerPage, depth)
				if err != nil {
					return storageformat.DirectoryContentIndexEntry{}, false, err
				}
				index = 0
				if len(page) < maxEntriesPerPage {
					done = true
				}
				if len(page) == 0 {
					return storageformat.DirectoryContentIndexEntry{}, false, nil
				}
				after, err = directoryContentIndexKey(page[len(page)-1])
				if err != nil {
					return storageformat.DirectoryContentIndexEntry{}, false, err
				}
			}
			value := page[index]
			index++
			return value, true, nil
		}
	}
	var sources []directoryContentDeltaSource
	if manifest.ContentBase != nil {
		baseScope, err := storedOperationScope(userID, manifest.ContentBase.Area)
		if err != nil {
			return nil, err
		}
		baseManifest, err := s.readDirectoryManifest(ctx, baseScope, manifest.ContentBase.DirectoryID, manifest.ContentBase.ManifestID)
		if err != nil {
			return nil, err
		}
		sources = append(sources, directoryContentDeltaSource{base: true, next: pageSource(baseScope, manifest.ContentBase.DirectoryID, baseManifest)})
	}
	for _, delta := range manifest.ContentDeltas {
		if delta.Entry != nil {
			emitted := false
			value := *delta.Entry
			sources = append(sources, directoryContentDeltaSource{remove: delta.Remove, next: func() (storageformat.DirectoryContentIndexEntry, bool, error) {
				if emitted {
					return storageformat.DirectoryContentIndexEntry{}, false, nil
				}
				emitted = true
				return value, true, nil
			}})
			continue
		}
		sourceScope, err := storedOperationScope(userID, delta.Area)
		if err != nil {
			return nil, err
		}
		sourceManifest, err := s.readDirectoryManifest(ctx, sourceScope, delta.DirectoryID, delta.ManifestID)
		if err != nil {
			return nil, err
		}
		prefix, _ := domain.ParseUserPath(delta.Prefix)
		sources = append(sources, directoryContentDeltaSource{
			remove: delta.Remove, prefix: prefix.Segments(),
			next: pageSource(sourceScope, delta.DirectoryID, sourceManifest),
		})
	}
	values := make(directoryContentDeltaHeap, 0, len(sources))
	for index := range sources {
		value, ok, err := sources[index].advance()
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		key, err := directoryContentIndexKey(value)
		if err != nil {
			return nil, err
		}
		values = append(values, directoryContentDeltaHeapItem{source: index, key: key, value: value})
	}
	heap.Init(&values)
	return func() (storageformat.DirectoryContentIndexEntry, bool, error) {
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
					return storageformat.DirectoryContentIndexEntry{}, false, domain.NewError(domain.ErrorInvalid, "duplicate lazy directory content delta key")
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
				return storageformat.DirectoryContentIndexEntry{}, false, domain.NewError(domain.ErrorInvalid, "lazy directory content removal does not match snapshot")
			}
			if removal != nil {
				base = nil
			}
			if addition != nil {
				if base != nil && !sameDirectoryContentIndexEntry(*base, *addition) {
					return storageformat.DirectoryContentIndexEntry{}, false, domain.NewError(domain.ErrorInvalid, "lazy directory content addition collides with snapshot")
				}
				base = addition
			}
			if base != nil {
				return *base, true, nil
			}
		}
		return storageformat.DirectoryContentIndexEntry{}, false, nil
	}, nil
}

func (s *FileStore) verifyDirectoryContentIndex(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest) error {
	after := ""
	var files, bytes int64
	for {
		page, err := s.collectDirectoryContentIndexEntries(ctx, scope, directoryID, manifest, after, maxEntriesPerPage)
		if err != nil {
			return err
		}
		for _, value := range page {
			if files == math.MaxInt64 || value.Size > math.MaxInt64-bytes {
				return domain.NewError(domain.ErrorInvalid, "directory content-index aggregates overflow")
			}
			entry, err := s.resolveDirectoryContentIndexEntry(ctx, scope, directoryID, manifest, value.RelativePath)
			if err != nil {
				return err
			}
			groupID, err := duplicateFileGroupID(entry)
			if err != nil || groupID != value.GroupID || entry.Size != value.Size {
				return domain.NewError(domain.ErrorInvalid, "directory content index disagrees with the filesystem")
			}
			files++
			bytes += value.Size
		}
		if len(page) < maxEntriesPerPage {
			break
		}
		after, _ = directoryContentIndexKey(page[len(page)-1])
	}
	if files != manifest.RecursiveFileCount || bytes != manifest.RecursiveBytes {
		return domain.NewError(domain.ErrorInvalid, "directory content index disagrees with recursive aggregates")
	}
	return nil
}

func (s *FileStore) resolveDirectoryContentIndexEntry(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest, relativePath string) (storageformat.DirectoryEntry, error) {
	path, err := domain.ParseUserPath(relativePath)
	if err != nil || path.IsRoot() {
		return storageformat.DirectoryEntry{}, domain.NewError(domain.ErrorInvalid, "invalid directory content-index path")
	}
	currentID := directoryID
	currentManifest := manifest
	storageScope := scope
	for index, segment := range path.Segments() {
		entry, err := s.directoryIndexEntry(ctx, storageScope, currentID, currentManifest, segment)
		if err != nil {
			return storageformat.DirectoryEntry{}, err
		}
		if index == len(path.Segments())-1 {
			if entry.Kind != domain.EntryFile {
				return storageformat.DirectoryEntry{}, domain.NewError(domain.ErrorInvalid, "directory content-index path is not a file")
			}
			return entry, nil
		}
		if entry.Kind != domain.EntryDirectory || entry.DirectoryID == "" {
			return storageformat.DirectoryEntry{}, domain.NewError(domain.ErrorInvalid, "directory content-index path crosses a file")
		}
		storageScope, err = directoryEntryStorageScope(storageScope, entry)
		if err != nil {
			return storageformat.DirectoryEntry{}, err
		}
		child, err := s.readDirectoryEntryMetadata(ctx, storageScope, entry)
		if err != nil {
			return storageformat.DirectoryEntry{}, err
		}
		currentID, currentManifest = entry.DirectoryID, child.manifest
	}
	return storageformat.DirectoryEntry{}, domain.NewError(domain.ErrorInvalid, "directory content-index path is empty")
}
