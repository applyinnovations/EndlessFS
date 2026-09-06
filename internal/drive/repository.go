// Package drive implements the authenticated file, trash, preview, and share
// control plane over provider-neutral storage and conditional state contracts.
package drive

import (
	"context"
	"errors"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/model"
	"github.com/applyinnovations/endlessfs/internal/state"
)

type repository struct {
	store state.Store
}

func newRepository(store state.Store) *repository { return &repository{store: store} }

func (r *repository) createShare(ctx context.Context, record model.Share) error {
	return r.create(ctx, shareKey(record.OwnerUserID, record.ShareID), &record)
}

func shareKey(owner domain.UserID, shareID string) state.Key {
	return state.MustKey(state.NamespaceShares, owner.String(), shareID)
}

func (r *repository) shareByID(ctx context.Context, owner domain.UserID, shareID string) (model.Share, state.Version, error) {
	return getRecord[model.Share](ctx, r.store, shareKey(owner, shareID))
}

func (r *repository) shares(ctx context.Context, owner domain.UserID) ([]model.Share, error) {
	return listRecords[model.Share](ctx, r.store, state.MustPrefix(state.NamespaceShares, owner.String()))
}

func (r *repository) updateShare(ctx context.Context, record model.Share, version state.Version) error {
	data, err := state.EncodeJSON(&record)
	if err != nil {
		return err
	}
	_, err = r.store.CompareAndSwap(ctx, shareKey(record.OwnerUserID, record.ShareID), version, data)
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
