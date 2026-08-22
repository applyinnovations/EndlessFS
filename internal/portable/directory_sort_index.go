package portable

import (
	"context"
	"fmt"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const directorySortIndexNodeSchema = "directory-sort-index-node-v1"

var directorySecondarySorts = []domain.SortField{domain.SortKind, domain.SortModified, domain.SortSize}

func directorySortKey(field domain.SortField, entry storageformat.DirectoryEntry) (string, error) {
	if err := validateDirectoryIndexEntry(entry); err != nil {
		return "", err
	}
	primary := ""
	switch field {
	case domain.SortKind:
		primary = string(entry.Kind)
	case domain.SortModified:
		primary = entry.ModifiedAt.UTC().Format("20060102T150405.000000000Z")
	case domain.SortSize:
		primary = fmt.Sprintf("%016x", uint64(entry.Size))
	default:
		return "", domain.NewError(domain.ErrorInvalid, "invalid directory secondary sort")
	}
	return primary + "\x00" + entry.Name, nil
}

func validateDirectorySortIndexChild(child storageformat.DirectorySortIndexChild) error {
	if child.NodeID == "" || child.NodeDigest == "" || child.FirstKey == "" || child.LastKey < child.FirstKey || child.EntryCount == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid directory sort-index child")
	}
	return nil
}

func validateDirectorySortIndexNode(directoryID string, field domain.SortField, node storageformat.DirectorySortIndexNode) error {
	if node.SchemaVersion != 1 || node.DirectoryID != directoryID || node.Sort != field || node.NodeID == "" || node.Leaf == (len(node.Entries) == 0) || node.Leaf && len(node.Children) != 0 || !node.Leaf && len(node.Entries) != 0 || !node.Leaf && len(node.Children) == 0 || len(node.Entries) > maxDirectoryIndexItems || len(node.Children) > maxDirectoryIndexItems {
		return domain.NewError(domain.ErrorInvalid, "invalid directory sort-index node")
	}
	previous := ""
	if node.Leaf {
		for _, value := range node.Entries {
			key, err := directorySortKey(field, value.Entry)
			if err != nil || value.SortKey != key || value.SortKey <= previous {
				return domain.NewError(domain.ErrorInvalid, "directory sort-index leaf is not uniquely ordered")
			}
			previous = value.SortKey
		}
		return nil
	}
	for _, child := range node.Children {
		if err := validateDirectorySortIndexChild(child); err != nil || child.FirstKey <= previous {
			return domain.NewError(domain.ErrorInvalid, "directory sort-index branch is not uniquely ordered")
		}
		previous = child.LastKey
	}
	return nil
}

func directorySortIndexNodeChild(node storageformat.DirectorySortIndexNode, digest string) (storageformat.DirectorySortIndexChild, error) {
	child := storageformat.DirectorySortIndexChild{NodeID: node.NodeID, NodeDigest: digest}
	if node.Leaf {
		if len(node.Entries) == 0 {
			return storageformat.DirectorySortIndexChild{}, domain.NewError(domain.ErrorInvalid, "empty directory sort-index leaf")
		}
		child.FirstKey, child.LastKey, child.EntryCount = node.Entries[0].SortKey, node.Entries[len(node.Entries)-1].SortKey, uint64(len(node.Entries))
		return child, nil
	}
	if len(node.Children) == 0 {
		return storageformat.DirectorySortIndexChild{}, domain.NewError(domain.ErrorInvalid, "empty directory sort-index branch")
	}
	child.FirstKey, child.LastKey = node.Children[0].FirstKey, node.Children[len(node.Children)-1].LastKey
	for _, nested := range node.Children {
		if nested.EntryCount > ^uint64(0)-child.EntryCount {
			return storageformat.DirectorySortIndexChild{}, domain.NewError(domain.ErrorInvalid, "directory sort-index count overflows")
		}
		child.EntryCount += nested.EntryCount
	}
	return child, nil
}

