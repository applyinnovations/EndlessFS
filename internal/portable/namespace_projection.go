package portable

import (
	"container/heap"
	"context"
	"errors"
	"sort"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/objectstore"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const namespaceProjectionHeadSchema = "namespace-sort-projection-v1"

func namespaceProjectionKind(field domain.SortField) (storageformat.ProjectionKind, error) {
	switch field {
	case domain.SortModified:
		return storageformat.ProjectionModified, nil
	case domain.SortSize:
		return storageformat.ProjectionSize, nil
	case domain.SortKind:
		return storageformat.ProjectionEntryKind, nil
	default:
		return "", domain.NewError(domain.ErrorInvalid, "invalid namespace projection sort")
	}
}

func namespaceProjectionID(owner domain.UserID, area domain.Area, directory storageformat.NamespaceEntry, field domain.SortField) string {
	return storageformat.Digest([]byte("endlessfs-namespace-sort-projection-v1\x00" + owner.String() + "\x00" + areaName(area) + "\x00" + directory.NodeID + "\x00" + string(field)))
}

func newNamespaceProjectionTreeSession(store *consistencyDomainStore, owner domain.UserID, projectionID string, kind storageformat.ProjectionKind) *consistencyDomainTreeSession {
	reference := consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: projectionID}
	session := newConsistencyDomainTreeSession(store, reference)
	session.pageKey = func(digest string) objectstore.Key {
		return storageformat.ProjectionPageKey(owner.String(), kind, digest)
	}
	return session
}

type namespaceProjectionSnapshot struct {
	head     storageformat.ProjectionHead
	object   objectstore.Object
	envelope storageformat.Envelope
	exists   bool
	valid    bool
}

func (store *namespaceStore) loadNamespaceProjection(ctx context.Context, owner domain.UserID, projectionID string, kind storageformat.ProjectionKind) (namespaceProjectionSnapshot, error) {
	key := storageformat.ScopedProjectionHeadKey(owner.String(), kind, projectionID)
	object, err := store.engine.backend.Get(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return namespaceProjectionSnapshot{}, nil
	}
	if err != nil {
		return namespaceProjectionSnapshot{}, err
	}
	snapshot := namespaceProjectionSnapshot{object: object, exists: true}
	if err := storageformat.DecodeEnvelope(object.Body, key, namespaceProjectionHeadSchema, &snapshot.envelope, &snapshot.head); err != nil {
		// A projection is not authority. Retain the native token only for the
		// immediate repair CAS and rebuild it from the authenticated namespace.
		return snapshot, nil
	}
	if err := storageformat.ValidateProjectionHead(snapshot.head); err != nil || snapshot.head.OwnerID != owner.String() || snapshot.head.ProjectionID != projectionID || snapshot.head.Kind != kind {
		return snapshot, nil
	}
	snapshot.valid = true
	return snapshot, nil
}

func (store *namespaceStore) namespaceSortProjection(ctx context.Context, view *namespaceView, area domain.Area, directory storageformat.NamespaceEntry, field domain.SortField) (storageformat.DomainTreeRoot, error) {
	if directory.Children.Digest == "" {
		return storageformat.DomainTreeRoot{}, nil
	}
	kind, err := namespaceProjectionKind(field)
	if err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	owner, err := domain.ParseUserID(view.reference.ID)
	if err != nil {
		return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "invalid namespace projection owner")
	}
	projectionID := namespaceProjectionID(owner, area, directory, field)
	for {
		snapshot, err := store.loadNamespaceProjection(ctx, owner, projectionID, kind)
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		if snapshot.valid && snapshot.head.SourceDomainID == view.reference.ID && snapshot.head.SourceRoot == directory.Children {
			return snapshot.head.Root, nil
		}
		session := newNamespaceProjectionTreeSession(store.domain, owner, projectionID, kind)
		root, err := store.buildNamespaceSortProjection(ctx, view, session, directory.Children, field)
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		next := storageformat.ProjectionHead{
			SchemaVersion: 1, OwnerID: owner.String(), ProjectionID: projectionID, Kind: kind,
			SourceDomainID: view.reference.ID, SourceRevision: view.head.Revision, SourceRoot: directory.Children, Root: root,
		}
		if err := storageformat.ValidateProjectionHead(next); err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		revision := uint64(1)
		condition := objectstore.PutCondition{Mode: objectstore.PutCreateOnly}
		if snapshot.exists {
			condition = objectstore.PutCondition{Mode: objectstore.PutMatch, Version: snapshot.object.Version}
			if snapshot.valid {
				revision = snapshot.envelope.Revision + 1
			}
		}
		key := storageformat.ScopedProjectionHeadKey(owner.String(), kind, projectionID)
		body, err := storageformat.EncodeEnvelope(namespaceProjectionHeadSchema, key, revision, next)
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		if _, err := store.engine.backend.Put(ctx, key, body, condition); err == nil {
			return root, nil
		} else if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return storageformat.DomainTreeRoot{}, err
		}
	}
}

