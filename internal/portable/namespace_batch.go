package portable

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

// The HTTP boundary historically capped selections at 100 because it ran a
// complete transaction per item. Schema 008 publishes one bounded owner
// transaction and stores the result in immutable pages, so the product-scale
// selection bound can be raised without growing the conditional head.
const maximumNamespaceBatchItems = storageformat.MaxNamespaceBatchItems

type namespaceBatchMoveSpec struct {
	from        domain.Scope
	to          domain.Scope
	request     domain.CopyRequest
	trashID     string
	attachTrash bool
}

type namespaceBatchMovePlan struct {
	spec             namespaceBatchMoveSpec
	sourceTrail      []namespaceFrame
	destinationTrail []namespaceFrame
	source           storageformat.NamespaceEntry
	existing         *storageformat.NamespaceEntry
	resolved         domain.UserPath
	placed           storageformat.NamespaceEntry
}

type namespaceBatchViewValidator func(context.Context, *namespaceView) error

func (s *FileStore) BatchCopyMove(ctx context.Context, owner domain.UserID, requests []domain.CopyRequest, move bool, idempotencyKey string) (domain.NamespaceBatchResult, error) {
	live, err := domain.NewScope(owner, domain.AreaLive)
	if err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	specs := make([]namespaceBatchMoveSpec, len(requests))
	for index, request := range requests {
		specs[index] = namespaceBatchMoveSpec{from: live, to: live, request: request}
	}
	kind := "batch-copy"
	if move {
		kind = "batch-move"
	}
	return newNamespaceStore(s.engine).batchCopyOrMove(ctx, owner, specs, move, kind, idempotencyKey)
}

func (s *FileStore) BatchMoveToTrash(ctx context.Context, owner domain.UserID, requests []domain.TrashRequest, idempotencyKey string) (domain.NamespaceBatchResult, error) {
	live, err := domain.NewScope(owner, domain.AreaLive)
	if err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	trash, err := domain.NewScope(owner, domain.AreaTrash)
	if err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	specs := make([]namespaceBatchMoveSpec, len(requests))
	for index, request := range requests {
		destination, parseErr := domain.ParseUserPath("/" + request.TrashID)
		if parseErr != nil || destination.Name() != request.TrashID {
			return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid trash identity")
		}
		specs[index] = namespaceBatchMoveSpec{
			from: live, to: trash, trashID: request.TrashID, attachTrash: true,
			request: domain.CopyRequest{Source: request.Path, Destination: destination, Conflict: domain.ConflictFail, ExpectedSource: request.ExpectedVersion},
		}
	}
	return newNamespaceStore(s.engine).batchCopyOrMove(ctx, owner, specs, true, "batch-trash", idempotencyKey)
}

func validateNamespaceBatchSize(count int) error {
	if count < 1 || count > maximumNamespaceBatchItems {
		return domain.NewError(domain.ErrorInvalid, "namespace batch must contain 1 to 10000 items")
	}
	return nil
}

func namespaceBatchFingerprint(kind string, specs []namespaceBatchMoveSpec) (string, error) {
	type item struct {
		From           string              `json:"from"`
		To             string              `json:"to"`
		Source         string              `json:"source"`
		Destination    string              `json:"destination"`
		Conflict       domain.ConflictMode `json:"conflict"`
		ExpectedSource domain.Version      `json:"expectedSource,omitempty"`
		ExpectedTarget domain.Version      `json:"expectedTarget,omitempty"`
		TrashID        string              `json:"trashID,omitempty"`
	}
	// Hash each bounded item before composing the ordered batch fingerprint.
	// The complete 10,000-item intent therefore remains bound without ever
	// attempting to encode it as one canonical record.
	intent := []byte("endlessfs-namespace-batch-v2\x00" + kind)
	for _, spec := range specs {
		value := item{
			From: areaName(spec.from.Area()), To: areaName(spec.to.Area()), Source: spec.request.Source.String(),
			Destination: spec.request.Destination.String(), Conflict: spec.request.Conflict,
			ExpectedSource: spec.request.ExpectedSource, ExpectedTarget: spec.request.ExpectedTarget, TrashID: spec.trashID,
		}
		body, err := storageformat.EncodeCanonical(value)
		if err != nil {
			return "", err
		}
		intent = append(intent, 0)
		intent = append(intent, storageformat.Digest(body)...)
	}
	return storageformat.Digest(intent), nil
}

