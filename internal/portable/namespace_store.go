package portable

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

const namespaceRootPrefix = "root/"

const (
	namespacePackRebuildMinimumEdits = domainPageMaximumItems
	namespacePackRebuildMaximumItems = 20_000
)

type namespaceStore struct {
	engine *Engine
	domain *consistencyDomainStore
}

type namespaceView struct {
	reference      consistencyDomainRef
	head           storageformat.DomainHead
	headSnapshot   *consistencyDomainHeadSnapshot
	snapshotDigest string
	session        *consistencyDomainTreeSession
	roots          map[domain.Area]storageformat.NamespaceEntry
	rootVersions   map[domain.Area]string
	rootExists     map[domain.Area]bool
	uploadAborts   map[string]portableUploadAbortCache
}

type portableUploadAbortCache struct {
	record storageformat.PortableUploadBatchAbort
	value  consistencyDomainValue
	found  bool
}

func (view *namespaceView) bindMutation(mutationID, fingerprint string) error {
	if view == nil || view.session == nil || mutationID == "" || fingerprint == "" {
		return domain.NewError(domain.ErrorInvalid, "namespace mutation binding is invalid")
	}
	seed := storageformat.Digest([]byte("endlessfs-namespace-packed-mutation-v1\x00" + mutationID + "\x00" + fingerprint))
	return view.session.bindPackedMutation(seed)
}

type namespaceFrame struct {
	key        string
	path       domain.UserPath
	area       domain.Area
	entry      storageformat.NamespaceEntry
	parentKey  string
	parentName string
	depth      int
}

type namespaceDirectoryEdit struct {
	before *storageformat.NamespaceEntry
	after  *storageformat.NamespaceEntry
}

type namespaceListCursor struct {
	SchemaVersion int              `json:"schemaVersion"`
	OwnerID       string           `json:"ownerID"`
	Area          string           `json:"area"`
	Directory     string           `json:"directory"`
	Sort          domain.SortField `json:"sort"`
	Descending    bool             `json:"descending"`
	PageSize      int              `json:"pageSize"`
	Snapshot      string           `json:"snapshot"`
	Bound         string           `json:"bound"`
	ExpiresAt     time.Time        `json:"expiresAt"`
}

func newNamespaceStore(engine *Engine) *namespaceStore {
	return &namespaceStore{engine: engine, domain: newConsistencyDomainStore(engine.backend, engine.scheduler, engine.clock)}
}

func namespaceReference(owner domain.UserID) consistencyDomainRef {
	return consistencyDomainRef{Kind: storageformat.DomainNamespace, ID: owner.String()}
}

func namespaceRootKey(area domain.Area) string { return namespaceRootPrefix + areaName(area) }

func namespaceRootPath() domain.UserPath { return domain.MustParseUserPath("/") }

func namespaceFrameKey(area domain.Area, path domain.UserPath) string {
	return areaName(area) + ":" + path.String()
}

func (store *namespaceStore) emptyRoot(owner domain.UserID, area domain.Area) (storageformat.NamespaceEntry, error) {
	accumulator, digest, err := directoryContentIdentity(nil)
	if err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	entry := storageformat.NamespaceEntry{
		SchemaVersion: 1,
		NodeID:        "root-" + areaName(area),
		Entry: storageformat.DirectoryEntry{
			Kind:          domain.EntryDirectory,
			DirectoryID:   "root-" + areaName(area),
			ContentDigest: digest,
			ModifiedAt:    time.Unix(0, 0).UTC(),
		},
		ContentAccumulator: accumulator,
	}
	entry.Entry.LogicalVersion = storageformat.Digest([]byte("endlessfs-namespace-empty-root-v1\x00" + owner.String() + "\x00" + areaName(area)))
	return entry, nil
}

func (store *namespaceStore) loadView(ctx context.Context, owner domain.UserID, snapshotDigest string) (*namespaceView, error) {
	reference := namespaceReference(owner)
	var head storageformat.DomainHead
	var headSnapshot *consistencyDomainHeadSnapshot
	if snapshotDigest == "" {
		for {
			snapshot, err := store.domain.loadHead(ctx, reference)
			if err != nil {
				return nil, err
			}
			if snapshot.exists && snapshot.head.Registered && len(snapshot.head.Deltas) >= consistencyDomainDeltaWindow {
				if err := store.domain.compactSnapshot(ctx, reference, snapshot); err == nil || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPreconditionFailed) {
					continue
				} else {
					return nil, err
				}
			}
			head = snapshot.head
			headSnapshot = &snapshot
			break
		}
	} else {
		var err error
		head, err = store.domain.loadHeadSnapshot(ctx, reference, snapshotDigest)
		if err != nil {
			return nil, err
		}
	}
	session := newConsistencyDomainTreeSession(store.domain, reference)
	headBody, err := storageformat.EncodeCanonical(head)
	if err != nil {
		return nil, err
	}
	session.enablePackedWrites(storageformat.Digest(headBody))
	view := &namespaceView{
		reference: reference, head: head, headSnapshot: headSnapshot, snapshotDigest: snapshotDigest,
		session: session, roots: make(map[domain.Area]storageformat.NamespaceEntry),
		rootVersions: make(map[domain.Area]string), rootExists: make(map[domain.Area]bool),
		uploadAborts: make(map[string]portableUploadAbortCache),
	}
	// Every namespace page root embedded in the authenticated delta window was
	// successfully published before that head became visible. Remember those
	// descriptors so an undo/restore that returns to a prior content identity
	// can reuse the already-authenticated immutable object without another
	// provider call. The candidate page is encoded and digest-checked below;
	// checkpoint reachability and garbage collection keep every head-referenced
	// immutable object alive.
	for _, delta := range head.Deltas {
		for _, change := range delta.Changes {
			if change.Delete || change.Key != namespaceRootKey(domain.AreaLive) && change.Key != namespaceRootKey(domain.AreaTrash) {
				continue
			}
			entry, err := decodeNamespaceEntry(change.Value)
			if err != nil {
				return nil, err
			}
			view.session.markKnown(entry.Children)
		}
	}
	for _, area := range []domain.Area{domain.AreaLive, domain.AreaTrash} {
		value, found, err := store.domain.lookupAtHeadWithSession(ctx, reference, head, namespaceRootKey(area), view.session)
		if err != nil {
			return nil, err
		}
		if !found {
			view.roots[area], err = store.emptyRoot(owner, area)
			if err != nil {
				return nil, err
			}
			continue
		}
		entry, err := decodeNamespaceEntry(value.Data)
		if err != nil || entry.Entry.Name != "" {
			return nil, domain.NewError(domain.ErrorInvalid, "invalid namespace root")
		}
		view.roots[area], view.rootVersions[area], view.rootExists[area] = entry, value.LogicalVersion, true
		view.session.markKnown(entry.Children)
	}
	return view, nil
}