func (s *FileStore) makeDirectorySortIndexNode(scope domain.Scope, directoryID string, field domain.SortField, leaf bool, entries []storageformat.DirectorySortIndexEntry, children []storageformat.DirectorySortIndexChild) (storageformat.DirectorySortIndexChild, storageformat.MutationObject, storageformat.DirectorySortIndexNode, error) {
	identity, err := storageformat.EncodeCanonical(struct {
		DirectoryID string                                  `json:"directoryID"`
		Sort        domain.SortField                        `json:"sort"`
		Leaf        bool                                    `json:"leaf"`
		Entries     []storageformat.DirectorySortIndexEntry `json:"entries,omitempty"`
		Children    []storageformat.DirectorySortIndexChild `json:"children,omitempty"`
	}{directoryID, field, leaf, entries, children})
	if err != nil {
		return storageformat.DirectorySortIndexChild{}, storageformat.MutationObject{}, storageformat.DirectorySortIndexNode{}, err
	}
	nodeID := storageformat.Digest(append([]byte("endlessfs-directory-sort-index-node-v1\x00"), identity...))
	node := storageformat.DirectorySortIndexNode{SchemaVersion: 1, DirectoryID: directoryID, Sort: field, NodeID: nodeID, Leaf: leaf, Entries: entries, Children: children}
	if err := validateDirectorySortIndexNode(directoryID, field, node); err != nil {
		return storageformat.DirectorySortIndexChild{}, storageformat.MutationObject{}, storageformat.DirectorySortIndexNode{}, err
	}
	key := storageformat.DirectorySortIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, field, nodeID)
	body, err := storageformat.EncodeEnvelope(directorySortIndexNodeSchema, key, 1, node)
	if err != nil {
		return storageformat.DirectorySortIndexChild{}, storageformat.MutationObject{}, storageformat.DirectorySortIndexNode{}, err
	}
	child, err := directorySortIndexNodeChild(node, storageformat.Digest(body))
	return child, storageformat.MutationObject{Key: key.String(), Body: body}, node, err
}

func (s *FileStore) buildDirectorySortIndexes(scope domain.Scope, directoryID string, source []storageformat.DirectoryEntry) ([]storageformat.DirectorySortIndexRoot, []storageformat.MutationObject, error) {
	if len(source) == 0 {
		return nil, nil, nil
	}
	var roots []storageformat.DirectorySortIndexRoot
	var objects []storageformat.MutationObject
	for _, field := range directorySecondarySorts {
		entries := make([]storageformat.DirectorySortIndexEntry, 0, len(source))
		for _, entry := range source {
			key, err := directorySortKey(field, entry)
			if err != nil {
				return nil, nil, err
			}
			entries = append(entries, storageformat.DirectorySortIndexEntry{SortKey: key, Entry: entry})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].SortKey < entries[j].SortKey })
		var references []storageformat.DirectorySortIndexChild
		for start := 0; start < len(entries); start += maxDirectoryIndexItems {
			end := min(start+maxDirectoryIndexItems, len(entries))
			reference, object, _, err := s.makeDirectorySortIndexNode(scope, directoryID, field, true, append([]storageformat.DirectorySortIndexEntry(nil), entries[start:end]...), nil)
			if err != nil {
				return nil, nil, err
			}
			references, objects = append(references, reference), append(objects, object)
		}
		for len(references) > 1 {
			var next []storageformat.DirectorySortIndexChild
			for start := 0; start < len(references); start += maxDirectoryIndexItems {
				end := min(start+maxDirectoryIndexItems, len(references))
				reference, object, _, err := s.makeDirectorySortIndexNode(scope, directoryID, field, false, nil, append([]storageformat.DirectorySortIndexChild(nil), references[start:end]...))
				if err != nil {
					return nil, nil, err
				}
				next, objects = append(next, reference), append(objects, object)
			}
			references = next
		}
		roots = append(roots, storageformat.DirectorySortIndexRoot{Sort: field, NodeID: references[0].NodeID, NodeDigest: references[0].NodeDigest})
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return roots, objects, nil
}

func validateDirectorySortIndexRoots(roots []storageformat.DirectorySortIndexRoot, entryCount int) error {
	if entryCount == 0 {
		if len(roots) != 0 {
			return domain.NewError(domain.ErrorInvalid, "empty directory has secondary sort indexes")
		}
		return nil
	}
	if len(roots) != len(directorySecondarySorts) {
		return domain.NewError(domain.ErrorInvalid, "directory secondary sort indexes are incomplete")
	}
	for index, field := range directorySecondarySorts {
		if roots[index].Sort != field || roots[index].NodeID == "" || roots[index].NodeDigest == "" {
			return domain.NewError(domain.ErrorInvalid, "invalid directory secondary sort root")
		}
	}
	return nil
}

func directorySortIndexManifestRoot(manifest storageformat.DirectoryManifest, field domain.SortField) (storageformat.DirectorySortIndexRoot, error) {
	if err := validateDirectorySortIndexRoots(manifest.SortIndexes, manifest.EntryCount); err != nil {
		return storageformat.DirectorySortIndexRoot{}, err
	}
	for _, root := range manifest.SortIndexes {
		if root.Sort == field {
			return root, nil
		}
	}
	return storageformat.DirectorySortIndexRoot{}, domain.NewError(domain.ErrorInvalid, "directory secondary sort root is missing")
}