func rejectOverlappingNamespaceBatchPaths(plans []namespaceBatchMovePlan, move bool) error {
	type pathRef struct {
		key  string
		kind byte
	}
	paths := make([]pathRef, 0, len(plans)*2)
	for _, plan := range plans {
		sourceKey := areaName(plan.spec.from.Area()) + ":" + plan.spec.request.Source.String()
		destinationKey := areaName(plan.spec.to.Area()) + ":" + plan.resolved.String()
		paths = append(paths, pathRef{key: destinationKey, kind: 'd'})
		if move {
			paths = append(paths, pathRef{key: sourceKey, kind: 's'})
		}
	}
	sort.Slice(paths, func(left, right int) bool {
		if paths[left].key == paths[right].key {
			return paths[left].kind < paths[right].kind
		}
		return paths[left].key < paths[right].key
	})
	for index := 1; index < len(paths); index++ {
		previous, current := paths[index-1].key, paths[index].key
		if current == previous || strings.HasPrefix(current, previous+"/") {
			return domain.NewError(domain.ErrorInvalid, "namespace batch contains overlapping paths")
		}
	}
	return nil
}

func (store *namespaceStore) batchCopyOrMove(ctx context.Context, owner domain.UserID, specs []namespaceBatchMoveSpec, move bool, kind, idempotencyKey string) (domain.NamespaceBatchResult, error) {
	return store.batchCopyOrMoveValidated(ctx, owner, specs, move, kind, idempotencyKey, "", nil)
}

