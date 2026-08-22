package portable

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const (
	stateIndexRootSchema = "state-index-root-v1"
	stateIndexNodeSchema = "state-index-node-v1"
	maxStateIndexItems   = 64
)

type stateIndexRootSnapshot struct {
	object   objectstore.Object
	envelope storageformat.Envelope
	root     storageformat.StateIndexRoot
	exists   bool
}

type preparedStateIndex struct {
	snapshot      stateIndexRootSnapshot
	root          storageformat.StateIndexRoot
	rootBody      []byte
	prerequisites []storageformat.MutationObject
}

func stateNamespace(key state.Key) string { return strings.SplitN(key.String(), "/", 2)[0] }

func (e *Engine) readStateIndexRoot(ctx context.Context, namespace string) (stateIndexRootSnapshot, error) {
	key := storageformat.StateIndexRootKey(namespace)
	object, err := e.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return stateIndexRootSnapshot{root: storageformat.StateIndexRoot{SchemaVersion: 1, Namespace: namespace}}, nil
	}
	if err != nil {
		return stateIndexRootSnapshot{}, err
	}
	var envelope storageformat.Envelope
	var root storageformat.StateIndexRoot
	if err := storageformat.DecodeEnvelope(object.Body, key, stateIndexRootSchema, &envelope, &root); err != nil {
		return stateIndexRootSnapshot{}, err
	}
	if root.SchemaVersion != 1 || root.Namespace != namespace || root.EntryCount == 0 && (root.NodeID != "" || root.NodeDigest != "") || root.EntryCount != 0 && (root.NodeID == "" || root.NodeDigest == "") {
		return stateIndexRootSnapshot{}, domain.NewError(domain.ErrorInvalid, "invalid state index root")
	}
	return stateIndexRootSnapshot{object: object, envelope: envelope, root: root, exists: true}, nil
}

func (e *Engine) readStateIndexNode(ctx context.Context, namespace string, reference storageformat.StateIndexChild) (storageformat.StateIndexNode, error) {
	if err := validateStateIndexChild(reference); err != nil {
		return storageformat.StateIndexNode{}, err
	}
	key := storageformat.StateIndexNodeKey(namespace, reference.NodeID)
	object, err := e.backend.Get(ctx, key)
	if err != nil {
		return storageformat.StateIndexNode{}, err
	}
	if storageformat.Digest(object.Body) != reference.NodeDigest {
		return storageformat.StateIndexNode{}, domain.NewError(domain.ErrorInvalid, "state index node digest mismatch")
	}
	var envelope storageformat.Envelope
	var node storageformat.StateIndexNode
	if err := storageformat.DecodeEnvelope(object.Body, key, stateIndexNodeSchema, &envelope, &node); err != nil {
		return storageformat.StateIndexNode{}, err
	}
	if err := validateStateIndexNode(namespace, node); err != nil {
		return storageformat.StateIndexNode{}, err
	}
	derived, err := stateIndexNodeChild(node, reference.NodeDigest)
	if err != nil || derived != reference {
		return storageformat.StateIndexNode{}, domain.NewError(domain.ErrorInvalid, "state index child metadata mismatch")
	}
	return node, nil
}

func validateStateIndexChild(child storageformat.StateIndexChild) error {
	if child.NodeID == "" || child.NodeDigest == "" || child.FirstKey == "" || child.LastKey < child.FirstKey || child.EntryCount == 0 {
		return domain.NewError(domain.ErrorInvalid, "invalid state index child")
	}
	return nil
}

func validateStateIndexNode(namespace string, node storageformat.StateIndexNode) error {
	if node.SchemaVersion != 1 || node.Namespace != namespace || node.NodeID == "" || node.Leaf == (len(node.Entries) == 0) || node.Leaf && len(node.Children) != 0 || !node.Leaf && len(node.Entries) != 0 || len(node.Entries) > maxStateIndexItems || len(node.Children) > maxStateIndexItems {
		return domain.NewError(domain.ErrorInvalid, "invalid state index node")
	}
	if node.Leaf {
		previous := ""
		for _, entry := range node.Entries {
			logical, err := parseExistingStateKey(entry.LogicalKey)
			if err != nil || stateNamespace(logical) != namespace || entry.LogicalKey <= previous || entry.LogicalVersion == "" {
				return domain.NewError(domain.ErrorInvalid, "invalid state index leaf")
			}
			previous = entry.LogicalKey
		}
		return nil
	}
	previous := ""
	for _, child := range node.Children {
		if err := validateStateIndexChild(child); err != nil || child.FirstKey <= previous {
			return domain.NewError(domain.ErrorInvalid, "invalid state index branch")
		}
		previous = child.LastKey
	}
	return nil
}

