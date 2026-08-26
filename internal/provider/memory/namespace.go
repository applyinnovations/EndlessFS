package memory

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func memoryTrashKey(owner domain.UserID, trashID string) string {
	return owner.String() + "\x00" + trashID
}

func validateMemoryTrashID(value string) error {
	if value == "" || strings.Contains(value, "/") || strings.ContainsRune(value, 0) {
		return domain.NewError(domain.ErrorInvalid, "invalid trash ID")
	}
	return nil
}

func memoryNamespaceMutationKey(owner domain.UserID, kind, key string) string {
	return owner.String() + "\x00" + kind + "\x00" + key
}

func (p *Provider) replayNamespaceMutation(owner domain.UserID, kind, key, fingerprint string) (domain.Operation, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prior, found := p.namespaceMutations[memoryNamespaceMutationKey(owner, kind, key)]
	if !found {
		return domain.Operation{}, false, nil
	}
	if prior.fingerprint != fingerprint {
		return domain.Operation{}, false, domain.NewError(domain.ErrorConflict, "idempotency key was used for a different request")
	}
	return prior.operation, true, nil
}

func (p *Provider) saveNamespaceMutation(owner domain.UserID, kind, key, fingerprint string, operation domain.Operation) {
	p.mu.Lock()
	p.namespaceMutations[memoryNamespaceMutationKey(owner, kind, key)] = namespaceMutationResult{fingerprint: fingerprint, operation: operation}
	p.mu.Unlock()
}

func (p *Provider) MoveToTrash(ctx context.Context, owner domain.UserID, request domain.TrashRequest) (domain.Operation, error) {
	if !owner.Valid() || !request.Path.Valid() || request.Path.IsRoot() || validateMemoryTrashID(request.TrashID) != nil {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid trash request")
	}
	if err := validateIdempotencyKey(request.IdempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	fingerprint := operationFingerprint(request.Path.String(), string(request.ExpectedVersion), request.TrashID)
	if prior, found, err := p.replayNamespaceMutation(owner, "trash", request.IdempotencyKey, fingerprint); found || err != nil {
		return prior, err
	}
	live, _ := domain.NewScope(owner, domain.AreaLive)
	trash, _ := domain.NewScope(owner, domain.AreaTrash)
	source, err := p.Stat(ctx, live, request.Path)
	if err != nil {
		return domain.Operation{}, err
	}
	if request.ExpectedVersion != "" && request.ExpectedVersion != source.Version {
		return domain.Operation{}, domain.NewError(domain.ErrorPreconditionFailed, "trash source version changed")
	}
	destination := domain.MustParseUserPath("/" + request.TrashID)
	operation, err := p.Move(ctx, live, trash, domain.MoveRequest{Source: request.Path, Destination: destination, Conflict: domain.ConflictFail, ExpectedSource: source.Version, IdempotencyKey: request.IdempotencyKey})
	if err != nil || operation.State != domain.OperationSucceeded {
		return operation, err
	}
	p.mu.Lock()
	entry := p.scopeObjectsLocked(trash)[destination.String()].entry
	p.trashEntries[memoryTrashKey(owner, request.TrashID)] = domain.TrashEntry{TrashID: request.TrashID, OwnerUserID: owner, OriginalPath: request.Path, TrashedPath: destination, Entry: entry, TrashedAt: p.clock.Now().UTC(), OriginalVersion: source.Version}
	p.mu.Unlock()
	p.saveNamespaceMutation(owner, "trash", request.IdempotencyKey, fingerprint, operation)
	return operation, nil
}

func (p *Provider) ListTrash(ctx context.Context, owner domain.UserID, request domain.TrashListRequest) (domain.TrashListPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.TrashListPage{}, domain.WrapError(domain.ErrorUnavailable, "provider request canceled", err)
	}
	if !owner.Valid() {
		return domain.TrashListPage{}, domain.NewError(domain.ErrorInvalid, "invalid trash owner")
	}
	limit := request.Limit
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > 1000 {
		return domain.TrashListPage{}, domain.NewError(domain.ErrorInvalid, "trash page limit must be between 1 and 1000")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if request.Cursor != "" {
		snapshot, found := p.trashSnapshots[request.Cursor]
		if !found || snapshot.owner != owner || snapshot.limit != limit {
			return domain.TrashListPage{}, domain.NewError(domain.ErrorInvalid, "invalid or out-of-scope trash cursor")
		}
		return p.trashPageLocked(request.Cursor, snapshot), nil
	}
	items := make([]domain.TrashEntry, 0)
	prefix := owner.String() + "\x00"
	for key, item := range p.trashEntries {
		if strings.HasPrefix(key, prefix) {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].TrashedAt.Equal(items[right].TrashedAt) {
			return items[left].TrashID < items[right].TrashID
		}
		return items[left].TrashedAt.After(items[right].TrashedAt)
	})
	if len(items) <= limit {
		return domain.TrashListPage{Items: items}, nil
	}
	cursor, err := p.ids.OpaqueID()
	if err != nil {
		return domain.TrashListPage{}, err
	}
	snapshot := &trashListSnapshot{owner: owner, limit: limit, items: items}
	p.trashSnapshots[cursor] = snapshot
	return p.trashPageLocked(cursor, snapshot), nil
}