func (store *namespaceStore) batchCopyOrMoveValidated(ctx context.Context, owner domain.UserID, specs []namespaceBatchMoveSpec, move bool, kind, idempotencyKey, intentBinding string, validate namespaceBatchViewValidator) (domain.NamespaceBatchResult, error) {
	if !owner.Valid() || kind == "" {
		return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid namespace batch owner")
	}
	if err := validateNamespaceBatchSize(len(specs)); err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	if err := validatePortableIdempotencyKey(idempotencyKey); err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	fingerprint, err := namespaceBatchFingerprint(kind, specs)
	if err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	if intentBinding != "" {
		fingerprint = storageformat.Digest([]byte("endlessfs-namespace-batch-bound-intent-v1\x00" + fingerprint + "\x00" + intentBinding))
	}
	operationID := namespaceOperationID(owner, kind, idempotencyKey)
	mutationID := string(operationID)
	now := store.engine.clock.Now().UTC()

	for {
		view, err := store.loadView(ctx, owner, "")
		if err != nil {
			return domain.NamespaceBatchResult{}, err
		}
		if replay, err := store.batchReplay(ctx, view, mutationID, fingerprint); err != nil || replay != nil {
			if replay == nil {
				return domain.NamespaceBatchResult{}, err
			}
			return *replay, err
		}
		if validate != nil {
			if err := validate(ctx, view); err != nil {
				return domain.NamespaceBatchResult{}, err
			}
		}

		plans := make([]namespaceBatchMovePlan, 0, len(specs))
		for index, spec := range specs {
			request := spec.request
			if spec.from.UserID() != owner || spec.to.UserID() != owner || !request.Source.Valid() || request.Source.IsRoot() || !request.Destination.Valid() || request.Destination.IsRoot() {
				return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid namespace batch path")
			}
			conflict, normalizeErr := domain.NormalizeConflictMode(request.Conflict)
			if normalizeErr != nil {
				return domain.NamespaceBatchResult{}, normalizeErr
			}
			request.Conflict = conflict
			if spec.from == spec.to && (request.Source == request.Destination && conflict != domain.ConflictRename || request.Destination.IsDescendantOf(request.Source)) {
				return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid namespace batch destination")
			}
			sourceTrail, resolveErr := store.resolveTrail(ctx, view, spec.from.Area(), request.Source.Parent())
			if resolveErr != nil {
				return domain.NamespaceBatchResult{}, resolveErr
			}
			sourceParent := sourceTrail[len(sourceTrail)-1]
			source, found, childErr := store.child(ctx, view, sourceParent.entry, request.Source.Name())
			if childErr != nil {
				return domain.NamespaceBatchResult{}, childErr
			}
			if !found {
				return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorNotFound, "namespace batch source does not exist")
			}
			if request.ExpectedSource != "" && request.ExpectedSource != domain.Version(source.Entry.LogicalVersion) {
				return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorPreconditionFailed, "namespace batch source version does not match")
			}
			destinationTrail, resolveErr := store.resolveTrail(ctx, view, spec.to.Area(), request.Destination.Parent())
			if resolveErr != nil {
				return domain.NamespaceBatchResult{}, resolveErr
			}
			destinationParent := destinationTrail[len(destinationTrail)-1]
			resolved, existing, destinationErr := store.resolveDestination(ctx, view, destinationParent.entry, request.Destination, conflict, request.ExpectedTarget)
			if destinationErr != nil {
				return domain.NamespaceBatchResult{}, destinationErr
			}
			placed := source
			placed.Entry.Name, placed.Entry.NameDigest, placed.Entry.ModifiedAt = resolved.Name(), storageformat.NameDigest(resolved.Name()), now
			if spec.attachTrash {
				if len(destinationTrail) != 1 || source.Trash != nil {
					return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid namespace batch trash placement")
				}
				placed.Trash = &storageformat.NamespaceTrashMetadata{OriginalPath: request.Source.String(), OriginalVersion: domain.Version(source.Entry.LogicalVersion), TrashedAt: now}
			}
			if !move {
				placed.NodeID = namespaceNodeID(operationID, fmt.Sprintf("copy-%016x", index))
				if placed.Entry.Kind == domain.EntryDirectory {
					placed.Entry.DirectoryID = placed.NodeID
				} else {
					placed.Entry.DirectoryID = ""
				}
			}
			placed.Entry.LogicalVersion, err = directoryEntryVersion(placed.Entry)
			if err != nil {
				return domain.NamespaceBatchResult{}, err
			}
			plans = append(plans, namespaceBatchMovePlan{
				spec:        namespaceBatchMoveSpec{from: spec.from, to: spec.to, request: request, trashID: spec.trashID, attachTrash: spec.attachTrash},
				sourceTrail: sourceTrail, destinationTrail: destinationTrail, source: source, existing: existing, resolved: resolved, placed: placed,
			})
		}
		if err := rejectOverlappingNamespaceBatchPaths(plans, move); err != nil {
			return domain.NamespaceBatchResult{}, err
		}

		frames := make(map[string]namespaceFrame)
		grouped := make(map[string][]namespaceDirectoryEdit)
		for _, plan := range plans {
			for key, frame := range mergeNamespaceFrames(plan.sourceTrail, plan.destinationTrail) {
				frames[key] = frame
			}
			sourceParent := plan.sourceTrail[len(plan.sourceTrail)-1]
			destinationParent := plan.destinationTrail[len(plan.destinationTrail)-1]
			if move {
				sourceCopy := plan.source
				grouped[sourceParent.key] = append(grouped[sourceParent.key], namespaceDirectoryEdit{before: &sourceCopy})
			}
			placedCopy := plan.placed
			destinationEdit := namespaceDirectoryEdit{after: &placedCopy}
			if plan.existing != nil {
				existingCopy := *plan.existing
				destinationEdit.before = &existingCopy
			}
			grouped[destinationParent.key] = append(grouped[destinationParent.key], destinationEdit)
		}

		changes := make(map[string]storageformat.NamespaceEntry, len(grouped))
		parentKeys := make([]string, 0, len(grouped))
		for key := range grouped {
			parentKeys = append(parentKeys, key)
		}
		sort.Strings(parentKeys)
		for _, key := range parentKeys {
			updated, applyErr := store.applyDirectoryEdits(ctx, view, frames[key].entry, grouped[key], now)
			if applyErr != nil {
				return domain.NamespaceBatchResult{}, applyErr
			}
			changes[key] = updated
		}
		if err := store.propagate(ctx, view, frames, changes, now); err != nil {
			return domain.NamespaceBatchResult{}, err
		}

		items := make([]domain.NamespaceBatchItemResult, len(plans))
		for index, plan := range plans {
			items[index] = domain.NamespaceBatchItemResult{
				Source: plan.spec.request.Source, Destination: plan.resolved, TrashID: plan.spec.trashID,
				OperationID: operationID, State: domain.OperationSucceeded,
			}
		}
		operation := domain.Operation{ID: operationID, State: domain.OperationSucceeded, StartedAt: now, UpdatedAt: now}
		root, err := store.writeBatchItems(ctx, view, items)
		if err != nil {
			return domain.NamespaceBatchResult{}, err
		}
		result, err := store.commit(ctx, view, mutationID, fingerprint, changes, storageformat.NamespaceMutationResult{Batch: &storageformat.NamespaceBatch{Operation: operation, Items: root, ItemCount: uint64(len(items))}})
		if err == nil {
			return store.decodeBatchResult(ctx, view, *result.Batch)
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return domain.NamespaceBatchResult{}, err
		}
	}
}