func stateIndexNodeChild(node storageformat.StateIndexNode, digest string) (storageformat.StateIndexChild, error) {
	child := storageformat.StateIndexChild{NodeID: node.NodeID, NodeDigest: digest}
	if node.Leaf {
		if len(node.Entries) == 0 {
			return storageformat.StateIndexChild{}, domain.NewError(domain.ErrorInvalid, "empty state index leaf")
		}
		child.FirstKey = node.Entries[0].LogicalKey
		child.LastKey = node.Entries[len(node.Entries)-1].LogicalKey
		child.EntryCount = uint64(len(node.Entries))
		return child, nil
	}
	if len(node.Children) == 0 {
		return storageformat.StateIndexChild{}, domain.NewError(domain.ErrorInvalid, "empty state index branch")
	}
	child.FirstKey = node.Children[0].FirstKey
	child.LastKey = node.Children[len(node.Children)-1].LastKey
	for _, nested := range node.Children {
		if nested.EntryCount > math.MaxUint64-child.EntryCount {
			return storageformat.StateIndexChild{}, domain.NewError(domain.ErrorInvalid, "state index entry count overflows")
		}
		child.EntryCount += nested.EntryCount
	}
	return child, nil
}

func (e *Engine) makeStateIndexNode(namespace string, leaf bool, entries []storageformat.StateIndexEntry, children []storageformat.StateIndexChild) (storageformat.StateIndexChild, storageformat.MutationObject, error) {
	identity, err := storageformat.EncodeCanonical(struct {
		Namespace string                          `json:"namespace"`
		Leaf      bool                            `json:"leaf"`
		Entries   []storageformat.StateIndexEntry `json:"entries,omitempty"`
		Children  []storageformat.StateIndexChild `json:"children,omitempty"`
	}{namespace, leaf, entries, children})
	if err != nil {
		return storageformat.StateIndexChild{}, storageformat.MutationObject{}, err
	}
	nodeID := storageformat.Digest(append([]byte("endlessfs-state-index-node-v1\x00"), identity...))
	node := storageformat.StateIndexNode{SchemaVersion: 1, Namespace: namespace, NodeID: nodeID, Leaf: leaf, Entries: entries, Children: children}
	if err := validateStateIndexNode(namespace, node); err != nil {
		return storageformat.StateIndexChild{}, storageformat.MutationObject{}, err
	}
	key := storageformat.StateIndexNodeKey(namespace, nodeID)
	body, err := storageformat.EncodeEnvelope(stateIndexNodeSchema, key, 1, node)
	if err != nil {
		return storageformat.StateIndexChild{}, storageformat.MutationObject{}, err
	}
	digest := storageformat.Digest(body)
	child, err := stateIndexNodeChild(node, digest)
	return child, storageformat.MutationObject{Key: key.String(), Body: body}, err
}

func (e *Engine) mutateStateIndexNode(ctx context.Context, namespace string, reference *storageformat.StateIndexChild, logicalKey, logicalVersion string, remove bool) ([]storageformat.StateIndexChild, []storageformat.MutationObject, error) {
	if reference == nil {
		if remove {
			return nil, nil, domain.NewError(domain.ErrorNotFound, "state key not found")
		}
		child, object, err := e.makeStateIndexNode(namespace, true, []storageformat.StateIndexEntry{{LogicalKey: logicalKey, LogicalVersion: logicalVersion}}, nil)
		return []storageformat.StateIndexChild{child}, []storageformat.MutationObject{object}, err
	}
	node, err := e.readStateIndexNode(ctx, namespace, *reference)
	if err != nil {
		return nil, nil, err
	}
	var objects []storageformat.MutationObject
	if node.Leaf {
		entries := append([]storageformat.StateIndexEntry(nil), node.Entries...)
		index := sort.Search(len(entries), func(index int) bool { return entries[index].LogicalKey >= logicalKey })
		found := index < len(entries) && entries[index].LogicalKey == logicalKey
		if remove {
			if !found {
				return nil, nil, domain.NewError(domain.ErrorNotFound, "state key not found")
			}
			entries = append(entries[:index], entries[index+1:]...)
		} else if found {
			entries[index].LogicalVersion = logicalVersion
		} else {
			entries = append(entries, storageformat.StateIndexEntry{})
			copy(entries[index+1:], entries[index:])
			entries[index] = storageformat.StateIndexEntry{LogicalKey: logicalKey, LogicalVersion: logicalVersion}
		}
		if len(entries) == 0 {
			return nil, nil, nil
		}
		return e.splitStateIndexNode(namespace, true, entries, nil)
	}
	children := append([]storageformat.StateIndexChild(nil), node.Children...)
	index := sort.Search(len(children), func(index int) bool { return children[index].LastKey >= logicalKey })
	if index == len(children) {
		index = len(children) - 1
	}
	replacements, nested, err := e.mutateStateIndexNode(ctx, namespace, &children[index], logicalKey, logicalVersion, remove)
	if err != nil {
		return nil, nil, err
	}
	objects = append(objects, nested...)
	updated := make([]storageformat.StateIndexChild, 0, len(children)-1+len(replacements))
	updated = append(updated, children[:index]...)
	updated = append(updated, replacements...)
	updated = append(updated, children[index+1:]...)
	if len(updated) == 0 {
		return nil, objects, nil
	}
	refs, branchObjects, err := e.splitStateIndexNode(namespace, false, nil, updated)
	objects = append(objects, branchObjects...)
	return refs, objects, err
}