func decodeNamespaceEntry(body []byte) (storageformat.NamespaceEntry, error) {
	var entry storageformat.NamespaceEntry
	if err := decodeCanonicalValue(body, &entry); err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	if err := storageformat.ValidateNamespaceEntry(entry); err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	version, err := directoryEntryVersion(entry.Entry)
	if err != nil || version != entry.Entry.LogicalVersion {
		return storageformat.NamespaceEntry{}, domain.NewError(domain.ErrorInvalid, "namespace entry logical version mismatch")
	}
	return entry, nil
}

func encodeNamespaceEntry(entry storageformat.NamespaceEntry) ([]byte, error) {
	if err := storageformat.ValidateNamespaceEntry(entry); err != nil {
		return nil, err
	}
	version, err := directoryEntryVersion(entry.Entry)
	if err != nil || version != entry.Entry.LogicalVersion {
		return nil, domain.NewError(domain.ErrorInvalid, "namespace entry logical version mismatch")
	}
	return storageformat.EncodeCanonical(entry)
}

func (store *namespaceStore) child(ctx context.Context, view *namespaceView, parent storageformat.NamespaceEntry, name string) (storageformat.NamespaceEntry, bool, error) {
	value, found, err := view.session.lookup(ctx, parent.Children, name)
	if err != nil || !found {
		return storageformat.NamespaceEntry{}, found, err
	}
	entry, err := decodeNamespaceEntry(value.Data)
	if err != nil || entry.Entry.Name != name || value.LogicalVersion != entry.Entry.LogicalVersion {
		return storageformat.NamespaceEntry{}, false, domain.NewError(domain.ErrorInvalid, "namespace child key binding mismatch")
	}
	return entry, true, nil
}

func (store *namespaceStore) resolveTrail(ctx context.Context, view *namespaceView, area domain.Area, path domain.UserPath) ([]namespaceFrame, error) {
	root := view.roots[area]
	trail := []namespaceFrame{{key: namespaceFrameKey(area, namespaceRootPath()), path: namespaceRootPath(), area: area, entry: root}}
	current := root
	currentPath := namespaceRootPath()
	for _, segment := range path.Segments() {
		entry, found, err := store.child(ctx, view, current, segment)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, domain.NewError(domain.ErrorNotFound, "directory does not exist")
		}
		if entry.Entry.Kind != domain.EntryDirectory {
			return nil, domain.NewError(domain.ErrorInvalid, "path component is not a directory")
		}
		nextPath, err := currentPath.Join(segment)
		if err != nil {
			return nil, domain.NewError(domain.ErrorInvalid, "stored namespace name is invalid")
		}
		trail = append(trail, namespaceFrame{key: namespaceFrameKey(area, nextPath), path: nextPath, area: area, entry: entry, parentKey: namespaceFrameKey(area, currentPath), parentName: segment, depth: len(trail)})
		current, currentPath = entry, nextPath
	}
	return trail, nil
}

func namespaceSortKey(field domain.SortField, entry storageformat.DirectoryEntry) (string, error) {
	if entry.Name == "" {
		return "", domain.NewError(domain.ErrorInvalid, "namespace child name is empty")
	}
	name := base64.RawURLEncoding.EncodeToString([]byte(entry.Name))
	switch field {
	case domain.SortModified:
		return entry.ModifiedAt.UTC().Format("20060102T150405.000000000Z") + "." + name, nil
	case domain.SortSize:
		if entry.Size < 0 {
			return "", domain.NewError(domain.ErrorInvalid, "namespace child size is negative")
		}
		return fmt.Sprintf("%016x.%s", uint64(entry.Size), name), nil
	case domain.SortKind:
		return string(entry.Kind) + "." + name, nil
	default:
		return "", domain.NewError(domain.ErrorInvalid, "invalid namespace secondary sort")
	}
}

func normalizeDomainChanges(values map[string]storageformat.DomainChange) []storageformat.DomainChange {
	changes := make([]storageformat.DomainChange, 0, len(values))
	for _, change := range values {
		changes = append(changes, change)
	}
	sort.Slice(changes, func(left, right int) bool { return changes[left].Key < changes[right].Key })
	return changes
}

func (store *namespaceStore) applyDirectoryEdits(ctx context.Context, view *namespaceView, parent storageformat.NamespaceEntry, edits []namespaceDirectoryEdit, modifiedAt time.Time) (storageformat.NamespaceEntry, error) {
	if parent.EntryCount > math.MaxInt64 {
		return storageformat.NamespaceEntry{}, domain.NewError(domain.ErrorInvalid, "namespace entry count overflows")
	}
	children := make(map[string]storageformat.DomainChange)
	beforeRecords := make([]storageformat.DirectoryEntry, 0, len(edits))
	afterRecords := make([]storageformat.DirectoryEntry, 0, len(edits))
	count := int64(parent.EntryCount)
	bytesValue, fileCount := parent.Entry.Size, parent.Entry.FileCount
	for _, edit := range edits {
		if edit.before != nil {
			before := edit.before.Entry
			beforeRecords = append(beforeRecords, before)
			children[before.Name] = storageformat.DomainChange{Key: before.Name, Delete: true}
			count--
			var err error
			bytesValue, err = addAggregateDelta(bytesValue, -before.Size, "namespace recursive bytes")
			if err != nil {
				return storageformat.NamespaceEntry{}, err
			}
			files := int64(1)
			if before.Kind == domain.EntryDirectory {
				files = before.FileCount
			}
			fileCount, err = addAggregateDelta(fileCount, -files, "namespace recursive file count")
			if err != nil {
				return storageformat.NamespaceEntry{}, err
			}
		}
		if edit.after != nil {
			after := edit.after.Entry
			afterRecords = append(afterRecords, after)
			body, err := encodeNamespaceEntry(*edit.after)
			if err != nil {
				return storageformat.NamespaceEntry{}, err
			}
			children[after.Name] = storageformat.DomainChange{Key: after.Name, Value: body, LogicalVersion: after.LogicalVersion}
			count++
			bytesValue, err = addAggregateDelta(bytesValue, after.Size, "namespace recursive bytes")
			if err != nil {
				return storageformat.NamespaceEntry{}, err
			}
			files := int64(1)
			if after.Kind == domain.EntryDirectory {
				files = after.FileCount
			}
			fileCount, err = addAggregateDelta(fileCount, files, "namespace recursive file count")
			if err != nil {
				return storageformat.NamespaceEntry{}, err
			}
		}
	}
	if count < 0 {
		return storageformat.NamespaceEntry{}, domain.NewError(domain.ErrorInvalid, "namespace entry count underflows")
	}
	var err error
	normalizedChanges := normalizeDomainChanges(children)
	if len(normalizedChanges) >= namespacePackRebuildMinimumEdits && parent.Children.EntryCount <= namespacePackRebuildMaximumItems && count <= namespacePackRebuildMaximumItems {
		parent.Children, err = view.session.rebuild(ctx, parent.Children, normalizedChanges)
	} else {
		parent.Children, err = view.session.apply(ctx, parent.Children, normalizedChanges)
	}
	if err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	parent.EntryCount = uint64(count)
	parent.ContentAccumulator, parent.Entry.ContentDigest, err = updateDirectoryContentIdentityAtCount(parent.ContentAccumulator, beforeRecords, afterRecords, int(count))
	if err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	parent.Entry.Size, parent.Entry.FileCount, parent.Entry.ModifiedAt = bytesValue, fileCount, modifiedAt.UTC()
	parent.Entry.LogicalVersion, err = directoryEntryVersion(parent.Entry)
	if err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	if err := storageformat.ValidateNamespaceEntry(parent); err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	return parent, nil
}