func (p *Provider) trashPageLocked(cursor string, snapshot *trashListSnapshot) domain.TrashListPage {
	end := min(snapshot.index+snapshot.limit, len(snapshot.items))
	items := append([]domain.TrashEntry(nil), snapshot.items[snapshot.index:end]...)
	snapshot.index = end
	if end == len(snapshot.items) {
		delete(p.trashSnapshots, cursor)
		cursor = ""
	}
	return domain.TrashListPage{Items: items, NextCursor: cursor}
}

func (p *Provider) RestoreFromTrash(ctx context.Context, owner domain.UserID, trashID string, conflict domain.ConflictMode, idempotencyKey string) (domain.Operation, error) {
	if err := ctx.Err(); err != nil {
		return domain.Operation{}, domain.WrapError(domain.ErrorUnavailable, "provider request canceled", err)
	}
	if !owner.Valid() || validateMemoryTrashID(trashID) != nil {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid restore request")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	fingerprint := operationFingerprint(trashID, string(conflict))
	if prior, found, err := p.replayNamespaceMutation(owner, "restore", idempotencyKey, fingerprint); found || err != nil {
		return prior, err
	}
	p.mu.Lock()
	record, found := p.trashEntries[memoryTrashKey(owner, trashID)]
	p.mu.Unlock()
	if !found {
		return domain.Operation{}, domain.NewError(domain.ErrorNotFound, "trash entry not found")
	}
	live, _ := domain.NewScope(owner, domain.AreaLive)
	trash, _ := domain.NewScope(owner, domain.AreaTrash)
	operation, err := p.Move(ctx, trash, live, domain.MoveRequest{Source: record.TrashedPath, Destination: record.OriginalPath, Conflict: conflict, ExpectedSource: record.Entry.Version, IdempotencyKey: idempotencyKey})
	if err == nil && operation.State == domain.OperationSucceeded {
		p.mu.Lock()
		delete(p.trashEntries, memoryTrashKey(owner, trashID))
		p.mu.Unlock()
		p.saveNamespaceMutation(owner, "restore", idempotencyKey, fingerprint, operation)
	}
	return operation, err
}

func (p *Provider) DeleteFromTrash(ctx context.Context, owner domain.UserID, trashID, idempotencyKey string) (domain.Operation, error) {
	if err := ctx.Err(); err != nil {
		return domain.Operation{}, domain.WrapError(domain.ErrorUnavailable, "provider request canceled", err)
	}
	if !owner.Valid() || validateMemoryTrashID(trashID) != nil {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "invalid trash deletion request")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	fingerprint := operationFingerprint(trashID)
	if prior, found, err := p.replayNamespaceMutation(owner, "delete-trash", idempotencyKey, fingerprint); found || err != nil {
		return prior, err
	}
	p.mu.Lock()
	record, found := p.trashEntries[memoryTrashKey(owner, trashID)]
	p.mu.Unlock()
	if !found {
		return domain.Operation{}, domain.NewError(domain.ErrorNotFound, "trash entry not found")
	}
	trash, _ := domain.NewScope(owner, domain.AreaTrash)
	operation, err := p.Delete(ctx, trash, domain.DeleteRequest{Path: record.TrashedPath, ExpectedVersion: record.Entry.Version, IdempotencyKey: idempotencyKey})
	if err == nil && operation.State == domain.OperationSucceeded {
		p.mu.Lock()
		delete(p.trashEntries, memoryTrashKey(owner, trashID))
		p.mu.Unlock()
		p.saveNamespaceMutation(owner, "delete-trash", idempotencyKey, fingerprint, operation)
	}
	return operation, err
}

type memoryNamespaceSnapshot struct {
	objects     map[domain.Scope]map[string]object
	operations  map[string]domain.Operation
	idempotency map[string]idempotentResult
	trash       map[string]domain.TrashEntry
	batches     map[string]namespaceBatchResult
	mutations   map[string]namespaceMutationResult
}

func (p *Provider) namespaceSnapshot() memoryNamespaceSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	objects := make(map[domain.Scope]map[string]object, len(p.objects))
	for scope, values := range p.objects {
		objects[scope] = cloneObjects(values)
	}
	operations := make(map[string]domain.Operation, len(p.operations))
	for key, value := range p.operations {
		operations[key] = value
	}
	idempotency := make(map[string]idempotentResult, len(p.idempotency))
	for key, value := range p.idempotency {
		idempotency[key] = value
	}
	trash := make(map[string]domain.TrashEntry, len(p.trashEntries))
	for key, value := range p.trashEntries {
		trash[key] = value
	}
	batches := make(map[string]namespaceBatchResult, len(p.namespaceBatches))
	for key, value := range p.namespaceBatches {
		batches[key] = value
	}
	mutations := make(map[string]namespaceMutationResult, len(p.namespaceMutations))
	for key, value := range p.namespaceMutations {
		mutations[key] = value
	}
	return memoryNamespaceSnapshot{objects: objects, operations: operations, idempotency: idempotency, trash: trash, batches: batches, mutations: mutations}
}