const namespaceProjectionMergeFanIn = 32

func (store *namespaceStore) buildNamespaceSortProjection(ctx context.Context, view *namespaceView, projection *consistencyDomainTreeSession, source storageformat.DomainTreeRoot, field domain.SortField) (storageformat.DomainTreeRoot, error) {
	sourceIterator, err := newConsistencyDomainTreeIterator(ctx, view.session, source)
	if err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	var runs []storageformat.DomainTreeRoot
	chunk := make([]storageformat.DomainEntry, 0, domainPageMaximumItems)
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		sortDomainProjectionEntries(chunk)
		root, err := projection.buildTree(ctx, chunk)
		if err != nil {
			return err
		}
		projection.pages = make(map[string]storageformat.DomainPage)
		runs = append(runs, root)
		chunk = chunk[:0]
		return nil
	}
	for {
		value, found, err := sourceIterator.Next()
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		if !found {
			break
		}
		entry, err := decodeNamespaceEntry(value.Value)
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		key, err := namespaceSortKey(field, entry.Entry)
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		chunk = append(chunk, storageformat.DomainEntry{Key: key, Value: value.Value, LogicalVersion: value.LogicalVersion})
		if len(chunk) == cap(chunk) {
			if err := flush(); err != nil {
				return storageformat.DomainTreeRoot{}, err
			}
		}
	}
	if err := flush(); err != nil {
		return storageformat.DomainTreeRoot{}, err
	}
	for len(runs) > 1 {
		next := make([]storageformat.DomainTreeRoot, 0, (len(runs)+namespaceProjectionMergeFanIn-1)/namespaceProjectionMergeFanIn)
		for offset := 0; offset < len(runs); offset += namespaceProjectionMergeFanIn {
			end := offset + namespaceProjectionMergeFanIn
			if end > len(runs) {
				end = len(runs)
			}
			merged, err := mergeNamespaceProjectionRuns(ctx, projection, runs[offset:end])
			if err != nil {
				return storageformat.DomainTreeRoot{}, err
			}
			next = append(next, merged)
		}
		runs = next
	}
	if len(runs) == 0 {
		return storageformat.DomainTreeRoot{}, nil
	}
	return runs[0], nil
}

func sortDomainProjectionEntries(entries []storageformat.DomainEntry) {
	sort.Slice(entries, func(left, right int) bool { return entries[left].Key < entries[right].Key })
}

type namespaceProjectionHeapItem struct {
	entry storageformat.DomainEntry
	run   int
}

type namespaceProjectionHeap []namespaceProjectionHeapItem

func (values namespaceProjectionHeap) Len() int { return len(values) }
func (values namespaceProjectionHeap) Less(left, right int) bool {
	return values[left].entry.Key < values[right].entry.Key
}
func (values namespaceProjectionHeap) Swap(left, right int) {
	values[left], values[right] = values[right], values[left]
}
func (values *namespaceProjectionHeap) Push(value any) {
	*values = append(*values, value.(namespaceProjectionHeapItem))
}
func (values *namespaceProjectionHeap) Pop() any {
	old := *values
	value := old[len(old)-1]
	*values = old[:len(old)-1]
	return value
}

func mergeNamespaceProjectionRuns(ctx context.Context, session *consistencyDomainTreeSession, runs []storageformat.DomainTreeRoot) (storageformat.DomainTreeRoot, error) {
	iterators := make([]*consistencyDomainTreeIterator, len(runs))
	values := &namespaceProjectionHeap{}
	heap.Init(values)
	for index, root := range runs {
		iterator, err := newConsistencyDomainTreeIterator(ctx, session, root)
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		iterators[index] = iterator
		entry, found, err := iterator.Next()
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		if found {
			heap.Push(values, namespaceProjectionHeapItem{entry: entry, run: index})
		}
	}
	builder := newConsistencyDomainTreeBuilder(ctx, session)
	previous := ""
	for values.Len() > 0 {
		item := heap.Pop(values).(namespaceProjectionHeapItem)
		if previous != "" && item.entry.Key <= previous {
			return storageformat.DomainTreeRoot{}, domain.NewError(domain.ErrorInvalid, "duplicate namespace projection ordering key")
		}
		previous = item.entry.Key
		if err := builder.Add(item.entry); err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		next, found, err := iterators[item.run].Next()
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		if found {
			heap.Push(values, namespaceProjectionHeapItem{entry: next, run: item.run})
		}
	}
	return builder.Finish()
}