func mergeNamespaceFrames(trails ...[]namespaceFrame) map[string]namespaceFrame {
	frames := make(map[string]namespaceFrame)
	for _, trail := range trails {
		for _, frame := range trail {
			frames[frame.key] = frame
		}
	}
	return frames
}

func (store *namespaceStore) propagate(ctx context.Context, view *namespaceView, frames map[string]namespaceFrame, changes map[string]storageformat.NamespaceEntry, modifiedAt time.Time) error {
	maxDepth := 0
	for key := range changes {
		if frame, found := frames[key]; found && frame.depth > maxDepth {
			maxDepth = frame.depth
		}
	}
	for depth := maxDepth; depth > 0; depth-- {
		grouped := make(map[string][]namespaceDirectoryEdit)
		for key, changed := range changes {
			frame, found := frames[key]
			if !found || frame.depth != depth {
				continue
			}
			before, after := frame.entry, changed
			grouped[frame.parentKey] = append(grouped[frame.parentKey], namespaceDirectoryEdit{before: &before, after: &after})
		}
		parents := make([]string, 0, len(grouped))
		for key := range grouped {
			parents = append(parents, key)
		}
		sort.Strings(parents)
		for _, parentKey := range parents {
			parent, found := changes[parentKey]
			if !found {
				parent = frames[parentKey].entry
			}
			updated, err := store.applyDirectoryEdits(ctx, view, parent, grouped[parentKey], modifiedAt)
			if err != nil {
				return err
			}
			changes[parentKey] = updated
		}
	}
	return nil
}

func (store *namespaceStore) operationReplay(ctx context.Context, view *namespaceView, mutationID, requestFingerprint string) (*storageformat.NamespaceMutationResult, error) {
	outcome, found, err := store.domain.lookupOutcomeAtHeadWithSession(ctx, view.reference, view.head, mutationID, view.session)
	if err != nil || !found {
		return nil, err
	}
	var result storageformat.NamespaceMutationResult
	if err := decodeCanonicalValue(outcome.Result, &result); err != nil || validateNamespaceMutationResult(result) != nil {
		return nil, domain.NewError(domain.ErrorInvalid, "invalid namespace mutation outcome")
	}
	if requestFingerprint != "" && result.RequestFingerprint != requestFingerprint {
		return nil, domain.NewError(domain.ErrorConflict, "namespace idempotency key was reused")
	}
	return &result, nil
}

func validateNamespaceMutationResult(result storageformat.NamespaceMutationResult) error {
	if err := storageformat.ValidateNamespaceMutationResult(result); err != nil {
		return err
	}
	validateOperation := func(operation domain.Operation, requireSuccess bool) error {
		terminal := operation.State == domain.OperationSucceeded || operation.State == domain.OperationFailed
		if operation.ID == "" || !terminal || requireSuccess && operation.State != domain.OperationSucceeded || operation.StartedAt.IsZero() || operation.UpdatedAt.IsZero() || operation.UpdatedAt.Before(operation.StartedAt) {
			return domain.NewError(domain.ErrorInvalid, "invalid namespace operation result")
		}
		if operation.State == domain.OperationSucceeded && (operation.ErrorKind != "" || operation.Error != "") {
			return domain.NewError(domain.ErrorInvalid, "successful namespace operation contains an error")
		}
		return nil
	}
	if result.Operation != nil {
		return validateOperation(*result.Operation, false)
	}
	if result.Entry != nil {
		return validateDirectoryEntries([]storageformat.DirectoryEntry{*result.Entry})
	}
	if result.Batch != nil {
		return validateOperation(result.Batch.Operation, true)
	}
	if result.UploadBatch != nil {
		return nil
	}
	return nil
}

func (store *namespaceStore) commit(ctx context.Context, view *namespaceView, mutationID string, requestFingerprint string, changes map[string]storageformat.NamespaceEntry, result storageformat.NamespaceMutationResult) (storageformat.NamespaceMutationResult, error) {
	return store.commitWithAdditionalChanges(ctx, view, mutationID, requestFingerprint, changes, nil, result)
}

// commitWithAdditionalChanges publishes namespace roots and other values that
// share the owner namespace invariant through one head CAS. Upload completion
// uses this to make the completed upload outcome and the visible file edge one
// atomic transition rather than a recoverable cross-domain sequence.
func (store *namespaceStore) commitWithAdditionalChanges(ctx context.Context, view *namespaceView, mutationID string, requestFingerprint string, changes map[string]storageformat.NamespaceEntry, additional []consistencyDomainChange, result storageformat.NamespaceMutationResult) (storageformat.NamespaceMutationResult, error) {
	return store.commitWithAdditionalChangesMode(ctx, view, mutationID, requestFingerprint, changes, additional, result, false)
}

func (store *namespaceStore) commitMaterializedWithAdditionalChanges(ctx context.Context, view *namespaceView, mutationID string, requestFingerprint string, changes map[string]storageformat.NamespaceEntry, additional []consistencyDomainChange, result storageformat.NamespaceMutationResult) (storageformat.NamespaceMutationResult, error) {
	return store.commitWithAdditionalChangesMode(ctx, view, mutationID, requestFingerprint, changes, additional, result, true)
}

