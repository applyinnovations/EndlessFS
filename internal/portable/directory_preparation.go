package portable

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

type directoryPreparationNode struct {
	path     domain.UserPath
	index    int
	children map[string]*directoryPreparationNode
}

func directoryPreparationTree(paths []domain.UserPath) *directoryPreparationNode {
	root := &directoryPreparationNode{path: namespaceRootPath(), index: -1, children: make(map[string]*directoryPreparationNode)}
	for index, path := range paths {
		node := root
		for _, name := range path.Segments() {
			child := node.children[name]
			if child == nil {
				childPath, _ := node.path.Join(name)
				child = &directoryPreparationNode{path: childPath, index: -1, children: make(map[string]*directoryPreparationNode)}
				node.children[name] = child
			}
			node = child
		}
		node.index = index
	}
	return root
}

// PrepareDirectories observes existing paths once and publishes all eligible
// new directories in one owner-head CAS. Unselected missing parents are never
// invented. Independent subtrees share immutable reads and execute in parallel;
// there are at most 100 selected leaves in a request.
func (s *FileStore) PrepareDirectories(ctx context.Context, scope domain.Scope, paths []domain.UserPath) (results []domain.DirectoryPreparationResult, failure error) {
	if err := validateFileRequest(ctx, scope); err != nil {
		return nil, err
	}
	if scope.Area() != domain.AreaLive {
		return nil, domain.NewError(domain.ErrorInvalid, "directory preparation requires live scope")
	}
	if err := domain.ValidateDirectoryPreparationPaths(paths); err != nil {
		return nil, err
	}
	stage := telemetry.NamespaceRead
	defer func() { failure = planningDiagnostic(failure, stage) }()
	store := newNamespaceStore(s.engine)
	mutationID, err := s.engine.ids.OpaqueID()
	if err != nil {
		return nil, err
	}
	values := make([]string, len(paths))
	for index, path := range paths {
		values[index] = path.String()
	}
	fingerprint := namespaceRequestFingerprint("prepare-directories", strings.Join(values, "\x00"))
	operationID := namespaceOperationID(scope.UserID(), "prepare-directories", mutationID)
	tree := directoryPreparationTree(paths)
	now := s.engine.clock.Now().UTC()
	for {
		stage = telemetry.NamespaceRead
		view, err := store.loadView(ctx, scope.UserID(), "")
		if err != nil {
			return nil, err
		}
		if err := view.bindMutation(mutationID, fingerprint); err != nil {
			return nil, err
		}
		// Fix physical grouping before sibling workers race to write their
		// first page. The complete source head and intent already bind this ID.
		if _, err := view.session.ensurePackID(fingerprint); err != nil {
			return nil, err
		}
		result := make([]domain.DirectoryPreparationResult, len(paths))
		for index, path := range paths {
			result[index] = domain.DirectoryPreparationResult{Index: index, Path: path}
		}
		var block func(*directoryPreparationNode, domain.ErrorKind)
		block = func(node *directoryPreparationNode, code domain.ErrorKind) {
			if node.index >= 0 {
				result[node.index].ErrorCode = code
			}
			for _, child := range node.children {
				block(child, code)
			}
		}
		var walk func(storageformat.NamespaceEntry, *directoryPreparationNode) (storageformat.NamespaceEntry, bool, error)
		walk = func(parent storageformat.NamespaceEntry, node *directoryPreparationNode) (storageformat.NamespaceEntry, bool, error) {
			names := make([]string, 0, len(node.children))
			for name := range node.children {
				names = append(names, name)
			}
			sort.Strings(names)
			edits := make([]namespaceDirectoryEdit, len(names))
			changed := make([]bool, len(names))
			failures := make([]error, len(names))
			process := func(index int) {
				childNode := node.children[names[index]]
				entry, found, childErr := store.child(ctx, view, parent, names[index])
				if childErr != nil {
					failures[index] = childErr
					return
				}
				if found && entry.Entry.Kind != domain.EntryDirectory {
					block(childNode, domain.ErrorConflict)
					return
				}
				if !found && childNode.index < 0 {
					block(childNode, domain.ErrorNotFound)
					return
				}
				before := entry
				if !found {
					accumulator, digest, createErr := directoryContentIdentity(nil)
					if createErr != nil {
						failures[index] = createErr
						return
					}
					nodeID := namespaceNodeID(operationID, childNode.path.String())
					entry = storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: nodeID, Entry: storageformat.DirectoryEntry{Name: names[index], NameDigest: storageformat.NameDigest(names[index]), Kind: domain.EntryDirectory, DirectoryID: nodeID, ContentDigest: digest, ModifiedAt: now}, ContentAccumulator: accumulator}
					entry.Entry.LogicalVersion, childErr = directoryEntryVersion(entry.Entry)
					if childErr != nil {
						failures[index] = childErr
						return
					}
				}
				updated, nestedChanged, childErr := walk(entry, childNode)
				if childErr != nil {
					failures[index] = childErr
					return
				}
				if childNode.index >= 0 {
					result[childNode.index].Ready = true
				}
				if !found || nestedChanged {
					edits[index].after = &updated
					if found {
						edits[index].before = &before
					}
					changed[index] = true
				}
			}
			if len(names) == 1 {
				process(0)
			} else {
				var wait sync.WaitGroup
				for index := range names {
					wait.Add(1)
					go func(index int) { defer wait.Done(); process(index) }(index)
				}
				wait.Wait()
			}
			selected := make([]namespaceDirectoryEdit, 0, len(names))
			for index := range names {
				if failures[index] != nil {
					return storageformat.NamespaceEntry{}, false, failures[index]
				}
				if changed[index] {
					selected = append(selected, edits[index])
				}
			}
			if len(selected) == 0 {
				return parent, false, nil
			}
			updated, err := store.applyDirectoryEdits(ctx, view, parent, selected, now)
			return updated, true, err
		}
		root, changed, err := walk(view.roots[domain.AreaLive], tree)
		if err != nil {
			return nil, err
		}
		if !changed {
			return result, nil
		}
		operation := domain.Operation{ID: operationID, State: domain.OperationSucceeded, StartedAt: now, UpdatedAt: now}
		stage = telemetry.NamespacePublish
		_, err = store.commit(ctx, view, mutationID, fingerprint, map[string]storageformat.NamespaceEntry{namespaceFrameKey(domain.AreaLive, namespaceRootPath()): root}, storageformat.NamespaceMutationResult{Operation: &operation})
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return nil, err
		}
	}
}