func (store *namespaceStore) writeBatchItems(ctx context.Context, view *namespaceView, items []domain.NamespaceBatchItemResult) (storageformat.DomainTreeRoot, error) {
	entries := make([]storageformat.DomainEntry, len(items))
	for index, item := range items {
		stored := storageformat.NamespaceBatchItem{
			Index: uint64(index), Source: item.Source.String(), Destination: item.Destination.String(), TrashID: item.TrashID,
			OperationID: item.OperationID, State: item.State,
		}
		if err := storageformat.ValidateNamespaceBatchItem(stored); err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		body, err := storageformat.EncodeCanonical(stored)
		if err != nil {
			return storageformat.DomainTreeRoot{}, err
		}
		entries[index] = storageformat.DomainEntry{Key: fmt.Sprintf("%016x", index), Value: body, LogicalVersion: storageformat.Digest(append([]byte("endlessfs-namespace-batch-item-v1\x00"), body...))}
	}
	return view.session.buildTree(ctx, entries)
}

func (store *namespaceStore) decodeBatchResult(ctx context.Context, view *namespaceView, batch storageformat.NamespaceBatch) (domain.NamespaceBatchResult, error) {
	if batch.ItemCount == 0 || batch.ItemCount > maximumNamespaceBatchItems || batch.Items.EntryCount != batch.ItemCount {
		return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid namespace batch outcome")
	}
	if err := storageformat.ValidateNamespaceBatch(batch); err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	values, err := view.session.collect(ctx, batch.Items, "", "", int(batch.ItemCount)+1)
	if err != nil || uint64(len(values)) != batch.ItemCount {
		if err != nil {
			return domain.NamespaceBatchResult{}, err
		}
		return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "incomplete namespace batch outcome")
	}
	result := domain.NamespaceBatchResult{Operation: batch.Operation, Items: make([]domain.NamespaceBatchItemResult, len(values))}
	for index, value := range values {
		if value.Key != fmt.Sprintf("%016x", index) {
			return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "misordered namespace batch outcome")
		}
		var stored storageformat.NamespaceBatchItem
		if err := decodeCanonicalValue(value.Value, &stored); err != nil || storageformat.ValidateNamespaceBatchItem(stored) != nil || stored.Index != uint64(index) || stored.OperationID != batch.Operation.ID {
			return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid namespace batch outcome item")
		}
		// ValidateNamespaceBatchItem already proved these path parses. Parsing
		// again only converts the canonical strings into their domain values.
		source, _ := domain.ParseUserPath(stored.Source)
		var destination domain.UserPath
		if stored.Destination != "" {
			destination, _ = domain.ParseUserPath(stored.Destination)
		}
		result.Items[index] = domain.NamespaceBatchItemResult{Source: source, Destination: destination, TrashID: stored.TrashID, OperationID: stored.OperationID, State: stored.State}
	}
	return result, nil
}

func (store *namespaceStore) batchReplay(ctx context.Context, view *namespaceView, mutationID, fingerprint string) (*domain.NamespaceBatchResult, error) {
	result, err := store.operationReplay(ctx, view, mutationID, fingerprint)
	if err != nil || result == nil {
		return nil, err
	}
	if result.Batch == nil {
		return nil, domain.NewError(domain.ErrorInvalid, "namespace outcome is not a batch")
	}
	decoded, err := store.decodeBatchResult(ctx, view, *result.Batch)
	return &decoded, err
}