func (store *namespaceStore) commitWithAdditionalChangesMode(ctx context.Context, view *namespaceView, mutationID string, requestFingerprint string, changes map[string]storageformat.NamespaceEntry, additional []consistencyDomainChange, result storageformat.NamespaceMutationResult, materialized bool) (storageformat.NamespaceMutationResult, error) {
	if view == nil || view.headSnapshot == nil || view.snapshotDigest != "" {
		return storageformat.NamespaceMutationResult{}, domain.NewError(domain.ErrorInvalid, "namespace mutation requires a live authoritative head")
	}
	mutationChanges := make([]consistencyDomainChange, 0, 2+len(additional))
	for _, area := range []domain.Area{domain.AreaLive, domain.AreaTrash} {
		key := namespaceFrameKey(area, namespaceRootPath())
		root, changed := changes[key]
		if !changed {
			continue
		}
		body, err := encodeNamespaceEntry(root)
		if err != nil {
			return storageformat.NamespaceMutationResult{}, err
		}
		requirement := domainValueAbsent
		if view.rootExists[area] {
			requirement = domainValuePresent
		}
		mutationChanges = append(mutationChanges, consistencyDomainChange{Key: namespaceRootKey(area), Require: requirement, ExpectedVersion: view.rootVersions[area], Value: body})
	}
	if len(mutationChanges) == 0 {
		return storageformat.NamespaceMutationResult{}, domain.NewError(domain.ErrorInvalid, "namespace mutation has no root change")
	}
	mutationChanges = append(mutationChanges, additional...)
	result.SchemaVersion, result.RequestFingerprint = 1, requestFingerprint
	if err := validateNamespaceMutationResult(result); err != nil {
		return storageformat.NamespaceMutationResult{}, err
	}
	resultBody, err := storageformat.EncodeCanonical(result)
	if err != nil {
		return storageformat.NamespaceMutationResult{}, err
	}
	mutation := consistencyDomainMutation{ID: mutationID, Changes: mutationChanges, Result: resultBody}
	var outcome consistencyDomainOutcome
	if materialized {
		outcome, err = store.domain.mutateMaterializedPrepared(ctx, view.reference, mutation, view.headSnapshot, view.session)
	} else {
		outcome, err = store.domain.mutatePrepared(ctx, view.reference, mutation, view.headSnapshot, view.session)
	}
	if err != nil {
		return storageformat.NamespaceMutationResult{}, err
	}
	if err := decodeCanonicalValue(outcome.Result, &result); err != nil || validateNamespaceMutationResult(result) != nil || result.RequestFingerprint != requestFingerprint {
		return storageformat.NamespaceMutationResult{}, domain.NewError(domain.ErrorInvalid, "namespace committed result is invalid")
	}
	return result, nil
}

func namespaceRequestFingerprint(kind string, values ...string) string {
	// Schema 008 deliberately retains the schema-007 normalized request digest.
	// Keeping this semantic identity lets an idempotent request committed before
	// migration replay the same outcome after cutover without retaining the old
	// persistence engine.
	return storageformat.Digest([]byte(strings.Join(append([]string{kind}, values...), "\x00")))
}

func namespaceOperationID(owner domain.UserID, kind, key string) domain.OperationID {
	return namespaceOperationIDFromKeyDigest(owner, kind, storageformat.Digest([]byte(key)))

}

func namespaceOperationIDFromKeyDigest(owner domain.UserID, kind, keyDigest string) domain.OperationID {
	return domain.OperationID(storageformat.Digest([]byte("endlessfs-namespace-operation-v2\x00" + owner.String() + "\x00" + kind + "\x00" + keyDigest)))
}

func namespaceNodeID(operationID domain.OperationID, role string) string {
	return storageformat.Digest([]byte("endlessfs-namespace-node-v1\x00" + string(operationID) + "\x00" + role))
}

func (store *namespaceStore) stat(ctx context.Context, scope domain.Scope, path domain.UserPath) (domain.Entry, error) {
	view, err := store.loadView(ctx, scope.UserID(), "")
	if err != nil {
		return domain.Entry{}, err
	}
	entry, err := store.resolveEntryAtView(ctx, view, scope, path)
	if err != nil {
		return domain.Entry{}, err
	}
	return namespaceDomainEntry(path, entry), nil
}

func (store *namespaceStore) resolveEntry(ctx context.Context, scope domain.Scope, path domain.UserPath) (storageformat.NamespaceEntry, error) {
	view, err := store.loadView(ctx, scope.UserID(), "")
	if err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	return store.resolveEntryAtView(ctx, view, scope, path)
}

func (store *namespaceStore) resolveEntryAtView(ctx context.Context, view *namespaceView, scope domain.Scope, path domain.UserPath) (storageformat.NamespaceEntry, error) {
	if view == nil || view.reference != namespaceReference(scope.UserID()) {
		return storageformat.NamespaceEntry{}, domain.NewError(domain.ErrorInvalid, "namespace entry view is misbound")
	}
	if path.IsRoot() {
		return view.roots[scope.Area()], nil
	}
	trail, err := store.resolveTrail(ctx, view, scope.Area(), path.Parent())
	if err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	entry, found, err := store.child(ctx, view, trail[len(trail)-1].entry, path.Name())
	if err != nil {
		return storageformat.NamespaceEntry{}, err
	}
	if !found {
		return storageformat.NamespaceEntry{}, domain.NewError(domain.ErrorNotFound, "entry does not exist")
	}
	return entry, nil
}

func namespaceDomainEntry(path domain.UserPath, entry storageformat.NamespaceEntry) domain.Entry {
	return domainEntry(path, entry.Entry)
}

func (store *namespaceStore) lookupChildren(ctx context.Context, scope domain.Scope, request domain.ChildLookupRequest) (domain.ChildLookup, error) {
	if !request.Directory.Valid() || len(request.Names) < 1 || len(request.Names) > 1000 {
		return domain.ChildLookup{}, domain.NewError(domain.ErrorInvalid, "child lookup request is invalid")
	}
	view, err := store.loadView(ctx, scope.UserID(), "")
	if err != nil {
		return domain.ChildLookup{}, err
	}
	trail, err := store.resolveTrail(ctx, view, scope.Area(), request.Directory)
	if err != nil {
		return domain.ChildLookup{}, err
	}
	parent := trail[len(trail)-1].entry
	result := domain.ChildLookup{Current: namespaceDomainEntry(request.Directory, parent), Entries: make([]domain.Entry, 0, len(request.Names))}
	seen := make(map[string]struct{}, len(request.Names))
	for _, name := range request.Names {
		path, err := request.Directory.Join(name)
		if err != nil {
			return domain.ChildLookup{}, domain.NewError(domain.ErrorInvalid, "child lookup name is invalid")
		}
		if _, exists := seen[name]; exists {
			return domain.ChildLookup{}, domain.NewError(domain.ErrorInvalid, "child lookup contains duplicate names")
		}
		seen[name] = struct{}{}
		entry, found, err := store.child(ctx, view, parent, name)
		if err != nil {
			return domain.ChildLookup{}, err
		}
		if !found {
			return domain.ChildLookup{}, domain.NewError(domain.ErrorNotFound, "child does not exist")
		}
		result.Entries = append(result.Entries, namespaceDomainEntry(path, entry))
	}
	return result, nil
}