func (e *Engine) splitStateIndexNode(namespace string, leaf bool, entries []storageformat.StateIndexEntry, children []storageformat.StateIndexChild) ([]storageformat.StateIndexChild, []storageformat.MutationObject, error) {
	length := len(children)
	if leaf {
		length = len(entries)
	}
	cut := length
	if length > maxStateIndexItems {
		cut = length / 2
	}
	parts := [][2]int{{0, cut}}
	if cut < length {
		parts = append(parts, [2]int{cut, length})
	}
	refs := make([]storageformat.StateIndexChild, 0, len(parts))
	objects := make([]storageformat.MutationObject, 0, len(parts))
	for _, part := range parts {
		var partEntries []storageformat.StateIndexEntry
		var partChildren []storageformat.StateIndexChild
		if leaf {
			partEntries = append([]storageformat.StateIndexEntry(nil), entries[part[0]:part[1]]...)
		} else {
			partChildren = append([]storageformat.StateIndexChild(nil), children[part[0]:part[1]]...)
		}
		ref, object, err := e.makeStateIndexNode(namespace, leaf, partEntries, partChildren)
		if err != nil {
			return nil, nil, err
		}
		refs, objects = append(refs, ref), append(objects, object)
	}
	return refs, objects, nil
}

func (e *Engine) prepareStateIndexMutation(ctx context.Context, logical state.Key, logicalVersion string, remove bool) (preparedStateIndex, error) {
	namespace := stateNamespace(logical)
	snapshot, err := e.readStateIndexRoot(ctx, namespace)
	if err != nil {
		return preparedStateIndex{}, err
	}
	var reference *storageformat.StateIndexChild
	if snapshot.root.EntryCount != 0 {
		reference = &storageformat.StateIndexChild{NodeID: snapshot.root.NodeID, NodeDigest: snapshot.root.NodeDigest, FirstKey: "placeholder", LastKey: "placeholder", EntryCount: snapshot.root.EntryCount}
		// The root deliberately stores only a constant-size node identity. Read
		// the node to derive and authenticate its range metadata.
		key := storageformat.StateIndexNodeKey(namespace, reference.NodeID)
		object, getErr := e.backend.Get(ctx, key)
		if getErr != nil {
			return preparedStateIndex{}, getErr
		}
		if storageformat.Digest(object.Body) != reference.NodeDigest {
			return preparedStateIndex{}, domain.NewError(domain.ErrorInvalid, "state index root node digest mismatch")
		}
		var envelope storageformat.Envelope
		var node storageformat.StateIndexNode
		if err := storageformat.DecodeEnvelope(object.Body, key, stateIndexNodeSchema, &envelope, &node); err != nil {
			return preparedStateIndex{}, err
		}
		if err := validateStateIndexNode(namespace, node); err != nil {
			return preparedStateIndex{}, err
		}
		derived, err := stateIndexNodeChild(node, reference.NodeDigest)
		if err != nil || derived.EntryCount != snapshot.root.EntryCount {
			return preparedStateIndex{}, domain.NewError(domain.ErrorInvalid, "state index root count mismatch")
		}
		reference = &derived
	}
	refs, prerequisites, err := e.mutateStateIndexNode(ctx, namespace, reference, logical.String(), logicalVersion, remove)
	if err != nil {
		return preparedStateIndex{}, err
	}
	for len(refs) > 1 {
		var objects []storageformat.MutationObject
		refs, objects, err = e.splitStateIndexNode(namespace, false, nil, refs)
		if err != nil {
			return preparedStateIndex{}, err
		}
		prerequisites = append(prerequisites, objects...)
	}
	root := storageformat.StateIndexRoot{SchemaVersion: 1, Namespace: namespace}
	if len(refs) == 1 {
		root.NodeID, root.NodeDigest, root.EntryCount = refs[0].NodeID, refs[0].NodeDigest, refs[0].EntryCount
	}
	key := storageformat.StateIndexRootKey(namespace)
	revision := uint64(1)
	if snapshot.exists {
		revision = snapshot.envelope.Revision + 1
	}
	body, err := storageformat.EncodeEnvelope(stateIndexRootSchema, key, revision, root)
	if err != nil {
		return preparedStateIndex{}, err
	}
	prerequisites, err = normalizeMutationObjects(prerequisites)
	return preparedStateIndex{snapshot: snapshot, root: root, rootBody: body, prerequisites: prerequisites}, err
}