func (s *FileStore) BatchDeleteFromTrash(ctx context.Context, owner domain.UserID, trashIDs []string, idempotencyKey string) (domain.NamespaceBatchResult, error) {
	if !owner.Valid() {
		return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid trash owner")
	}
	if err := validateNamespaceBatchSize(len(trashIDs)); err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	if err := validatePortableIdempotencyKey(idempotencyKey); err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	type intent struct {
		TrashIDs []string `json:"trashIDs"`
	}
	body, err := storageformat.EncodeCanonical(intent{TrashIDs: trashIDs})
	if err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	fingerprint := storageformat.Digest(append([]byte("endlessfs-namespace-batch-delete-trash-v1\x00"), body...))
	operationID := namespaceOperationID(owner, "batch-delete-trash", idempotencyKey)
	mutationID, now := string(operationID), s.engine.clock.Now().UTC()
	store := newNamespaceStore(s.engine)

	for {
		view, err := store.loadView(ctx, owner, "")
		if err != nil {
			return domain.NamespaceBatchResult{}, err
		}
		if replay, err := store.batchReplay(ctx, view, mutationID, fingerprint); err != nil || replay != nil {
			if replay == nil {
				return domain.NamespaceBatchResult{}, err
			}
			return *replay, err
		}
		rootFrame := namespaceFrame{key: namespaceFrameKey(domain.AreaTrash, namespaceRootPath()), path: namespaceRootPath(), area: domain.AreaTrash, entry: view.roots[domain.AreaTrash]}
		edits := make([]namespaceDirectoryEdit, 0, len(trashIDs))
		items := make([]domain.NamespaceBatchItemResult, 0, len(trashIDs))
		seen := make(map[string]struct{}, len(trashIDs))
		for _, trashID := range trashIDs {
			if _, duplicate := seen[trashID]; duplicate {
				return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "trash batch contains duplicate identities")
			}
			seen[trashID] = struct{}{}
			path, parseErr := domain.ParseUserPath("/" + trashID)
			if parseErr != nil || path.Name() != trashID {
				return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid trash identity")
			}
			entry, found, childErr := store.child(ctx, view, rootFrame.entry, trashID)
			if childErr != nil {
				return domain.NamespaceBatchResult{}, childErr
			}
			if !found {
				return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorNotFound, "trash entry does not exist")
			}
			trashEntry, trashErr := namespaceTrashEntry(owner, path, entry)
			if trashErr != nil {
				return domain.NamespaceBatchResult{}, trashErr
			}
			entryCopy := entry
			edits = append(edits, namespaceDirectoryEdit{before: &entryCopy})
			items = append(items, domain.NamespaceBatchItemResult{Source: trashEntry.OriginalPath, TrashID: trashID, OperationID: operationID, State: domain.OperationSucceeded})
		}
		updated, err := store.applyDirectoryEdits(ctx, view, rootFrame.entry, edits, now)
		if err != nil {
			return domain.NamespaceBatchResult{}, err
		}
		changes := map[string]storageformat.NamespaceEntry{rootFrame.key: updated}
		operation := domain.Operation{ID: operationID, State: domain.OperationSucceeded, StartedAt: now, UpdatedAt: now}
		root, err := store.writeBatchItems(ctx, view, items)
		if err != nil {
			return domain.NamespaceBatchResult{}, err
		}
		result, err := store.commit(ctx, view, mutationID, fingerprint, changes, storageformat.NamespaceMutationResult{Batch: &storageformat.NamespaceBatch{Operation: operation, Items: root, ItemCount: uint64(len(items))}})
		if err == nil {
			return store.decodeBatchResult(ctx, view, *result.Batch)
		}
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPreconditionFailed) {
			return domain.NamespaceBatchResult{}, err
		}
	}
}

func (s *FileStore) GetBatchOperation(ctx context.Context, owner domain.UserID, operationID domain.OperationID) (domain.Operation, error) {
	view, err := newNamespaceStore(s.engine).loadView(ctx, owner, "")
	if err != nil {
		return domain.Operation{}, err
	}
	result, err := newNamespaceStore(s.engine).operationReplay(ctx, view, string(operationID), "")
	if err != nil {
		return domain.Operation{}, err
	}
	if result == nil || result.Batch == nil {
		return domain.Operation{}, domain.NewError(domain.ErrorNotFound, "batch operation does not exist")
	}
	return result.Batch.Operation, nil
}