func (store *namespaceStore) list(ctx context.Context, scope domain.Scope, request domain.ListRequest) (domain.ListPage, error) {
	pageSize, err := normalizeFilePageSize(request.PageSize)
	if err != nil {
		return domain.ListPage{}, err
	}
	if request.Sort == "" {
		request.Sort = domain.SortName
	}
	if !validSort(request.Sort) || !request.Directory.Valid() {
		return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "invalid namespace list request")
	}
	bound, snapshotDigest := "", ""
	expiresAt := store.engine.clock.Now().UTC().Add(store.engine.cursorTTL)
	if request.Cursor != "" {
		cursor, err := store.decodeListCursor(request.Cursor)
		if err != nil || cursor.OwnerID != scope.UserID().String() || cursor.Area != areaName(scope.Area()) || cursor.Directory != request.Directory.String() || cursor.Sort != request.Sort || cursor.Descending != request.Descending || cursor.PageSize != pageSize || !store.engine.clock.Now().Before(cursor.ExpiresAt) {
			return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope namespace cursor")
		}
		bound, snapshotDigest, expiresAt = cursor.Bound, cursor.Snapshot, cursor.ExpiresAt
	}
	view, err := store.loadView(ctx, scope.UserID(), snapshotDigest)
	if err != nil {
		return domain.ListPage{}, err
	}
	trail, err := store.resolveTrail(ctx, view, scope.Area(), request.Directory)
	if err != nil {
		return domain.ListPage{}, err
	}
	directory := trail[len(trail)-1].entry
	root := directory.Children
	listingSession := view.session
	if request.Sort != domain.SortName {
		root, err = store.namespaceSortProjection(ctx, view, scope.Area(), directory, request.Sort)
		if err != nil {
			return domain.ListPage{}, err
		}
		kind, err := namespaceProjectionKind(request.Sort)
		if err != nil {
			return domain.ListPage{}, err
		}
		listingSession = newNamespaceProjectionTreeSession(store.domain, scope.UserID(), namespaceProjectionID(scope.UserID(), scope.Area(), directory, request.Sort), kind)
	}
	entries, err := listingSession.collectOrdered(ctx, root, bound, pageSize+1, request.Descending)
	if err != nil {
		return domain.ListPage{}, err
	}
	hasMore := len(entries) > pageSize
	if hasMore {
		entries = entries[:pageSize]
	}
	page := domain.ListPage{Current: namespaceDomainEntry(request.Directory, directory), Entries: make([]domain.Entry, 0, len(entries))}
	for _, value := range entries {
		entry, err := decodeNamespaceEntry(value.Value)
		if err != nil {
			return domain.ListPage{}, err
		}
		path, err := request.Directory.Join(entry.Entry.Name)
		if err != nil {
			return domain.ListPage{}, domain.NewError(domain.ErrorInvalid, "stored namespace name is invalid")
		}
		page.Entries = append(page.Entries, namespaceDomainEntry(path, entry))
	}
	if hasMore {
		if view.snapshotDigest == "" {
			view.snapshotDigest, err = store.domain.writeHeadSnapshot(ctx, view.reference, view.head, expiresAt)
			if err != nil {
				return domain.ListPage{}, err
			}
		}
		page.NextCursor, err = store.encodeListCursor(namespaceListCursor{SchemaVersion: 1, OwnerID: scope.UserID().String(), Area: areaName(scope.Area()), Directory: request.Directory.String(), Sort: request.Sort, Descending: request.Descending, PageSize: pageSize, Snapshot: view.snapshotDigest, Bound: entries[len(entries)-1].Key, ExpiresAt: expiresAt})
		if err != nil {
			return domain.ListPage{}, err
		}
	}
	return page, nil
}

func (store *namespaceStore) encodeListCursor(cursor namespaceListCursor) (string, error) {
	body, err := storageformat.EncodeCanonical(cursor)
	if err != nil {
		return "", err
	}
	random, err := store.engine.ids.BearerToken()
	if err != nil {
		return "", err
	}
	nonce, err := base64.RawURLEncoding.DecodeString(random)
	if err != nil || len(nonce) < store.engine.cursorAEAD.NonceSize() {
		return "", domain.NewError(domain.ErrorInternal, "secure cursor randomness unavailable")
	}
	nonce = nonce[:store.engine.cursorAEAD.NonceSize()]
	sealed := store.engine.cursorAEAD.Seal(append([]byte(nil), nonce...), nonce, body, []byte("endlessfs-namespace-cursor-v1"))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (store *namespaceStore) decodeListCursor(value string) (namespaceListCursor, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(sealed) <= store.engine.cursorAEAD.NonceSize() {
		return namespaceListCursor{}, domain.NewError(domain.ErrorInvalid, "invalid namespace cursor")
	}
	nonceSize := store.engine.cursorAEAD.NonceSize()
	body, err := store.engine.cursorAEAD.Open(nil, sealed[:nonceSize], sealed[nonceSize:], []byte("endlessfs-namespace-cursor-v1"))
	if err != nil {
		return namespaceListCursor{}, domain.NewError(domain.ErrorInvalid, "invalid namespace cursor")
	}
	var cursor namespaceListCursor
	if err := decodeCanonicalValue(body, &cursor); err != nil || cursor.SchemaVersion != 1 || cursor.OwnerID == "" || cursor.Area == "" || cursor.Directory == "" || cursor.PageSize < 1 || cursor.Snapshot == "" || cursor.Bound == "" || cursor.ExpiresAt.IsZero() {
		return namespaceListCursor{}, domain.NewError(domain.ErrorInvalid, "invalid namespace cursor")
	}
	return cursor, nil
}

func (store *namespaceStore) resolveDestination(ctx context.Context, view *namespaceView, parent storageformat.NamespaceEntry, requested domain.UserPath, conflict domain.ConflictMode, expected domain.Version) (domain.UserPath, *storageformat.NamespaceEntry, error) {
	existing, found, err := store.child(ctx, view, parent, requested.Name())
	if err != nil {
		return domain.UserPath{}, nil, err
	}
	if !found {
		return requested, nil, nil
	}
	switch conflict {
	case domain.ConflictFail:
		return domain.UserPath{}, nil, domain.NewError(domain.ErrorConflict, "destination already exists")
	case domain.ConflictReplace:
		if expected == "" || expected != domain.Version(existing.Entry.LogicalVersion) {
			return domain.UserPath{}, nil, domain.NewError(domain.ErrorPreconditionFailed, "destination version does not match")
		}
		return requested, &existing, nil
	case domain.ConflictRename:
		name := requested.Name()
		extensionIndex := strings.LastIndexByte(name, '.')
		base, extension := name, ""
		if extensionIndex > 0 {
			base, extension = name[:extensionIndex], name[extensionIndex:]
		}
		for index := 1; index <= 10_000; index++ {
			suffix, candidateBase := fmt.Sprintf(" (%d)", index), base
			for len(candidateBase)+len(suffix)+len(extension) > 255 && candidateBase != "" {
				_, size := utf8.DecodeLastRuneInString(candidateBase)
				candidateBase = candidateBase[:len(candidateBase)-size]
			}
			candidate, joinErr := requested.Parent().Join(candidateBase + suffix + extension)
			if joinErr != nil {
				return domain.UserPath{}, nil, joinErr
			}
			if _, found, lookupErr := store.child(ctx, view, parent, candidate.Name()); lookupErr != nil {
				return domain.UserPath{}, nil, lookupErr
			} else if !found {
				return candidate, nil, nil
			}
		}
		return domain.UserPath{}, nil, domain.NewError(domain.ErrorConflict, "unable to generate a conflict-free name")
	default:
		return domain.UserPath{}, nil, domain.NewError(domain.ErrorInvalid, "invalid conflict mode")
	}
}

func (store *namespaceStore) prepareDestinationAtView(ctx context.Context, view *namespaceView, scope domain.Scope, requested domain.UserPath, conflict domain.ConflictMode, expected domain.Version) (domain.UserPath, bool, error) {
	if view == nil || view.reference != namespaceReference(scope.UserID()) {
		return domain.UserPath{}, false, domain.NewError(domain.ErrorInvalid, "upload destination view is misbound")
	}
	trail, err := store.resolveTrail(ctx, view, scope.Area(), requested.Parent())
	if err != nil {
		return domain.UserPath{}, false, err
	}
	resolved, existing, err := store.resolveDestination(ctx, view, trail[len(trail)-1].entry, requested, conflict, expected)
	return resolved, existing != nil, err
}

func (store *namespaceStore) createDirectory(ctx context.Context, scope domain.Scope, request domain.CreateDirectoryRequest) (domain.Entry, error) {
	conflict, err := domain.NormalizeConflictMode(request.Conflict)
	if err != nil || !request.Path.Valid() || request.Path.IsRoot() {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "directory path is invalid")
	}
	mutationID, err := store.engine.ids.OpaqueID()
	if err != nil {
		return domain.Entry{}, err
	}
	nodeID := storageformat.Digest([]byte("endlessfs-namespace-directory-v1\x00" + mutationID))
	now := store.engine.clock.Now().UTC()
	fingerprint := namespaceRequestFingerprint("create-directory", areaName(scope.Area()), request.Path.String(), string(conflict), string(request.ExpectedVersion))
	for {
		view, err := store.loadView(ctx, scope.UserID(), "")
		if err != nil {
			return domain.Entry{}, err
		}
		if replay, replayErr := store.operationReplay(ctx, view, mutationID, fingerprint); replayErr != nil {
			return domain.Entry{}, replayErr
		} else if replay != nil {
			if replay.Entry == nil {
				return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "directory mutation outcome is missing its entry")
			}
			resolved, joinErr := request.Path.Parent().Join(replay.Entry.Name)
			if joinErr != nil {
				return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "directory mutation outcome path is invalid")
			}
			return domainEntry(resolved, *replay.Entry), nil
		}
		if err := view.bindMutation(mutationID, fingerprint); err != nil {
			return domain.Entry{}, err
		}
		trail, err := store.resolveTrail(ctx, view, scope.Area(), request.Path.Parent())
		if err != nil {
			return domain.Entry{}, err
		}
		parentFrame := trail[len(trail)-1]
		resolved, existing, err := store.resolveDestination(ctx, view, parentFrame.entry, request.Path, conflict, request.ExpectedVersion)
		if err != nil {
			return domain.Entry{}, err
		}
		accumulator, digest, err := directoryContentIdentity(nil)
		if err != nil {
			return domain.Entry{}, err
		}
		created := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: nodeID, Entry: storageformat.DirectoryEntry{Name: resolved.Name(), NameDigest: storageformat.NameDigest(resolved.Name()), Kind: domain.EntryDirectory, DirectoryID: nodeID, ContentDigest: digest, ModifiedAt: now}, ContentAccumulator: accumulator}
		created.Entry.LogicalVersion, err = directoryEntryVersion(created.Entry)
		if err != nil {
			return domain.Entry{}, err
		}
		edits := []namespaceDirectoryEdit{{after: &created}}
		if existing != nil {
			edits[0].before = existing
		}
		updated, err := store.applyDirectoryEdits(ctx, view, parentFrame.entry, edits, now)
		if err != nil {
			return domain.Entry{}, err
		}
		frames := mergeNamespaceFrames(trail)
		changes := map[string]storageformat.NamespaceEntry{parentFrame.key: updated}
		if err := store.propagate(ctx, view, frames, changes, now); err != nil {
			return domain.Entry{}, err
		}
		entryCopy := created.Entry
		result, err := store.commit(ctx, view, mutationID, fingerprint, changes, storageformat.NamespaceMutationResult{Entry: &entryCopy})
		if err == nil {
			return domainEntry(resolved, *result.Entry), nil
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return domain.Entry{}, err
		}
	}
}