func (p *Provider) restoreNamespaceSnapshot(snapshot memoryNamespaceSnapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.objects, p.operations, p.idempotency, p.trashEntries, p.namespaceBatches, p.namespaceMutations = snapshot.objects, snapshot.operations, snapshot.idempotency, snapshot.trash, snapshot.batches, snapshot.mutations
}

func memoryBatchKey(owner domain.UserID, label, key string) string {
	return owner.String() + "\x00" + label + "\x00" + key
}

func (p *Provider) replayMemoryBatch(owner domain.UserID, label, key, fingerprint string) (domain.NamespaceBatchResult, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prior, found := p.namespaceBatches[memoryBatchKey(owner, label, key)]
	if !found {
		return domain.NamespaceBatchResult{}, false, nil
	}
	if prior.fingerprint != fingerprint {
		return domain.NamespaceBatchResult{}, false, domain.NewError(domain.ErrorConflict, "idempotency key was used for a different batch")
	}
	return prior.result, true, nil
}

func (p *Provider) finishMemoryBatch(owner domain.UserID, label, key, fingerprint string, items []domain.NamespaceBatchItemResult) (domain.NamespaceBatchResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	operation, err := p.newOperationLocked(owner)
	if err != nil {
		return domain.NamespaceBatchResult{}, err
	}
	operation.State, operation.UpdatedAt = domain.OperationSucceeded, p.clock.Now().UTC()
	p.saveOperationLocked(owner, operation)
	for index := range items {
		items[index].OperationID, items[index].State = operation.ID, domain.OperationSucceeded
	}
	result := domain.NamespaceBatchResult{Operation: operation, Items: items}
	p.namespaceBatches[memoryBatchKey(owner, label, key)] = namespaceBatchResult{fingerprint: fingerprint, result: result}
	return result, nil
}

func (p *Provider) BatchCopyMove(ctx context.Context, owner domain.UserID, requests []domain.CopyRequest, move bool, idempotencyKey string) (domain.NamespaceBatchResult, error) {
	if !owner.Valid() || len(requests) < 1 || len(requests) > 10_000 || validateIdempotencyKey(idempotencyKey) != nil {
		return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid copy/move batch")
	}
	label := fmt.Sprintf("batch-copy-move-%t", move)
	parts := make([]string, 0, len(requests)*6)
	for _, request := range requests {
		parts = append(parts, request.Source.String(), request.Destination.String(), string(request.Conflict), string(request.ExpectedSource), string(request.ExpectedTarget), fmt.Sprint(move))
	}
	fingerprint := operationFingerprint(parts...)
	if prior, found, err := p.replayMemoryBatch(owner, label, idempotencyKey, fingerprint); found || err != nil {
		return prior, err
	}
	snapshot := p.namespaceSnapshot()
	live, _ := domain.NewScope(owner, domain.AreaLive)
	items := make([]domain.NamespaceBatchItemResult, 0, len(requests))
	for index, request := range requests {
		request.IdempotencyKey = idempotencyKey + ":" + strconv.Itoa(index)
		var operation domain.Operation
		var err error
		if move {
			operation, err = p.Move(ctx, live, live, request)
		} else {
			operation, err = p.Copy(ctx, live, live, request)
		}
		if err != nil || operation.State != domain.OperationSucceeded {
			p.restoreNamespaceSnapshot(snapshot)
			if err == nil {
				err = domain.NewError(domain.ErrorUnavailable, "mock batch item failed")
			}
			return domain.NamespaceBatchResult{}, err
		}
		items = append(items, domain.NamespaceBatchItemResult{Source: request.Source, Destination: request.Destination})
	}
	result, err := p.finishMemoryBatch(owner, label, idempotencyKey, fingerprint, items)
	if err != nil {
		p.restoreNamespaceSnapshot(snapshot)
	}
	return result, err
}