func (s *FileStore) readDirectorySortIndexNode(ctx context.Context, scope domain.Scope, directoryID string, field domain.SortField, reference storageformat.DirectorySortIndexChild) (storageformat.DirectorySortIndexNode, error) {
	if err := validateDirectorySortIndexChild(reference); err != nil {
		return storageformat.DirectorySortIndexNode{}, err
	}
	key := storageformat.DirectorySortIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, field, reference.NodeID)
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return storageformat.DirectorySortIndexNode{}, err
	}
	if storageformat.Digest(object.Body) != reference.NodeDigest {
		return storageformat.DirectorySortIndexNode{}, domain.NewError(domain.ErrorInvalid, "directory sort-index node digest mismatch")
	}
	var envelope storageformat.Envelope
	var node storageformat.DirectorySortIndexNode
	if err := storageformat.DecodeEnvelope(object.Body, key, directorySortIndexNodeSchema, &envelope, &node); err != nil {
		return storageformat.DirectorySortIndexNode{}, err
	}
	if err := validateDirectorySortIndexNode(directoryID, field, node); err != nil {
		return storageformat.DirectorySortIndexNode{}, err
	}
	derived, err := directorySortIndexNodeChild(node, reference.NodeDigest)
	if err != nil || derived != reference {
		return storageformat.DirectorySortIndexNode{}, domain.NewError(domain.ErrorInvalid, "directory sort-index child metadata mismatch")
	}
	return node, nil
}

func (s *FileStore) directorySortIndexRoot(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest, field domain.SortField) (storageformat.DirectorySortIndexChild, error) {
	root, err := directorySortIndexManifestRoot(manifest, field)
	if err != nil {
		return storageformat.DirectorySortIndexChild{}, err
	}
	reference := storageformat.DirectorySortIndexChild{NodeID: root.NodeID, NodeDigest: root.NodeDigest}
	key := storageformat.DirectorySortIndexNodeKey(scope.UserID().String(), areaName(scope.Area()), directoryID, field, root.NodeID)
	object, err := s.engine.backend.Get(ctx, key)
	if err != nil {
		return storageformat.DirectorySortIndexChild{}, err
	}
	var envelope storageformat.Envelope
	var node storageformat.DirectorySortIndexNode
	if storageformat.Digest(object.Body) != root.NodeDigest || storageformat.DecodeEnvelope(object.Body, key, directorySortIndexNodeSchema, &envelope, &node) != nil {
		return storageformat.DirectorySortIndexChild{}, domain.NewError(domain.ErrorInvalid, "invalid directory sort-index root")
	}
	reference, err = directorySortIndexNodeChild(node, root.NodeDigest)
	if err != nil || reference.EntryCount != uint64(manifest.EntryCount) || validateDirectorySortIndexNode(directoryID, field, node) != nil {
		return storageformat.DirectorySortIndexChild{}, domain.NewError(domain.ErrorInvalid, "directory sort-index root count mismatch")
	}
	return reference, nil
}