func (store *namespaceStore) publishFileWithChanges(ctx context.Context, scope domain.Scope, path domain.UserPath, conflict domain.ConflictMode, expected domain.Version, mutationID, requestFingerprint string, entry storageformat.DirectoryEntry, additional []consistencyDomainChange) (domain.Entry, error) {
	return store.publishFileWithChangesAtView(ctx, nil, scope, path, conflict, expected, mutationID, requestFingerprint, entry, additional)
}

func (store *namespaceStore) publishFileWithChangesAtView(ctx context.Context, initial *namespaceView, scope domain.Scope, path domain.UserPath, conflict domain.ConflictMode, expected domain.Version, mutationID, requestFingerprint string, entry storageformat.DirectoryEntry, additional []consistencyDomainChange) (domain.Entry, error) {
	if !path.Valid() || path.IsRoot() || entry.Kind != domain.EntryFile || entry.BlobID == "" || entry.Size < 0 || entry.MediaType == "" || entry.ModifiedAt.IsZero() || mutationID == "" || requestFingerprint == "" {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "invalid namespace file publication")
	}
	if initial != nil && (initial.reference != namespaceReference(scope.UserID()) || initial.snapshotDigest != "" || initial.headSnapshot == nil) {
		return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "namespace file publication view is misbound")
	}
	entry.Name, entry.NameDigest = path.Name(), storageformat.NameDigest(path.Name())
	entry.DirectoryID, entry.ManifestID, entry.StorageArea, entry.FileCount, entry.ContentDigest, entry.SHA256 = "", "", "", 0, "", ""
	entry.LogicalVersion = ""
	var err error
	entry.LogicalVersion, err = directoryEntryVersion(entry)
	if err != nil {
		return domain.Entry{}, err
	}
	nodeID := storageformat.Digest([]byte("endlessfs-namespace-file-v1\x00" + mutationID))
	file := storageformat.NamespaceEntry{SchemaVersion: 1, NodeID: nodeID, Entry: entry}
	for {
		view := initial
		initial = nil
		if view == nil {
			view, err = store.loadView(ctx, scope.UserID(), "")
			if err != nil {
				return domain.Entry{}, err
			}
		}
		if replay, err := store.operationReplay(ctx, view, mutationID, requestFingerprint); err != nil {
			return domain.Entry{}, err
		} else if replay != nil {
			if replay.Entry == nil {
				return domain.Entry{}, domain.NewError(domain.ErrorInvalid, "file mutation outcome is missing its entry")
			}
			return domainEntry(path, *replay.Entry), nil
		}
		if err := view.bindMutation(mutationID, requestFingerprint); err != nil {
			return domain.Entry{}, err
		}
		trail, err := store.resolveTrail(ctx, view, scope.Area(), path.Parent())
		if err != nil {
			return domain.Entry{}, err
		}
		parent := trail[len(trail)-1]
		resolved, existing, err := store.resolveDestination(ctx, view, parent.entry, path, conflict, expected)
		if err != nil {
			return domain.Entry{}, err
		}
		file.Entry.Name, file.Entry.NameDigest = resolved.Name(), storageformat.NameDigest(resolved.Name())
		file.Entry.LogicalVersion = ""
		file.Entry.LogicalVersion, err = directoryEntryVersion(file.Entry)
		if err != nil {
			return domain.Entry{}, err
		}
		fileCopy := file
		edit := namespaceDirectoryEdit{after: &fileCopy}
		if existing != nil {
			existingCopy := *existing
			edit.before = &existingCopy
		}
		updated, err := store.applyDirectoryEdits(ctx, view, parent.entry, []namespaceDirectoryEdit{edit}, entry.ModifiedAt)
		if err != nil {
			return domain.Entry{}, err
		}
		frames := mergeNamespaceFrames(trail)
		changes := map[string]storageformat.NamespaceEntry{parent.key: updated}
		if err := store.propagate(ctx, view, frames, changes, entry.ModifiedAt); err != nil {
			return domain.Entry{}, err
		}
		entryCopy := file.Entry
		result, err := store.commitWithAdditionalChanges(ctx, view, mutationID, requestFingerprint, changes, additional, storageformat.NamespaceMutationResult{Entry: &entryCopy})
		if err == nil {
			return domainEntry(resolved, *result.Entry), nil
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return domain.Entry{}, err
		}
		for _, extra := range additional {
			if extra.Require != domainValuePresent || extra.ExpectedVersion == "" {
				continue
			}
			current, getErr := store.domain.get(ctx, view.reference, extra.Key)
			if getErr != nil || current.LogicalVersion != extra.ExpectedVersion {
				return domain.Entry{}, err
			}
		}
	}
}

