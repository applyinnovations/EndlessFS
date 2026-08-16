// Package drive implements the authenticated file, trash, preview, and share
// control plane over provider-neutral storage and conditional state contracts.
package drive

import (
	"context"
	"errors"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type repository struct {
	store state.Store
}

func newRepository(store state.Store) *repository { return &repository{store: store} }

func (r *repository) createTrash(ctx context.Context, record model.Trash) error {
	return r.create(ctx, state.MustKey(state.NamespaceTrash, record.OwnerUserID.String(), record.TrashID), &record)
}

func (r *repository) createBatchOperation(ctx context.Context, record model.BatchOperation) error {
	return r.create(ctx, state.MustKey(state.NamespaceOperations, "batch", record.OwnerUserID.String(), string(record.OperationID)), &record)
}

func (r *repository) batchOperation(ctx context.Context, owner domain.UserID, operationID domain.OperationID) (model.BatchOperation, error) {
	record, _, err := getRecord[model.BatchOperation](ctx, r.store, state.MustKey(state.NamespaceOperations, "batch", owner.String(), string(operationID)))
	return record, err
}

func (r *repository) createMutationOutcome(ctx context.Context, key string, record model.MutationOutcome) error {
	return r.create(ctx, state.MustKey(state.NamespaceIdempotency, "drive", record.OwnerUserID.String(), secret.Hash(key)), &record)
}

func (r *repository) mutationOutcome(ctx context.Context, owner domain.UserID, key string) (model.MutationOutcome, error) {
	record, _, err := getRecord[model.MutationOutcome](ctx, r.store, state.MustKey(state.NamespaceIdempotency, "drive", owner.String(), secret.Hash(key)))
	return record, err
}

func (r *repository) trash(ctx context.Context, owner domain.UserID, trashID string) (model.Trash, state.Version, error) {
	return getRecord[model.Trash](ctx, r.store, state.MustKey(state.NamespaceTrash, owner.String(), trashID))
}

func (r *repository) deleteTrash(ctx context.Context, owner domain.UserID, trashID string, version state.Version) error {
	return r.store.Delete(ctx, state.MustKey(state.NamespaceTrash, owner.String(), trashID), version)
}

func (r *repository) trashList(ctx context.Context, owner domain.UserID) ([]model.Trash, error) {
	return listRecords[model.Trash](ctx, r.store, state.MustPrefix(state.NamespaceTrash, owner.String()))
}

func (r *repository) trashPage(ctx context.Context, owner domain.UserID, limit int, cursor string) ([]model.Trash, string, error) {
	page, err := r.store.List(ctx, state.MustPrefix(state.NamespaceTrash, owner.String()), state.PageRequest{Limit: limit, Cursor: cursor})
	if err != nil {
		return nil, "", err
	}
	records := make([]model.Trash, 0, len(page.Items))
	for _, item := range page.Items {
		var record model.Trash
		if err := state.DecodeJSON(item.Value.Data, &record); err != nil {
			return nil, "", err
		}
		records = append(records, record)
	}
	return records, page.NextCursor, nil
}

func (r *repository) createShare(ctx context.Context, record model.Share) error {
	return r.create(ctx, state.MustKey(state.NamespaceShares, record.TokenHash), &record)
}

func (r *repository) shareByTokenHash(ctx context.Context, tokenHash string) (model.Share, state.Version, error) {
	return getRecord[model.Share](ctx, r.store, state.MustKey(state.NamespaceShares, tokenHash))
}

func (r *repository) shareByID(ctx context.Context, owner domain.UserID, shareID string) (model.Share, state.Version, error) {
	records, err := r.shares(ctx, owner)
	if err != nil {
		return model.Share{}, "", err
	}
	for _, record := range records {
		if record.ShareID == shareID {
			value, version, err := r.shareByTokenHash(ctx, record.TokenHash)
			return value, version, err
		}
	}
	return model.Share{}, "", domain.NewError(domain.ErrorNotFound, "share not found")
}

func (r *repository) shares(ctx context.Context, owner domain.UserID) ([]model.Share, error) {
	records, err := listRecords[model.Share](ctx, r.store, state.MustPrefix(state.NamespaceShares))
	if err != nil {
		return nil, err
	}
	owned := make([]model.Share, 0, len(records))
	for _, record := range records {
		if record.OwnerUserID == owner {
			owned = append(owned, record)
		}
	}
	return owned, nil
}

func (r *repository) updateShare(ctx context.Context, record model.Share, version state.Version) error {
	data, err := state.EncodeJSON(&record)
	if err != nil {
		return err
	}
	_, err = r.store.CompareAndSwap(ctx, state.MustKey(state.NamespaceShares, record.TokenHash), version, data)
	return err
}

func (r *repository) create(ctx context.Context, key state.Key, record any) error {
	data, err := state.EncodeJSON(record)
	if err != nil {
		return err
	}
	_, err = r.store.Create(ctx, key, data)
	return err
}

func getRecord[T any](ctx context.Context, store state.Store, key state.Key) (T, state.Version, error) {
	var record T
	value, err := store.Get(ctx, key)
	if err != nil {
		return record, "", err
	}
	if err := state.DecodeJSON(value.Data, &record); err != nil {
		return record, "", err
	}
	return record, value.Version, nil
}

func listRecords[T any](ctx context.Context, store state.Store, prefix state.Prefix) ([]T, error) {
	var records []T
	request := state.PageRequest{Limit: 200}
	for {
		page, err := store.List(ctx, prefix, request)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			var record T
			if err := state.DecodeJSON(item.Value.Data, &record); err != nil {
				return nil, err
			}
			records = append(records, record)
		}
		if page.NextCursor == "" {
			return records, nil
		}
		request.Cursor = page.NextCursor
	}
}

func createOrMatch(err error) error {
	if errors.Is(err, domain.ErrConflict) {
		return nil
	}
	return err
}
