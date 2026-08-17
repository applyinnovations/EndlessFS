package memory

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func (p *Provider) Copy(ctx context.Context, from, to domain.Scope, request domain.CopyRequest) (domain.Operation, error) {
	return p.copyOrMove(ctx, OperationCopy, false, from, to, request)
}

func (p *Provider) Move(ctx context.Context, from, to domain.Scope, request domain.MoveRequest) (domain.Operation, error) {
	return p.copyOrMove(ctx, OperationMove, true, from, to, request)
}

func (p *Provider) copyOrMove(ctx context.Context, operationName string, move bool, from, to domain.Scope, request domain.CopyRequest) (domain.Operation, error) {
	if err := validateContextScope(ctx, from); err != nil {
		return domain.Operation{}, err
	}
	if err := validateContextScope(ctx, to); err != nil {
		return domain.Operation{}, err
	}
	if from.UserID() != to.UserID() {
		return domain.Operation{}, domain.NewError(domain.ErrorUnauthorized, "cross-user operations are forbidden")
	}
	if !request.Source.Valid() || request.Source.IsRoot() || !request.Destination.Valid() || request.Destination.IsRoot() {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "source and destination paths are required")
	}
	conflict, err := domain.NormalizeConflictMode(request.Conflict)
	if err != nil {
		return domain.Operation{}, err
	}
	if from == to && request.Source == request.Destination && conflict != domain.ConflictRename {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "source and destination are identical")
	}
	if from == to && request.Destination.IsDescendantOf(request.Source) {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "destination cannot be inside the source tree")
	}
	if err := validateIdempotencyKey(request.IdempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	fingerprint := operationFingerprint(
		from.UserID().String(), fmt.Sprint(from.Area()), fmt.Sprint(to.Area()),
		request.Source.String(), request.Destination.String(), string(conflict),
		string(request.ExpectedSource), string(request.ExpectedTarget), fmt.Sprint(move),
	)

	p.mu.Lock()
	defer p.mu.Unlock()
	if result, found, err := p.idempotentLocked(from.UserID(), operationName, request.IdempotencyKey, fingerprint); found || err != nil {
		return result, err
	}
	if err := p.beforeLocked(operationName); err != nil {
		return domain.Operation{}, err
	}
	root, found := p.scopeObjectsLocked(from)[request.Source.String()]
	if !found {
		return domain.Operation{}, domain.NewError(domain.ErrorNotFound, "source not found")
	}
	if request.ExpectedSource != "" && root.entry.Version != request.ExpectedSource {
		return domain.Operation{}, domain.NewError(domain.ErrorPreconditionFailed, "source version does not match")
	}
	if err := p.requireParentLocked(to, request.Destination); err != nil {
		return domain.Operation{}, err
	}
	destination, err := p.resolveTreeDestinationLocked(to, request.Destination, conflict, request.ExpectedTarget)
	if err != nil {
		return domain.Operation{}, err
	}
	operation, err := p.newOperationLocked(from.UserID())
	if err != nil {
		return domain.Operation{}, err
	}
	if p.consumeSpecificFaultLocked(operationName, FaultPartialOperation) {
		operation.State = domain.OperationFailed
		operation.ErrorKind = domain.ErrorUnavailable
		operation.Error = "injected partial operation failure"
		operation.UpdatedAt = p.clock.Now().UTC()
		p.saveOperationLocked(from.UserID(), operation)
		p.saveIdempotentLocked(from.UserID(), operationName, request.IdempotencyKey, fingerprint, operation)
		return operation, nil
	}

	type treeItem struct {
		oldPath domain.UserPath
		newPath domain.UserPath
		value   object
		entry   domain.Entry
	}
	items := make([]treeItem, 0)
	for pathValue, item := range p.scopeObjectsLocked(from) {
		path, parseErr := domain.ParseUserPath(pathValue)
		if parseErr != nil || (path != request.Source && !path.IsDescendantOf(request.Source)) {
			continue
		}
		relative := strings.TrimPrefix(path.String(), request.Source.String())
		newPath, parseErr := domain.ParseUserPath(destination.String() + relative)
		if parseErr != nil {
			return domain.Operation{}, parseErr
		}
		if existing, exists := p.scopeObjectsLocked(to)[newPath.String()]; exists {
			if newPath != destination || conflict != domain.ConflictReplace || existing.entry.Version != request.ExpectedTarget {
				return domain.Operation{}, domain.NewError(domain.ErrorConflict, "destination tree conflicts with existing content")
			}
		}
		entry := p.newEntryLocked(newPath, item.entry.Kind, item.entry.Size, item.entry.MediaType)
		if item.entry.Kind == domain.EntryFile {
			if move {
				entry.ContentID = item.entry.ContentID
				entry.ContentVersion = item.entry.ContentVersion
				entry.ContentModifiedAt = item.entry.ContentModifiedAt
			} else {
				entry, err = p.newFileEntryLocked(newPath, item.entry.Size, item.entry.MediaType, domain.PreviewContentIdentity{})
				if err != nil {
					return domain.Operation{}, err
				}
			}
		}
		items = append(items, treeItem{oldPath: path, newPath: newPath, value: item, entry: entry})
	}
	if conflict == domain.ConflictReplace {
		p.deleteTreeLocked(to, destination)
	}
	for _, item := range items {
		p.scopeObjectsLocked(to)[item.newPath.String()] = object{
			entry: item.entry, data: append([]byte(nil), item.value.data...), materialized: item.value.materialized,
		}
	}
	if move {
		for _, item := range items {
			delete(p.scopeObjectsLocked(from), item.oldPath.String())
		}
	}
	operation.State = domain.OperationSucceeded
	operation.UpdatedAt = p.clock.Now().UTC()
	p.saveOperationLocked(from.UserID(), operation)
	p.saveIdempotentLocked(from.UserID(), operationName, request.IdempotencyKey, fingerprint, operation)
	return operation, nil
}