func (store *namespaceStore) copyOrMove(ctx context.Context, move bool, from, to domain.Scope, request domain.CopyRequest) (domain.Operation, error) {
	return store.copyOrMoveResolved(ctx, move, false, from, to, request)
}

// restoreFromTrash resolves the original destination from trash metadata in
// the same owner-domain snapshot that is conditionally published. This avoids
// a preliminary stat whose result could become stale and whose provider reads
// would otherwise be paid again by copyOrMove.
func (store *namespaceStore) restoreFromTrash(ctx context.Context, from, to domain.Scope, request domain.CopyRequest) (domain.Operation, error) {
	return store.copyOrMoveResolved(ctx, true, true, from, to, request)
}

func (store *namespaceStore) copyOrMoveResolved(ctx context.Context, move, restoreOriginal bool, from, to domain.Scope, request domain.CopyRequest) (domain.Operation, error) {
	if from.UserID() != to.UserID() {
		return domain.Operation{}, domain.NewError(domain.ErrorUnauthorized, "cross-user operations are forbidden")
	}
	if !request.Source.Valid() || request.Source.IsRoot() || (!restoreOriginal && (!request.Destination.Valid() || request.Destination.IsRoot())) {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "source and destination paths are required")
	}
	conflict, err := domain.NormalizeConflictMode(request.Conflict)
	if err != nil {
		return domain.Operation{}, err
	}
	if !restoreOriginal && (from == to && request.Source == request.Destination && conflict != domain.ConflictRename || from == to && request.Destination.IsDescendantOf(request.Source)) {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid move or copy destination")
	}
	if err := validatePortableIdempotencyKey(request.IdempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	kind := operationCopy
	if move {
		kind = operationMove
	}
	fingerprintDestination := request.Destination.String()
	if restoreOriginal {
		kind, fingerprintDestination = "restore", "original-trash-path"
	}
	requestFingerprint := namespaceRequestFingerprint(kind, areaName(from.Area()), areaName(to.Area()), request.Source.String(), fingerprintDestination, string(conflict), string(request.ExpectedSource), string(request.ExpectedTarget))
	operationID := namespaceOperationID(from.UserID(), kind, request.IdempotencyKey)
	if request.IdempotencyKey == "" {
		randomID, err := store.engine.ids.OpaqueID()
		if err != nil {
			return domain.Operation{}, err
		}
		operationID = domain.OperationID(randomID)
	}
	mutationID := string(operationID)
	nodeID := namespaceNodeID(operationID, "copy-root")
	now := store.engine.clock.Now().UTC()
	for {
		view, err := store.loadView(ctx, from.UserID(), "")
		if err != nil {
			return domain.Operation{}, err
		}
		if replay, err := store.operationReplay(ctx, view, mutationID, requestFingerprint); err != nil {
			return domain.Operation{}, err
		} else if replay != nil {
			if replay.Operation == nil {
				return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "copy or move outcome is missing its operation")
			}
			return *replay.Operation, nil
		}
		if err := view.bindMutation(mutationID, requestFingerprint); err != nil {
			return domain.Operation{}, err
		}
		sourceTrail, err := store.resolveTrail(ctx, view, from.Area(), request.Source.Parent())
		if err != nil {
			return domain.Operation{}, err
		}
		sourceParent := sourceTrail[len(sourceTrail)-1]
		source, found, err := store.child(ctx, view, sourceParent.entry, request.Source.Name())
		if err != nil {
			return domain.Operation{}, err
		}
		if !found {
			return domain.Operation{}, domain.NewError(domain.ErrorNotFound, "source does not exist")
		}
		if request.ExpectedSource != "" && request.ExpectedSource != domain.Version(source.Entry.LogicalVersion) {
			return domain.Operation{}, domain.NewError(domain.ErrorPreconditionFailed, "source version does not match")
		}
		destination := request.Destination
		if restoreOriginal {
			if source.Trash == nil || len(sourceTrail) != 1 {
				return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "trash entry metadata is missing")
			}
			destination, err = domain.ParseUserPath(source.Trash.OriginalPath)
			if err != nil || destination.IsRoot() {
				return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "trash entry original path is invalid")
			}
		}
		destinationTrail, err := store.resolveTrail(ctx, view, to.Area(), destination.Parent())
		if err != nil {
			return domain.Operation{}, err
		}
		destinationParent := destinationTrail[len(destinationTrail)-1]
		resolved, existing, err := store.resolveDestination(ctx, view, destinationParent.entry, destination, conflict, request.ExpectedTarget)
		if err != nil {
			return domain.Operation{}, err
		}
		placed := source
		placed.Entry.Name, placed.Entry.NameDigest = resolved.Name(), storageformat.NameDigest(resolved.Name())
		placed.Entry.ModifiedAt = now
		if move && from.Area() == domain.AreaLive && to.Area() == domain.AreaTrash {
			if len(destinationTrail) != 1 || source.Trash != nil {
				return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid move into trash")
			}
			placed.Trash = &storageformat.NamespaceTrashMetadata{OriginalPath: request.Source.String(), OriginalVersion: domain.Version(source.Entry.LogicalVersion), TrashedAt: now}
		} else if move && from.Area() == domain.AreaTrash && to.Area() == domain.AreaLive {
			if len(sourceTrail) != 1 || source.Trash == nil {
				return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid restore from trash")
			}
			placed.Trash = nil
		}
		if !move {
			placed.NodeID = nodeID
			if placed.Entry.Kind == domain.EntryDirectory {
				placed.Entry.DirectoryID = nodeID
				placed.OccurrenceContextID = nodeID
			} else {
				placed.Entry.DirectoryID = ""
			}
		} else if placed.Entry.Kind == domain.EntryDirectory && placed.OccurrenceContextID == "" {
			// A nested node can inherit the occurrence context of an O(1) copied
			// ancestor. Persist that context only when the node is detached, so a
			// subsequent move does not change its duplicate-ignore identity and no
			// descendant is rewritten.
			for _, frame := range sourceTrail {
				if frame.entry.OccurrenceContextID != "" {
					placed.OccurrenceContextID = frame.entry.OccurrenceContextID
					break
				}
			}
		}
		placed.Entry.LogicalVersion, err = directoryEntryVersion(placed.Entry)
		if err != nil {
			return domain.Operation{}, err
		}
		frames := mergeNamespaceFrames(sourceTrail, destinationTrail)
		changes := make(map[string]storageformat.NamespaceEntry)
		if sourceParent.key == destinationParent.key {
			edits := []namespaceDirectoryEdit{}
			if move {
				sourceCopy := source
				edits = append(edits, namespaceDirectoryEdit{before: &sourceCopy})
			}
			if existing != nil {
				existingCopy := *existing
				edits = append(edits, namespaceDirectoryEdit{before: &existingCopy})
			}
			placedCopy := placed
			edits = append(edits, namespaceDirectoryEdit{after: &placedCopy})
			updated, err := store.applyDirectoryEdits(ctx, view, sourceParent.entry, edits, now)
			if err != nil {
				return domain.Operation{}, err
			}
			changes[sourceParent.key] = updated
		} else {
			if move {
				sourceCopy := source
				updated, err := store.applyDirectoryEdits(ctx, view, sourceParent.entry, []namespaceDirectoryEdit{{before: &sourceCopy}}, now)
				if err != nil {
					return domain.Operation{}, err
				}
				changes[sourceParent.key] = updated
			}
			placedCopy := placed
			destinationEdit := namespaceDirectoryEdit{after: &placedCopy}
			if existing != nil {
				existingCopy := *existing
				destinationEdit.before = &existingCopy
			}
			updated, err := store.applyDirectoryEdits(ctx, view, destinationParent.entry, []namespaceDirectoryEdit{destinationEdit}, now)
			if err != nil {
				return domain.Operation{}, err
			}
			changes[destinationParent.key] = updated
		}
		if err := store.propagate(ctx, view, frames, changes, now); err != nil {
			return domain.Operation{}, err
		}
		operation := domain.Operation{ID: operationID, State: domain.OperationSucceeded, StartedAt: now, UpdatedAt: now}
		result, err := store.commit(ctx, view, mutationID, requestFingerprint, changes, storageformat.NamespaceMutationResult{Operation: &operation})
		if err == nil {
			return *result.Operation, nil
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return domain.Operation{}, err
		}
	}
}