func (e *Engine) stateIndexEntry(ctx context.Context, logical state.Key) (storageformat.StateIndexEntry, error) {
	root, err := e.readStateIndexRoot(ctx, stateNamespace(logical))
	if err != nil {
		return storageformat.StateIndexEntry{}, err
	}
	return e.stateIndexEntryAtRoot(ctx, root.root, logical.String())
}

func (e *Engine) stateIndexEntryAtRoot(ctx context.Context, root storageformat.StateIndexRoot, logicalKey string) (storageformat.StateIndexEntry, error) {
	if root.EntryCount == 0 {
		return storageformat.StateIndexEntry{}, domain.NewError(domain.ErrorNotFound, "state key not found")
	}
	reference, err := e.rootStateIndexChild(ctx, root)
	if err != nil {
		return storageformat.StateIndexEntry{}, err
	}
	for {
		node, err := e.readStateIndexNode(ctx, root.Namespace, reference)
		if err != nil {
			return storageformat.StateIndexEntry{}, err
		}
		if node.Leaf {
			index := sort.Search(len(node.Entries), func(index int) bool { return node.Entries[index].LogicalKey >= logicalKey })
			if index < len(node.Entries) && node.Entries[index].LogicalKey == logicalKey {
				return node.Entries[index], nil
			}
			return storageformat.StateIndexEntry{}, domain.NewError(domain.ErrorNotFound, "state key not found")
		}
		index := sort.Search(len(node.Children), func(index int) bool { return node.Children[index].LastKey >= logicalKey })
		if index == len(node.Children) || node.Children[index].FirstKey > logicalKey {
			return storageformat.StateIndexEntry{}, domain.NewError(domain.ErrorNotFound, "state key not found")
		}
		reference = node.Children[index]
	}
}

func (e *Engine) rootStateIndexChild(ctx context.Context, root storageformat.StateIndexRoot) (storageformat.StateIndexChild, error) {
	if root.NodeID == "" || root.NodeDigest == "" || root.EntryCount == 0 {
		return storageformat.StateIndexChild{}, domain.NewError(domain.ErrorInvalid, "invalid non-empty state index root")
	}
	key := storageformat.StateIndexNodeKey(root.Namespace, root.NodeID)
	object, err := e.backend.Get(ctx, key)
	if err != nil {
		return storageformat.StateIndexChild{}, err
	}
	if storageformat.Digest(object.Body) != root.NodeDigest {
		return storageformat.StateIndexChild{}, domain.NewError(domain.ErrorInvalid, "state index root node digest mismatch")
	}
	var envelope storageformat.Envelope
	var node storageformat.StateIndexNode
	if err := storageformat.DecodeEnvelope(object.Body, key, stateIndexNodeSchema, &envelope, &node); err != nil {
		return storageformat.StateIndexChild{}, err
	}
	if err := validateStateIndexNode(root.Namespace, node); err != nil {
		return storageformat.StateIndexChild{}, err
	}
	child, err := stateIndexNodeChild(node, root.NodeDigest)
	if err != nil || child.EntryCount != root.EntryCount {
		return storageformat.StateIndexChild{}, domain.NewError(domain.ErrorInvalid, "state index root metadata mismatch")
	}
	return child, nil
}

func (e *Engine) collectStateIndexEntries(ctx context.Context, root storageformat.StateIndexRoot, prefix, after string, limit int) ([]storageformat.StateIndexEntry, error) {
	if root.EntryCount == 0 {
		return nil, nil
	}
	reference, err := e.rootStateIndexChild(ctx, root)
	if err != nil {
		return nil, err
	}
	result := make([]storageformat.StateIndexEntry, 0, limit)
	upper := prefix + "\xff"
	var walk func(storageformat.StateIndexChild) error
	walk = func(reference storageformat.StateIndexChild) error {
		if len(result) >= limit || reference.LastKey <= after || reference.LastKey < prefix || reference.FirstKey > upper {
			return nil
		}
		node, err := e.readStateIndexNode(ctx, root.Namespace, reference)
		if err != nil {
			return err
		}
		if node.Leaf {
			for _, entry := range node.Entries {
				if entry.LogicalKey <= after || !strings.HasPrefix(entry.LogicalKey, prefix) {
					continue
				}
				result = append(result, entry)
				if len(result) == limit {
					break
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
	return result, walk(reference)
}