func (s *FileStore) collectDirectorySortIndexEntries(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest, field domain.SortField, after string, descending bool, limit int) ([]storageformat.DirectoryEntry, error) {
	if manifest.EntryCount == 0 {
		return nil, nil
	}
	root, err := s.directorySortIndexRoot(ctx, scope, directoryID, manifest, field)
	if err != nil {
		return nil, err
	}
	result := make([]storageformat.DirectoryEntry, 0, limit)
	var walk func(storageformat.DirectorySortIndexChild) error
	walk = func(reference storageformat.DirectorySortIndexChild) error {
		if len(result) >= limit || !descending && after != "" && reference.LastKey <= after || descending && after != "" && reference.FirstKey >= after {
			return nil
		}
		node, err := s.readDirectorySortIndexNode(ctx, scope, directoryID, field, reference)
		if err != nil {
			return err
		}
		if node.Leaf {
			if descending {
				for index := len(node.Entries) - 1; index >= 0 && len(result) < limit; index-- {
					if after == "" || node.Entries[index].SortKey < after {
						result = append(result, node.Entries[index].Entry)
					}
				}
				return nil
			}
			for _, value := range node.Entries {
				if after == "" || value.SortKey > after {
					result = append(result, value.Entry)
					if len(result) == limit {
						break
					}
				}
			}
			return nil
		}
		if descending {
			for index := len(node.Children) - 1; index >= 0 && len(result) < limit; index-- {
				if err := walk(node.Children[index]); err != nil {
					return err
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

type stagedDirectorySortIndex struct {
	objects map[string]storageformat.MutationObject
	nodes   map[string]storageformat.DirectorySortIndexNode
}

func newStagedDirectorySortIndex() *stagedDirectorySortIndex {
	return &stagedDirectorySortIndex{objects: make(map[string]storageformat.MutationObject), nodes: make(map[string]storageformat.DirectorySortIndexNode)}
}

func (s *FileStore) readStagedDirectorySortIndexNode(ctx context.Context, scope domain.Scope, directoryID string, field domain.SortField, reference storageformat.DirectorySortIndexChild, staged *stagedDirectorySortIndex) (storageformat.DirectorySortIndexNode, error) {
	if node, found := staged.nodes[reference.NodeID]; found {
		object := staged.objects[reference.NodeID]
		derived, err := directorySortIndexNodeChild(node, storageformat.Digest(object.Body))
		if err != nil || derived != reference {
			return storageformat.DirectorySortIndexNode{}, domain.NewError(domain.ErrorInvalid, "staged directory sort-index metadata mismatch")
		}
		return node, nil
	}
	return s.readDirectorySortIndexNode(ctx, scope, directoryID, field, reference)
}

func (s *FileStore) stageDirectorySortIndexNode(scope domain.Scope, directoryID string, field domain.SortField, leaf bool, entries []storageformat.DirectorySortIndexEntry, children []storageformat.DirectorySortIndexChild, staged *stagedDirectorySortIndex) (storageformat.DirectorySortIndexChild, error) {
	reference, object, node, err := s.makeDirectorySortIndexNode(scope, directoryID, field, leaf, entries, children)
	if err != nil {
		return storageformat.DirectorySortIndexChild{}, err
	}
	staged.objects[reference.NodeID], staged.nodes[reference.NodeID] = object, node
	return reference, nil
}

func (s *FileStore) splitDirectorySortIndexNode(scope domain.Scope, directoryID string, field domain.SortField, leaf bool, entries []storageformat.DirectorySortIndexEntry, children []storageformat.DirectorySortIndexChild, staged *stagedDirectorySortIndex) ([]storageformat.DirectorySortIndexChild, error) {
	length := len(children)
	if leaf {
		length = len(entries)
	}
	cut := length
	if length > maxDirectoryIndexItems {
		cut = length / 2
	}
	parts := [][2]int{{0, cut}}
	if cut < length {
		parts = append(parts, [2]int{cut, length})
	}
	result := make([]storageformat.DirectorySortIndexChild, 0, len(parts))
	for _, part := range parts {
		var partEntries []storageformat.DirectorySortIndexEntry
		var partChildren []storageformat.DirectorySortIndexChild
		if leaf {
			partEntries = append([]storageformat.DirectorySortIndexEntry(nil), entries[part[0]:part[1]]...)
		} else {
			partChildren = append([]storageformat.DirectorySortIndexChild(nil), children[part[0]:part[1]]...)
		}
		reference, err := s.stageDirectorySortIndexNode(scope, directoryID, field, leaf, partEntries, partChildren, staged)
		if err != nil {
			return nil, err
		}
		result = append(result, reference)
	}
	return result, nil
}

func (s *FileStore) mutateDirectorySortIndexNode(ctx context.Context, scope domain.Scope, directoryID string, field domain.SortField, reference *storageformat.DirectorySortIndexChild, key string, replacement *storageformat.DirectorySortIndexEntry, staged *stagedDirectorySortIndex) ([]storageformat.DirectorySortIndexChild, error) {
	if reference == nil {
		if replacement == nil {
			return nil, domain.NewError(domain.ErrorNotFound, "directory sort-index entry not found")
		}
		return s.splitDirectorySortIndexNode(scope, directoryID, field, true, []storageformat.DirectorySortIndexEntry{*replacement}, nil, staged)
	}
	node, err := s.readStagedDirectorySortIndexNode(ctx, scope, directoryID, field, *reference, staged)
	if err != nil {
		return nil, err
	}
	if node.Leaf {
		entries := append([]storageformat.DirectorySortIndexEntry(nil), node.Entries...)
		index := sort.Search(len(entries), func(index int) bool { return entries[index].SortKey >= key })
		found := index < len(entries) && entries[index].SortKey == key
		switch {
		case replacement == nil && !found:
			return nil, domain.NewError(domain.ErrorNotFound, "directory sort-index entry not found")
		case replacement == nil:
			entries = append(entries[:index], entries[index+1:]...)
		case found:
			entries[index] = *replacement
		default:
			entries = append(entries, storageformat.DirectorySortIndexEntry{})
			copy(entries[index+1:], entries[index:])
			entries[index] = *replacement
		}
		if len(entries) == 0 {
			return nil, nil
		}
		return s.splitDirectorySortIndexNode(scope, directoryID, field, true, entries, nil, staged)
	}
	children := append([]storageformat.DirectorySortIndexChild(nil), node.Children...)
	index := sort.Search(len(children), func(index int) bool { return children[index].LastKey >= key })
	if index == len(children) {
		index--
	}
	replacements, err := s.mutateDirectorySortIndexNode(ctx, scope, directoryID, field, &children[index], key, replacement, staged)
	if err != nil {
		return nil, err
	}
	updated := make([]storageformat.DirectorySortIndexChild, 0, len(children)-1+len(replacements))
	updated = append(updated, children[:index]...)
	updated = append(updated, replacements...)
	updated = append(updated, children[index+1:]...)
	if len(updated) == 0 {
		return nil, nil
	}
	return s.splitDirectorySortIndexNode(scope, directoryID, field, false, nil, updated, staged)
}

func (s *FileStore) mutateDirectorySortIndexes(ctx context.Context, scope domain.Scope, directoryID string, manifest storageformat.DirectoryManifest, changes map[string]directoryEntryMutation, expectedCount int) ([]storageformat.DirectorySortIndexRoot, []storageformat.MutationObject, error) {
	if expectedCount == 0 && manifest.EntryCount == 0 {
		return nil, nil, nil
	}
	var roots []storageformat.DirectorySortIndexRoot
	var objects []storageformat.MutationObject
	for _, field := range directorySecondarySorts {
		var root *storageformat.DirectorySortIndexChild
		if manifest.EntryCount > 0 {
			reference, err := s.directorySortIndexRoot(ctx, scope, directoryID, manifest, field)
			if err != nil {
				return nil, nil, err
			}
			root = &reference
		}
		staged := newStagedDirectorySortIndex()
		names := make([]string, 0, len(changes))
		for name := range changes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			change := changes[name]
			beforeKey, afterKey := "", ""
			var replacement *storageformat.DirectorySortIndexEntry
			if change.before != nil {
				var err error
				beforeKey, err = directorySortKey(field, *change.before)
				if err != nil {
					return nil, nil, err
				}
			}
			if change.after != nil {
				var err error
				afterKey, err = directorySortKey(field, *change.after)
				if err != nil {
					return nil, nil, err
				}
				value := storageformat.DirectorySortIndexEntry{SortKey: afterKey, Entry: *change.after}
				replacement = &value
			}
			if beforeKey != "" && beforeKey != afterKey {
				references, err := s.mutateDirectorySortIndexNode(ctx, scope, directoryID, field, root, beforeKey, nil, staged)
				if err != nil {
					return nil, nil, err
				}
				root = singleDirectorySortRoot(references)
			}
			if afterKey != "" {
				references, err := s.mutateDirectorySortIndexNode(ctx, scope, directoryID, field, root, afterKey, replacement, staged)
				if err != nil {
					return nil, nil, err
				}
				for len(references) > 1 {
					references, err = s.splitDirectorySortIndexNode(scope, directoryID, field, false, nil, references, staged)
					if err != nil {
						return nil, nil, err
					}
				}
				root = singleDirectorySortRoot(references)
			}
		}
		if expectedCount == 0 {
			if root != nil {
				return nil, nil, domain.NewError(domain.ErrorInvalid, "non-empty directory sort index after removing every entry")
			}
			continue
		}
		if root == nil || root.EntryCount != uint64(expectedCount) {
			return nil, nil, domain.NewError(domain.ErrorInvalid, "directory sort-index mutation count mismatch")
		}
		roots = append(roots, storageformat.DirectorySortIndexRoot{Sort: field, NodeID: root.NodeID, NodeDigest: root.NodeDigest})
		reachable := make(map[string]struct{})
		var visit func(storageformat.DirectorySortIndexChild)
		visit = func(reference storageformat.DirectorySortIndexChild) {
			if _, found := reachable[reference.NodeID]; found {
				return
			}
			reachable[reference.NodeID] = struct{}{}
			if node, found := staged.nodes[reference.NodeID]; found {
				objects = append(objects, staged.objects[reference.NodeID])
				for _, child := range node.Children {
					visit(child)
				}
			}
		}
		visit(*root)
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return roots, objects, nil
}

func singleDirectorySortRoot(references []storageformat.DirectorySortIndexChild) *storageformat.DirectorySortIndexChild {
	if len(references) != 1 {
		return nil
	}
	root := references[0]
	return &root
}