func (store *namespaceStore) delete(ctx context.Context, scope domain.Scope, request domain.DeleteRequest) (domain.Operation, error) {
	return store.deleteResolved(ctx, scope, request, false)
}

func (store *namespaceStore) deleteFromTrash(ctx context.Context, scope domain.Scope, request domain.DeleteRequest) (domain.Operation, error) {
	return store.deleteResolved(ctx, scope, request, true)
}

func (store *namespaceStore) deleteResolved(ctx context.Context, scope domain.Scope, request domain.DeleteRequest, requireTrashMetadata bool) (domain.Operation, error) {
	if !request.Path.Valid() || request.Path.IsRoot() {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "delete path is invalid")
	}
	if err := validatePortableIdempotencyKey(request.IdempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	requestFingerprint := namespaceRequestFingerprint(operationDelete, areaName(scope.Area()), request.Path.String(), string(request.ExpectedVersion))
	operationID := namespaceOperationID(scope.UserID(), operationDelete, request.IdempotencyKey)
	if request.IdempotencyKey == "" {
		randomID, err := store.engine.ids.OpaqueID()
		if err != nil {
			return domain.Operation{}, err
		}
		operationID = domain.OperationID(randomID)
	}
	mutationID, now := string(operationID), store.engine.clock.Now().UTC()
	for {
		view, err := store.loadView(ctx, scope.UserID(), "")
		if err != nil {
			return domain.Operation{}, err
		}
		if replay, err := store.operationReplay(ctx, view, mutationID, requestFingerprint); err != nil {
			return domain.Operation{}, err
		} else if replay != nil {
			if replay.Operation == nil {
				return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "delete outcome is missing its operation")
			}
			return *replay.Operation, nil
		}
		if err := view.bindMutation(mutationID, requestFingerprint); err != nil {
			return domain.Operation{}, err
		}
		trail, err := store.resolveTrail(ctx, view, scope.Area(), request.Path.Parent())
		if err != nil {
			return domain.Operation{}, err
		}
		parent := trail[len(trail)-1]
		entry, found, err := store.child(ctx, view, parent.entry, request.Path.Name())
		if err != nil {
			return domain.Operation{}, err
		}
		if !found {
			return domain.Operation{}, domain.NewError(domain.ErrorNotFound, "entry does not exist")
		}
		if requireTrashMetadata && (scope.Area() != domain.AreaTrash || len(trail) != 1 || entry.Trash == nil) {
			return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "trash entry metadata is missing")
		}
		if request.ExpectedVersion != "" && request.ExpectedVersion != domain.Version(entry.Entry.LogicalVersion) {
			return domain.Operation{}, domain.NewError(domain.ErrorPreconditionFailed, "entry version does not match")
		}
		entryCopy := entry
		updated, err := store.applyDirectoryEdits(ctx, view, parent.entry, []namespaceDirectoryEdit{{before: &entryCopy}}, now)
		if err != nil {
			return domain.Operation{}, err
		}
		frames := mergeNamespaceFrames(trail)
		changes := map[string]storageformat.NamespaceEntry{parent.key: updated}
		if err := store.propagate(ctx, view, frames, changes, now); err != nil {
			return domain.Operation{}, err
		}
		operation := domain.Operation{ID: operationID, State: domain.OperationSucceeded, StartedAt: now, UpdatedAt: now}
		result, err := store.commit(ctx, view, mutationID, requestFingerprint, changes, storageformat.NamespaceMutationResult{Operation: &operation})
		if err == nil {
			return *result.Operation, nil
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return domain.Operation{}, err
		}
	}
}

func (store *namespaceStore) getOperation(ctx context.Context, owner domain.UserID, operationID domain.OperationID) (domain.Operation, error) {
	view, err := store.loadView(ctx, owner, "")
	if err != nil {
		return domain.Operation{}, err
	}
	result, err := store.operationReplay(ctx, view, string(operationID), "")
	if err != nil {
		return domain.Operation{}, err
	}
	if result == nil || result.Operation == nil {
		if result != nil && result.Batch != nil {
			return result.Batch.Operation, nil
		}
		return domain.Operation{}, domain.NewError(domain.ErrorNotFound, "operation does not exist")
	}
	return *result.Operation, nil
}