func (p *Provider) BatchMoveToTrash(ctx context.Context, owner domain.UserID, requests []domain.TrashRequest, idempotencyKey string) (domain.NamespaceBatchResult, error) {
	if !owner.Valid() || len(requests) < 1 || len(requests) > 10_000 || validateIdempotencyKey(idempotencyKey) != nil {
		return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid trash batch")
	}
	parts := make([]string, 0, len(requests)*3)
	for _, request := range requests {
		parts = append(parts, request.Path.String(), string(request.ExpectedVersion), request.TrashID)
	}
	fingerprint := operationFingerprint(parts...)
	if prior, found, err := p.replayMemoryBatch(owner, "batch-trash", idempotencyKey, fingerprint); found || err != nil {
		return prior, err
	}
	snapshot := p.namespaceSnapshot()
	items := make([]domain.NamespaceBatchItemResult, 0, len(requests))
	for index, request := range requests {
		request.IdempotencyKey = idempotencyKey + ":" + strconv.Itoa(index)
		operation, err := p.MoveToTrash(ctx, owner, request)
		if err != nil || operation.State != domain.OperationSucceeded {
			p.restoreNamespaceSnapshot(snapshot)
			if err == nil {
				err = domain.NewError(domain.ErrorUnavailable, "mock batch item failed")
			}
			return domain.NamespaceBatchResult{}, err
		}
		items = append(items, domain.NamespaceBatchItemResult{Source: request.Path, TrashID: request.TrashID})
	}
	result, err := p.finishMemoryBatch(owner, "batch-trash", idempotencyKey, fingerprint, items)
	if err != nil {
		p.restoreNamespaceSnapshot(snapshot)
	}
	return result, err
}

func (p *Provider) BatchDeleteFromTrash(ctx context.Context, owner domain.UserID, trashIDs []string, idempotencyKey string) (domain.NamespaceBatchResult, error) {
	if !owner.Valid() || len(trashIDs) < 1 || len(trashIDs) > 10_000 || validateIdempotencyKey(idempotencyKey) != nil {
		return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid delete-trash batch")
	}
	fingerprint := operationFingerprint(trashIDs...)
	if prior, found, err := p.replayMemoryBatch(owner, "batch-delete-trash", idempotencyKey, fingerprint); found || err != nil {
		return prior, err
	}
	seen := make(map[string]struct{}, len(trashIDs))
	for _, trashID := range trashIDs {
		if validateMemoryTrashID(trashID) != nil {
			return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "invalid trash ID")
		}
		if _, duplicate := seen[trashID]; duplicate {
			return domain.NamespaceBatchResult{}, domain.NewError(domain.ErrorInvalid, "duplicate trash ID")
		}
		seen[trashID] = struct{}{}
	}
	snapshot := p.namespaceSnapshot()
	items := make([]domain.NamespaceBatchItemResult, 0, len(trashIDs))
	for index, trashID := range trashIDs {
		p.mu.Lock()
		record := p.trashEntries[memoryTrashKey(owner, trashID)]
		p.mu.Unlock()
		operation, err := p.DeleteFromTrash(ctx, owner, trashID, idempotencyKey+":"+strconv.Itoa(index))
		if err != nil || operation.State != domain.OperationSucceeded {
			p.restoreNamespaceSnapshot(snapshot)
			if err == nil {
				err = domain.NewError(domain.ErrorUnavailable, "mock batch item failed")
			}
			return domain.NamespaceBatchResult{}, err
		}
		items = append(items, domain.NamespaceBatchItemResult{Source: record.OriginalPath, TrashID: trashID})
	}
	result, err := p.finishMemoryBatch(owner, "batch-delete-trash", idempotencyKey, fingerprint, items)
	if err != nil {
		p.restoreNamespaceSnapshot(snapshot)
	}
	return result, err
}

func (p *Provider) GetBatchOperation(ctx context.Context, owner domain.UserID, operationID domain.OperationID) (domain.Operation, error) {
	return p.GetOperation(ctx, owner, operationID)
}