func (p *Provider) resolveTreeDestinationLocked(scope domain.Scope, path domain.UserPath, conflict domain.ConflictMode, expected domain.Version) (domain.UserPath, error) {
	item, exists := p.scopeObjectsLocked(scope)[path.String()]
	if !exists {
		return path, nil
	}
	switch conflict {
	case domain.ConflictFail:
		return domain.UserPath{}, domain.NewError(domain.ErrorConflict, "destination already exists")
	case domain.ConflictReplace:
		if expected == "" || item.entry.Version != expected {
			return domain.UserPath{}, domain.NewError(domain.ErrorPreconditionFailed, "destination version does not match")
		}
		return path, nil
	case domain.ConflictRename:
		return p.availableRenamedPathLocked(scope, path)
	default:
		return domain.UserPath{}, domain.NewError(domain.ErrorInvalid, "invalid conflict mode")
	}
}

func (p *Provider) Delete(ctx context.Context, scope domain.Scope, request domain.DeleteRequest) (domain.Operation, error) {
	if err := validateContextScope(ctx, scope); err != nil {
		return domain.Operation{}, err
	}
	if !request.Path.Valid() || request.Path.IsRoot() {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "delete path is invalid")
	}
	if err := validateIdempotencyKey(request.IdempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	fingerprint := operationFingerprint(scope.UserID().String(), fmt.Sprint(scope.Area()), request.Path.String(), string(request.ExpectedVersion))
	p.mu.Lock()
	defer p.mu.Unlock()
	if result, found, err := p.idempotentLocked(scope.UserID(), OperationDelete, request.IdempotencyKey, fingerprint); found || err != nil {
		return result, err
	}
	if err := p.beforeLocked(OperationDelete); err != nil {
		return domain.Operation{}, err
	}
	item, found := p.scopeObjectsLocked(scope)[request.Path.String()]
	if !found {
		return domain.Operation{}, domain.NewError(domain.ErrorNotFound, "entry not found")
	}
	if request.ExpectedVersion != "" && item.entry.Version != request.ExpectedVersion {
		return domain.Operation{}, domain.NewError(domain.ErrorPreconditionFailed, "entry version does not match")
	}
	operation, err := p.newOperationLocked(scope.UserID())
	if err != nil {
		return domain.Operation{}, err
	}
	if p.consumeSpecificFaultLocked(OperationDelete, FaultPartialOperation) {
		operation.State = domain.OperationFailed
		operation.ErrorKind = domain.ErrorUnavailable
		operation.Error = "injected partial operation failure"
	} else {
		p.deleteTreeLocked(scope, request.Path)
		operation.State = domain.OperationSucceeded
	}
	operation.UpdatedAt = p.clock.Now().UTC()
	p.saveOperationLocked(scope.UserID(), operation)
	p.saveIdempotentLocked(scope.UserID(), OperationDelete, request.IdempotencyKey, fingerprint, operation)
	return operation, nil
}

func (p *Provider) GetOperation(ctx context.Context, userID domain.UserID, operationID domain.OperationID) (domain.Operation, error) {
	if err := ctx.Err(); err != nil {
		return domain.Operation{}, domain.WrapError(domain.ErrorUnavailable, "provider request canceled", err)
	}
	if !userID.Valid() || operationID == "" {
		return domain.Operation{}, domain.NewError(domain.ErrorInvalid, "user and operation IDs are required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	operation, found := p.operations[operationKey(userID, operationID)]
	if !found {
		return domain.Operation{}, domain.NewError(domain.ErrorNotFound, "operation not found")
	}
	return operation, nil
}

func (p *Provider) newOperationLocked(userID domain.UserID) (domain.Operation, error) {
	value, err := p.ids.OpaqueID()
	if err != nil {
		return domain.Operation{}, err
	}
	now := p.clock.Now().UTC()
	return domain.Operation{ID: domain.OperationID(value), State: domain.OperationRunning, StartedAt: now, UpdatedAt: now}, nil
}

func (p *Provider) saveOperationLocked(userID domain.UserID, operation domain.Operation) {
	p.operations[operationKey(userID, operation.ID)] = operation
}

func (p *Provider) idempotentLocked(userID domain.UserID, operation, key, fingerprint string) (domain.Operation, bool, error) {
	if key == "" {
		return domain.Operation{}, false, nil
	}
	result, found := p.idempotency[idempotencyKey(userID, operation, key)]
	if !found {
		return domain.Operation{}, false, nil
	}
	if result.fingerprint != fingerprint {
		return domain.Operation{}, false, domain.NewError(domain.ErrorConflict, "idempotency key was used for a different request")
	}
	return result.operation, true, nil
}

func (p *Provider) saveIdempotentLocked(userID domain.UserID, operation, key, fingerprint string, result domain.Operation) {
	if key == "" {
		return
	}
	p.idempotency[idempotencyKey(userID, operation, key)] = idempotentResult{fingerprint: fingerprint, operation: result}
}

func validateIdempotencyKey(value string) error {
	if value == "" {
		return nil
	}
	if !utf8.ValidString(value) || len(value) > 128 {
		return domain.NewError(domain.ErrorInvalid, "invalid idempotency key")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return domain.NewError(domain.ErrorInvalid, "invalid idempotency key")
		}
	}
	return nil
}
